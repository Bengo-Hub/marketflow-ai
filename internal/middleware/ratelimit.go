package middleware

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"time"

	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"github.com/google/uuid"
)

// TenantResolver turns the raw X-Tenant-ID header (a UUID in any spelling, or a slug) into the
// real tenant UUID. It is the same resolver the chat handler bills with
// (sessions.Service.ResolveTenantID); false means no such tenant.
type TenantResolver func(ctx context.Context, raw string) (uuid.UUID, bool)

// PublicChatLimits limits the unauthenticated public chat endpoint per client IP and per
// tenant, on shared-ratelimit (GCRA in Redis, exact across replicas, per-pod fallback while
// Redis is down). It replaces a local Lua sliding window whose keys had no service prefix,
// which kept a global "rl:seq" counter outside its declared keys, and which trusted the
// first X-Forwarded-For entry for the client IP.
//
// The tenant comes from the X-Tenant-ID header by design (the embeddable widget has no
// login), so the per-tenant cap is caller-supplied: the per-IP limit is what makes it costly
// to burn another tenant's allowance.
//
// The tenant limits key on the resolved tenant UUID in its canonical form, not on the header
// text (OWASP review 2026-11-18). The header used to be the key, so the hyphenated UUID, the
// bare 32 hex digits, the {braced} and urn:uuid: spellings and the slug of one business were
// five separate daily buckets, and anyone could empty the slug bucket the real widget uses.
// A header that does not resolve (unknown, or longer than 64 characters) never gets a bucket
// of its own: it counts against one "unresolved" bucket for the client's network. resolve may
// be nil (tests, no database); the header is then only canonicalized when it parses as a UUID.
//
// The per-IP limit buckets an IPv6 client by its /64, since one host usually holds a whole
// /64 and could otherwise take a fresh bucket per address. IPv4 keys stay the exact address.
//
// dailyTenantLimit (0 = none) is a per-tenant ceiling per 24 hours, so a bot cannot spend a
// tenant's AI credits all day at the per-minute rate (audit MF #10). The shared limiter skips
// requests that ask for an event stream; the stream hint is hidden while the limits are checked
// and restored for the handler, so streamed chats are limited too.
func PublicChatLimits(l *ratelimit.Limiter, resolve TenantResolver, ipLimit, tenantLimit, dailyTenantLimit int) func(http.Handler) http.Handler {
	// The tenant key is worked out once per request, after the per-IP limit has passed (so a
	// flood of junk headers is held by the IP limit before any database lookup), and read from
	// the request context by both tenant limits.
	tenantKey := func(r *http.Request) string {
		k, _ := r.Context().Value(tenantKeyCtx{}).(string)
		return k
	}
	perIP := l.MiddlewareWith(ipKey, ratelimit.Options{Name: "public-chat-ip", Limit: ipLimit, Window: time.Minute})
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
		tenantLimited := perTenant(perDay(restore))
		resolveTenant := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), tenantKeyCtx{}, resolvedTenantKey(r, resolve))
			tenantLimited.ServeHTTP(w, r.WithContext(ctx))
		})
		limited := perIP(resolveTenant)
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

// tenantKeyCtx carries the resolved tenant key from the resolve step to the tenant limits.
type tenantKeyCtx struct{}

// resolvedTenantKey is the tenant limits' subject: "tenant:<canonical uuid>" for a header that
// resolves, "unresolved:<client network>" for one that does not, and "" (no tenant limit, the
// per-IP limit still holds) when the header is missing. The raw header goes to the resolver
// untouched, so it resolves exactly as it does in the chat handler.
func resolvedTenantKey(r *http.Request, resolve TenantResolver) string {
	raw := r.Header.Get("X-Tenant-ID")
	t := strings.TrimSpace(raw)
	if t == "" {
		return ""
	}
	if len(t) > 64 {
		return "unresolved:" + ClientNet(r)
	}
	if resolve == nil {
		if id, err := uuid.Parse(t); err == nil {
			return "tenant:" + id.String()
		}
		return "tenant:" + strings.ToLower(t)
	}
	if id, ok := resolve(r.Context(), raw); ok && id != uuid.Nil {
		return "tenant:" + id.String()
	}
	return "unresolved:" + ClientNet(r)
}

// ipKey is the per-IP limit's subject, with IPv6 clients grouped by /64.
func ipKey(r *http.Request) string {
	return "ip:" + ClientNet(r)
}

// ClientNet is the client's network for rate limiting: the exact address for IPv4 (and an
// IPv4-mapped IPv6 address), the /64 prefix for IPv6. The address itself comes from
// ratelimit.ClientIP, which trusts only the ingress-set headers. An address that does not
// parse is returned as it is.
func ClientNet(r *http.Request) string {
	ip := ratelimit.ClientIP(r)
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return p.String()
}

// hiddenAcceptHeader carries the client's Accept past the limiter's streaming skip.
const hiddenAcceptHeader = "X-Mf-Original-Accept"
