package media

import (
	"context"
	"image"
	"image/color"
	"strings"
	"testing"
)

// Tests for the 2026-11-19 media pipeline audit fixes (findings 4 to 7).

func TestFrameChoiceFollowsTheRule(t *testing.T) {
	photos := []string{"https://x/offer.jpg", "https://x/lib-logo.png", "https://x/lib-shop.jpg", "https://x/lib-plain.jpg"}
	info := []PhotoInfo{{}, {Library: true, Logo: true, Caption: "kids coding class laptop"},
		{Library: true, Caption: "Shop counter with a till and a cashier", Tags: []string{"retail"}}, {Library: true}}
	c := newFrameChooser(photos, nil, info)

	if f := c.choose(ScriptScene{Shot: ShotPresenter}); f.Source != FrameActor {
		t.Fatalf("presenter: %+v", f)
	}
	// A product shot takes the offering's own photo, even when the script points at a library picture.
	if f := c.choose(ScriptScene{Shot: ShotProduct, Asset: 3}); f.Source != FramePhoto || f.URL != photos[0] || f.Origin != OriginOffering {
		t.Fatalf("product: %+v", f)
	}
	// A logo is never a frame, even when its caption matches the scene.
	if f := c.choose(ScriptScene{Shot: ShotBRoll, Visual: "Kids coding in a class on a laptop"}); f.Source != FrameGenerated {
		t.Fatalf("logo used as a frame: %+v", f)
	}
	// A library picture is used when its caption clearly matches.
	if f := c.choose(ScriptScene{Shot: ShotBRoll, Visual: "A busy shop counter, the cashier at the till"}); f.Source != FramePhoto || f.URL != photos[2] || f.Origin != OriginLibrary {
		t.Fatalf("matching library picture not used: %+v", f)
	}
	// A tag that names the scene is enough.
	if f := c.choose(ScriptScene{Shot: ShotBRoll, Visual: "Retail owner smiling"}); f.URL != photos[2] {
		t.Fatalf("tag match: %+v", f)
	}
	// One shared word, or a picture without a caption, is not a match: a still is generated.
	if f := c.choose(ScriptScene{Shot: ShotBRoll, Visual: "A shop owner closing up at night"}); f.Source != FrameGenerated {
		t.Fatalf("weak match used: %+v", f)
	}
	// Without descriptions every photo counts as the offering's, and b-roll is still generated.
	plain := newFrameChooser([]string{"https://x/a.jpg", "https://x/b.jpg"}, nil, nil)
	if f := plain.choose(ScriptScene{Shot: ShotBRoll}); f.Source != FrameGenerated {
		t.Fatalf("b-roll from a photo in turn: %+v", f)
	}
	if a, b := plain.choose(ScriptScene{Shot: ShotProduct}), plain.choose(ScriptScene{Shot: ShotProduct}); a.URL == b.URL {
		t.Fatalf("offering photos not taken in turn: %s %s", a.URL, b.URL)
	}
}

func TestStoryboardPlanUsesTheRuleAndTheShape(t *testing.T) {
	s := &Storyboards{}
	req := StoryboardRequest{Format: "1920x1080", Kind: AdPresenter, Delivery: DeliveryUGC,
		Photos: []string{"https://x/offer.jpg", "https://x/logo.png"}, PhotoInfo: []PhotoInfo{{}, {Library: true, Logo: true}},
		Scenes: []ScriptScene{{ID: "s1", Shot: ShotPresenter}, {ID: "s2", Shot: ShotBRoll, Visual: "A classroom of kids"},
			{ID: "s3", Shot: ShotProduct}, {ID: "s4", Shot: ShotEndCard}}}
	stills, generated := s.Plan(req)
	if generated != 2 { // the b-roll still and the actor
		t.Fatalf("generated = %d", generated)
	}
	if stills[1].Source != StillGenerated || !strings.Contains(stills[1].Prompt, "landscape 16:9") || strings.Contains(stills[1].Prompt, "vertical") {
		t.Fatalf("b-roll still: %+v", stills[1])
	}
	if stills[2].URL != "https://x/offer.jpg" || stills[3].URL != "https://x/offer.jpg" {
		t.Fatalf("product frames: %+v", stills)
	}
	if p := ActorPrompt("1920x1080", "a man"); !strings.HasPrefix(p, "Landscape 16:9") || strings.Contains(p, "Vertical") {
		t.Fatalf("landscape actor prompt: %s", p)
	}
	if p := ActorPrompt("1080x1920", "a man"); !strings.HasPrefix(p, "Vertical selfie-style") {
		t.Fatalf("vertical actor prompt: %s", p)
	}
}

