package llm

import (
	"context"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// CompleteOn uses the given targets, not the class's, skipping a provider that is not configured.
func TestCompleteOnUsesGivenTargets(t *testing.T) {
	groq := &fakeClient{reply: "seen"}
	r := NewRouter(Config{
		Providers: fakeSource{"groq": {Client: groq, Models: Models{Main: "big"}}},
		Defaults:  testPolicy(),
	})
	targets := []Target{{Provider: "missing", Model: "x"}, {Provider: "groq", Model: "vision-model"}}
	resp, call, err := r.CompleteOn(context.Background(), Nano, targets, ai.ChatRequest{})
	if err != nil {
		t.Fatalf("complete on: %v", err)
	}
	if resp.Choices[0].Message.Content != "seen" || call.Model != "vision-model" || groq.reqs[0].Model != "vision-model" {
		t.Fatalf("call %+v, request model %q", call, groq.reqs[0].Model)
	}
	if _, _, err := r.CompleteOn(context.Background(), Nano, []Target{{Provider: "missing", Model: "x"}}, ai.ChatRequest{}); err == nil {
		t.Fatal("no usable target must fail")
	}
}
