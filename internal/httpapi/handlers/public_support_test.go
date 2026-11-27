package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/support"
)

type fakeSupportRouter struct {
	settings support.Settings
	got      *support.Request
}

func (f *fakeSupportRouter) Settings(context.Context, string) support.Settings { return f.settings }
func (f *fakeSupportRouter) Route(_ context.Context, req support.Request, _ support.Settings) support.Outcome {
	f.got = &req
	return support.Outcome{Route: support.RouteStaffNotified, Message: "sent"}
}
func (f *fakeSupportRouter) ContactOnly(context.Context, string) support.Outcome {
	return support.Outcome{Route: support.RouteContactOnly, Message: "reach us"}
}

type fakeCodes struct {
	email string
	err   error
}

func (f fakeCodes) Validate(context.Context, string) (string, error) { return f.email, f.err }

func supportRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/public/support", strings.NewReader(body))
	r.Header.Set("X-Tenant-ID", "hadia")
	r.Header.Set("X-OTP-Token", "tok")
	return r
}

func known(context.Context, string) (uuid.UUID, bool) { return uuid.New(), true }

func TestPublicSupportRoutingOffKeepsTheOldTicket(t *testing.T) {
	var gotPath, gotToken string
	var gotBody map[string]string
	ticketing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken = r.URL.Path, r.Header.Get("X-OTP-Token")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ticket_number":"TKT-000007"}`))
	}))
	defer ticketing.Close()
	forms := NewPublicFormsHandler("", ticketing.URL, "", zap.NewNop())
	h := NewPublicSupportHandler(forms, &fakeSupportRouter{settings: support.DefaultSettings()}, fakeCodes{}, known, zap.NewNop())

	w := httptest.NewRecorder()
	h.Submit(w, supportRequest(`{"name":"Jane","description":"My gift is missing","priority":"Low","page":"https://hadiafrica.com/r/1"}`))
	if w.Code != http.StatusCreated || gotPath != "/api/v1/hadia/public/tickets" || gotToken != "tok" {
		t.Fatalf("legacy forward: %d %s %q", w.Code, gotPath, gotToken)
	}
	if gotBody["subject"] != "My gift is missing" || !strings.Contains(gotBody["description"], "hadiafrica.com") || gotBody["name"] != "Jane" {
		t.Fatalf("legacy body: %v", gotBody)
	}
}

func TestPublicSupportRoutesWithTheVerifiedEmail(t *testing.T) {
	s := support.DefaultSettings()
	s.Enabled = true
	fr := &fakeSupportRouter{settings: s}
	h := NewPublicSupportHandler(NewPublicFormsHandler("", "", "", zap.NewNop()), fr, fakeCodes{email: "verified@example.com"}, known, zap.NewNop())
	w := httptest.NewRecorder()
	h.Submit(w, supportRequest(`{"name":"Jane","description":"refund please","phone":"0712345678"}`))
	if w.Code != http.StatusOK || fr.got == nil || fr.got.Email != "verified@example.com" || fr.got.TenantSlug != "hadia" || fr.got.RequestID == "" {
		t.Fatalf("route: %d %+v", w.Code, fr.got)
	}
}

func TestPublicSupportCodeProblems(t *testing.T) {
	s := support.DefaultSettings()
	s.Enabled = true
	for name, tc := range map[string]struct {
		err    error
		status int
		route  string
	}{
		"expired code":     {support.ErrOTPInvalid, http.StatusUnauthorized, ""},
		"auth-api is down": {errors.New("dial tcp: refused"), http.StatusOK, support.RouteContactOnly},
	} {
		fr := &fakeSupportRouter{settings: s}
		h := NewPublicSupportHandler(NewPublicFormsHandler("", "", "", zap.NewNop()), fr, fakeCodes{err: tc.err}, known, zap.NewNop())
		w := httptest.NewRecorder()
		h.Submit(w, supportRequest(`{"description":"help"}`))
		if w.Code != tc.status || fr.got != nil {
			t.Fatalf("%s: %d, routed %v", name, w.Code, fr.got != nil)
		}
		if tc.route != "" && !strings.Contains(w.Body.String(), tc.route) {
			t.Fatalf("%s: body %s", name, w.Body.String())
		}
	}
}

func TestPublicSupportValidation(t *testing.T) {
	h := NewPublicSupportHandler(NewPublicFormsHandler("", "", "", zap.NewNop()), &fakeSupportRouter{}, fakeCodes{}, known, zap.NewNop())
	w := httptest.NewRecorder()
	h.Submit(w, supportRequest(`{"description":"  "}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty description: %d", w.Code)
	}
	r := supportRequest(`{"description":"x"}`)
	r.Header.Del("X-OTP-Token")
	w = httptest.NewRecorder()
	h.Submit(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no code: %d", w.Code)
	}
}
