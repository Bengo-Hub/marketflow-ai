package rag

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Business facts (marketflow-api marketingos/business_facts.go): what Vera may state as true about
// the business. With them comes one rule for every business type: an address, opening hours, a phone
// number or a price is stated only from these facts or the knowledge base, never guessed. Urban
// Loft's assistant had no facts and told a visitor the cafe was "downtown, near the train station";
// it is in Busia.

// noGuessRule closes every prompt, with or without facts.
const noGuessRule = `
FACTS YOU MAY STATE — STRICT:
Only state an address, location, directions, opening hours, phone number, email, price or availability if it appears in BUSINESS FACTS below or in the knowledge base context. If the visitor asks for one that is not there, say plainly that you do not have that detail and give the contact details from BUSINESS FACTS (or suggest the website). Never guess, estimate or describe a location you were not given.
`

// withFacts adds the business facts and the rule to a system prompt.
func (p *Pipeline) withFacts(ctx context.Context, tenantID uuid.UUID, prompt string) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n")
	b.WriteString(noGuessRule)
	if p.facts != nil && tenantID != uuid.Nil {
		fctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		f := p.facts(fctx, tenantID)
		cancel()
		if strings.TrimSpace(f) != "" {
			b.WriteString("\nBUSINESS FACTS (true, from the business's own records and website):\n")
			b.WriteString(f)
		}
	}
	return b.String()
}
