// Package llm is the model router: every generative call names a model class
// (nano, standard, premium, premium_plus) instead of a provider, and the router
// tries that class's targets in order with a circuit breaker per model, records
// usage and estimated cost, and falls back on errors.
//
// Judgement calls (classification, routing, scoring, yes/no checks) do not use
// these classes; they go to the decide port (internal/decide).
package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Class is a model tier picked by task, not by provider.
type Class string

const (
	// Nano is the light class, free and lightweight models first: public widget
	// answers, optimised briefs, one-line summaries, triage, the decide fallback.
	Nano Class = "nano"
	// Standard answers chat, writes simple drafts and drives tool use.
	Standard Class = "standard"
	// Premium plans multi-step jobs and writes copy meant to go live.
	Premium Class = "premium"
	// PremiumPlus writes marketing plans, strategy and the brand-context draft.
	PremiumPlus Class = "premium_plus"
)

// Classes lists every class in cost order, cheapest first.
var Classes = []Class{Nano, Standard, Premium, PremiumPlus}

// ParseClass validates a class name.
func ParseClass(s string) (Class, error) {
	c := Class(strings.TrimSpace(strings.ToLower(s)))
	for _, known := range Classes {
		if c == known {
			return c, nil
		}
	}
	return "", fmt.Errorf("unknown model class %q", s)
}

// Model aliases resolved against the provider's configured models, so the platform
// owner can change Groq models on /platform/providers without editing the policy.
const (
	ModelMain     = ""          // the provider's main (responder) model
	ModelFallback = "@fallback" // the provider's fallback model
	ModelRouter   = "@router"   // the provider's small router model
	ModelPremium  = "@premium"  // the provider's premium model (Anthropic's Sonnet)
)

// Target is one model a class may use, in fallback order.
type Target struct {
	Provider string `json:"provider"` // groq | ollama | openai | anthropic
	Model    string `json:"model"`    // concrete id, or an alias above
	// MaxTokens caps the reply when the caller leaves it unset.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature overrides the caller's value for OpenAI-compatible providers.
	Temperature *float64 `json:"temperature,omitempty"`
	// Effort is sent to Anthropic as output_config.effort.
	Effort string `json:"effort,omitempty"`
	// TimeoutSeconds bounds a non-streaming call (local models are slow under load).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Prices in USD per million tokens, used for the cost estimate only.
	InputUSDPerM  float64 `json:"input_usd_per_m"`
	OutputUSDPerM float64 `json:"output_usd_per_m"`
}

// DecideSettings tunes the decide port (see internal/decide).
type DecideSettings struct {
	// Thresholds maps a decision name to the minimum confidence for acting on it.
	// Below it the caller takes the safe branch.
	Thresholds map[string]float64 `json:"thresholds,omitempty"`
	// MonthlyUSDCeiling stops paid decide adapters for the rest of the month once
	// reached; the LLM fallback answers instead. Zero means no ceiling.
	MonthlyUSDCeiling float64 `json:"monthly_usd_ceiling,omitempty"`
}

// Policy maps classes to targets. It is stored as the platform ProviderSetting
// ai_routing/policy (key "policy") and merged over the code defaults.
type Policy struct {
	Classes map[Class][]Target `json:"classes"`
	Decide  DecideSettings     `json:"decide"`
	// Prices are USD per million tokens by model family (a case-insensitive regular expression
	// on the model id), for targets that carry no price of their own. A target follows the
	// model its provider is set to, so its cost estimate follows that model too. Stored entries
	// merge over the defaults; the provider's "*" entry prices a model no family matches.
	Prices map[string]map[string]Price `json:"prices,omitempty"`
}

// Price is a model's list price in USD per million tokens.
type Price struct {
	In  float64 `json:"input_usd_per_m"`
	Out float64 `json:"output_usd_per_m"`
}

// DefaultPrices is the documented fallback price list (Groq's gpt-oss checked 2026-10-24, the
// rest 2026-10-22). An unlisted model takes its provider's "*" price, set at the dearest known
// model so a new model is never estimated as cheaper than it is.
func DefaultPrices() map[string]map[string]Price {
	return map[string]map[string]Price{
		"groq": {
			`gpt-oss-120b`: {0.15, 0.60},
			`gpt-oss-20b`:  {0.075, 0.30},
			"*":            {0.15, 0.60},
		},
		"openai": {
			`^gpt-4o-mini`: {0.15, 0.60},
			"*":            {2.50, 10.00},
		},
		"anthropic": {
			`haiku`:  {0.10, 0.50},
			`sonnet`: {2.00, 10.00},
			"*":      {2.00, 10.00},
		},
	}
}

// PriceFor prices a resolved model: a provider family match first, then the provider's "*".
// The local model (ollama) and an unknown provider cost nothing.
func (p Policy) PriceFor(provider, model string) Price {
	table := p.Prices[provider]
	keys := make([]string, 0, len(table))
	for k := range table {
		if k != "*" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) }) // most specific first
	for _, k := range keys {
		if re, err := regexp.Compile("(?i)" + k); err == nil && re.MatchString(model) {
			return table[k]
		}
	}
	return table["*"]
}

// Defaults is kept for callers that build the default policy. Model names no longer come from
// here: every default target names an alias resolved against the provider's configured models
// (Platform > Providers, then the env fallback), so syncing or changing a provider's model
// moves the routing with it.
type Defaults struct{}

func f(v float64) *float64 { return &v }

