package tasks

import (
	"strings"
	"testing"
)

// Every task prompt carries the business type's rules and the marketing laws, and its own format
// rules: a hook first and one call to action (golden assertions, 2026-11-17).
func TestTaskPromptsCarryTheLaws(t *testing.T) {
	for taskType := range taskSystemPrompts {
		p := systemPromptFor(taskType, "restaurant", "[menu] Pilau: KES 350")
		for _, want := range []string{"FORMAT RULES", "BUSINESS RULES (Restaurant or cafe)", "MARKETING LAWS", "Urgency and scarcity:", "PRODUCT KNOWLEDGE"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: the prompt lacks %q", taskType, want)
			}
		}
		// The prompts were Forever Living only until 2026-11-17; a restaurant must not get FLP text.
		for _, bad := range []string{"Forever Living", "FDA", "#ForeverLiving", `"Learn More" or "Shop Now"`} {
			if strings.Contains(p, bad) {
				t.Errorf("%s: a restaurant's prompt still says %q", taskType, bad)
			}
		}
	}
	for _, tt := range []TaskType{TaskCreateAdCopy, TaskGenerateSocialPost, TaskDraftEmail, TaskWhatsAppBroadcast} {
		p := taskSystemPrompts[tt]
		if !strings.Contains(strings.ToLower(p), "hook") {
			t.Errorf("%s: the format rules must ask for a hook first", tt)
		}
		if !strings.Contains(strings.ToLower(p), "one call to action") {
			t.Errorf("%s: the format rules must ask for one call to action", tt)
		}
	}
	// FLP keeps its own rules through the same path.
	if p := systemPromptFor(TaskCreateAdCopy, "flp", ""); !strings.Contains(p, "Forever Living (FBO)") || !strings.Contains(p, "supports") {
		t.Fatalf("an FLP task must get the FLP rules:\n%s", p)
	}
	// An unknown task falls back to the ad prompt.
	if p := systemPromptFor("nope", "", ""); !strings.Contains(p, "Meta (Facebook/Instagram) ad") {
		t.Fatal("unknown task types use the ad prompt")
	}
}
