package main

import (
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

type elasticClient struct {
	baseURL  string
	index    string
	username string
	password string
	client   *http.Client
}

var (
	indexer     *elasticClient
	rabbitReady atomic.Bool

	searchIndexOperationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "search_index_operations_total",
		Help: "Total search index operations by type",
	}, []string{"operation"})
)

func init() {
	prometheus.MustRegister(searchIndexOperationsTotal)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(string(body)))
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
			log.Printf("search-indexer consumer stopped: %v", err)
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
	queue, err := ch.QueueDeclare("search-indexer.task-events", true, false, false, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(queue.Name, "task.*", "devboard.events", false, nil); err != nil {
		return err
	}
	deliveries, err := ch.Consume(queue.Name, "search-indexer", false, false, false, false, nil)
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
	go consumeEvents(context.Background(), rabbitURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := indexer.ping(r.Context()); err != nil {
			fmt.Fprintf(w, `{"status":"degraded","service":"search-indexer","rabbit_ready":%t,"elastic_ready":false,"error":%q}`, rabbitReady.Load(), err.Error())
			return
		}
		fmt.Fprintf(w, `{"status":"ok","service":"search-indexer","rabbit_ready":%t,"elastic_ready":true}`, rabbitReady.Load())
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("search-indexer listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
