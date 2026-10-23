package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// hangClient blocks until its context ends, like a provider that accepted the
// request and went silent.
type hangClient struct{ fakeClient }

func (h *hangClient) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	h.fakeClient.mu.Lock()
	h.fakeClient.reqs = append(h.fakeClient.reqs, req)
	h.fakeClient.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

// silentStream connects, then never sends a token.
type silentStream struct{ fakeClient }

func (s *silentStream) ChatStream(ctx context.Context, _ ai.ChatRequest) (<-chan ai.StreamDelta, error) {
	ch := make(chan ai.StreamDelta)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// failFastProbe records whether the router asked it to fail fast.
type failFastProbe struct {
	fakeClient
	sawFailFast []bool
}

func (p *failFastProbe) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	p.sawFailFast = append(p.sawFailFast, ai.FailFast(ctx))
	return p.fakeClient.Chat(ctx, req)
}

func twoTargets(first, second string, timeout int) Policy {
	return Policy{Classes: map[Class][]Target{Standard: {
		{Provider: first, Model: "a", TimeoutSeconds: timeout},
		{Provider: second, Model: "b", TimeoutSeconds: timeout},
	}}}
}

func TestHungTargetTimesOutAndFallsThrough(t *testing.T) {
	hung := &hangClient{}
	ok := &fakeClient{reply: "fallback"}
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: hung}, "openai": {Client: ok}},
		Defaults:  twoTargets("groq", "openai", 1),
	})
	start := time.Now()
	_, call, err := r.Complete(context.Background(), Standard, ai.ChatRequest{})
	if err != nil || call.Provider != "openai" {
		t.Fatalf("want openai fallback, got %+v %v", call, err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("fallback took %v; the hung target must be cut at its timeout", d)
	}
}

func TestClassBudgetCapsTheWholeChain(t *testing.T) {
	prev := classBudget[Standard]
	classBudget[Standard] = 1500 * time.Millisecond
	t.Cleanup(func() { classBudget[Standard] = prev })

	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: &hangClient{}}, "openai": {Client: &hangClient{}}},
		Defaults:  twoTargets("groq", "openai", 60),
	})
	start := time.Now()
	if _, _, err := r.Complete(context.Background(), Standard, ai.ChatRequest{}); err == nil {
		t.Fatal("two hung targets must fail")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("chain ran %v past the class budget", d)
	}
}

func TestOnlyLastTargetMayWaitOnRateLimits(t *testing.T) {
	a := &failFastProbe{fakeClient: fakeClient{err: &ai.StatusError{Code: 503}}}
	b := &failFastProbe{fakeClient: fakeClient{reply: "ok"}}
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: a}, "openai": {Client: b}},
		Defaults:  twoTargets("groq", "openai", 5),
	})
	if _, _, err := r.Complete(context.Background(), Standard, ai.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if !a.sawFailFast[0] || b.sawFailFast[0] {
		t.Fatalf("fail-fast first=%v last=%v, want true then false", a.sawFailFast, b.sawFailFast)
	}
}

func TestBadRequestDoesNotTripBreaker(t *testing.T) {
	bad := &fakeClient{err: &ai.StatusError{Code: 400, Body: "bad param"}}
	ok := &fakeClient{reply: "ok"}
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: bad}, "openai": {Client: ok}},
		Defaults:  twoTargets("groq", "openai", 5),
	})
	for i := 0; i < breakerFailures+2; i++ {
		_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{})
	}
	if bad.calls() != breakerFailures+2 {
		t.Fatalf("a 400 opened the breaker: groq tried %d times", bad.calls())
	}
}

func TestRejectedKeyOpensBreakerAtOnceForLonger(t *testing.T) {
	denied := &fakeClient{err: &ai.StatusError{Code: 401}}
	ok := &fakeClient{reply: "ok"}
	now := time.Unix(1_700_000_000, 0)
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: denied}, "openai": {Client: ok}},
		Defaults:  twoTargets("groq", "openai", 5),
	})
	r.nowFn = func() time.Time { return now }
	_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{})
	now = now.Add(breakerCooldown + time.Second)
	_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{})
	if denied.calls() != 1 {
		t.Fatalf("a rejected key was retried within the auth cooldown (%d calls)", denied.calls())
	}
}

