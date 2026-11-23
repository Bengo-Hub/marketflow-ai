package app

import (
	"context"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"go.uber.org/zap"
)

// providerTTL is how long resolved provider clients are reused before the
// platform settings are read again, so a key rotated on /platform/providers is
// picked up without a restart.
const providerTTL = 5 * time.Minute

// llmProviders resolves the router's providers from platform settings (DB first,
// env fallback) and caches the clients. One caller refreshes at a time; the rest
// keep using the previous set, so a slow settings read never stalls chat.
type llmProviders struct {
	resolver *providers.Resolver
	cfg      *config.AIConfig
	logger   *zap.Logger

	mu         sync.Mutex
	set        map[string]llm.Provider
	missed     map[string]time.Time // names compatible() found unconfigured, and when
	loadedAt   time.Time
	refreshing bool
}

func newLLMProviders(resolver *providers.Resolver, cfg *config.AIConfig, logger *zap.Logger) *llmProviders {
	return &llmProviders{resolver: resolver, cfg: cfg, logger: logger.Named("llm.providers")}
}

// Provider implements llm.ProviderSource.
func (s *llmProviders) Provider(ctx context.Context, name string) (llm.Provider, bool) {
	s.mu.Lock()
	stale := time.Since(s.loadedAt) > providerTTL
	first := s.set == nil
	if stale && (!s.refreshing || first) {
		s.refreshing = true
		s.mu.Unlock()
		next := s.load(ctx)
		s.mu.Lock()
		s.set, s.loadedAt, s.refreshing = next, time.Now(), false
	}
	p, ok := s.set[name]
	s.mu.Unlock()
	if ok {
		return p, true
	}
	return s.compatible(ctx, name)
}

// compatible resolves a provider the fixed set does not know: any platform row
// ai_llm/<name> with a base_url and an api_key is an OpenAI-compatible endpoint
// (OpenRouter, Google Gemini's OpenAI endpoint, Cerebras and the like). A routing
// policy or the media review names it, and Platform > Providers holds its key, so
// a new free fallback needs no code. Misses are remembered for the same TTL.
func (s *llmProviders) compatible(ctx context.Context, name string) (llm.Provider, bool) {
	s.mu.Lock()
	if at, miss := s.missed[name]; miss && time.Since(at) < providerTTL {
		s.mu.Unlock()
		return llm.Provider{}, false
	}
	s.mu.Unlock()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	raw, err := s.resolver.ResolveRaw(rctx, "ai_llm", name)
	var p llm.Provider
	ok := err == nil && raw["base_url"] != "" && raw["api_key"] != ""
	if ok {
		p = llm.Provider{
			Client: ai.NewClient(raw["base_url"], raw["api_key"], s.logger),
			Models: llm.Models{Main: raw["main_model"], Fallback: raw["fallback_model"], Premium: raw["premium_model"]},
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		if s.missed == nil {
			s.missed = map[string]time.Time{}
		}
		s.missed[name] = time.Now()
		return llm.Provider{}, false
	}
	if s.set == nil {
		s.set = map[string]llm.Provider{}
	}
	s.set[name] = p
	s.logger.Info("openai-compatible llm provider resolved", zap.String("provider", name))
	return p, true
}

func (s *llmProviders) load(parent context.Context) map[string]llm.Provider {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	set := make(map[string]llm.Provider, 4)

	if gs, err := s.resolver.ResolveGroqSettings(ctx); err == nil && gs.APIKey != "" {
		set["groq"] = llm.Provider{
			Client: ai.NewClient(firstNonEmpty(gs.BaseURL, s.cfg.GroqBaseURL), gs.APIKey, s.logger),
			Models: llm.Models{
				Main:     firstNonEmpty(gs.MainModel, s.cfg.GroqMainModel),
				Fallback: firstNonEmpty(gs.FallbackModel, s.cfg.GroqFallback),
				Router:   firstNonEmpty(gs.RouterModel, s.cfg.GroqRouterModel),
			},
		}
	}
	// Every provider's models come from Platform > Providers first (what Sync models
	// preselects), then the env fallback, so the default routing follows them.
	if s.cfg.OllamaURL != "" {
		// Cluster-internal and keyless.
		local, _ := s.resolver.ResolveRaw(ctx, "ai_embed", "ollama")
		set["ollama"] = llm.Provider{
			Client: ai.NewClient(s.cfg.OllamaURL+"/v1", "", s.logger),
			Models: llm.Models{Main: firstNonEmpty(local["fallback_model"], s.cfg.OllamaFallbackModel)},
		}
	}
	if c, model, err := s.resolver.ResolveOpenAIClient(ctx); err == nil {
		raw, _ := s.resolver.ResolveRaw(ctx, "ai_llm", "openai")
		set["openai"] = llm.Provider{Client: c, Models: llm.Models{Main: model, Premium: raw["premium_model"]}}
	}
	if c, model, err := s.resolver.ResolveAnthropicClientFromDB(ctx); err == nil {
		raw, _ := s.resolver.ResolveRaw(ctx, "ai_llm", "anthropic")
		set["anthropic"] = llm.Provider{Client: c, Models: llm.Models{Main: model,
			Premium: firstNonEmpty(raw["plus_model"], s.cfg.ClaudePlusModel)}}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	s.logger.Info("llm providers resolved", zap.Strings("providers", names))
	return set
}

// routingPolicySource reads the stored routing policy JSON (platform
// ProviderSetting ai_routing/policy, key "policy").
func routingPolicySource(resolver *providers.Resolver) llm.PolicySource {
	return func(ctx context.Context) (string, error) {
		m, err := resolver.ResolveRaw(ctx, "ai_routing", "policy")
		if err != nil {
			return "", err
		}
		return m["policy"], nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
