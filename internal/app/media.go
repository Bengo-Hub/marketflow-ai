package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
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

// higgsfieldWebhookSecrets reads the Standard Webhooks signing secret saved on the
// media/higgsfield provider setting (webhook_secret, "whsec_..."), falling back to
// MF_HIGGSFIELD_WEBHOOK_SECRET locally. A second secret (webhook_secret_previous) covers a
// rotation. The answer is reused for a minute, and a failed lookup keeps the last good one.
func higgsfieldWebhookSecrets(resolver *providers.Resolver) func(ctx context.Context) []string {
	var (
		mu   sync.Mutex
		last []string
		at   time.Time
	)
	return func(ctx context.Context) []string {
		mu.Lock()
		defer mu.Unlock()
		if !at.IsZero() && time.Since(at) < time.Minute {
			return last
		}
		var list []string
		ok := false
		if resolver != nil {
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			settings, err := resolver.ResolveRaw(rctx, "media", "higgsfield")
			cancel()
			if err == nil {
				ok = true
				for _, k := range []string{"webhook_secret", "webhook_secret_previous"} {
					if v := strings.TrimSpace(settings[k]); strings.HasPrefix(v, "whsec_") {
						list = append(list, v)
					}
				}
			}
		}
		if len(list) == 0 {
			if v := strings.TrimSpace(os.Getenv("MF_HIGGSFIELD_WEBHOOK_SECRET")); strings.HasPrefix(v, "whsec_") {
				list = append(list, v)
				ok = true
			}
		}
		if ok || at.IsZero() {
			last, at = list, time.Now()
		}
		return last
	}
}

// publicBaseURL is where providers reach marketflow-ai (MF_PUBLIC_URL); in the cluster it
// defaults to the public ingress host. Locally it is empty, so no webhook is asked for.
func publicBaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("MF_PUBLIC_URL")), "/"); v != "" {
		return v
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return "https://marketflowai.codevertexafrica.com"
	}
	return ""
}

// mediaPolicySource reads the routing policy from platform settings (ProviderSetting platform /
// media / routing: prefer, premium_for_exempt, premium_budget_usd), reused for a minute. A failed
// lookup keeps the last good policy, and with none the code defaults (the bake-off evidence) hold.
func mediaPolicySource(resolver *providers.Resolver) media.PolicySource {
	var (
		mu   sync.Mutex
		last = media.DefaultRoutePolicy()
		at   time.Time
	)
	return func(ctx context.Context) media.RoutePolicy {
		mu.Lock()
		defer mu.Unlock()
		if resolver == nil || time.Since(at) < time.Minute {
			return last
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		settings, err := resolver.ResolveRaw(rctx, "media", "routing")
		cancel()
		at = time.Now()
		if err == nil {
			last = media.PolicyFromSettings(settings)
		}
		return last
	}
}

// defaultReviewTargets are the vision models the self-review tries when ProviderSetting platform /
// media / review sets no "targets": Groq's Llama 4 Scout (cheap and fast, reads images), then
// Anthropic's Haiku. Change the setting, not this.
var defaultReviewTargets = []llm.Target{
	{Provider: "groq", Model: "meta-llama/llama-4-scout-17b-16e-instruct", MaxTokens: 600},
	{Provider: "anthropic", Model: "claude-haiku-4-5", MaxTokens: 600},
}

// mediaReviewer wires the self-review: its settings from ProviderSetting platform / media / review
// (reused for a minute, the last good copy kept on a failed read) and its model call through the
// LLM router on the configured vision targets.
func mediaReviewer(resolver *providers.Resolver, router *llm.Router) *media.Reviewer {
	if router == nil {
		return nil
	}
	var (
		mu   sync.Mutex
		last = media.DefaultReviewSettings()
		at   time.Time
	)
	settings := func(ctx context.Context) media.ReviewSettings {
		mu.Lock()
		defer mu.Unlock()
		if resolver == nil || time.Since(at) < time.Minute {
			return last
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		raw, err := resolver.ResolveRaw(rctx, "media", "review")
		cancel()
		at = time.Now()
		if err == nil {
			last = media.ReviewSettingsFromMap(raw)
		}
		return last
	}
	call := func(ctx context.Context, system, user string, images []string) (string, string, float64, error) {
		targets := defaultReviewTargets
		if raw := settings(ctx).Targets; raw != "" {
			var list []llm.Target
			if json.Unmarshal([]byte(raw), &list) == nil && len(list) > 0 {
				targets = list
			}
		}
		resp, c, err := router.CompleteOn(ctx, llm.Nano, targets, ai.ChatRequest{
			Messages:    []ai.Message{{Role: "system", Content: system}, {Role: "user", Content: user, Images: images}},
			Temperature: 0.1, MaxTokens: 600,
		})
		if err != nil {
			return "", "", 0, err
		}
		return resp.Choices[0].Message.Content, c.Provider + "/" + c.Model, c.CostUSD, nil
	}
	return media.NewReviewer(call, settings)
}