func TestHalfOpenAllowsOneProbe(t *testing.T) {
	failing := &fakeClient{err: errors.New("503")}
	ok := &fakeClient{reply: "ok"}
	now := time.Unix(1_700_000_000, 0)
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: failing}, "openai": {Client: ok}},
		Defaults:  twoTargets("groq", "openai", 5),
	})
	r.nowFn = func() time.Time { return now }
	for i := 0; i < breakerFailures; i++ {
		_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{})
	}
	opened := failing.calls()
	now = now.Add(breakerCooldown + time.Second)
	_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{}) // the probe, fails
	_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{}) // re-opened: skipped
	if failing.calls() != opened+1 {
		t.Fatalf("want exactly one probe after the cooldown, got %d", failing.calls()-opened)
	}
	// The probe succeeds next time and closes the breaker.
	now = now.Add(breakerCooldown + time.Second)
	failing.err = nil
	failing.reply = "back"
	_, call, _ := r.Complete(context.Background(), Standard, ai.ChatRequest{})
	if call.Provider != "groq" {
		t.Fatalf("successful probe should answer, got %+v", call)
	}
	if _, call, _ := r.Complete(context.Background(), Standard, ai.ChatRequest{}); call.Provider != "groq" {
		t.Fatal("breaker did not close after a successful probe")
	}
}

func TestUnreportedProbeExpires(t *testing.T) {
	// groq's breaker is open; openai (ordered first) answers every time, so the
	// claimed probe on groq is never run. The claim must not block groq forever.
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: &fakeClient{reply: "g"}}, "openai": {Client: &fakeClient{reply: "o"}}},
		Defaults:  twoTargets("openai", "groq", 5),
	})
	now := time.Unix(1_700_000_000, 0)
	r.nowFn = func() time.Time { return now }
	r.br.OpenUntil(key(Target{Provider: "groq", Model: "b"}, "b"), now.Add(-time.Second))
	_, _, _ = r.Complete(context.Background(), Standard, ai.ChatRequest{}) // claims the probe
	now = now.Add(breakerCooldown + time.Second)
	order := r.order(twoTargets("openai", "groq", 5).Classes[Standard])
	if len(order) != 2 || order[1].Provider != "groq" {
		t.Fatalf("order = %+v", order)
	}
	if !r.br.Probing(key(Target{Provider: "groq", Model: "b"}, "b")) {
		t.Fatal("a fresh probe should be claimed once the window passes")
	}
}

func TestSilentStreamMovesOn(t *testing.T) {
	prev := firstTokenTimeout
	firstTokenTimeout = 300 * time.Millisecond
	t.Cleanup(func() { firstTokenTimeout = prev })

	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: &silentStream{}}, "openai": {Client: &fakeClient{reply: "hi"}}},
		Defaults:  twoTargets("groq", "openai", 30),
	})
	start := time.Now()
	ch, call, err := r.Stream(context.Background(), Standard, ai.ChatRequest{})
	if err != nil || call.Provider != "openai" {
		t.Fatalf("want openai stream, got %+v %v", call, err)
	}
	if d := <-ch; d.Content != "hi" {
		t.Fatalf("first delta = %+v", d)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("silent stream held the chat for %v", d)
	}
}

func TestCallerCancelDoesNotTripBreaker(t *testing.T) {
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: &hangClient{}}},
		Defaults:  Policy{Classes: map[Class][]Target{Standard: {{Provider: "groq", Model: "a", TimeoutSeconds: 30}}}},
	})
	for i := 0; i < breakerFailures+1; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _, _ = r.Complete(ctx, Standard, ai.ChatRequest{})
		cancel()
	}
	if r.br.Tripped(key(Target{Provider: "groq", Model: "a"}, "a")) {
		t.Fatal("callers giving up opened the provider's breaker")
	}
}
