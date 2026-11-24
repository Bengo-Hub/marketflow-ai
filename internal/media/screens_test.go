package media

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func screenScenes() []ScriptScene {
	return []ScriptScene{
		{ID: "s1", Shot: ShotBRoll, Visual: "a shop owner smiles at a laptop on the counter"},
		{ID: "s2", Shot: ShotBRoll, Visual: "a busy street outside"},
		{ID: "s3", Shot: ShotBRoll, Visual: "a customer holds a smartphone"},
		{ID: "s4", Shot: ShotBRoll, Visual: "the cashier taps the POS"},
	}
}

func TestPlanScreens(t *testing.T) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	shots := []Screenshot{{ID: "dash"}, {ID: "till", Device: DevicePOS}, {ID: "app"}}
	sizes := map[string]image.Point{"dash": {X: 1600, Y: 900}, "till": {X: 1200, Y: 800}, "app": {X: 800, Y: 1700}}
	ids := func(ps []Placement) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.SceneID+":"+p.Device+":"+p.ScreenID)
		}
		return strings.Join(s, ",")
	}
	// A phone takes the tall screenshot, the POS the one marked for it; the cap holds.
	if got := ids(planScreens(screenScenes(), ScreenRules{Enabled: true, MaxScenes: 3}, shots, sizes, all, none)); got != "s1:laptop:dash,s3:phone:app,s4:pos:till" {
		t.Fatalf("screens %s", got)
	}
	if got := ids(planScreens(screenScenes(), DefaultScreenRules(), shots, sizes, all, none)); got != "s1:laptop:dash,s3:phone:app" {
		t.Fatalf("default cap %s", got)
	}
	// None without screenshots, when off, or in a scene already carrying the logo.
	if planScreens(screenScenes(), DefaultScreenRules(), nil, sizes, all, none) != nil ||
		planScreens(screenScenes(), ScreenRules{}, shots, sizes, all, none) != nil {
		t.Fatal("planned a screen that must be off")
	}
	if got := ids(planScreens(screenScenes(), ScreenRules{Enabled: true, MaxScenes: 1}, shots, sizes, all, func(id string) bool { return id == "s1" })); got != "s3:phone:app" {
		t.Fatalf("taken %s", got)
	}
}

// The screenshot fills the screen and nothing else; a finger over the screen stays in front.
func TestPlaceOnScreen(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{60, 50, 40, 255}), image.Point{}, draw.Src)
	screen := image.Rect(300, 300, 700, 550)
	draw.Draw(canvas, screen, image.NewUniform(color.RGBA{225, 225, 228, 255}), image.Point{}, draw.Src)
	finger := image.Rect(600, 480, 640, 550)
	draw.Draw(canvas, finger, image.NewUniform(color.RGBA{90, 55, 40, 255}), image.Point{}, draw.Src)
	shot := image.NewRGBA(image.Rect(0, 0, 1600, 1000))
	draw.Draw(shot, shot.Bounds(), image.NewUniform(color.RGBA{20, 120, 60, 255}), image.Point{}, draw.Src)
	if !placeOnScreen(canvas, shot, [4][2]float64{{0.3, 0.3}, {0.7, 0.3}, {0.7, 0.55}, {0.3, 0.55}}) {
		t.Fatal("not placed")
	}
	green := 0
	for y := 0; y < 1000; y++ {
		for x := 0; x < 1000; x++ {
			c, in := canvas.RGBAAt(x, y), (image.Point{x, y}).In(screen)
			if c.G > c.R+40 {
				green++
				if !in {
					t.Fatalf("painted outside the screen at %d,%d", x, y)
				}
			}
		}
	}
	if green < 90000 || canvas.RGBAAt(620, 515) != (color.RGBA{90, 55, 40, 255}) {
		t.Fatalf("%d screenshot pixels, finger %v", green, canvas.RGBAAt(620, 515))
	}
	// A tall phone screenshot on a wide screen would lose most of itself.
	if placeOnScreen(canvas, image.NewRGBA(image.Rect(0, 0, 400, 1800)), [4][2]float64{{0.3, 0.3}, {0.7, 0.3}, {0.7, 0.55}, {0.3, 0.55}}) {
		t.Fatal("cropped away most of a screenshot")
	}
}

// A device scene carries the screenshot when it is found and passes; otherwise it is drawn again
// with no screen in view, never left blank.
func TestStoryboardPlacesScreens(t *testing.T) {
	shot := image.NewRGBA(image.Rect(0, 0, 1600, 1000))
	draw.Draw(shot, shot.Bounds(), image.NewUniform(color.RGBA{20, 120, 60, 255}), image.Point{}, draw.Src)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = png.Encode(w, shot) }))
	defer srv.Close()
	for name, locate := range map[string]string{
		"placed":    `{"found":true,"corners":[[0.3,0.3],[0.7,0.3],[0.7,0.6],[0.3,0.6]]}`,
		"not found": `{"found":false}`,
	} {
		SetReviewer(NewReviewer(func(_ context.Context, system, _ string, _ []string) (string, string, float64, error) {
			if system == locatePrompt {
				return locate, "fake/vision", 0.001, nil
			}
			return `{"pass":true,"issues":[]}`, "fake/vision", 0.001, nil
		}, nil))
		newFakeProviders(t)
		store := newServingStore(t)
		s := NewStoryboards(keysFor("higgsfield"), store, NewPosters(keysFor("higgsfield"), store, nil), nil)
		rules := DefaultScreenRules()
		res, err := s.Make(context.Background(), StoryboardRequest{TenantID: [16]byte{1}, VersionID: "v1", Format: "1920x1080", Kind: AdTemplate,
			Scenes: screenScenes()[:2], Screens: &rules, Screenshots: []Screenshot{{ID: "dash", URL: srv.URL + "/dash.png"}}})
		SetReviewer(nil)
		if err != nil {
			t.Fatal(name, err)
		}
		st := res.Stills[0]
		blank := strings.Contains(st.Prompt, "completely blank")
		if name == "placed" && (st.Screen == nil || st.Screen.ID != "dash" || !blank || st.PlainURL != "") {
			t.Fatalf("placed still %+v", st)
		}
		if name == "not found" && (st.Screen != nil || blank || !strings.Contains(st.Prompt, "no screen is visible") || !strings.Contains(st.URL, "noscreen")) {
			t.Fatalf("fallback still %+v", st)
		}
	}
}

// A still whose screenshot was removed is drawn again.
func TestRemovedScreenshotIsNotReused(t *testing.T) {
	req := StoryboardRequest{Kind: AdTemplate, Format: "1920x1080", Scenes: screenScenes()[:1],
		Previous: []Still{{SceneID: "s1", Source: StillGenerated, URL: "https://x/1.jpg", Screen: &PlacedScreen{ID: "gone"}}}}
	if _, generated := (&Storyboards{}).Plan(req); generated != 1 {
		t.Fatal("reused a still of a removed screenshot")
	}
}

// A scene carrying a screenshot is never sent to a clip route.
func TestStaticSceneIsNotAnimated(t *testing.T) {
	a := &Ads{}
	req := AdRequest{Kind: AdPresenter, Format: "1920x1080", Scenes: screenScenes()[:1], Stills: map[string]string{"s1": "https://x/1.jpg"},
		StaticScenes: []string{"s1"}}
	plan, err := a.Plan(context.Background(), req)
	if err != nil || plan.Scenes[0].Work != WorkStill || plan.Scenes[0].CostUSD != 0 {
		t.Fatalf("plan %+v err %v", plan, err)
	}
}
