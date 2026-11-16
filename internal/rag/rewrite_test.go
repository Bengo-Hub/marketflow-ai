package rag

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

// scriptedModel answers each call with the next reply and records the system prompts.
type scriptedModel struct {
	mu      sync.Mutex
	replies []string
	systems []string
}

func (s *scriptedModel) Chat(_ context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req.Messages) > 0 {
		s.systems = append(s.systems, req.Messages[0].Content)
	}
	text := ""
	if len(s.replies) > 0 {
		text, s.replies = s.replies[0], s.replies[1:]
	}
	resp := &ai.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index   int        `json:"index"`
		Message ai.Message `json:"message"`
		Finish  string     `json:"finish_reason"`
	}{Message: ai.Message{Role: "assistant", Content: text}})
	return resp, nil
}

func (s *scriptedModel) ChatStream(ctx context.Context, req ai.ChatRequest) (<-chan ai.StreamDelta, error) {
	resp, _ := s.Chat(ctx, req)
	ch := make(chan ai.StreamDelta, 2)
	ch <- ai.StreamDelta{Content: resp.Choices[0].Message.Content}
	ch <- ai.StreamDelta{Done: true}
	close(ch)
	return ch, nil
}

func (s *scriptedModel) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.systems)
}

func scriptedPipeline(m *scriptedModel) *Pipeline {
	router := llm.NewRouter(llm.Config{Providers: oneProvider{m}, Defaults: llm.DefaultPolicy(llm.Defaults{}), Logger: zap.NewNop()})
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	return NewPipeline(Config{
		Redis: rdb, LLM: router, Router: ai.NewRouter(router.Client(llm.Nano), "", zap.NewNop()),
		RedisNamespace: "test", Logger: zap.NewNop(),
	})
}

// answerOf runs a model answer through the same steps the chat paths use: one rewrite when the
// filter blocks it, then the filter (with its fallback reply).
func answerOf(p *Pipeline, input ChatInput, raw string) string {
	msgs := []ai.Message{{Role: "system", Content: "You are Vera."}, {Role: "user", Content: input.Question}}
	return p.applyFilter(p.rewriteIfBlocked(context.Background(), input, msgs, raw, nil), input.BusinessType)
}

func TestSafeRewrite(t *testing.T) {
	input := ChatInput{Question: "Do you have amoxicillin?", BusinessType: "pharmacy", PublicMode: true}
	blocked := "Yes! Get 20% off amoxicillin this week."

	t.Run("passes first time: no second call", func(t *testing.T) {
		m := &scriptedModel{}
		p := scriptedPipeline(m)
		clean := "Amoxicillin needs a prescription. Bring it in and our pharmacist will help."
		if got := answerOf(p, input, clean); got != clean {
			t.Fatalf("a clean answer must pass unchanged: %q", got)
		}
		if m.calls() != 0 {
			t.Fatalf("no rewrite call expected, got %d", m.calls())
		}
	})

	t.Run("passes on rewrite", func(t *testing.T) {
		rewrite := "Amoxicillin is prescription-only. Bring your prescription and our pharmacist will help."
		m := &scriptedModel{replies: []string{rewrite}}
		p := scriptedPipeline(m)
		if got := answerOf(p, input, blocked); got != rewrite {
			t.Fatalf("the passing rewrite must be sent, got %q", got)
		}
		if m.calls() != 1 {
			t.Fatalf("exactly one rewrite call, got %d", m.calls())
		}
		sys := m.systems[0]
		if !strings.Contains(sys, "YOUR LAST ANSWER BROKE THESE BUSINESS RULES") || !strings.Contains(sys, "prescription-only") || !strings.Contains(sys, "amoxicillin") {
			t.Fatalf("the broken rule and the words must be in the system prompt:\n%s", sys)
		}
	})

	t.Run("fails twice: fallback reply", func(t *testing.T) {
		m := &scriptedModel{replies: []string{"Ozempic now on offer!", "should never be asked"}}
		p := scriptedPipeline(m)
		got := answerOf(p, input, blocked)
		if !strings.Contains(got, "contact the business directly") {
			t.Fatalf("two blocked answers must end in the fallback reply, got %q", got)
		}
		if m.calls() != 1 {
			t.Fatalf("one retry at most, got %d calls", m.calls())
		}
	})

	t.Run("flp keeps its strict filter", func(t *testing.T) {
		rewrite := "Aloe vera gel supports digestive comfort."
		m := &scriptedModel{replies: []string{rewrite}}
		p := scriptedPipeline(m)
		got := answerOf(p, ChatInput{Question: "Does aloe cure ulcers?", BusinessType: "flp"}, "Aloe cures ulcers.")
		if !strings.HasPrefix(got, rewrite) || !strings.Contains(got, "Food and Drug Administration") {
			t.Fatalf("flp rewrite must pass with its disclaimer, got %q", got)
		}
	})
}
