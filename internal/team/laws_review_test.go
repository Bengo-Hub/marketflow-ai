package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/decide"
	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// Golden assertions: every specialist prompt carries the marketing laws (from
// compliance.PromptRules) and the format's own specifics (from its playbook).
func TestSpecialistPromptsCarryMarketingLaws(t *testing.T) {
	cases := map[string]string{
		"ad-creative":      "one call to action on one channel",
		"poster-brief":     "outcome-first headline",
		"video-script":     "The hook lands in the first 2 seconds",
		"ugc-video":        "Hook in the first 2 to 3 seconds",
		"social":           "same core message",
		"whatsapp-sms":     "then the hook in the same first line",
		"emails":           "the first sentence is the hook",
		"content-strategy": "one core message",
		"sales-enablement": "Offer one next step",
	}
	for key, want := range cases {
		r, f := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return "ok" }, nil)
		pb, ok := r.lib.Playbook(key)
		if !ok {
			t.Fatalf("playbook %s is missing", key)
		}
		m, _ := r.member(pb.Specialist, TenantContext{})
		r.runStep(context.Background(), uuid.New(), plannedStep{member: m, playbook: pb, task: "x"}, &promptopt.TaskSpec{}, TenantContext{BusinessType: "salon"}, "", 0)
		if len(f.systems) == 0 {
			t.Fatalf("%s: no model call", key)
		}
		sys := f.systems[0]
		for _, need := range []string{"HOUSE RULES", "BUSINESS RULES (Salon, barber or beauty)", "MARKETING LAWS.", "Urgency and scarcity:", "Repetition:", want} {
			if !strings.Contains(sys, need) {
				t.Errorf("%s: the specialist prompt lacks %q", key, need)
			}
		}
		if strings.Count(sys, "MARKETING LAWS.") != 1 {
			t.Errorf("%s: the laws must be sent once", key)
		}
	}
}

// The brand profile field writer gets the laws too.
func TestFieldSuggestCarriesMarketingLaws(t *testing.T) {
	var got string
	r, _ := newRuntime(promptopt.TaskSpec{}, func(system, _ string) string { got = system; return `{"text":"Fresh cuts in Kisumu"}` }, nil)
	if _, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "tagline"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "MARKETING LAWS.") {
		t.Fatalf("the field writer's prompt lacks the laws:\n%s", got)
	}
}

func adBatch(t *testing.T, vs ...AdVariant) *Step {
	t.Helper()
	b, err := json.Marshal(AdCopyBatch{Variants: vs})
	if err != nil {
		t.Fatal(err)
	}
	return &Step{Contract: "ad_copy_batch", Playbook: "ad-creative", Output: b}
}

func findingWith(fs []Finding, quotePrefix, issue, severity string) bool {
	for _, f := range fs {
		if strings.HasPrefix(f.Quote, quotePrefix) && strings.Contains(f.Issue, issue) && (severity == "" || f.Severity == severity) {
			return true
		}
	}
	return false
}

