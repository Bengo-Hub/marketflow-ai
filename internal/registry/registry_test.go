package registry

import "testing"

func TestDefaultCreditsMatchPlanTable(t *testing.T) {
	r := Defaults()
	// Credits from the Ads Autopilot plan, section 4.4.1, re-based on 2026-10-24 on
	// the provider routes confirmed in docs/integrations/video-generation-providers.md
	// (presenter and cinematic lite/fast rose; the rest held).
	want := map[string]int{
		ChatTurn: 1, CopyBatch: 1, Script: 1, Storyboard: 3, Poster: 2, PosterPremium: 5,
		TemplateVideo: 12, Presenter: 53, PresenterPremium: 91,
		CinematicLite: 57, CinematicFast: 92, CinematicStandard: 262, LaunchFee: 10,
		MotionPoster: 14, MotionPosterPremium: 23,
	}
	for k, w := range want {
		got, err := r.Credits(k)
		if err != nil || got != w {
			t.Errorf("%s = %d (%v), want %d", k, got, err, w)
		}
	}
	if _, err := r.Credits("nope"); err == nil {
		t.Fatal("unknown output must error")
	}
	// Per-5 s outputs scale with length, rounding up.
	if c, _ := r.CreditsFor(MotionPoster, 5); c != 14 {
		t.Errorf("5 s = %d", c)
	}
	if c, _ := r.CreditsFor(MotionPoster, 8); c != 23 { // 14*8/5 = 22.4
		t.Errorf("8 s = %d", c)
	}
}

