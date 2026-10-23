package decide

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/bengobox/marketflow-ai/internal/breaker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// PrimarySource resolves the primary (Jev) adapter from platform settings. ok is
// false when it is not configured.
type PrimarySource func(ctx context.Context) (Adapter, bool)

// CeilingSource returns the monthly USD ceiling for paid decisions (0 = none).
type CeilingSource func(ctx context.Context) float64

// SpendTracker records paid decision spend per calendar month.
type SpendTracker interface {
	Spent(ctx context.Context, month string) float64
	Add(ctx context.Context, month string, usd float64)
}

// Config configures a Decider.
type Config struct {
	Primary  PrimarySource
	Fallback Adapter
	Ceiling  CeilingSource
	Spend    SpendTracker
	Logger   *zap.Logger
}

// Decider runs decisions: masks PII, tries the primary adapter while it is
// configured, healthy and under the monthly ceiling, then the fallback.
type Decider struct {
	cfg    Config
	logger *zap.Logger
	nowFn  func() time.Time
	br     *breaker.Set // one breaker, for the primary adapter
}

// primaryKey is the primary adapter's breaker key.
const primaryKey = "primary"

const (
	primaryFailures = 3
	primaryCooldown = 2 * time.Minute
	// authCooldown applies when the key is rejected or unpaid: retrying soon
	// cannot help, so decisions go straight to the fallback for a while.
	authCooldown  = 15 * time.Minute
	maxStateBytes = 48 << 10 // well under Jev's 64k-token limit
	// storeTimeout bounds the spend-counter reads and writes; Redis is a cost
	// guard here and must never hold up a decision.
	storeTimeout = 300 * time.Millisecond
)

// Budgets: Jev normally answers in well under a second, so a slow call is cut
// early and the fallback still has time. The whole decision, fallback included,
// ends within decideBudget; past it the caller takes its safe branch.
var (
	primaryBudget = 3 * time.Second
	decideBudget  = 8 * time.Second
)

// New builds a Decider.
func New(cfg Config) *Decider {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	d := &Decider{cfg: cfg, logger: logger.Named("decide"), nowFn: time.Now,
		br: breaker.New(primaryFailures, primaryCooldown, authCooldown)}
	d.br.Now = func() time.Time { return d.nowFn() } // one clock, replaceable in tests
	return d
}

var metricDecisions = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "marketflow_ai_decisions_total",
	Help: "Typed decisions by name, adapter and outcome.",
}, []string{"decision", "adapter", "outcome"})

var metricDecideCost = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "marketflow_ai_decide_cost_usd_total",
	Help: "Estimated USD spent on decisions, by adapter.",
}, []string{"adapter"})

// Decide answers questions about state. name labels the decision in metrics and
// logs ("route", "injection", "qc"). An error means no adapter could answer; the
// caller must then take its safe branch.
func (d *Decider) Decide(ctx context.Context, name, state string, questions map[string]Question) (Answers, Usage, error) {
	if len(questions) == 0 {
		return nil, Usage{}, fmt.Errorf("decide: no questions")
	}
	for id, q := range questions {
		if err := q.Validate(); err != nil {
			return nil, Usage{}, fmt.Errorf("%s/%s: %w", name, id, err)
		}
	}
	state = MaskPII(state)
	if len(state) > maxStateBytes {
		state = state[:maxStateBytes]
	}
	ctx, cancel := context.WithTimeout(ctx, decideBudget)
	defer cancel()

	if primary, ok := d.primary(ctx); ok {
		pctx, pcancel := context.WithTimeout(ctx, primaryBudget)
		answers, usage, err := primary.Decide(pctx, state, questions)
		pcancel()
		if err == nil {
			d.br.Succeeded(primaryKey)
			d.record(ctx, usage)
			metricDecisions.WithLabelValues(name, usage.Adapter, "ok").Inc()
			return answers, usage, nil
		}
		// A slow Jev (its own 3s budget) counts against it; the decision's
		// whole budget running out is the caller giving up and does not.
		d.br.Failed(primaryKey, err, ctx.Err() != nil)
		metricDecisions.WithLabelValues(name, primary.Name(), "error").Inc()
		d.logger.Warn("primary decide failed, using fallback", zap.String("decision", name), zap.Error(err))
	}
	if d.cfg.Fallback == nil {
		return nil, Usage{}, fmt.Errorf("decide: no adapter available for %s", name)
	}
	if ctx.Err() != nil {
		return nil, Usage{}, fmt.Errorf("decide %s: out of time: %w", name, ctx.Err())
	}
	answers, usage, err := d.cfg.Fallback.Decide(ctx, state, questions)
	if err != nil {
		metricDecisions.WithLabelValues(name, d.cfg.Fallback.Name(), "error").Inc()
		return nil, Usage{}, err
	}
	metricDecisions.WithLabelValues(name, usage.Adapter, "ok").Inc()
	metricDecideCost.WithLabelValues(usage.Adapter).Add(usage.CostUSD)
	return answers, usage, nil
}

// primary returns the primary adapter when it is configured, its breaker is
// closed and the month's spend is under the ceiling.
func (d *Decider) primary(ctx context.Context) (Adapter, bool) {
	if d.cfg.Primary == nil {
		return nil, false
	}
	if !d.br.Admit(primaryKey) {
		return nil, false
	}
	if d.cfg.Ceiling != nil && d.cfg.Spend != nil {
		sctx, cancel := context.WithTimeout(ctx, storeTimeout)
		over := false
		if ceiling := d.cfg.Ceiling(sctx); ceiling > 0 && d.cfg.Spend.Spent(sctx, d.month()) >= ceiling {
			over = true
		}
		cancel()
		if over {
			return nil, false
		}
	}
	return d.cfg.Primary(ctx)
}

func (d *Decider) record(ctx context.Context, u Usage) {
	metricDecideCost.WithLabelValues(u.Adapter).Add(u.CostUSD)
	if d.cfg.Spend != nil && u.CostUSD > 0 {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		d.cfg.Spend.Add(sctx, d.month(), u.CostUSD)
		cancel()
	}
}

func (d *Decider) month() string { return d.nowFn().UTC().Format("2006-01") }

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// Phone-like runs: optional +, then 9 to 15 digits with spaces, dots or dashes.
	phoneRe = regexp.MustCompile(`\+?\d[\d\s.\-]{7,18}\d`)
)

// MaskPII replaces emails and phone numbers, so lead contact details never reach
// a decision provider.
func MaskPII(s string) string {
	s = emailRe.ReplaceAllString(s, "[email]")
	return phoneRe.ReplaceAllStringFunc(s, func(m string) string {
		digits := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits < 9 {
			return m // amounts and short numbers stay
		}
		return "[phone]"
	})
}

// RedisSpend tracks monthly spend in Redis, shared by every replica.
type RedisSpend struct {
	Client    *redis.Client
	Namespace string
}

func (r RedisSpend) key(month string) string { return r.Namespace + ":decide:spend:" + month }

// Spent implements SpendTracker. A read failure reports zero, so a Redis outage
// never blocks decisions; the ceiling is a cost guard, not a security control.
func (r RedisSpend) Spent(ctx context.Context, month string) float64 {
	v, err := r.Client.Get(ctx, r.key(month)).Result()
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(v, 64)
	return f
}

// Add implements SpendTracker.
func (r RedisSpend) Add(ctx context.Context, month string, usd float64) {
	k := r.key(month)
	if err := r.Client.IncrByFloat(ctx, k, usd).Err(); err == nil {
		r.Client.Expire(ctx, k, 40*24*time.Hour)
	}
}
