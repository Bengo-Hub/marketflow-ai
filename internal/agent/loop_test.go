package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// fakeTool is a deterministic in-memory Tool used to exercise the loop.
type fakeTool struct {
	name     string
	desc     string
	schema   map[string]any
	response string
	calls    atomic.Int32
}

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Description() string          { return f.desc }
func (f *fakeTool) Schema() map[string]any       { return f.schema }
func (f *fakeTool) Invoke(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	f.calls.Add(1)
	return f.response, nil
}

// mockGroqServer returns an httptest.Server that emits a scripted sequence of
// chat-completion responses. The loop reads the head of the queue on each
// request; the final response has no tool_calls so the loop terminates.
func mockGroqServer(t *testing.T, scripted []ai.ChatResponse) *httptest.Server {
	t.Helper()
	var idx atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		i := idx.Add(1) - 1
		if int(i) >= len(scripted) {
			http.Error(w, "script exhausted", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scripted[i])
	}))
}

// TestAgentLoop_KBOnly verifies the simplest path: LLM answers directly
// without calling any tool, loop terminates after one iteration.
func TestAgentLoop_KBOnly(t *testing.T) {
	srv := mockGroqServer(t, []ai.ChatResponse{
		{
			Choices: []struct {
				Index   int        `json:"index"`
				Message ai.Message `json:"message"`
				Finish  string     `json:"finish_reason"`
			}{{
				Message: ai.Message{Role: "assistant", Content: "Forever Aloe Vera Gel (#015) supports digestive wellness."},
				Finish:  "stop",
			}},
		},
	})
	defer srv.Close()

	client := ai.NewClient(srv.URL, "test-key", zap.NewNop())
	loop := New(Config{
		Client:   client,
		Model:    "llama-3.3-70b-versatile",
		Tools:    nil,
		MaxIters: 3,
		TokenCap: 6000,
		Logger:   zap.NewNop(),
	})
	res, err := loop.Run(context.Background(), Input{
		TenantID: uuid.New(),
		Question: "Tell me about Forever Aloe Vera Gel",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Answer, "Aloe Vera Gel") {
		t.Errorf("answer missing product name: %q", res.Answer)
	}
	if res.Iterations != 1 {
		t.Errorf("expected 1 iteration, got %d", res.Iterations)
	}
	if res.ToolCallsN != 0 {
		t.Errorf("expected 0 tool calls, got %d", res.ToolCallsN)
	}
}

// TestAgentLoop_ToolCallingRoundTrip verifies: LLM issues a tool call →
// agent dispatches to the registered tool → second LLM call produces the
// final answer. Mirrors the "is Forever Argi+ in stock?" flow from the
// compliance filter tests.
func TestAgentLoop_ToolCallingRoundTrip(t *testing.T) {
	kbTool := &fakeTool{
		name:     "kb_search",
		desc:     "Search the knowledge base",
		schema:   map[string]any{"type": "object"},
		response: `{"results":[{"title":"Forever Argi+","content":"Nitric oxide booster"}]}`,
	}
	webTool := &fakeTool{
		name:     "web_search",
		desc:     "Search the web",
		schema:   map[string]any{"type": "object"},
		response: `{"answer":"Argi+ remains in stock globally as of April 2026."}`,
	}

	srv := mockGroqServer(t, []ai.ChatResponse{
		{ // Iter 1 — assistant decides to call kb_search then web_search.
			Choices: []struct {
				Index   int        `json:"index"`
				Message ai.Message `json:"message"`
				Finish  string     `json:"finish_reason"`
			}{{
				Message: ai.Message{
					Role: "assistant",
					ToolCalls: []ai.ToolCall{
						{ID: "call_1", Type: "function", Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "kb_search", Arguments: `{"query":"Argi+"}`}},
						{ID: "call_2", Type: "function", Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "web_search", Arguments: `{"query":"Argi+ stock 2026"}`}},
					},
				},
				Finish: "tool_calls",
			}},
		},
		{ // Iter 2 — final answer.
			Choices: []struct {
				Index   int        `json:"index"`
				Message ai.Message `json:"message"`
				Finish  string     `json:"finish_reason"`
			}{{
				Message: ai.Message{Role: "assistant", Content: "Argi+ is in stock globally as of April 2026."},
				Finish:  "stop",
			}},
		},
	})
	defer srv.Close()

	client := ai.NewClient(srv.URL, "test-key", zap.NewNop())
	loop := New(Config{
		Client:   client,
		Model:    "llama-3.3-70b-versatile",
		Tools:    []Tool{kbTool, webTool},
		MaxIters: 5,
		TokenCap: 6000,
		Logger:   zap.NewNop(),
	})
	res, err := loop.Run(context.Background(), Input{
		TenantID: uuid.New(),
		Question: "Is Forever Argi+ in stock?",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Iterations != 2 {
		t.Errorf("expected 2 iterations, got %d", res.Iterations)
	}
	if res.ToolCallsN != 2 {
		t.Errorf("expected 2 tool calls, got %d", res.ToolCallsN)
	}
	if kbTool.calls.Load() != 1 || webTool.calls.Load() != 1 {
		t.Errorf("tools not invoked as expected: kb=%d web=%d", kbTool.calls.Load(), webTool.calls.Load())
	}
	if !strings.Contains(res.Answer, "stock") {
		t.Errorf("final answer missing expected content: %q", res.Answer)
	}
	// Trace should record both iterations and both tool calls.
	if len(res.Trace) != 2 {
		t.Errorf("expected 2 trace entries, got %d", len(res.Trace))
	}
	if len(res.Trace[0].ToolCalls) != 2 {
		t.Errorf("expected 2 tool calls in iter 1, got %d", len(res.Trace[0].ToolCalls))
	}
}

// TestAgentLoop_UnknownTool ensures the loop doesn't crash when the model
// invents a tool that wasn't registered — it records an error message and
// keeps going rather than exploding.
func TestAgentLoop_UnknownTool(t *testing.T) {
	srv := mockGroqServer(t, []ai.ChatResponse{
		{
			Choices: []struct {
				Index   int        `json:"index"`
				Message ai.Message `json:"message"`
				Finish  string     `json:"finish_reason"`
			}{{
				Message: ai.Message{
					Role: "assistant",
					ToolCalls: []ai.ToolCall{{
						ID: "call_x", Type: "function", Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: "does_not_exist", Arguments: "{}"},
					}},
				},
				Finish: "tool_calls",
			}},
		},
		{
			Choices: []struct {
				Index   int        `json:"index"`
				Message ai.Message `json:"message"`
				Finish  string     `json:"finish_reason"`
			}{{
				Message: ai.Message{Role: "assistant", Content: "Sorry, I can't help with that."},
				Finish:  "stop",
			}},
		},
	})
	defer srv.Close()
	client := ai.NewClient(srv.URL, "test-key", zap.NewNop())
	loop := New(Config{
		Client:   client,
		Model:    "llama-3.3-70b-versatile",
		Tools:    []Tool{&fakeTool{name: "kb_search"}},
		MaxIters: 3,
		TokenCap: 6000,
		Logger:   zap.NewNop(),
	})
	res, err := loop.Run(context.Background(), Input{TenantID: uuid.New(), Question: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Answer == "" {
		t.Errorf("expected final answer even with unknown tool call")
	}
	if len(res.Trace) < 1 || len(res.Trace[0].ToolCalls) != 1 || res.Trace[0].ToolCalls[0].Error == "" {
		t.Errorf("trace should record the unknown tool error")
	}
}

// TestAgentLoop_MaxIterations verifies that infinite tool-call loops are
// bounded; when the model keeps calling tools past MaxIters the loop returns
// a synthesised answer with StoppedEarly=true.
func TestAgentLoop_MaxIterations(t *testing.T) {
	// Every response asks for another tool call → loop should cap itself.
	toolCallResp := ai.ChatResponse{
		Choices: []struct {
			Index   int        `json:"index"`
			Message ai.Message `json:"message"`
			Finish  string     `json:"finish_reason"`
		}{{
			Message: ai.Message{
				Role: "assistant",
				ToolCalls: []ai.ToolCall{{
					ID: "loop", Type: "function", Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "kb_search", Arguments: "{}"},
				}},
			},
			Finish: "tool_calls",
		}},
	}
	finalResp := ai.ChatResponse{
		Choices: []struct {
			Index   int        `json:"index"`
			Message ai.Message `json:"message"`
			Finish  string     `json:"finish_reason"`
		}{{
			Message: ai.Message{Role: "assistant", Content: "Final synthesis."},
			Finish:  "stop",
		}},
	}
	// MaxIters=2 means 2 loop iterations + 1 final synthesis call.
	script := []ai.ChatResponse{toolCallResp, toolCallResp, finalResp}
	srv := mockGroqServer(t, script)
	defer srv.Close()
	client := ai.NewClient(srv.URL, "test-key", zap.NewNop())
	loop := New(Config{
		Client:   client,
		Model:    "llama-3.3-70b-versatile",
		Tools:    []Tool{&fakeTool{name: "kb_search", response: "{}"}},
		MaxIters: 2,
		TokenCap: 100000,
		Logger:   zap.NewNop(),
	})
	res, err := loop.Run(context.Background(), Input{TenantID: uuid.New(), Question: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.StoppedEarly {
		t.Errorf("expected StoppedEarly=true when max iterations reached")
	}
	if !strings.Contains(res.StopReason, "max_iterations") {
		t.Errorf("unexpected stop reason: %q", res.StopReason)
	}
}

// TestAgentLoop_NoClient guards the misconfiguration path.
func TestAgentLoop_NoClient(t *testing.T) {
	loop := New(Config{Client: nil, Logger: zap.NewNop()})
	_, err := loop.Run(context.Background(), Input{TenantID: uuid.New(), Question: "hello"})
	if err == nil || !strings.Contains(err.Error(), "client not configured") {
		t.Fatalf("expected client-not-configured error, got %v", err)
	}
}

// Compile-time assertion the test file is registered.
var _ = fmt.Sprintf