package team

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// The brand context page promises "The reviewer blocks any draft that says these": the
// business's banned claims are checked in code, not only given to the model.
func TestBannedClaims(t *testing.T) {
	claims := BannedClaims(`{"positioning":"x","banned_claims":["Cures eczema","  ","guaranteed income"]}`)
	if len(claims) != 2 {
		t.Fatalf("claims: %q", claims)
	}
	if len(BannedClaims("plain text context")) != 0 || len(BannedClaims("")) != 0 {
		t.Fatal("no list, no claims")
	}
	cases := []struct {
		text string
		want string
	}{
		{"This lotion CURES   eczema in days", "Cures eczema"},
		{"Earn a guaranteed income!", "guaranteed income"},
		{"Soothes dry skin", ""},
		{"curesEczema", ""}, // whole words only
	}
	for _, c := range cases {
		if got := MatchBanned(c.text, claims); got != c.want {
			t.Errorf("%q: got %q want %q", c.text, got, c.want)
		}
	}
}

func TestReviewBlocksBannedClaim(t *testing.T) {
	r, _ := newRuntime(promptopt.TaskSpec{}, func(string, string) string { return "" }, nil)
	batch, _ := json.Marshal(AdCopyBatch{Variants: []AdVariant{
		{ID: "v1", Hook: "Dry skin?", Headline: "Shea butter lotion", PrimaryText: "Soft skin all day.", CTA: "Order"},
		{ID: "v2", Hook: "Itchy?", Headline: "It cures eczema", PrimaryText: "Try it.", CTA: "Order"},
	}})
	st := &Step{Contract: "ad_copy_batch", Output: batch}
	tc := TenantContext{BusinessType: "generic", MarketingContext: `{"banned_claims":["cures eczema"]}`}
	shortlist, findings := r.review(context.Background(), st, &promptopt.TaskSpec{}, tc, 3)
	if len(shortlist) != 1 || shortlist[0] != "v1" {
		t.Fatalf("the banned variant is dropped: %v", shortlist)
	}
	found := false
	for _, f := range findings {
		if f.Severity == "high" && f.Quote == "v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a high finding names the variant: %+v", findings)
	}

	// Plain text steps are blocked the same way.
	txt := &Step{Contract: "plain", Text: "Our lotion cures eczema."}
	r.review(context.Background(), txt, &promptopt.TaskSpec{}, tc, 3)
	if txt.Text != "" || txt.Error == "" {
		t.Fatalf("plain text with a banned claim is blocked: %+v", txt)
	}
}
