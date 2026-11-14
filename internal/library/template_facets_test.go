package library

import (
	"strings"
	"testing"
)

// Every starter carries the facets the gallery filters on, taken from the data.
func TestTemplateFacets(t *testing.T) {
	lib := MustLoad()
	all := lib.Templates("", "", 100)
	byTitle := map[string]Template{}
	for i, tpl := range all {
		if tpl.Rank != i+1 {
			t.Fatalf("rank %d at position %d", tpl.Rank, i+1)
		}
		if tpl.Specialist == "" || tpl.Goal == "" || tpl.Output == "" || tpl.Language == "" || tpl.Added == "" {
			t.Errorf("%q lacks facets: %+v", tpl.Title, tpl)
		}
		byTitle[tpl.Title] = tpl
	}
	ads := byTitle["Ads for my product this week"]
	if ads.Specialist != "ads_specialist" || ads.Goal != "ads" || ads.Output != "copy" || ads.Language != "en" ||
		ads.Tier != "poster" || strings.Join(ads.Inputs, ",") != "offering,platforms,budget" {
		t.Fatalf("ads starter = %+v", ads)
	}
	if sw := byTitle["Tangazo la wiki hii"]; sw.Language != "sw" {
		t.Fatalf("swahili starter language = %q", sw.Language)
	}
	if v := byTitle["Video ya mtu akionyesha bidhaa"]; v.Output != "video" || strings.Join(v.Channels, ",") != "tiktok" {
		t.Fatalf("ugc swahili starter = %+v", v)
	}
	if c := byTitle["Why can't people find me on Google?"]; strings.Join(c.Channels, ",") != "google" || c.Goal != "seo" {
		t.Fatalf("seo starter = %+v", c)
	}
	if w := byTitle["WhatsApp broadcast"]; strings.Join(w.Channels, ",") != "whatsapp" || w.Tier != "" {
		t.Fatalf("whatsapp starter = %+v", w)
	}
	if r := byTitle["Is my campaign working?"]; r.Output != "report" {
		t.Fatalf("review starter = %+v", r)
	}

	f := lib.Facets()
	if len(f.Goals) == 0 || len(f.Outputs) == 0 || len(f.Languages) != 2 || len(f.Channels) == 0 || len(f.Inputs) != len(Inputs) {
		t.Fatalf("facets = %+v", f)
	}
	if len(f.Specialists) != len(lib.Members()) || f.Specialists[0].Key != "manager" || f.Specialists[0].Label == "" {
		t.Fatalf("specialists = %+v", f.Specialists)
	}
	// Every facet a starter uses has a label, so the gallery never shows a raw key.
	for _, tpl := range all {
		for _, check := range []struct {
			list []FacetOption
			keys []string
		}{
			{f.Goals, []string{tpl.Goal}}, {f.Outputs, []string{tpl.Output}}, {f.Languages, []string{tpl.Language}},
			{f.Channels, tpl.Channels}, {f.Inputs, tpl.Inputs}, {f.Specialists, []string{tpl.Specialist}},
		} {
			for _, k := range check.keys {
				if !hasOption(check.list, k) {
					t.Errorf("%q uses unlabelled facet %q", tpl.Title, k)
				}
			}
		}
	}
	// Curated prompts carry no facets.
	for _, tpl := range lib.Templates("sales", "", 100) {
		if tpl.Source != "marketflow" && (tpl.Goal != "" || tpl.Specialist != "") {
			t.Fatalf("a curated prompt got facets: %+v", tpl)
		}
	}
}
