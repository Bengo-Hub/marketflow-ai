package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// "Generate with AI" on the profile and brand context forms (owner, 2026-11-11): one field
// at a time, from the business's own context, within the field's length, through the same
// compliance and banned-claims checks as every draft.
func TestSuggestFieldText(t *testing.T) {
	var gotSystem, gotUser string
	r, _ := newRuntime(promptopt.TaskSpec{}, func(system, user string) string {
		gotSystem, gotUser = system, user
		return `{"text":"Handmade shea butter care from Kisumu, for soft skin all day."}`
	}, nil)
	out, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "tagline", Form: map[string]string{"name": "Shea Glow"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text == "" || len([]rune(out.Text)) > 80 || len(out.Items) != 0 {
		t.Fatalf("tagline: %+v", out)
	}
	if !strings.Contains(gotUser, "Shea butter lotion made in Kisumu") || !strings.Contains(gotUser, "Shea Glow") || !strings.Contains(gotSystem, "80") {
		t.Fatalf("prompt carries the business context, the form and the limit:\nSYSTEM %s\nUSER %s", gotSystem, gotUser)
	}
}

func TestSuggestFieldList(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string {
		return `{"items":["Busy mothers in Kisumu","Salon owners","","Students who want affordable skin care","Office workers","Gym goers","Extra one"]}`
	}, nil)
	out, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "audience"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) == 0 || len(out.Items) > 5 || out.Text != "" {
		t.Fatalf("audience list: %+v", out)
	}
	for _, it := range out.Items {
		if strings.TrimSpace(it) == "" {
			t.Fatal("empty items are dropped")
		}
	}
}

// The inbox's default reply always carries a link placeholder the auto reply can fill, inside
// the 500 characters marketflow-api keeps.
func TestSuggestInboxReplyKeepsLink(t *testing.T) {
	cases := map[string]string{
		"kept":     `{"text":"Thank you for asking! See prices and leave your number here: {{booking_link}}"}`,
		"dropped":  `{"text":"Thank you for asking! See our prices and leave your number on our page."}`,
		"too long": `{"text":"` + strings.Repeat("Thanks for asking about our shea butter. ", 15) + `{{profile_link}}"}`,
	}
	for name, reply := range cases {
		r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return reply }, nil)
		out, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "inbox_reply_text"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hasLink := strings.Contains(out.Text, "{{profile_link}}") || strings.Contains(out.Text, "{{booking_link}}")
		if !hasLink || len([]rune(out.Text)) > 500 || strings.Count(out.Text, "{{") != 1 {
			t.Fatalf("%s: %q", name, out.Text)
		}
	}
	if got := withLink("{{profile_link}}", 500); got != "" {
		t.Fatalf("a bare placeholder is no reply: %q", got)
	}
}

func TestSuggestFieldRefusals(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return `{"text":"It cures eczema in a week."}` }, nil)
	if _, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "password"}); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("only listed fields: %v", err)
	}
	// The business bans this claim: the suggestion is refused, never shown.
	r.source = ctxSource{TenantContext{BusinessType: "generic", MarketingContext: `{"banned_claims":["cures eczema"]}`}}
	if _, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "bio"}); !errors.Is(err, ErrSuggestionBlocked) {
		t.Fatalf("banned claim: %v", err)
	}
}

// "What has worked before" learns from the tenant's own competitors, reference ads and
// complaints (owner, 2026-11-28), and a lesson that names a competitor would still be the
// model's job to avoid; the prompt says so.
func TestSuggestLearningsFromCompetitors(t *testing.T) {
	var gotSystem, gotUser string
	f := &fakeLLM{reply: func(system, user string) string {
		gotSystem, gotUser = system, user
		return `{"items":["Short vertical videos of a couple opening gifts hold attention","Show M-Pesa contributions landing in real time"]}`
	}}
	router := llm.NewRouter(llm.Config{Providers: oneSource{f}, Defaults: llm.DefaultPolicy(llm.Defaults{})})
	r := New(Config{Library: library.MustLoad(), Opt: fixedOpt{}, Router: router, Source: ctxSource{TenantContext{
		BusinessType: "generic", MarketingContext: `{"positioning":"Gift registry for Kenyan celebrations"}`, HasPlan: true,
		MarketSignals: &MarketSignals{
			Competitors: []string{"Zola", "M-Changa"},
			References:  []ReferenceAd{{URL: "https://example.com/a", Platform: "tiktok", Notes: "couple unboxing gifts"}},
			Complaints:  []string{"Guests duplicate gifts"},
		},
	}}})
	out, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "learnings"})
	if err != nil || len(out.Items) != 2 {
		t.Fatalf("learnings: %v %+v", err, out)
	}
	for _, want := range []string{"Zola; M-Changa", "couple unboxing gifts", "Guests duplicate gifts", "Gift registry for Kenyan celebrations"} {
		if !strings.Contains(gotUser, want) {
			t.Fatalf("prompt misses %q:\n%s", want, gotUser)
		}
	}
	if !strings.Contains(gotSystem, "Never name another business") {
		t.Fatalf("system must forbid naming rivals: %s", gotSystem)
	}
	// The ad writers never see the names.
	if strings.Contains(marketBlock(&MarketSignals{Competitors: []string{"Zola"}, Complaints: []string{"x"}}, "ad_copy_batch"), "Zola") {
		t.Fatal("competitor names must not reach the ad writers")
	}
}
