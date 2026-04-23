package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	jwtSecret []byte

	identityServiceURL         string
	boardServiceURL            string
	projectServiceURL          string
	taskOrchestratorServiceURL string
	notificationServiceURL     string
	frontendURL                string

	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "web_bff_http_requests_total",
		Help: "Total number of HTTP requests handled by web-bff",
	}, []string{"method", "path", "status"})
	httpRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "web_bff_http_request_duration_seconds",
		Help:    "HTTP request latency for web-bff",
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newProxy(rawURL string) *httputil.ReverseProxy {
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Fatalf("invalid upstream URL %s: %v", rawURL, err)
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.SetXForwarded()
			if host := pr.In.Host; host != "" {
				pr.Out.Header.Set("X-Forwarded-Host", host)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, proxyErr error) {
			log.Printf("proxy error to %s: %v", rawURL, proxyErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream unavailable"})
		},
	}
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
	if !ok || sub == 0 {
		return 0, false
	}
	return uint(sub), true
}

func withJWT(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		userID, ok := validateJWT(strings.TrimPrefix(auth, "Bearer "))
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		r.Header.Set("X-User-ID", strconv.FormatUint(uint64(userID), 10))
		next.ServeHTTP(w, r)
	})
}

func withJWTOrQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tokenStr = strings.TrimPrefix(auth, "Bearer ")
		} else {
			tokenStr = r.URL.Query().Get("token")
		}
		if tokenStr == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		userID, ok := validateJWT(tokenStr)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}
		r.Header.Set("X-User-ID", strconv.FormatUint(uint64(userID), 10))
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

func methodRouter(routes map[string]http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := routes[r.Method]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		httpRequestsTotal.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(time.Since(start).Seconds())
	})
}

func main() {
	identityServiceURL = getEnv("IDENTITY_SERVICE_URL", "http://identity-service:8080")
	boardServiceURL = getEnv("BOARD_SERVICE_URL", "http://board-service:8080")
	projectServiceURL = getEnv("PROJECT_SERVICE_URL", "http://project-service:8080")
	taskOrchestratorServiceURL = getEnv("TASK_ORCHESTRATOR_SERVICE_URL", "http://task-orchestrator-service:8080")
	notificationServiceURL = getEnv("NOTIFICATION_SERVICE_URL", "http://notification-service:8080")
	frontendURL = getEnv("FRONTEND_URL", "http://frontend:3000")

	identityProxy := newProxy(identityServiceURL)
	boardProxy := newProxy(boardServiceURL)
	projectProxy := newProxy(projectServiceURL)
	taskOrchestratorProxy := newProxy(taskOrchestratorServiceURL)
	notificationProxy := newProxy(notificationServiceURL)
	frontendProxy := newProxy(frontendURL)

	boardHandler := stripPrefix("/api", boardProxy)
	projectHandler := stripPrefix("/api", projectProxy)
	taskHandler := stripPrefix("/api", taskOrchestratorProxy)
	identityHandler := stripPrefix("/api/auth", identityProxy)

	mux := http.NewServeMux()
	mux.Handle("/api/auth/me", withJWT(identityHandler))
	mux.Handle("/api/auth/", identityHandler)
	mux.Handle("/api/dashboard", withJWT(boardHandler))
	mux.Handle("/api/search/", withJWT(boardHandler))
	mux.Handle("/api/projects", withJWT(methodRouter(map[string]http.Handler{
		http.MethodGet:  boardHandler,
		http.MethodPost: projectHandler,
	})))
	mux.Handle("/api/tasks", withJWT(methodRouter(map[string]http.Handler{
		http.MethodGet:  boardHandler,
		http.MethodPost: taskHandler,
	})))
	mux.Handle("/api/tasks/", withJWT(methodRouter(map[string]http.Handler{
		http.MethodGet:    boardHandler,
		http.MethodPut:    taskHandler,
		http.MethodDelete: taskHandler,
	})))
	mux.Handle("/api/notifications/", withJWTOrQuery(stripPrefix("/api/notifications", notificationProxy)))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "web-bff"})
	})
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/", frontendProxy)

	port := getEnv("PORT", "8080")
	log.Printf("web-bff listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, withMetrics(mux)))
}
