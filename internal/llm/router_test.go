package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// fakeClient answers with a fixed reply or error and records the requests.
type fakeClient struct {
	mu    sync.Mutex
	err   error
	reply string
	usage ai.Usage
	reqs  []ai.ChatRequest
}

func (f *fakeClient) Chat(_ context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	resp := &ai.ChatResponse{Usage: f.usage}
	resp.Choices = append(resp.Choices, struct {
		Index   int        `json:"index"`
		Message ai.Message `json:"message"`
		Finish  string     `json:"finish_reason"`
	}{Message: ai.Message{Role: "assistant", Content: f.reply}})
	return resp, nil
}

func (f *fakeClient) ChatStream(_ context.Context, req ai.ChatRequest) (<-chan ai.StreamDelta, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan ai.StreamDelta, 2)
	ch <- ai.StreamDelta{Content: f.reply}
	ch <- ai.StreamDelta{Done: true}
	close(ch)
	return ch, nil
}

func (f *fakeClient) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

type fakeSource map[string]Provider

func (s fakeSource) Provider(_ context.Context, name string) (Provider, bool) {
	p, ok := s[name]
	return p, ok
}

func testPolicy() Policy {
	return Policy{Classes: map[Class][]Target{
		Standard: {
			{Provider: "groq", Model: ModelMain, InputUSDPerM: 1, OutputUSDPerM: 2},
			{Provider: "groq", Model: ModelFallback},
			{Provider: "anthropic", Model: "claude-haiku-5-5", Effort: "low"},
		},
		Premium: {
			{Provider: "anthropic", Model: "claude-haiku-5-5", Effort: "medium"},
			{Provider: "groq", Model: ModelMain},
		},
	}}
}

func TestCompleteUsesFirstTargetAndMeters(t *testing.T) {
	groq := &fakeClient{reply: "ok", usage: ai.Usage{PromptTokens: 1_000_000, CompletionTokens: 500_000, TotalTokens: 1_500_000}}
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: groq, Models: Models{Main: "big", Fallback: "small"}}},
		Defaults:  testPolicy(),
	})
	m := &Meter{}
	resp, call, err := r.Complete(WithMeter(context.Background(), m), Standard, ai.ChatRequest{Model: "ignored"})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" || call.Model != "big" || call.Provider != "groq" {
		t.Fatalf("unexpected call %+v", call)
	}
	if groq.reqs[0].Model != "big" {
		t.Fatalf("request model = %q, want the resolved main model", groq.reqs[0].Model)
	}
	// 1M prompt at 1 USD/M plus 0.5M completion at 2 USD/M.
	if call.CostUSD != 2 {
		t.Fatalf("cost = %v, want 2", call.CostUSD)
	}
	if tokens, usd := m.Totals(); tokens != 1_500_000 || usd != 2 {
		t.Fatalf("meter totals = %d, %v", tokens, usd)
	}
}

func TestCompleteFallsBackAndBreakerOpens(t *testing.T) {
	failing := &fakeClient{err: errors.New("503")}
	anth := &fakeClient{reply: "claude"}
	now := time.Unix(1_700_000_000, 0)
	r := NewRouter(Config{
		Providers: fakeSource{
			"groq":      {Client: failing, Models: Models{Main: "big", Fallback: "small"}},
			"anthropic": {Client: anth, Models: Models{Main: "claude-haiku-5-5"}},
		},
		Defaults: testPolicy(),
	})
	r.nowFn = func() time.Time { return now }

	for i := 0; i < breakerFailures; i++ {
		_, call, err := r.Complete(context.Background(), Standard, ai.ChatRequest{})
		if err != nil || call.Provider != "anthropic" {
			t.Fatalf("round %d: want anthropic fallback, got %+v err=%v", i, call, err)
		}
	}
	if got := anth.reqs[0].Effort; got != "low" {
		t.Fatalf("effort = %q, want the target's low", got)
	}
	before := failing.calls()
	// Both groq targets are now open, so they are tried last and anthropic answers
	// without another groq attempt.
	if _, call, _ := r.Complete(context.Background(), Standard, ai.ChatRequest{}); call.Provider != "anthropic" {
		t.Fatalf("want anthropic first while groq breakers are open, got %+v", call)
	}
	if failing.calls() != before {
		t.Fatalf("open breaker was tried before a healthy target")
	}
	// After the cooldown groq is tried first again.
	now = now.Add(breakerCooldown + time.Second)
	_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{})
	if failing.calls() == before {
		t.Fatalf("groq not retried after cooldown")
	}
}

