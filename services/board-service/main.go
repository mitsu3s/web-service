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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type ProjectMembership struct {
	ProjectID uint      `json:"project_id"`
	UserID    uint      `json:"user_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type Project struct {
	ID              uint      `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	OwnerUserID     uint      `json:"owner_user_id"`
	WorkflowProfile string    `json:"workflow_profile"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type ProjectSummary struct {
	ID              uint      `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	OwnerUserID     uint      `json:"owner_user_id"`
	WorkflowProfile string    `json:"workflow_profile"`
	Role            string    `json:"role"`
	CreatedAt       time.Time `json:"created_at"`
}

type Task struct {
	ID          uint      `json:"id"`
	ProjectID   uint      `json:"project_id"`
	UserID      uint      `json:"user_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	OccurredAt  time.Time `json:"occurred_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type Activity struct {
	ID          uint      `json:"id"`
	EventID     string    `json:"event_id"`
	TaskID      uint      `json:"task_id"`
	ProjectID   uint      `json:"project_id"`
	UserID      uint      `json:"user_id"`
	EventType   string    `json:"event_type"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	OccurredAt  time.Time `json:"occurred_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type workflowDefinition struct {
	Name             string              `json:"name"`
	DefaultStatus    string              `json:"default_status"`
	Statuses         []string            `json:"statuses"`
	Transitions      map[string][]string `json:"transitions"`
	TerminalStatuses []string            `json:"terminal_statuses"`
}

type dashboardResponse struct {
	Projects        []ProjectSummary    `json:"projects"`
	SelectedProject *ProjectSummary     `json:"selected_project,omitempty"`
	Tasks           []Task              `json:"tasks"`
	Activities      []Activity          `json:"activity"`
	Workflow        *workflowDefinition `json:"workflow,omitempty"`
}

