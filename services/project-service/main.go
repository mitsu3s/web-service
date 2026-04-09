package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

type Project struct {
	ID              uint      `json:"id" gorm:"primaryKey"`
	Name            string    `json:"name" gorm:"type:varchar(255);not null"`
	Description     string    `json:"description"`
	OwnerUserID     uint      `json:"owner_user_id" gorm:"not null;index"`
	WorkflowProfile string    `json:"workflow_profile" gorm:"type:varchar(64);not null;default:'team-kanban'"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type projectLookupRequest struct {
	IDs []uint `json:"ids"`
}

type membershipCreateRequest struct {
	ProjectID uint   `json:"project_id"`
	UserID    uint   `json:"user_id"`
	Role      string `json:"role"`
}

var (
	db                   *gorm.DB
	httpClient           = &http.Client{Timeout: 5 * time.Second}
	membershipServiceURL string

	projectsCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "projects_created_total",
		Help: "Total projects created",
	})
	projectLookupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "project_service_lookups_total",
		Help: "Total project lookups by endpoint",
	}, []string{"endpoint"})
)

func init() {
	prometheus.MustRegister(projectsCreatedTotal, projectLookupsTotal)
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
	if err := db.AutoMigrate(&Project{}); err != nil {
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

func ensureOwnerMembership(ctx context.Context, projectID, userID uint) error {
	payload := membershipCreateRequest{
		ProjectID: projectID,
		UserID:    userID,
		Role:      "owner",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(membershipServiceURL, "/")+"/internal/memberships", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return upstreamError{status: resp.StatusCode, body: strings.TrimSpace(string(bodyBytes))}
	}
	return nil
}

type upstreamError struct {
	status int
	body   string
}

func (e upstreamError) Error() string {
	if e.body != "" {
		return e.body
	}
	return http.StatusText(e.status)
}

func projectsHandler(w http.ResponseWriter, r *http.Request) {
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
		Name            string `json:"name"`
		Description     string `json:"description"`
		WorkflowProfile string `json:"workflow_profile"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	workflowProfile := strings.TrimSpace(req.WorkflowProfile)
	if workflowProfile == "" {
		workflowProfile = "team-kanban"
	}

	project := Project{
		Name:            strings.TrimSpace(req.Name),
		Description:     strings.TrimSpace(req.Description),
		OwnerUserID:     userID,
		WorkflowProfile: workflowProfile,
	}
	if err := db.WithContext(r.Context()).Create(&project).Error; err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to create project"})
		return
	}

	if err := ensureOwnerMembership(r.Context(), project.ID, userID); err != nil {
		_ = db.WithContext(r.Context()).Delete(&Project{}, project.ID).Error
		log.Printf("failed to create owner membership for project %d: %v", project.ID, err)
		jsonResp(w, http.StatusBadGateway, map[string]string{"error": "membership-service unavailable"})
		return
	}

	projectsCreatedTotal.Inc()
	jsonResp(w, http.StatusCreated, project)
}

func projectLookupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req projectLookupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if len(req.IDs) == 0 {
		jsonResp(w, http.StatusOK, []Project{})
		return
	}

	var projects []Project
	if err := db.WithContext(r.Context()).Where("id IN ?", req.IDs).Find(&projects).Error; err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to load projects"})
		return
	}

	projectByID := make(map[uint]Project, len(projects))
	for _, project := range projects {
		projectByID[project.ID] = project
	}

	ordered := make([]Project, 0, len(req.IDs))
	for _, id := range req.IDs {
		if project, ok := projectByID[id]; ok {
			ordered = append(ordered, project)
		}
	}

	projectLookupsTotal.WithLabelValues("lookup").Inc()
	jsonResp(w, http.StatusOK, ordered)
}

func projectByIDHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, "/projects/")
	id, err := strconv.ParseUint(strings.Trim(idStr, "/"), 10, 64)
	if err != nil || id == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project id"})
		return
	}

	var project Project
	if err := db.WithContext(r.Context()).First(&project, uint(id)).Error; err != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	projectLookupsTotal.WithLabelValues("by_id").Inc()
	jsonResp(w, http.StatusOK, project)
}

func main() {
	initDB()
	membershipServiceURL = getEnv("MEMBERSHIP_SERVICE_URL", "http://membership-service:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/projects", projectsHandler)
	mux.HandleFunc("/projects/lookup", projectLookupHandler)
	mux.HandleFunc("/projects/", projectByIDHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "project-service"})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("project-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