func TestToolCallsSkipAnthropic(t *testing.T) {
	failing := &fakeClient{err: errors.New("down")}
	anth := &fakeClient{reply: "claude"}
	r := NewRouter(Config{
		Providers: fakeSource{
			"groq":      {Client: failing, Models: Models{Main: "big"}},
			"anthropic": {Client: anth, Models: Models{Main: "claude-haiku-5-5"}},
		},
		Defaults: testPolicy(),
	})
	req := ai.ChatRequest{Tools: []ai.ToolDef{{Type: "function"}}}
	if _, _, err := r.Complete(context.Background(), Standard, req); err == nil {
		t.Fatalf("want error when only non-tool providers remain")
	}
	if anth.calls() != 0 {
		t.Fatalf("anthropic must not receive tool requests")
	}
}

func TestNoConfiguredTarget(t *testing.T) {
	r := NewRouter(Config{Providers: fakeSource{}, Defaults: testPolicy()})
	_, _, err := r.Complete(context.Background(), Premium, ai.ChatRequest{})
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("want ErrNoTarget, got %v", err)
	}
}

func TestStreamFallsBack(t *testing.T) {
	r := NewRouter(Config{
		Providers: fakeSource{
			"groq":      {Client: &fakeClient{err: errors.New("down")}, Models: Models{Main: "big", Fallback: "small"}},
			"anthropic": {Client: &fakeClient{reply: "hi"}, Models: Models{Main: "x"}},
		},
		Defaults: testPolicy(),
	})
	ch, call, err := r.Stream(context.Background(), Standard, ai.ChatRequest{})
	if err != nil || call.Provider != "anthropic" {
		t.Fatalf("stream fallback: %+v %v", call, err)
	}
	if d := <-ch; d.Content != "hi" {
		t.Fatalf("delta = %+v", d)
	}
}

func TestBoundClientIgnoresModel(t *testing.T) {
	groq := &fakeClient{reply: "ok"}
	r := NewRouter(Config{Providers: fakeSource{"groq": {Client: groq, Models: Models{Main: "big"}}}, Defaults: testPolicy()})
	if _, err := r.Client(Standard).Chat(context.Background(), ai.ChatRequest{Model: "caller-model"}); err != nil {
		t.Fatal(err)
	}
	if groq.reqs[0].Model != "big" {
		t.Fatalf("model = %q", groq.reqs[0].Model)
	}
}

func TestPolicyReloadAndInvalidKept(t *testing.T) {
	raw := `{"classes":{"standard":[{"provider":"anthropic","model":"m1","input_usd_per_m":1,"output_usd_per_m":1}]},"decide":{"thresholds":{"route":0.9}}}`
	now := time.Unix(1_700_000_000, 0)
	r := NewRouter(Config{
		Providers: fakeSource{},
		Policy:    func(context.Context) (string, error) { return raw, nil },
		Defaults:  DefaultPolicy(Defaults{}),
	})
	r.nowFn = func() time.Time { return now }
	p := r.Policy(context.Background())
	if got := p.Classes[Standard]; len(got) != 1 || got[0].Model != "m1" {
		t.Fatalf("stored standard class not applied: %+v", got)
	}
	if len(p.Classes[Nano]) == 0 {
		t.Fatalf("missing class must keep defaults")
	}
	if p.Threshold("route") != 0.9 || p.Threshold("injection") != 0.5 {
		t.Fatalf("threshold merge wrong: %v", p.Decide.Thresholds)
	}
	// An invalid stored policy keeps the last good one.
	raw = `{"classes":{"turbo":[{"provider":"x"}]}}`
	now = now.Add(policyTTL + time.Second)
	if p2 := r.Policy(context.Background()); p2.Classes[Standard][0].Model != "m1" {
		t.Fatalf("invalid policy replaced the good one")
	}
}

