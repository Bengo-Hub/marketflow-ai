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

// TestCheckGeneric_BlocksMisleadingPromises covers the rules every non-FLP tenant gets.
func TestCheckGeneric_BlocksMisleadingPromises(t *testing.T) {
	f := NewFilter()
	blocked := []string{
		"This tea cures headaches.",
		"A miracle offer for you.",
		"Guaranteed income every week.",
		"Get rich with our course.",
		"Easy money, start today.",
		"Make money fast from home.",
		"Invest now for guaranteed returns.",
		"Enjoy guaranteed profits monthly.",
		"A risk-free investment in our scheme.",
		"Risk free returns of 20 percent.",
		"Double your money in 30 days.",
	}
	for _, in := range blocked {
		t.Run(in, func(t *testing.T) {
			if _, isBlocked, _ := f.CheckGeneric(in); !isBlocked {
				t.Fatalf("expected %q to be blocked", in)
			}
		})
	}
}

// TestCheckGeneric_AllowsEverydayCopy guards against false positives that would break ordinary
// business copy for cafes, shops, schools and service businesses.
func TestCheckGeneric_AllowsEverydayCopy(t *testing.T) {
	f := NewFilter()
	allowed := []string{
		"Treat yourself to our new pastries.",
		"Satisfaction guaranteed or your money back.",
		"Our POS keeps your sales safe even offline.",
		"Learn Python in our 8-week course.",
		"Healthy lunch bowls from KES 450.",
		"Your guaranteed delivery slot is confirmed.",
	}
	for _, in := range allowed {
		t.Run(in, func(t *testing.T) {
			out, isBlocked, reason := f.CheckGeneric(in)
			if isBlocked {
				t.Fatalf("everyday copy %q was blocked (%s)", in, reason)
			}
			if out != in {
				t.Fatalf("generic filter must not rewrite copy: %q became %q", in, out)
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
