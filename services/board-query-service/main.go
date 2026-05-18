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
	"gorm.io/gorm/clause"
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

type TaskView struct {
	ID          uint      `json:"id" gorm:"primaryKey;autoIncrement:false;column:task_id"`
	ProjectID   uint      `json:"project_id" gorm:"index"`
	UserID      uint      `json:"user_id" gorm:"index"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	OccurredAt  time.Time `json:"occurred_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedAt   time.Time `json:"created_at"`
}

func (TaskView) TableName() string {
	return "task_views"
}

type sourceTask struct {
	ID          uint      `gorm:"column:id"`
	UserID      uint      `gorm:"column:user_id"`
	ProjectID   uint      `gorm:"column:project_id"`
	Title       string    `gorm:"column:title"`
	Description string    `gorm:"column:description"`
	Status      string    `gorm:"column:status"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (sourceTask) TableName() string {
	return "tasks"
}

var (
	db          *gorm.DB
	rabbitReady atomic.Bool

	queryReadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "board_query_reads_total",
		Help: "Total board-query-service reads by endpoint",
	}, []string{"endpoint"})
	querySyncTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "board_query_projection_events_total",
		Help: "Total projection sync operations by source",
	}, []string{"source", "result"})
	projectionApplyDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "board_query_projection_apply_duration_seconds",
		Help:    "Time spent applying board-query projection writes",
		Buckets: prometheus.DefBuckets,
	}, []string{"source", "result"})
	backfillTasksTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "board_query_backfill_tasks_total",
		Help: "Number of source tasks loaded by the latest board-query backfill",
	})
	backfillDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "board_query_backfill_duration_seconds",
		Help:    "Time spent running board-query startup backfill",
		Buckets: prometheus.DefBuckets,
	})
	rabbitConsumerReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "board_query_rabbitmq_consumer_ready",
		Help: "Whether board-query-service has an active RabbitMQ consumer",
	})
)

func init() {
	prometheus.MustRegister(queryReadsTotal, querySyncTotal, projectionApplyDuration, backfillTasksTotal, backfillDuration, rabbitConsumerReady)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
	if err := db.AutoMigrate(&TaskView{}); err != nil {
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

func upsertTaskView(ctx context.Context, view TaskView) error {
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "task_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"project_id",
			"user_id",
			"title",
			"description",
			"status",
			"occurred_at",
			"updated_at",
		}),
	}).Create(&view).Error
}

func backfillTaskViews(ctx context.Context) {
	start := time.Now()
	result := "success"
	defer func() {
		backfillDuration.Observe(time.Since(start).Seconds())
		querySyncTotal.WithLabelValues("backfill", result).Inc()
	}()

	var tasks []sourceTask
	if err := db.WithContext(ctx).Find(&tasks).Error; err != nil {
		logger.Error("board-query backfill failed", zap.Error(err))
		result = "error"
		return
	}
	backfillTasksTotal.Set(float64(len(tasks)))

	for _, task := range tasks {
		view := TaskView{
			ID:          task.ID,
			ProjectID:   task.ProjectID,
			UserID:      task.UserID,
			Title:       task.Title,
			Description: task.Description,
			Status:      task.Status,
			OccurredAt:  task.UpdatedAt,
			CreatedAt:   task.CreatedAt,
			UpdatedAt:   task.UpdatedAt,
		}
		if err := applyProjection(ctx, "backfill", view); err != nil {
			logger.Error("board-query backfill upsert failed", zap.Error(err))
			result = "error"
			return
		}
	}
}

func consumeEvents(ctx context.Context, rabbitURL string) {
	for {
		if err := consumeOnce(ctx, rabbitURL); err != nil {
			rabbitReady.Store(false)
			logger.Error("board-query consumer stopped", zap.Error(err))
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
	queue, err := ch.QueueDeclare("board-query-service.task-events", true, false, false, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(queue.Name, "task.*", "devboard.events", false, nil); err != nil {
		return err
	}

	deliveries, err := ch.Consume(queue.Name, "board-query-service", false, false, false, false, nil)
	if err != nil {
		return err
	}

	rabbitReady.Store(true)
	rabbitConsumerReady.Set(1)
	defer rabbitConsumerReady.Set(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			if err := handleDelivery(ctx, msg); err != nil {
				logger.Error("failed to update board projection", zap.Error(err))
				_ = msg.Nack(false, true)
				querySyncTotal.WithLabelValues("event", "error").Inc()
				continue
			}
			_ = msg.Ack(false)
			querySyncTotal.WithLabelValues("event", "success").Inc()
		}
	}
}

func handleDelivery(ctx context.Context, msg amqp.Delivery) error {
	var evt TaskEvent
	if err := json.Unmarshal(msg.Body, &evt); err != nil {
		logger.Warn("invalid task event payload", zap.Error(err))
		return nil
	}
	ctx, span := startAMQPConsumerSpan(ctx, msg, evt.Type, evt.EventID, evt.TaskID, evt.ProjectID)
	defer span.End()

	if evt.Type == "task.deleted" {
		err := deleteProjection(ctx, "event", evt.TaskID)
		recordSpanError(span, err)
		return err
	}

	view := TaskView{
		ID:          evt.TaskID,
		ProjectID:   evt.ProjectID,
		UserID:      evt.UserID,
		Title:       evt.Title,
		Description: evt.Description,
		Status:      evt.Status,
		OccurredAt:  evt.OccurredAt,
		UpdatedAt:   evt.OccurredAt,
	}
	err := applyProjection(ctx, "event", view)
	recordSpanError(span, err)
	return err
}

func applyProjection(ctx context.Context, source string, view TaskView) error {
	start := time.Now()
	err := upsertTaskView(ctx, view)
	result := "success"
	if err != nil {
		result = "error"
	}
	projectionApplyDuration.WithLabelValues(source, result).Observe(time.Since(start).Seconds())
	return err
}

func deleteProjection(ctx context.Context, source string, taskID uint) error {
	start := time.Now()
	err := db.WithContext(ctx).Delete(&TaskView{}, "task_id = ?", taskID).Error
	result := "success"
	if err != nil {
		result = "error"
	}
	projectionApplyDuration.WithLabelValues(source, result).Observe(time.Since(start).Seconds())
	return err
}

func tasksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	query := db.WithContext(r.Context()).Where("user_id = ?", userID)
	if raw := strings.TrimSpace(r.URL.Query().Get("project_id")); raw != "" {
		projectID, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
			return
		}
		query = query.Where("project_id = ?", uint(projectID))
	}

	var tasks []TaskView
	if err := query.Order("updated_at desc").Find(&tasks).Error; err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to load tasks"})
		return
	}

	queryReadsTotal.WithLabelValues("tasks").Inc()
	jsonResp(w, http.StatusOK, tasks)
}

func taskByIDHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

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

	var task TaskView
	if err := db.WithContext(r.Context()).
		Where("task_id = ? AND user_id = ?", uint(id), userID).
		First(&task).Error; err != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	queryReadsTotal.WithLabelValues("task").Inc()
	jsonResp(w, http.StatusOK, task)
}

func internalTaskHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/internal/tasks/")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var task TaskView
	if err := db.WithContext(r.Context()).Where("task_id = ?", uint(id)).First(&task).Error; err != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}

	queryReadsTotal.WithLabelValues("internal-task").Inc()
	jsonResp(w, http.StatusOK, task)
}

func main() {
	defer initLogging("board-query-service")()
	defer initTracing("board-query-service")()

	initDB()
	backfillTaskViews(context.Background())

	rabbitURL := getEnv("RABBITMQ_URL", "amqp://devboard:devboard123@rabbitmq:5672/")
	go consumeEvents(context.Background(), rabbitURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", tasksHandler)
	mux.HandleFunc("/tasks/", taskByIDHandler)
	mux.HandleFunc("/internal/tasks/", internalTaskHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
		jsonResp(w, http.StatusOK, map[string]any{
			"status":       "ok",
			"service":      "board-query-service",
			"rabbit_ready": rabbitReady.Load(),
		})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	logger.Info("board-query-service listening", zap.String("port", port))
	if err := http.ListenAndServe(":"+port, tracedHTTPHandler("board-query-service", mux)); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
}
