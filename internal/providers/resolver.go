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

// Resolver resolves AI provider config per-tenant.
// Fetches from marketflow-api's ProviderSetting endpoints, caches in Redis (5min TTL).
// Falls back to env vars if API call fails.
type Resolver struct {
	apiBaseURL string
	redis      *redis.Client
	envConfig  *config.AIConfig
	httpClient *http.Client
	namespace  string
	logger     *zap.Logger
}

// NewResolver creates a new provider config resolver.
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

// AISettings holds resolved AI provider settings.
type AISettings struct {
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	RouterModel   string `json:"router_model"`
	MainModel     string `json:"main_model"`
	FallbackModel string `json:"fallback_model"`
	Temperature   string `json:"temperature"`
	MaxTokens     string `json:"max_tokens"`
}

// ResolveGroqSettings gets Groq AI config for a tenant, with caching and env fallback.
func (r *Resolver) ResolveGroqSettings(ctx context.Context, tenantID string) (*AISettings, error) {
	cacheKey := r.namespace + ":provider:" + tenantID + ":ai_llm:groq"

	// 1. Check Redis cache
	cached, err := r.redis.Get(ctx, cacheKey).Result()
	if err == nil && cached != "" {
		var settings AISettings
		if json.Unmarshal([]byte(cached), &settings) == nil {
			return &settings, nil
		}
	}

	// 2. Try fetching from marketflow-api
	settings, err := r.fetchFromAPI(ctx, tenantID, "ai_llm", "groq")
	if err == nil && settings.APIKey != "" {
		// Cache for 5 minutes
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

// ResolveGroqClient creates a Groq AI client for a tenant.
func (r *Resolver) ResolveGroqClient(ctx context.Context, tenantID string) (*ai.Client, string, string, error) {
	settings, err := r.ResolveGroqSettings(ctx, tenantID)
	if err != nil {
		return nil, "", "", err
	}
	if settings.APIKey == "" {
		return nil, "", "", fmt.Errorf("no Groq API key configured for tenant %s", tenantID)
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

// fetchFromAPI calls marketflow-api's provider settings endpoint.
func (r *Resolver) fetchFromAPI(ctx context.Context, tenantID, providerType, providerName string) (*AISettings, error) {
	url := fmt.Sprintf("%s/api/v1/admin/providers/settings?type=%s&name=%s",
		r.apiBaseURL, providerType, providerName)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-ID", tenantID)

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
