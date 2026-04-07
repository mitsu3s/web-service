package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Task struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	UserID      uint      `json:"user_id" gorm:"not null;index"`
	ProjectID   uint      `json:"project_id" gorm:"index"`
	Title       string    `json:"title" gorm:"not null"`
	Description string    `json:"description"`
	Status      string    `json:"status" gorm:"default:'pending'"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type OutboxEvent struct {
	ID          uint   `gorm:"primaryKey"`
	EventID     string `gorm:"type:varchar(64);uniqueIndex;not null"`
	RoutingKey  string `gorm:"type:varchar(64);not null;index"`
	Payload     string `gorm:"type:text;not null"`
	PublishedAt *time.Time
	CreatedAt   time.Time
}

type TaskEvent struct {
	EventID     string    `json:"event_id"`
	Type        string    `json:"type"`
	TaskID      uint      `json:"task_id"`
	ProjectID   uint      `json:"project_id"`
	UserID      uint      `json:"user_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type amqpPublisher struct {
	url      string
	exchange string

	mu    sync.Mutex
	conn  *amqp.Connection
	ch    *amqp.Channel
	ready atomic.Bool
}

var (
	db                *gorm.DB
	rdb               *redis.Client
	publisher         *amqpPublisher
	projectServiceURL string
	httpClient        = &http.Client{Timeout: 5 * time.Second}

	tasksCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasks_created_total",
		Help: "Total tasks created",
	})
	cacheHitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Redis cache hits and misses",
	}, []string{"result"})
	outboxPublishedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_outbox_published_total",
		Help: "Total outbox events published to RabbitMQ",
	})
	outboxPublishFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_outbox_publish_failures_total",
		Help: "Total outbox publish failures",
	})
)

func init() {
	prometheus.MustRegister(
		tasksCreatedTotal,
		cacheHitsTotal,
		outboxPublishedTotal,
		outboxPublishFailuresTotal,
	)
}

func initMySQL() {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "devboard:devboard123@tcp(mysql:3306)/devboard?charset=utf8mb4&parseTime=True&loc=Local"
	}

	var err error
	for i := 1; i <= 15; i++ {
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Info),
		})
		if err == nil {
			break
		}
		log.Printf("MySQL not ready (%d/15): %v", i, err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		log.Fatalf("failed to connect MySQL: %v", err)
	}
	if err := db.AutoMigrate(&Task{}, &OutboxEvent{}); err != nil {
		log.Fatalf("AutoMigrate: %v", err)
	}
	log.Println("MySQL connected")
}

func initRedis() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "redis:6379"
	}
	rdb = redis.NewClient(&redis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("Redis not available: %v (cache will degrade gracefully)", err)
		return
	}
	log.Println("Redis connected")
}

func newPublisher(url, exchange string) *amqpPublisher {
	return &amqpPublisher{url: url, exchange: exchange}
}

func (p *amqpPublisher) closeLocked() {
	if p.ch != nil {
		_ = p.ch.Close()
		p.ch = nil
	}
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.ready.Store(false)
}

func (p *amqpPublisher) ensureLocked() error {
	if p.conn != nil && !p.conn.IsClosed() && p.ch != nil {
		return nil
	}

	conn, err := amqp.Dial(p.url)
	if err != nil {
		p.ready.Store(false)
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		p.ready.Store(false)
		return err
	}
	if err := ch.ExchangeDeclare(p.exchange, "topic", true, false, false, false, nil); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		p.ready.Store(false)
		return err
	}

	p.conn = conn
	p.ch = ch
	p.ready.Store(true)
	return nil
}

func (p *amqpPublisher) Publish(ctx context.Context, routingKey string, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.ensureLocked(); err != nil {
		return err
	}
	err := p.ch.PublishWithContext(ctx, p.exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now().UTC(),
		Body:         body,
	})
	if err == nil {
		return nil
	}

	p.closeLocked()
	if err := p.ensureLocked(); err != nil {
		return err
	}
	return p.ch.PublishWithContext(ctx, p.exchange, routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now().UTC(),
		Body:         body,
	})
}

func (p *amqpPublisher) Ready() bool {
	return p != nil && p.ready.Load()
}

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func getUserID(r *http.Request) (uint, bool) {
	raw := r.Header.Get("X-User-ID")
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}

func newEventID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func cacheVersionKey(userID uint) string {
	return fmt.Sprintf("tasks:user:%d:version", userID)
}

func cacheKey(userID uint, version string, projectID *uint) string {
	scope := "all"
	if projectID != nil {
		scope = fmt.Sprintf("project:%d", *projectID)
	}
	return fmt.Sprintf("tasks:user:%d:v:%s:%s", userID, version, scope)
}

func getCacheVersion(ctx context.Context, userID uint) string {
	if rdb == nil {
		return "0"
	}
	version, err := rdb.Get(ctx, cacheVersionKey(userID)).Result()
	if err == nil {
		return version
	}
	if err != redis.Nil {
		log.Printf("cache version lookup failed: %v", err)
	}
	return "0"
}

func invalidateCache(ctx context.Context, userID uint) {
	if rdb == nil {
		return
	}
	if err := rdb.Incr(ctx, cacheVersionKey(userID)).Err(); err != nil {
		log.Printf("cache invalidation failed: %v", err)
	}
}

func parseProjectID(r *http.Request) (*uint, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid project_id")
	}
	projectID := uint(id)
	return &projectID, nil
}

func validateProjectAccess(ctx context.Context, userID, projectID uint) error {
	if projectID == 0 || projectServiceURL == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%d/access", strings.TrimRight(projectServiceURL, "/"), projectID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-ID", strconv.FormatUint(uint64(userID), 10))

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("project not accessible")
	}
	return fmt.Errorf("project-service returned %s", resp.Status)
}

