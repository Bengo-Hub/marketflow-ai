// Package reqid carries the request id of an incoming call (httpware.RequestID) onto marketflow-ai's
// own calls to other Codevertex services, as X-Request-ID, so one request can be followed through
// marketflow-api, marketflow-ai and the services they call. marketflow-api does the same with its
// internal/platform/reqid package.
//
// The id is internal: it is sent only to hosts this service knows as Codevertex services (the
// service URLs in its config, and anything inside the cluster), never to model providers, fal or
// any other third party.
package reqid

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Bengo-Hub/httpware"
)

// clusterSuffix marks an in-cluster service host.
const clusterSuffix = ".svc.cluster.local"

// Hosts is the set of hosts that get the request id.
type Hosts map[string]bool

// HostsFrom builds the set from service base URLs; unreadable or empty URLs are skipped.
func HostsFrom(urls ...string) Hosts {
	out := Hosts{}
	for _, raw := range urls {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" {
			continue
		}
		out[strings.ToLower(u.Hostname())] = true
	}
	return out
}

// internal reports whether host is a Codevertex service.
func (h Hosts) internal(host string) bool {
	host = strings.ToLower(host)
	return h[host] || strings.HasSuffix(host, clusterSuffix)
}

// Transport adds the context's request id to requests for internal hosts that carry none.
type Transport struct {
	Base  http.RoundTripper
	Hosts Hosts
}

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if id := httpware.GetRequestID(r.Context()); id != "" && r.Header.Get(httpware.HeaderRequestID) == "" && t.Hosts.internal(r.URL.Hostname()) {
		r = r.Clone(r.Context()) // a RoundTripper must not change the caller's request
		r.Header.Set(httpware.HeaderRequestID, id)
	}
	return base.RoundTrip(r)
}

var installOnce sync.Once

// Install wraps http.DefaultTransport once, so every client that uses the default transport
// (most of this service's clients, and guardrail.ActingTransport's base) forwards the id.
func Install(hosts Hosts) {
	installOnce.Do(func() {
		http.DefaultTransport = Transport{Base: http.DefaultTransport, Hosts: hosts}
	})
}
