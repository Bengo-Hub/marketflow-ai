package llm

import (
	"context"
	"sync"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Call records one completed model call for metering and traces.
type Call struct {
	Class     Class    `json:"class"`
	Provider  string   `json:"provider"`
	Model     string   `json:"model"`
	Usage     ai.Usage `json:"usage"`
	CostUSD   float64  `json:"cost_usd"`
	LatencyMs int64    `json:"latency_ms"`
}

// Meter accumulates the calls made while serving one request or run. Attach it
// with WithMeter; the router appends to it, so a run's total spend is known
// without threading usage through every function.
type Meter struct {
	mu    sync.Mutex
	calls []Call
}

type meterKey struct{}

// WithMeter returns a context carrying m.
func WithMeter(ctx context.Context, m *Meter) context.Context {
	return context.WithValue(ctx, meterKey{}, m)
}

// MeterFrom returns the meter on ctx, or nil.
func MeterFrom(ctx context.Context) *Meter {
	m, _ := ctx.Value(meterKey{}).(*Meter)
	return m
}

// Add records a call.
func (m *Meter) Add(c Call) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.calls = append(m.calls, c)
	m.mu.Unlock()
}

// Calls returns a copy of the recorded calls.
func (m *Meter) Calls() []Call {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Call, len(m.calls))
	copy(out, m.calls)
	return out
}

// Totals sums tokens and estimated cost.
func (m *Meter) Totals() (tokens int, usd float64) {
	for _, c := range m.Calls() {
		tokens += c.Usage.TotalTokens
		usd += c.CostUSD
	}
	return tokens, usd
}

// EstimateUSD prices a call from the target's list prices. Cached reads bill at
// 0.1x input and cache writes at 1.25x (Anthropic 5-minute cache).
func EstimateUSD(t Target, u ai.Usage) float64 {
	uncached := u.PromptTokens - u.CacheReadTokens - u.CacheWriteTokens
	if uncached < 0 {
		uncached = 0
	}
	in := float64(uncached) + 0.1*float64(u.CacheReadTokens) + 1.25*float64(u.CacheWriteTokens)
	return (in*t.InputUSDPerM + float64(u.CompletionTokens)*t.OutputUSDPerM) / 1e6
}

var (
	metricTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marketflow_ai_llm_tokens_total",
		Help: "Tokens used by model calls, by class, provider, model and kind (prompt, completion, cache_read).",
	}, []string{"class", "provider", "model", "kind"})
	metricCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marketflow_ai_llm_cost_usd_total",
		Help: "Estimated USD cost of model calls from the routing policy's list prices.",
	}, []string{"class", "provider", "model"})
	metricCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marketflow_ai_llm_calls_total",
		Help: "Model calls by class, provider, model and outcome (ok, error, skipped).",
	}, []string{"class", "provider", "model", "outcome"})
)

func observe(c Call) {
	metricTokens.WithLabelValues(string(c.Class), c.Provider, c.Model, "prompt").Add(float64(c.Usage.PromptTokens))
	metricTokens.WithLabelValues(string(c.Class), c.Provider, c.Model, "completion").Add(float64(c.Usage.CompletionTokens))
	if c.Usage.CacheReadTokens > 0 {
		metricTokens.WithLabelValues(string(c.Class), c.Provider, c.Model, "cache_read").Add(float64(c.Usage.CacheReadTokens))
	}
	metricCost.WithLabelValues(string(c.Class), c.Provider, c.Model).Add(c.CostUSD)
}
