package library

import (
	"strings"
	"testing"
)

func TestAdTemplatesLoad(t *testing.T) {
	lib := MustLoad()
	want := []string{"educate", "day-in-the-life", "routine", "problem-agitate-solve", "before-after",
		"myth-fact", "founder-story", "testimonial", "comparison", "unboxing-demo"}
	got := lib.AdTemplates()
	if len(got) != len(want) {
		t.Fatalf("%d templates, want %d", len(got), len(want))
	}
	for i, key := range want {
		if got[i].Key != key {
			t.Errorf("template %d = %q, want %q", i, got[i].Key, key)
		}
	}
	for _, tpl := range got {
		text := tpl.Name + tpl.WhenToUse + tpl.Hook + tpl.CTA + strings.Join(tpl.Beats, " ") + strings.Join(tpl.Honesty, " ")
		for _, bad := range []string{"—", " - "} {
			if strings.Contains(text, bad) {
				t.Errorf("%s uses %q as punctuation", tpl.Key, bad)
			}
		}
	}
	if tpl, _ := lib.AdTemplate("testimonial"); !contains(tpl.Requires, NeedRealReviews) {
		t.Fatal("the testimonial template must require real reviews")
	}
	if len(lib.VoiceTags) == 0 || !lib.AllowedVoiceTag("Excited") || lib.AllowedVoiceTag("shouting") {
		t.Fatalf("voice tags = %v", lib.VoiceTags)
	}
	if lib.Quality.HookMaxChars <= 0 || len(lib.Quality.WeakCTAs) == 0 {
		t.Fatalf("quality = %+v", lib.Quality)
	}
	// The honesty rules for persuasion are part of every specialist's prompt.
	rules := lib.HouseRulesText()
	for _, want := range []string{"Persuade honestly", "social proof only from real reviews", "never hide a price",
		"Fear, shame and loss threats", "never claims to be a customer"} {
		if !strings.Contains(rules, want) {
			t.Errorf("house rules lack %q", want)
		}
	}
}

func TestSelectAdTemplate(t *testing.T) {
	lib := MustLoad()
	cases := []struct {
		name     string
		c        AdCriteria
		want     string
		wantNote bool
	}{
		{"asked for and usable", AdCriteria{Key: "myth-fact", Kind: AdKindPresenter}, "myth-fact", false},
		{"testimonial without real reviews falls back", AdCriteria{Key: "testimonial", Kind: AdKindUGC}, "day-in-the-life", true},
		{"testimonial with real reviews", AdCriteria{Key: "testimonial", Kind: AdKindUGC, Has: map[string]bool{NeedRealReviews: true}}, "testimonial", false},
		{"founder story needs the story", AdCriteria{Key: "founder-story", Kind: AdKindTemplate}, "educate", true},
		{"routine does not suit a template ad", AdCriteria{Key: "routine", Kind: AdKindTemplate, Text: "show a before and after of the shop"}, "before-after", true},
		{"unknown key", AdCriteria{Key: "viral-dance", Kind: AdKindPresenter}, "problem-agitate-solve", true},
		{"keywords pick myth versus fact", AdCriteria{Text: "people think a POS is too expensive, show the truth", Kind: AdKindTemplate}, "myth-fact", false},
		{"phrase beats a single word", AdCriteria{Text: "a day in the life of a duka owner, with tips", Kind: AdKindUGC}, "day-in-the-life", false},
		{"swahili keywords", AdCriteria{Text: "video ya kufungua na kuonyesha inavyofanya kazi", Kind: AdKindUGC}, "unboxing-demo", false},
		{"review keyword ignored without reviews", AdCriteria{Text: "customer review video", Kind: AdKindUGC}, "day-in-the-life", false},
		{"default for ugc", AdCriteria{Kind: AdKindUGC}, "day-in-the-life", false},
		{"default for presenter", AdCriteria{Kind: AdKindPresenter}, "problem-agitate-solve", false},
		{"unknown kind counts as template", AdCriteria{Kind: "cinematic"}, "educate", false},
	}
	for _, tc := range cases {
		got, note := lib.SelectAdTemplate(tc.c)
		if got.Key != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got.Key, tc.want)
		}
		if (note != "") != tc.wantNote {
			t.Errorf("%s: note = %q", tc.name, note)
		}
	}
}

func TestMatchAdTemplateAndBrief(t *testing.T) {
	lib := MustLoad()
	if _, ok := lib.MatchAdTemplate("make a video ad for my shop"); ok {
		t.Fatal("a request with no approach words matched a template")
	}
	tpl, ok := lib.MatchAdTemplate("Testimonial video from our customer reviews")
	if !ok || tpl.Key != "testimonial" {
		t.Fatalf("match = %q, %v", tpl.Key, ok)
	}
	brief := lib.AdTemplateBrief(tpl)
	for _, want := range []string{`"testimonial"`, "Hook, inside the first 2 seconds", "Needs: real reviews", "never invent"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if !strings.Contains(lib.VoiceTagsText(), "[warm]") {
		t.Fatalf("voice tags text = %q", lib.VoiceTagsText())
	}
}
