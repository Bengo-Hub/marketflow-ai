package tools

import (
	"strings"
	"testing"
)

// The email writer serves every business: no Forever Living persona, the marketing laws, a hook
// first and one call to action (2026-11-17).
func TestDraftEmailPromptCarriesMarketingLaws(t *testing.T) {
	p := draftEmailPrompt("friendly", "Wanjiku", "follow up after the salon visit", `{"name":"Wanjiku","phone":"0712000000"}`)
	for _, want := range []string{"MARKETING LAWS.", "Urgency and scarcity:", "one call to action", "first sentence is the hook", "Never promise a price"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
	for _, bad := range []string{"Forever Living", "FBO", "0712000000"} {
		if strings.Contains(p, bad) {
			t.Errorf("the prompt still carries %q", bad)
		}
	}
}
