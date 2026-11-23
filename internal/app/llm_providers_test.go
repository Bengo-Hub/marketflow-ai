package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/config"
	"github.com/bengobox/marketflow-ai/internal/providers"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// A provider the fixed set does not know resolves from its ai_llm row when it has a base URL and
// a key (an OpenAI-compatible free tier such as OpenRouter), and an unconfigured name is not
// looked up again on every call.
func TestCompatibleProvider(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Query().Get("name") {
		case "openrouter":
			_, _ = w.Write([]byte(`{"settings":{"base_url":"https://openrouter.ai/api/v1","api_key":"k","main_model":"m:free"}}`))
		default:
			_, _ = w.Write([]byte(`{"settings":{}}`))
		}
	}))
	defer srv.Close()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // unreachable: the cache is skipped
	res := providers.NewResolver(srv.URL, "key", rdb, &config.AIConfig{}, "test", zap.NewNop())
	s := newLLMProviders(res, &config.AIConfig{}, zap.NewNop())
	ctx := context.Background()

	p, ok := s.compatible(ctx, "openrouter")
	if !ok || p.Models.Main != "m:free" || p.Client == nil {
		t.Fatalf("openrouter: ok=%v models=%+v", ok, p.Models)
	}
	if _, ok := s.compatible(ctx, "nothing"); ok {
		t.Fatal("an unconfigured name must not resolve")
	}
	before := calls.Load()
	if _, ok := s.compatible(ctx, "nothing"); ok || calls.Load() != before {
		t.Fatalf("a miss must be remembered (calls %d then %d)", before, calls.Load())
	}
}
