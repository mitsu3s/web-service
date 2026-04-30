package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type ProjectMembership struct {
	ID        uint      `json:"id" gorm:"primaryKey"`
	ProjectID uint      `json:"project_id" gorm:"not null;index;uniqueIndex:idx_project_user"`
	UserID    uint      `json:"user_id" gorm:"not null;index;uniqueIndex:idx_project_user"`
	Role      string    `json:"role" gorm:"type:varchar(32);not null"`
	CreatedAt time.Time `json:"created_at"`
}

type membershipCreateRequest struct {
	ProjectID uint   `json:"project_id"`
	UserID    uint   `json:"user_id"`
	Role      string `json:"role"`
}

var (
	db *gorm.DB

	membershipsListedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "membership_service_list_requests_total",
		Help: "Total membership list requests",
	})
	projectAccessChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "membership_service_project_access_checks_total",
		Help: "Total project access checks by result",
	}, []string{"result"})
	internalMembershipWritesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "membership_service_internal_writes_total",
		Help: "Total internal membership write requests",
	})
)

func init() {
	prometheus.MustRegister(membershipsListedTotal, projectAccessChecksTotal, internalMembershipWritesTotal)
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
	if err := db.AutoMigrate(&ProjectMembership{}); err != nil {
		log.Fatalf("AutoMigrate failed: %v", err)
	}
	log.Println("MySQL connected")
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

func normalizeRole(role string) string {
	switch strings.TrimSpace(strings.ToLower(role)) {
	case "", "member":
		return "member"
	case "editor":
		return "editor"
	case "owner":
		return "owner"
	default:
		return ""
	}
}

func membershipsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	var memberships []ProjectMembership
	if err := db.WithContext(r.Context()).
		Where("user_id = ?", userID).
		Order("created_at ASC").
		Find(&memberships).Error; err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to load memberships"})
		return
	}

	membershipsListedTotal.Inc()
	jsonResp(w, http.StatusOK, memberships)
}

func projectAccessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/projects/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[1] != "access" {
		http.NotFound(w, r)
		return
	}

	projectID, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || projectID == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project id"})
		return
	}

	var membership ProjectMembership
	if err := db.WithContext(r.Context()).
		Where("project_id = ? AND user_id = ?", uint(projectID), userID).
		First(&membership).Error; err != nil {
		projectAccessChecksTotal.WithLabelValues("denied").Inc()
		jsonResp(w, http.StatusForbidden, map[string]string{"error": "project not accessible"})
		return
	}

	projectAccessChecksTotal.WithLabelValues("allowed").Inc()
	jsonResp(w, http.StatusOK, map[string]any{
		"allowed":    true,
		"project_id": membership.ProjectID,
		"role":       membership.Role,
	})
}

func internalMembershipsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req membershipCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if req.ProjectID == 0 || req.UserID == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "project_id and user_id are required"})
		return
	}

	role := normalizeRole(req.Role)
	if role == "" {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid role"})
		return
	}

	membership := ProjectMembership{
		ProjectID: req.ProjectID,
		UserID:    req.UserID,
		Role:      role,
	}

	var existing ProjectMembership
	err := db.WithContext(r.Context()).
		Where("project_id = ? AND user_id = ?", req.ProjectID, req.UserID).
		First(&existing).Error
	if err == nil {
		if existing.Role != role {
			existing.Role = role
			if saveErr := db.WithContext(r.Context()).Save(&existing).Error; saveErr != nil {
				jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to update membership"})
				return
			}
		}
		internalMembershipWritesTotal.Inc()
		jsonResp(w, http.StatusOK, existing)
		return
	}

	if err := db.WithContext(r.Context()).Create(&membership).Error; err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to create membership"})
		return
	}

	internalMembershipWritesTotal.Inc()
	jsonResp(w, http.StatusCreated, membership)
}

func main() {
	defer initTracing("membership-service")()

	initDB()

	mux := http.NewServeMux()
	mux.HandleFunc("/memberships", membershipsHandler)
	mux.HandleFunc("/projects/", projectAccessHandler)
	mux.HandleFunc("/internal/memberships", internalMembershipsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "membership-service"})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("membership-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, tracedHTTPHandler("membership-service", mux)))
}
