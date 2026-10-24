package rag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

// fakeModel answers every call the same way and records the requests.
type fakeModel struct {
	mu     sync.Mutex
	err    error
	text   string
	models []string
}

func (f *fakeModel) Chat(_ context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	f.mu.Lock()
	f.models = append(f.models, req.Model)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	resp := &ai.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index   int        `json:"index"`
		Message ai.Message `json:"message"`
		Finish  string     `json:"finish_reason"`
	}{Message: ai.Message{Role: "assistant", Content: f.text}})
	return resp, nil
}

func (f *fakeModel) ChatStream(_ context.Context, req ai.ChatRequest) (<-chan ai.StreamDelta, error) {
	f.mu.Lock()
	f.models = append(f.models, req.Model)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan ai.StreamDelta, 3)
	ch <- ai.StreamDelta{Content: f.text} // may be empty: a model that said nothing
	ch <- ai.StreamDelta{Done: true}
	close(ch)
	return ch, nil
}

type oneProvider struct{ c ai.ChatClient }

func (o oneProvider) Provider(_ context.Context, name string) (llm.Provider, bool) {
	if name != "groq" {
		return llm.Provider{}, false
	}
	return llm.Provider{Client: o.c, Models: llm.Models{Main: "openai/gpt-oss-120b", Router: "openai/gpt-oss-20b", Fallback: "openai/gpt-oss-20b"}}, true
}

// testPipeline has one fake Groq provider, an unreachable Redis (every cache
// misses) and no embedder, so the knowledge base is empty.
func testPipeline(model *fakeModel) *Pipeline {
	router := llm.NewRouter(llm.Config{Providers: oneProvider{model}, Defaults: llm.DefaultPolicy(llm.Defaults{}), Logger: zap.NewNop()})
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	return NewPipeline(Config{
		Redis: rdb, LLM: router, Router: ai.NewRouter(router.Client(llm.Nano), "", zap.NewNop()),
		RedisNamespace: "test", Logger: zap.NewNop(),
	})
}

func drain(t *testing.T, ch <-chan ai.StreamDelta) string {
	t.Helper()
	var final string
	for _, d := range collect(t, ch) {
		if d.Type == "replace" {
			final = d.Content
		} else {
			final += d.Content
		}
	}
	return final
}

func TestKBDirect(t *testing.T) {
	p := testPipeline(&fakeModel{})
	ctx := context.Background()
	emb := []float32{1, 0}
	faq := SearchResult{Category: "faq", Title: "How does the business work?", Content: "You buy at a discount and earn on resale.", Similarity: 0.81}
	in := ChatInput{Question: "How does the business work?"}

	if got, ok := p.kbDirect(ctx, in, emb, []SearchResult{faq}); !ok || got != faq.Content {
		t.Fatalf("FAQ answer at 0.81: %q %v", got, ok)
	}
	low := faq
	low.Similarity, low.Title = 0.6, "Something else entirely"
	if _, ok := p.kbDirect(ctx, in, emb, []SearchResult{low}); ok {
		t.Fatal("a loose FAQ match must go to the model")
	}
	exact := low
	exact.Title = "how does the business work"
	if _, ok := p.kbDirect(ctx, in, emb, []SearchResult{exact}); !ok {
		t.Fatal("an FAQ whose title is the question answers it")
	}
	doc := SearchResult{Category: "product", Content: "Aloe gel, 1 litre.", Similarity: 0.84}
	if _, ok := p.kbDirect(ctx, in, emb, []SearchResult{doc}); ok {
		t.Fatal("a product entry needs a closer match than an FAQ")
	}
	doc.Similarity = 0.9
	if _, ok := p.kbDirect(ctx, in, emb, []SearchResult{doc}); !ok {
		t.Fatal("a close product entry answers")
	}
	long := faq
	long.Content = ""
	if _, ok := p.kbDirect(ctx, in, emb, []SearchResult{long}); ok {
		t.Fatal("a long entry (no content returned) never answers as is")
	}
	later := faq
	later.ChunkIndex = 2
	if _, ok := p.kbDirect(ctx, in, emb, []SearchResult{later}); ok {
		t.Fatal("only an entry's opening chunk answers")
	}
	follow := in
	follow.History = []ai.Message{{Role: "user", Content: "hi"}}
	if _, ok := p.kbDirect(ctx, follow, emb, []SearchResult{faq}); ok {
		t.Fatal("a follow-up question needs the conversation, not a lookup")
	}
}