type searchTask struct {
	TaskID      uint      `json:"task_id"`
	ProjectID   uint      `json:"project_id"`
	UserID      uint      `json:"user_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	EventType   string    `json:"event_type"`
	OccurredAt  time.Time `json:"occurred_at"`
	Score       float64   `json:"score"`
}

type searchResponse struct {
	Query   string       `json:"query"`
	Total   int          `json:"total"`
	Results []searchTask `json:"results"`
}

type upstreamStatusError struct {
	StatusCode int
	Body       string
}

func (e upstreamStatusError) Error() string {
	if e.Body != "" {
		return e.Body
	}
	return fmt.Sprintf("upstream returned %d", e.StatusCode)
}

var (
	httpClient = &http.Client{Timeout: 10 * time.Second}

	membershipServiceURL string
	projectServiceURL    string
	accessServiceURL     string
	boardQueryServiceURL string
	activityServiceURL   string
	searchServiceURL     string
	workflowServiceURL   string

	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "board_service_http_requests_total",
		Help: "Total board-service requests by endpoint and result",
	}, []string{"method", "path", "status"})
	dashboardLoadsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "board_service_dashboard_loads_total",
		Help: "Total dashboard loads",
	})
	projectListsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "board_service_project_lists_total",
		Help: "Total project list loads",
	})
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, dashboardLoadsTotal, projectListsTotal)
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

func fetchJSON[T any](ctx context.Context, method, url string, userID uint, payload any, out *T) error {
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

func ensureProjectAccess(ctx context.Context, userID, projectID uint) error {
	if projectID == 0 {
		return nil
	}
	var resp map[string]any
	return fetchJSON(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%d/access", strings.TrimRight(accessServiceURL, "/"), projectID), userID, nil, &resp)
}

func loadProjects(ctx context.Context, userID uint) ([]ProjectSummary, error) {
	var memberships []ProjectMembership
	if err := fetchJSON(ctx, http.MethodGet, strings.TrimRight(membershipServiceURL, "/")+"/memberships", userID, nil, &memberships); err != nil {
		return nil, err
	}
	if len(memberships) == 0 {
		return []ProjectSummary{}, nil
	}

	projectIDs := make([]uint, 0, len(memberships))
	roleByProject := make(map[uint]string, len(memberships))
	for _, membership := range memberships {
		projectIDs = append(projectIDs, membership.ProjectID)
		roleByProject[membership.ProjectID] = membership.Role
	}

	var projects []Project
	if err := fetchJSON(ctx, http.MethodPost, strings.TrimRight(projectServiceURL, "/")+"/projects/lookup", 0, map[string]any{"ids": projectIDs}, &projects); err != nil {
		return nil, err
	}

	projectByID := make(map[uint]Project, len(projects))
	for _, project := range projects {
		projectByID[project.ID] = project
	}

	summaries := make([]ProjectSummary, 0, len(projectIDs))
	for _, projectID := range projectIDs {
		project, ok := projectByID[projectID]
		if !ok {
			continue
		}
		summaries = append(summaries, ProjectSummary{
			ID:              project.ID,
			Name:            project.Name,
			Description:     project.Description,
			OwnerUserID:     project.OwnerUserID,
			WorkflowProfile: project.WorkflowProfile,
			Role:            roleByProject[project.ID],
			CreatedAt:       project.CreatedAt,
		})
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.Before(summaries[j].CreatedAt)
	})
	return summaries, nil
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
	log.Printf("board-service upstream failed: %v", err)
	jsonResp(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
}

func projectsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	projects, err := loadProjects(r.Context(), userID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	projectListsTotal.Inc()
	jsonResp(w, http.StatusOK, projects)
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	projects, err := loadProjects(r.Context(), userID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	resp := dashboardResponse{
		Projects:   projects,
		Tasks:      []Task{},
		Activities: []Activity{},
	}
	if len(projects) == 0 {
		dashboardLoadsTotal.Inc()
		jsonResp(w, http.StatusOK, resp)
		return
	}

	var selectedProject *ProjectSummary
	if raw := strings.TrimSpace(r.URL.Query().Get("project_id")); raw != "" {
		projectID, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || projectID == 0 {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
			return
		}
		if err := ensureProjectAccess(r.Context(), userID, uint(projectID)); err != nil {
			writeUpstreamError(w, err)
			return
		}
		for i := range projects {
			if projects[i].ID == uint(projectID) {
				selectedProject = &projects[i]
				break
			}
		}
		if selectedProject == nil {
			jsonResp(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
	} else {
		selectedProject = &projects[0]
	}
	resp.SelectedProject = selectedProject

	activityLimit := strings.TrimSpace(r.URL.Query().Get("activity_limit"))
	if activityLimit == "" {
		activityLimit = "12"
	}

	var (
		tasks      []Task
		activities []Activity
		workflow   workflowDefinition
		fetchErr   error
		errMu      sync.Mutex
		wg         sync.WaitGroup
	)

	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if fetchErr == nil {
			fetchErr = err
		}
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		recordErr(fetchJSON(r.Context(), http.MethodGet, fmt.Sprintf("%s/tasks?project_id=%d", strings.TrimRight(boardQueryServiceURL, "/"), selectedProject.ID), userID, nil, &tasks))
	}()
	go func() {
		defer wg.Done()
		recordErr(fetchJSON(r.Context(), http.MethodGet, fmt.Sprintf("%s/activity?project_id=%d&limit=%s", strings.TrimRight(activityServiceURL, "/"), selectedProject.ID, activityLimit), userID, nil, &activities))
	}()
	go func() {
		defer wg.Done()
		recordErr(fetchJSON(r.Context(), http.MethodGet, fmt.Sprintf("%s/projects/%d/workflow", strings.TrimRight(workflowServiceURL, "/"), selectedProject.ID), userID, nil, &workflow))
	}()
	wg.Wait()

	if fetchErr != nil {
		writeUpstreamError(w, fetchErr)
		return
	}

	resp.Tasks = tasks
	resp.Activities = activities
	resp.Workflow = &workflow
	dashboardLoadsTotal.Inc()
	jsonResp(w, http.StatusOK, resp)
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

	if rawProjectID := strings.TrimSpace(r.URL.Query().Get("project_id")); rawProjectID != "" {
		projectID, err := strconv.ParseUint(rawProjectID, 10, 64)
		if err != nil || projectID == 0 {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
			return
		}
		if err := ensureProjectAccess(r.Context(), userID, uint(projectID)); err != nil {
			writeUpstreamError(w, err)
			return
		}
	}

	var tasks []Task
	if err := fetchJSON(r.Context(), http.MethodGet, strings.TrimRight(boardQueryServiceURL, "/")+r.URL.RequestURI(), userID, nil, &tasks); err != nil {
		writeUpstreamError(w, err)
		return
	}
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

	var task Task
	if err := fetchJSON(r.Context(), http.MethodGet, strings.TrimRight(boardQueryServiceURL, "/")+r.URL.Path, userID, nil, &task); err != nil {
		writeUpstreamError(w, err)
		return
	}
	jsonResp(w, http.StatusOK, task)
}

func searchTasksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	if rawProjectID := strings.TrimSpace(r.URL.Query().Get("project_id")); rawProjectID != "" {
		projectID, err := strconv.ParseUint(rawProjectID, 10, 64)
		if err != nil || projectID == 0 {
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
			return
		}
		if err := ensureProjectAccess(r.Context(), userID, uint(projectID)); err != nil {
			writeUpstreamError(w, err)
			return
		}
	}

	var resp searchResponse
	if err := fetchJSON(r.Context(), http.MethodGet, strings.TrimRight(searchServiceURL, "/")+"/tasks?"+r.URL.RawQuery, userID, nil, &resp); err != nil {
		writeUpstreamError(w, err)
		return
	}
	jsonResp(w, http.StatusOK, resp)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func withMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		httpRequestsTotal.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(rec.status)).Inc()
	})
}

func main() {
	membershipServiceURL = getEnv("MEMBERSHIP_SERVICE_URL", "http://membership-service:8080")
	projectServiceURL = getEnv("PROJECT_SERVICE_URL", "http://project-service:8080")
	accessServiceURL = getEnv("ACCESS_SERVICE_URL", "http://access-service:8080")
	boardQueryServiceURL = getEnv("BOARD_QUERY_SERVICE_URL", "http://board-query-service:8080")
	activityServiceURL = getEnv("ACTIVITY_SERVICE_URL", "http://activity-service:8080")
	searchServiceURL = getEnv("SEARCH_SERVICE_URL", "http://search-service.logging.svc.cluster.local:8080")
	workflowServiceURL = getEnv("WORKFLOW_SERVICE_URL", "http://workflow-service:8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/projects", projectsHandler)
	mux.HandleFunc("/dashboard", dashboardHandler)
	mux.HandleFunc("/tasks", tasksHandler)
	mux.HandleFunc("/tasks/", taskByIDHandler)
	mux.HandleFunc("/search/tasks", searchTasksHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, http.StatusOK, map[string]string{"status": "ok", "service": "board-service"})
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("board-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, withMetrics(mux)))
}
