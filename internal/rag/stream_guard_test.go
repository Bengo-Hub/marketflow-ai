package rag

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/compliance"
)

// collect drains a guarded stream with a timeout so a missing Done fails the test instead of hanging.
func collect(t *testing.T, ch <-chan ai.StreamDelta) []ai.StreamDelta {
	t.Helper()
	var got []ai.StreamDelta
	timeout := time.After(2 * time.Second)
	for {
		select {
		case d, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, d)
		case <-timeout:
			t.Fatal("stream did not finish")
		}
	}
}

func feed(parts []string, withDone bool) <-chan ai.StreamDelta {
	ch := make(chan ai.StreamDelta, len(parts)+1)
	for _, p := range parts {
		ch <- ai.StreamDelta{Content: p}
	}
	if withDone {
		ch <- ai.StreamDelta{Done: true}
	}
	close(ch)
	return ch
}

func replaceOf(deltas []ai.StreamDelta) (string, bool) {
	for _, d := range deltas {
		if d.Type == "replace" {
			return d.Content, true
		}
	}
	return "", false
}

func TestGuardStreamNoReplaceWhenUnchanged(t *testing.T) {
	out := collect(t, guardStream(feed([]string{"Hello ", "there"}, true), func(s string) string { return s }))
	if _, ok := replaceOf(out); ok {
		t.Fatal("unchanged text must not produce a replace delta")
	}
	if last := out[len(out)-1]; !last.Done {
		t.Fatal("stream must end with Done")
	}
}

func TestGuardStreamReplacesBeforeDone(t *testing.T) {
	out := collect(t, guardStream(feed([]string{"bad ", "text"}, true), func(string) string { return "clean" }))
	got, ok := replaceOf(out)
	if !ok || got != "clean" {
		t.Fatalf("replace = %q, %v; want clean", got, ok)
	}
	if out[len(out)-2].Type != "replace" || !out[len(out)-1].Done {
		t.Fatal("replace must come immediately before Done")
	}
}

// An abrupt provider close (no Done) must still be finalized, so it cannot skip compliance.
func TestGuardStreamFinalizesWhenUpstreamClosesWithoutDone(t *testing.T) {
	out := collect(t, guardStream(feed([]string{"bad"}, false), func(string) string { return "clean" }))
	if got, ok := replaceOf(out); !ok || got != "clean" {
		t.Fatalf("abrupt close skipped finalize: replace=%q ok=%v", got, ok)
	}
	if !out[len(out)-1].Done {
		t.Fatal("stream must still end with Done")
	}
}

// End to end with the real filter: the streaming path now gets the same compliance treatment
// as the non-streaming path.
func TestStreamComplianceMatchesNonStreamingPath(t *testing.T) {
	p := &Pipeline{filter: compliance.NewFilter(), logger: zap.NewNop()}
	finalize := func(bt string) func(string) string {
		return func(s string) string { return p.applyFilter(ScreenOutput(s), bt) }
	}

	t.Run("flp cure claim is replaced", func(t *testing.T) {
		raw := "Aloe vera cures diabetes."
		got, ok := replaceOf(collect(t, guardStream(feed([]string{raw}, true), finalize("flp"))))
		if !ok {
			t.Fatal("an FLP cure claim streamed through without a replace")
		}
		if strings.Contains(strings.ToLower(got), "cures diabetes") {
			t.Fatalf("replacement still carries the claim: %q", got)
		}
		if want := p.applyFilter(raw, "flp"); got != want {
			t.Fatalf("stream result %q differs from the non-streaming filter %q", got, want)
		}
	})
	t.Run("flp wellness copy gains its disclaimer", func(t *testing.T) {
		raw := "Our aloe drink supports digestive wellness."
		got, ok := replaceOf(collect(t, guardStream(feed([]string{raw}, true), finalize("flp"))))
		if !ok || !strings.Contains(got, "Food and Drug Administration") {
			t.Fatalf("FLP wellness copy streamed without its disclaimer: %q", got)
		}
	})
	t.Run("generic clean copy is untouched", func(t *testing.T) {
		out := collect(t, guardStream(feed([]string{"Our POS works offline and syncs later."}, true), finalize("generic")))
		if _, ok := replaceOf(out); ok {
			t.Fatal("clean generic copy must not be replaced")
		}
	})
	t.Run("generic financial promise is replaced", func(t *testing.T) {
		got, ok := replaceOf(collect(t, guardStream(feed([]string{"Join now for guaranteed returns."}, true), finalize("generic"))))
		if !ok || strings.Contains(strings.ToLower(got), "guaranteed returns") {
			t.Fatalf("financial promise streamed through: %q", got)
		}
	})
}
