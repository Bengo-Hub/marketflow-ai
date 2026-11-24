package media

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func brandScenes() []ScriptScene {
	return []ScriptScene{
		{ID: "s1", Shot: ShotBRoll, Visual: "kids at laptops", Setting: "a bright learning room"},
		{ID: "s2", Shot: ShotBRoll, Visual: "parents walk up", Setting: "Oginga Odinga Road outside, matatus passing"},
		{ID: "s3", Shot: ShotProduct, Visual: "the robotics kit"},
		{ID: "s4", Shot: ShotPresenter},
		{ID: "s5", Shot: ShotEndCard, Visual: "a mother and son smile"},
	}
}

func TestPlanPlacements(t *testing.T) {
	all := func(string) bool { return true }
	ids := func(ps []Placement) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.SceneID+":"+p.Kind)
		}
		return strings.Join(s, ",")
	}
	if got := ids(planPlacements(brandScenes(), DefaultBrandingRules(), true, all)); got != "s2:establishing,actor:presenter,s5:end_card" {
		t.Fatalf("placements %s", got)
	}
	// Product close-ups never carry one, the cap holds and a kept actor gets none.
	if got := ids(planPlacements(brandScenes(), BrandingRules{Enabled: true, MaxScenes: 2}, false, all)); got != "s2:establishing,s5:end_card" {
		t.Fatalf("capped %s", got)
	}
	if planPlacements(brandScenes(), BrandingRules{}, true, all) != nil {
		t.Fatal("disabled rules placed a logo")
	}
	// Only scenes with a generated still can be given a blank surface.
	if got := ids(planPlacements(brandScenes(), BrandingRules{Enabled: true, Kinds: []string{BrandEndCard}}, true, func(id string) bool { return id != "s5" })); got != "" {
		t.Fatalf("a photo end card got %s", got)
	}
}

