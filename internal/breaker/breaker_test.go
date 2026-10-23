package breaker

import (
	"errors"
	"testing"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

func clocked() (*Set, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	s := New(3, time.Minute, 10*time.Minute)
	s.Now = func() time.Time { return now }
	return s, &now
}

func TestOpensAfterFailuresAndProbesOnce(t *testing.T) {
	s, now := clocked()
	outage := errors.New("503")
	for i := 0; i < 3; i++ {
		if !s.Admit("a") {
			t.Fatalf("closed breaker refused on attempt %d", i)
		}
		s.Failed("a", outage, false)
	}
	if s.Admit("a") {
		t.Fatal("open breaker admitted a caller")
	}
	*now = now.Add(time.Minute + time.Second)
	if !s.Admit("a") || s.Admit("a") {
		t.Fatal("want exactly one half-open probe")
	}
	s.Failed("a", outage, false) // probe failed: open again
	if s.Admit("a") {
		t.Fatal("a failed probe must re-open the breaker")
	}
	*now = now.Add(time.Minute + time.Second)
	if !s.Admit("a") {
		t.Fatal("no probe after the second cooldown")
	}
	s.Succeeded("a")
	if !s.Admit("a") || s.Tripped("a") {
		t.Fatal("a good probe must close the breaker")
	}
}

func TestOurBadRequestAndCallerCancelDoNotCount(t *testing.T) {
	s, _ := clocked()
	for i := 0; i < 5; i++ {
		s.Failed("a", &ai.StatusError{Code: 400}, false)
		s.Failed("a", errors.New("context deadline exceeded"), true)
	}
	if s.Tripped("a") {
		t.Fatal("a 4xx or a caller giving up opened the breaker")
	}
}

func TestRejectedKeyOpensAtOnceForLonger(t *testing.T) {
	s, now := clocked()
	s.Failed("a", &ai.StatusError{Code: 401}, false)
	*now = now.Add(2 * time.Minute)
	if s.Admit("a") {
		t.Fatal("a rejected key must stay open past the normal cooldown")
	}
	*now = now.Add(9 * time.Minute)
	if !s.Admit("a") {
		t.Fatal("no probe after the auth cooldown")
	}
}

func TestUnreportedProbeExpires(t *testing.T) {
	s, now := clocked()
	s.OpenUntil("a", now.Add(-time.Second))
	if !s.Admit("a") || !s.Probing("a") {
		t.Fatal("the expired breaker should hand out a probe")
	}
	// The probe never reports back (an earlier route answered).
	*now = now.Add(time.Minute + time.Second)
	if !s.Admit("a") {
		t.Fatal("an unreported probe blocked the route for good")
	}
}

func TestCallerCancelDuringProbeLetsNextCallerProbe(t *testing.T) {
	s, now := clocked()
	s.OpenUntil("a", now.Add(-time.Second))
	s.Admit("a")
	s.Failed("a", errors.New("canceled"), true)
	if !s.Admit("a") {
		t.Fatal("after a cancelled probe the next caller must probe at once")
	}
}