// Every marketing law is advice (owner, 2026-11-17): pressure without a fact and an unproven
// "best" are review findings and rank the variant lower, never drop it; the format checks are low.
func TestReviewAppliesMarketingLaws(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return "" }, nil)
	variants := []AdVariant{
		{ID: "v1", Hook: "Dry skin this cold?", Headline: "Soft skin all day", PrimaryText: "Shea butter lotion, KES 850.", CTA: "Order on WhatsApp"},
		{ID: "v2", Hook: "Last chance!", Headline: "Soft skin all day", PrimaryText: "Offer ends soon.", CTA: "Order on WhatsApp"},
		{ID: "v3", Hook: "Dry skin?", Headline: "Lotion deal", PrimaryText: "Was KES 1,200, now KES 850.", CTA: "Order on WhatsApp"},
		{ID: "v4", Hook: "Dry skin?", Headline: "The best lotion in Kisumu", PrimaryText: "Shea butter, KES 850.", CTA: "Order on WhatsApp"},
		{ID: "v5", Hook: "Dry skin?", Headline: "Soft skin", PrimaryText: "Shea butter, KES 850.", CTA: "Call or WhatsApp us for delivery"},
		{ID: "v6", Hook: "", Headline: "Soft skin", PrimaryText: "", CTA: ""},
		{ID: "v7", Hook: "Dry skin?", Headline: "Soft skin", PrimaryText: "Shea butter, KES 850.", CTA: "Learn more"},
	}
	tc := TenantContext{BusinessType: "generic", MarketingContext: `{"offers":["Shea Butter Body Lotion KES 850"]}`}
	st := adBatch(t, variants...)
	shortlist, findings := r.review(context.Background(), st, &promptopt.TaskSpec{}, tc, 10)

	kept := strings.Join(shortlist, ",")
	for _, id := range []string{"v1", "v2", "v3", "v4", "v5", "v6", "v7"} {
		if !strings.Contains(kept, id) {
			t.Errorf("%s: a marketing law never drops a variant: %v", id, shortlist)
		}
	}
	var out AdCopyBatch
	if err := json.Unmarshal(st.Output, &out); err != nil || len(out.Variants) != len(variants) || st.Error != "" {
		t.Fatalf("every variant stays in the output: %d of %d, error %q", len(out.Variants), len(variants), st.Error)
	}
	for _, f := range findings {
		if strings.HasPrefix(f.Issue, "marketing law (") && f.Severity != "review" {
			t.Errorf("a marketing law finding is review, never %q: %+v", f.Severity, f)
		}
	}
	checks := []struct{ quote, issue, sev string }{
		{"v2", "marketing law (urgency)", "review"},
		{"v3", "marketing law (contrast)", "review"},
		{"v4", "marketing law (superlative)", "review"},
		{"v5", "more than one next step", "low"},
		{"v6", "no hook", "low"},
		{"v6", "no call to action", "low"},
		{"v7", "generic", "low"},
	}
	for _, c := range checks {
		if !findingWith(findings, c.quote, c.issue, c.sev) {
			t.Errorf("missing %s finding %q (%s): %+v", c.quote, c.issue, c.sev, findings)
		}
	}
	if findingWith(findings, "v1", "", "") {
		t.Errorf("the clean variant got findings: %+v", findings)
	}
	// A variant with an unbacked law ranks below an otherwise equal clean variant.
	if strings.Index(kept, "v4") < strings.Index(kept, "v1") {
		t.Errorf("v4 should rank below v1: %v", shortlist)
	}

	// With the real deadline in the facts, the same urgency is honest and gets no finding.
	tc.MarketingContext = `{"offers":["Shea Butter Body Lotion KES 850. Offer ends: 30 November 2026"]}`
	_, f := r.review(context.Background(), adBatch(t, variants[1]), &promptopt.TaskSpec{}, tc, 10)
	if findingWith(f, "v2", "marketing law (urgency)", "") {
		t.Fatalf("urgency backed by a real end date has no finding: %+v", f)
	}
	// The owner's own request counts as a fact (a Studio brief carries the promotion's end date).
	tc.MarketingContext = ""
	_, f = r.review(context.Background(), adBatch(t, variants[1]), &promptopt.TaskSpec{OptimisedBrief: "Offer ends: 30 November 2026"}, tc, 10)
	if findingWith(f, "v2", "marketing law (urgency)", "") {
		t.Fatalf("urgency backed by the brief has no finding: %+v", f)
	}
}

// Plain-text copy (a WhatsApp message, a plan) keeps its text; the law is a review finding.
func TestReviewMarketingLawsOnPlainText(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return "" }, nil)
	tc := TenantContext{BusinessType: "generic"}
	for _, st := range []*Step{
		{Playbook: "whatsapp-sms", Text: "Hurry up, only 2 left!"},
		{Playbook: "marketing-plan", Text: "Week 2: a post saying only 2 left once stock is low."},
	} {
		text := st.Text
		_, f := r.review(context.Background(), st, &promptopt.TaskSpec{}, tc, 3)
		if st.Text != text || st.Error != "" || !findingWith(f, "text", "marketing law (scarcity)", "review") {
			t.Fatalf("%s: kept as written with a review finding: %+v %+v", st.Playbook, st, f)
		}
	}
}

// Unbacked pressure never triggers the safe rewrite; a banned claim still does.
func TestStepViolationsSkipMarketingLaws(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return "" }, nil)
	st := adBatch(t, AdVariant{ID: "v1", Hook: "Today only!", Headline: "Soft skin", PrimaryText: "Only 2 left. Was KES 1,200.", CTA: "Order on WhatsApp"})
	if v := r.stepViolations(st, TenantContext{BusinessType: "generic"}); len(v) != 0 {
		t.Fatalf("a marketing law is not a violation: %v", v)
	}
	msg := &Step{Playbook: "whatsapp-sms", Text: "Last chance, only 2 left!"}
	if v := r.stepViolations(msg, TenantContext{BusinessType: "generic"}); len(v) != 0 {
		t.Fatalf("a marketing law is not a violation on plain copy either: %v", v)
	}
	banned := adBatch(t, AdVariant{ID: "v1", Hook: "Dry skin?", Headline: "Soft skin", PrimaryText: "It cures eczema.", CTA: "Order on WhatsApp"})
	tc := TenantContext{BusinessType: "generic", MarketingContext: `{"banned_claims":["cures eczema"]}`}
	if v := r.stepViolations(banned, tc); len(v) == 0 {
		t.Fatal("a banned claim still triggers the rewrite")
	}
}

