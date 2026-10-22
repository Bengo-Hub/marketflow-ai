package rag

import (
	"database/sql"

	"github.com/bengobox/marketflow-ai/internal/agent"
	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/llm"
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

// Pipeline orchestrates the full RAG flow: embed, search, prompt, LLM, filter.
//
// Every generative call goes through the model router (internal/llm) by class,
// so provider fallback, circuit breaking and usage metering live in one place
// instead of a ladder copied per code path.
type Pipeline struct {
	db             *sql.DB
	redis          *redis.Client
	llm            *llm.Router
	ollamaClient   *ai.Client // embeddings only (native Ollama API)
	router         *ai.Router
	filter         *compliance.Filter
	sessionService *sessions.Service
	taskExecutor   *tasks.Executor
	agentLoop      *agent.Loop
	agentEnabled   bool
	embeddingModel string
	embeddingDim   int
	redisNamespace string
	cache          *cacheLayer
	logger         *zap.Logger
}

// Config holds pipeline configuration.
type Config struct {
	DB             *sql.DB
	Redis          *redis.Client
	LLM            *llm.Router
	OllamaClient   *ai.Client // embeddings (native Ollama /api/embeddings)
	Router         *ai.Router
	SessionService *sessions.Service
	TaskExecutor   *tasks.Executor
	Agent          *agent.Loop
	AgentEnabled   bool
	EmbeddingModel string
	EmbeddingDim   int
	RedisNamespace string
	Logger         *zap.Logger
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
		db:             cfg.DB,
		redis:          cfg.Redis,
		llm:            cfg.LLM,
		ollamaClient:   cfg.OllamaClient,
		router:         cfg.Router,
		filter:         compliance.NewFilter(),
		sessionService: cfg.SessionService,
		taskExecutor:   cfg.TaskExecutor,
		agentLoop:      cfg.Agent,
		agentEnabled:   cfg.AgentEnabled,
		embeddingModel: cfg.EmbeddingModel,
		embeddingDim:   cfg.EmbeddingDim,
		redisNamespace: cfg.RedisNamespace,
		logger:         cfg.Logger.Named("rag"),
	}
	p.cache = newCacheLayer(cfg.Redis, cfg.RedisNamespace, p.logger)
	return p
}
