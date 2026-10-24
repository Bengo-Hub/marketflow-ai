package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Resolver resolves AI provider config at platform level.
// AI models (Groq, Ollama, Claude, Tavily) are shared resources configured once
// by the platform admin — NOT per-tenant. Tenant-specific config only applies
// to Meta Page tokens, Google Calendar refresh tokens, and FBO-specific
// settings (handled elsewhere).
//
// This resolver calls marketflow-api's /admin/providers/resolve endpoint,
// which returns DECRYPTED secret values (unlike /admin/providers/settings
// which masks). Authenticated via platform X-API-Key.
type Resolver struct {
	apiBaseURL string
	apiKey     string
	redis      *redis.Client
	envConfig  *config.AIConfig
	httpClient *http.Client
	namespace  string
	logger     *zap.Logger

	retiredOnce sync.Once // one warning per process about retired Groq models
}

// NewResolver creates a new platform-level provider config resolver.
func NewResolver(apiBaseURL, platformAPIKey string, redis *redis.Client, envConfig *config.AIConfig, namespace string, logger *zap.Logger) *Resolver {
	return &Resolver{
		apiBaseURL: apiBaseURL,
		apiKey:     platformAPIKey,
		redis:      redis,
		envConfig:  envConfig,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		namespace:  namespace,
		logger:     logger.Named("providers.resolver"),
	}
}

// AISettings holds resolved AI provider settings (platform-level, shared across all tenants).
type AISettings struct {
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	RouterModel   string `json:"router_model"`
	MainModel     string `json:"main_model"`
	FallbackModel string `json:"fallback_model"`
	Temperature   string `json:"temperature"`
	MaxTokens     string `json:"max_tokens"`
}

// retiredGroqModels maps model ids Groq no longer serves on pay-as-you-go to their
// replacements (console.groq.com/docs/deprecations, checked 2026-10-24). The Llama
// ids became enterprise-only on 2026-08-16 and answer 404 model_not_found, which
// silently emptied every chat that still named them.
var retiredGroqModels = map[string]string{
	"llama-3.3-70b-versatile": "openai/gpt-oss-120b",
	"llama-3.1-8b-instant":    "openai/gpt-oss-20b",
	"gemma2-9b-it":            "openai/gpt-oss-20b",
}

// upgradeRetired swaps retired Groq model ids for their replacements, so a value
// saved on /platform/providers before a retirement keeps working.
func (r *Resolver) upgradeRetired(s *AISettings) *AISettings {
	for _, m := range []*string{&s.MainModel, &s.RouterModel, &s.FallbackModel} {
		if next, ok := retiredGroqModels[*m]; ok {
			r.retiredOnce.Do(func() {
				r.logger.Warn("groq settings name retired models; using replacements, update /platform/providers",
					zap.String("example", *m), zap.String("replacement", next))
			})
			*m = next
		}
	}
	return s
}

// ResolveGroqSettings gets the platform-level Groq AI config, with caching and env fallback.
func (r *Resolver) ResolveGroqSettings(ctx context.Context) (*AISettings, error) {
	s, err := r.resolveGroqSettings(ctx)
	if s != nil {
		s = r.upgradeRetired(s)
	}
	return s, err
}

func (r *Resolver) resolveGroqSettings(ctx context.Context) (*AISettings, error) {
	cacheKey := r.namespace + ":provider:platform:ai_llm:groq"

	// 1. Check Redis cache
	cached, err := r.redis.Get(ctx, cacheKey).Result()
	if err == nil && cached != "" {
		var settings AISettings
		if json.Unmarshal([]byte(cached), &settings) == nil {
			return &settings, nil
		}
	}

	// 2. Try fetching from marketflow-api (platform-level provider settings)
	settings, err := r.fetchFromAPI(ctx, "ai_llm", "groq")
	if err == nil && settings.APIKey != "" {
		data, _ := json.Marshal(settings)
		_ = r.redis.Set(ctx, cacheKey, string(data), 5*time.Minute).Err()
		return settings, nil
	}

	if err != nil {
		r.logger.Debug("API fetch failed, using env fallback", zap.Error(err))
	}

	// 3. Fallback to env vars
	return &AISettings{
		BaseURL:       r.envConfig.GroqBaseURL,
		APIKey:        r.envConfig.GroqAPIKey,
		RouterModel:   r.envConfig.GroqRouterModel,
		MainModel:     r.envConfig.GroqMainModel,
		FallbackModel: r.envConfig.GroqFallback,
	}, nil
}

