package providers

import (
	"testing"

	"go.uber.org/zap"
)

func TestRetiredGroqModelsAreReplaced(t *testing.T) {
	r := &Resolver{logger: zap.NewNop()}
	s := r.upgradeRetired(&AISettings{
		MainModel:     "llama-3.3-70b-versatile",
		RouterModel:   "llama-3.1-8b-instant",
		FallbackModel: "gemma2-9b-it",
	})
	if s.MainModel != "openai/gpt-oss-120b" || s.RouterModel != "openai/gpt-oss-20b" || s.FallbackModel != "openai/gpt-oss-20b" {
		t.Fatalf("not replaced: %+v", s)
	}
	kept := r.upgradeRetired(&AISettings{MainModel: "qwen/qwen3.8-27b"})
	if kept.MainModel != "qwen/qwen3.8-27b" {
		t.Fatalf("a current model was changed: %q", kept.MainModel)
	}
}
