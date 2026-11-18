package reqid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Bengo-Hub/httpware"
)

// sent returns the X-Request-ID a request to target carried, through a Transport whose base
// answers locally.
func sent(t *testing.T, hosts Hosts, target string, ctx context.Context, preset string) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(httpware.HeaderRequestID)
	}))
	defer srv.Close()
	// Send to the local server, but name the target host in the URL the transport sees.
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		u, _ := url.Parse(srv.URL)
		r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(r)
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if preset != "" {
		req.Header.Set(httpware.HeaderRequestID, preset)
	}
	resp, err := (&http.Client{Transport: Transport{Base: base, Hosts: hosts}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return got
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportForwardsToServicesOnly(t *testing.T) {
	hosts := HostsFrom("http://marketflow-api.marketflow.svc.cluster.local:4000", "https://sso.codevertexafrica.com", "", "::bad")
	ctx := httpware.WithRequestID(context.Background(), "req-123")

	if got := sent(t, hosts, "http://marketflow-api.marketflow.svc.cluster.local:4000/api/v1/x", ctx, ""); got != "req-123" {
		t.Fatalf("marketflow-api gets the request id, got %q", got)
	}
	if got := sent(t, hosts, "https://SSO.codevertexafrica.com/api/v1/auth/me", ctx, ""); got != "req-123" {
		t.Fatalf("a configured service host gets it, case-insensitively, got %q", got)
	}
	if got := sent(t, hosts, "http://notifications-api.notifications.svc.cluster.local:4000/x", ctx, ""); got != "req-123" {
		t.Fatalf("any in-cluster service gets it, got %q", got)
	}
	if got := sent(t, hosts, "https://api.anthropic.com/v1/messages", ctx, ""); got != "" {
		t.Fatalf("a third party never gets the internal id, got %q", got)
	}
	if got := sent(t, hosts, "http://marketflow-api.marketflow.svc.cluster.local:4000/x", context.Background(), ""); got != "" {
		t.Fatalf("no id in the context, no header, got %q", got)
	}
	if got := sent(t, hosts, "http://marketflow-api.marketflow.svc.cluster.local:4000/x", ctx, "caller-set"); got != "caller-set" {
		t.Fatalf("an id the caller set is kept, got %q", got)
	}
}

func TestTransportDoesNotChangeTheCallersRequest(t *testing.T) {
	hosts := HostsFrom("http://api.internal:4000")
	req, _ := http.NewRequestWithContext(httpware.WithRequestID(context.Background(), "r1"), http.MethodGet, "http://api.internal:4000/x", nil)
	tr := Transport{Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	}), Hosts: hosts}
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(httpware.HeaderRequestID) != "" {
		t.Fatal("the caller's request must stay untouched")
	}
}
