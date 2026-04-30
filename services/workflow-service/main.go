package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type projectMetadata struct {
	ID              uint   `json:"id"`
	WorkflowProfile string `json:"workflow_profile"`
}

type workflowDefinition struct {
	Name             string              `json:"name"`
	DefaultStatus    string              `json:"default_status"`
	Statuses         []string            `json:"statuses"`
	Transitions      map[string][]string `json:"transitions"`
	TerminalStatuses []string            `json:"terminal_statuses"`
}

type initializeTaskRequest struct {
	RequestedStatus string `json:"requested_status"`
}

type validateTransitionRequest struct {
	CurrentStatus string `json:"current_status"`
	TargetStatus  string `json:"target_status"`
}

type workflowResponse struct {
	ProfileName         string              `json:"profile_name"`
	Allowed             bool                `json:"allowed"`
	NormalizedStatus    string              `json:"normalized_status"`
	AllowedNextStatuses []string            `json:"allowed_next_statuses"`
	Workflow            *workflowDefinition `json:"workflow,omitempty"`
}

var (
	projectServiceURL string
	httpClient        = tracedHTTPClient(0)

	workflowRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "workflow_service_requests_total",
		Help: "Total workflow-service requests by endpoint and result",
	}, []string{"endpoint", "result"})
	profiles = map[string]workflowDefinition{
		"team-kanban": {
			Name:          "team-kanban",
			DefaultStatus: "backlog",
			Statuses:      []string{"backlog", "planned", "in_progress", "blocked", "review", "done"},
			Transitions: map[string][]string{
				"backlog":     {"planned", "in_progress"},
				"planned":     {"in_progress", "blocked"},
				"in_progress": {"blocked", "review", "done"},
				"blocked":     {"planned", "in_progress"},
				"review":      {"in_progress", "blocked", "done"},
				"done":        {"planned"},
			},
			TerminalStatuses: []string{"done"},
		},
		"fast-track": {
			Name:          "fast-track",
			DefaultStatus: "planned",
			Statuses:      []string{"planned", "in_progress", "done"},
			Transitions: map[string][]string{
				"planned":     {"in_progress"},
				"in_progress": {"done", "planned"},
				"done":        {"planned"},
			},
			TerminalStatuses: []string{"done"},
		},
	}
)

func init() {
	prometheus.MustRegister(workflowRequestsTotal)
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

func containsStatus(workflow workflowDefinition, status string) bool {
	for _, candidate := range workflow.Statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func loadWorkflow(ctx context.Context, projectID uint) (string, workflowDefinition, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%d", strings.TrimRight(projectServiceURL, "/"), projectID), nil)
	if err != nil {
		return "", workflowDefinition{}, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", workflowDefinition{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", workflowDefinition{}, upstreamError{status: http.StatusNotFound, body: "project not found"}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", workflowDefinition{}, upstreamError{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}

	var project projectMetadata
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return "", workflowDefinition{}, err
	}

	profileName := strings.TrimSpace(project.WorkflowProfile)
	if profileName == "" {
		profileName = "team-kanban"
	}
	workflow, ok := profiles[profileName]
	if !ok {
		workflow = profiles["team-kanban"]
		profileName = workflow.Name
	}
	return profileName, workflow, nil
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

func parseProjectPath(path string) (uint, string, bool) {
	trimmed := strings.Trim(strings.TrimPrefix(path, "/projects/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return 0, "", false
	}
	projectID, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || projectID == 0 {
		return 0, "", false
	}
	return uint(projectID), strings.Join(parts[1:], "/"), true
}

func projectScopedHandler(w http.ResponseWriter, r *http.Request) {
	projectID, suffix, ok := parseProjectPath(r.URL.Path)
	if !ok {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project path"})
		return
	}

	profileName, workflow, err := loadWorkflow(r.Context(), projectID)
	if err != nil {
		if upstreamErr, ok := err.(upstreamError); ok {
			workflowRequestsTotal.WithLabelValues(suffix, "error").Inc()
			jsonResp(w, upstreamErr.status, map[string]string{"error": upstreamErr.Error()})
			return
		}
		workflowRequestsTotal.WithLabelValues(suffix, "error").Inc()
		jsonResp(w, http.StatusBadGateway, map[string]string{"error": "project-service unavailable"})
		return
	}

	switch suffix {
	case "workflow":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		workflowRequestsTotal.WithLabelValues("workflow", "ok").Inc()
		jsonResp(w, http.StatusOK, workflow)
		return

	case "tasks/initialize":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req initializeTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			workflowRequestsTotal.WithLabelValues("initialize", "bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}

		status := strings.TrimSpace(req.RequestedStatus)
		if status == "" {
			status = workflow.DefaultStatus
		}
		if !containsStatus(workflow, status) {
			workflowRequestsTotal.WithLabelValues("initialize", "bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "unknown workflow status"})
			return
		}

		workflowRequestsTotal.WithLabelValues("initialize", "ok").Inc()
		jsonResp(w, http.StatusOK, workflowResponse{
			ProfileName:         profileName,
			Allowed:             true,
			NormalizedStatus:    status,
			AllowedNextStatuses: append([]string(nil), workflow.Transitions[status]...),
			Workflow:            &workflow,
		})
		return

	case "tasks/validate-transition":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req validateTransitionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			workflowRequestsTotal.WithLabelValues("validate", "bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}

		current := strings.TrimSpace(req.CurrentStatus)
		target := strings.TrimSpace(req.TargetStatus)
		if current == "" || target == "" {
			workflowRequestsTotal.WithLabelValues("validate", "bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "current_status and target_status are required"})
			return
		}
		if !containsStatus(workflow, current) || !containsStatus(workflow, target) {
			workflowRequestsTotal.WithLabelValues("validate", "bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "unknown workflow status"})
			return
		}
		if current != target {
			allowed := false
			for _, candidate := range workflow.Transitions[current] {
				if candidate == target {
					allowed = true
					break
				}
			}
			if !allowed {
				workflowRequestsTotal.WithLabelValues("validate", "rejected").Inc()
				jsonResp(w, http.StatusUnprocessableEntity, map[string]any{
					"error":                 "workflow transition not allowed",
					"allowed_next_statuses": workflow.Transitions[current],
				})
				return
			}
		}

		workflowRequestsTotal.WithLabelValues("validate", "ok").Inc()
		jsonResp(w, http.StatusOK, workflowResponse{
			ProfileName:         profileName,
			Allowed:             true,
			NormalizedStatus:    target,
			AllowedNextStatuses: append([]string(nil), workflow.Transitions[target]...),
			Workflow:            &workflow,
		})
		return
	}

	http.NotFound(w, r)
}

func main() {
	defer initTracing("workflow-service")()

	projectServiceURL = getEnv("PROJECT_SERVICE_URL", "http://project-service:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/projects/", projectScopedHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "workflow-service"})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("workflow-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, tracedHTTPHandler("workflow-service", mux)))
}
