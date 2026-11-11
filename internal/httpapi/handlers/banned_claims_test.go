package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/team"
)

type bannedSource struct{ ctx string }

func (s bannedSource) Load(context.Context, uuid.UUID) (team.TenantContext, error) {
	return team.TenantContext{MarketingContext: s.ctx}, nil
}

// Autopilot and Studio video scripts are checked against the business's banned claims before
// anything is rendered (they do not pass the team Reviewer).
func TestOSHandlerBannedClaims(t *testing.T) {
	h := &OSHandler{source: bannedSource{`{"banned_claims":["cures eczema"]}`}}
	got := h.bannedClaims(context.Background(), uuid.New())
	if len(got) != 1 || team.MatchBanned("It cures eczema", got) == "" {
		t.Fatalf("banned claims: %q", got)
	}
	if (&OSHandler{}).bannedClaims(context.Background(), uuid.New()) != nil {
		t.Fatal("no source, no claims")
	}
}
