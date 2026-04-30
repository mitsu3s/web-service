package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type taskMetadata struct {
	ID          uint   `json:"id"`
	ProjectID   uint   `json:"project_id"`
	UserID      uint   `json:"user_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type authorizeTaskResponse struct {
	Allowed   bool          `json:"allowed"`
	ProjectID uint          `json:"project_id"`
	Role      string        `json:"role"`
	Task      *taskMetadata `json:"task,omitempty"`
	Action    string        `json:"action"`
}

type workflowResponse struct {
	ProfileName         string              `json:"profile_name"`
	Allowed             bool                `json:"allowed"`
	NormalizedStatus    string              `json:"normalized_status"`
	AllowedNextStatuses []string            `json:"allowed_next_statuses"`
	Workflow            *workflowDefinition `json:"workflow,omitempty"`
}

type workflowDefinition struct {
	Name             string              `json:"name"`
	DefaultStatus    string              `json:"default_status"`
	Statuses         []string            `json:"statuses"`
	Transitions      map[string][]string `json:"transitions"`
	TerminalStatuses []string            `json:"terminal_statuses"`
}

type commandTask struct {
	ID          uint      `json:"id"`
	ProjectID   uint      `json:"project_id"`
	UserID      uint      `json:"user_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type upstreamStatusError struct {
	StatusCode int
	Body       string
}

func (e upstreamStatusError) Error() string {
	if e.Body != "" {
		return e.Body
	}
	return http.StatusText(e.StatusCode)
}

var (
	httpClient = tracedHTTPClient(10 * time.Second)

	accessServiceURL      string
	workflowServiceURL    string
	taskCommandServiceURL string

	orchestratedCommandsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "task_orchestrator_commands_total",
		Help: "Total task-orchestrator commands by action and result",
	}, []string{"action", "result"})
)

func init() {
	prometheus.MustRegister(orchestratedCommandsTotal)
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

func doJSON[T any](ctx context.Context, method, url string, userID uint, payload any, out *T) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if userID != 0 {
		req.Header.Set("X-User-ID", strconv.FormatUint(uint64(userID), 10))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return upstreamStatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(bodyBytes))}
	}

	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	var statusErr upstreamStatusError
	if errors.As(err, &statusErr) {
		body := statusErr.Body
		if body == "" {
			body = http.StatusText(statusErr.StatusCode)
		}
		if strings.HasPrefix(strings.TrimSpace(body), "{") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusErr.StatusCode)
			_, _ = w.Write([]byte(body))
			return
		}
		jsonResp(w, statusErr.StatusCode, map[string]string{"error": body})
		return
	}
	log.Printf("task-orchestrator upstream failed: %v", err)
	jsonResp(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
}

