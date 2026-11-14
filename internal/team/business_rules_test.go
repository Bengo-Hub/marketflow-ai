package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// Sprint 7 #34: the tenant's business type rules reach the writer's prompt, and its blocks
// refuse copy that breaks them.
func TestBusinessTypeRulesShapeDrafts(t *testing.T) {
	var gotSystem string
	r, _ := newRuntime(promptopt.TaskSpec{}, func(system, user string) string {
		gotSystem = system
		return `{"text":"Get 20% off amoxicillin this week."}`
	}, nil)
	r.source = ctxSource{TenantContext{BusinessType: "pharmacy"}}
	_, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "tagline"})
	if !strings.Contains(gotSystem, "Pharmacy or chemist") || !strings.Contains(gotSystem, "prescription-only") {
		t.Fatalf("the pharmacy rules are missing from the prompt:\n%s", gotSystem)
	}
	if !errors.Is(err, ErrSuggestionBlocked) {
		t.Fatalf("a prescription-only promotion must be refused for a pharmacy: %v", err)
	}

	// The same line passes for a restaurant (not its rule), and the prompt carries the restaurant rules.
	r.source = ctxSource{TenantContext{BusinessType: "restaurant"}}
	if _, err := r.SuggestField(context.Background(), FieldSuggestRequest{TenantID: uuid.New(), Field: "tagline"}); err != nil {
		t.Fatalf("restaurant: %v", err)
	}
	if !strings.Contains(gotSystem, "Restaurant or cafe") || strings.Contains(gotSystem, "prescription-only") {
		t.Fatalf("restaurant prompt has the wrong rules:\n%s", gotSystem)
	}
}
