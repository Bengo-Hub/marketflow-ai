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

	// ── Provider Resolver (platform-level AI config from DB, cached in Redis) ─
	resolver := providers.NewResolver(cfg.AI.MarketFlowAPIURL, cfg.AI.MarketFlowAPIKey, redisClient, &cfg.AI, cfg.Redis.Namespace, logger)

	// ── AI Clients (DB-resolved, env fallback) ───────────────────────────
	// Resolve Groq settings from the ProviderSetting table so the platform
	// owner manages the key via /platform/providers without touching env vars.
	resolveStartup, resolveCancel := context.WithTimeout(ctx, 5*time.Second)
	groqClient, groqMainModel, _, err := resolver.ResolveGroqClient(resolveStartup)
	resolveCancel()
	if err != nil {
		logger.Warn("Groq resolver failed, using env fallback", zap.Error(err))
		groqClient = ai.NewClient(cfg.AI.GroqBaseURL, cfg.AI.GroqAPIKey, logger)
		groqMainModel = cfg.AI.GroqMainModel
	} else {
		logger.Info("Groq resolved from DB", zap.String("model", groqMainModel))
	}
	_ = groqMainModel // used by router + agent below
	var ollamaClient *ai.Client
	if cfg.AI.OllamaURL != "" {
		ollamaClient = ai.NewClient(cfg.AI.OllamaURL, "", logger) // Ollama has no auth
	}

	// ── Intent Router ─────────────────────────────────────────────────────
	router := ai.NewRouter(groqClient, cfg.AI.GroqRouterModel, logger)

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
		// Build web_search: resolve credentials DB-first (ProviderSetting
		// rows written from /platform/providers), env-var fallback. Prefer
		// Google Custom Search when both are configured; Tavily is used as
		// automatic fallback on 429/quota from Google.
		resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		tavilyCfg := resolver.ResolveTavilySettings(resolveCtx)
		googleCfg := resolver.ResolveGoogleCSESettings(resolveCtx)
		resolveCancel()

		var tavily *agenttools.TavilySearcher
		if tavilyCfg.APIKey != "" {
			tavily = agenttools.NewTavilySearcher(agenttools.TavilyConfig{
				APIKey:    tavilyCfg.APIKey,
				BaseURL:   tavilyCfg.BaseURL,
				DailyCap:  tavilyCfg.DailyCap,
				Redis:     redisClient,
				Namespace: cfg.Redis.Namespace,
				Logger:    logger,
			})
		}
		switch {
		case cfg.AI.WebSearchProvider == "google" && googleCfg.APIKey != "" && googleCfg.CSEID != "":
			var fallback agenttools.Searcher
			if tavily != nil {
				fallback = tavily
			}
			tools = append(tools, agenttools.NewGoogleSearcher(agenttools.GoogleSearchConfig{
				APIKey:    googleCfg.APIKey,
				CSEID:     googleCfg.CSEID,
				DailyCap:  googleCfg.DailyCap,
				Redis:     redisClient,
				Namespace: cfg.Redis.Namespace,
				Logger:    logger,
				Fallback:  fallback,
			}))
			logger.Info("web_search: Google Custom Search (DB-resolved)",
				zap.Bool("has_tavily_fallback", tavily != nil),
				zap.Int("daily_cap", googleCfg.DailyCap))
		case tavily != nil:
			tools = append(tools, tavily)
			logger.Info("web_search: Tavily", zap.Int("daily_cap", tavilyCfg.DailyCap))
		default:
			logger.Warn("neither Google Custom Search nor Tavily configured; web_search tool disabled — configure at /platform/providers")
		}
		// Tools that require marketflow-api service-to-service calls.
		if cfg.AI.MarketFlowAPIKey != "" {
			tools = append(tools,
				agenttools.NewBookMeeting(agenttools.BookMeetingConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					APIKey:     cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewGetFBOProfile(agenttools.FBOProfileConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					APIKey:     cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewCheckIntegrations(cfg.AI.MarketFlowAPIURL, cfg.AI.MarketFlowAPIKey, logger),
				agenttools.NewGenerateLinks(logger),
				agenttools.NewCreateCampaign(cfg.AI.MarketFlowAPIURL, cfg.AI.MarketFlowAPIKey, logger),
				agenttools.NewCreateFunnel(cfg.AI.MarketFlowAPIURL, cfg.AI.MarketFlowAPIKey, logger),
				agenttools.NewCalculateCost(logger),
			)
			// Image search uses the same Google CSE key as web_search.
			if googleCfg.APIKey != "" && googleCfg.CSEID != "" {
				tools = append(tools, agenttools.NewImageSearch(googleCfg.APIKey, googleCfg.CSEID, logger))
			}
		} else {
			logger.Warn("MF_AI_MARKETFLOW_API_KEY not set; service tools disabled (book_meeting, get_fbo_profile, check_integrations, generate_links)")
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

	// ── Anthropic Claude (third-tier fallback when all Groq models fail) ──
	var claudeClient *ai.AnthropicClient
	if cfg.AI.ClaudeAPIKey != "" {
		claudeClient = ai.NewAnthropicClient(cfg.AI.ClaudeBaseURL, cfg.AI.ClaudeAPIKey, cfg.AI.ClaudeVersion, logger)
		logger.Info("Anthropic Claude fallback enabled", zap.String("model", cfg.AI.ClaudeModel))
	} else {
		logger.Warn("MF_AI_CLAUDE_API_KEY not set — Claude fallback disabled; set it for resilience when Groq is down")
	}

	// ── RAG Pipeline ──────────────────────────────────────────────────────
	pipeline := rag.NewPipeline(rag.Config{
		DB:             db,
		Redis:          redisClient,
		GroqClient:     groqClient,
		ClaudeClient:   claudeClient,
		ClaudeModel:    cfg.AI.ClaudeModel,
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
	// Accepts BOTH JWT (from human callers via the UI) AND the platform
	// X-API-Key (from service-to-service callers, internal smoke tests,
	// and the embeddable widget's SSE stream). Mirrors the marketflow-api
	// setup so the chat endpoint is reachable with the same auth options.
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
		apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Security.AuthServiceURL, nil)
		authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
		logger.Info("auth middleware initialized (JWT + platform X-API-Key)",
			zap.String("jwks_url", cfg.Security.JWKSUrl),
			zap.String("auth_service", cfg.Security.AuthServiceURL),
		)
	}

	// ── Chat Handler ──────────────────────────────────────────────────────
	var usageReporter handlers.UsageReporter
	if cfg.Security.SubscriptionsURL != "" {
		usageReporter = handlers.NewHTTPUsageReporter(cfg.Security.SubscriptionsURL, cfg.Security.SubscriptionsKey, logger)
	}
	chatHandler := handlers.NewChatHandler(pipeline, sessionService, usageReporter, logger)

	// ── HTTP Router ───────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
	r.Use(chimiddleware.Timeout(120 * time.Second)) // longer for AI responses
	r.Use(cors.Handler(cors.Options{
		AllowOriginFunc: func(r *http.Request, origin string) bool { return true },
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Tenant-ID", "X-API-Key", "X-Widget-Mode"},
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

	// Public chat endpoint — no auth required. The embeddable widget on
	// /p/{slug} and external sites uses this. Rate-limited by tenant slug
	// (via the existing per-tenant daily caps on web_search + AI credits).
	r.Route("/api/v1/public", func(r chi.Router) {
		r.Post("/chat", chatHandler.Chat)
	})

	// Authenticated chat — for the dashboard AI chat page where the user
	// is logged in with a JWT or using the platform X-API-Key.
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
