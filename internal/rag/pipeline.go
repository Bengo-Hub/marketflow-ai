package rag

import (
	"context"
	"database/sql"

	"github.com/bengobox/marketflow-ai/internal/agent"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/bengobox/marketflow-ai/internal/tasks"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// SearchResult represents a vector similarity search result from pgvector.
type SearchResult struct {
	EntryID    uuid.UUID
	ChunkText  string
	Similarity float64
	Category   string
	Title      string
}

// Pipeline orchestrates the full RAG flow: embed → search → prompt → LLM → filter.
//
// LLM fallback ladder (fastest/most-available first):
//
//	Tier 1 — Groq main (DB-resolved, llama-3.3-70b-versatile, ~82ms)
//	Tier 2 — Groq fallback model (gemma2-9b-it, same API, ~70ms)
//	Tier 3 — Ollama local LLM (llama3.1:8b, cluster-internal, no quota)
//	Tier 4 — OpenAI (gpt-4o-mini, DB-resolved; graceful skip on 429/quota)
//	Tier 5 — Anthropic Claude (DB-resolved; graceful skip on 400/credits)
type Pipeline struct {
	db              *sql.DB
	redis           *redis.Client
	groqClient      *ai.Client
	claudeClient    *ai.AnthropicClient // tier-5 fallback
	claudeModel     string
	ollamaClient    *ai.Client // embeddings only (native Ollama API)
	ollamaLLMClient *ai.Client // tier-3 fallback LLM (OpenAI-compat /v1 path)
	ollamaLLMModel  string
	openaiClient    *ai.Client // tier-4 fallback
	openaiModel     string
	router          *ai.Router
	filter          *compliance.Filter
	resolver        *providers.Resolver
	sessionService  *sessions.Service
	taskExecutor    *tasks.Executor
	agentLoop       *agent.Loop
	agentEnabled    bool
	embeddingModel  string
	mainModel       string
	fallbackModel   string
	embeddingDim    int
	redisNamespace  string
	cache           *cacheLayer
	logger          *zap.Logger
}

// Config holds pipeline configuration.
type Config struct {
	DB              *sql.DB
	Redis           *redis.Client
	GroqClient      *ai.Client
	ClaudeClient    *ai.AnthropicClient // tier-5 fallback
	ClaudeModel     string
	OllamaClient    *ai.Client // embeddings (native Ollama /api/embeddings)
	OllamaLLMClient *ai.Client // tier-3 LLM fallback (OpenAI-compat /v1)
	OllamaLLMModel  string
	OpenAIClient    *ai.Client // tier-4 fallback
	OpenAIModel     string
	Router          *ai.Router
	Resolver        *providers.Resolver
	SessionService  *sessions.Service
	TaskExecutor    *tasks.Executor
	Agent           *agent.Loop
	AgentEnabled    bool
	EmbeddingModel  string
	MainModel       string
	FallbackModel   string
	EmbeddingDim    int
	RedisNamespace  string
	Logger          *zap.Logger
}

// ChatInput represents a user chat message.
type ChatInput struct {
	TenantID     uuid.UUID
	TenantSlug   string       // original slug from X-Tenant-ID (may differ from UUID)
	SessionID    uuid.UUID
	Question     string
	History      []ai.Message // previous messages in this session
	BusinessType string       // "flp" | "codevertex" | "cafe" | "generic"
	PublicMode   bool         // true for /public/chat — prefer cache/KB, avoid agent loops
}

// ChatResult contains the AI response and metadata.
type ChatResult struct {
	Response    string  `json:"response"`
	RawResponse string  `json:"-"` // pre-compliance-filter; used for session history only
	Intent      string  `json:"intent"`
	Confidence  float64 `json:"confidence"`
	Sources     int     `json:"sources"`
	Cached      bool    `json:"cached"`
	Model       string  `json:"model"`
}

// NewPipeline creates a new RAG pipeline.
func NewPipeline(cfg Config) *Pipeline {
	p := &Pipeline{
		db:              cfg.DB,
		redis:           cfg.Redis,
		groqClient:      cfg.GroqClient,
		claudeClient:    cfg.ClaudeClient,
		claudeModel:     cfg.ClaudeModel,
		ollamaClient:    cfg.OllamaClient,
		ollamaLLMClient: cfg.OllamaLLMClient,
		ollamaLLMModel:  cfg.OllamaLLMModel,
		openaiClient:    cfg.OpenAIClient,
		openaiModel:     cfg.OpenAIModel,
		router:          cfg.Router,
		filter:          compliance.NewFilter(),
		resolver:        cfg.Resolver,
		sessionService:  cfg.SessionService,
		taskExecutor:    cfg.TaskExecutor,
		agentLoop:       cfg.Agent,
		agentEnabled:    cfg.AgentEnabled,
		embeddingModel:  cfg.EmbeddingModel,
		mainModel:       cfg.MainModel,
		fallbackModel:   cfg.FallbackModel,
		embeddingDim:    cfg.EmbeddingDim,
		redisNamespace:  cfg.RedisNamespace,
		logger:          cfg.Logger.Named("rag"),
	}
	p.cache = newCacheLayer(cfg.Redis, cfg.RedisNamespace, p.logger)
	return p
}

// resolveClient returns the platform-level Groq client and model.
// AI models are shared resources — NOT per-tenant. Configured once by platform admin.
func (p *Pipeline) resolveClient(ctx context.Context) (*ai.Client, string) {
	if p.resolver != nil {
		client, mainModel, _, err := p.resolver.ResolveGroqClient(ctx)
		if err == nil && client != nil {
			return client, mainModel
		}
		p.logger.Debug("resolver failed, using static client", zap.Error(err))
	}
	return p.groqClient, p.mainModel
}
