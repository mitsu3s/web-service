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

type Project struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Name        string    `json:"name" gorm:"type:varchar(255);not null"`
	Description string    `json:"description"`
	OwnerUserID uint      `json:"owner_user_id" gorm:"not null;index"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ProjectMember struct {
	ID        uint   `gorm:"primaryKey"`
	ProjectID uint   `gorm:"not null;index"`
	UserID    uint   `gorm:"not null;index"`
	Role      string `gorm:"type:varchar(32);not null"`
	CreatedAt time.Time
}

var (
	db *gorm.DB

	projectsCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "projects_created_total",
		Help: "Total projects created",
	})
	projectAccessChecksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "project_access_checks_total",
		Help: "Total project access checks",
	})
)

func init() {
	prometheus.MustRegister(projectsCreatedTotal, projectAccessChecksTotal)
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
	if err := db.AutoMigrate(&Project{}, &ProjectMember{}); err != nil {
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

func loadProjectForUser(projectID, userID uint) (*Project, error) {
	projectAccessChecksTotal.Inc()

	var project Project
	err := db.
		Table("projects").
		Joins("JOIN project_members ON project_members.project_id = projects.id").
		Where("projects.id = ? AND project_members.user_id = ?", projectID, userID).
		First(&project).Error
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func projectsHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		var projects []Project
		if err := db.
			Table("projects").
			Joins("JOIN project_members ON project_members.project_id = projects.id").
			Where("project_members.user_id = ?", userID).
			Order("projects.created_at ASC").
			Find(&projects).Error; err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to load projects"})
			return
		}
		jsonResp(w, http.StatusOK, projects)

	case http.MethodPost:
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}

		project := Project{
			Name:        strings.TrimSpace(req.Name),
			Description: strings.TrimSpace(req.Description),
			OwnerUserID: userID,
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&project).Error; err != nil {
				return err
			}
			return tx.Create(&ProjectMember{
				ProjectID: project.ID,
				UserID:    userID,
				Role:      "owner",
			}).Error
		}); err != nil {
			jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "failed to create project"})
			return
		}

		projectsCreatedTotal.Inc()
		jsonResp(w, http.StatusCreated, project)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func projectByIDHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/projects/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project id"})
		return
	}

	project, err := loadProjectForUser(uint(id), userID)
	if err != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	if len(parts) == 1 && r.Method == http.MethodGet {
		jsonResp(w, http.StatusOK, project)
		return
	}
	if len(parts) == 2 && parts[1] == "access" && r.Method == http.MethodGet {
		jsonResp(w, http.StatusOK, map[string]any{
			"allowed":    true,
			"project_id": project.ID,
			"name":       project.Name,
		})
		return
	}

	http.NotFound(w, r)
}

func main() {
	initDB()

	mux := http.NewServeMux()
	mux.HandleFunc("/projects", projectsHandler)
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

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("project-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
