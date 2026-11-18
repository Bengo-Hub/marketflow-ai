package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bengo-Hub/shared-ratelimit"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func serveN(h http.Handler, n int, accept, tenant string) (last int, sawStream bool) {
	for i := 0; i < n; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/public/chat", nil)
		r.RemoteAddr = "203.0.113.7:1234"
		r.Header.Set("Accept", accept)
		r.Header.Set("X-Tenant-ID", tenant)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		last = w.Code
		if w.Header().Get("X-Saw-Accept") == "text/event-stream" {
			sawStream = true
		}
	}
	return last, sawStream
}

// Security audit 2026-11-10 (MF #10): the shared limiter skips requests that ask for an event
// stream, so a public chat request with "Accept: text/event-stream" bypassed every public chat
// limit and could spend a tenant's AI credits without bound. The limits now apply to streams,
// and the handler still sees the stream request.
func TestPublicChatLimitsApplyToStreams(t *testing.T) {
	mr := miniredis.RunT(t)
	l := ratelimit.NewLimiter(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil, "test")
	h := PublicChatLimits(l, nil, 3, 100, 0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Saw-Accept", r.Header.Get("Accept"))
		w.WriteHeader(http.StatusOK)
	}))
	code, saw := serveN(h, 4, "text/event-stream", "t1")
	if code != http.StatusTooManyRequests {
		t.Fatalf("4th streamed request in a minute from one IP: %d, want 429", code)
	}
	if !saw {
		t.Fatal("the handler must still see Accept: text/event-stream")
	}
}

// MF #10: a daily ceiling per tenant on top of the per-minute limits.
func TestPublicChatDailyTenantCap(t *testing.T) {
	mr := miniredis.RunT(t)
	l := ratelimit.NewLimiter(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil, "test")
	h := PublicChatLimits(l, nil, 1000, 1000, 5)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	if code, _ := serveN(h, 5, "application/json", "t2"); code != http.StatusOK {
		t.Fatalf("within the daily cap: %d", code)
	}
	if code, _ := serveN(h, 1, "application/json", "t2"); code != http.StatusTooManyRequests {
		t.Fatalf("over the daily cap: %d, want 429", code)
	}
}

var (
	tenantA = uuid.MustParse("3f2b8c1e-7d4a-4e5b-9c6d-0a1b2c3d4e5f")
	tenantB = uuid.MustParse("9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b")
)

// fakeResolve resolves the way sessions.Service.ResolveTenantID does: anything uuid.Parse
// accepts is looked up as an id, anything else as a slug.
func fakeResolve(_ context.Context, raw string) (uuid.UUID, bool) {
	if id, err := uuid.Parse(raw); err == nil {
		if id == tenantA || id == tenantB {
			return id, true
		}
		return uuid.Nil, false
	}
	switch raw {
	case "acme":
		return tenantA, true
	case "globex":
		return tenantB, true
	}
	return uuid.Nil, false
}

