package registry

import (
	"fmt"
	"math"
)

// PlanInput is what the Creative Plan is sized from.
type PlanInput struct {
	AdBudgetKES float64  `json:"ad_budget_kes"` // 0 for organic only
	Days        int      `json:"days"`
	Platforms   []string `json:"platforms"`
	// Tier is the media output for picked variants (poster, template video ...).
	// Empty means poster, the cheapest media.
	Tier string `json:"tier"`
	// CreditsAvailable is the spendable balance (allowance plus bundles), or the
	// Autopilot monthly ceiling when that is lower. Negative means unknown.
	CreditsAvailable int `json:"credits_available"`
	// PlanCap limits live ads (plan feature), 0 for no cap.
	PlanCap int `json:"plan_cap"`
	// HasPlan is false for clients without a subscription (launch fee applies).
	HasPlan bool `json:"has_plan"`
	// Features unlocked for the tenant; nil means every feature (no gating info).
	Features map[string]bool `json:"features,omitempty"`
}

// Line is one priced line of a quote.
type Line struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Qty     int    `json:"qty"`
	Credits int    `json:"credits"`
}

// Option is a priced way to do the job.
type Option struct {
	Label       string `json:"label"`
	Variants    int    `json:"variants"`  // N text variants written
	Shortlist   int    `json:"shortlist"` // K picked and produced
	Tier        string `json:"tier,omitempty"`
	Lines       []Line `json:"lines"`
	Credits     int    `json:"credits"`
	Affordable  bool   `json:"affordable"`
	Description string `json:"description"`
}

