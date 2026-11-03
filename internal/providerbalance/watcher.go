package providerbalance

import (
	"context"
	"encoding/json"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	events "github.com/Bengo-Hub/shared-events"
	"github.com/Bengo-Hub/shared-events/balancealert"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/media"
)

// Interval is how often balances are read.
const Interval = 15 * time.Minute

// readTimeout bounds one provider's read, so one slow provider never holds the others.
const readTimeout = 20 * time.Second

// KeySource returns a provider's API key ("" when not configured).
type KeySource func(ctx context.Context, provider string) string

// SettingsSource returns a provider's ProviderSetting values (media/<provider>).
type SettingsSource func(ctx context.Context, provider string) Settings

// Watcher reads the balances and raises the alerts.
type Watcher struct {
	Readers  []Reader
	Keys     KeySource
	Settings SettingsSource
	Redis    redis.UniversalClient
	// Namespace prefixes the Redis keys.
	Namespace string
	// Publish sends the alert event; nil logs the alert only (no NATS configured).
	Publish balancealert.PublishFunc
	// Claim is the once-per-period claim (sharedcache.ClaimOnce in production).
	Claim balancealert.ClaimFunc
	// Balances is the router's gate (media.ProviderBalances in production).
	Balances *media.Balances
	Logger   *zap.Logger
}

func (w *Watcher) log() *zap.Logger {
	if w.Logger == nil {
		return zap.NewNop()
	}
	return w.Logger
}

func (w *Watcher) readingsKey() string { return w.Namespace + ":provider-balances" }

// Run reads at start and then every Interval until ctx ends. Every replica runs it: one
// replica per period does the reads (RunOnce), and every replica loads the shared readings
// into its own router gate.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		w.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick is one period: read once fleet-wide, then load the readings here.
func (w *Watcher) Tick(ctx context.Context) {
	if w.Redis != nil {
		key := sharedcache.PeriodKey(w.Namespace+":provider-balance-watch", Interval)
		if _, err := sharedcache.RunOnce(ctx, w.Redis, w.log(), key, Interval, w.ReadAll); err != nil {
			w.log().Warn("provider balance read failed", zap.Error(err))
		}
		w.load(ctx)
		return
	}
	_ = w.ReadAll(ctx)
}

// ReadAll reads every provider with a key, stores the readings and raises alerts. A read
// failure skips that provider and keeps its previous reading.
func (w *Watcher) ReadAll(ctx context.Context) error {
	readings := map[string]media.BalanceReading{}
	for _, rd := range w.Readers {
		p := rd.Provider()
		key := w.Keys(ctx, p)
		if key == "" {
			continue
		}
		var s Settings
		if w.Settings != nil {
			s = w.Settings(ctx, p)
		}
		rctx, cancel := context.WithTimeout(ctx, readTimeout)
		r, ok, err := rd.Read(rctx, key, s)
		cancel()
		if err != nil {
			w.log().Warn("provider balance not read", zap.String("provider", p), zap.Error(err))
			continue
		}
		if !ok {
			continue
		}
		if r.At.IsZero() {
			r.At = now().UTC()
		}
		mark := LowMark(r.Unit, s)
		br := media.BalanceReading{Remaining: r.Remaining, Unit: r.Unit, LowMark: mark, Estimate: r.Estimate, At: r.At}
		readings[p] = br
		if w.Balances != nil {
			w.Balances.Set(p, br)
		}
		w.alert(ctx, rd, r, mark)
	}
	if w.Redis != nil && len(readings) > 0 {
		if b, err := json.Marshal(readings); err == nil {
			if err := w.Redis.Set(ctx, w.readingsKey(), b, 2*time.Hour).Err(); err != nil {
				w.log().Warn("provider balances not shared", zap.Error(err))
			}
		}
	}
	return nil
}

// load copies the shared readings into this replica's gate.
func (w *Watcher) load(ctx context.Context) {
	if w.Balances == nil {
		return
	}
	b, err := w.Redis.Get(ctx, w.readingsKey()).Bytes()
	if err != nil {
		return
	}
	var readings map[string]media.BalanceReading
	if json.Unmarshal(b, &readings) != nil {
		return
	}
	for p, r := range readings {
		w.Balances.Set(p, r)
	}
}

// alert raises billing.balance.low for the platform owner, once per provider, level and day.
func (w *Watcher) alert(ctx context.Context, rd Reader, r Reading, mark float64) {
	level, low := balancealert.LevelOf(r.Remaining, mark)
	if !low {
		return
	}
	a := balancealert.Alert{
		Scope: balancealert.ScopePlatform, Resource: rd.Provider(), ResourceLabel: rd.Label(), Level: level,
		Remaining: r.Remaining, Unit: r.Unit, Threshold: mark, Period: r.At.UTC().Format("2006-01-02"),
		BuyURL: rd.TopUpURL(), Estimate: r.Estimate, OccurredAt: r.At,
	}
	pub := w.Publish
	if pub == nil {
		pub = func(context.Context, *events.Event) error { return nil }
	}
	sent, err := balancealert.Notify(ctx, w.Claim, 26*time.Hour, a, pub)
	switch {
	case err != nil:
		w.log().Error("provider balance alert not published", zap.String("provider", rd.Provider()), zap.String("level", level), zap.Error(err))
	case sent:
		w.log().Warn("provider balance low", zap.String("provider", rd.Provider()), zap.String("level", level),
			zap.Float64("remaining", r.Remaining), zap.String("unit", r.Unit), zap.Bool("estimate", r.Estimate), zap.Bool("published", w.Publish != nil))
	}
}
