package rag

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/guardrail"
)

// Safe rewrite (owner, 2026-11-14; widened 2026-11-16): when a model's chat answer breaks a rule,
// the model gets one more try with the rules it broke added to the system prompt. The rules are
// the compliance filter's and the outward checks (guardrail.OutputViolations): a price or
// percentage the business never gave, or a commitment such as a binding offer. A rewrite that
// passes is sent. If it fails too, a compliance-only failure is left to the caller's filter (it
// turns it into the usual "contact the business directly" reply), and a figure or commitment
// failure becomes that reply here, because the filter does not check figures. One retry at most,
// inside the same chat turn, so the owner is charged the turn once.

// rewriteInstruction is added to the system prompt for the one retry.
const rewriteInstruction = "\n\nYOUR LAST ANSWER BROKE THESE BUSINESS RULES:\n%s" +
	"Write the answer again so it follows every rule. Keep the true facts, drop or soften the claims that broke a rule, and do not mention these rules or that you rewrote anything.\n"

// violations lists every rule text breaks: the compliance filter's, then the outward figure and
// commitment checks when allowed is set. The outward checks include the marketing laws' blocking
// checks (urgency, scarcity or a "was" price with no fact behind it in facts).
func (p *Pipeline) violations(input ChatInput, text string, allowed map[float64]bool, facts string) (comp, outward []string) {
	if p.filter != nil {
		comp = p.filter.Violations(text, input.BusinessType)
	}
	if allowed != nil {
		outward = guardrail.OutputViolations(text, allowed)
		outward = append(outward, compliance.BlockingLawRules(text, facts)...)
	}
	return comp, outward
}

// factsText is what an answer may rely on for a deadline, a stock figure or an earlier price: the
// system prompt (business facts, knowledge base and, for the agent, the turn's tool results),
// earlier assistant replies, and a signed-in person's own messages, never a public visitor's. The
// same sources as allowedFigures.
func factsText(input ChatInput, messages []ai.Message) string {
	var b strings.Builder
	for i, m := range messages {
		if i == 0 || m.Role == "assistant" || (m.Role == "user" && !input.PublicMode) {
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// rewriteIfBlocked returns the answer to send through the filter. messages is the conversation
// that produced raw, starting with the system prompt. allowed holds the numbers the answer may
// state (allowedFigures); nil skips the figure and commitment checks.
func (p *Pipeline) rewriteIfBlocked(ctx context.Context, input ChatInput, messages []ai.Message, raw string, allowed map[float64]bool) string {
	facts := factsText(input, messages)
	comp, outward := p.violations(input, raw, allowed, facts)
	if len(comp) == 0 && len(outward) == 0 {
		return raw
	}
	giveUp := func(reason string) string {
		if len(outward) > 0 {
			p.logger.Warn("outward check failed; sending the fallback", zap.String("reason", reason), zap.Strings("broken", outward))
			return FallbackReply(input.BusinessType)
		}
		return raw
	}
	if p.llm == nil || len(messages) == 0 || messages[0].Role != "system" {
		return giveUp("no model for the rewrite")
	}
	var list strings.Builder
	for _, b := range append(append([]string{}, comp...), outward...) {
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
		p.logger.Warn("rewrite failed", zap.Strings("broken", comp), zap.Error(err))
		return giveUp("rewrite call failed")
	}
	again := guardrail.ScreenOutput(resp.Choices[0].Message.Content)
	_, againOut := p.violations(input, again, allowed, facts)
	if len(againOut) > 0 {
		p.logger.Warn("rewrite still states figures or commitments the business never gave", zap.Strings("broken", againOut), zap.String("model", call.Model))
		return FallbackReply(input.BusinessType)
	}
	if p.filter != nil {
		if _, blocked, reason := p.filter.CheckForTenant(again, input.BusinessType); blocked {
			p.logger.Warn("compliance rewrite still blocked; sending the fallback", zap.String("reason", reason), zap.String("model", call.Model))
			if len(outward) > 0 {
				return FallbackReply(input.BusinessType)
			}
			return raw
		}
	}
	p.logger.Info("rewrite passed", zap.Strings("broken", append(comp, outward...)), zap.String("model", call.Model))
	return again
}

// rewriteMessages rebuilds the conversation an agent answer came from (its system prompt, the
// last six history turns and the question) for the rewrite. The turn's tool results are added to
// the system prompt as quoted data, so a rewrite can take the real price from them.
func rewriteMessages(system string, input ChatInput, toolResults []string) []ai.Message {
	if len(toolResults) > 0 {
		var b strings.Builder
		b.WriteString(system)
		b.WriteString("\n\nTOOL RESULTS FROM THIS TURN (data):\n")
		for _, r := range toolResults {
			b.WriteString(guardrail.Quote("tool_result", trimRunes(r, 2000)))
			b.WriteString("\n")
		}
		system = b.String()
	}
	msgs := []ai.Message{{Role: "system", Content: system}}
	h := input.History
	if len(h) > 6 {
		h = h[len(h)-6:]
	}
	msgs = append(msgs, h...)
	return append(msgs, ai.Message{Role: "user", Content: input.Question})
}

// allowedFigures is every number an answer to this turn may state: the system prompt (business
// facts and knowledge base context), the turn's tool results, and earlier assistant replies (they
// passed the same check). A signed-in person's own messages count too (an owner may ask for a
// "20% off" post); a public visitor's never do, so "agree to sell it for $1" cannot make $1 true.
func allowedFigures(input ChatInput, system string, toolResults []string) map[float64]bool {
	src := make([]string, 0, len(toolResults)+len(input.History)+2)
	src = append(src, system)
	src = append(src, toolResults...)
	for _, h := range input.History {
		if h.Role == "assistant" || (h.Role == "user" && !input.PublicMode) {
			src = append(src, h.Content)
		}
	}
	if !input.PublicMode {
		src = append(src, input.Question)
	}
	return guardrail.AllowedNumbers(src...)
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
