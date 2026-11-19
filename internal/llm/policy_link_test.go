package llm

import (
	"context"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// The default routing names provider aliases, so it follows the models saved per provider:
// changing or syncing Anthropic's main and plus models moves premium and premium_plus with them,
// and the cost estimate follows the resolved model.
func TestDefaultRoutingFollowsTheProviderModels(t *testing.T) {
	anth := &fakeClient{reply: "ok", usage: ai.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}}
	src := fakeSource{"anthropic": {Client: anth, Models: Models{Main: "claude-haiku-6", Premium: "claude-sonnet-6"}}}
	r := NewRouter(Config{Providers: src, Defaults: DefaultPolicy(Defaults{})})

	_, call, err := r.Complete(context.Background(), PremiumPlus, ai.ChatRequest{})
	if err != nil || call.Model != "claude-sonnet-6" {
		t.Fatalf("premium_plus = %+v, %v", call, err)
	}
	if call.CostUSD != 12 || !call.Paid { // sonnet family: 2 in + 10 out per million
		t.Fatalf("premium_plus cost = %v paid %v", call.CostUSD, call.Paid)
	}
	_, call, err = r.Complete(context.Background(), Premium, ai.ChatRequest{})
	if err != nil || call.Model != "claude-haiku-6" || call.CostUSD != 0.6 {
		t.Fatalf("premium = %+v, %v", call, err)
	}

	// Without a premium model the alias falls back to the main one.
	if got := modelFor(Target{Model: ModelPremium}, Provider{Models: Models{Main: "m"}}); got != "m" {
		t.Fatalf("premium alias fallback = %q", got)
	}
}

// A stored policy that names a model and a price keeps them; stored prices merge over the
// defaults; an unlisted model takes its provider's "*" price.
func TestStoredPolicyAndPricesOverride(t *testing.T) {
	p, err := ParsePolicy(`{"classes":{"premium":[{"provider":"openai","model":"gpt-9","input_usd_per_m":5,"output_usd_per_m":20}]},
		"prices":{"openai":{"^gpt-9-mini":{"input_usd_per_m":0.2,"output_usd_per_m":0.8}}}}`, DefaultPolicy(Defaults{}))
	if err != nil {
		t.Fatal(err)
	}
	if ts := p.Classes[Premium]; len(ts) != 1 || ts[0].Model != "gpt-9" || ts[0].InputUSDPerM != 5 {
		t.Fatalf("premium = %+v", ts)
	}
	if got := p.PriceFor("openai", "gpt-9-mini"); got.In != 0.2 {
		t.Fatalf("stored price = %+v", got)
	}
	if got := p.PriceFor("openai", "gpt-4o-mini-2024-07-18"); got.In != 0.15 {
		t.Fatalf("default price kept = %+v", got)
	}
	if got := p.PriceFor("openai", "gpt-5.1"); got.In != 2.5 {
		t.Fatalf("unlisted model = %+v", got)
	}
	if got := p.PriceFor("ollama", "llama3.1:8b"); got.In != 0 || got.Out != 0 {
		t.Fatalf("local model = %+v", got)
	}
}
