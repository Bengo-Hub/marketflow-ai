package support

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// Backoff bounds one retried call: at most Attempts tries, each limited by PerTry, with an
// exponential delay between tries (Base doubled each time, capped at Max) and full jitter. The
// caller's context is the total budget, so a retry never runs past it.
type Backoff struct {
	Attempts int
	Base     time.Duration
	Max      time.Duration
	PerTry   time.Duration
}

// DefaultBackoff is the support calls' default: three tries of up to 5 seconds, 300ms then up
// to 600ms between them. With jitter the whole call stays under about 16 seconds, and the step
// budget in Router cuts it shorter.
var DefaultBackoff = Backoff{Attempts: 3, Base: 300 * time.Millisecond, Max: 2 * time.Second, PerTry: 5 * time.Second}

// statusError is a downstream answer that was not a success. Retryable says a later try may
// succeed (429 or 5xx); any other 4xx is our own request being wrong, so it is never retried.
type statusError struct {
	Service string
	Status  int
	Body    string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s answered %d: %s", e.Service, e.Status, e.Body)
}

func (e *statusError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// errPermanent wraps an error that must not be retried (a bad request, a missing setting).
type errPermanent struct{ err error }

func (e errPermanent) Error() string { return e.err.Error() }
func (e errPermanent) Unwrap() error { return e.err }

func permanent(err error) error { return errPermanent{err} }

// retryable reports whether another try may help: network errors, timeouts of one try, 429 and
// 5xx. A cancelled or expired caller context is never retried.
func retryable(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var p errPermanent
	if errors.As(err, &p) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.retryable()
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// sleep waits d or until ctx ends; false means ctx ended first.
var sleep = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// do runs fn up to b.Attempts times. Each try gets its own deadline (b.PerTry, never past the
// caller's). It stops at once on success, on an error that is not retryable, or when the caller's
// budget cannot fit the next delay. The last error is returned.
func (b Backoff) do(ctx context.Context, fn func(ctx context.Context) error) error {
	attempts := b.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		tryCtx, cancel := ctx, context.CancelFunc(func() {})
		if b.PerTry > 0 {
			tryCtx, cancel = context.WithTimeout(ctx, b.PerTry)
		}
		err = fn(tryCtx)
		cancel()
		if err == nil || i == attempts-1 || !retryable(ctx, err) {
			return err
		}
		delay := b.delay(i)
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < delay+250*time.Millisecond {
			return err
		}
		if !sleep(ctx, delay) {
			return err
		}
	}
	return err
}

// delay is the wait before try i+2: full jitter over Base*2^i, capped at Max.
func (b Backoff) delay(i int) time.Duration {
	ceiling := b.Base << i
	if b.Max > 0 && (ceiling > b.Max || ceiling <= 0) {
		ceiling = b.Max
	}
	if ceiling <= 0 {
		return 0
	}
	// Half fixed, half random, so two failing calls never retry in step and the wait is never zero.
	half := ceiling / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}
