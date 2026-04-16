package compliance

import (
	"strings"
	"testing"
)

// bannedVerbs are FDA-prohibited medical claim verbs. They must NEVER appear
// in filter output (the filter may choose to either block the full message
// or substitute an approved verb like "supports" / "promotes" / "helps
// maintain" — either way, the output is safe).
var bannedVerbs = []string{"cures", "treats", "heals"}

// TestFilter_NeutralisesMedicalClaims asserts that any FDA-prohibited medical
// claim is either blocked outright or has its banned verb substituted before
// the message reaches a lead. Protects every FBO tenant through the
// agentic RAG's compliance gate regardless of which tool produced the
// text (kb_search / web_search / book_meeting / LLM direct).
func TestFilter_NeutralisesMedicalClaims(t *testing.T) {
	f := NewFilter()
	cases := []string{
		"Aloe vera cures diabetes in 30 days.",
		"Forever Argi+ treats cancer effectively.",
		"Our aloe heals all wounds overnight.",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			out, blocked, _ := f.Check(in)
			if blocked {
				return // hard-blocked — safe
			}
			lower := strings.ToLower(out)
			for _, v := range bannedVerbs {
				// Match whole-word boundary so "heart" isn't caught by a substring of "heals".
				if strings.Contains(" "+lower+" ", " "+v+" ") {
					t.Errorf("banned verb %q leaked into filter output: %q", v, out)
				}
			}
		})
	}
}

// TestFilter_Preserves_ApprovedLanguage confirms the FLP-approved vocabulary
// survives a filter round-trip.
func TestFilter_Preserves_ApprovedLanguage(t *testing.T) {
	f := NewFilter()
	msg := "Forever Aloe Vera Gel (product #015, CC 2.054) supports digestive wellness. " +
		"Results vary by individual. These statements have not been evaluated by the FDA."
	out, blocked, _ := f.Check(msg)
	if blocked {
		t.Fatalf("approved copy was blocked: %q", out)
	}
	if !strings.Contains(out, "Results vary") {
		t.Errorf("compliance filter stripped required 'Results vary' disclaimer: %q", out)
	}
}