func TestMergeOverrides(t *testing.T) {
	r, err := Merge(Defaults(), `{"settings":{"fx_rate":140,"fixed_surcharge":2},"outputs":{"poster":{"name":"Poster","kind":"media","cost_usd":0.05,"feature":"ai_static_ads"}},"bundles":[{"code":"b1000","units":1000,"price_kes":6600},{"code":"b100","units":100,"price_kes":700}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Settings.FXRate != 140 || r.Settings.CreditCostBasis != 5 || r.Settings.MinDailyTestKES["tiktok"] != 2600 {
		t.Fatalf("settings not merged field by field: %+v", r.Settings)
	}
	if c, _ := r.Credits(Poster); c != 4 { // 0.05*140*1.05*1.15 = 8.45 KES -> 2, + surcharge 2
		t.Fatalf("poster = %d", c)
	}
	if r.Bundles[0].Units != 100 {
		t.Fatalf("bundles not sorted: %+v", r.Bundles)
	}
	if _, err := Merge(Defaults(), `{"settings":{"credit_cost_basis":0}}`); err == nil {
		t.Fatal("a zero credit basis must be rejected")
	}
}

func TestPlanSwahiliExample(t *testing.T) {
	// "budget 3k this week" on Facebook with 100 credits, posters.
	p, err := Defaults().Plan(PlanInput{AdBudgetKES: 3000, Days: 7, Platforms: []string{"facebook"}, CreditsAvailable: 100, HasPlan: true})
	if err != nil {
		t.Fatal(err)
	}
	// 3000 / (7 * 300) = 1 live ad.
	if p.MaxLiveAds != 1 || p.Recommended.Shortlist != 1 || p.Recommended.Variants != 3 {
		t.Fatalf("plan = %+v", p)
	}
	if p.Recommended.Credits != 3 { // copy 1 + poster 2
		t.Fatalf("credits = %d", p.Recommended.Credits)
	}
	if len(p.Notes) == 0 {
		t.Fatal("a one-ad budget must say so")
	}
	if len(p.Alternatives) == 0 || p.Alternatives[len(p.Alternatives)-1].Label != "Copy only" {
		t.Fatalf("alternatives = %+v", p.Alternatives)
	}
}

func TestPlanBiggerBudgetVideoAndShortfall(t *testing.T) {
	p, err := Defaults().Plan(PlanInput{AdBudgetKES: 30000, Days: 10, Platforms: []string{"facebook", "instagram"}, Tier: TemplateVideo, CreditsAvailable: 20, HasPlan: false})
	if err != nil {
		t.Fatal(err)
	}
	// 15000 / (10*300) = 5 per platform -> 10 live, capped at MaxShortlist 5.
	if p.MaxLiveAds != 10 {
		t.Fatalf("max live = %d", p.MaxLiveAds)
	}
	// Spendable 20 - reserve 5 - copy 1 - launch 10 = 4; a 12-credit video does not fit.
	if p.MaxRenders != 0 || p.Recommended.Shortlist != 1 {
		t.Fatalf("renders = %d shortlist = %d", p.MaxRenders, p.Recommended.Shortlist)
	}
	// copy 1 + video 12 + launch 10 = 23 > 20.
	if p.Recommended.Credits != 23 || p.Shortfall != 3 || p.SuggestedBundle == nil || p.SuggestedBundle.Units != 100 {
		t.Fatalf("quote = %+v shortfall=%d bundle=%+v", p.Recommended, p.Shortfall, p.SuggestedBundle)
	}
	if p.Alternatives[0].Tier != Poster {
		t.Fatalf("first alternative must be posters: %+v", p.Alternatives[0])
	}
}

// The worked example in marketflow/docs/integrations/ugc-video-ads.md.
func TestPlanUGCWorkedExample(t *testing.T) {
	r := Defaults()
	tiktok, _ := r.Plan(PlanInput{AdBudgetKES: 6000, Days: 7, Platforms: []string{"tiktok"}, Tier: Presenter, CreditsAvailable: 430, HasPlan: true})
	if tiktok.MaxLiveAds != 0 || len(tiktok.Notes) == 0 {
		t.Fatalf("TikTok minimum must be flagged: %+v", tiktok)
	}
	p, err := r.Plan(PlanInput{AdBudgetKES: 6000, Days: 7, Platforms: []string{"facebook", "instagram"}, Tier: Presenter, CreditsAvailable: 430, HasPlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxLiveAds != 2 || p.Recommended.Shortlist != 2 || p.Recommended.Variants != 6 || p.Recommended.Credits != 113 {
		t.Fatalf("recommended = %+v (max live %d)", p.Recommended, p.MaxLiveAds)
	}
	if p.Alternatives[0].Credits != 5 || p.Alternatives[1].Credits != 1 {
		t.Fatalf("alternatives = %+v", p.Alternatives)
	}
	faceless, _ := r.Plan(PlanInput{AdBudgetKES: 6000, Days: 7, Platforms: []string{"facebook", "instagram"}, Tier: TemplateVideo, CreditsAvailable: 430, HasPlan: true})
	if faceless.Recommended.Credits != 25 {
		t.Fatalf("faceless = %d", faceless.Recommended.Credits)
	}
}

func TestPlanUnknownBalanceOrganicAndCaps(t *testing.T) {
	p, err := Defaults().Plan(PlanInput{CreditsAvailable: -1, PlanCap: 2, HasPlan: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Recommended.Shortlist != 2 || p.Recommended.Variants != 6 || p.Shortfall != 0 {
		t.Fatalf("organic capped plan = %+v", p.Recommended)
	}
	// TikTok minimum is too high for KES 5,000 over 7 days.
	p, _ = Defaults().Plan(PlanInput{AdBudgetKES: 5000, Days: 7, Platforms: []string{"tiktok"}, CreditsAvailable: 200, HasPlan: true})
	if p.MaxLiveAds != 0 || len(p.Notes) == 0 {
		t.Fatalf("tiktok minimum not flagged: %+v", p)
	}
	// A tier outside the tenant's features is refused.
	if _, err := Defaults().Plan(PlanInput{Tier: Presenter, Features: map[string]bool{"ai_static_ads": true}}); err == nil {
		t.Fatal("a presenter tier without the feature must be refused")
	}
	if _, err := Defaults().Plan(PlanInput{Tier: CopyBatch}); err == nil {
		t.Fatal("a text output is not a media tier")
	}
}
