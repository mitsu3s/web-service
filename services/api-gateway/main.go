package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	jwtSecret []byte

	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests",
	}, []string{"method", "path", "status"})

	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration)

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "change-me-in-production"
	}
	jwtSecret = []byte(secret)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newProxy(rawURL string) *httputil.ReverseProxy {
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("invalid upstream URL %s: %v", rawURL, err)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

func validateJWT(tokenStr string) (uint, bool) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return 0, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return 0, false
	}
	sub, ok := claims["sub"].(float64)
	if !ok {
		return 0, false
	}
	return uint(sub), true
}

func withJWT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		userID, ok := validateJWT(strings.TrimPrefix(auth, "Bearer "))
		if !ok {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		// Forward user ID to upstream services
		r.Header.Set("X-User-ID", fmt.Sprintf("%d", userID))
		next.ServeHTTP(w, r)
	})
}

func stripPrefix(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
		h.ServeHTTP(w, r2)
	})
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
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		dur := time.Since(start).Seconds()

		path := r.URL.Path
		httpRequestsTotal.WithLabelValues(r.Method, path, fmt.Sprintf("%d", rec.status)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, path).Observe(dur)
	})
}

func main() {
	authProxy := newProxy(getEnv("AUTH_SERVICE_URL", "http://auth-service:8080"))
	taskProxy := newProxy(getEnv("TASK_SERVICE_URL", "http://task-service:8080"))
	notifProxy := newProxy(getEnv("NOTIFICATION_SERVICE_URL", "http://notification-service:8080"))
	frontendProxy := newProxy(getEnv("FRONTEND_URL", "http://frontend:3000"))

	mux := http.NewServeMux()

	// Auth: no JWT required (login/register)
	mux.Handle("/api/auth/", stripPrefix("/api/auth", authProxy))

	// Tasks: JWT required
	mux.Handle("/api/tasks", withJWT(stripPrefix("/api/tasks", taskProxy)))
	mux.Handle("/api/tasks/", withJWT(stripPrefix("/api/tasks", taskProxy)))

	// Notifications: JWT required (SSE)
	mux.Handle("/api/notifications/", withJWT(stripPrefix("/api/notifications", notifProxy)))

	// Frontend passthrough
	mux.Handle("/", frontendProxy)

	// Health + metrics
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","service":"api-gateway"}`))
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := getEnv("PORT", "8080")
	log.Printf("api-gateway listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, withMetrics(mux)))
}
