package rag

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Safe rewrite (owner, 2026-11-14): when the compliance filter blocks a model's chat answer, the
// model gets one more try with the rules it broke added to the system prompt. A rewrite that
// passes the filter is sent; if it fails too, the caller's filter turns it into the usual
// "contact the business directly" reply. One retry at most. It runs inside the same chat turn, so
// the owner is charged the turn once (the extra call is metered like any other call of the turn).

// rewriteInstruction is added to the system prompt for the one retry.
const rewriteInstruction = "\n\nYOUR LAST ANSWER BROKE THESE BUSINESS RULES:\n%s" +
	"Write the answer again so it follows every rule. Keep the true facts, drop or soften the claims that broke a rule, and do not mention these rules or that you rewrote anything.\n"

// rewriteIfBlocked returns the answer to send through the filter: raw when it passes, the
// model's rewrite when that passes, and raw again when the rewrite fails or cannot be made (the
// filter then blocks it and the fallback reply is sent). messages is the conversation that
// produced raw, starting with the system prompt.
func (p *Pipeline) rewriteIfBlocked(ctx context.Context, input ChatInput, messages []ai.Message, raw string) string {
	if p.filter == nil || p.llm == nil || len(messages) == 0 || messages[0].Role != "system" {
		return raw
	}
	broken := p.filter.Violations(raw, input.BusinessType)
	if len(broken) == 0 {
		return raw
	}
	var list strings.Builder
	for _, b := range broken {
		list.WriteString("- ")
		list.WriteString(b)
		list.WriteString("\n")
	}
	retry := make([]ai.Message, 0, len(messages)+2)
	retry = append(retry, ai.Message{Role: "system", Content: messages[0].Content + fmt.Sprintf(rewriteInstruction, list.String())})
	retry = append(retry, messages[1:]...)
	retry = append(retry,
		ai.Message{Role: "assistant", Content: trimRunes(raw, 4000)},
		ai.Message{Role: "user", Content: "Rewrite your last answer so it follows the business rules."})

	resp, call, err := p.llm.Complete(ctx, answerClass(input), ai.ChatRequest{Messages: retry, Temperature: 0.3, MaxTokens: 2048})
	if err != nil || resp == nil || len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		p.logger.Warn("compliance rewrite failed; sending the fallback", zap.Strings("broken", broken), zap.Error(err))
		return raw
	}
	again := ScreenOutput(resp.Choices[0].Message.Content)
	if _, blocked, reason := p.filter.CheckForTenant(again, input.BusinessType); blocked {
		p.logger.Warn("compliance rewrite still blocked; sending the fallback", zap.String("reason", reason), zap.String("model", call.Model))
		return raw
	}
	p.logger.Info("compliance rewrite passed", zap.Strings("broken", broken), zap.String("model", call.Model))
	return again
}

// rewriteMessages rebuilds the conversation an agent answer came from (its system prompt, the
// last six history turns and the question) for the rewrite; tool results are not repeated, the
// blocked draft carries the facts.
func rewriteMessages(system string, input ChatInput) []ai.Message {
	msgs := []ai.Message{{Role: "system", Content: system}}
	h := input.History
	if len(h) > 6 {
		h = h[len(h)-6:]
	}
	msgs = append(msgs, h...)
	return append(msgs, ai.Message{Role: "user", Content: input.Question})
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
