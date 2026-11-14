package promptopt

import (
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/library"
)

// The optimiser finds the ad template a request asks for in its own words, only for the
// outputs a template shapes, and the brief carries the template's hook, beats and CTA.
func TestApproachForAndBrief(t *testing.T) {
	lib := library.MustLoad()
	ugc, _ := lib.Playbook("ugc-video")
	social, _ := lib.Playbook("social")
	if got := approachFor(lib, ugc, "UGC video, myth vs fact about solar lamps"); got != "myth-fact" {
		t.Fatalf("ugc approach = %q", got)
	}
	if got := approachFor(lib, social, "posts busting a myth about solar lamps"); got != "" {
		t.Fatalf("a social post got approach %q", got)
	}
	if got := approachFor(lib, ugc, "a video ad for my lamps"); got != "" {
		t.Fatalf("no approach words, got %q", got)
	}
	spec := &TaskSpec{Playbook: "ugc-video", Original: "unboxing video", Approach: "unboxing-demo"}
	b := brief(lib, spec)
	for _, want := range []string{"APPROACH: Unboxing or demo", "Hook, inside the first 2 seconds", "Call to action"} {
		if !strings.Contains(b, want) {
			t.Fatalf("brief lacks %q:\n%s", want, b)
		}
	}
}
