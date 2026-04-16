package config

import (
	"fmt"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config aggregates all runtime settings for the AI service.
type Config struct {
	App      AppConfig      `envPrefix:""`
	HTTP     HTTPConfig     `envPrefix:"HTTP_"`
	Database DatabaseConfig `envPrefix:""`
	Redis    RedisConfig    `envPrefix:"REDIS_"`
	Security SecurityConfig `envPrefix:"MF_SECURITY_"`
	AI       AIConfig       `envPrefix:"MF_AI_"`
}

type AppConfig struct {
	Environment string `env:"APP_ENV" envDefault:"development"`
	ServiceName string `env:"APP_NAME" envDefault:"marketflow-ai"`
}

type HTTPConfig struct {
	Host              string        `env:"HOST" envDefault:"0.0.0.0"`
	Port              int           `env:"PORT" envDefault:"4001"`
	AllowedOrigins    string        `env:"ALLOWED_ORIGINS" envDefault:"http://localhost:3000"`
	ReadTimeout       time.Duration `env:"READ_TIMEOUT" envDefault:"30s"`
	WriteTimeout      time.Duration `env:"WRITE_TIMEOUT" envDefault:"120s"`
	IdleTimeout       time.Duration `env:"IDLE_TIMEOUT" envDefault:"120s"`
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"5s"`
	ShutdownTimeout   time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`
}

type DatabaseConfig struct {
	URL             string        `env:"POSTGRES_URL"`
	MaxOpenConns    int           `env:"POSTGRES_MAX_OPEN_CONNS" envDefault:"4"`
	MaxIdleConns    int           `env:"POSTGRES_MAX_IDLE_CONNS" envDefault:"2"`
	ConnMaxLifetime time.Duration `env:"POSTGRES_CONN_MAX_LIFETIME" envDefault:"15m"`
}

type RedisConfig struct {
	Addr      string `env:"ADDR" envDefault:"127.0.0.1:6379"`
	Password  string `env:"PASSWORD"`
	DB        int    `env:"DB" envDefault:"0"`
	EnableTLS bool   `env:"ENABLE_TLS" envDefault:"false"`
	Namespace string `env:"NAMESPACE" envDefault:"marketflow-ai"`
}

type SecurityConfig struct {
	JWKSUrl       string `env:"JWKS_URL" envDefault:"https://sso.codevertexitsolutions.com/api/v1/.well-known/jwks.json"`
	JWTIssuer     string `env:"JWT_ISSUER" envDefault:"https://sso.codevertexitsolutions.com"`
	JWTAudience   string `env:"JWT_AUDIENCE" envDefault:"codevertex"`
	RequireJWT    bool   `env:"REQUIRE_JWT" envDefault:"true"`
	EncryptionKey string `env:"ENCRYPTION_KEY"`
}

// AIConfig holds AI/LLM provider settings (defaults; overridden by ProviderSetting DB).
type AIConfig struct {
	// Groq (primary LLM)
	GroqAPIKey      string `env:"GROQ_API_KEY"`
	GroqBaseURL     string `env:"GROQ_BASE_URL" envDefault:"https://api.groq.com/openai/v1"`
	GroqRouterModel string `env:"GROQ_ROUTER_MODEL" envDefault:"llama-3.1-8b-instant"`
	GroqMainModel   string `env:"GROQ_MAIN_MODEL" envDefault:"llama-3.3-70b-versatile"`
	GroqFallback    string `env:"GROQ_FALLBACK_MODEL" envDefault:"gemma2-9b-it"`
	Temperature     float64 `env:"TEMPERATURE" envDefault:"0.7"`
	MaxTokens       int    `env:"MAX_TOKENS" envDefault:"2048"`

	// Ollama (embeddings) — shared cluster-wide pod in shared-infra namespace.
	OllamaURL      string `env:"OLLAMA_URL" envDefault:"http://ollama.shared-infra.svc.cluster.local:11434"`
	EmbeddingModel string `env:"EMBEDDING_MODEL" envDefault:"nomic-embed-text"`
	EmbeddingDim   int    `env:"EMBEDDING_DIM" envDefault:"768"`
	OllamaFallbackModel string `env:"OLLAMA_FALLBACK_MODEL" envDefault:"llama3.1:8b"`

	// Anthropic (premium fallback)
	ClaudeAPIKey string `env:"CLAUDE_API_KEY"`
	ClaudeModel  string `env:"CLAUDE_MODEL" envDefault:"claude-haiku-4-5-20251001"`

	// MarketFlow API for KB/provider config
	MarketFlowAPIURL string `env:"MARKETFLOW_API_URL" envDefault:"http://marketflow-api.marketflow.svc.cluster.local:4000"`

	// Agentic RAG (platform-level, not per-tenant).
	AgentEnabled   bool   `env:"AGENT_ENABLED" envDefault:"true"`
	AgentMaxIters  int    `env:"AGENT_MAX_ITERS" envDefault:"5"`
	AgentTokenCap  int    `env:"AGENT_TOKEN_CAP" envDefault:"6000"`
	TavilyAPIKey   string `env:"TAVILY_API_KEY"`
	TavilyBaseURL  string `env:"TAVILY_URL" envDefault:"https://api.tavily.com"`
	TavilyDailyCap int    `env:"TAVILY_DAILY_CAP" envDefault:"20"`

	// Google Custom Search (preferred web_search provider when configured;
	// falls back to Tavily on 429 / quota exhausted).
	GoogleAPIKey         string `env:"GOOGLE_API_KEY"`
	GoogleCSEID          string `env:"GOOGLE_CSE_ID"`
	GoogleSearchDailyCap int    `env:"GOOGLE_SEARCH_DAILY_CAP" envDefault:"80"`
	WebSearchProvider    string `env:"WEB_SEARCH_PROVIDER" envDefault:"google"` // google | tavily

	// Platform API key for service-to-service calls back to marketflow-api
	// (used by the book_meeting agent tool).
	MarketFlowAPIKey string `env:"MARKETFLOW_API_KEY"`
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("POSTGRES_URL is required")
	}
	if cfg.AI.GroqAPIKey == "" {
		cfg.AI.GroqAPIKey = os.Getenv("GROQ_API_KEY")
	}
	return cfg, nil
}
