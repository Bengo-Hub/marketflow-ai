package compliance

import (
	"fmt"
	"regexp"
	"strings"
)

// The owner's marketing laws (2026-11-17). The writer's guidance and the deterministic checks both
// live in data/business_types.yaml (marketing_laws), so every generator gets the same wording
// through PromptRules or MarketingLaws, and every reviewer applies the same checks through
// LawFindings. A law is used only in its honest form: urgency needs a real deadline, scarcity a
// real stock figure, a "was" price a real earlier price, and a "best" claim proof.
//
// Every finding is advice (owner, 2026-11-17): it is shown to the owner as a review finding and
// never blocks, empties or replaces a draft. Invented prices and percentages
// (guardrail.OutputViolations), commitments, banned claims and the compliance blocks stay blocking;
// they are checked elsewhere.

// LawCheck is one deterministic check: a draft matching Pattern needs a fact behind it.
type LawCheck struct {
	Law         string `yaml:"law"`
	Pattern     string `yaml:"pattern"`
	SupportedBy string `yaml:"supported_by"`
	// SamePhrase also accepts the facts when they contain the matched words themselves (an
	// award title such as "Best Nyama Choma 2025").
	SamePhrase bool   `yaml:"same_phrase"`
	Reason     string `yaml:"reason"`
	Fix        string `yaml:"fix"`
	re         *regexp.Regexp
	support    *regexp.Regexp
}

// LawFinding is one place a draft uses a law without the fact behind it. It is advice for the
// owner, never a block.
type LawFinding struct {
	Law    string
	Quote  string
	Reason string
	Fix    string
}

// Note is the finding as one line for the owner's review notes or a log.
func (f LawFinding) Note() string {
	return fmt.Sprintf("marketing law (%s): %s: %q", f.Law, f.Reason, f.Quote)
}

// LawNotes is the note line of every finding on text, for a review list or a log.
func LawNotes(text, facts string) []string {
	var out []string
	for _, f := range LawFindings(text, facts) {
		out = append(out, f.Note())
	}
	return out
}

func compileLawChecks(checks []LawCheck) error {
	for i := range checks {
		c := &checks[i]
		if strings.TrimSpace(c.Law) == "" || strings.TrimSpace(c.Reason) == "" || strings.TrimSpace(c.Fix) == "" {
			return fmt.Errorf("compliance: marketing law check %d needs a law, a reason and a fix", i+1)
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return fmt.Errorf("compliance: marketing law check %s: %w", c.Law, err)
		}
		c.re = re
		c.support = nil
		if strings.TrimSpace(c.SupportedBy) != "" {
			s, err := regexp.Compile(c.SupportedBy)
			if err != nil {
				return fmt.Errorf("compliance: marketing law check %s supported_by: %w", c.Law, err)
			}
			c.support = s
		}
		if c.support == nil && !c.SamePhrase {
			return fmt.Errorf("compliance: marketing law check %s can never be supported", c.Law)
		}
	}
	return nil
}

// MarketingLaws is the laws block for a writer's prompt. PromptRules already ends with it; the FLP
// personas, which carry their own compliance text instead of PromptRules, append it alone. It
// starts with a blank line so it can be appended to any prompt.
func MarketingLaws() string {
	if len(rules.lawGuidance) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nMARKETING LAWS. Use each one in its honest form only; a tactic is allowed only when the fact behind it is true:\n")
	for _, g := range rules.lawGuidance {
		b.WriteString("- ")
		b.WriteString(g)
		b.WriteString("\n")
	}
	return b.String()
}

// LawFindings lists the laws a draft uses without a fact behind it, one finding per law at most.
// facts is everything the draft may rely on: brand context, business facts, knowledge base, tool
// results and the owner's own request (never a public visitor's message).
func LawFindings(text, facts string) []LawFinding {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lowerFacts := strings.ToLower(facts)
	var out []LawFinding
	for i := range rules.lawChecks {
		c := &rules.lawChecks[i]
		m := c.re.FindString(text)
		if m == "" {
			continue
		}
		m = strings.TrimSpace(m)
		if c.support != nil && c.support.MatchString(facts) {
			continue
		}
		if c.SamePhrase && strings.Contains(lowerFacts, strings.ToLower(m)) {
			continue
		}
		out = append(out, LawFinding{Law: c.Law, Quote: m, Reason: c.Reason, Fix: c.Fix})
	}
	return out
}
