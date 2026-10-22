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
	return p, ok
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
	if s.cfg.OllamaURL != "" {
		// Cluster-internal and keyless; the policy names the model.
		set["ollama"] = llm.Provider{
			Client: ai.NewClient(s.cfg.OllamaURL+"/v1", "", s.logger),
			Models: llm.Models{Main: s.cfg.OllamaFallbackModel},
		}
	}
	if c, model, err := s.resolver.ResolveOpenAIClient(ctx); err == nil {
		set["openai"] = llm.Provider{Client: c, Models: llm.Models{Main: model}}
	}
	if c, model, err := s.resolver.ResolveAnthropicClientFromDB(ctx); err == nil {
		set["anthropic"] = llm.Provider{Client: c, Models: llm.Models{Main: model}}
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
