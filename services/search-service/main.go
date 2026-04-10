package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
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

type elasticClient struct {
	baseURL  string
	index    string
	username string
	password string
	client   *http.Client
}

var (
	indexer          *elasticClient
	rabbitReady      atomic.Bool
	accessServiceURL string
	accessHTTPClient = &http.Client{Timeout: 5 * time.Second}

	searchIndexOperationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "search_service_index_operations_total",
		Help: "Total search index operations by type",
	}, []string{"operation"})
	searchRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "search_service_requests_total",
		Help: "Total search API requests by result",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(searchIndexOperationsTotal, searchRequestsTotal)
}

func validateProjectAccess(ctx context.Context, userID, projectID uint) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%d/access", strings.TrimRight(accessServiceURL, "/"), projectID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-User-ID", strconv.FormatUint(uint64(userID), 10))

	resp, err := accessHTTPClient.Do(req)
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("access-service returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
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

func newElasticClient() (*elasticClient, error) {
	baseURL := strings.TrimRight(os.Getenv("ELASTICSEARCH_URL"), "/")
	if baseURL == "" {
		baseURL = "https://elasticsearch-es-http.logging.svc.cluster.local:9200"
	}
	indexName := os.Getenv("TASKS_INDEX_NAME")
	if indexName == "" {
		indexName = "devboard-tasks"
	}

	tlsConfig := &tls.Config{}
	if strings.EqualFold(os.Getenv("ELASTICSEARCH_SKIP_VERIFY"), "true") {
		tlsConfig.InsecureSkipVerify = true
	} else if certPath := os.Getenv("ELASTICSEARCH_CA_CERT_PATH"); certPath != "" {
		certBytes, err := os.ReadFile(certPath)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certBytes) {
			return nil, fmt.Errorf("failed to parse elasticsearch CA cert")
		}
		tlsConfig.RootCAs = pool
	}

	return &elasticClient{
		baseURL:  baseURL,
		index:    indexName,
		username: os.Getenv("ELASTICSEARCH_USERNAME"),
		password: os.Getenv("ELASTICSEARCH_PASSWORD"),
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		},
	}, nil
}

func (c *elasticClient) do(req *http.Request) (*http.Response, error) {
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	return c.client.Do(req)
}

func (c *elasticClient) ensureIndex(ctx context.Context) error {
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+"/"+c.index, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(headReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("unexpected HEAD %s", resp.Status)
	}

	body := `{
	  "mappings": {
	    "properties": {
	      "task_id": {"type": "long"},
	      "project_id": {"type": "long"},
	      "user_id": {"type": "long"},
	      "title": {"type": "text"},
	      "description": {"type": "text"},
	      "status": {"type": "keyword"},
	      "event_type": {"type": "keyword"},
	      "occurred_at": {"type": "date"}
	    }
	  }
	}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/"+c.index, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("create index failed: %s: %s", resp.Status, string(respBody))
}

func (c *elasticClient) upsertTask(ctx context.Context, evt TaskEvent) error {
	doc := map[string]any{
		"task_id":       evt.TaskID,
		"project_id":    evt.ProjectID,
		"user_id":       evt.UserID,
		"title":         evt.Title,
		"description":   evt.Description,
		"status":        evt.Status,
		"event_type":    evt.Type,
		"occurred_at":   evt.OccurredAt,
		"last_event_id": evt.EventID,
	}
	body, _ := json.Marshal(doc)

	url := fmt.Sprintf("%s/%s/_doc/%s", c.baseURL, c.index, strconv.FormatUint(uint64(evt.TaskID), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		searchIndexOperationsTotal.WithLabelValues("upsert").Inc()
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("index upsert failed: %s: %s", resp.Status, string(respBody))
}

func (c *elasticClient) deleteTask(ctx context.Context, evt TaskEvent) error {
	url := fmt.Sprintf("%s/%s/_doc/%s", c.baseURL, c.index, strconv.FormatUint(uint64(evt.TaskID), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		searchIndexOperationsTotal.WithLabelValues("delete").Inc()
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("index delete failed: %s: %s", resp.Status, string(respBody))
}

func (c *elasticClient) searchTasks(ctx context.Context, userID uint, query string, projectID *uint, limit int) ([]searchTask, int, error) {
	if err := c.ensureIndex(ctx); err != nil {
		return nil, 0, err
	}

	filters := []map[string]any{
		{"term": map[string]any{"user_id": userID}},
	}
	if projectID != nil {
		filters = append(filters, map[string]any{"term": map[string]any{"project_id": *projectID}})
	}

	queryClause := map[string]any{"match_all": map[string]any{}}
	if strings.TrimSpace(query) != "" {
		queryClause = map[string]any{
			"multi_match": map[string]any{
				"query":  query,
				"fields": []string{"title^3", "description"},
			},
		}
	}

	payload := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": filters,
				"must":   []map[string]any{queryClause},
			},
		},
		"sort": []map[string]any{
			{"_score": map[string]string{"order": "desc"}},
			{"occurred_at": map[string]string{"order": "desc"}},
		},
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/%s/_search", c.baseURL, c.index), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("search failed: %s: %s", resp.Status, string(respBody))
	}

	var esResp struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				Score  float64 `json:"_score"`
				Source struct {
					TaskID      uint      `json:"task_id"`
					ProjectID   uint      `json:"project_id"`
					UserID      uint      `json:"user_id"`
					Title       string    `json:"title"`
					Description string    `json:"description"`
					Status      string    `json:"status"`
					EventType   string    `json:"event_type"`
					OccurredAt  time.Time `json:"occurred_at"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&esResp); err != nil {
		return nil, 0, err
	}

	results := make([]searchTask, 0, len(esResp.Hits.Hits))
	for _, hit := range esResp.Hits.Hits {
		results = append(results, searchTask{
			TaskID:      hit.Source.TaskID,
			ProjectID:   hit.Source.ProjectID,
			UserID:      hit.Source.UserID,
			Title:       hit.Source.Title,
			Description: hit.Source.Description,
			Status:      hit.Source.Status,
			EventType:   hit.Source.EventType,
			OccurredAt:  hit.Source.OccurredAt,
			Score:       hit.Score,
		})
	}

	return results, esResp.Hits.Total.Value, nil
}

