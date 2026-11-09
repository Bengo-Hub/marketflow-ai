package team

import (
	"strings"
	"testing"
)

func TestRenderFactsListsOnlyWhatIsKnown(t *testing.T) {
	got := renderFacts(businessFacts{Name: "Urban Loft", Phone: "+254116010638", Summary: "Cafe in Busia", Hours: []string{"Mon 07:30 to 22:00"}})
	for _, want := range []string{"- Business name: Urban Loft", "- Phone: +254116010638", "- Opening hours: Mon 07:30 to 22:00", "Cafe in Busia"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "Address") || strings.Contains(got, "Email") {
		t.Errorf("an empty fact was listed: %q", got)
	}
}
