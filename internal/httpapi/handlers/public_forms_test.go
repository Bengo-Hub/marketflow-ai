package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestPublicFormsForward(t *testing.T) {
	type seen struct{ path, realIP, otp, body string }
	var got seen
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.URL.Path, r.Header.Get("X-Real-IP"), r.Header.Get("X-OTP-Token"), string(b)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ticket_number":"TCK-1"}`))
	}))
	defer owner.Close()
	h := NewPublicFormsHandler(owner.URL, owner.URL, owner.URL, zap.NewNop())

	post := func(handler http.HandlerFunc, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.RemoteAddr = "203.0.113.7:0" // as ratelimit.TrustedRealIP leaves it
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	rec := post(h.Ticket, `{"subject":"POS down","description":"It will not print"}`,
		map[string]string{"X-Tenant-ID": "codevertex", "X-OTP-Token": "tok"})
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), "TCK-1") {
		t.Fatalf("ticket: %d %s", rec.Code, rec.Body.String())
	}
	if got.path != "/api/v1/codevertex/public/tickets" || got.realIP != "203.0.113.7" || got.otp != "tok" {
		t.Fatalf("ticket forwarded as %+v", got)
	}

	post(h.IntegrationRequest, `{"requester_name":"A","requester_email":"a@b.co"}`, nil)
	if got.path != "/api/v1/integration-requests" || got.otp != "" {
		t.Fatalf("integration forwarded as %+v", got)
	}

	post(h.Lead, `{"email":"a@b.co"}`, map[string]string{"X-Tenant-ID": "jane-fbo"})
	if got.path != "/api/v1/public/leads/jane-fbo" {
		t.Fatalf("lead forwarded as %+v", got)
	}

	got = seen{}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"ticket without code": post(h.Ticket, `{}`, map[string]string{"X-Tenant-ID": "codevertex"}),
		"bad slug":            post(h.Lead, `{}`, map[string]string{"X-Tenant-ID": "../admin"}),
		"not an object":       post(h.IntegrationRequest, `["x"]`, nil),
	} {
		if rec.Code < 400 || got.path != "" {
			t.Fatalf("%s: want a 4xx and no forward, got %d and %+v", name, rec.Code, got)
		}
	}
}
