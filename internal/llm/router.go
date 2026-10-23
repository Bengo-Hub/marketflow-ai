package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/breaker"
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
	// authCooldown keeps a provider whose key was rejected out of the way longer:
	// retrying it within a minute cannot succeed.
	authCooldown = 10 * time.Minute
	policyTTL    = 5 * time.Minute
)

// firstTokenTimeout is how long a stream may stay silent after connecting
// before the router gives up on it and opens the next target.
var firstTokenTimeout = 15 * time.Second

// classBudget bounds one Complete call across all of a class's targets, so a
// chain of slow providers never adds up to a stuck request. Callers with a
// shorter deadline keep theirs.
var classBudget = map[Class]time.Duration{
	Nano:        20 * time.Second,
	Standard:    45 * time.Second,
	Premium:     70 * time.Second,
	PremiumPlus: 90 * time.Second,
}

// defaultTargetTimeout applies to targets saved without timeout_seconds.
var defaultTargetTimeout = map[Class]time.Duration{
	Nano:        10 * time.Second,
	Standard:    25 * time.Second,
	Premium:     45 * time.Second,
	PremiumPlus: 75 * time.Second,
}

func targetTimeout(class Class, t Target) time.Duration {
	if t.TimeoutSeconds > 0 {
		return time.Duration(t.TimeoutSeconds) * time.Second
	}
	if d, ok := defaultTargetTimeout[class]; ok {
		return d
	}
	return 30 * time.Second
}

