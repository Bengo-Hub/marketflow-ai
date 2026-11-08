package team

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/decide"
)

func TestNormalizePhrase(t *testing.T) {
	cases := map[string]string{
		"Where is my team?":                 "where is my team",
		"  Create   AND show me the TEAM!!": "create and show me the team",
		"Nionyeshe timu yangu, tafadhali":   "nionyeshe timu yangu tafadhali",
		"email me at a@b.co":                "",
		"call 0712 345 678 about the team":  "",
		"one two three four five six seven eight nine ten eleven twelve thirteen": "",
	}
	for in, want := range cases {
		if got := NormalizePhrase(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

type fakeLearner struct {
	mu      sync.Mutex
	known   []string
	learned chan string
}

func (f *fakeLearner) Phrases(context.Context, uuid.UUID, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.known...), nil
}

func (f *fakeLearner) RecordPhrase(_ context.Context, _ uuid.UUID, _, phrase string) error {
	f.mu.Lock()
	f.known = append(f.known, phrase)
	f.mu.Unlock()
	f.learned <- phrase
	return nil
}

type countingDecider struct {
	mu    sync.Mutex
	calls int
	prob  float64
}

func (d *countingDecider) Decide(_ context.Context, _, _ string, qs map[string]decide.Question) (decide.Answers, decide.Usage, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	out := decide.Answers{}
	for id := range qs {
		out[id] = decide.Answer{Type: decide.KindNoul, Prob: d.prob, Confidence: 0.9}
	}
	return out, decide.Usage{}, nil
}

// An unknown wording with a team word costs one decision, is learned, and the same wording then
// matches with no decision. A message with no team word never costs one, and a confident no is
// not learned.
func TestTeamIntentLearns(t *testing.T) {
	d := &countingDecider{prob: 0.9}
	r := &Runtime{decider: d}
	l := &fakeLearner{learned: make(chan string, 1)}
	tid := uuid.New()
	ctx := context.Background()

	if ok, how := r.TeamIntent(ctx, tid, "set up my marketing team", l); !ok || how != "pattern" || d.calls != 0 {
		t.Fatalf("fixed pattern, no decision: %v %s %d", ok, how, d.calls)
	}
	if ok, how := r.TeamIntent(ctx, tid, "Where is my team?", l); !ok || how != "decided" || d.calls != 1 {
		t.Fatalf("decided once: %v %s %d", ok, how, d.calls)
	}
	select {
	case p := <-l.learned:
		if p != "where is my team" {
			t.Fatalf("learned %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a confident yes is learned")
	}
	if ok, how := r.TeamIntent(ctx, tid, "where is my TEAM", l); !ok || how != "learned" || d.calls != 1 {
		t.Fatalf("the learned wording needs no decision: %v %s %d", ok, how, d.calls)
	}
	if ok, _ := r.TeamIntent(ctx, tid, "write three ads for the lotion", l); ok || d.calls != 1 {
		t.Fatal("no team word, no decision")
	}
	d.prob = 0.1
	if ok, _ := r.TeamIntent(ctx, tid, "ask the team to plan this month's posts", l); ok || d.calls != 2 {
		t.Fatalf("a confident no is not a team request: %d", d.calls)
	}
	select {
	case p := <-l.learned:
		t.Fatalf("a no must not be learned: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}
