package compliance

import (
	"strings"
	"testing"
)

// The laws reach every business type's prompt, once, and the FLP block alone.
func TestMarketingLawsInEveryPrompt(t *testing.T) {
	laws := MarketingLaws()
	if !strings.HasPrefix(laws, "\n") {
		t.Fatal("the laws block must start on a new line so it can be appended")
	}
	for _, want := range []string{"MARKETING LAWS", "Attention:", "Relevance:", "Emotion:", "Friction:", "Urgency and scarcity:",
		"Identity:", "Contrast:", "Repetition:", "Association:", "Perceived value:", "Loss aversion:"} {
		if !strings.Contains(laws, want) {
			t.Fatalf("the laws block lacks %q:\n%s", want, laws)
		}
	}
	// No em dash (U+2014) and no section sign (U+00A7), checked by code point.
	for _, bad := range []rune{0x2014, 0xA7} {
		if strings.ContainsRune(laws, bad) {
			t.Fatalf("the laws block uses %q", bad)
		}
	}
	for _, ti := range Types() {
		p := PromptRules(ti.Key)
		if strings.Count(p, "MARKETING LAWS") != 1 {
			t.Fatalf("%s: the laws must appear exactly once in the prompt rules", ti.Key)
		}
	}
	// The guidance replaced the old baseline lines it covers, so they are not said twice.
	if strings.Contains(PromptRules("generic"), "Do not run down competitors") {
		t.Fatal("the contrast law replaced the old competitor line; it must not be repeated")
	}
}

// The laws text is part of the chat system prompt, which counts as facts there. It must never
// back a pressure claim by itself.
func TestLawGuidanceIsNotAFact(t *testing.T) {
	for _, draft := range []string{"Offer ends today, last chance!", "Only 3 left in stock.", "Was KES 1,200, now KES 900.", "Half price this week."} {
		if len(BlockingLawRules(draft, PromptRules("generic"))) == 0 {
			t.Errorf("the prompt rules alone let %q through", draft)
		}
	}
}

func TestLawFindings(t *testing.T) {
	cases := []struct {
		name, text, facts string
		wantLaw           string // "" means no finding
		wantBlock         bool
	}{
		// Urgency
		{"invented today only", "Today only: 2 for KES 1,000!", "Shea lotion KES 850", "urgency", true},
		{"last chance without a date", "Last chance to book your table.", "", "urgency", true},
		{"swahili leo tu", "Ofa hii ni leo tu!", "", "urgency", true},
		{"offer ends with a real date", "Offer ends Sunday, order now.", "Promotion code: SUN10\nOffer ends: 30 November 2026", "", false},
		{"deadline in a weekday", "Last chance: the class starts Saturday.", "Next intake starts Saturday 6 December", "", false},
		{"opening hours are not a deadline", "Hurry up, offer ends soon!", "Open until 9pm, Monday to Friday", "urgency", true},
		{"calm copy", "Fresh mandazi every morning in Kisumu.", "", "", false},
		// Scarcity
		{"invented stock", "Only 3 left, grab yours.", "Shea lotion KES 850", "scarcity", true},
		{"selling fast", "These are selling fast!", "", "scarcity", true},
		{"real stock figure", "Only 3 left at this price.", "Stock: 3 left", "", false},
		{"real seats", "Limited seats for Saturday's class.", "Capacity: 20 seats left", "", false},
		{"pieces in a pack are not stock", "Only 3 pieces left!", "Pack of 3 pieces, KES 450", "scarcity", true},
		// Contrast (was price, percent off)
		{"invented was price", "Was KES 1,200, now KES 900.", "Price: KES 900", "contrast", true},
		{"invented percent off", "Get 20% off this week.", "Price: KES 900", "contrast", true},
		{"real promotion percent", "Get 20% off with code SUN10.", "About it: 20% off. Weekend promo", "", false},
		{"real earlier price", "Was KES 1,200, now KES 900.", "Regular price KES 1,200; sale price KES 900", "", false},
		{"swahili bei ya zamani", "Bei ya zamani 1500, sasa 1000!", "", "contrast", true},
		// Superlatives are advice, never a block
		{"unproven best", "The best chapati in Kisumu.", "", "superlative", false},
		{"number one", "Kenya's No. 1 POS for dukas.", "", "superlative", false},
		{"award in the facts", "The best nyama choma in town.", "Winner, Best Nyama Choma 2025 (Kisumu Food Awards)", "", false},
		{"same phrase in the facts", "Our best-selling lotion.", "Shea lotion is our best-selling product", "", false},
	}
	for _, c := range cases {
		got := LawFindings(c.text, c.facts)
		if c.wantLaw == "" {
			if len(got) != 0 {
				t.Errorf("%s: unexpected findings %+v", c.name, got)
			}
			continue
		}
		found := false
		for _, f := range got {
			if f.Law == c.wantLaw {
				found = true
				if f.Block != c.wantBlock {
					t.Errorf("%s: block = %v, want %v", c.name, f.Block, c.wantBlock)
				}
				if f.Quote == "" || f.Fix == "" || !strings.Contains(f.Rule(), f.Quote) {
					t.Errorf("%s: finding lacks a quote or fix: %+v", c.name, f)
				}
			}
		}
		if !found {
			t.Errorf("%s: want a %s finding, got %+v", c.name, c.wantLaw, got)
		}
	}
}

func TestBlockingLawRulesSkipsAdvice(t *testing.T) {
	if r := BlockingLawRules("The best chapati in Kisumu.", ""); len(r) != 0 {
		t.Fatalf("a superlative is advice, not a blocking rule: %v", r)
	}
	if r := BlockingLawRules("Last chance, only 2 left!", ""); len(r) != 2 {
		t.Fatalf("urgency and scarcity both block: %v", r)
	}
}

func TestParseCatalogRejectsBadLawCheck(t *testing.T) {
	base := "types:\n  - {key: generic, label: G, description: d}\n  - {key: flp, label: F, description: d}\n"
	cases := map[string]string{
		"bad pattern":   "marketing_laws:\n  checks:\n    - {law: u, reason: r, fix: f, pattern: '(', supported_by: x}\n",
		"no support":    "marketing_laws:\n  checks:\n    - {law: u, reason: r, fix: f, pattern: x}\n",
		"missing fix":   "marketing_laws:\n  checks:\n    - {law: u, reason: r, pattern: x, supported_by: y}\n",
		"bad supported": "marketing_laws:\n  checks:\n    - {law: u, reason: r, fix: f, pattern: x, supported_by: '('}\n",
	}
	for name, doc := range cases {
		if _, err := parseCatalog([]byte(doc + base)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
