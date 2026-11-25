package media

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
)

// The routing policy (owner, 2026-11-20). Two rules, both platform config (ProviderSetting
// platform / media / routing), with the code defaults below only as the fallback:
//
//  1. Each scenario starts on the route the paid bake-offs proved the best quality for its cost
//     (.claude/plans/marketflow-ugc-bakeoff-and-sprints-2026-11-03.md and
//     marketflow-media-provider-review-2026-11-11.md). The router's score still orders the rest,
//     and a route without a key, with a low balance or an open breaker is passed over.
//  2. The platform owner's tenant (the exempt tenant) gets premium routes for every feature. A
//     video ad falls back to standard scene by scene only when its all-premium quote is over the
//     budget, or when no premium route can take a scene (provider offline, unpaid or low).
//
// Voice is not routed here: ElevenLabs stays the voice default (voice.go).

// Scenarios a preference names.
const (
	ScenarioPresenter = "presenter" // presenter scenes of a video ad
	ScenarioAdClip    = "ad_clip"   // product and b-roll clips of a video ad
	ScenarioMotion    = "motion"    // motion posters
	ScenarioStill     = "still"     // storyboard stills and the actor
	ScenarioPoster    = "poster"    // poster scenes
)

// RoutePolicy is the routing policy in force.
type RoutePolicy struct {
	// Prefer maps "scenario/tier" to the routes tried first, in order.
	Prefer map[string][]string `json:"prefer"`
	// PremiumForExempt gives the exempt tenant premium routes by default.
	PremiumForExempt bool `json:"premium_for_exempt"`
	// PremiumBudgetUSD is the most an exempt tenant's video ad may cost at provider prices before
	// its dearest scenes fall back to standard; never above MaxVideoUSD (0 reads as MaxVideoUSD).
	PremiumBudgetUSD float64 `json:"premium_budget_usd"`
	// MaxVideoUSD is the ceiling on one video ad's provider cost for every tenant and tier
	// (setting "max_video_budget_usd"; owner 2026-11-20: configurable, USD 2 by default).
	MaxVideoUSD float64 `json:"max_video_budget_usd"`
	// StillTimeoutSeconds is how long one storyboard still may take on a route, queue included,
	// before the next route is tried (setting "still_timeout_seconds", 30 to 600; owner 2026-11-24:
	// fal queues ran past 60 s and failed whole storyboards).
	StillTimeoutSeconds int `json:"still_timeout_seconds"`
	// IdentityRoutes are the routes that may draw an actor from a consented person's picture, in
	// order (setting "identity_routes", a JSON list; [] turns brand faces' route off; faces.go).
	IdentityRoutes []string `json:"identity_routes"`
	// TextRoutes are the routes that render a few words well enough for a local cue (setting
	// "text_routes", a JSON list; [] draws no cues; cues.go).
	TextRoutes []string `json:"text_routes"`
}

// DefaultStillTimeoutSeconds is only the fallback while the platform sets none.
const DefaultStillTimeoutSeconds = 180

// DefaultMaxVideoBudgetUSD is only the fallback when the platform has not set
// max_video_budget_usd (owner 2026-11-20); change the setting, not this.
const DefaultMaxVideoBudgetUSD = 2.0