// CreativePlan is the quote shown before anything is generated.
type CreativePlan struct {
	Recommended      Option   `json:"recommended"`
	Alternatives     []Option `json:"alternatives"`
	MaxLiveAds       int      `json:"max_live_ads"`
	MaxRenders       int      `json:"max_renders"`
	DailyPerAdKES    float64  `json:"daily_per_ad_kes,omitempty"`
	AdBudgetKES      float64  `json:"ad_budget_kes"`
	Days             int      `json:"days"`
	CreditsAvailable int      `json:"credits_available"`
	Shortfall        int      `json:"shortfall"`
	SuggestedBundle  *Bundle  `json:"suggested_bundle,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

// Plan sizes the job: how many ads the budget can test, how many renders the
// credits cover, and the variant counts that follow. All arithmetic lives here;
// models never compute prices or counts.
func (r Registry) Plan(in PlanInput) (CreativePlan, error) {
	s := r.Settings
	tier := in.Tier
	if tier == "" {
		tier = Poster
	}
	media, ok := r.Outputs[tier]
	if !ok || media.Kind != "media" {
		return CreativePlan{}, fmt.Errorf("registry: %q is not a media tier", tier)
	}
	if in.Features != nil && media.Feature != "" && !in.Features[media.Feature] {
		return CreativePlan{}, fmt.Errorf("registry: the plan does not include %s", media.Feature)
	}
	days := in.Days
	if days <= 0 {
		days = 7
	}
	plan := CreativePlan{AdBudgetKES: in.AdBudgetKES, Days: days, CreditsAvailable: in.CreditsAvailable}

	// How many live ads the budget can test.
	platforms := in.Platforms
	if len(platforms) == 0 {
		platforms = []string{"facebook"}
	}
	maxLive := 0
	if in.AdBudgetKES > 0 {
		share := in.AdBudgetKES / float64(len(platforms))
		for _, p := range platforms {
			min := s.MinDailyTestKES[p]
			if min <= 0 {
				min = 300
			}
			n := int(math.Floor(share / (float64(days) * min)))
			if n == 0 {
				plan.Notes = append(plan.Notes, fmt.Sprintf("The budget is below the daily minimum to test one ad on %s (about KES %.0f a day); drop %s or raise the budget.", p, min, p))
			}
			maxLive += n
		}
		plan.MaxLiveAds = maxLive
		if maxLive > 0 {
			plan.DailyPerAdKES = math.Floor(in.AdBudgetKES / float64(days) / float64(maxLive))
		}
	}

	mediaCredits, err := r.Credits(tier)
	if err != nil {
		return CreativePlan{}, err
	}
	sbCredits := 0
	if media.NeedsStoryboard {
		sbCredits, _ = r.Credits(Storyboard)
	}
	perRender := mediaCredits + sbCredits
	copyCredits, _ := r.Credits(CopyBatch)
	launch := 0
	if in.AdBudgetKES > 0 && !in.HasPlan {
		launch, _ = r.Credits(LaunchFee)
	}

	maxRenders := s.MaxShortlist
	if in.CreditsAvailable >= 0 {
		spendable := in.CreditsAvailable - s.ReserveCredits - copyCredits - launch
		maxRenders = 0
		if spendable > 0 {
			maxRenders = spendable / perRender
		}
	}
	plan.MaxRenders = maxRenders

	k := s.MaxShortlist
	if in.AdBudgetKES > 0 {
		k = minInt(k, plan.MaxLiveAds)
	} else {
		k = minInt(k, 3) // organic: a few strong pieces beat many
	}
	if in.PlanCap > 0 {
		k = minInt(k, in.PlanCap)
	}
	affordableK := minInt(k, maxRenders)
	recommendedK := maxInt(affordableK, 1)
	if in.AdBudgetKES > 0 && plan.MaxLiveAds == 0 {
		recommendedK = 1
	}

	plan.Recommended = r.option("Recommended", recommendedK, tier, mediaCredits, sbCredits, copyCredits, launch, in.CreditsAvailable)
	if recommendedK == 1 && in.AdBudgetKES > 0 && plan.MaxLiveAds <= 1 {
		plan.Notes = append(plan.Notes, "The budget can test one live ad at a time; a poster A/B test is cheaper than several videos.")
	}

	// Two cheaper ways, always: posters only (when a pricier tier was asked), and copy only.
	if tier != Poster {
		if pc, err := r.Credits(Poster); err == nil {
			plan.Alternatives = append(plan.Alternatives, r.option("Posters instead of video", recommendedK, Poster, pc, 0, copyCredits, launch, in.CreditsAvailable))
		}
	} else if recommendedK > 1 {
		plan.Alternatives = append(plan.Alternatives, r.option("Fewer posters", recommendedK-1, Poster, mediaCredits, 0, copyCredits, launch, in.CreditsAvailable))
	}
	copyOnly := Option{
		Label: "Copy only", Variants: variants(recommendedK, s.MaxVariants), Shortlist: recommendedK,
		Lines:       []Line{{Key: CopyBatch, Name: r.Outputs[CopyBatch].Name, Qty: 1, Credits: copyCredits}},
		Credits:     copyCredits,
		Description: "Text ads and captions only; use your own photos.",
	}
	copyOnly.Affordable = in.CreditsAvailable < 0 || copyOnly.Credits <= in.CreditsAvailable
	plan.Alternatives = append(plan.Alternatives, copyOnly)

	if in.CreditsAvailable >= 0 && plan.Recommended.Credits > in.CreditsAvailable {
		plan.Shortfall = plan.Recommended.Credits - in.CreditsAvailable
		if b, ok := r.SmallestBundle(plan.Shortfall); ok {
			plan.SuggestedBundle = &b
		}
	}
	return plan, nil
}

func (r Registry) option(label string, k int, tier string, mediaCredits, sbCredits, copyCredits, launch, available int) Option {
	o := Option{Label: label, Variants: variants(k, r.Settings.MaxVariants), Shortlist: k, Tier: tier}
	o.Lines = append(o.Lines, Line{Key: CopyBatch, Name: r.Outputs[CopyBatch].Name, Qty: 1, Credits: copyCredits})
	if sbCredits > 0 {
		o.Lines = append(o.Lines, Line{Key: Storyboard, Name: r.Outputs[Storyboard].Name, Qty: k, Credits: k * sbCredits})
	}
	o.Lines = append(o.Lines, Line{Key: tier, Name: r.Outputs[tier].Name, Qty: k, Credits: k * mediaCredits})
	if launch > 0 {
		o.Lines = append(o.Lines, Line{Key: LaunchFee, Name: r.Outputs[LaunchFee].Name, Qty: 1, Credits: launch})
	}
	for _, l := range o.Lines {
		o.Credits += l.Credits
	}
	o.Affordable = available < 0 || o.Credits <= available
	o.Description = fmt.Sprintf("Write %d variants, shortlist and produce %d.", o.Variants, k)
	return o
}

// variants returns N: about three per produced ad, between 2K and 4K, capped.
func variants(k, max int) int {
	n := 3 * k
	if n < 3 {
		n = 3
	}
	if max > 0 && n > max {
		n = max
	}
	return n
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
