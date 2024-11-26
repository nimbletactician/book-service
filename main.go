package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	_ "github.com/lib/pq"
)

var (
	redisURL string
	isReady  = false // Flag to track readiness
)

// Domain types
type Book struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Rating    float64   `json:"rating"`
	CreatedAt time.Time `json:"created_at"`
}

// Repository interface
type BookRepository interface {
	Create(ctx context.Context, book *Book) error
	GetByID(ctx context.Context, id string) (*Book, error)
	List(ctx context.Context) ([]Book, error)
}

// Repository implementation
type BookRepositoryImpl struct {
	db    *sql.DB
	cache *redis.Client
}

// Add these new types and variables at the top level
type HealthStatus struct {
	Status  string `json:"status"`
	Details string `json:"details,omitempty"`
}

// Add these methods to BookRepositoryImpl
func (r *BookRepositoryImpl) CheckDBHealth(ctx context.Context) error {
	return r.db.PingContext(ctx)
}

func (r *BookRepositoryImpl) CheckRedisHealth(ctx context.Context) error {
	return r.cache.Ping(ctx).Err()
}

func NewBookRepository(db *sql.DB) *BookRepositoryImpl {
	redisURL = os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis-cache:6379"
	}

	cache := redis.NewClient(&redis.Options{
		Addr: redisURL,
	})

	return &BookRepositoryImpl{
		db:    db,
		cache: cache,
	}
}

func (r *BookRepositoryImpl) Create(ctx context.Context, book *Book) error {
	query := `
        INSERT INTO books (id, title, rating, created_at)
        VALUES ($1, $2, $3, $4)
        RETURNING created_at`

	err := r.db.QueryRowContext(
		ctx,
		query,
		book.ID,
		book.Title,
		book.Rating,
		time.Now(),
	).Scan(&book.CreatedAt)

	if err != nil {
		return err
	}

	// Store in cache
	cacheKey := fmt.Sprintf("book:%s", book.ID)
	if bookJSON, err := json.Marshal(book); err == nil {
		r.cache.Set(ctx, cacheKey, bookJSON, time.Hour)
	}

	// Invalidate list cache
	r.cache.Del(ctx, "books:all")

	return nil
}

func (r *BookRepositoryImpl) GetByID(ctx context.Context, id string) (*Book, error) {
	// Try cache first
	cacheKey := fmt.Sprintf("book:%s", id)
	if cached, err := r.cache.Get(ctx, cacheKey).Result(); err == nil {
		var book Book
		if err := json.Unmarshal([]byte(cached), &book); err == nil {
			return &book, nil
		}
	}

	// If not in cache, get from DB
	book := &Book{}
	query := `
        SELECT id, title, rating, created_at
        FROM books
        WHERE id = $1`

	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&book.ID,
		&book.Title,
		&book.Rating,
		&book.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Store in cache
	if bookJSON, err := json.Marshal(book); err == nil {
		r.cache.Set(ctx, cacheKey, bookJSON, time.Hour)
	}

	return book, nil
}

func (r *BookRepositoryImpl) List(ctx context.Context) ([]Book, error) {
	// Try cache first
	cacheKey := "books:all"
	if cached, err := r.cache.Get(ctx, cacheKey).Result(); err == nil {
		var books []Book
		if err := json.Unmarshal([]byte(cached), &books); err == nil {
			return books, nil
		}
	}

	// If not in cache, get from DB
	query := `
        SELECT id, title, rating, created_at
        FROM books
        ORDER BY created_at DESC`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var book Book
		if err := rows.Scan(
			&book.ID,
			&book.Title,
			&book.Rating,
			&book.CreatedAt,
		); err != nil {
			return nil, err
		}
		books = append(books, book)
	}

	// Store in cache
	if booksJSON, err := json.Marshal(books); err == nil {
		r.cache.Set(ctx, cacheKey, booksJSON, time.Hour)
	}

	return books, nil
}

// Service layer
type BookService struct {
	repo BookRepository
}

func NewBookService(repo BookRepository) *BookService {
	return &BookService{repo: repo}
}

func (s *BookService) CreateBook(ctx context.Context, book *Book) error {
	if book.Title == "" {
		return fmt.Errorf("title is required")
	}
	if book.Rating < 0 || book.Rating > 5 {
		return fmt.Errorf("rating must be between 0 and 5")
	}
	return s.repo.Create(ctx, book)
}

func (s *BookService) GetBook(ctx context.Context, id string) (*Book, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *BookService) ListBooks(ctx context.Context) ([]Book, error) {
	return s.repo.List(ctx)
}

// HTTP Handler
type BookHandler struct {
	service *BookService
}

func NewBookHandler(service *BookService) *BookHandler {
	return &BookHandler{service: service}
}

func (h *BookHandler) CreateBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var book Book
	if err := json.NewDecoder(r.Body).Decode(&book); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.service.CreateBook(r.Context(), &book); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(book)
}

func (h *BookHandler) GetBook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		books, err := h.service.ListBooks(r.Context())
		if err != nil {
			http.Error(w, "Failed to fetch books", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(books)
		return
	}

	book, err := h.service.GetBook(r.Context(), id)
	if err != nil {
		http.Error(w, "Failed to fetch book", http.StatusInternalServerError)
		return
	}
	if book == nil {
		http.Error(w, "Book not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(book)
}

// Middleware
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("Started %s %s", r.Method, r.URL.Path)
		next(w, r)
		log.Printf("Completed %s %s in %v", r.Method, r.URL.Path, time.Since(start))
	}
}

// Add new handler methods to BookHandler
func (h *BookHandler) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	repo, ok := h.service.repo.(*BookRepositoryImpl)
	if !ok {
		http.Error(w, "Repository type assertion failed", http.StatusInternalServerError)
		return
	}

	// Check database connectivity
	if err := repo.CheckDBHealth(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(HealthStatus{
			Status:  "not ready",
			Details: fmt.Sprintf("database check failed: %v", err),
		})
		return
	}

	// Check Redis connectivity
	if err := repo.CheckRedisHealth(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(HealthStatus{
			Status:  "not ready",
			Details: fmt.Sprintf("cache check failed: %v", err),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(HealthStatus{Status: "ready"})
}

func (h *BookHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(HealthStatus{Status: "healthy"})
}

func main() {
	// Database setup
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Check if running in init mode
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := initDB(db); err != nil {
			log.Fatalf("Failed to initialize database: %v", err)
		}
		log.Println("DB initialized successfully")
		return
	}

	// Setup dependencies
	repo := NewBookRepository(db)
	service := NewBookService(repo)
	handler := NewBookHandler(service)

	// Setup routes
	http.HandleFunc("/books", loggingMiddleware(handler.CreateBook))
	http.HandleFunc("/books/", loggingMiddleware(handler.GetBook))
	http.HandleFunc("/ready", loggingMiddleware(handler.ReadyHandler))
	http.HandleFunc("/health", loggingMiddleware(handler.HealthHandler))

	// Start server
	server := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown
	go func() {
		log.Printf("Server starting on port 8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
}

func initDB(db *sql.DB) error {
	query := `
        CREATE TABLE IF NOT EXISTS books (
            id TEXT PRIMARY KEY,
            title TEXT NOT NULL,
            rating FLOAT CHECK (rating >= 0 AND rating <= 5),
            created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
        )`
	_, err := db.Exec(query)
	return err
}
