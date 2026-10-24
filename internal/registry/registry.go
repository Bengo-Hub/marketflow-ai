// Package registry is the read side of the platform model and output registry
// and the credit cost engine. It prices every generated output in AI credits
// from its provider cost, so prices are computed, never hard-coded in callers:
//
//	credits = ceil(cost_usd * fx * (1 + fx_buffer) * (1 + retry_overhead) / credit_cost_basis_kes)
//
// The defaults reproduce the Ads Autopilot plan's price table (section 4.4.1).
// The platform owner overrides any value through the platform ProviderSetting
// ai_registry/outputs (key "registry", JSON merged over the defaults). Sprint 15
// adds the provider adapters to this same registry.
package registry

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// Output is one priced thing the OS can produce.
type Output struct {
	Key     string  `json:"key"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`     // text | media | fee
	CostUSD float64 `json:"cost_usd"` // provider cost per unit, upper estimate
	// Credits fixes the price instead of computing it (chat turn, launch fee).
	Credits int `json:"credits,omitempty"`
	// Feature is the plan feature that unlocks the output ("" for everyone).
	Feature string `json:"feature,omitempty"`
	// NeedsStoryboard adds the storyboard output before rendering.
	NeedsStoryboard bool `json:"needs_storyboard,omitempty"`
}

// Bundle is a purchasable AI credit pack.
type Bundle struct {
	Code     string  `json:"code"`
	Units    int     `json:"units"`
	PriceKES float64 `json:"price_kes"`
}

// Settings are the cost-engine inputs.
type Settings struct {
	FXRate          float64 `json:"fx_rate"`            // KES per USD
	FXBuffer        float64 `json:"fx_buffer"`          // 0.05 = 5%
	RetryOverhead   float64 `json:"retry_overhead"`     // 0.15 = 15%
	CreditCostBasis float64 `json:"credit_cost_basis"`  // KES of real cost per credit
	FixedSurcharge  int     `json:"fixed_surcharge"`    // amortised vendor plans, credits per render
	ReserveCredits  int     `json:"reserve_credits"`    // kept back for chat turns and revisions
	NoPlanLaunchFee int     `json:"no_plan_launch_fee"` // credits per paid launch for clients without a plan
	MaxShortlist    int     `json:"max_shortlist"`
	MaxVariants     int     `json:"max_variants"`
	// MinDailyTestKES is the minimum daily spend per live ad for a platform to
	// learn, by platform. It bounds how many ads a budget can test.
	MinDailyTestKES map[string]float64 `json:"min_daily_test_kes"`
}

// Registry is the merged registry.
type Registry struct {
	Settings Settings          `json:"settings"`
	Outputs  map[string]Output `json:"outputs"`
	Bundles  []Bundle          `json:"bundles"`
}

// Output keys used by the OS.
const (
	ChatTurn          = "chat_turn"
	CopyBatch         = "copy_batch"
	Script            = "script"
	Storyboard        = "storyboard"
	Poster            = "poster"
	PosterPremium     = "poster_premium"
	TemplateVideo     = "template_video_30s"
	// Motion poster: a poster's clean scene animated, its exact words laid over
	// it. Priced per 5 s clip; other lengths scale (see CreditsFor).
	MotionPoster        = "motion_poster_5s"
	MotionPosterPremium = "motion_poster_premium_5s"
	Presenter         = "presenter_30s"
	PresenterPremium  = "presenter_premium_30s"
	CinematicLite     = "cinematic_lite_20s"
	CinematicFast     = "cinematic_fast_20s"
	CinematicStandard = "cinematic_standard_20s"
	LaunchFee         = "launch_fee"
)

