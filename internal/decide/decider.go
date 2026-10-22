package decide

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

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

	mu        sync.Mutex
	fails     int
	openUntil time.Time
}

const (
	primaryFailures = 3
	primaryCooldown = 2 * time.Minute
	maxStateBytes   = 48 << 10 // well under Jev's 64k-token limit
)

// New builds a Decider.
func New(cfg Config) *Decider {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Decider{cfg: cfg, logger: logger.Named("decide"), nowFn: time.Now}
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

	if primary, ok := d.primary(ctx); ok {
		answers, usage, err := primary.Decide(ctx, state, questions)
		if err == nil {
			d.succeed()
			d.record(ctx, usage)
			metricDecisions.WithLabelValues(name, usage.Adapter, "ok").Inc()
			return answers, usage, nil
		}
		d.fail()
		metricDecisions.WithLabelValues(name, primary.Name(), "error").Inc()
		d.logger.Warn("primary decide failed, using fallback", zap.String("decision", name), zap.Error(err))
	}
	if d.cfg.Fallback == nil {
		return nil, Usage{}, fmt.Errorf("decide: no adapter available for %s", name)
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
	d.mu.Lock()
	open := d.nowFn().Before(d.openUntil)
	d.mu.Unlock()
	if open {
		return nil, false
	}
	if d.cfg.Ceiling != nil && d.cfg.Spend != nil {
		if ceiling := d.cfg.Ceiling(ctx); ceiling > 0 && d.cfg.Spend.Spent(ctx, d.month()) >= ceiling {
			return nil, false
		}
	}
	return d.cfg.Primary(ctx)
}

func (d *Decider) record(ctx context.Context, u Usage) {
	metricDecideCost.WithLabelValues(u.Adapter).Add(u.CostUSD)
	if d.cfg.Spend != nil && u.CostUSD > 0 {
		d.cfg.Spend.Add(ctx, d.month(), u.CostUSD)
	}
}

func (d *Decider) month() string { return d.nowFn().UTC().Format("2006-01") }

func (d *Decider) fail() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fails++
	if d.fails >= primaryFailures {
		d.openUntil = d.nowFn().Add(primaryCooldown)
		d.fails = 0
	}
}

func (d *Decider) succeed() {
	d.mu.Lock()
	d.fails = 0
	d.mu.Unlock()
}

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