// serveFrom sends one public chat request from remoteAddr with the given X-Tenant-ID.
func serveFrom(h http.Handler, remoteAddr, tenant string) int {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/public/chat", nil)
	r.RemoteAddr = remoteAddr
	r.Header.Set("Accept", "application/json")
	if tenant != "" {
		r.Header.Set("X-Tenant-ID", tenant)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// OWASP review 2026-11-18: the tenant limits keyed on the header text, so each spelling of one
// tenant's UUID and its slug was a separate daily bucket. All of them now share the bucket of
// the tenant they resolve to.
func TestPublicChatDailyCapSharedAcrossTenantSpellings(t *testing.T) {
	mr := miniredis.RunT(t)
	l := ratelimit.NewLimiter(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil, "test")
	h := PublicChatLimits(l, fakeResolve, 1000, 1000, 5)(okHandler())

	s := tenantA.String()
	spellings := []string{
		s,                               // hyphenated
		strings.ReplaceAll(s, "-", ""),  // 32 hex digits
		"{" + strings.ToUpper(s) + "}", // braced, upper case
		"urn:uuid:" + s,                 // URN
		"acme",                          // slug
	}
	for _, sp := range spellings {
		if code := serveFrom(h, "203.0.113.7:1234", sp); code != http.StatusOK {
			t.Fatalf("%q within the daily cap: %d", sp, code)
		}
	}
	for _, sp := range spellings {
		if code := serveFrom(h, "203.0.113.7:1234", sp); code != http.StatusTooManyRequests {
			t.Fatalf("%q after the tenant's daily cap is spent: %d, want 429", sp, code)
		}
	}
	// Another tenant has its own bucket.
	if code := serveFrom(h, "203.0.113.7:1234", "globex"); code != http.StatusOK {
		t.Fatalf("a different tenant must not share the bucket: %d", code)
	}
	if code := serveFrom(h, "203.0.113.7:1234", tenantB.String()); code != http.StatusOK {
		t.Fatalf("a different tenant must not share the bucket: %d", code)
	}
}

// A header that does not resolve never mints a bucket of its own: every unknown header from
// one client network counts against one "unresolved" bucket.
func TestPublicChatUnresolvedTenantsShareOneBucket(t *testing.T) {
	mr := miniredis.RunT(t)
	l := ratelimit.NewLimiter(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil, "test")
	h := PublicChatLimits(l, fakeResolve, 1000, 1000, 3)(okHandler())

	for i, junk := range []string{"nope-1", "nope-2", uuid.NewString()} {
		if code := serveFrom(h, "203.0.113.7:1234", junk); code != http.StatusOK {
			t.Fatalf("unresolved header %d within the cap: %d", i, code)
		}
	}
	if code := serveFrom(h, "203.0.113.7:1234", "nope-4"); code != http.StatusTooManyRequests {
		t.Fatalf("a fresh unknown header must not get a fresh bucket: %d, want 429", code)
	}
	if code := serveFrom(h, "203.0.113.7:1234", strings.Repeat("x", 65)); code != http.StatusTooManyRequests {
		t.Fatalf("an over-long header must count as unresolved: %d, want 429", code)
	}
	// The real tenant is not affected by the junk.
	if code := serveFrom(h, "203.0.113.7:1234", "acme"); code != http.StatusOK {
		t.Fatalf("a real tenant after junk headers: %d", code)
	}
}

// OWASP review 2026-11-18: an IPv6 client holds a whole /64, so the per-IP limit groups it.
func TestPublicChatIPv6GroupedBySlash64(t *testing.T) {
	mr := miniredis.RunT(t)
	l := ratelimit.NewLimiter(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil, "test")
	h := PublicChatLimits(l, fakeResolve, 3, 1000, 0)(okHandler())

	for _, addr := range []string{"[2001:db8:1:2::1]:1234", "[2001:db8:1:2::2]:1234", "[2001:db8:1:2:ffff:ffff:ffff:fffe]:1234"} {
		if code := serveFrom(h, addr, "acme"); code != http.StatusOK {
			t.Fatalf("%s within the per-IP limit: %d", addr, code)
		}
	}
	if code := serveFrom(h, "[2001:db8:1:2:abcd::9]:1234", "acme"); code != http.StatusTooManyRequests {
		t.Fatalf("a new address in the same /64 must share the bucket: %d, want 429", code)
	}
	if code := serveFrom(h, "[2001:db8:1:3::1]:1234", "acme"); code != http.StatusOK {
		t.Fatalf("a different /64 has its own bucket: %d", code)
	}
}

func TestClientNet(t *testing.T) {
	cases := []struct{ remote, want string }{
		{"203.0.113.7:1234", "203.0.113.7"},
		{"203.0.113.8:1234", "203.0.113.8"},
		{"[::ffff:203.0.113.7]:1234", "203.0.113.7"},
		{"[2001:db8:1:2::1]:1234", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:1234", "2001:db8:1:2::/64"},
		{"[2001:db8:1:3::1]:1234", "2001:db8:1:3::/64"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if got := ClientNet(r); got != c.want {
			t.Errorf("ClientNet(%s) = %q, want %q", c.remote, got, c.want)
		}
	}
}
