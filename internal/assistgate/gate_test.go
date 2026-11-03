package assistgate

import (
	"context"
	"errors"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/decide"
)

type fakeDecider struct {
	prob     float64
	category string
	fallback bool
	err      error
	calls    int
}

func (f *fakeDecider) Decide(context.Context, string, string, map[string]decide.Question) (decide.Answers, decide.Usage, error) {
	f.calls++
	if f.err != nil {
		return nil, decide.Usage{}, f.err
	}
	return decide.Answers{
		"marketing_request": {Type: decide.KindNoul, Prob: f.prob},
		"category":          {Type: decide.KindChoice, Choice: f.category, Confidence: 0.8},
	}, decide.Usage{Fallback: f.fallback}, nil
}

func TestRuleScore(t *testing.T) {
	cases := map[string]int{
		"hello how are you":                            0,
		"Make a 30s TikTok video ad for our new shoes": 4, // tiktok, video, ad + action
		"nitengenezee tangazo la biashara":             3, // tangazo, biashara + action
		"what is my invoice balance":                   0,
		"post it":                                      2, // post is a topic and an action
		"ad ad ad":                                     1,
	}
	for text, want := range cases {
		if got := RuleScore(text); got != want {
			t.Errorf("%q: got %d, want %d", text, got, want)
		}
	}
}

func TestGateLayers(t *testing.T) {
	ctx := context.Background()

	// Layer 1: no marketing word means no model call at all.
	d := &fakeDecider{prob: 0.99, category: "create_content"}
	if v := New(d).Check(ctx, "Habari, my order has not arrived", false); v.Pass || d.calls != 0 || v.Source != SourceRules {
		t.Fatalf("plain chat must stop at the rules: %+v calls=%d", v, d.calls)
	}

	// Layer 2: Jev decides above its bar.
	d = &fakeDecider{prob: 0.86, category: "create_content"}
	if v := New(d).Check(ctx, "Make a video ad for our holiday coding classes", false); !v.Pass || v.Source != SourceJev {
		t.Fatalf("a clear request must pass on Jev: %+v", v)
	}
	d = &fakeDecider{prob: 0.66, category: "create_content"}
	if v := New(d).Check(ctx, "Make a video ad for our holiday coding classes", false); v.Pass {
		t.Fatalf("0.66 is under Jev's bar: %+v", v)
	}
	// The fallback model has a lower bar, since the decide layer shrinks its answers.
	d = &fakeDecider{prob: 0.66, category: "create_content", fallback: true}
	if v := New(d).Check(ctx, "Make a video ad for our holiday coding classes", false); !v.Pass || v.Source != SourceFallback {
		t.Fatalf("fallback above its bar must pass: %+v", v)
	}
	// A confident "not marketing" stops a message that only mentions a marketing word.
	d = &fakeDecider{prob: 0.75, category: "not_marketing"}
	if v := New(d).Check(ctx, "I saw your ad, what are your opening hours?", false); v.Pass {
		t.Fatalf("not_marketing must win: %+v", v)
	}

	// An open session lets a short follow-up through.
	d = &fakeDecider{prob: 0.58, category: "create_content"}
	if v := New(d).Check(ctx, "shorter", true); !v.Pass {
		t.Fatalf("a follow-up in session must pass: %+v", v)
	}

	// Layer 3: both models down, the rules decide strictly, and nothing errors.
	d = &fakeDecider{err: errors.New("jev and fallback down")}
	if v := New(d).Check(ctx, "Make a TikTok video ad", false); !v.Pass || v.Source != SourceRules {
		t.Fatalf("a strong rule score must pass with no model: %+v", v)
	}
	if v := New(d).Check(ctx, "nice video", false); v.Pass {
		t.Fatalf("a weak rule score must not pass with no model: %+v", v)
	}
	if v := New(nil).Check(ctx, "Make a TikTok video ad", false); !v.Pass {
		t.Fatalf("no decider configured must still work: %+v", v)
	}
}
