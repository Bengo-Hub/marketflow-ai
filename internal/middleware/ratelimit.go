package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript atomically checks and increments a sorted-set sliding window counter.
// Returns 1 if the request should be rejected (limit exceeded), 0 if allowed.
var slidingWindowScript = redis.NewScript(`
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local min    = now - window

redis.call('ZREMRANGEBYSCORE', key, 0, min)
local count = redis.call('ZCARD', key)
if count >= limit then
  return 1
end
redis.call('ZADD', key, now, now .. '-' .. redis.call('INCR', 'rl:seq'))
redis.call('EXPIRE', key, window + 1)
return 0
`)

// RateLimit returns middleware that enforces per-IP and per-tenant sliding-window limits.
// ipLimit: max requests per IP per 60 s.
// tenantLimit: max requests per tenant slug per 60 s.
// Pass 0 for either to skip that check.
func RateLimit(rdb *redis.Client, ipLimit, tenantLimit int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			now := float64(time.Now().UnixMilli())
			window := float64(60_000) // 60 s in ms

			ip := realIP(r)
			tenant := r.Header.Get("X-Tenant-ID")
			if tenant == "" {
				tenant = "unknown"
			}

			if ipLimit > 0 {
				key := "rl:ip:" + ip
				if rejected := runScript(ctx, rdb, key, now, window, ipLimit); rejected {
					rateLimitResponse(w, "Too many requests from your IP. Please wait a moment.")
					return
				}
			}

			if tenantLimit > 0 && tenant != "unknown" {
				key := "rl:tenant:" + tenant
				if rejected := runScript(ctx, rdb, key, now, window, tenantLimit); rejected {
					rateLimitResponse(w, "Too many requests for this tenant. Please wait a moment.")
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

func runScript(ctx context.Context, rdb *redis.Client, key string, now, window float64, limit int) bool {
	result, err := slidingWindowScript.Run(ctx, rdb,
		[]string{key},
		fmt.Sprintf("%.0f", now),
		fmt.Sprintf("%.0f", window),
		limit,
	).Int()
	if err != nil {
		// Redis error — fail open (allow the request)
		return false
	}
	return result == 1
}

func realIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first (leftmost) address — that's the real client
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	// Strip port from RemoteAddr
	addr := r.RemoteAddr
	if idx := strings.LastIndexByte(addr, ':'); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

func rateLimitResponse(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(w, `{"error":%q}`, msg)
}