func (c *elasticClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return nil
	}
	return fmt.Errorf("elasticsearch returned %s", resp.Status)
}

func consumeEvents(ctx context.Context, rabbitURL string) {
	for {
		if err := consumeOnce(ctx, rabbitURL); err != nil {
			rabbitReady.Store(false)
			log.Printf("search-service consumer stopped: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func consumeOnce(ctx context.Context, rabbitURL string) error {
	if err := indexer.ensureIndex(ctx); err != nil {
		return err
	}

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
	queue, err := ch.QueueDeclare("search-service.task-events", true, false, false, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(queue.Name, "task.*", "devboard.events", false, nil); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queue.Name, "search-service", false, false, false, false, nil)
	if err != nil {
		return err
	}

	rabbitReady.Store(true)
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}

			var evt TaskEvent
			if err := json.Unmarshal(msg.Body, &evt); err != nil {
				log.Printf("invalid task event payload: %v", err)
				_ = msg.Ack(false)
				continue
			}

			var handleErr error
			switch evt.Type {
			case "task.deleted":
				handleErr = indexer.deleteTask(ctx, evt)
			default:
				handleErr = indexer.upsertTask(ctx, evt)
			}
			if handleErr != nil {
				log.Printf("failed to index task event: %v", handleErr)
				_ = msg.Nack(false, true)
				continue
			}
			_ = msg.Ack(false)
		}
	}
}

func searchTasksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID, ok := getUserID(r)
	if !ok {
		searchRequestsTotal.WithLabelValues("unauthorized").Inc()
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "missing user context"})
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 8
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 50 {
			searchRequestsTotal.WithLabelValues("bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
			return
		}
		limit = parsed
	}

	var projectID *uint
	if raw := strings.TrimSpace(r.URL.Query().Get("project_id")); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 {
			searchRequestsTotal.WithLabelValues("bad_request").Inc()
			jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid project_id"})
			return
		}
		project := uint(parsed)
		if err := validateProjectAccess(r.Context(), userID, project); err != nil {
			searchRequestsTotal.WithLabelValues("forbidden").Inc()
			jsonResp(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		projectID = &project
	}

	results, total, err := indexer.searchTasks(r.Context(), userID, query, projectID, limit)
	if err != nil {
		searchRequestsTotal.WithLabelValues("error").Inc()
		log.Printf("search request failed: %v", err)
		jsonResp(w, http.StatusBadGateway, map[string]string{"error": "search backend unavailable"})
		return
	}

	searchRequestsTotal.WithLabelValues("ok").Inc()
	jsonResp(w, http.StatusOK, searchResponse{
		Query:   query,
		Total:   total,
		Results: results,
	})
}

func main() {
	var err error
	indexer, err = newElasticClient()
	if err != nil {
		log.Fatalf("failed to configure elasticsearch client: %v", err)
	}

	rabbitURL := os.Getenv("RABBITMQ_URL")
	if rabbitURL == "" {
		rabbitURL = "amqp://devboard:devboard123@rabbitmq.devboard.svc.cluster.local:5672/"
	}
	accessServiceURL = os.Getenv("ACCESS_SERVICE_URL")
	if accessServiceURL == "" {
		accessServiceURL = "http://access-service.devboard.svc.cluster.local:8080"
	}
	go consumeEvents(context.Background(), rabbitURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", searchTasksHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := indexer.ping(r.Context()); err != nil {
			fmt.Fprintf(w, `{"status":"degraded","service":"search-service","rabbit_ready":%t,"elastic_ready":false,"error":%q}`, rabbitReady.Load(), err.Error())
			return
		}
		fmt.Fprintf(w, `{"status":"ok","service":"search-service","rabbit_ready":%t,"elastic_ready":true}`, rabbitReady.Load())
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("search-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
