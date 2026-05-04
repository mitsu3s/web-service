package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

type taskMetadata struct {
	ID          uint   `json:"id"`
	ProjectID   uint   `json:"project_id"`
	UserID      uint   `json:"user_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type membershipAccessResponse struct {
	Allowed   bool   `json:"allowed"`
	ProjectID uint   `json:"project_id"`
	Role      string `json:"role"`
}

type authorizeTaskRequest struct {
	TaskID    uint   `json:"task_id"`
	ProjectID uint   `json:"project_id"`
	Action    string `json:"action"`
}

type authorizeTaskResponse struct {
	Allowed   bool          `json:"allowed"`
	ProjectID uint          `json:"project_id"`
	Role      string        `json:"role"`
	Task      *taskMetadata `json:"task,omitempty"`
	Action    string        `json:"action"`
}

var (
	membershipServiceURL string
	boardQueryServiceURL string
	httpClient           = tracedHTTPClient(5 * time.Second)

	accessChecksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "access_service_checks_total",
		Help: "Total access checks by target and result",
	}, []string{"target", "result"})
)

func init() {
	prometheus.MustRegister(accessChecksTotal)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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

func loadProjectAccess(ctx context.Context, userID, projectID uint) (*membershipAccessResponse, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%d/access", strings.TrimRight(membershipServiceURL, "/"), projectID), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-User-ID", strconv.FormatUint(uint64(userID), 10))

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, resp.StatusCode, fmt.Errorf("membership-service returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var access membershipAccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&access); err != nil {
		return nil, resp.StatusCode, err
	}
	return &access, resp.StatusCode, nil
}

func loadTaskMetadata(ctx context.Context, taskID uint) (*taskMetadata, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/internal/tasks/%d", strings.TrimRight(boardQueryServiceURL, "/"), taskID), nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, resp.StatusCode, fmt.Errorf("board-query-service returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var task taskMetadata
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, resp.StatusCode, err
	}
	return &task, resp.StatusCode, nil
}

func actionAllowed(action, role string, task *taskMetadata, userID uint) bool {
	switch action {
	case "create", "view":
		return role == "owner" || role == "editor" || role == "member"
	case "update":
		return role == "owner" || role == "editor" || (task != nil && task.UserID == userID)
	case "delete":
		return role == "owner" || (task != nil && task.UserID == userID)
	default:
		return false
	}
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

	access, status, err := loadProjectAccess(r.Context(), userID, uint(projectID))
	if err != nil {
		logFromContext(r.Context()).Error("project access lookup failed", zap.Error(err))
		accessChecksTotal.WithLabelValues("project", "error").Inc()
		jsonResp(w, http.StatusBadGateway, map[string]string{"error": "membership-service unavailable"})
		return
	}
	if status == http.StatusForbidden || access == nil {
		accessChecksTotal.WithLabelValues("project", "denied").Inc()
		jsonResp(w, http.StatusForbidden, map[string]string{"error": "project not accessible"})
		return
	}

	accessChecksTotal.WithLabelValues("project", "allowed").Inc()
	jsonResp(w, http.StatusOK, access)
}

func authorizeTaskHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	var req authorizeTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Action) == "" {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	var task *taskMetadata
	projectID := req.ProjectID
	if req.TaskID != 0 {
		var status int
		var err error
		task, status, err = loadTaskMetadata(r.Context(), req.TaskID)
		if err != nil {
			logFromContext(r.Context()).Error("task lookup failed", zap.Error(err))
			accessChecksTotal.WithLabelValues("task", "error").Inc()
			jsonResp(w, http.StatusBadGateway, map[string]string{"error": "board-query-service unavailable"})
			return
		}
		if status == http.StatusNotFound || task == nil {
			accessChecksTotal.WithLabelValues("task", "missing").Inc()
			jsonResp(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		projectID = task.ProjectID
	}

	if projectID == 0 {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "project_id is required"})
		return
	}

	access, status, err := loadProjectAccess(r.Context(), userID, projectID)
	if err != nil {
		logFromContext(r.Context()).Error("membership lookup failed", zap.Error(err))
		accessChecksTotal.WithLabelValues("task", "error").Inc()
		jsonResp(w, http.StatusBadGateway, map[string]string{"error": "membership-service unavailable"})
		return
	}
	if status == http.StatusForbidden || access == nil {
		accessChecksTotal.WithLabelValues("task", "denied").Inc()
		jsonResp(w, http.StatusForbidden, map[string]string{"error": "project not accessible"})
		return
	}

	if !actionAllowed(req.Action, access.Role, task, userID) {
		accessChecksTotal.WithLabelValues("task", "denied").Inc()
		jsonResp(w, http.StatusForbidden, map[string]string{"error": "action denied by access policy"})
		return
	}

	accessChecksTotal.WithLabelValues("task", "allowed").Inc()
	jsonResp(w, http.StatusOK, authorizeTaskResponse{
		Allowed:   true,
		ProjectID: projectID,
		Role:      access.Role,
		Task:      task,
		Action:    req.Action,
	})
}

func main() {
	defer initLogging("access-service")()
	defer initTracing("access-service")()

	membershipServiceURL = getEnv("MEMBERSHIP_SERVICE_URL", "http://membership-service:8080")
	boardQueryServiceURL = getEnv("BOARD_QUERY_SERVICE_URL", "http://board-query-service:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/projects/", projectAccessHandler)
	mux.HandleFunc("/authorize/task", authorizeTaskHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "access-service"})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	logger.Info("access-service listening", zap.String("port", port))
	if err := http.ListenAndServe(":"+port, tracedHTTPHandler("access-service", mux)); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
}
