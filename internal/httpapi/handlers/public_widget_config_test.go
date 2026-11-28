package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The widget config passes on the profile's white_label and nothing else; any failure keeps the
// badge (false), and a bad slug never reaches marketflow-api.
func TestWidgetConfig(t *testing.T) {
	var asked []string
	var realIP string
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		realIP = r.Header.Get("X-Real-IP")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/public/profile/acme":
			_, _ = w.Write([]byte(`{"id":"1","name":"Acme","white_label":true,"whatsapp_number":"254700000000","metadata":{"logo_url":"https://cdn.example/acme.png"}}`))
		case "/api/v1/public/profile/plain":
			_, _ = w.Write([]byte(`{"id":"2","name":"Plain","white_label":false,"metadata":{"logo_url":"javascript:alert(1)"}}`))
		case "/api/v1/public/profile/broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v1/public/profile/garbled":
			_, _ = w.Write([]byte(`not json`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"profile not found"}`))
		}
	}))
	defer owner.Close()
	h := NewPublicFormsHandler(owner.URL, owner.URL, owner.URL, zap.NewNop())

	get := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/public/widget-config"+query, nil)
		req.RemoteAddr = "203.0.113.7:0" // as ratelimit.TrustedRealIP leaves it
		rec := httptest.NewRecorder()
		h.WidgetConfig(rec, req)
		return rec
	}

	cases := []struct {
		query, want string
	}{
		{"?tenant=acme", `{"white_label":true,"logo_url":"https://cdn.example/acme.png","name":"Acme"}`},
		{"?tenant=ACME", `{"white_label":true,"logo_url":"https://cdn.example/acme.png","name":"Acme"}`},
		// Only an https logo is passed on.
		{"?tenant=plain", `{"white_label":false,"name":"Plain"}`},
		{"?tenant=missing", `{"white_label":false}`},
		{"?tenant=broken", `{"white_label":false}`},
		{"?tenant=garbled", `{"white_label":false}`},
	}
	for _, c := range cases {
		rec := get(c.query)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != c.want {
			t.Errorf("%s: %d %s, want %s", c.query, rec.Code, rec.Body.String(), c.want)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
			t.Errorf("%s: Cache-Control %q", c.query, cc)
		}
	}
	// Only the flag, name and logo are passed on, never the rest of the profile.
	if rec := get("?tenant=acme"); strings.Contains(rec.Body.String(), "whatsapp") {
		t.Errorf("profile fields leaked: %s", rec.Body.String())
	}
	if realIP != "203.0.113.7" {
		t.Errorf("visitor address not passed on: %q", realIP)
	}

	asked = nil
	for _, q := range []string{"", "?tenant=", "?tenant=../admin", "?tenant=a%2Fb"} {
		if rec := get(q); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d, want 400", q, rec.Code)
		}
	}
	if len(asked) != 0 {
		t.Errorf("a bad slug reached marketflow-api: %v", asked)
	}
}