// Defaults returns the registry from the Ads Autopilot plan (researched
// 2026-10-22). Bundle prices are the live subscriptions-api prices; the
// re-priced bundles proposed for Sprint 15 apply once they ship there.
func Defaults() Registry {
	return Registry{
		Settings: Settings{
			FXRate: 129, FXBuffer: 0.05, RetryOverhead: 0.15, CreditCostBasis: 5,
			FixedSurcharge: 0, ReserveCredits: 5, NoPlanLaunchFee: 10,
			MaxShortlist: 5, MaxVariants: 20,
			MinDailyTestKES: map[string]float64{
				"facebook": 300, "instagram": 300, "whatsapp": 300,
				"tiktok": 2600, // TikTok's ad group minimum is about USD 20 a day
				"google": 300, "youtube": 300,
			},
		},
		Outputs: map[string]Output{
			ChatTurn:  {Key: ChatTurn, Name: "Conversation turn", Kind: "fee", Credits: 1},
			CopyBatch: {Key: CopyBatch, Name: "Copy batch (text variants)", Kind: "text", Credits: 1},
			Script:    {Key: Script, Name: "Script and 3 hooks", Kind: "text", Credits: 1},
			// Six keyframes at about USD 0.015 each on a Flux-class model (the plan's
			// USD 0.02-0.15 range; its 3-credit price matches the middle of it).
			Storyboard: {Key: Storyboard, Name: "Storyboard (6 keyframes)", Kind: "media", CostUSD: 0.09},
			// Upper-bound provider costs per output, from the routes confirmed on
			// 2026-10-24 (docs/integrations/video-generation-providers.md, "What an ad
			// costs"). An estimate never sits below its confirmed route cost.
			// Poster: Seedream v5 lite USD 0.035 plus our compositor (free).
			Poster: {Key: Poster, Name: "Poster or static image ad", Kind: "media", CostUSD: 0.05, Feature: "ai_static_ads"},
			// Premium poster: Nano Banana Pro USD 0.15 plus our compositor.
			PosterPremium: {Key: PosterPremium, Name: "Premium poster", Kind: "media", CostUSD: 0.15, Feature: "ai_static_ads"},
			// Template video: about USD 0.25 (frames, images-to-video, voice, music).
			TemplateVideo: {Key: TemplateVideo, Name: "Template video ad, 30 s", Kind: "media", CostUSD: 0.38, Feature: "ai_video_template"},
			// Motion poster 5 s: Kling 3.0 std at the list USD 0.084/s (0.42) plus our
			// ffmpeg text overlay (free); premium is Seedance 2.0 at USD 0.141/s (0.71).
			MotionPoster:        {Key: MotionPoster, Name: "Motion poster, 5 s", Kind: "media", CostUSD: 0.42, Feature: "ai_video_template"},
			MotionPosterPremium: {Key: MotionPosterPremium, Name: "Premium motion poster, 5 s", Kind: "media", CostUSD: 0.71, Feature: "ai_video_template"},
			// Presenter: 12 s VEED Fabric, two 4 s Kling 3.0 clips, voice: about USD 1.66.
			Presenter: {Key: Presenter, Name: "AI presenter ad, 30 s", Kind: "media", CostUSD: 1.70, Feature: "ai_video_presenter", NeedsStoryboard: true},
			// Premium presenter: Aurora 720p, Seedance 2.0 clips, Eleven v4: about USD 2.85.
			PresenterPremium: {Key: PresenterPremium, Name: "Premium presenter ad, 30 s", Kind: "media", CostUSD: 2.90, Feature: "ai_video_presenter_premium", NeedsStoryboard: true},
			// Cinematic lite: Kling 3.0 std 20 s at the list price USD 1.68, plus voice and music.
			CinematicLite: {Key: CinematicLite, Name: "Cinematic ad 20 s (Lite)", Kind: "media", CostUSD: 1.80, Feature: "ai_video_cinematic", NeedsStoryboard: true},
			// Cinematic fast: Seedance 2.0 20 s at the list price USD 2.82, plus voice and music.
			CinematicFast:     {Key: CinematicFast, Name: "Cinematic ad 20 s (Fast)", Kind: "media", CostUSD: 2.95, Feature: "ai_video_cinematic", NeedsStoryboard: true},
			CinematicStandard: {Key: CinematicStandard, Name: "Cinematic ad 20 s (Standard)", Kind: "media", CostUSD: 8.40, Feature: "ai_video_cinematic_premium", NeedsStoryboard: true},
			LaunchFee:         {Key: LaunchFee, Name: "Paid launch (clients without a plan)", Kind: "fee", Credits: 10},
		},
		Bundles: []Bundle{
			{Code: "bundle_100", Units: 100, PriceKES: 1000},
			{Code: "bundle_500", Units: 500, PriceKES: 4000},
		},
	}
}

