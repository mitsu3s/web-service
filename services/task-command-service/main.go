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
	Status      string    `json:"status" gorm:"default:'backlog'"`
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
	db        *gorm.DB
	publisher *amqpPublisher

	taskCommandsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "task_command_operations_total",
		Help: "Total task write operations",
	}, []string{"action"})
	outboxPublishedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_command_outbox_published_total",
		Help: "Total outbox events published to RabbitMQ",
	})
	outboxPublishFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "task_command_outbox_publish_failures_total",
		Help: "Total outbox publish failures",
	})
)

func init() {
	prometheus.MustRegister(taskCommandsTotal, outboxPublishedTotal, outboxPublishFailuresTotal)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
		log.Fatalf("AutoMigrate failed: %v", err)
	}
	log.Println("MySQL connected")
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
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		ProjectID   uint   `json:"project_id"`
		Status      string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" || req.ProjectID == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "title and project_id are required"})
		return
	}

	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "backlog"
	}

	task := Task{
		UserID:      userID,
		ProjectID:   req.ProjectID,
		Title:       strings.TrimSpace(req.Title),
		Description: strings.TrimSpace(req.Description),
		Status:      status,
	}

	if err := db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		return enqueueTaskEvent(tx, "task.created", task)
	}); err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to create task"})
		return
	}

	taskCommandsTotal.WithLabelValues("create").Inc()
	jsonResp(w, http.StatusCreated, task)
}

func taskHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var task Task
	if result := db.WithContext(r.Context()).First(&task, id); result.Error != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	switch r.Method {
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
		if task.UserID != userID {
			jsonResp(w, http.StatusForbidden, map[string]string{"error": "task not owned by user"})
			return
		}
		if req.ProjectID != nil && *req.ProjectID != task.ProjectID {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "moving tasks across projects is not supported"})
			return
		}

		if strings.TrimSpace(req.Title) != "" {
			task.Title = strings.TrimSpace(req.Title)
		}
		if req.Description != "" {
			task.Description = strings.TrimSpace(req.Description)
		}
		if strings.TrimSpace(req.Status) != "" {
			task.Status = strings.TrimSpace(req.Status)
		}

		if err := db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			if err := tx.Save(&task).Error; err != nil {
				return err
			}
			return enqueueTaskEvent(tx, "task.updated", task)
		}); err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to update task"})
			return
		}

		taskCommandsTotal.WithLabelValues("update").Inc()
		jsonResp(w, http.StatusOK, task)

	case http.MethodDelete:
		if task.UserID != userID {
			jsonResp(w, http.StatusForbidden, map[string]string{"error": "task not owned by user"})
			return
		}
		if err := db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			if err := tx.Delete(&task).Error; err != nil {
				return err
			}
			return enqueueTaskEvent(tx, "task.deleted", task)
		}); err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete task"})
			return
		}

		taskCommandsTotal.WithLabelValues("delete").Inc()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func main() {
	initMySQL()

	rabbitURL := getEnv("RABBITMQ_URL", "amqp://devboard:devboard123@rabbitmq:5672/")
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
			"service":      "task-command-service",
			"rabbit_ready": publisher.Ready(),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("task-command-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
