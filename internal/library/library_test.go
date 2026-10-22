package library

import (
	"strings"
	"testing"
)

func TestLoadValidatesEmbeddedData(t *testing.T) {
	lib, err := Load()
	if err != nil {
		t.Fatalf("embedded library invalid: %v", err)
	}
	if n := len(lib.Playbooks()); n < 25 {
		t.Fatalf("only %d playbooks", n)
	}
	if len(lib.Members()) != 10 || lib.Members()[0].Key != "manager" {
		t.Fatalf("team = %+v", lib.Members())
	}
	if len(lib.Prompts()) < 100 {
		t.Fatalf("only %d curated prompts", len(lib.Prompts()))
	}
	for _, p := range lib.Prompts() {
		if p.License != "CC0-1.0" {
			t.Fatalf("prompt %s has licence %q", p.ID, p.License)
		}
	}
	// Every member's playbook must point back to that member, so routing by
	// playbook always lands on a member that lists it.
	for _, m := range lib.Members() {
		for _, key := range m.Playbooks {
			pb, _ := lib.Playbook(key)
			if pb.Specialist != m.Key && key != "offers" {
				t.Errorf("member %s lists %s owned by %s", m.Key, key, pb.Specialist)
			}
		}
	}
}

func TestNoAITellsInLibraryText(t *testing.T) {
	lib := MustLoad()
	check := func(where, s string) {
		for _, bad := range []string{"—", "§", "→"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q: %s", where, bad, s)
			}
		}
	}
	for _, r := range lib.HouseRules {
		check("house rule", r)
	}
	for _, p := range lib.Playbooks() {
		check(p.Key, p.WhenToUse+strings.Join(p.Method, " ")+strings.Join(p.Rules, " "))
		for _, s := range p.Starters {
			check(p.Key+" starter", s.Template)
		}
	}
}

func TestSearchPlaybooks(t *testing.T) {
	lib := MustLoad()
	cases := map[string]string{
		"nataka tangazo la course yangu ya Python wiki hii, budget 3k": "ad-creative",
		"what should I post on instagram this week":                    "content-strategy",
		"make a poster for my salon":                                   "poster-brief",
		"why are my leads down this week":                              "attribution-analytics",
		"write a whatsapp broadcast for my customers":                  "whatsapp-sms",
		"nataka video ya mtu akionyesha lotion yetu kama za tiktok":    "ugc-video",
		"I want a UGC talking head video for my shoes":                 "ugc-video",
	}
	for q, want := range cases {
		got := lib.SearchPlaybooks(q, 3)
		if len(got) == 0 || got[0].Key != want {
			t.Errorf("%q: top = %+v, want %s", q, got, want)
		}
	}
}

func TestRenderAndBrief(t *testing.T) {
	got := Render("Ads for ${offering} on ${platforms}, budget ${budget}.", map[string]string{"offering": "Python course", "budget": "KES 3,000"})
	if got != "Ads for Python course on [platforms], budget KES 3,000." {
		t.Fatalf("render = %q", got)
	}
	lib := MustLoad()
	pb, _ := lib.Playbook("ad-creative")
	brief := lib.Brief(pb)
	if !strings.Contains(brief, "1. Pick 3 to 5 distinct angles") || !strings.Contains(lib.HouseRulesText(), "HOUSE RULES") {
		t.Fatalf("brief = %s", brief)
	}
}

func TestTemplates(t *testing.T) {
	lib := MustLoad()
	all := lib.Templates("", "", 100)
	if len(all) < 20 {
		t.Fatalf("only %d starters", len(all))
	}
	ads := lib.Templates("ads", "", 50)
	found := false
	for _, tp := range ads {
		if tp.Playbook == "ad-creative" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ads query found no ad-creative starter: %+v", ads)
	}
}
