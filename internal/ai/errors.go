package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// StatusError is a non-200 reply from a model provider. Callers use the code to
// tell our own bad requests (4xx) from provider trouble (429, 5xx).
type StatusError struct {
	Provider string
	Code     int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: API error %d: %s", e.Provider, e.Code, e.Body)
}

// StatusCode returns the provider status code wrapped in err, or 0.
func StatusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// Retryable reports whether another provider might succeed where this one
// failed: timeouts, transport errors, rate limits and server errors. Plain 4xx
// replies (bad request, unknown model) are not provider outages.
func Retryable(err error) bool {
	switch c := StatusCode(err); {
	case c == 0:
		return true
	case c == http.StatusTooManyRequests, c == http.StatusRequestTimeout, c >= 500:
		return true
	case c == http.StatusUnauthorized, c == http.StatusForbidden, c == http.StatusPaymentRequired:
		return true // this key is unusable; another provider may still work
	default:
		return false
	}
}

// AuthFailure reports a rejected or unpaid key: retrying the same provider soon
// cannot help.
func AuthFailure(err error) bool {
	c := StatusCode(err)
	return c == http.StatusUnauthorized || c == http.StatusForbidden || c == http.StatusPaymentRequired
}

type failFastKey struct{}

// WithFailFast tells a client not to wait and retry inside one call (for example
// on a 429), because the caller has another provider to try next.
func WithFailFast(ctx context.Context) context.Context {
	return context.WithValue(ctx, failFastKey{}, true)
}

// FailFast reports whether the caller asked for no in-call retries.
func FailFast(ctx context.Context) bool {
	v, _ := ctx.Value(failFastKey{}).(bool)
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
