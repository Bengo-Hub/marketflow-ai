// Package breaker is the circuit breaker shared by the model router and the
// media router: one breaker per route key, opened by provider failures and
// recovered by a single half-open probe.
//
// Rules (the "fallbacks that never stall" policy, docs/agents-architecture.md):
//   - our own bad requests (a 4xx other than auth) say nothing about the
//     provider's health and close the breaker, since the provider answered;
//   - a caller that gave up (its own deadline or cancel) does not count;
//   - a rejected or unpaid key opens the breaker at once, for longer;
//   - after the cooldown exactly one caller probes; claiming the probe opens a
//     new window, so a probe that never runs expires instead of blocking.
package breaker

import (
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Set holds the breakers of one router. It is safe for concurrent use.
type Set struct {
	Failures     int           // consecutive failures that open a breaker
	Cooldown     time.Duration // how long an opened breaker stays open
	AuthCooldown time.Duration // how long a rejected key keeps it open
	// Now is the clock; tests replace it.
	Now func() time.Time

	mu sync.Mutex
	m  map[string]*state
}

type state struct {
	fails     int
	openUntil time.Time
	probing   bool
}

// New builds a Set.
func New(failures int, cooldown, authCooldown time.Duration) *Set {
	return &Set{Failures: failures, Cooldown: cooldown, AuthCooldown: authCooldown, Now: time.Now, m: map[string]*state{}}
}

// Admit reports whether key may be tried now: its breaker is closed, or its
// cooldown has passed and this caller takes the single half-open probe.
func (s *Set) Admit(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	now := s.Now()
	switch {
	case b == nil || b.openUntil.IsZero():
		return true
	case now.Before(b.openUntil):
		return false
	default:
		b.probing = true
		b.openUntil = now.Add(s.Cooldown)
		return true
	}
}

// Succeeded closes key's breaker.
func (s *Set) Succeeded(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// Failed records a failed attempt on key. callerGaveUp is true when the
// caller's own context ended (deadline or cancel).
func (s *Set) Failed(key string, err error, callerGaveUp bool) {
	if !callerGaveUp && !ai.Retryable(err) {
		// The provider answered, so it is up; the request itself was wrong.
		s.Succeeded(key)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	if callerGaveUp {
		if b != nil && b.probing {
			b.probing, b.openUntil = false, s.Now() // the next caller probes
		}
		return
	}
	if b == nil {
		b = &state{}
		s.m[key] = b
	}
	now := s.Now()
	switch {
	case ai.AuthFailure(err):
		b.openUntil, b.fails = now.Add(s.AuthCooldown), 0
	case b.probing || !b.openUntil.IsZero():
		// The half-open probe failed: open again for a full cooldown.
		b.openUntil, b.fails = now.Add(s.Cooldown), 0
	default:
		b.fails++
		if b.fails >= s.Failures {
			b.openUntil, b.fails = now.Add(s.Cooldown), 0
		}
	}
	b.probing = false
}

// Tripped reports whether key's breaker has been opened (open now, or past its
// cooldown and waiting for a probe).
func (s *Set) Tripped(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	return b != nil && !b.openUntil.IsZero()
}

// Probing reports whether a caller has claimed key's half-open probe.
func (s *Set) Probing(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[key]
	return b != nil && b.probing
}

// OpenUntil opens key's breaker until t (tests and manual operations).
func (s *Set) OpenUntil(key string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = &state{openUntil: t}
}
