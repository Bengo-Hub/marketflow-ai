package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"go.uber.org/zap"
)

// Models are a provider's configured model names, used to resolve the aliases.
type Models struct {
	Main     string
	Fallback string
	Router   string
}

// Provider is a resolved provider: a client plus its configured models.
type Provider struct {
	Client ai.ChatClient
	Models Models
}

// ProviderSource resolves providers by name (groq, ollama, openai, anthropic).
// ok is false when the provider is not configured, and the router skips it.
type ProviderSource interface {
	Provider(ctx context.Context, name string) (Provider, bool)
}

// PolicySource returns the stored routing policy JSON (empty when none is saved).
type PolicySource func(ctx context.Context) (string, error)

// ErrNoTarget is returned when no target of a class could be tried.
var ErrNoTarget = errors.New("llm: no configured model for this class")

const (
	breakerFailures = 3
	breakerCooldown = 60 * time.Second
	policyTTL       = 5 * time.Minute
)

// Router routes calls by class. It is safe for concurrent use.
type Router struct {
	providers ProviderSource
	policySrc PolicySource
	base      Policy
	logger    *zap.Logger

	mu       sync.Mutex
	policy   Policy
	loadedAt time.Time
	breakers map[string]*breaker
	nowFn    func() time.Time
}

type breaker struct {
	fails     int
	openUntil time.Time
}

// Config configures a Router.
type Config struct {
	Providers ProviderSource
	Policy    PolicySource // optional; nil uses the defaults only
	Defaults  Policy
	Logger    *zap.Logger
}

// NewRouter builds a router.
func NewRouter(cfg Config) *Router {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Router{
		providers: cfg.Providers,
		policySrc: cfg.Policy,
		base:      cfg.Defaults,
		policy:    cfg.Defaults,
		logger:    logger.Named("llm.router"),
		breakers:  make(map[string]*breaker),
		nowFn:     time.Now,
	}
}

// Policy returns the current policy, reloading the stored one at most every five
// minutes. A load or parse failure keeps the last good policy.
func (r *Router) Policy(ctx context.Context) Policy {
	r.mu.Lock()
	fresh := r.policySrc == nil || r.nowFn().Sub(r.loadedAt) < policyTTL
	current := r.policy
	r.mu.Unlock()
	if fresh {
		return current
	}
	raw, err := r.policySrc(ctx)
	next := current
	if err != nil {
		r.logger.Debug("routing policy load failed, keeping current", zap.Error(err))
	} else if p, perr := ParsePolicy(raw, r.base); perr != nil {
		r.logger.Warn("stored routing policy is invalid, keeping current", zap.Error(perr))
	} else {
		next = p
	}
	r.mu.Lock()
	r.policy = next
	r.loadedAt = r.nowFn()
	r.mu.Unlock()
	return next
}

// Complete runs a non-streaming call on the first healthy target of class.
func (r *Router) Complete(ctx context.Context, class Class, req ai.ChatRequest) (*ai.ChatResponse, Call, error) {
	targets := r.Policy(ctx).Classes[class]
	var lastErr error
	tried := 0
	for _, t := range r.order(targets) {
		if ctx.Err() != nil {
			return nil, Call{}, ctx.Err()
		}
		p, ok := r.resolve(ctx, t)
		if !ok || (t.Provider == "anthropic" && len(req.Tools) > 0) {
			// Tool loops stay on OpenAI-compatible providers: a Claude tool turn would
			// need its thinking blocks replayed, which this message format drops.
			continue
		}
		tried++
		call := Call{Class: class, Provider: t.Provider, Model: modelFor(t, p)}
		creq := r.prepare(req, t, call.Model)
		cctx, cancel := ctx, context.CancelFunc(func() {})
		if t.TimeoutSeconds > 0 {
			cctx, cancel = context.WithTimeout(ctx, time.Duration(t.TimeoutSeconds)*time.Second)
		}
		started := r.nowFn()
		resp, err := p.Client.Chat(cctx, creq)
		cancel()
		if err == nil && (resp == nil || len(resp.Choices) == 0) {
			err = fmt.Errorf("empty response")
		}
		if err != nil {
			lastErr = fmt.Errorf("%s/%s: %w", t.Provider, call.Model, err)
			r.fail(key(t, call.Model))
			metricCalls.WithLabelValues(string(class), t.Provider, call.Model, "error").Inc()
			r.logger.Warn("model call failed, trying next target",
				zap.String("class", string(class)), zap.String("provider", t.Provider),
				zap.String("model", call.Model), zap.Error(err))
			continue
		}
		r.succeed(key(t, call.Model))
		call.Usage = resp.Usage
		call.CostUSD = EstimateUSD(t, resp.Usage)
		call.LatencyMs = r.nowFn().Sub(started).Milliseconds()
		metricCalls.WithLabelValues(string(class), t.Provider, call.Model, "ok").Inc()
		observe(call)
		MeterFrom(ctx).Add(call)
		return resp, call, nil
	}
	if tried == 0 {
		return nil, Call{}, fmt.Errorf("%w: %s", ErrNoTarget, class)
	}
	return nil, Call{}, fmt.Errorf("llm: all %s targets failed: %w", class, lastErr)
}