// SearchSettings holds platform-level web-search provider config (Tavily /
// Google Custom Search). Values are resolved DB-first then env-var fallback.
type SearchSettings struct {
	Provider string // "google" | "tavily"
	APIKey   string
	CSEID    string // Google Custom Search Engine ID; empty for Tavily
	BaseURL  string
	DailyCap int
}

// ResolveTavilySettings returns the Tavily web-search config with DB-first,
// env fallback, 5-min Redis cache.
func (r *Resolver) ResolveTavilySettings(ctx context.Context) *SearchSettings {
	cacheKey := r.namespace + ":provider:platform:ai_search:tavily"
	if cached, err := r.redis.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		var s SearchSettings
		if json.Unmarshal([]byte(cached), &s) == nil {
			return &s
		}
	}
	if settings, err := r.fetchRawSettings(ctx, "ai_search", "tavily"); err == nil && settings["api_key"] != "" {
		s := &SearchSettings{
			Provider: "tavily",
			APIKey:   settings["api_key"],
			BaseURL:  firstNonEmpty(settings["base_url"], r.envConfig.TavilyBaseURL),
			DailyCap: parseIntOr(settings["daily_cap"], r.envConfig.TavilyDailyCap),
		}
		data, _ := json.Marshal(s)
		_ = r.redis.Set(ctx, cacheKey, string(data), 5*time.Minute).Err()
		return s
	}
	return &SearchSettings{
		Provider: "tavily",
		APIKey:   r.envConfig.TavilyAPIKey,
		BaseURL:  r.envConfig.TavilyBaseURL,
		DailyCap: r.envConfig.TavilyDailyCap,
	}
}

// ResolveGoogleCSESettings returns the Google Custom Search config with
// DB-first, env fallback, 5-min Redis cache. Reads provider_type=ai_search,
// provider_name=custom_search (matches the UI catalog).
func (r *Resolver) ResolveGoogleCSESettings(ctx context.Context) *SearchSettings {
	cacheKey := r.namespace + ":provider:platform:ai_search:custom_search"
	if cached, err := r.redis.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		var s SearchSettings
		if json.Unmarshal([]byte(cached), &s) == nil {
			return &s
		}
	}
	if settings, err := r.fetchRawSettings(ctx, "ai_search", "custom_search"); err == nil && settings["api_key"] != "" {
		s := &SearchSettings{
			Provider: "google",
			APIKey:   settings["api_key"],
			CSEID:    settings["cse_id"],
			DailyCap: parseIntOr(settings["daily_cap"], r.envConfig.GoogleSearchDailyCap),
		}
		data, _ := json.Marshal(s)
		_ = r.redis.Set(ctx, cacheKey, string(data), 5*time.Minute).Err()
		return s
	}
	return &SearchSettings{
		Provider: "google",
		APIKey:   r.envConfig.GoogleAPIKey,
		CSEID:    r.envConfig.GoogleCSEID,
		DailyCap: r.envConfig.GoogleSearchDailyCap,
	}
}

// fetchRawSettings returns the decrypted settings map for an arbitrary
// platform provider. Same wire-format as fetchFromAPI but without the
// AI-specific key mapping.
func (r *Resolver) fetchRawSettings(ctx context.Context, providerType, providerName string) (map[string]string, error) {
	if r.apiKey == "" {
		return nil, fmt.Errorf("no platform api key — skipping DB resolve")
	}
	url := fmt.Sprintf("%s/api/v1/admin/providers/resolve?type=%s&name=%s",
		r.apiBaseURL, providerType, providerName)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", r.apiKey)
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Settings, nil
}

func parseIntOr(s string, fallback int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		return n
	}
	return fallback
}

