package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
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

type subscriberHub struct {
	mu    sync.RWMutex
	users map[uint]map[chan TaskEvent]struct{}
}

var (
	hub         = &subscriberHub{users: make(map[uint]map[chan TaskEvent]struct{})}
	rabbitReady atomic.Bool

	sseConnectionsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sse_connections_active",
		Help: "Number of active SSE connections",
	})
	sseEventsDelivered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sse_events_delivered_total",
		Help: "Total SSE events delivered to clients",
	})
	sseEventsDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sse_events_dropped_total",
		Help: "Total SSE events dropped because a subscriber channel was full",
	})
	notificationEventsReceived = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "notification_events_received_total",
		Help: "Total task events consumed by notification-service from RabbitMQ",
	})
	notificationRabbitConsumerReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "notification_rabbitmq_consumer_ready",
		Help: "Whether notification-service has an active RabbitMQ consumer",
	})
)

func init() {
	prometheus.MustRegister(sseConnectionsActive, sseEventsDelivered, sseEventsDropped, notificationEventsReceived, notificationRabbitConsumerReady)
}

func (h *subscriberHub) subscribe(userID uint) chan TaskEvent {
	ch := make(chan TaskEvent, 16)
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.users[userID] == nil {
		h.users[userID] = make(map[chan TaskEvent]struct{})
	}
	h.users[userID][ch] = struct{}{}
	return ch
}

func (h *subscriberHub) unsubscribe(userID uint, ch chan TaskEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subs := h.users[userID]
	if subs == nil {
		return
	}
	delete(subs, ch)
	if len(subs) == 0 {
		delete(h.users, userID)
	}
	close(ch)
}

func (h *subscriberHub) broadcast(evt TaskEvent) {
	h.mu.RLock()
	subs := h.users[evt.UserID]
	h.mu.RUnlock()

	for ch := range subs {
		select {
		case ch <- evt:
		default:
			sseEventsDropped.Inc()
		}
	}
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

func consumeEvents(ctx context.Context, rabbitURL string) {
	for {
		if err := consumeOnce(ctx, rabbitURL); err != nil {
			rabbitReady.Store(false)
			notificationRabbitConsumerReady.Set(0)
			logger.Error("rabbitmq consumer stopped", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func consumeOnce(ctx context.Context, rabbitURL string) error {
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

	queue, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(queue.Name, "task.*", "devboard.events", false, nil); err != nil {
		return err
	}

	deliveries, err := ch.Consume(queue.Name, "", true, true, false, false, nil)
	if err != nil {
		return err
	}

	rabbitReady.Store(true)
	notificationRabbitConsumerReady.Set(1)
	defer notificationRabbitConsumerReady.Set(0)
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel closed")
			}
			handleDelivery(ctx, msg)
		}
	}
}

func handleDelivery(ctx context.Context, msg amqp.Delivery) {
	var evt TaskEvent
	if err := json.Unmarshal(msg.Body, &evt); err != nil {
		logger.Warn("failed to decode task event", zap.Error(err))
		return
	}
	notificationEventsReceived.Inc()
	_, span := startAMQPConsumerSpan(ctx, msg, evt.Type, evt.EventID, evt.TaskID, evt.ProjectID)
	defer span.End()
	hub.broadcast(evt)
}

func eventsHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := getUserID(r)
	if !ok {
		http.Error(w, `{"error":"missing user context"}`, http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := hub.subscribe(userID)
	defer hub.unsubscribe(userID, ch)

	sseConnectionsActive.Inc()
	defer sseConnectionsActive.Dec()

	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\"}\n\n")
	flusher.Flush()

	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case evt := <-ch:
			body, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: task-event\ndata: %s\n\n", body)
			flusher.Flush()
			sseEventsDelivered.Inc()
		case <-keepAlive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func main() {
	defer initLogging("notification-service")()
	defer initTracing("notification-service")()

	rabbitURL := os.Getenv("RABBITMQ_URL")
	if rabbitURL == "" {
		rabbitURL = "amqp://devboard:devboard123@rabbitmq:5672/"
	}
	go consumeEvents(context.Background(), rabbitURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/events", eventsHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":"notification-service","rabbit_ready":%t}`, rabbitReady.Load())
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	logger.Info("notification-service listening", zap.String("port", port))
	if err := http.ListenAndServe(":"+port, tracedHTTPHandler("notification-service", mux)); err != nil {
		logger.Fatal("server failed", zap.Error(err))
	}
}
