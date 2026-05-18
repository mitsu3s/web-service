package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

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

type Activity struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	EventID     string    `json:"event_id" gorm:"type:varchar(64);uniqueIndex;not null"`
	TaskID      uint      `json:"task_id" gorm:"index"`
	ProjectID   uint      `json:"project_id" gorm:"index"`
	UserID      uint      `json:"user_id" gorm:"index"`
	EventType   string    `json:"event_type" gorm:"type:varchar(64);index"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	OccurredAt  time.Time `json:"occurred_at"`
	CreatedAt   time.Time `json:"created_at"`
}

var (
	db               *gorm.DB
	accessServiceURL string
	httpClient       = tracedHTTPClient(5 * time.Second)
	rabbitReady      atomic.Bool

	activityEventsStoredTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "activity_events_stored_total",
		Help: "Total activity events stored from RabbitMQ",
	})
	activityReadRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "activity_read_requests_total",
		Help: "Total activity read requests by result",
	}, []string{"result"})
	activityEventStoreDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "activity_event_store_duration_seconds",
		Help:    "Time spent storing activity events consumed from RabbitMQ",
		Buckets: prometheus.DefBuckets,
	}, []string{"result"})
	activityRabbitConsumerReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "activity_rabbitmq_consumer_ready",
		Help: "Whether activity-service has an active RabbitMQ consumer",
	})
)

func init() {
	prometheus.MustRegister(activityEventsStoredTotal, activityReadRequestsTotal, activityEventStoreDuration, activityRabbitConsumerReady)
}

func initDB() {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "devboard:devboard123@tcp(mysql:3306)/devboard?charset=utf8mb4&parseTime=True&loc=Local"
	}

	var err error
	for i := 1; i <= 15; i++ {
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{
			Logger: newZapGormLogger(),
		})
		if err == nil {
			break
		}
		logger.Warn("MySQL not ready", zap.Int("attempt", i), zap.Error(err))
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		logger.Fatal("failed to connect MySQL", zap.Error(err))
	}
	if err := db.AutoMigrate(&Activity{}); err != nil {
		logger.Fatal("AutoMigrate failed", zap.Error(err))
	}
	logger.Info("MySQL connected")
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

func validateProjectAccess(ctx context.Context, userID, projectID uint) error {
	if projectID == 0 || accessServiceURL == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%d/access", strings.TrimRight(accessServiceURL, "/"), projectID), nil)
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
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("project not accessible")
	}
	return fmt.Errorf("access-service returned %s", resp.Status)
}

func consumeEvents(ctx context.Context, rabbitURL string) {
	for {
		if err := consumeOnce(ctx, rabbitURL); err != nil {
			rabbitReady.Store(false)
			activityRabbitConsumerReady.Set(0)
			logger.Error("activity consumer stopped", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func consumeOnce(ctx context.Context, rabbitURL string) error {
	conn, err := amqp.Dial(rabbitURL)
	if err != nil {
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	if err := ch.ExchangeDeclare("devboard.events", "topic", true, false, false, false, nil); err != nil {
		return err
	}
	queue, err := ch.QueueDeclare("activity-service.task-events", true, false, false, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(queue.Name, "task.*", "devboard.events", false, nil); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queue.Name, "activity-service", false, false, false, false, nil)
	if err != nil {
		return err
	}

	rabbitReady.Store(true)
	activityRabbitConsumerReady.Set(1)
	defer activityRabbitConsumerReady.Set(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			if err := handleDelivery(ctx, msg); err != nil {
				logger.Error("failed to store activity event", zap.Error(err))
				_ = msg.Nack(false, true)
				continue
			}
			_ = msg.Ack(false)
		}
	}
}

func handleDelivery(ctx context.Context, msg amqp.Delivery) error {
	start := time.Now()
	result := "success"
	defer func() {
		activityEventStoreDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	}()

	var evt TaskEvent
	if err := json.Unmarshal(msg.Body, &evt); err != nil {
		result = "invalid_payload"
		logger.Warn("invalid activity payload", zap.Error(err))
		return nil
	}
	ctx, span := startAMQPConsumerSpan(ctx, msg, evt.Type, evt.EventID, evt.TaskID, evt.ProjectID)
	defer span.End()

	activity := Activity{
		EventID:     evt.EventID,
		TaskID:      evt.TaskID,
		ProjectID:   evt.ProjectID,
		UserID:      evt.UserID,
		EventType:   evt.Type,
		Title:       evt.Title,
		Description: evt.Description,
		Status:      evt.Status,
		OccurredAt:  evt.OccurredAt,
	}
	if err := db.WithContext(ctx).Create(&activity).Error; err != nil {
		if strings.Contains(err.Error(), "Duplicate entry") {
			result = "duplicate"
			return nil
		}
		result = "error"
		recordSpanError(span, err)
		return err
	}
	activityEventsStoredTotal.Inc()
	return nil
}

func activityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		activityReadRequestsTotal.WithLabelValues("unauthorized").Inc()
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	var projectID *uint
	if raw := strings.TrimSpace(r.URL.Query().Get("project_id")); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			activityReadRequestsTotal.WithLabelValues("bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
			return
		}
		parsed := uint(id)
		projectID = &parsed
		if err := validateProjectAccess(r.Context(), userID, parsed); err != nil {
			activityReadRequestsTotal.WithLabelValues("forbidden").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	query := db.WithContext(r.Context()).Where("user_id = ?", userID)
	if projectID != nil {
		query = query.Where("project_id = ?", *projectID)
	}

	var activities []Activity
	if err := query.Order("occurred_at desc").Limit(limit).Find(&activities).Error; err != nil {
		activityReadRequestsTotal.WithLabelValues("error").Inc()
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to load activity"})
		return
	}
	activityReadRequestsTotal.WithLabelValues("ok").Inc()
	jsonResp(w, http.StatusOK, activities)
}

func main() {
	defer initLogging("activity-service")()
	defer initTracing("activity-service")()

	initDB()

	accessServiceURL = os.Getenv("ACCESS_SERVICE_URL")
	rabbitURL := os.Getenv("RABBITMQ_URL")
	if rabbitURL == "" {
		rabbitURL = "amqp://devboard:devboard123@rabbitmq:5672/"
	}
	go consumeEvents(context.Background(), rabbitURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/activity", activityHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
		jsonResp(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"service":      "activity-service",
			"rabbit_ready": rabbitReady.Load(),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger.Info("activity-service listening", zap.String("port", port))
	if err := http.ListenAndServe(":"+port, tracedHTTPHandler("activity-service", mux)); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
}