// Stream opens a streaming call on the first healthy target that accepts it.
// Streams carry no usage, so only the call count is recorded.
func (r *Router) Stream(ctx context.Context, class Class, req ai.ChatRequest) (<-chan ai.StreamDelta, Call, error) {
	targets := r.Policy(ctx).Classes[class]
	var lastErr error
	tried := 0
	for _, t := range r.order(targets) {
		p, ok := r.resolve(ctx, t)
		if !ok || (t.Provider == "anthropic" && len(req.Tools) > 0) {
			continue
		}
		tried++
		model := modelFor(t, p)
		// No per-target timeout here: the stream outlives this call and a deferred
		// cancel would cut it off mid-answer.
		ch, err := p.Client.ChatStream(ctx, r.prepare(req, t, model))
		if err != nil {
			lastErr = fmt.Errorf("%s/%s: %w", t.Provider, model, err)
			r.fail(key(t, model))
			metricCalls.WithLabelValues(string(class), t.Provider, model, "error").Inc()
			r.logger.Warn("model stream failed, trying next target",
				zap.String("class", string(class)), zap.String("provider", t.Provider),
				zap.String("model", model), zap.Error(err))
			continue
		}
		r.succeed(key(t, model))
		metricCalls.WithLabelValues(string(class), t.Provider, model, "ok").Inc()
		call := Call{Class: class, Provider: t.Provider, Model: model}
		MeterFrom(ctx).Add(call)
		return ch, call, nil
	}
	if tried == 0 {
		return nil, Call{}, fmt.Errorf("%w: %s", ErrNoTarget, class)
	}
	return nil, Call{}, fmt.Errorf("llm: all %s stream targets failed: %w", class, lastErr)
}

// Client returns an ai.ChatClient bound to class, for code written against a
// single client (the agent loop, CRM tools, the task executor). The request's
// Model field is ignored; the policy picks the model.
func (r *Router) Client(class Class) ai.ChatClient {
	return &boundClient{r: r, class: class}
}

type boundClient struct {
	r     *Router
	class Class
}

func (b *boundClient) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	resp, _, err := b.r.Complete(ctx, b.class, req)
	return resp, err
}

func (b *boundClient) ChatStream(ctx context.Context, req ai.ChatRequest) (<-chan ai.StreamDelta, error) {
	ch, _, err := b.r.Stream(ctx, b.class, req)
	return ch, err
}

func (r *Router) resolve(ctx context.Context, t Target) (Provider, bool) {
	if r.providers == nil {
		return Provider{}, false
	}
	p, ok := r.providers.Provider(ctx, t.Provider)
	if !ok || p.Client == nil || modelFor(t, p) == "" {
		return Provider{}, false
	}
	return p, true
}

func (r *Router) prepare(req ai.ChatRequest, t Target, model string) ai.ChatRequest {
	out := req
	out.Model = model
	if out.MaxTokens == 0 {
		out.MaxTokens = t.MaxTokens
	}
	if t.Temperature != nil {
		out.Temperature = *t.Temperature
	}
	if t.Effort != "" && out.Effort == "" {
		out.Effort = t.Effort
	}
	return out
}

// order puts targets with a closed breaker first, keeping policy order, and the
// open ones last so a class is never left with nothing to try.
func (r *Router) order(targets []Target) []Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowFn()
	healthy := make([]Target, 0, len(targets))
	var open []Target
	for _, t := range targets {
		if b := r.breakers[key(t, t.Model)]; b != nil && now.Before(b.openUntil) {
			open = append(open, t)
			continue
		}
		healthy = append(healthy, t)
	}
	return append(healthy, open...)
}

func (r *Router) fail(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.breakers[k]
	if b == nil {
		b = &breaker{}
		r.breakers[k] = b
	}
	b.fails++
	if b.fails >= breakerFailures {
		b.openUntil = r.nowFn().Add(breakerCooldown)
		b.fails = 0
	}
}

func (r *Router) succeed(k string) {
	r.mu.Lock()
	delete(r.breakers, k)
	r.mu.Unlock()
}

func modelFor(t Target, p Provider) string {
	switch t.Model {
	case ModelMain:
		return p.Models.Main
	case ModelFallback:
		return firstNonEmpty(p.Models.Fallback, p.Models.Main)
	case ModelRouter:
		return firstNonEmpty(p.Models.Router, p.Models.Fallback, p.Models.Main)
	default:
		return t.Model
	}
}

// key identifies a breaker by provider and the policy's model entry (aliases
// included), so an alias and its resolved name never split one model's state.
func key(t Target, _ string) string { return t.Provider + "|" + t.Model }
