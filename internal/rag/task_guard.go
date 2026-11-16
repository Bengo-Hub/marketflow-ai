package rag

import (
	"github.com/bengobox/marketflow-ai/internal/guardrail"
	"go.uber.org/zap"
)

// guardTaskContent applies the chat output rules to a content task's result (ad copy, posts,
// emails), field by field: the leak and persona screen, the tenant's compliance filter and the
// image allowlist. A field the filter blocks is emptied and the reason is recorded under
// "_compliance", so the owner sees why a field is missing instead of receiving a blocked claim.
// Content tasks run only for signed-in people (flow.go).
func (p *Pipeline) guardTaskContent(content map[string]any, businessType string) {
	if content == nil {
		return
	}
	var reasons []string
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			s := guardrail.SafeMarkdown(guardrail.ScreenOutput(t), nil)
			if p.filter == nil {
				return s
			}
			filtered, blocked, reason := p.filter.CheckForTenant(s, businessType)
			if blocked {
				reasons = append(reasons, reason)
				return ""
			}
			return filtered
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
}
