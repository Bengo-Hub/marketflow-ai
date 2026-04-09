package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/httpapi/handlers"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// App wires core dependencies and exposes server lifecycle controls.
type App struct {
	cfg        *config.Config
	logger     *zap.Logger
	db         *sql.DB
	redis      *redis.Client
	httpServer *http.Server
}

// New constructs the application.
func New(ctx context.Context, cfg *config.Config, logger *zap.Logger) (*App, error) {
	// ── Database (shared with marketflow-api, for pgvector queries) ────────
	db, err := sql.Open("pgx", cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)
	db.SetConnMaxIdleTime(1 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	// ── Redis ─────────────────────────────────────────────────────────────
	redisClient := redis.NewClient(&redis.Options{
		Addr:            cfg.Redis.Addr,
		Password:        cfg.Redis.Password,
		DB:              cfg.Redis.DB,
		DisableIdentity: true,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	// ── AI Clients ────────────────────────────────────────────────────────
	groqClient := ai.NewClient(cfg.AI.GroqBaseURL, cfg.AI.GroqAPIKey, logger)
	var ollamaClient *ai.Client
	if cfg.AI.OllamaURL != "" {
		ollamaClient = ai.NewClient(cfg.AI.OllamaURL, "", logger) // Ollama has no auth
	}

	// ── Intent Router ─────────────────────────────────────────────────────
	router := ai.NewRouter(groqClient, cfg.AI.GroqRouterModel, logger)

	// ── RAG Pipeline ──────────────────────────────────────────────────────
	pipeline := rag.NewPipeline(rag.Config{
		DB:             db,
		Redis:          redisClient,
		GroqClient:     groqClient,
		OllamaClient:   ollamaClient,
		Router:         router,
		EmbeddingModel: cfg.AI.EmbeddingModel,
		MainModel:      cfg.AI.GroqMainModel,
		FallbackModel:  cfg.AI.GroqFallback,
		EmbeddingDim:   cfg.AI.EmbeddingDim,
		RedisNamespace: cfg.Redis.Namespace,
		Logger:         logger,
	})

	// ── Auth Middleware ────────────────────────────────────────────────────
	var authMiddleware *authclient.AuthMiddleware
	if cfg.Security.RequireJWT {
		authConfig := authclient.DefaultConfig(
			cfg.Security.JWKSUrl,
			cfg.Security.JWTIssuer,
			cfg.Security.JWTAudience,
		)
		authConfig.RedisClient = redisClient
		validator, err := authclient.NewValidator(authConfig)
		if err != nil {
			return nil, fmt.Errorf("create JWT validator: %w", err)
		}
		authMiddleware = authclient.NewAuthMiddleware(validator)
		logger.Info("JWT auth middleware initialized")
	}

	// ── Chat Handler ──────────────────────────────────────────────────────
	chatHandler := handlers.NewChatHandler(pipeline, logger)

	// ── HTTP Router ───────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Timeout(120 * time.Second)) // longer for AI responses
	r.Use(cors.Handler(cors.Options{
		AllowOriginFunc: func(r *http.Request, origin string) bool { return true },
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Tenant-ID"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/healthz", handlers.Health)
	r.Method("GET", "/metrics", promhttp.Handler())

	r.Route("/api/v1", func(r chi.Router) {
		if authMiddleware != nil {
			r.Use(authMiddleware.RequireAuth)
		}
		r.Post("/chat", chatHandler.Chat)
	})

	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           r,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:        cfg,
		logger:     logger,
		db:         db,
		redis:      redisClient,
		httpServer: server,
	}, nil
}

// Run starts the HTTP server.
func (a *App) Run() error {
	a.logger.Info("starting HTTP server", zap.String("addr", a.httpServer.Addr))
	return a.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server and closes resources.
func (a *App) Shutdown(ctx context.Context) error {
	shutdownErr := a.httpServer.Shutdown(ctx)
	if err := a.db.Close(); err != nil && shutdownErr == nil {
		shutdownErr = err
	}
	if err := a.redis.Close(); err != nil && shutdownErr == nil {
		shutdownErr = err
	}
	return shutdownErr
}
