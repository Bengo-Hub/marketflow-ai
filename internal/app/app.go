package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-ai/internal/agent"
	agenttools "github.com/bengobox/marketflow-ai/internal/agent/tools"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/httpapi"
	"github.com/bengobox/marketflow-ai/internal/httpapi/handlers"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/bengobox/marketflow-ai/internal/tasks"
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

	// ── Provider Resolver (platform-level AI config from DB, cached in Redis) ─
	resolver := providers.NewResolver(cfg.AI.MarketFlowAPIURL, cfg.AI.MarketFlowAPIKey, redisClient, &cfg.AI, cfg.Redis.Namespace, logger)

	// ── Chat Session Service ──────────────────────────────────────────────
	sessionService := sessions.NewService(db, logger)

	// ── Task Executor ─────────────────────────────────────────────────────
	taskExecutor := tasks.NewExecutor(logger)

	// ── Agentic RAG Loop (platform-level tools: KB + Tavily web search) ───
	var agentLoop *agent.Loop
	if cfg.AI.AgentEnabled {
		tools := []agent.Tool{
			agenttools.NewKBSearcher(db, ollamaClient, cfg.AI.EmbeddingModel, logger),
			agenttools.NewGetProduct(db, logger),
			agenttools.NewGetFAQ(db, logger),
			agenttools.NewGetComplianceRules(),
		}
		if cfg.AI.TavilyAPIKey != "" {
			tools = append(tools, agenttools.NewTavilySearcher(agenttools.TavilyConfig{
				APIKey:    cfg.AI.TavilyAPIKey,
				BaseURL:   cfg.AI.TavilyBaseURL,
				DailyCap:  cfg.AI.TavilyDailyCap,
				Redis:     redisClient,
				Namespace: cfg.Redis.Namespace,
				Logger:    logger,
			}))
		} else {
			logger.Warn("MF_AI_TAVILY_API_KEY not set; web_search tool disabled")
		}
		// book_meeting: agentic meeting scheduling. Calls marketflow-api with
		// the platform API key; marketflow-api handles the Google Calendar push.
		if cfg.AI.MarketFlowAPIKey != "" {
			tools = append(tools, agenttools.NewBookMeeting(agenttools.BookMeetingConfig{
				APIBaseURL: cfg.AI.MarketFlowAPIURL,
				APIKey:     cfg.AI.MarketFlowAPIKey,
				Logger:     logger,
			}))
		} else {
			logger.Warn("MF_AI_MARKETFLOW_API_KEY not set; book_meeting tool disabled")
		}
		agentLoop = agent.New(agent.Config{
			Client:   groqClient,
			Model:    cfg.AI.GroqMainModel,
			Tools:    tools,
			MaxIters: cfg.AI.AgentMaxIters,
			TokenCap: cfg.AI.AgentTokenCap,
			Logger:   logger,
		})
		logger.Info("agentic RAG enabled", zap.Int("tools", len(tools)))
	}

	// ── RAG Pipeline ──────────────────────────────────────────────────────
	pipeline := rag.NewPipeline(rag.Config{
		DB:             db,
		Redis:          redisClient,
		GroqClient:     groqClient,
		OllamaClient:   ollamaClient,
		Router:         router,
		Resolver:       resolver,
		SessionService: sessionService,
		TaskExecutor:   taskExecutor,
		Agent:          agentLoop,
		AgentEnabled:   cfg.AI.AgentEnabled,
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
	chatHandler := handlers.NewChatHandler(pipeline, sessionService, logger)

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

	// Base URL redirects to Swagger docs
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/docs/", http.StatusMovedPermanently)
	})

	r.Get("/healthz", handlers.Health)
	r.Method("GET", "/metrics", promhttp.Handler())

	// Swagger UI and OpenAPI spec
	r.Get("/v1/docs/*", httpapi.SwaggerUI)
	r.Get("/api/v1/openapi.json", httpapi.OpenAPIJSON)

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
