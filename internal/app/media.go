package app

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/providers"
)

// mediaKeyEnv names the local-development fallbacks (marketflow-ai .env, never
// committed). Production keys are platform ProviderSettings media/<provider>,
// key api_key, saved on /platform/providers.
var mediaKeyEnv = map[string][]string{
	"fal":        {"MF_FAL_API_KEY", "FAL_KEY"},
	"higgsfield": {"MF_HIGGSFIELD_API_KEY"},
	"elevenlabs": {"MF_ELEVEN_LABS_API_KEY", "ELEVENLABS_API_KEY"},
	"creatomate": {"MF_CREATOMATE_API_KEY"},
}

// splitList reads a comma-separated setting, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mediaKeySource resolves a provider's key from platform settings, falling back
// to the environment. Each provider's answer is reused for a minute, and a
// failed lookup keeps the last good key, so a settings or Redis hiccup never
// stalls a render.
func mediaKeySource(resolver *providers.Resolver) media.KeySource {
	type entry struct {
		key string
		at  time.Time
	}
	var (
		mu    sync.Mutex
		cache = map[string]entry{}
	)
	return func(ctx context.Context, provider string) string {
		mu.Lock()
		e, ok := cache[provider]
		mu.Unlock()
		if ok && time.Since(e.at) < time.Minute {
			return e.key
		}
		key := ""
		if resolver != nil {
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			settings, err := resolver.ResolveRaw(rctx, "media", provider)
			cancel()
			if err != nil && ok {
				key = e.key // keep the last good key through a lookup failure
			} else if err == nil {
				key = strings.TrimSpace(settings["api_key"])
			}
		}
		if key == "" {
			for _, name := range mediaKeyEnv[provider] {
				if v := strings.TrimSpace(os.Getenv(name)); v != "" {
					key = v
					break
				}
			}
		}
		mu.Lock()
		cache[provider] = entry{key: key, at: time.Now()}
		mu.Unlock()
		return key
	}
}
