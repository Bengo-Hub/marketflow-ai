package middleware

import (
	"net/http"
	"strings"
	"time"

	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
)

// PublicChatLimits limits the unauthenticated public chat endpoint per client IP and per
// tenant, on shared-ratelimit (GCRA in Redis, exact across replicas, per-pod fallback while
// Redis is down). It replaces a local Lua sliding window whose keys had no service prefix,
// which kept a global "rl:seq" counter outside its declared keys, and which trusted the
// first X-Forwarded-For entry for the client IP.
//
// The tenant comes from the X-Tenant-ID header by design (the embeddable widget has no
// login), so the per-tenant cap is caller-supplied: the per-IP limit is what makes it costly
// to burn another tenant's allowance. Tenant keys are normalized and length-bounded so junk
// headers cannot mint unlimited keys.
//
// dailyTenantLimit (0 = none) is a per-tenant ceiling per 24 hours, so a bot cannot spend a
// tenant's AI credits all day at the per-minute rate (audit MF #10). The shared limiter skips
// requests that ask for an event stream; the stream hint is hidden while the limits are checked
// and restored for the handler, so streamed chats are limited too.
func PublicChatLimits(l *ratelimit.Limiter, ipLimit, tenantLimit, dailyTenantLimit int) func(http.Handler) http.Handler {
	tenantKey := func(r *http.Request) string {
		t := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tenant-ID")))
		if t == "" || len(t) > 64 {
			return ""
		}
		return "tenant:" + t
	}
	perIP := l.MiddlewareWith(ratelimit.IPKey, ratelimit.Options{Name: "public-chat-ip", Limit: ipLimit, Window: time.Minute})
	perTenant := l.MiddlewareWith(tenantKey, ratelimit.Options{Name: "public-chat-tenant", Limit: tenantLimit, Window: time.Minute})
	perDay := func(next http.Handler) http.Handler { return next }
	if dailyTenantLimit > 0 {
		perDay = l.MiddlewareWith(tenantKey, ratelimit.Options{
			Name: "public-chat-tenant-day", Limit: dailyTenantLimit, Window: 24 * time.Hour,
		})
	}
	return func(next http.Handler) http.Handler {
		restore := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a := r.Header.Get(hiddenAcceptHeader); a != "" {
				r.Header.Set("Accept", a)
				r.Header.Del(hiddenAcceptHeader)
			}
			next.ServeHTTP(w, r)
		})
		limited := perIP(perTenant(perDay(restore)))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del(hiddenAcceptHeader) // never trusted from the client
			if a := r.Header.Get("Accept"); strings.Contains(a, "text/event-stream") {
				r.Header.Set(hiddenAcceptHeader, a)
				r.Header.Set("Accept", "application/json")
			}
			limited.ServeHTTP(w, r)
		})
	}
}

// hiddenAcceptHeader carries the client's Accept past the limiter's streaming skip.
const hiddenAcceptHeader = "X-Mf-Original-Accept"
