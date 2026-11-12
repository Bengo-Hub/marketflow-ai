package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
)

func TestRestrictedOnly(t *testing.T) {
	restricted := []string{"marketflow_reviewer"}
	cases := []struct {
		name   string
		claims *authclient.Claims
		want   bool
	}{
		{"no claims (service key caller)", nil, false},
		{"reviewer only", &authclient.Claims{Roles: []string{"marketflow_reviewer"}}, true},
		{"reviewer, case and spaces", &authclient.Claims{Roles: []string{" Marketflow_Reviewer "}}, true},
		{"reviewer plus member", &authclient.Claims{Roles: []string{"marketflow_reviewer", "marketflow_member"}}, false},
		{"plain member", &authclient.Claims{Roles: []string{"member"}}, false},
		{"platform owner reviewer", &authclient.Claims{Roles: []string{"marketflow_reviewer"}, IsPlatformOwner: true}, false},
	}
	for _, c := range cases {
		if got := restrictedOnly(c.claims, restricted); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDenyRestrictedRoles(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	claimsOf := func(r *http.Request) (*authclient.Claims, bool) {
		if r.Header.Get("X-Test-Reviewer") == "1" {
			return &authclient.Claims{Roles: []string{"marketflow_reviewer"}}, true
		}
		return nil, false
	}
	h := denyRestrictedRoles([]string{"marketflow_reviewer"}, claimsOf)(next)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/os/make-media", nil)
	r.Header.Set("X-Test-Reviewer", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("reviewer got %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/os/make-media", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("other caller got %d, want 200", w.Code)
	}

	// An empty list switches the guard off.
	off := denyRestrictedRoles(nil, claimsOf)(next)
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/api/v1/chat", nil)
	r.Header.Set("X-Test-Reviewer", "1")
	off.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("guard off got %d, want 200", w.Code)
	}
}
