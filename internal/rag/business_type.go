package rag

import (
	"strings"

	"github.com/bengobox/marketflow-ai/internal/compliance"
)

// promptPersona picks the chat persona for a business type. "flp", "cafe" (auto-detected or a
// cafe widget) and "generic" keep their own personas. The other MarketFlow business types
// (restaurant, retail, salon, pharmacy, hospitality and the rest, Sprint 7 #32) share the neutral
// business persona; their own rules come from compliance.PromptRules. "", "codevertex" and
// anything unknown keep the legacy Codevertex persona, as before.
func promptPersona(businessType string) string {
	switch businessType {
	case "flp", "cafe", "generic":
		return businessType
	}
	if compliance.Known(businessType) {
		if compliance.Normalize(businessType) == compliance.TypeFLP {
			return "flp"
		}
		return "generic"
	}
	return "codevertex"
}

// withTypeRules appends the business type's rules to a persona prompt built without knowledge
// base context, then the context, so the rules sit with the persona and the facts come last.
func withTypeRules(persona, businessType string, chunks []SearchResult) string {
	var b strings.Builder
	b.WriteString(persona)
	b.WriteString(compliance.PromptRules(businessType))
	appendKBContext(&b, chunks)
	return b.String()
}