func authorizeTask(ctx context.Context, userID uint, payload any) (*authorizeTaskResponse, error) {
	var resp authorizeTaskResponse
	err := doJSON(ctx, http.MethodPost, strings.TrimRight(accessServiceURL, "/")+"/authorize/task", userID, payload, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func initializeWorkflow(ctx context.Context, userID, projectID uint, requestedStatus string) (*workflowResponse, error) {
	payload := map[string]string{}
	if strings.TrimSpace(requestedStatus) != "" {
		payload["requested_status"] = strings.TrimSpace(requestedStatus)
	}
	var resp workflowResponse
	err := doJSON(ctx, http.MethodPost, fmt.Sprintf("%s/projects/%d/tasks/initialize", strings.TrimRight(workflowServiceURL, "/"), projectID), userID, payload, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func validateTransition(ctx context.Context, userID, projectID uint, currentStatus, targetStatus string) (*workflowResponse, error) {
	var resp workflowResponse
	err := doJSON(ctx, http.MethodPost, fmt.Sprintf("%s/projects/%d/tasks/validate-transition", strings.TrimRight(workflowServiceURL, "/"), projectID), userID, map[string]string{
		"current_status": strings.TrimSpace(currentStatus),
		"target_status":  strings.TrimSpace(targetStatus),
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func createTaskHandler(w http.ResponseWriter, r *http.Request) {
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

	if _, err := authorizeTask(r.Context(), userID, map[string]any{
		"project_id": req.ProjectID,
		"action":     "create",
	}); err != nil {
		orchestratedCommandsTotal.WithLabelValues("create", "rejected").Inc()
		writeUpstreamError(w, err)
		return
	}

	workflowResp, err := initializeWorkflow(r.Context(), userID, req.ProjectID, req.Status)
	if err != nil {
		orchestratedCommandsTotal.WithLabelValues("create", "rejected").Inc()
		writeUpstreamError(w, err)
		return
	}

	var created commandTask
	if err := doJSON(r.Context(), http.MethodPost, strings.TrimRight(taskCommandServiceURL, "/")+"/tasks", userID, map[string]any{
		"title":       strings.TrimSpace(req.Title),
		"description": strings.TrimSpace(req.Description),
		"project_id":  req.ProjectID,
		"status":      workflowResp.NormalizedStatus,
	}, &created); err != nil {
		orchestratedCommandsTotal.WithLabelValues("create", "error").Inc()
		writeUpstreamError(w, err)
		return
	}

	orchestratedCommandsTotal.WithLabelValues("create", "ok").Inc()
	jsonResp(w, http.StatusCreated, created)
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

		authResp, err := authorizeTask(r.Context(), userID, map[string]any{
			"task_id": id,
			"action":  "update",
		})
		if err != nil {
			orchestratedCommandsTotal.WithLabelValues("update", "rejected").Inc()
			writeUpstreamError(w, err)
			return
		}
		if authResp.Task == nil {
			jsonResp(w, http.StatusBadGateway, map[string]string{"error": "access-service returned no task context"})
			return
		}

		if req.ProjectID != nil && *req.ProjectID != authResp.Task.ProjectID {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "moving tasks across projects is not supported"})
			return
		}

		status := strings.TrimSpace(req.Status)
		if status != "" && status != authResp.Task.Status {
			workflowResp, workflowErr := validateTransition(r.Context(), userID, authResp.Task.ProjectID, authResp.Task.Status, status)
			if workflowErr != nil {
				orchestratedCommandsTotal.WithLabelValues("update", "rejected").Inc()
				writeUpstreamError(w, workflowErr)
				return
			}
			status = workflowResp.NormalizedStatus
		}

		payload := map[string]any{}
		if strings.TrimSpace(req.Title) != "" {
			payload["title"] = strings.TrimSpace(req.Title)
		}
		if req.Description != "" {
			payload["description"] = strings.TrimSpace(req.Description)
		}
		if status != "" {
			payload["status"] = status
		}

		var updated commandTask
		if err := doJSON(r.Context(), http.MethodPut, fmt.Sprintf("%s/tasks/%d", strings.TrimRight(taskCommandServiceURL, "/"), id), userID, payload, &updated); err != nil {
			orchestratedCommandsTotal.WithLabelValues("update", "error").Inc()
			writeUpstreamError(w, err)
			return
		}

		orchestratedCommandsTotal.WithLabelValues("update", "ok").Inc()
		jsonResp(w, http.StatusOK, updated)

	case http.MethodDelete:
		if _, err := authorizeTask(r.Context(), userID, map[string]any{
			"task_id": id,
			"action":  "delete",
		}); err != nil {
			orchestratedCommandsTotal.WithLabelValues("delete", "rejected").Inc()
			writeUpstreamError(w, err)
			return
		}

		if err := doJSON[map[string]any](r.Context(), http.MethodDelete, fmt.Sprintf("%s/tasks/%d", strings.TrimRight(taskCommandServiceURL, "/"), id), userID, nil, nil); err != nil {
			orchestratedCommandsTotal.WithLabelValues("delete", "error").Inc()
			writeUpstreamError(w, err)
			return
		}

		orchestratedCommandsTotal.WithLabelValues("delete", "ok").Inc()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func main() {
	defer initTracing("task-orchestrator-service")()

	accessServiceURL = getEnv("ACCESS_SERVICE_URL", "http://access-service:8080")
	workflowServiceURL = getEnv("WORKFLOW_SERVICE_URL", "http://workflow-service:8080")
	taskCommandServiceURL = getEnv("TASK_COMMAND_SERVICE_URL", "http://task-command-service:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", createTaskHandler)
	mux.HandleFunc("/tasks/", taskHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "task-orchestrator-service"})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("task-orchestrator-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, tracedHTTPHandler("task-orchestrator-service", mux)))
}
