package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Resolver resolves AI provider config at platform level.
// AI models (Groq, Ollama, Claude) are shared resources configured once
// by the platform admin — NOT per-tenant. Tenant-specific config only
// applies to Meta Ads, WhatsApp, and FBO-specific settings (handled in marketflow-api).
type Resolver struct {
	apiBaseURL string
	redis      *redis.Client
	envConfig  *config.AIConfig
	httpClient *http.Client
	namespace  string
	logger     *zap.Logger
}

// NewResolver creates a new platform-level provider config resolver.
func NewResolver(apiBaseURL string, redis *redis.Client, envConfig *config.AIConfig, namespace string, logger *zap.Logger) *Resolver {
	return &Resolver{
		apiBaseURL: apiBaseURL,
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

// ResolveGroqSettings gets the platform-level Groq AI config, with caching and env fallback.
func (r *Resolver) ResolveGroqSettings(ctx context.Context) (*AISettings, error) {
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

// fetchFromAPI calls marketflow-api's platform-level provider settings endpoint.
func (r *Resolver) fetchFromAPI(ctx context.Context, providerType, providerName string) (*AISettings, error) {
	url := fmt.Sprintf("%s/api/v1/admin/providers/settings?type=%s&name=%s",
		r.apiBaseURL, providerType, providerName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// No X-Tenant-ID header — platform-level config only

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
		Data []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	settings := &AISettings{}
	for _, item := range result.Data {
		switch item.Key {
		case "base_url":
			settings.BaseURL = item.Value
		case "api_key":
			settings.APIKey = item.Value
		case "router_model":
			settings.RouterModel = item.Value
		case "responder_model", "main_model":
			settings.MainModel = item.Value
		case "fallback_model":
			settings.FallbackModel = item.Value
		case "temperature":
			settings.Temperature = item.Value
		case "max_tokens":
			settings.MaxTokens = item.Value
		}
	}

	return settings, nil
}
