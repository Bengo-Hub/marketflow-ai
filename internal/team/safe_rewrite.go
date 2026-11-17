package team

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// Safe rewrite for team drafts (owner, 2026-11-14, the chat's rewrite extended to scripts,
// captions and posts): a draft that breaks the business type's compliance rules or the brand's
// banned claims is written once more with the broken rules spelled out. The rewrite is kept only
// when it breaks none of them; otherwise the first draft goes on to the review, which blocks it
// with its findings as before. One retry at most, metered like any other call of the turn.

// stepViolations lists the rules a step's drafts break: the business type's blocks and the brand's
// banned claims, with the words that broke them. The marketing laws are not here: they are advice
// (owner, 2026-11-17), reported by the review and never a reason to rewrite.
func (r *Runtime) stepViolations(st *Step, tc TenantContext) []string {
	if st.Error != "" || r.filter == nil {
		return nil
	}
	var texts []string
	if outward[st.Contract] {
		for _, it := range reviewItems(st) {
			texts = append(texts, it.text)
		}
	} else if st.Text != "" {
		texts = append(texts, st.Text)
	}
	banned := BannedClaims(tc.MarketingContext)
	seen := map[string]bool{}
	var out []string
	for _, t := range texts {
		for _, v := range r.filter.Violations(t, tc.BusinessType) {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		if claim := MatchBanned(t, banned); claim != "" {
			v := "the business never makes this claim: " + claim
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// rewriteInstruction is what the retry is told about its first draft.
func rewriteInstruction(violations []string) string {
	var b strings.Builder
	b.WriteString("\n\nYOUR FIRST DRAFT BROKE THESE RULES. Write it again, keeping its idea and format, without breaking any of them:\n")
	for _, v := range violations {
		b.WriteString("- ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	b.WriteString("Use only claims the facts support. Never invent results, cures, guarantees, prices, reviews, deadlines, stock levels or earlier prices.")
	return b.String()
}

// runStepSafe runs a step and, when its drafts break the rules and there is time, once more with
// the broken rules spelled out; the clean rewrite replaces the first draft.
func (r *Runtime) runStepSafe(ctx context.Context, tenantID uuid.UUID, st plannedStep, spec *promptopt.TaskSpec, tc TenantContext, earlier string, n int) Step {
	first := r.runStep(ctx, tenantID, st, spec, tc, earlier, n)
	violations := r.stepViolations(&first, tc)
	if len(violations) == 0 || timeLeft(ctx) < minStepTime {
		return first
	}
	retry := st
	retry.task = st.task + rewriteInstruction(violations)
	second := r.runStep(ctx, tenantID, retry, spec, tc, earlier, n)
	if second.Error != "" || len(r.stepViolations(&second, tc)) > 0 {
		return first
	}
	second.Task = st.task // the step reads as asked, not with the rewrite note
	return second
}