// Router routes calls by class. It is safe for concurrent use.
type Router struct {
	providers ProviderSource
	policySrc PolicySource
	base      Policy
	logger    *zap.Logger

	mu       sync.Mutex
	policy   Policy
	loadedAt time.Time
	br       *breaker.Set // one breaker per provider and model
	nowFn    func() time.Time
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
	r := &Router{
		providers: cfg.Providers,
		policySrc: cfg.Policy,
		base:      cfg.Defaults,
		policy:    cfg.Defaults,
		logger:    logger.Named("llm.router"),
		br:        breaker.New(breakerFailures, breakerCooldown, authCooldown),
		nowFn:     time.Now,
	}
	r.br.Now = func() time.Time { return r.nowFn() } // one clock, replaceable in tests
	return r
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

// usable lists the targets that can serve req, healthy ones first.
func (r *Router) usable(ctx context.Context, class Class, req ai.ChatRequest) []resolvedTarget {
	var out []resolvedTarget
	for _, t := range r.order(r.Policy(ctx).Classes[class]) {
		p, ok := r.resolve(ctx, t)
		if !ok || (t.Provider == "anthropic" && len(req.Tools) > 0) {
			// Tool loops stay on OpenAI-compatible providers: a Claude tool turn would
			// need its thinking blocks replayed, which this message format drops.
			continue
		}
		out = append(out, resolvedTarget{t: t, p: p, model: modelFor(t, p)})
	}
	return out
}

type resolvedTarget struct {
	t     Target
	p     Provider
	model string
}

// Complete runs a non-streaming call on the first healthy target of class. The
// whole call, fallbacks included, stays inside the class budget; each target
// gets its own timeout, and every target but the last fails fast instead of
// waiting out a rate limit.
func (r *Router) Complete(ctx context.Context, class Class, req ai.ChatRequest) (*ai.ChatResponse, Call, error) {
	if budget, ok := classBudget[class]; ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	targets := r.usable(ctx, class, req)
	if len(targets) == 0 {
		return nil, Call{}, fmt.Errorf("%w: %s", ErrNoTarget, class)
	}
	var lastErr error
	for i, rt := range targets {
		if ctx.Err() != nil {
			break
		}
		t := rt.t
		call := Call{Class: class, Provider: t.Provider, Model: rt.model}
		creq := r.prepare(req, t, call.Model)
		cctx, cancel := context.WithTimeout(ctx, targetTimeout(class, t))
		if i < len(targets)-1 {
			cctx = ai.WithFailFast(cctx)
		}
		started := r.nowFn()
		resp, err := rt.p.Client.Chat(cctx, creq)
		cancel()
		if err == nil && (resp == nil || len(resp.Choices) == 0) {
			err = fmt.Errorf("empty response")
		}
		if err != nil {
			lastErr = fmt.Errorf("%s/%s: %w", t.Provider, call.Model, err)
			r.failed(ctx, key(t, call.Model), err)
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
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, Call{}, fmt.Errorf("llm: all %s targets failed: %w", class, lastErr)
}

// Stream opens a streaming call on the first healthy target that starts
// answering. A target that connects but sends nothing within firstTokenTimeout
// is abandoned for the next one, so a stalled provider never freezes a chat.
// Streams carry no usage, so only the call count is recorded.
func (r *Router) Stream(ctx context.Context, class Class, req ai.ChatRequest) (<-chan ai.StreamDelta, Call, error) {
	targets := r.usable(ctx, class, req)
	if len(targets) == 0 {
		return nil, Call{}, fmt.Errorf("%w: %s", ErrNoTarget, class)
	}
	var lastErr error
	for i, rt := range targets {
		if ctx.Err() != nil {
			break
		}
		t, model := rt.t, rt.model
		// The attempt context lives as long as the stream; it is cancelled when
		// the stream ends, or right away when the target is abandoned.
		actx, cancel := context.WithCancel(ctx)
		if i < len(targets)-1 {
			actx = ai.WithFailFast(actx)
		}
		wait := firstTokenTimeout
		if d := targetTimeout(class, t); d < wait {
			wait = d
		}
		ch, first, err := r.openStream(actx, rt, req, wait)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("%s/%s: %w", t.Provider, model, err)
			r.failed(ctx, key(t, model), err)
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
		return relay(first, ch, cancel), call, nil
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, Call{}, fmt.Errorf("llm: all %s stream targets failed: %w", class, lastErr)
}

var errNoFirstToken = errors.New("no output before the first-token deadline")

// openStream connects and waits for the first delta, both within wait.
func (r *Router) openStream(ctx context.Context, rt resolvedTarget, req ai.ChatRequest, wait time.Duration) (<-chan ai.StreamDelta, ai.StreamDelta, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	type opened struct {
		ch  <-chan ai.StreamDelta
		err error
	}
	got := make(chan opened, 1)
	go func() {
		ch, err := rt.p.Client.ChatStream(ctx, r.prepare(req, rt.t, rt.model))
		got <- opened{ch, err}
	}()
	var ch <-chan ai.StreamDelta
	select {
	case o := <-got:
		if o.err != nil {
			return nil, ai.StreamDelta{}, o.err
		}
		ch = o.ch
	case <-timer.C:
		return nil, ai.StreamDelta{}, errNoFirstToken
	case <-ctx.Done():
		return nil, ai.StreamDelta{}, ctx.Err()
	}
	select {
	case d, ok := <-ch:
		if !ok {
			return nil, ai.StreamDelta{}, fmt.Errorf("stream closed before any output")
		}
		return ch, d, nil
	case <-timer.C:
		return nil, ai.StreamDelta{}, errNoFirstToken
	case <-ctx.Done():
		return nil, ai.StreamDelta{}, ctx.Err()
	}
}

// relay hands the caller the first delta and the rest of the stream, and
// releases the attempt context when the stream ends.
func relay(first ai.StreamDelta, src <-chan ai.StreamDelta, done context.CancelFunc) <-chan ai.StreamDelta {
	out := make(chan ai.StreamDelta, 100)
	go func() {
		defer done()
		defer close(out)
		out <- first
		if first.Done {
			return
		}
		for d := range src {
			out <- d
		}
	}()
	return out
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
// open ones last so a class is never left with nothing to try. When an open
// breaker's cooldown has passed, one caller gets it in the healthy list as the
// half-open probe (internal/breaker).
func (r *Router) order(targets []Target) []Target {
	healthy := make([]Target, 0, len(targets))
	var open []Target
	for _, t := range targets {
		if r.br.Admit(key(t, t.Model)) {
			healthy = append(healthy, t)
		} else {
			open = append(open, t)
		}
	}
	return append(healthy, open...)
}

// failed records a failed attempt; internal/breaker decides what counts.
func (r *Router) failed(parent context.Context, k string, err error) {
	r.br.Failed(k, err, parent.Err() != nil)
}

func (r *Router) succeed(k string) { r.br.Succeeded(k) }

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