// DefaultPolicy is the routing used when the platform has not saved one. Every target is an
// alias of its provider's configured models: Groq's responder, fallback and router; OpenAI's
// main model; Anthropic's main (Haiku) and premium (Sonnet) models; the local Ollama model.
// Prices come from Prices by the resolved model and only feed the cost estimate. Groq's free
// tier and the local model cost nothing, so the light class tries them before any paid
// provider. A stored policy that names a model outright still wins for its class.
func DefaultPolicy(_ Defaults) Policy {
	// Timeouts are per attempt and sit inside the router's class budget (nano 20s,
	// standard 45s, premium 70s, premium_plus 90s), so the worst chain still ends
	// in time to answer. The local model is last: it is free but slow under load.
	groqMain := Target{Provider: "groq", Model: ModelMain, TimeoutSeconds: 20}
	groqSmall := Target{Provider: "groq", Model: ModelFallback, TimeoutSeconds: 15}
	groqRouter := Target{Provider: "groq", Model: ModelRouter, TimeoutSeconds: 8}
	local := Target{Provider: "ollama", Model: ModelMain, TimeoutSeconds: 25}
	gpt := Target{Provider: "openai", Model: ModelMain, TimeoutSeconds: 15}
	claudeLow := Target{Provider: "anthropic", Model: ModelMain, Effort: "low", TimeoutSeconds: 25}
	claudeMed := Target{Provider: "anthropic", Model: ModelMain, Effort: "medium", TimeoutSeconds: 45}
	claudePlus := Target{Provider: "anthropic", Model: ModelPremium, Effort: "medium", TimeoutSeconds: 60}

	// Light class: Groq's small model, then its main one (both on the free tier),
	// then the local model, and only then the paid providers.
	nanoRouter := groqRouter
	nanoRouter.Temperature = f(0.3)
	nanoMain, nanoGPT, nanoClaude, nanoLocal := groqMain, gpt, claudeLow, local
	nanoMain.TimeoutSeconds, nanoGPT.TimeoutSeconds, nanoClaude.TimeoutSeconds, nanoLocal.TimeoutSeconds = 8, 8, 10, 12
	return Policy{
		Classes: map[Class][]Target{
			Nano:        {nanoRouter, nanoMain, nanoLocal, nanoGPT, nanoClaude},
			Standard:    {groqMain, groqSmall, gpt, claudeLow, local},
			Premium:     {claudeMed, groqMain, gpt},
			PremiumPlus: {claudePlus, claudeMed, groqMain},
		},
		Decide: DecideSettings{
			Thresholds: map[string]float64{
				"route":      0.75,
				"complexity": 0.60,
				"injection":  0.50,
				"action":     0.70,
				"qc":         0.60,
			},
			MonthlyUSDCeiling: 20,
		},
		Prices: DefaultPrices(),
	}
}

// ParsePolicy decodes a stored policy and merges it over base: a class present in
// the stored JSON with at least one target replaces the default list, missing
// classes keep the defaults, and decide thresholds merge key by key.
func ParsePolicy(raw string, base Policy) (Policy, error) {
	if strings.TrimSpace(raw) == "" {
		return base, nil
	}
	var stored Policy
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return base, fmt.Errorf("decode routing policy: %w", err)
	}
	out := Policy{Classes: make(map[Class][]Target, len(base.Classes)), Decide: base.Decide, Prices: mergePrices(base.Prices, stored.Prices)}
	for c, ts := range base.Classes {
		out.Classes[c] = ts
	}
	for c, ts := range stored.Classes {
		if _, err := ParseClass(string(c)); err != nil {
			return base, err
		}
		valid := make([]Target, 0, len(ts))
		for _, t := range ts {
			if t.Provider == "" {
				return base, fmt.Errorf("routing policy: class %s has a target without provider", c)
			}
			valid = append(valid, t)
		}
		if len(valid) > 0 {
			out.Classes[c] = valid
		}
	}
	if len(stored.Decide.Thresholds) > 0 {
		merged := make(map[string]float64, len(base.Decide.Thresholds)+len(stored.Decide.Thresholds))
		for k, v := range base.Decide.Thresholds {
			merged[k] = v
		}
		for k, v := range stored.Decide.Thresholds {
			if v >= 0 && v <= 1 {
				merged[k] = v
			}
		}
		out.Decide.Thresholds = merged
	}
	if stored.Decide.MonthlyUSDCeiling > 0 {
		out.Decide.MonthlyUSDCeiling = stored.Decide.MonthlyUSDCeiling
	}
	return out, nil
}

func mergePrices(base, stored map[string]map[string]Price) map[string]map[string]Price {
	if len(base) == 0 && len(stored) == 0 {
		return nil
	}
	out := make(map[string]map[string]Price, len(base)+len(stored))
	for prov, t := range base {
		out[prov] = make(map[string]Price, len(t))
		for k, v := range t {
			out[prov][k] = v
		}
	}
	for prov, t := range stored {
		if out[prov] == nil {
			out[prov] = map[string]Price{}
		}
		for k, v := range t {
			if _, err := regexp.Compile("(?i)" + k); k == "*" || err == nil {
				out[prov][k] = v
			}
		}
	}
	return out
}

// Threshold returns the confidence a decision needs, defaulting to 0.7.
func (p Policy) Threshold(decision string) float64 {
	if v, ok := p.Decide.Thresholds[decision]; ok {
		return v
	}
	return 0.7
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
