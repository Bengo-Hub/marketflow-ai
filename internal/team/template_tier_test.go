package team

import (
	"testing"

	"github.com/bengobox/marketflow-ai/internal/library"
)

// The template gallery prices a media template from its facets' tier; it must be the tier the
// Creative Plan quotes for that playbook, and text playbooks have none.
func TestTemplateFacetTiersMatchMediaPlaybooks(t *testing.T) {
	lib := library.MustLoad()
	for key, pf := range lib.Facets().Playbooks {
		want := mediaPlaybooks[key]
		if pf.Tier != want {
			t.Errorf("playbook %s: facets tier %q, Creative Plan tier %q", key, pf.Tier, want)
		}
	}
}
