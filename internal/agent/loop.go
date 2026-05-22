package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"go.uber.org/zap"
)

// maxToolCallsPerTurn is the hard limit on tool invocations within a single
// LLM response. Prevents prompt-injection from triggering unbounded tool chains.
const maxToolCallsPerTurn = 5

// mutationTools are tools that create or modify data. They are blocked when the
// request comes from the public widget (PublicMode=true) to prevent abuse.
var mutationTools = map[string]bool{
	"create_deal":         true,
	"create_campaign":     true,
	"create_funnel":       true,
	"create_task":         true,
	"create_activity":     true,
	"create_ticket":       true,
	"book_meeting":        true,
	"draft_email":         true,
	"move_pipeline_stage": true,
	"enrich_contact":      true,
}

// Loop drives the tool-calling agentic RAG conversation.
type Loop struct {
	client    *ai.Client
	model     string
	tools     map[string]Tool
	toolDefs  []ai.ToolDef
	maxIters  int
	tokenCap  int
	logger    *zap.Logger
}

// Config configures a Loop.
type Config struct {
	Client    *ai.Client
	Model     string
	Tools     []Tool
	MaxIters  int
	TokenCap  int
	Logger    *zap.Logger
}

// New constructs a Loop and pre-builds the tool definition list.
func New(cfg Config) *Loop {
	if cfg.MaxIters <= 0 {
		cfg.MaxIters = 5
	}
	if cfg.TokenCap <= 0 {
		cfg.TokenCap = 6000
	}
	tools := make(map[string]Tool, len(cfg.Tools))
	defs := make([]ai.ToolDef, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		tools[t.Name()] = t
		var d ai.ToolDef
		d.Type = "function"
		d.Function.Name = t.Name()
		d.Function.Description = t.Description()
		d.Function.Parameters = t.Schema()
		defs = append(defs, d)
	}
	return &Loop{
		client:   cfg.Client,
		model:    cfg.Model,
		tools:    tools,
		toolDefs: defs,
		maxIters: cfg.MaxIters,
		tokenCap: cfg.TokenCap,
		logger:   cfg.Logger.Named("agent"),
	}
}

