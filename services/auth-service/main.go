package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---- Models ----------------------------------------------------------------

type User struct {
	ID        uint      `json:"id"         gorm:"primaryKey"`
	Email     string    `json:"email"      gorm:"uniqueIndex;not null"`
	Password  string    `json:"-"          gorm:"not null"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- Globals ---------------------------------------------------------------

var (
	db        *gorm.DB
	jwtSecret []byte

	registrationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "auth_registrations_total",
		Help: "Total user registrations",
	})
	loginsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "auth_logins_total",
		Help: "Total login attempts",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(registrationsTotal, loginsTotal)

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "change-me-in-production"
	}
	jwtSecret = []byte(secret)
}

// ---- DB --------------------------------------------------------------------

func initDB() {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		dsn = "devboard:devboard123@tcp(mysql:3306)/devboard?charset=utf8mb4&parseTime=True&loc=Local"
	}

	var err error
	for i := 1; i <= 15; i++ {
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Info),
		})
		if err == nil {
			break
		}
		log.Printf("MySQL not ready (attempt %d/15): %v", i, err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		log.Fatalf("failed to connect to MySQL: %v", err)
	}

	if err := db.AutoMigrate(&User{}); err != nil {
		log.Fatalf("AutoMigrate failed: %v", err)
	}
	log.Println("MySQL connected and migrated")
}

// ---- Helpers ---------------------------------------------------------------

func jsonResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func generateJWT(userID uint) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(24 * time.Hour).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

// ---- Handlers --------------------------------------------------------------

// POST /register
func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" || req.Password == "" {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "email and password are required"})
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	user := User{Email: req.Email, Password: string(hashed)}
	if result := db.Create(&user); result.Error != nil {
		jsonResp(w, http.StatusConflict, map[string]string{"error": "email already exists"})
		return
	}

	registrationsTotal.Inc()
	jsonResp(w, http.StatusCreated, map[string]any{"id": user.ID, "email": user.Email})
}

// POST /login
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResp(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	var user User
	if result := db.Where("email = ?", req.Email).First(&user); result.Error != nil {
		loginsTotal.WithLabelValues("failure").Inc()
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		loginsTotal.WithLabelValues("failure").Inc()
		jsonResp(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	token, err := generateJWT(user.ID)
	if err != nil {
		jsonResp(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	loginsTotal.WithLabelValues("success").Inc()
	jsonResp(w, http.StatusOK, map[string]string{"token": token})
}

// GET /me  (X-User-ID is injected by api-gateway)
func meHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userIDStr := r.Header.Get("X-User-ID")
	var user User
	if result := db.First(&user, userIDStr); result.Error != nil {
		jsonResp(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	jsonResp(w, http.StatusOK, user)
}

// ---- Main ------------------------------------------------------------------

func main() {
	initDB()

	mux := http.NewServeMux()
	mux.HandleFunc("/register", registerHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/me", meHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sqlDB, _ := db.DB()
		if err := sqlDB.Ping(); err != nil {
			jsonResp(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","service":"auth-service"}`)
	})
	mux.Handle("/metrics", promhttp.Handler())

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("auth-service listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