func TestEstimateUSDWithCache(t *testing.T) {
	tg := Target{InputUSDPerM: 1, OutputUSDPerM: 5}
	u := ai.Usage{PromptTokens: 1_000_000, CacheReadTokens: 800_000, CacheWriteTokens: 0, CompletionTokens: 0}
	// 200k uncached at 1 + 800k cached at 0.1 = 0.2 + 0.08.
	if got := EstimateUSD(tg, u); got < 0.2799 || got > 0.2801 {
		t.Fatalf("cost = %v", got)
	}
}

// Reasoning models get reasoning_effort (low unless the target says otherwise),
// others never do: their reasoning tokens count against max_tokens.
func TestReasoningEffortForReasoningModels(t *testing.T) {
	groq := &fakeClient{reply: "ok"}
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: groq, Models: Models{Main: "openai/gpt-oss-120b", Router: "openai/gpt-oss-20b"}}},
		Defaults: Policy{Classes: map[Class][]Target{
			Nano:     {{Provider: "groq", Model: ModelRouter}},
			Standard: {{Provider: "groq", Model: ModelMain, Effort: "medium"}},
			Premium:  {{Provider: "groq", Model: "llama-3.3-70b-versatile"}},
		}},
	})
	for _, c := range []Class{Nano, Standard, Premium} {
		if _, _, err := r.Complete(context.Background(), c, ai.ChatRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := groq.reqs[0].ReasoningEffort; got != "low" {
		t.Errorf("gpt-oss-20b effort = %q, want low", got)
	}
	if got := groq.reqs[1].ReasoningEffort; got != "medium" {
		t.Errorf("target effort = %q, want medium", got)
	}
	if got := groq.reqs[2].ReasoningEffort; got != "" {
		t.Errorf("non-reasoning model got effort %q", got)
	}
}

// A call on a priced target is paid; a target without prices (the local model,
// or Groq set to 0 for its free tier) is not. Only successful calls are metered.
func TestMeterPaid(t *testing.T) {
	free := &fakeClient{reply: "ok"}
	r := NewRouter(Config{
		Providers: fakeSource{"ollama": {Client: free}, "groq": {Client: &fakeClient{reply: "ok"}, Models: Models{Main: "m"}}},
		Defaults: Policy{Classes: map[Class][]Target{
			Nano:     {{Provider: "ollama", Model: "llama3.1:8b"}},
			Standard: {{Provider: "groq", Model: ModelMain, InputUSDPerM: 0.15}},
		}},
	})
	m := &Meter{}
	ctx := WithMeter(context.Background(), m)
	if _, call, err := r.Complete(ctx, Nano, ai.ChatRequest{}); err != nil || call.Paid {
		t.Fatalf("free call: %v %+v", err, call)
	}
	if m.Paid() {
		t.Fatal("only free calls so far")
	}
	if _, call, err := r.Complete(ctx, Standard, ai.ChatRequest{}); err != nil || !call.Paid {
		t.Fatalf("paid call: %v %+v", err, call)
	}
	if !m.Paid() {
		t.Fatal("a paid call was recorded")
	}
	var none *Meter
	if none.Paid() {
		t.Fatal("a nil meter is not paid")
	}
}

// The light class tries the free models (Groq, then local) before paid ones.
func TestLightClassIsFreeFirst(t *testing.T) {
	nano := DefaultPolicy(Defaults{}).Classes[Nano]
	if len(nano) < 3 || nano[0].Provider != "groq" || nano[1].Provider != "groq" || nano[2].Provider != "ollama" {
		t.Fatalf("nano order: %+v", nano)
	}
	for _, tg := range nano[3:] {
		if tg.Provider == "groq" || tg.Provider == "ollama" {
			t.Fatalf("free target after a paid one: %+v", nano)
		}
	}
}