// DefaultRoutePolicy is the bake-off evidence as code defaults:
//   - presenter: Creatify Boreal (owner's pick after the 2026-11-11 paid test: good lip-sync and
//     picture at USD 0.01/s); premium Seedance 2.5 at 720p (2026-11-03 tier map);
//   - ad clips: fal Hailuo 2.3 Fast (owner's pick, USD 0.19 per 6 s); premium Kling 3.0 std, the
//     premium presenter's b-roll in the 2026-11-03 tier map;
//   - motion posters: Hailuo 2.3 Fast; premium Seedance 2.0 at 720p;
//   - storyboard stills: Soul v2 (USD 0.003); premium Seedream v5 lite;
//   - posters: Seedream v5 lite with our compositor; premium Nano Banana Pro.
func DefaultRoutePolicy() RoutePolicy {
	return RoutePolicy{
		Prefer: map[string][]string{
			ScenarioPresenter + "/" + TierStandard: {"fal/creatify-boreal"},
			ScenarioPresenter + "/" + TierPremium:  {"higgsfield/seedance-2.5-720p"},
			ScenarioAdClip + "/" + TierStandard:    {"fal/hailuo-2.3-fast"},
			ScenarioAdClip + "/" + TierPremium:     {"higgsfield/kling-v3-std"},
			ScenarioMotion + "/" + TierStandard:    {"fal/hailuo-2.3-fast"},
			ScenarioMotion + "/" + TierPremium:     {"higgsfield/seedance-2.0"},
			ScenarioStill + "/" + TierStandard:     {"higgsfield/soul-v2"},
			ScenarioStill + "/" + TierPremium:      {"fal/seedream-v5-lite"},
			ScenarioPoster + "/" + TierStandard:    {"fal/seedream-v5-lite"},
			ScenarioPoster + "/" + TierPremium:     {"fal/nano-banana-pro"},
		},
		PremiumForExempt:    true,
		PremiumBudgetUSD:    DefaultMaxVideoBudgetUSD,
		MaxVideoUSD:         DefaultMaxVideoBudgetUSD,
		StillTimeoutSeconds: DefaultStillTimeoutSeconds,
		// fal's page for this edit route says it keeps "resemblance and consistency for up to 5
		// people across edits" (read 2026-11-25).
		IdentityRoutes: []string{"fal/nano-banana-pro"},
		// The same page names "enhanced text rendering"; the pages of the cheaper still routes claim
		// none, so they are not on the list until a test shows otherwise.
		TextRoutes: []string{"fal/nano-banana-pro"},
	}
}

// PolicySource returns the policy in force; nil means the defaults.
type PolicySource func(ctx context.Context) RoutePolicy

// policyOf reads a source, falling back to the defaults.
func policyOf(ctx context.Context, src PolicySource) RoutePolicy {
	if src == nil {
		return DefaultRoutePolicy()
	}
	return src(ctx)
}

// PolicyFromSettings reads the platform's routing settings (ProviderSetting platform / media /
// routing): "prefer" is JSON {"scenario/tier": ["route", ...]} and replaces the defaults it names,
// "premium_for_exempt" is true or false, "premium_budget_usd" a number. Anything unreadable keeps
// the default.
func PolicyFromSettings(s map[string]string) RoutePolicy {
	p := DefaultRoutePolicy()
	if raw := strings.TrimSpace(s["prefer"]); raw != "" {
		var m map[string][]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for k, v := range m {
				p.Prefer[k] = v
			}
		}
	}
	if v, err := strconv.ParseBool(strings.TrimSpace(s["premium_for_exempt"])); err == nil {
		p.PremiumForExempt = v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(s["premium_budget_usd"]), 64); err == nil && v > 0 {
		p.PremiumBudgetUSD = v
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(s["max_video_budget_usd"]), 64); err == nil && v > 0 {
		p.MaxVideoUSD = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s["still_timeout_seconds"])); err == nil && v >= 30 && v <= 600 {
		p.StillTimeoutSeconds = v
	}
	routeList(s["identity_routes"], &p.IdentityRoutes)
	routeList(s["text_routes"], &p.TextRoutes)
	if p.PremiumBudgetUSD <= 0 || p.PremiumBudgetUSD > p.MaxVideoUSD {
		p.PremiumBudgetUSD = p.MaxVideoUSD
	}
	return p
}

// routeList reads a JSON list of route keys over dst; an empty list is kept, an unset or unreadable
// value keeps the default.
func routeList(raw string, dst *[]string) {
	var list []string
	if raw = strings.TrimSpace(raw); raw != "" && json.Unmarshal([]byte(raw), &list) == nil && list != nil {
		*dst = list
	}
}

// preferredKeys is the routes a scenario and tier try first.
func preferredKeys(ctx context.Context, src PolicySource, scenario, tier string) []string {
	if tier == "" {
		tier = TierStandard
	}
	return policyOf(ctx, src).Prefer[scenario+"/"+tier]
}

// routingPolicy is the policy source every pipeline reads; app wiring sets it once at start.
var routingPolicy PolicySource

// SetPolicySource sets the platform's routing policy source (nil: the defaults).
func SetPolicySource(src PolicySource) { routingPolicy = src }

// Policy is the routing policy in force now.
func Policy(ctx context.Context) RoutePolicy { return policyOf(ctx, routingPolicy) }

// prefer is the preferred routes of a scenario and tier under the policy in force.
func prefer(ctx context.Context, scenario, tier string) []string {
	return preferredKeys(ctx, routingPolicy, scenario, tier)
}
