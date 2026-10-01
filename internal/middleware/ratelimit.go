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
func PublicChatLimits(l *ratelimit.Limiter, ipLimit, tenantLimit int) func(http.Handler) http.Handler {
	perIP := l.MiddlewareWith(ratelimit.IPKey, ratelimit.Options{Name: "public-chat-ip", Limit: ipLimit, Window: time.Minute})
	perTenant := l.MiddlewareWith(func(r *http.Request) string {
		t := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tenant-ID")))
		if t == "" || len(t) > 64 {
			return ""
		}
		return "tenant:" + t
	}, ratelimit.Options{Name: "public-chat-tenant", Limit: tenantLimit, Window: time.Minute})
	return func(next http.Handler) http.Handler {
		return perIP(perTenant(next))
	}
}
