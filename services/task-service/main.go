package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---- Models ----------------------------------------------------------------

type Task struct {
	ID          uint      `json:"id"          gorm:"primaryKey"`
	UserID      uint      `json:"user_id"     gorm:"not null;index"`
	Title       string    `json:"title"       gorm:"not null"`
	Description string    `json:"description"`
	Status      string    `json:"status"      gorm:"default:'pending'"` // pending | in_progress | done
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ---- Globals ---------------------------------------------------------------

var (
	db  *gorm.DB
	rdb *redis.Client

	tasksCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tasks_created_total",
		Help: "Total tasks created",
	})
	cacheHitsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cache_hits_total",
		Help: "Redis cache hits/misses",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(tasksCreatedTotal, cacheHitsTotal)
}

// ---- DB --------------------------------------------------------------------

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
	if err := db.AutoMigrate(&Task{}); err != nil {
		log.Fatalf("AutoMigrate: %v", err)
	}
	log.Println("MySQL connected")
}

func initRedis() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "redis-master:6379"
	}
	rdb = redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("Redis not available: %v (cache will be skipped)", err)
	} else {
		log.Println("Redis connected")
	}
}

// ---- Helpers ---------------------------------------------------------------

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func getUserID(r *http.Request) uint {
	id, _ := strconv.ParseUint(r.Header.Get("X-User-ID"), 10, 64)
	return uint(id)
}

func cacheKey(userID uint) string {
	return fmt.Sprintf("tasks:user:%d", userID)
}

func invalidateCache(ctx context.Context, userID uint) {
	rdb.Del(ctx, cacheKey(userID))
}

func publishEvent(ctx context.Context, userID uint, event string) {
	msg := fmt.Sprintf(`{"user_id":%d,"event":"%s","ts":"%s"}`,
		userID, event, time.Now().UTC().Format(time.RFC3339))
	rdb.Publish(ctx, "task-events", msg)
}

// ---- Handlers --------------------------------------------------------------

// GET /tasks  POST /tasks
func tasksHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := getUserID(r)

	switch r.Method {
	case http.MethodGet:
		// Try Redis cache
		if cached, err := rdb.Get(ctx, cacheKey(userID)).Result(); err == nil {
			cacheHitsTotal.WithLabelValues("hit").Inc()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			fmt.Fprint(w, cached)
			return
		}
		cacheHitsTotal.WithLabelValues("miss").Inc()

		var tasks []Task
		db.Where("user_id = ?", userID).Order("created_at desc").Find(&tasks)
		b, _ := json.Marshal(tasks)
		rdb.Set(ctx, cacheKey(userID), b, 5*time.Minute)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(b)

	case http.MethodPost:
		var req struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
			return
		}
		task := Task{UserID: userID, Title: req.Title, Description: req.Description, Status: "pending"}
		db.Create(&task)
		invalidateCache(ctx, userID)
		publishEvent(ctx, userID, "task_created")
		tasksCreatedTotal.Inc()
		jsonResp(w, http.StatusCreated, task)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// GET /tasks/{id}  PUT /tasks/{id}  DELETE /tasks/{id}
func taskHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := getUserID(r)

	// Extract ID: path is /tasks/{id}
	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}

	var task Task
	if result := db.Where("id = ? AND user_id = ?", id, userID).First(&task); result.Error != nil {
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
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Title != "" {
			task.Title = req.Title
		}
		if req.Description != "" {
			task.Description = req.Description
		}
		if req.Status != "" {
			task.Status = req.Status
		}
		db.Save(&task)
		invalidateCache(ctx, userID)
		publishEvent(ctx, userID, "task_updated")
		jsonResp(w, http.StatusOK, task)

	case http.MethodDelete:
		db.Delete(&task)
		invalidateCache(ctx, userID)
		publishEvent(ctx, userID, "task_deleted")
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- Main ------------------------------------------------------------------

func main() {
	initMySQL()
	initRedis()

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", tasksHandler)
	mux.HandleFunc("/tasks/", taskHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","service":"task-service"}`)
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("task-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