func TestParseQuad(t *testing.T) {
	q, ok := parseQuad("Here: {\"found\":true,\"corners\":[[0.4,0.2],[0.6,0.2],[0.6,0.7],[0.4,0.7]]}")
	if !ok || q[2] != [2]float64{0.6, 0.7} {
		t.Fatalf("quad %v %v", q, ok)
	}
	for _, bad := range []string{
		`{"found":false}`,
		`{"found":true,"corners":[[0.4,0.2],[0.6,0.7],[0.6,0.2],[0.4,0.7]]}`,     // crossed
		`{"found":true,"corners":[[0.5,0.5],[0.51,0.5],[0.51,0.51],[0.5,0.51]]}`, // too small
		`{"found":true,"corners":[[0,0],[1,0],[1,1],[0,1]]}`,                     // the whole frame
		`{"found":true,"corners":[[0.4,0.2],[1.6,0.2],[0.6,0.7],[0.4,0.7]]}`,     // off the frame
		`not json`,
	} {
		if _, ok := parseQuad(bad); ok {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// The real logo lands inside the surface, shaded by it, and nothing outside it changes.
func TestPlaceOnSurface(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{90, 90, 90, 255}), image.Point{}, draw.Src)
	banner := image.Rect(400, 200, 600, 700)
	draw.Draw(canvas, banner, image.NewUniform(color.RGBA{0xf3, 0xe8, 0xff, 0xff}), image.Point{}, draw.Src)
	before := image.NewRGBA(canvas.Bounds())
	draw.Draw(before, before.Bounds(), canvas, image.Point{}, draw.Src)
	logo := inkLogo(codevertexPurple)
	q := [4][2]float64{{0.4, 0.2}, {0.6, 0.2}, {0.6, 0.7}, {0.4, 0.7}}
	if !placeOnSurface(canvas, analyseLogo(logo).img, q, 0.18) {
		t.Fatal("not placed")
	}
	purple, minX, maxX := 0, 1000, 0
	for y := 0; y < 1000; y++ {
		for x := 0; x < 1000; x++ {
			c := canvas.RGBAAt(x, y)
			if c == before.RGBAAt(x, y) {
				continue
			}
			if !(image.Point{x, y}).In(banner) {
				t.Fatalf("pixel %d,%d outside the banner changed", x, y)
			}
			if c.R < 160 && c.B > c.G {
				purple++
				minX, maxX = min(minX, x), max(maxX, x)
			}
		}
	}
	if purple < 2000 || maxX-minX > 180 {
		t.Fatalf("%d logo pixels spanning %d px", purple, maxX-minX)
	}
	if placeOnSurface(canvas, nil, q, 0.18) {
		t.Fatal("placed a missing logo")
	}
}

func TestColourName(t *testing.T) {
	for c, want := range map[color.RGBA]string{
		{0xf3, 0xe8, 0xff, 0xff}: "soft lavender", {0xff, 0xff, 0xff, 0xff}: "white", {0x14, 0x14, 0x18, 0xff}: "charcoal",
		{0x1e, 0x6b, 0x2f, 0xff}: "green", {0x0a, 0x1f, 0x5c, 0xff}: "deep blue",
	} {
		if got := colourName(c); got != want {
			t.Fatalf("%v: %q, want %q", c, got, want)
		}
	}
}

// A storyboard with branding asks for a blank surface in the planned scenes, places the real
// logo where the vision model finds it, and keeps the plain still when the review fails.
func TestStoryboardPlacesTheLogo(t *testing.T) {
	for _, pass := range []bool{true, false} {
		SetReviewer(NewReviewer(func(_ context.Context, system, user string, images []string) (string, string, float64, error) {
			if system == locatePrompt {
				return `{"found":true,"corners":[[0.4,0.2],[0.6,0.2],[0.6,0.7],[0.4,0.7]]}`, "fake/vision", 0.001, nil
			}
			if !pass && strings.Contains(user, "logo printed on") {
				return `{"pass":false,"issues":[{"frame":"sb-s2","kind":"text_artefact","severity":"high","note":"smeared"}]}`, "fake/vision", 0.001, nil
			}
			return `{"pass":true,"issues":[]}`, "fake/vision", 0.001, nil
		}, nil))
		f := newFakeProviders(t)
		store := newServingStore(t)
		s := NewStoryboards(keysFor("higgsfield"), store, NewPosters(keysFor("higgsfield"), store, nil), nil)
		rules := DefaultBrandingRules()
		res, err := s.Make(context.Background(), StoryboardRequest{TenantID: [16]byte{1}, VersionID: "v1", Format: "1920x1080", Kind: AdTemplate,
			Scenes: brandScenes()[:2], LogoURL: f.srv.URL + "/scene.png", Branding: &rules})
		SetReviewer(nil)
		if err != nil {
			t.Fatal(err)
		}
		st := res.Stills[1]
		if !strings.Contains(st.Prompt, "completely blank") || strings.Contains(res.Stills[0].Prompt, "banner") {
			t.Fatalf("prompts %q / %q", st.Prompt, res.Stills[0].Prompt)
		}
		if pass && (st.Logo == nil || st.PlainURL == "" || st.URL == st.PlainURL) {
			t.Fatalf("placed still %+v", st)
		}
		if !pass && (st.Logo != nil || st.PlainURL != "") {
			t.Fatalf("a failed review kept the placement %+v", st)
		}
	}
}

// servingStore stores like memStore and serves what it stored, so a pipeline can read back the
// stills it drew.
type servingStore struct {
	memStore
	srv *httptest.Server
}

func newServingStore(t *testing.T) *servingStore {
	s := &servingStore{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		data, ok := s.puts[strings.TrimPrefix(r.URL.Path, "/")]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *servingStore) PutPoster(ctx context.Context, tenantID uuid.UUID, name string, data []byte) (string, string, error) {
	key, _, err := s.memStore.PutPoster(ctx, tenantID, name, data)
	return key, s.srv.URL + "/" + key, err
}

// A still vouched for without a prompt is not reused for a scene that now gets a banner.
func TestVouchedStillRedrawnForAPlacement(t *testing.T) {
	rules := DefaultBrandingRules()
	req := StoryboardRequest{Kind: AdTemplate, Format: "1920x1080", Scenes: brandScenes()[:2],
		Previous: []Still{{SceneID: "s1", Source: StillGenerated, URL: "https://x/1.jpg"}, {SceneID: "s2", Source: StillGenerated, URL: "https://x/2.jpg"}}}
	req.brand = &brandPlan{rules: rules, placements: map[string]Placement{"s2": {SceneID: "s2", Kind: BrandEstablishing, Surface: "a blank banner"}}}
	stills, generated := (&Storyboards{}).Plan(req)
	if generated != 1 || !stills[0].Reused || stills[1].Reused {
		t.Fatalf("stills %+v generated %d", stills, generated)
	}
}

func TestStillTimeoutIsASetting(t *testing.T) {
	if p := DefaultRoutePolicy(); p.StillTimeoutSeconds != 180 {
		t.Fatalf("default %d", p.StillTimeoutSeconds)
	}
	if p := PolicyFromSettings(map[string]string{"still_timeout_seconds": "240"}); p.StillTimeoutSeconds != 240 {
		t.Fatalf("set %d", p.StillTimeoutSeconds)
	}
	for _, bad := range []string{"5", "9000", "soon"} {
		if p := PolicyFromSettings(map[string]string{"still_timeout_seconds": bad}); p.StillTimeoutSeconds != 180 {
			t.Fatalf("%q gave %d", bad, p.StillTimeoutSeconds)
		}
	}
}
