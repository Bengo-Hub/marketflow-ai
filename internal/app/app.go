package app

import (
	"context"
	"database/sql"
	"fmt"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/httpware/objectstore"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"net/http"
	"os"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-ai/internal/agent"
	agenttools "github.com/bengobox/marketflow-ai/internal/agent/tools"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/decide"
	"github.com/bengobox/marketflow-ai/internal/httpapi"
	"github.com/bengobox/marketflow-ai/internal/httpapi/handlers"
	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/media"
	mw "github.com/bengobox/marketflow-ai/internal/middleware"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/bengobox/marketflow-ai/internal/tasks"
	"github.com/bengobox/marketflow-ai/internal/team"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
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
	redisClient, err := sharedcache.NewRedis(ctx, sharedcache.RedisConfig{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB,
	})
	if err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	// ── Provider Resolver (platform-level AI config from DB, cached in Redis) ─
	resolver := providers.NewResolver(cfg.AI.MarketFlowAPIURL, cfg.AI.MarketFlowAPIKey, redisClient, &cfg.AI, cfg.Redis.Namespace, logger)
	// Output registry (credit prices, FX): the OS, the creative endpoints and the
	// agent's ad-cost tool all price from it.
	registrySource := newRegistrySource(resolver, logger)

	// ── Model router ──────────────────────────────────────────────────────
	// Every generative call names a class (nano, standard, premium, premium_plus);
	// providers are resolved DB-first with env fallback and re-read every five
	// minutes, and the routing policy is the platform ProviderSetting
	// ai_routing/policy merged over the code defaults.
	modelRouter := llm.NewRouter(llm.Config{
		Providers: newLLMProviders(resolver, &cfg.AI, logger),
		Policy:    routingPolicySource(resolver),
		Defaults:  llm.DefaultPolicy(llm.Defaults{OllamaModel: cfg.AI.OllamaFallbackModel}),
		Logger:    logger,
	})
	nanoLLM := modelRouter.Client(llm.Nano)
	standardLLM := modelRouter.Client(llm.Standard)

	var ollamaClient *ai.Client
	if cfg.AI.OllamaURL != "" {
		ollamaClient = ai.NewClient(cfg.AI.OllamaURL, "", logger) // embeddings: /api/embeddings
	}

	// ── Intent Router ─────────────────────────────────────────────────────
	router := ai.NewRouter(nanoLLM, cfg.AI.GroqRouterModel, logger)

	// ── Chat Session Service ──────────────────────────────────────────────
	sessionService := sessions.NewService(db, logger)

	// ── Task Executor ─────────────────────────────────────────────────────
	taskExecutor := tasks.NewExecutor(logger)

	// ── Agentic RAG Loop (platform-level tools: KB + Tavily web search) ───
	var agentLoop *agent.Loop
	var meetingPrep *agenttools.MeetingPrep
	var agentTools []agent.Tool // shared with the Marketing OS team (read-only subset)
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
		// query_service_data: queries live data from any registered Codevertex
		// microservice (ordering, inventory, treasury, codevertex-website, etc.)
		// using the shared INTERNAL_SERVICE_KEY.
		if cfg.App.InternalServiceKey != "" {
			tools = append(tools, agenttools.NewQueryServiceData(agenttools.QueryServiceDataConfig{
				ServiceKey: cfg.App.InternalServiceKey,
				Logger:     logger,
			}))
			logger.Info("query_service_data tool enabled (codevertex, ordering, treasury, ...)")

			// tax_advisor: Kenya tax/statutory rates + compliance calendar from the
			// treasury Smart Tax & Compliance module (read-only S2S).
			tools = append(tools, agenttools.NewTaxAdvisor(agenttools.TaxAdvisorConfig{
				ServiceKey: cfg.App.InternalServiceKey,
				Logger:     logger,
			}))
			logger.Info("tax_advisor tool enabled (Kenya tax rates + compliance calendar)")

			// create_ticket: opens a support ticket in ticketing-service. (Was defined but
			// never actually registered here -- fixed alongside adding request_integration below.)
			if cfg.AI.TicketingURL != "" {
				tools = append(tools, agenttools.NewCreateTicket(agenttools.CreateTicketConfig{
					TicketingURL: cfg.AI.TicketingURL,
					ServiceKey:   cfg.App.InternalServiceKey,
					Logger:       logger,
				}))
				logger.Info("create_ticket tool enabled")
			}

			// request_integration: submits a platform-integration request (eTIMS, TruLoad
			// weighing, cameras, PLCs, Arduino/IoT) and notifies support via auth-api's
			// existing Slack/email notifier.
			if cfg.AI.AuthAPIURL != "" {
				tools = append(tools, agenttools.NewRequestIntegration(agenttools.RequestIntegrationConfig{
					AuthAPIURL: cfg.AI.AuthAPIURL,
					ServiceKey: cfg.App.InternalServiceKey,
					Logger:     logger,
				}))
				logger.Info("request_integration tool enabled")
			}
		} else {
			logger.Warn("MF_AI_INTERNAL_SERVICE_KEY not set; query_service_data tool disabled — set it to enable live course/enrollment/order queries")
		}

		// Tools that require marketflow-api service-to-service calls.
		if cfg.AI.MarketFlowAPIKey != "" {
			// Also served to the worker's meeting reminders (/internal/os/meeting-prep).
			meetingPrep = agenttools.NewMeetingPrep(agenttools.MeetingPrepConfig{
				APIBaseURL: cfg.AI.MarketFlowAPIURL,
				ServiceKey: cfg.AI.MarketFlowAPIKey,
				LLMClient:  standardLLM,
				LLMModel:   string(llm.Standard),
				Logger:     logger,
			})
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
				agenttools.NewCalculateCost(registrySource.Get, logger),
				// Sprint 10: CRM agent tools
				agenttools.NewCreateDeal(agenttools.CreateDealConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewMovePipelineStage(agenttools.MovePipelineStageConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewCreateActivity(agenttools.CreateActivityConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewCreateTask(agenttools.CreateTaskConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewDraftEmail(agenttools.DraftEmailConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					LLMClient:  standardLLM,
					LLMModel:   string(llm.Standard),
					Logger:     logger,
				}),
				agenttools.NewAnalyzeDeal(agenttools.AnalyzeDealConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					LLMClient:  standardLLM,
					LLMModel:   string(llm.Standard),
					Logger:     logger,
				}),
				agenttools.NewScoreLead(agenttools.ScoreLeadConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewGetContact360(agenttools.GetContact360Config{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				agenttools.NewEnrichContact(agenttools.EnrichContactConfig{
					APIBaseURL: cfg.AI.MarketFlowAPIURL,
					ServiceKey: cfg.AI.MarketFlowAPIKey,
					Logger:     logger,
				}),
				meetingPrep,
			)
			// Image search uses the same Google CSE key as web_search.
			if googleCfg.APIKey != "" && googleCfg.CSEID != "" {
				tools = append(tools, agenttools.NewImageSearch(googleCfg.APIKey, googleCfg.CSEID, logger))
			}
		} else {
			logger.Warn("MF_AI_MARKETFLOW_API_KEY not set; service tools disabled (book_meeting, get_fbo_profile, check_integrations, generate_links)")
		}
		agentTools = tools
		agentLoop = agent.New(agent.Config{
			Client:   standardLLM,
			Model:    string(llm.Standard),
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
		LLM:            modelRouter,
		OllamaClient:   ollamaClient,
		Router:         router,
		SessionService: sessionService,
		TaskExecutor:   taskExecutor,
		Agent:          agentLoop,
		AgentEnabled:   cfg.AI.AgentEnabled,
		EmbeddingModel: cfg.AI.EmbeddingModel,
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
	// AI credits are owned by subscriptions-api: each chat message spends one there.
	var creditSpender handlers.CreditSpender
	if cfg.Security.SubscriptionsURL != "" && cfg.Security.SubscriptionsKey != "" {
		creditSpender = handlers.NewHTTPCreditSpender(cfg.Security.SubscriptionsURL, cfg.Security.SubscriptionsKey, logger)
	}
	chatHandler := handlers.NewChatHandler(pipeline, sessionService, creditSpender, logger)
	// ── Marketing OS (Sprint 21): decide layer, library, optimiser, team ──
	lib := library.MustLoad()
	decider := decide.New(decide.Config{
		Primary:  typeSafeSource(resolver),
		Fallback: decide.NewLLMFallback(modelRouter),
		Ceiling:  func(ctx context.Context) float64 { return modelRouter.Policy(ctx).Decide.MonthlyUSDCeiling },
		Spend:    decide.RedisSpend{Client: redisClient, Namespace: cfg.Redis.Namespace},
		Logger:   logger,
	})
	optimizer := promptopt.New(promptopt.Config{
		Library: lib, Decider: decider, Router: modelRouter,
		Redis: redisClient, Namespace: cfg.Redis.Namespace, Logger: logger,
	})
	osContext := &team.APISource{
		BaseURL: cfg.AI.MarketFlowAPIURL, Key: cfg.App.InternalServiceKey,
		Redis: redisClient, Namespace: cfg.Redis.Namespace,
	}
	teamRuntime := team.New(team.Config{
		Library: lib, Opt: optimizer, Router: modelRouter, Decider: decider,
		Registry: registrySource.Get, Tools: agentTools, Source: osContext, Logger: logger,
	})
	osHandler := handlers.NewOSHandler(handlers.OSConfig{
		Runtime: teamRuntime, Opt: optimizer, Library: lib, Source: osContext, Pipeline: pipeline,
		Agent: agentLoop, Credits: creditSpender, Registry: registrySource.Get, Logger: logger,
	})
	if meetingPrep != nil {
		osHandler.SetMeetingPrep(meetingPrep)
	}
	// Creative studio posters (Sprint 15). Finished posters go to the shared
	// object storage (OBJECT_STORAGE_*, or MEDIA_ROOT locally); without it the
	// endpoint answers 503 rather than paying for an image it cannot keep.
	mediaStore, err := objectstore.New("marketflow", objectstore.FromEnv())
	switch {
	case err != nil:
		logger.Warn("object storage unavailable; posters disabled", zap.Error(err))
		mediaStore = nil
	case !mediaStore.Enabled():
		logger.Warn("object storage not configured (OBJECT_STORAGE_* or MEDIA_ROOT); posters disabled")
	default:
		media.SetBases(os.Getenv("MF_MEDIA_FAL_QUEUE_URL"), os.Getenv("MF_MEDIA_HIGGSFIELD_API_URL"))
		posters := media.NewPosters(mediaKeySource(resolver), media.ObjectStore{S: mediaStore}, logger)
		osHandler.SetPosters(posters)
		// Motion posters: background jobs in Redis; ffmpeg draws the exact words.
		clips := media.NewClips(mediaKeySource(resolver), media.ObjectStore{S: mediaStore}, posters, logger)
		if !clips.Ready() {
			logger.Warn("ffmpeg not found; motion posters disabled")
		}
		osHandler.SetClips(media.NewClipJobs(clips, redisClient, cfg.Redis.Namespace, logger))
	}

	triageHandler := handlers.NewTriageHandler(nanoLLM, string(llm.Nano), logger)
	crmAIHandler := handlers.NewCRMAIHandler(standardLLM, string(llm.Standard), cfg.AI.MarketFlowAPIURL, cfg.App.InternalServiceKey, logger)

	// ── HTTP Router ───────────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	// Never chi RealIP: it trusts client-sent True-Client-IP/X-Forwarded-For.
	r.Use(ratelimit.TrustedRealIP)
	r.Use(chimiddleware.Recoverer)
	// Longer for AI responses; streamed (SSE) answers end on their own or on disconnect.
	r.Use(httpware.BypassForStreaming(chimiddleware.Timeout(120 * time.Second)))
	// Platform apps and ALLOWED_ORIGINS get credentialed CORS. The embeddable chat widget runs on
	// customer sites, so any other origin is answered with "*" (no credentials); the pattern order
	// matters because the first match wins.
	corsOrigins := []string{"http://localhost:*", "https://*.codevertexafrica.com"}
	for _, o := range strings.Split(cfg.HTTP.AllowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			corsOrigins = append(corsOrigins, o)
		}
	}
	corsOrigins = append(corsOrigins, "*")
	r.Use(httpware.CORS(httpware.StandardCORS(corsOrigins, "X-Widget-Mode", "X-Widget-Persona")))

	// Base URL redirects to Swagger docs
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/docs/", http.StatusMovedPermanently)
	})

	r.Get("/healthz", handlers.Health)
	r.Method("GET", "/metrics", promhttp.Handler())
	// Local media (MEDIA_ROOT, MEDIA_URL_BASE=<this service>/media) in development;
	// with object storage it redirects public keys to the bucket's public URL.
	if h := mediaStore.MediaHandler(); h != nil {
		r.Handle("/media/*", http.StripPrefix("/media", h))
	}

	// Swagger UI and OpenAPI spec
	r.Get("/v1/docs/*", httpapi.SwaggerUI)
	r.Get("/api/v1/openapi.json", httpapi.OpenAPIJSON)

	// Public chat endpoint — no auth required. The embeddable widget on
	// /p/{slug} and external sites uses this. Rate-limited per-IP (15/min)
	// and per-tenant (100/min) to prevent flooding.
	r.Route("/api/v1/public", func(r chi.Router) {
		r.Use(mw.PublicChatLimits(ratelimit.NewLimiter(redisClient, logger, "marketflow-ai"), 15, 100))
		r.Post("/chat", chatHandler.Chat)
		// Clear chat and delete a message; the session id is the credential, as for history.
		r.Delete("/chat/sessions/{id}", chatHandler.ClearSession)
		r.Delete("/chat/sessions/{id}/turns/{n}", chatHandler.DeleteTurn)
	})

	// Authenticated chat + CRM AI — JWT or platform X-API-Key.
	r.Route("/api/v1", func(r chi.Router) {
		if authMiddleware != nil {
			r.Use(authMiddleware.RequireAuth)
		}
		r.Post("/chat", chatHandler.Chat)
		r.Delete("/chat/sessions/{id}", chatHandler.ClearSession)
		r.Delete("/chat/sessions/{id}/turns/{n}", chatHandler.DeleteTurn)
		r.Post("/ai/pipeline-summary", crmAIHandler.PipelineSummary)
		r.Post("/ai/score-lead", crmAIHandler.ScoreLead)

		// Marketing OS. Tenant comes from the token; a mismatched X-Tenant-ID is refused.
		r.Route("/os", func(r chi.Router) {
			r.Use(mw.PublicChatLimits(ratelimit.NewLimiter(redisClient, logger, "marketflow-ai-os"), 30, 120))
			r.Post("/ask", osHandler.Ask)
			r.Post("/optimize", osHandler.Optimize)
			r.Post("/plan/quote", osHandler.Quote)
			r.Get("/creative/quote", osHandler.CreativeQuote)
			r.Get("/library/templates", osHandler.Templates)
			r.Get("/team", osHandler.Team)
		})
	})

	// Custom-agent runs from the worker (the route it always called), S2S only.
	r.With(httpware.RequireServiceKey(cfg.App.InternalServiceKey)).Post("/api/v1/agent/run", osHandler.AgentRun)

	// Internal S2S routes — X-API-Key only (ticketing-api, marketflow-worker).
	internalKey := cfg.App.InternalServiceKey
	r.Route("/api/v1/internal", func(r chi.Router) {
		r.Use(httpware.RequireServiceKey(internalKey))
		r.Post("/triage", triageHandler.Triage)
		r.Post("/os/daily-brief", osHandler.DailyBrief)
		r.Post("/os/meeting-prep", osHandler.MeetingPrep)
		r.Post("/creative/poster", osHandler.Poster)
		r.Post("/creative/clip", osHandler.Clip)
		r.Get("/creative/clip/{version_id}", osHandler.ClipStatus)
	})

	// classify — used by ticketing-api (same X-API-Key auth).
	r.Route("/api/v1/classify", func(r chi.Router) {
		r.Use(httpware.RequireServiceKey(internalKey))
		r.Post("/", triageHandler.Classify)
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
