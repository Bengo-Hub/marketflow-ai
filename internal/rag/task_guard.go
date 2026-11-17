package rag

import (
	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/guardrail"
	"go.uber.org/zap"
)

// guardTaskContent applies the chat output rules to a content task's result (ad copy, posts,
// emails), field by field: the leak and persona screen, the tenant's compliance filter and the
// image allowlist. A field the filter blocks is emptied and the reason is recorded under
// "_compliance", so the owner sees why a field is missing instead of receiving a blocked claim.
// Content tasks run only for signed-in people (flow.go).
//
// The marketing laws' checks run on each field too, as advice (owner, 2026-11-17): a field that
// uses urgency, scarcity, a "was" price or an unproven "best" with no fact behind it in facts (the
// knowledge base context of the task and the owner's own request) is kept as written, and the
// note is recorded under "_review" so the owner can check it before sending.
func (p *Pipeline) guardTaskContent(content map[string]any, businessType, facts string) {
	if content == nil {
		return
	}
	var reasons, review []string
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			s := guardrail.SafeMarkdown(guardrail.ScreenOutput(t), nil)
			if p.filter != nil {
				filtered, blocked, reason := p.filter.CheckForTenant(s, businessType)
				if blocked {
					reasons = append(reasons, reason)
					return ""
				}
				s = filtered
			}
			review = append(review, compliance.LawNotes(s, facts)...)
			return s
		case map[string]any:
			for k, inner := range t {
				t[k] = walk(inner)
			}
			return t
		case []any:
			for i, inner := range t {
				t[i] = walk(inner)
			}
			return t
		default:
			return v
		}
	}
	for k, v := range content {
		content[k] = walk(v)
	}
	if len(reasons) > 0 {
		content["_compliance"] = reasons
		p.logger.Warn("task content fields blocked by compliance", zap.Strings("reasons", reasons))
	}
	if len(review) > 0 {
		content["_review"] = review
	}
}