func TestKBFallbackNeverEmpty(t *testing.T) {
	in := ChatInput{BusinessType: "flp"}
	if got := kbFallback(in, nil); !strings.Contains(got, "WhatsApp") {
		t.Fatalf("no knowledge: %q", got)
	}
	weak := []SearchResult{{ChunkText: "unrelated", Similarity: 0.3}}
	if got := kbFallback(in, weak); strings.Contains(got, "unrelated") {
		t.Fatalf("an unrelated entry must not be shown: %q", got)
	}
	long := strings.Repeat("Aloe vera gel supports digestion. ", 60)
	got := kbFallback(in, []SearchResult{{ChunkText: long, Similarity: 0.7}})
	if !strings.HasPrefix(got, "Here is what I can share") || len(got) > fallbackLen+300 {
		t.Fatalf("excerpt: %d chars", len(got))
	}
	short := kbFallback(in, []SearchResult{{Content: "Open 8am to 5pm.", ChunkText: "Open 8am", Similarity: 0.6}})
	if !strings.Contains(short, "Open 8am to 5pm.") {
		t.Fatalf("a short entry is shown whole: %q", short)
	}
}

func TestPublicStreamNeverEmpty(t *testing.T) {
	ctx := context.Background()
	in := ChatInput{TenantID: uuid.New(), Question: "How does the business work?", BusinessType: "generic", PublicMode: true}

	// Every model fails: the stream still answers, with no model.
	down := &fakeModel{err: errors.New("model_not_found")}
	ch, meta, err := testPipeline(down).ChatStream(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if text := drain(t, ch); strings.TrimSpace(text) == "" || meta.Model != ModelKBFallback || !FreeAnswer(meta) {
		t.Fatalf("all down: %q model %q", text, meta.Model)
	}

	// A model that streams nothing: the bubble is replaced with the fallback.
	silent := &fakeModel{text: ""}
	ch, meta, err = testPipeline(silent).ChatStream(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if FreeAnswer(meta) {
		t.Fatal("a model was streaming: not free until it turns out empty")
	}
	if text := drain(t, ch); strings.TrimSpace(text) == "" {
		t.Fatal("an empty model reply reached the visitor")
	}
	// After Done the handler sees the fallback and refunds the credit.
	if !FreeAnswer(meta) || meta.Model != ModelKBFallback {
		t.Fatalf("after an empty stream the answer is the fallback: %q", meta.Model)
	}

	// A working model: the public widget uses the light class, so Groq's small model first.
	ok := &fakeModel{text: "You buy at a discount."}
	ch, meta, err = testPipeline(ok).ChatStream(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if text := drain(t, ch); !strings.Contains(text, "discount") || meta.Model != "openai/gpt-oss-20b" {
		t.Fatalf("answer %q from %q", text, meta.Model)
	}
}

func TestChatFallsBackToKnowledgeBase(t *testing.T) {
	down := &fakeModel{err: errors.New("credit balance is too low")}
	res, err := testPipeline(down).Chat(context.Background(), ChatInput{TenantID: uuid.New(), Question: "How does the business work?", BusinessType: "codevertex", PublicMode: true})
	if err != nil || res.Model != ModelKBFallback || !strings.Contains(res.Response, "info@codevertexafrica.com") {
		t.Fatalf("chat with every model down: %v %+v", err, res)
	}
}