// ResolveGroqClient creates a Groq AI client from platform config.
func (r *Resolver) ResolveGroqClient(ctx context.Context) (*ai.Client, string, string, error) {
	settings, err := r.ResolveGroqSettings(ctx)
	if err != nil {
		return nil, "", "", err
	}
	if settings.APIKey == "" {
		return nil, "", "", fmt.Errorf("no Groq API key configured at platform level")
	}

	client := ai.NewClient(settings.BaseURL, settings.APIKey, r.logger)
	mainModel := settings.MainModel
	if mainModel == "" {
		mainModel = r.envConfig.GroqMainModel
	}
	fallbackModel := settings.FallbackModel
	if fallbackModel == "" {
		fallbackModel = r.envConfig.GroqFallback
	}

	return client, mainModel, fallbackModel, nil
}

// fetchFromAPI calls marketflow-api's /admin/providers/resolve endpoint
// with the platform X-API-Key; the response contains decrypted values.
func (r *Resolver) fetchFromAPI(ctx context.Context, providerType, providerName string) (*AISettings, error) {
	if r.apiKey == "" {
		return nil, fmt.Errorf("no platform api key — skipping DB resolve")
	}
	url := fmt.Sprintf("%s/api/v1/admin/providers/resolve?type=%s&name=%s",
		r.apiBaseURL, providerType, providerName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", r.apiKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	settings := &AISettings{
		BaseURL:       result.Settings["base_url"],
		APIKey:        result.Settings["api_key"],
		RouterModel:   result.Settings["router_model"],
		MainModel:     firstNonEmpty(result.Settings["responder_model"], result.Settings["main_model"]),
		FallbackModel: result.Settings["fallback_model"],
		Temperature:   result.Settings["temperature"],
		MaxTokens:     result.Settings["max_tokens"],
	}
	return settings, nil
}

// ResolveOpenAIClient resolves OpenAI config DB-first, no env fallback (optional provider).
func (r *Resolver) ResolveOpenAIClient(ctx context.Context) (*ai.Client, string, error) {
	settings, err := r.fetchFromAPI(ctx, "ai_llm", "openai")
	if err != nil || settings.APIKey == "" {
		return nil, "", fmt.Errorf("OpenAI not configured in DB: %w", err)
	}
	baseURL := firstNonEmpty(settings.BaseURL, "https://api.openai.com/v1")
	model := firstNonEmpty(settings.MainModel, "gpt-4o-mini")
	return ai.NewClient(baseURL, settings.APIKey, r.logger), model, nil
}

// ResolveAnthropicClientFromDB resolves Anthropic/Claude config DB-first, env fallback.
func (r *Resolver) ResolveAnthropicClientFromDB(ctx context.Context) (*ai.AnthropicClient, string, error) {
	settings, err := r.fetchFromAPI(ctx, "ai_llm", "anthropic")
	if err == nil && settings.APIKey != "" {
		baseURL := firstNonEmpty(settings.BaseURL, r.envConfig.ClaudeBaseURL)
		model := firstNonEmpty(settings.MainModel, r.envConfig.ClaudeModel)
		return ai.NewAnthropicClient(baseURL, settings.APIKey, r.envConfig.ClaudeVersion, r.logger), model, nil
	}
	if r.envConfig.ClaudeAPIKey != "" {
		return ai.NewAnthropicClient(r.envConfig.ClaudeBaseURL, r.envConfig.ClaudeAPIKey, r.envConfig.ClaudeVersion, r.logger), r.envConfig.ClaudeModel, nil
	}
	return nil, "", fmt.Errorf("Anthropic not configured in DB or env")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ResolveRaw returns a platform provider's decrypted settings map (for example
// ai_routing/policy or decide/typesafe), cached in Redis for five minutes. A
// provider with no saved row returns an empty map and no error, so callers fall
// back to code defaults.
func (r *Resolver) ResolveRaw(ctx context.Context, providerType, providerName string) (map[string]string, error) {
	cacheKey := r.namespace + ":provider:platform:" + providerType + ":" + providerName
	// The cache is an optimisation: a slow Redis must not hold up the lookup.
	gctx, gcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	cached, gerr := r.redis.Get(gctx, cacheKey).Result()
	gcancel()
	if gerr == nil && cached != "" {
		var m map[string]string
		if json.Unmarshal([]byte(cached), &m) == nil {
			return m, nil
		}
	}
	settings, err := r.fetchRawSettings(ctx, providerType, providerName)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = map[string]string{}
	}
	data, _ := json.Marshal(settings)
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Millisecond)
	_ = r.redis.Set(sctx, cacheKey, string(data), 5*time.Minute).Err()
	scancel()
	return settings, nil
}
