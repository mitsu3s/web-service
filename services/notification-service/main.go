package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var (
	rdb *redis.Client

	sseConnectionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sse_connections_active",
		Help: "Number of active SSE connections",
	})
	sseEventsDelivered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sse_events_delivered_total",
		Help: "Total SSE events delivered to clients",
	})
)

func init() {
	prometheus.MustRegister(sseConnectionsActive, sseEventsDelivered)
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
		log.Fatalf("Redis connection failed: %v", err)
	}
	log.Println("Redis connected")
}

// GET /events  — Server-Sent Events endpoint
// Clients subscribe here to receive real-time task update notifications.
// The api-gateway injects X-User-ID after JWT validation.
func eventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub := rdb.Subscribe(ctx, "task-events")
	defer sub.Close()

	sseConnectionsActive.Inc()
	defer sseConnectionsActive.Dec()

	// Initial connection acknowledgement
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\"}\n\n")
	flusher.Flush()

	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()

	ch := sub.Channel()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: task-event\ndata: %s\n\n", msg.Payload)
			flusher.Flush()
			sseEventsDelivered.Inc()

		case <-keepAlive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()

		case <-ctx.Done():
			return
		}
	}
}

func main() {
	initRedis()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", eventsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"unhealthy","error":%q}`, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","service":"notification-service"}`)
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("notification-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
