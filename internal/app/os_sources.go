package app

import (
	"context"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/decide"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"go.uber.org/zap"
)

// typeSafeSourceTTL is how long the resolved Jev settings, or their absence,
// are reused in memory. Every decision asks for the adapter, so without this a
// Redis or marketflow-api hiccup would add a settings lookup to each one.
const typeSafeSourceTTL = time.Minute

// typeSafeSource resolves the Jev adapter from the platform ProviderSetting
// decide/typesafe (api_key, optional model and base_url). The adapter is reused
// while the settings are unchanged, so its HTTP client keeps its connections.
func typeSafeSource(resolver *providers.Resolver) decide.PrimarySource {
	var (
		mu       sync.Mutex
		cached   *decide.TypeSafe
		sig      string
		checked  time.Time
		inflight bool
	)
	return func(ctx context.Context) (decide.Adapter, bool) {
		mu.Lock()
		if time.Since(checked) < typeSafeSourceTTL || inflight {
			// Fresh, or another caller is already refreshing: use what we have.
			a := cached
			mu.Unlock()
			return a, a != nil
		}
		inflight = true
		mu.Unlock()

		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		settings, err := resolver.ResolveRaw(rctx, "decide", "typesafe")
		cancel()

		mu.Lock()
		defer mu.Unlock()
		inflight, checked = false, time.Now()
		if err != nil {
			// Keep the last good adapter through a lookup failure; it is retried after the TTL.
			return cached, cached != nil
		}
		if settings["api_key"] == "" {
			cached, sig = nil, ""
			return nil, false
		}
		key := settings["api_key"] + "|" + settings["model"] + "|" + settings["base_url"]
		if cached == nil || key != sig {
			cached = decide.NewTypeSafe(decide.TypeSafeConfig{
				APIKey: settings["api_key"], Model: settings["model"], BaseURL: settings["base_url"],
			})
			sig = key
		}
		return cached, true
	}
}

// registrySource serves the output registry (platform ProviderSetting
// ai_registry/outputs, key "registry", merged over the code defaults), reloaded
// at most every five minutes. A bad stored value keeps the last good registry.
type registrySource struct {
	resolver *providers.Resolver
	logger   *zap.Logger

	mu       sync.Mutex
	current  registry.Registry
	loadedAt time.Time
}

func newRegistrySource(resolver *providers.Resolver, logger *zap.Logger) *registrySource {
	return &registrySource{resolver: resolver, logger: logger.Named("registry"), current: registry.Defaults()}
}

// Get returns the current registry.
func (s *registrySource) Get(ctx context.Context) registry.Registry {
	s.mu.Lock()
	if time.Since(s.loadedAt) < 5*time.Minute {
		r := s.current
		s.mu.Unlock()
		return r
	}
	s.loadedAt = time.Now() // one reload at a time; others use the current value
	s.mu.Unlock()

	next := registry.Defaults()
	if settings, err := s.resolver.ResolveRaw(ctx, "ai_registry", "outputs"); err == nil {
		if merged, merr := registry.Merge(next, settings["registry"]); merr == nil {
			next = merged
		} else {
			s.logger.Warn("stored output registry is invalid, keeping defaults", zap.Error(merr))
		}
	}
	s.mu.Lock()
	s.current = next
	s.mu.Unlock()
	return next
}
