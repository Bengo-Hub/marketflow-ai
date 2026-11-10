package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/shared-ratelimit"
	"github.com/alicebob/miniredis/v2"
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
	h := PublicChatLimits(l, 3, 100, 0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	h := PublicChatLimits(l, 1000, 1000, 5)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	if code, _ := serveN(h, 5, "application/json", "t2"); code != http.StatusOK {
		t.Fatalf("within the daily cap: %d", code)
	}
	if code, _ := serveN(h, 1, "application/json", "t2"); code != http.StatusTooManyRequests {
		t.Fatalf("over the daily cap: %d, want 429", code)
	}
}