func enqueueTaskEvent(tx *gorm.DB, eventType string, task Task) error {
	event := TaskEvent{
		EventID:     newEventID(),
		Type:        eventType,
		TaskID:      task.ID,
		ProjectID:   task.ProjectID,
		UserID:      task.UserID,
		Title:       task.Title,
		Description: task.Description,
		Status:      task.Status,
		OccurredAt:  time.Now().UTC(),
	}

	body, err := json.Marshal(event)
	if err != nil {
		return err
	}

	return tx.Create(&OutboxEvent{
		EventID:    event.EventID,
		RoutingKey: event.Type,
		Payload:    string(body),
	}).Error
}

func dispatchPendingOutbox(ctx context.Context) {
	if publisher == nil {
		return
	}

	var events []OutboxEvent
	if err := db.WithContext(ctx).
		Where("published_at IS NULL").
		Order("id ASC").
		Limit(50).
		Find(&events).Error; err != nil {
		log.Printf("failed to load outbox events: %v", err)
		return
	}

	for _, evt := range events {
		if err := publisher.Publish(ctx, evt.RoutingKey, []byte(evt.Payload)); err != nil {
			outboxPublishFailuresTotal.Inc()
			log.Printf("failed to publish outbox event %s: %v", evt.EventID, err)
			return
		}

		now := time.Now().UTC()
		if err := db.WithContext(ctx).
			Model(&OutboxEvent{}).
			Where("id = ? AND published_at IS NULL", evt.ID).
			Update("published_at", &now).Error; err != nil {
			outboxPublishFailuresTotal.Inc()
			log.Printf("failed to mark outbox event %s published: %v", evt.EventID, err)
			return
		}
		outboxPublishedTotal.Inc()
	}
}

func dispatchOutboxLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		dispatchPendingOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func tasksHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	projectID, err := parseProjectID(r)
	if err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if projectID != nil {
		if err := validateProjectAccess(ctx, userID, *projectID); err != nil {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		version := getCacheVersion(ctx, userID)
		if rdb != nil {
			if cached, err := rdb.Get(ctx, cacheKey(userID, version, projectID)).Result(); err == nil {
				cacheHitsTotal.WithLabelValues("hit").Inc()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				fmt.Fprint(w, cached)
				return
			} else if err != redis.Nil {
				log.Printf("cache read failed: %v", err)
			}
		}

		cacheHitsTotal.WithLabelValues("miss").Inc()

		query := db.WithContext(ctx).Where("user_id = ?", userID)
		if projectID != nil {
			query = query.Where("project_id = ?", *projectID)
		}

		var tasks []Task
		query.Order("created_at desc").Find(&tasks)
		body, _ := json.Marshal(tasks)
		if rdb != nil {
			if err := rdb.Set(ctx, cacheKey(userID, version, projectID), body, 5*time.Minute).Err(); err != nil {
				log.Printf("cache write failed: %v", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)

	case http.MethodPost:
		var req struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			ProjectID   uint   `json:"project_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
			return
		}
		if req.ProjectID != 0 {
			if err := validateProjectAccess(ctx, userID, req.ProjectID); err != nil {
				jsonResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}

		task := Task{
			UserID:      userID,
			ProjectID:   req.ProjectID,
			Title:       strings.TrimSpace(req.Title),
			Description: strings.TrimSpace(req.Description),
			Status:      "pending",
		}

		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			return enqueueTaskEvent(tx, "task.created", task)
		}); err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to create task"})
			return
		}

		invalidateCache(ctx, userID)
		tasksCreatedTotal.Inc()
		jsonResp(w, http.StatusCreated, task)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func taskHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var task Task
	if result := db.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&task); result.Error != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		jsonResp(w, http.StatusOK, task)

	case http.MethodPut:
		var req struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Status      string `json:"status"`
			ProjectID   *uint  `json:"project_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if req.ProjectID != nil && *req.ProjectID != 0 && *req.ProjectID != task.ProjectID {
			if err := validateProjectAccess(ctx, userID, *req.ProjectID); err != nil {
				jsonResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			task.ProjectID = *req.ProjectID
		}
		if strings.TrimSpace(req.Title) != "" {
			task.Title = strings.TrimSpace(req.Title)
		}
		if req.Description != "" {
			task.Description = strings.TrimSpace(req.Description)
		}
		if req.Status != "" {
			task.Status = req.Status
		}

		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Save(&task).Error; err != nil {
				return err
			}
			return enqueueTaskEvent(tx, "task.updated", task)
		}); err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to update task"})
			return
		}

		invalidateCache(ctx, userID)
		jsonResp(w, http.StatusOK, task)

	case http.MethodDelete:
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Delete(&task).Error; err != nil {
				return err
			}
			return enqueueTaskEvent(tx, "task.deleted", task)
		}); err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete task"})
			return
		}

		invalidateCache(ctx, userID)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func main() {
	initMySQL()
	initRedis()

	projectServiceURL = os.Getenv("PROJECT_SERVICE_URL")
	rabbitURL := os.Getenv("RABBITMQ_URL")
	if rabbitURL == "" {
		rabbitURL = "amqp://devboard:devboard123@rabbitmq:5672/"
	}
	publisher = newPublisher(rabbitURL, "devboard.events")

	go dispatchOutboxLoop(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", tasksHandler)
	mux.HandleFunc("/tasks/", taskHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
		jsonResp(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"service":      "task-service",
			"rabbit_ready": publisher.Ready(),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("task-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
