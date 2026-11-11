package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

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
