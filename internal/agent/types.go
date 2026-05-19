// Package agent implements an agentic RAG loop using OpenAI-compatible tool-calling.
//
// The loop feeds the conversation (plus tool schemas) to the model; when the
// assistant response contains tool_calls the agent dispatches each call to the
// registered Tool, appends a "tool" role message with the result, and re-invokes
// the model. It terminates when the assistant returns a plain content message,
// when max iterations is reached, or when the token budget is exhausted.
package agent

import (
	"context"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/google/uuid"
)

// Tool is invoked by the agent when the model chooses it.
type Tool interface {
	Name() string
	Description() string
	// Schema returns a JSON-schema object for the tool's arguments.
	Schema() map[string]any
	Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error)
}

// TraceEntry records one iteration of the loop for telemetry / audit.
type TraceEntry struct {
	Iteration  int               `json:"iteration"`
	ToolCalls  []TraceToolCall   `json:"tool_calls,omitempty"`
	AssistMsg  string            `json:"assistant_message,omitempty"`
	PromptTok  int               `json:"prompt_tokens"`
	CompletTok int               `json:"completion_tokens"`
	Finish     string            `json:"finish_reason,omitempty"`
}

// TraceToolCall is a per-tool record inside a TraceEntry.
type TraceToolCall struct {
	Name      string `json:"name"`
	Args      string `json:"args"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

// Result is the final agent output.
type Result struct {
	Answer       string       `json:"answer"`
	Iterations   int          `json:"iterations"`
	TotalTokens  int          `json:"total_tokens"`
	ToolCallsN   int          `json:"tool_calls"`
	Trace        []TraceEntry `json:"trace"`
	Model        string       `json:"model"`
	StoppedEarly bool         `json:"stopped_early,omitempty"`
	StopReason   string       `json:"stop_reason,omitempty"`
}

// Input is the per-conversation input to the loop.
type Input struct {
	TenantID     uuid.UUID
	SystemPrompt string
	History      []ai.Message
	Question     string
	MaxIters     int  // optional per-request override; 0 means use Loop default
	ForceToolUse bool // set true for task_execute / data_query — forces ToolChoice:"required" on iter 1
}
