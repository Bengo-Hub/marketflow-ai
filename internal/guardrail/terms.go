package guardrail

import (
	"regexp"
	"strings"
)

// The marketing team's glossary (owner, 2026-11-20). The models made up "M-Pay" for M-Pesa and
// stated a "7 days" period nobody asked for. The terms come from marketflow-api (service config
// marketing.payment_methods and marketing.glossary, platform value with a tenant override), so
// nothing here names a business value: this file only states the rules and applies the fixes.

// Term is one correction from the glossary: Wrong is replaced by Right wherever it appears as a
// whole word, in any letter case.
type Term struct {
	Wrong string `json:"wrong"`
	Right string `json:"right"`
}

// TermsBlock is the prompt section every specialist and the manager read: the payment methods
// the business takes, the corrections, and the rule on assumed defaults.
func TermsBlock(paymentMethods []string, terms []Term) string {
	var b strings.Builder
	b.WriteString("\nTERMS:\n")
	if len(paymentMethods) > 0 {
		b.WriteString("- The business takes payment by: " + strings.Join(paymentMethods, ", ") + ". Name these exactly as written and no other payment method.\n")
	}
	for _, t := range terms {
		b.WriteString("- Write \"" + t.Right + "\", never \"" + t.Wrong + "\".\n")
	}
	b.WriteString("- Never invent a brand, product, offer, payment method or programme name. Use only names from the brand context, the catalogue, the knowledge base or the owner's words.\n")
	b.WriteString("- When the owner did not give a period, budget, date or quantity and you need one, choose a reasonable value and label it as an assumption the owner can change, for example \"Assumed: 7 days, change it if you like\". Never present it as something the owner asked for.\n")
	return b.String()
}

// FixTerms replaces every glossary Wrong with its Right (whole words, any case).
func FixTerms(text string, terms []Term) string {
	for _, t := range terms {
		wrong := strings.TrimSpace(t.Wrong)
		if wrong == "" || strings.TrimSpace(t.Right) == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)(^|[^\pL\pN-])` + regexp.QuoteMeta(wrong) + `($|[^\pL\pN-])`)
		if err != nil {
			continue
		}
		// Two passes: adjacent matches share a boundary character, so one pass can skip the second.
		for i := 0; i < 2; i++ {
			text = re.ReplaceAllString(text, "${1}"+strings.ReplaceAll(t.Right, "$", "$$")+"${2}")
		}
	}
	return text
}