func TestPosterAndCalendarFormatChecks(t *testing.T) {
	q := library.MustLoad().Quality
	poster, _ := json.Marshal(PosterBrief{Headline: "Our brand new shea butter body lotion is now available in Kisumu", CTA: "WhatsApp or pay by M-Pesa"})
	f := lawFormatFindings(&Step{Contract: "poster_brief", Output: poster}, q)
	if !findingWith(f, "", "too many to read at a glance", "low") || !findingWith(f, "poster", "more than one next step", "low") {
		t.Fatalf("poster findings = %+v", f)
	}
	good, _ := json.Marshal(PosterBrief{Headline: "Soft skin all day", CTA: "WhatsApp 'SHEA' to order"})
	if f := lawFormatFindings(&Step{Contract: "poster_brief", Output: good}, q); len(f) != 0 {
		t.Fatalf("a good poster got findings: %+v", f)
	}
	cal, _ := json.Marshal(ContentCalendar{Items: []CalendarItem{{Hook: "Dry skin?", CTA: "WhatsApp us"}, {Hook: "Soft skin", CTA: ""}}})
	if f := lawFormatFindings(&Step{Contract: "content_calendar", Output: cal}, q); !findingWith(f, "c2", "no call to action", "low") {
		t.Fatalf("calendar findings = %+v", f)
	}
	posts, _ := json.Marshal(SocialPosts{Posts: []SocialPost{{Hook: "", Text: ""}, {Hook: "Dry skin?", Text: "Shea butter."}}})
	if f := lawFormatFindings(&Step{Contract: "social_posts", Output: posts}, q); len(f) != 1 || !findingWith(f, "p1", "no hook", "low") {
		t.Fatalf("post findings = %+v", f)
	}
}

func TestCTAChannels(t *testing.T) {
	channels := library.MustLoad().Quality.CTAChannels
	cases := map[string]int{
		"Order on WhatsApp":                        1,
		"WhatsApp 'LAMP' to 0712 for delivery":     1,
		"Call 0712 000 000 to book a fitting":      1,
		"Call or WhatsApp us":                      2,
		"Pay via M-Pesa till number 123, or visit": 2,
		"Fill in the form, email us or DM us":      3,
		"Agiza leo":                                0,
		"Lipa kwa Mpesa":                           1,
	}
	for cta, want := range cases {
		if got := ctaChannels(cta, channels); len(got) != want {
			t.Errorf("%q: channels %v, want %d", cta, got, want)
		}
	}
	// The video script check uses the same rule.
	q := library.MustLoad().Quality
	v := VideoScript{CTA: "Call or WhatsApp 0712 000 000", Scenes: []Scene{{ID: "s1", DurationS: 2.5, Voiceover: "Lights out again?"}}}
	if f := QualityFindings(v, q); !findingWith(f, "Call or WhatsApp", "more than one next step", "low") {
		t.Fatalf("script findings = %+v", f)
	}
}

// lawsDecider answers the persuasion score weak and the mixed-message question yes.
type lawsDecider struct{}

func (lawsDecider) Decide(_ context.Context, _ string, _ string, qs map[string]decide.Question) (decide.Answers, decide.Usage, error) {
	out := decide.Answers{}
	for id, q := range qs {
		switch {
		case strings.HasPrefix(id, "laws_"):
			out[id] = decide.Answer{Type: q.Type, Score: 0, Label: "weak", Confidence: 0.9}
		case id == "mixed_message":
			out[id] = decide.Answer{Type: q.Type, Prob: 0.9, Confidence: 0.9}
		case q.Type == decide.KindScore:
			out[id] = decide.Answer{Type: q.Type, Score: 1, Label: "fair", Confidence: 1}
		default:
			out[id] = decide.Answer{Type: q.Type, Prob: 0.05, Confidence: 0.9}
		}
	}
	return out, decide.Usage{}, nil
}

// The laws code cannot check are scored by the decide call: advice, never a block.
func TestReviewScoresPersuasionAndRepetition(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return "" }, lawsDecider{})
	st := adBatch(t,
		AdVariant{ID: "v1", Hook: "Dry skin?", Headline: "Soft skin", PrimaryText: "KES 850", CTA: "Order on WhatsApp"},
		AdVariant{ID: "v2", Hook: "New stock", Headline: "Lotion", PrimaryText: "KES 850", CTA: "Order on WhatsApp"})
	shortlist, f := r.review(context.Background(), st, &promptopt.TaskSpec{}, TenantContext{BusinessType: "generic"}, 10)
	if len(shortlist) != 2 {
		t.Fatalf("advice never drops a variant: %v", shortlist)
	}
	if !findingWith(f, "v1", "persuades weakly", "low") || !findingWith(f, "", "do not share one core message", "low") {
		t.Fatalf("findings = %+v", f)
	}
}