func TestLandscapeSizesEverywhere(t *testing.T) {
	if s, ok := Formats["1920x1080"]; !ok || s != (image.Point{X: 1920, Y: 1080}) || !Landscape("1920x1080") || Landscape("1080x1920") {
		t.Fatalf("formats: %v", Formats)
	}
	if aspectFor("1920x1080") != "16:9" || soulAspect("1920x1080") != "16:9" || borealAspect("1920x1080") != "16:9" ||
		borealAspect("1080x1920") != "9:16" || borealAspect("1080x1350") != "3:4" {
		t.Fatal("aspect ratios")
	}
	if sz := seedreamSize("1920x1080"); sz["width"] != 3840 || sz["height"] != 2160 {
		t.Fatalf("seedream size: %v", sz)
	}
	// Type is sized from the short side: a landscape caption is no bigger than a portrait one.
	if layoutUnit(1920, 1080) != 1080 || layoutUnit(1080, 1920) != 1080 {
		t.Fatal("layout unit")
	}
	for _, f := range []string{"1920x1080", "1080x1920"} {
		if _, err := ComposeCaption(f, "Close the day in five minutes"); err != nil {
			t.Fatal(err)
		}
		if _, err := ComposeOverlay(f, Overlay{Headline: "Duka POS", Price: "KES 2,500", CTA: "Chat on WhatsApp"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFitFrameKeepsTheWholePicture(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 600)) // portrait, bright red
	for y := 0; y < 600; y++ {
		for x := 0; x < 300; x++ {
			src.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	size := Formats["1920x1080"]
	out := FitFrame(src, size)
	if out.Bounds().Dx() != 1920 || out.Bounds().Dy() != 1080 {
		t.Fatalf("size %v", out.Bounds())
	}
	// The picture is fitted by height, so its top and bottom rows are in the frame, centred.
	if c := out.RGBAAt(960, 2); c.R < 200 {
		t.Fatalf("top of the picture cropped: %v", c)
	}
	if c := out.RGBAAt(960, 1077); c.R < 200 {
		t.Fatalf("bottom of the picture cropped: %v", c)
	}
	// The sides are the darkened fill.
	if c := out.RGBAAt(20, 540); c.R > 230 {
		t.Fatalf("side is not the darkened fill: %v", c)
	}
	if !aspectClose(1920, 1080, size) || aspectClose(1080, 1920, size) {
		t.Fatal("aspectClose")
	}
}

func TestTextSceneGoesToANegativePromptRoute(t *testing.T) {
	if !showsWriting("Owner checks the sales dashboard on a laptop") || !showsWriting("A shop sign at dusk") || showsWriting("Kids laughing in a park") {
		t.Fatal("showsWriting")
	}
	c := NewClips(keysFor("fal", "higgsfield"), nil, nil, nil)
	plain := c.order(context.Background(), ScenarioAdClip, TierStandard, 6, false)
	avoid := c.order(context.Background(), ScenarioAdClip, TierStandard, 6, true)
	if len(avoid) == 0 || !avoid[0].NegativePrompt {
		t.Fatalf("a text scene starts on %v", avoid)
	}
	if len(plain) == 0 || plain[0].Key != "fal/hailuo-2.3-fast" || len(plain) != len(avoid) {
		t.Fatalf("plain order changed: %v", plain)
	}
	rt, _ := clipRoute(avoid[0].Key)
	_, input, err := clipInput(rt, ClipRequest{SceneURL: "https://x/f.jpg", Seconds: 6, Prompt: "laptop"})
	if err != nil || input["negative_prompt"] != clipNegativePrompt {
		t.Fatalf("negative prompt not sent: %v %v", input, err)
	}
}

func TestPresenterAskedForInTheAdShape(t *testing.T) {
	p, n := presenterPromptFor("fal/creatify-boreal", "At a desk")
	if !strings.Contains(p, "[VISUAL]\nAt a desk") || n == "" || strings.Contains(p, "selfie") {
		t.Fatalf("boreal prompt: %q %q", p, n)
	}
}

func TestAdPlanShowsTheGenerationPlan(t *testing.T) {
	a := NewAds(nil, nil).WithPresenters(NewPresenters(keysFor("fal", "higgsfield"), nil)).WithClips(NewClips(keysFor("fal", "higgsfield"), nil, nil, nil), newSceneFiles(t))
	req := AdRequest{Format: "1920x1080", Kind: AdPresenter, ActorURL: "https://x/actor.jpg",
		Photos: []string{"https://x/offer.jpg"}, Stills: map[string]string{"s2": "https://x/still2.jpg"},
		Scenes: []ScriptScene{{ID: "s1", Shot: ShotPresenter, Voiceover: "Hello", Visual: "At a desk"},
			{ID: "s2", Shot: ShotBRoll, Voiceover: "Every sale counted", Visual: "A cashier at the till screen"},
			{ID: "s3", Shot: ShotProduct, Voiceover: "Duka POS"}}}
	plan, err := a.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	s1, s2, s3 := plan.Scenes[0], plan.Scenes[1], plan.Scenes[2]
	if s1.Frame != FrameActor || s1.FrameURL != "https://x/actor.jpg" || s1.Prompt == "" {
		t.Fatalf("presenter plan: %+v", s1)
	}
	if s2.Frame != FrameGenerated || s2.FrameURL != "https://x/still2.jpg" || !s2.AvoidText || s2.NegativePrompt == "" ||
		!strings.Contains(s2.Prompt, "no subtitles") || !strings.Contains(s2.StillPrompt, "Landscape 16:9") {
		t.Fatalf("b-roll plan: %+v", s2)
	}
	if s3.Frame != FramePhoto || s3.Origin != OriginOffering || s3.FrameURL != "https://x/offer.jpg" || s3.Work != WorkClip {
		t.Fatalf("product plan: %+v", s3)
	}
	// A b-roll scene with no still and no matching picture cannot render before the storyboard.
	delete(req.Stills, "s2")
	a.asm, a.voices = &Assembler{ffmpeg: "ffmpeg", store: &fileStore{}}, &Voices{}
	if err := a.Validate(req); err == nil || !strings.Contains(err.Error(), "storyboard") {
		t.Fatalf("missing still: %v", err)
	}
}

func TestMusicDuckedAtEveryLevel(t *testing.T) {
	cfg := DefaultAudioConfig()
	d := cfg.Ducking()
	g := strings.Join(mixGraph(mixInputs{voice: true, music: true, total: 20, mix: AudioMix{Duck: &d}}), ";")
	if !strings.Contains(g, "sidechaincompress") || !strings.Contains(g, "volume=0.25") {
		t.Fatalf("music not ducked: %s", g)
	}
}

func TestRoutingPolicyDefaultsAndOverrides(t *testing.T) {
	def := DefaultRoutePolicy()
	if def.Prefer["presenter/standard"][0] != "fal/creatify-boreal" || def.Prefer["ad_clip/standard"][0] != "fal/hailuo-2.3-fast" ||
		def.Prefer["ad_clip/premium"][0] != "higgsfield/kling-v3-std" || !def.PremiumForExempt || def.PremiumBudgetUSD <= 0 {
		t.Fatalf("defaults: %+v", def)
	}
	p := PolicyFromSettings(map[string]string{"prefer": `{"presenter/standard":["fal/veed-fabric"]}`,
		"premium_for_exempt": "false", "premium_budget_usd": "3.5"})
	if p.Prefer["presenter/standard"][0] != "fal/veed-fabric" || p.Prefer["ad_clip/standard"][0] != "fal/hailuo-2.3-fast" ||
		p.PremiumForExempt || p.PremiumBudgetUSD != 3.5 {
		t.Fatalf("overrides: %+v", p)
	}
	if bad := PolicyFromSettings(map[string]string{"prefer": "{not json", "premium_budget_usd": "x"}); bad.PremiumBudgetUSD != def.PremiumBudgetUSD {
		t.Fatalf("bad settings: %+v", bad)
	}
	// A preference moves its route first, against the score.
	r := NewRouter(PresenterRoutes)
	got := r.OrderPreferred(CapPresenter, TierStandard, 8, nil, []string{"fal/veed-fabric"})
	if got[0].Key != "fal/veed-fabric" {
		t.Fatalf("preferred route not first: %v", got[0].Key)
	}
}

func TestPremiumForThePlatformOwnerFallsBackByScene(t *testing.T) {
	a := NewAds(nil, nil).WithPresenters(NewPresenters(keysFor("fal", "higgsfield"), nil)).
		WithClips(NewClips(keysFor("fal", "higgsfield"), nil, nil, nil), newSceneFiles(t))
	req := AdRequest{Format: "1920x1080", Kind: AdPresenter, ActorURL: "https://x/actor.jpg", Photos: []string{"https://x/offer.jpg"},
		Scenes: []ScriptScene{{ID: "s1", Shot: ShotPresenter, Voiceover: "Hello there, shop owners of Nairobi"},
			{ID: "s2", Shot: ShotProduct, Voiceover: "Duka POS counts every sale"}},
		AutoPremium: true}
	plan, err := a.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scenes[0].Tier != TierPremium || plan.Scenes[0].Route != "higgsfield/seedance-2.5-720p" ||
		plan.Scenes[1].Tier != TierPremium || plan.Scenes[1].Route != "higgsfield/kling-v3-std" {
		t.Fatalf("all premium: %+v", plan.Scenes)
	}
	req.PremiumBudgetUSD = 0.05
	cheap, err := a.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if cheap.Scenes[0].Tier != TierStandard || cheap.Scenes[0].Route != "fal/creatify-boreal" || cheap.CostUSD >= plan.CostUSD {
		t.Fatalf("over budget, scenes not moved to standard: %+v", cheap.Scenes)
	}
}