// Run executes the agent loop until a terminal assistant message is produced,
// max iterations are reached, or the token budget is exhausted.
func (l *Loop) Run(ctx context.Context, in Input) (*Result, error) {
	if l.client == nil {
		return nil, fmt.Errorf("agent: client not configured")
	}

	messages := make([]ai.Message, 0, len(in.History)+3)
	if in.SystemPrompt != "" {
		messages = append(messages, ai.Message{Role: "system", Content: in.SystemPrompt})
	}
	// Keep last 6 turns of prior history.
	start := 0
	if len(in.History) > 6 {
		start = len(in.History) - 6
	}
	messages = append(messages, in.History[start:]...)
	messages = append(messages, ai.Message{Role: "user", Content: in.Question})

	maxIters := l.maxIters
	if in.MaxIters > 0 {
		maxIters = in.MaxIters
	}

	result := &Result{Model: l.model, Trace: make([]TraceEntry, 0, maxIters)}

	for iter := 1; iter <= maxIters; iter++ {
		// On the first iteration of action/data intents, require a tool call
		// so the model cannot skip straight to generating text. After iter 1
		// revert to "auto" so it can synthesise and stop when ready.
		toolChoice := "auto"
		if in.ForceToolUse && iter == 1 && len(l.toolDefs) > 0 {
			toolChoice = "required"
		}
		req := ai.ChatRequest{
			Model:       l.model,
			Messages:    messages,
			Temperature: 0.4,
			MaxTokens:   2048,
			Tools:       l.toolDefs,
			ToolChoice:  toolChoice,
		}
		resp, err := l.client.Chat(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("agent: llm call iter %d: %w", iter, err)
		}
		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("agent: empty response iter %d", iter)
		}

		choice := resp.Choices[0]
		result.TotalTokens += resp.Usage.TotalTokens
		entry := TraceEntry{
			Iteration:  iter,
			AssistMsg:  choice.Message.Content,
			PromptTok:  resp.Usage.PromptTokens,
			CompletTok: resp.Usage.CompletionTokens,
			Finish:     choice.Finish,
		}

		// Terminal: no tool calls -> answer is final.
		if len(choice.Message.ToolCalls) == 0 {
			result.Trace = append(result.Trace, entry)
			result.Answer = choice.Message.Content
			result.Iterations = iter
			return result, nil
		}

		// Append assistant's tool-calling message to history.
		messages = append(messages, choice.Message)

		// Dispatch each tool call sequentially, subject to per-turn limits.
		turnToolCalls := 0
		for _, tc := range choice.Message.ToolCalls {
			if turnToolCalls >= maxToolCallsPerTurn {
				l.logger.Warn("per-turn tool call limit reached; truncating",
					zap.Int("limit", maxToolCallsPerTurn),
					zap.String("dropped_tool", tc.Function.Name))
				break
			}

			// Block mutation tools in public-mode (widget) requests.
			if in.PublicMode && mutationTools[tc.Function.Name] {
				toolResult := `{"error":"this action is not available from the public widget"}`
				entry.ToolCalls = append(entry.ToolCalls, TraceToolCall{
					Name: tc.Function.Name, Args: tc.Function.Arguments,
					Error: "blocked in public mode",
				})
				messages = append(messages, ai.Message{
					Role: "tool", Content: toolResult, ToolCallID: tc.ID, Name: tc.Function.Name,
				})
				continue
			}

			tool, ok := l.tools[tc.Function.Name]
			if !ok {
				toolResult := fmt.Sprintf(`{"error":"unknown tool: %s"}`, tc.Function.Name)
				entry.ToolCalls = append(entry.ToolCalls, TraceToolCall{
					Name: tc.Function.Name, Args: tc.Function.Arguments, Error: "unknown tool",
				})
				messages = append(messages, ai.Message{
					Role: "tool", Content: toolResult, ToolCallID: tc.ID, Name: tc.Function.Name,
				})
				continue
			}
			started := time.Now()
			out, err := tool.Invoke(ctx, in.TenantID, tc.Function.Arguments)
			latency := time.Since(started).Milliseconds()
			rec := TraceToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments, LatencyMs: latency}
			if err != nil {
				rec.Error = err.Error()
				out = fmt.Sprintf(`{"error":%q}`, err.Error())
			} else {
				rec.Result = out
			}
			entry.ToolCalls = append(entry.ToolCalls, rec)
			result.ToolCallsN++
			turnToolCalls++
			messages = append(messages, ai.Message{
				Role: "tool", Content: out, ToolCallID: tc.ID, Name: tc.Function.Name,
			})
		}

		result.Trace = append(result.Trace, entry)

		// Token budget guard.
		if result.TotalTokens >= l.tokenCap {
			result.StoppedEarly = true
			result.StopReason = "token_cap_reached"
			result.Iterations = iter
			result.Answer = choice.Message.Content
			if result.Answer == "" {
				result.Answer = "I've gathered enough information; let me summarise. " +
					"If you need more detail, please ask a follow-up."
			}
			l.logger.Warn("agent token cap reached", zap.Int("tokens", result.TotalTokens))
			return result, nil
		}
	}

	// Max iterations reached: one last call without tools to force a synthesis.
	final, err := l.client.Chat(ctx, ai.ChatRequest{
		Model:    l.model,
		Messages: append(messages, ai.Message{Role: "user", Content: "Please provide your final answer now based on the information gathered."}),
		Temperature: 0.3,
		MaxTokens:   1024,
	})
	if err != nil || len(final.Choices) == 0 {
		result.StoppedEarly = true
		result.StopReason = "max_iterations"
		result.Iterations = maxIters
		result.Answer = "I gathered partial information but ran out of reasoning steps. Please try rephrasing your question."
		return result, nil
	}
	result.Answer = final.Choices[0].Message.Content
	result.StoppedEarly = true
	result.StopReason = "max_iterations_synthesised"
	result.Iterations = maxIters
	return result, nil
}

// TraceJSON serialises the trace slice for storage on ChatMessage.metadata.
func (r *Result) TraceJSON() string {
	b, err := json.Marshal(r.Trace)
	if err != nil {
		return "[]"
	}
	return string(b)
}
