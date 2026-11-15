package team

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/library"
)

func TestIsTeamRequest(t *testing.T) {
	for _, s := range []string{
		"Set up a marketing team for me", "setup my team", "Please create my marketing team", "show me my team",
		"review my marketing team", "who is on my team?", "our team structure", "org chart", "organogram",
		"Unda timu ya marketing", "nionyeshe timu yangu", "timu yangu",
	} {
		if !IsTeamRequest(s) {
			t.Errorf("%q is a team request", s)
		}
	}
	for _, s := range []string{
		"ask my team to plan this month's posts", "write three ads for the lotion", "plan a launch",
		"how many leads came in yesterday", "my team sold 40 bags", "the football team ad",
	} {
		if IsTeamRequest(s) {
			t.Errorf("%q is marketing work, not a team request", s)
		}
	}
}

// The org chart names the manager first, every working member with skills and what it can do in
// plain words, the members that are off with the reason, and what changed. It names no endpoint,
// key or internal tool, and fits one WhatsApp message.
func TestOrgChart(t *testing.T) {
	lib := library.MustLoad()
	setup := TeamSetup{First: true, Members: []MemberFit{
		{Key: "seo_specialist", Enabled: false, Reason: "has no website or online store to improve yet: add one to switch it on", Changed: true},
		{Key: "ads_specialist", Enabled: true, Limits: []string{"writes and tests ad copy; launching paid ads needs a connected ad account"}},
	}}
	got := OrgChart(lib, setup, []MemberOverride{{Key: "manager", Name: "Amani"}})
	for _, want := range []string{"Your marketing team is ready", "Amani, Marketing Manager", "Reporting to Amani", "Strategist",
		"Skills:", "reads your product and service catalogue", "Off for now", "SEO Specialist: has no website",
		"What I changed: SEO Specialist switched off", "launching paid ads needs a connected ad account", "I quote credits"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, leak := range []string{"query_service_data", "kb_search", "/api/", "X-API-Key", "premium", "model"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the org chart must not show %q:\n%s", leak, got)
		}
	}
	if len(got) > 4096 {
		t.Fatalf("the org chart must fit one WhatsApp message: %d characters", len(got))
	}
	if strings.Contains(got, "\u2014") {
		t.Fatal("no em dashes in replies")
	}
	if !strings.Contains(OrgChart(lib, TeamSetup{}, nil), "I reviewed your marketing team") {
		t.Fatal("a later run is a review")
	}
}

// A portal request goes to the role-checked route with the user's own token; WhatsApp goes to the
// internal route with the service key; a 403 is ErrTeamForbidden.
func TestAPISourceSetupTeam(t *testing.T) {
	var gotPath, gotAuth, gotKey, gotTenant string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotKey, gotTenant = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-API-Key"), r.Header.Get("X-Tenant-ID")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"first":true,"members":[{"key":"seo_specialist","enabled":false}]}`))
	}))
	defer srv.Close()
	s := &APISource{BaseURL: srv.URL, Key: "svc"}
	tid := uuid.New()

	out, err := s.SetupTeam(context.Background(), tid, "Bearer user-token")
	if err != nil || !out.First || len(out.Members) != 1 {
		t.Fatalf("portal setup: %+v %v", out, err)
	}
	if gotPath != "/api/v1/agents/team/setup" || gotAuth != "Bearer user-token" || gotKey != "" || gotTenant != tid.String() {
		t.Fatalf("portal call: %s %q %q %q", gotPath, gotAuth, gotKey, gotTenant)
	}
	if _, err := s.SetupTeam(context.Background(), tid, ""); err != nil || gotPath != "/api/v1/internal/os/team/setup" || gotKey != "svc" || gotAuth != "" {
		t.Fatalf("internal call: %s %q %q %v", gotPath, gotKey, gotAuth, err)
	}
	status = http.StatusForbidden
	if _, err := s.SetupTeam(context.Background(), tid, "Bearer viewer"); !errors.Is(err, ErrTeamForbidden) {
		t.Fatalf("a viewer is refused: %v", err)
	}
}

// The free first setup asks marketflow-api for only_first on both routes; a 409 means the first
// setup was done and the request goes through the charged turn.
func TestAPISourceSetupFirstTeam(t *testing.T) {
	var gotQuery []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = append(gotQuery, r.URL.Path+"?"+r.URL.RawQuery)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"first":true,"members":[]}`))
	}))
	defer srv.Close()
	s := &APISource{BaseURL: srv.URL, Key: "svc"}
	tid := uuid.New()

	if out, err := s.SetupFirstTeam(context.Background(), tid, "Bearer owner"); err != nil || !out.First {
		t.Fatalf("first setup: %+v %v", out, err)
	}
	if _, err := s.SetupFirstTeam(context.Background(), tid, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/v1/agents/team/setup?only_first=true", "/api/v1/internal/os/team/setup?tenant_id=" + tid.String() + "&only_first=true"}
	if len(gotQuery) != 2 || gotQuery[0] != want[0] || gotQuery[1] != want[1] {
		t.Fatalf("calls %v, want %v", gotQuery, want)
	}
	status = http.StatusConflict
	if _, err := s.SetupFirstTeam(context.Background(), tid, "Bearer owner"); !errors.Is(err, ErrTeamAlreadySetUp) {
		t.Fatalf("a done setup is ErrTeamAlreadySetUp: %v", err)
	}
	// The normal setup never reads 409 as "already set up".
	if _, err := s.SetupTeam(context.Background(), tid, "Bearer owner"); err == nil || errors.Is(err, ErrTeamAlreadySetUp) {
		t.Fatalf("plain setup on 409: %v", err)
	}
}

// KnownTeamRequest never asks for a decision: patterns, then learned phrasings with a team word.
func TestKnownTeamRequest(t *testing.T) {
	ctx, tid := context.Background(), uuid.New()
	l := &fakeLearner{known: []string{"get my team going"}}
	if !KnownTeamRequest(ctx, tid, "set up my marketing team", nil) {
		t.Error("a fixed pattern is known")
	}
	if !KnownTeamRequest(ctx, tid, "Get my team going!", l) {
		t.Error("a learned phrasing is known")
	}
	if KnownTeamRequest(ctx, tid, "write three ads for the lotion", l) {
		t.Error("marketing work is not a team request")
	}
}