// Merge applies a stored JSON override: settings fields that are set, outputs by
// key, and the bundle list when given.
func Merge(base Registry, raw string) (Registry, error) {
	if raw == "" {
		return base, nil
	}
	var over struct {
		Settings map[string]json.RawMessage `json:"settings"`
		Outputs  map[string]Output          `json:"outputs"`
		Bundles  []Bundle                   `json:"bundles"`
	}
	if err := json.Unmarshal([]byte(raw), &over); err != nil {
		return base, fmt.Errorf("registry: %w", err)
	}
	out := base
	out.Outputs = make(map[string]Output, len(base.Outputs))
	for k, v := range base.Outputs {
		out.Outputs[k] = v
	}
	if len(over.Settings) > 0 {
		cur, _ := json.Marshal(base.Settings)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(cur, &m)
		for k, v := range over.Settings {
			m[k] = v
		}
		merged, _ := json.Marshal(m)
		var s Settings
		if err := json.Unmarshal(merged, &s); err != nil {
			return base, fmt.Errorf("registry settings: %w", err)
		}
		if s.CreditCostBasis <= 0 || s.FXRate <= 0 {
			return base, fmt.Errorf("registry: fx_rate and credit_cost_basis must be positive")
		}
		out.Settings = s
	}
	for k, o := range over.Outputs {
		o.Key = k
		if o.CostUSD < 0 || o.Credits < 0 {
			return base, fmt.Errorf("registry: output %s has a negative price", k)
		}
		out.Outputs[k] = o
	}
	if len(over.Bundles) > 0 {
		out.Bundles = over.Bundles
	}
	sort.Slice(out.Bundles, func(i, j int) bool { return out.Bundles[i].Units < out.Bundles[j].Units })
	return out, nil
}

// Credits prices one unit of an output.
// CreditsFor prices n seconds of an output priced per 5 s clip, rounding up.
func (r Registry) CreditsFor(key string, seconds int) (int, error) {
	per5, err := r.Credits(key)
	if err != nil || seconds <= 0 {
		return per5, err
	}
	return (per5*seconds + 4) / 5, nil
}

func (r Registry) Credits(key string) (int, error) {
	o, ok := r.Outputs[key]
	if !ok {
		return 0, fmt.Errorf("registry: unknown output %q", key)
	}
	if o.Credits > 0 || o.CostUSD == 0 {
		return o.Credits, nil
	}
	s := r.Settings
	costKES := o.CostUSD * s.FXRate * (1 + s.FXBuffer) * (1 + s.RetryOverhead)
	c := int(math.Ceil(costKES/s.CreditCostBasis - 1e-9))
	if o.Kind == "media" && o.Key != Storyboard {
		c += s.FixedSurcharge
	}
	return c, nil
}

// SmallestBundle returns the cheapest bundle covering a shortfall, or the
// largest when none does on its own.
func (r Registry) SmallestBundle(shortfall int) (Bundle, bool) {
	if shortfall <= 0 || len(r.Bundles) == 0 {
		return Bundle{}, false
	}
	var best *Bundle
	for i := range r.Bundles {
		b := r.Bundles[i]
		if b.Units >= shortfall && (best == nil || b.PriceKES < best.PriceKES) {
			best = &b
		}
	}
	if best == nil {
		largest := r.Bundles[0]
		for _, b := range r.Bundles[1:] {
			if b.Units > largest.Units {
				largest = b
			}
		}
		return largest, true
	}
	return *best, true
}
