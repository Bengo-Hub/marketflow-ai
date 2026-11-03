package media

import (
	"context"
	"sync"
	"time"
)

// Provider balances, as the balance watcher last read them (internal/providerbalance). The
// router's available() consults them: a provider that is below its low-water mark is skipped
// for any output that costs more than what remains, so a job moves to the fallback route
// instead of failing half way. A provider with no reading, or a stale one, is allowed: a
// watcher outage must never stop renders.

// BalanceReading is one provider's balance in its own unit (usd for fal and Higgsfield,
// characters for ElevenLabs).
type BalanceReading struct {
	Remaining float64
	Unit      string
	LowMark   float64
	Estimate  bool
	At        time.Time
}

// Low reports whether the reading is at or below its mark.
func (b BalanceReading) Low() bool {
	return b.Remaining <= 0 || (b.LowMark > 0 && b.Remaining <= b.LowMark)
}

// balanceStale is how long a reading counts; the watcher reads every 15 minutes.
const balanceStale = time.Hour

// Balances holds the latest reading per provider.
type Balances struct {
	mu  sync.RWMutex
	m   map[string]BalanceReading
	now func() time.Time
}

// NewBalances returns an empty set (every provider allowed).
func NewBalances() *Balances { return &Balances{m: map[string]BalanceReading{}, now: time.Now} }

var providerBalances = NewBalances()

// ProviderBalances is the process-wide set the watcher writes and the pipelines read.
func ProviderBalances() *Balances { return providerBalances }

// Set records a reading.
func (b *Balances) Set(provider string, r BalanceReading) {
	if r.At.IsZero() {
		r.At = b.now()
	}
	b.mu.Lock()
	b.m[provider] = r
	b.mu.Unlock()
}

// Get returns the last reading.
func (b *Balances) Get(provider string) (BalanceReading, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	r, ok := b.m[provider]
	return r, ok
}

// Allows reports whether an output costing cost (in the provider's unit) may go to provider.
func (b *Balances) Allows(provider string, cost float64) bool {
	if b == nil {
		return true
	}
	r, ok := b.Get(provider)
	if !ok || b.now().Sub(r.At) > balanceStale {
		return true
	}
	if !r.Low() {
		return true
	}
	return cost <= r.Remaining && r.Remaining > 0
}

// usable is the available() check every pipeline shares: a key is configured and the
// provider's balance covers the output.
func usable(ctx context.Context, keys KeySource, rt Route, cost float64) bool {
	return keys(ctx, rt.Provider) != "" && providerBalances.Allows(rt.Provider, cost)
}
