package decide

import (
	"context"
	"testing"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// slowAdapter answers only when its context ends, like a provider that hangs.
type slowAdapter struct{ calls int }

func (s *slowAdapter) Name() string { return "typesafe" }

func (s *slowAdapter) Decide(ctx context.Context, _ string, _ map[string]Question) (Answers, Usage, error) {
	s.calls++
	<-ctx.Done()
	return nil, Usage{}, ctx.Err()
}

func withBudgets(t *testing.T, primary, total time.Duration) {
	pb, db := primaryBudget, decideBudget
	primaryBudget, decideBudget = primary, total
	t.Cleanup(func() { primaryBudget, decideBudget = pb, db })
}

func TestSlowPrimaryIsCutAndFallbackAnswers(t *testing.T) {
	withBudgets(t, 200*time.Millisecond, 2*time.Second)
	fb, _ := fallbackWith(`{"answers":{"has_budget":{"noul":0.9}}}`)
	d := New(Config{Primary: func(context.Context) (Adapter, bool) { return &slowAdapter{}, true }, Fallback: fb})
	start := time.Now()
	_, u, err := d.Decide(context.Background(), "t", "x", map[string]Question{"has_budget": Noul("budget stated?")})
	if err != nil || !u.Fallback {
		t.Fatalf("want a fallback answer, got %+v %v", u, err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("a hung primary held the decision for %v", el)
	}
}

func TestWholeDecisionIsBounded(t *testing.T) {
	withBudgets(t, 100*time.Millisecond, 300*time.Millisecond)
	// Both adapters hang: the decision must still end, with an error, so the
	// caller takes its safe branch.
	d := New(Config{
		Primary:  func(context.Context) (Adapter, bool) { return &slowAdapter{}, true },
		Fallback: &slowAdapter{},
	})
	start := time.Now()
	if _, _, err := d.Decide(context.Background(), "t", "x", map[string]Question{"q": Noul("?")}); err == nil {
		t.Fatal("two hung adapters must fail")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("decision ran %v past its budget", el)
	}
}

func TestOwnBadQuestionDoesNotTripJevBreaker(t *testing.T) {
	primary := &countingAdapter{err: &ai.StatusError{Provider: "typesafe", Code: 422}}
	fb, _ := fallbackWith(`{"answers":{"q":{"noul":0.2}}}`)
	d := New(Config{Primary: func(context.Context) (Adapter, bool) { return primary, true }, Fallback: fb})
	for i := 0; i < primaryFailures+2; i++ {
		_, _, _ = d.Decide(context.Background(), "t", "x", map[string]Question{"q": Noul("?")})
	}
	if primary.calls != primaryFailures+2 {
		t.Fatalf("a 422 opened Jev's breaker: %d calls", primary.calls)
	}
}

func TestRejectedKeyAndHalfOpenProbe(t *testing.T) {
	primary := &countingAdapter{err: &ai.StatusError{Provider: "typesafe", Code: 401}}
	fb, _ := fallbackWith(`{"answers":{"q":{"noul":0.2}}}`)
	d := New(Config{Primary: func(context.Context) (Adapter, bool) { return primary, true }, Fallback: fb})
	now := time.Unix(1_700_000_000, 0)
	d.nowFn = func() time.Time { return now }
	qs := map[string]Question{"q": Noul("?")}

	_, _, _ = d.Decide(context.Background(), "t", "x", qs)
	now = now.Add(primaryCooldown + time.Second) // past a normal cooldown, inside the auth one
	_, _, _ = d.Decide(context.Background(), "t", "x", qs)
	if primary.calls != 1 {
		t.Fatalf("a rejected key was retried within the auth cooldown (%d calls)", primary.calls)
	}

	// After the auth cooldown, one probe; it fails and the breaker re-opens.
	now = now.Add(authCooldown)
	primary.err = &ai.StatusError{Provider: "typesafe", Code: 503}
	_, _, _ = d.Decide(context.Background(), "t", "x", qs)
	_, _, _ = d.Decide(context.Background(), "t", "x", qs)
	if primary.calls != 2 {
		t.Fatalf("want exactly one probe, got %d calls", primary.calls)
	}

	// The next probe succeeds and closes the breaker.
	now = now.Add(primaryCooldown + time.Second)
	primary.err = nil
	_, _, _ = d.Decide(context.Background(), "t", "x", qs)
	_, u, _ := d.Decide(context.Background(), "t", "x", qs)
	if primary.calls != 4 || u.Fallback {
		t.Fatalf("breaker did not close after a good probe: calls=%d fallback=%v", primary.calls, u.Fallback)
	}
}
