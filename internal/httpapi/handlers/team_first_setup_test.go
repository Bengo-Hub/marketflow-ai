package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// firstSetupSource is a context source whose marketflow-api answers the first-only setup.
type firstSetupSource struct {
	done       bool // a first setup already happened: only_first answers 409
	firstCalls int
	bearer     string
}

func (s *firstSetupSource) Load(context.Context, uuid.UUID) (team.TenantContext, error) {
	return team.TenantContext{}, nil
}

func (s *firstSetupSource) SetupTeam(context.Context, uuid.UUID, string) (team.TeamSetup, error) {
	return team.TeamSetup{}, nil
}

func (s *firstSetupSource) SetupFirstTeam(_ context.Context, _ uuid.UUID, bearer string) (team.TeamSetup, error) {
	s.firstCalls++
	s.bearer = bearer
	if s.done {
		return team.TeamSetup{}, team.ErrTeamAlreadySetUp
	}
	return team.TeamSetup{First: true, Members: []team.MemberFit{{Key: "seo_specialist", Enabled: false, Reason: "no website yet"}}}, nil
}

func askCall(t *testing.T, h *OSHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/os/ask", strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer owner-token")
	r = r.WithContext(authclient.ContextWithClaims(r.Context(), &authclient.Claims{TenantID: uuid.NewString()}))
	rec := httptest.NewRecorder()
	h.Ask(rec, r)
	return rec
}

// Sprint 21: a tenant with zero AI credits reaches the free first team setup from the console,
// by the Set up my team button or by asking in words, and is never charged for it.
func TestAskFirstTeamSetupWithoutCredits(t *testing.T) {
	broke := &fakeSpender{decision: CreditDecision{Allowed: false, Reason: "insufficient_credits"}}
	src := &firstSetupSource{}
	h := &OSHandler{lib: library.MustLoad(), source: src, credits: broke, logger: zap.NewNop()}

	for _, body := range []map[string]any{
		{"text": "Set up my team", "team_setup": true},
		{"text": "set up my marketing team"},
	} {
		rec := askCall(t, h, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%v: status %d %s", body, rec.Code, rec.Body.String())
		}
		var out askResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Result == nil || out.Result.Route != team.RouteTeam || out.CreditsCharged != 0 || !strings.Contains(out.Answer, "Your marketing team is ready") {
			t.Fatalf("%v: org chart answer, nothing charged: %+v", body, out)
		}
	}
	if broke.spent != 0 || len(broke.refunded) != 0 {
		t.Fatalf("no credit touched: spent %d refunded %v", broke.spent, broke.refunded)
	}
	if src.bearer != "Bearer owner-token" {
		t.Fatalf("the portal setup runs with the user's own token (role check): %q", src.bearer)
	}

	// After the first setup a review is the normal charged turn, so zero credits answer 402.
	src.done = true
	if rec := askCall(t, h, map[string]any{"text": "review my team", "team_setup": true}); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("a review without credits is refused: %d %s", rec.Code, rec.Body.String())
	}

	// Marketing work never tries the free setup.
	src.firstCalls = 0
	_ = askCall(t, h, map[string]any{"text": "write three ads for the lotion"})
	if src.firstCalls != 0 {
		t.Fatal("marketing work must not run the team setup")
	}
}
