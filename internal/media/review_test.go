package media

import (
	"context"
	"image"
	"image/color"
	"strings"
	"testing"
)

func TestCleanContacts(t *testing.T) {
	got := CleanContacts([]Contact{
		{Kind: "WhatsApp", Value: " 0712 345 678 "},
		{Kind: "fax", Value: "1"},
		{Kind: "web", Value: "https://codevertexitsolutions.com/"},
		{Kind: "whatsapp", Value: "0712 345 678"},
		{Kind: "phone", Value: ""},
		{Kind: "email", Value: "hello@example.com"},
		{Kind: "link", Value: "https://digitika.example/demo"},
		{Kind: "phone", Value: "0700 000 000"},
	})
	if len(got) != 4 {
		t.Fatalf("kept %d contacts: %+v", len(got), got)
	}
	if got[0].Kind != ContactWhatsApp || got[0].Value != "0712 345 678" || got[1].Value != "codevertexitsolutions.com" {
		t.Fatalf("contacts not cleaned: %+v", got)
	}
}

// No poster or end card draws a button: no row near the bottom is a solid bar of the accent colour.
func TestPosterHasNoButton(t *testing.T) {
	accent := color.RGBA{0x10, 0x80, 0x40, 0xff}
	scene := solid(800, 800, color.RGBA{0xc0, 0xc0, 0xc0, 0xff})
	for _, format := range []string{"1920x1080", "1080x1920"} {
		img, err := ComposePoster(scene, format, Overlay{Headline: "Close the day in five minutes", CTA: "WhatsApp us for a demo",
			Contacts: []Contact{{Kind: ContactWhatsApp, Value: "0712 345 678"}, {Kind: ContactWeb, Value: "codevertexitsolutions.com"}}, Accent: accent})
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		for y := b.Max.Y * 7 / 10; y < b.Max.Y; y++ {
			n := 0
			for x := 0; x < b.Max.X; x++ {
				r, g, bl, _ := img.At(x, y).RGBA()
				if uint8(r>>8) == accent.R && uint8(g>>8) == accent.G && uint8(bl>>8) == accent.B {
					n++
				}
			}
			if n > b.Max.X/2 {
				t.Fatalf("%s: row %d is a solid accent bar (%d of %d pixels): a button", format, y, n, b.Max.X)
			}
		}
	}
}

// The logo goes to the calmer top corner, away from a busy or skin-toned area.
func TestLogoGoesToTheCalmerCorner(t *testing.T) {
	scene := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	for y := 0; y < 1080; y++ {
		for x := 0; x < 1920; x++ {
			c := color.RGBA{0x30, 0x60, 0x90, 0xff}
			if x < 960 && (x/6+y/6)%2 == 0 {
				c = color.RGBA{0xf0, 0xf0, 0xf0, 0xff} // a busy pattern on the left
			}
			scene.Set(x, y, c)
		}
	}
	o := Overlay{Logo: image.NewRGBA(image.Rect(0, 0, 200, 80))}
	if got := AutoLogoCorner(scene, "1920x1080", o); got != LogoTopRight {
		t.Fatalf("corner = %s, want the calm right corner", got)
	}
	o.LogoCorner = LogoTopLeft
	if got := AutoLogoCorner(scene, "1920x1080", o); got != LogoTopLeft {
		t.Fatalf("a pinned corner must stay, got %s", got)
	}
}

func TestParseVerdict(t *testing.T) {
	v, err := parseVerdict("Here: {\"pass\":true,\"issues\":[{\"frame\":\"poster\",\"kind\":\"logo\",\"severity\":\"high\",\"note\":\"on a face\"},{\"frame\":\"s2\",\"kind\":\"weird\",\"severity\":\"low\"}]}")
	if err != nil {
		t.Fatal(err)
	}
	if v.Pass || len(v.Issues) != 2 || v.Issues[1].Kind != IssueOther {
		t.Fatalf("verdict %+v: a high issue fails and unknown kinds become other", v)
	}
	if _, err := parseVerdict("no json"); err == nil {
		t.Fatal("a reply without JSON must fail")
	}
}

func TestReviewSettingsFromMap(t *testing.T) {
	s := ReviewSettingsFromMap(map[string]string{"enabled": "false", "max_retries": "9", "poster_retry_usd": "0.2", "video_frames": "x"})
	if s.Enabled || s.MaxRetries != maxReviewRetries || s.PosterRetryUSD != 0.2 || s.VideoFrames != DefaultReviewFrames {
		t.Fatalf("settings %+v", s)
	}
	if d := ReviewSettingsFromMap(nil); !d.Enabled || d.MaxRetries != DefaultReviewRetries {
		t.Fatalf("defaults %+v", d)
	}
}

// A poster whose logo the reviewer rejects is laid out again for free, then passes.
func TestPosterReviewRelaysOutTheLogo(t *testing.T) {
	calls := 0
	SetReviewer(NewReviewer(func(_ context.Context, system, user string, images []string) (string, string, float64, error) {
		calls++
		if len(images) != 1 || !strings.HasPrefix(images[0], "data:image/jpeg;base64,") || !strings.Contains(user, "Kisumu") {
			t.Fatalf("review input: %d images, user %q", len(images), user)
		}
		if calls == 1 {
			return `{"pass":false,"issues":[{"frame":"poster","kind":"logo","severity":"high","note":"on the face"}]}`, "fake/vision", 0.001, nil
		}
		return `{"pass":true,"issues":[]}`, "fake/vision", 0.001, nil
	}, nil))
	defer SetReviewer(nil)
	p := NewPosters(nil, nil, nil)
	req := PosterRequest{Format: "1080x1080", Prompt: "a till in a Kisumu shop", Place: "Kisumu CBD",
		Overlay: Overlay{Headline: "Duka POS", Logo: image.NewRGBA(image.Rect(0, 0, 100, 40))}}
	scene := solid(800, 800, color.RGBA{0x80, 0x80, 0x80, 0xff})
	_, _, out, rep, extra := p.reviewPoster(context.Background(), PosterRoutes[0], req, scene, "https://x/scene.jpg")
	if rep == nil || !rep.Passed || rep.Rounds != 2 || extra != 0 || len(rep.Fixed) != 1 {
		t.Fatalf("report %+v, extra %v", rep, extra)
	}
	if out.Overlay.LogoCorner == "" {
		t.Fatal("the retry must pin the logo to the other corner")
	}
}

func TestSceneSettingReachesPrompts(t *testing.T) {
	sc := ScriptScene{ID: "s1", Shot: ShotBRoll, Visual: "A girl codes on a laptop.", Setting: "a learning hub, Kisumu CBD; afternoon light"}
	if got := sceneVisual(sc); !strings.Contains(got, "Setting: a learning hub, Kisumu CBD") {
		t.Fatalf("still visual %q", got)
	}
	if p := StillPrompt("1920x1080", DeliveryUGC, sceneVisual(sc)); !strings.Contains(p, "Kisumu CBD") {
		t.Fatalf("still prompt %q", p)
	}
	if got := sceneText(ScriptScene{Visual: "x"}); got != "x" {
		t.Fatalf("a scene without a setting is unchanged, got %q", got)
	}
	p := ActorPromptIn("1920x1080", "a Kenyan mother in her thirties", "a modern family home in Kisumu")
	if !strings.Contains(p, "in a modern family home in Kisumu") || strings.Contains(p, "plain uncluttered background") {
		t.Fatalf("actor prompt %q", p)
	}
	if !strings.Contains(ActorPrompt("1920x1080", "a man"), "plain uncluttered background") {
		t.Fatal("without a setting the actor keeps the plain background")
	}
}

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

// Picture prompts never ask the image model for what the overlay draws.
func TestPictureOnlyDropsOverlayWords(t *testing.T) {
	got := sceneVisual(ScriptScene{Shot: ShotEndCard, Visual: "Digitika logo with contact details, WhatsApp icon",
		Setting: "Pioneer House reception, Kisumu CBD; calm evening; presenter by the Codevertex logo; M-Pesa icon"})
	for _, w := range []string{"logo", "icon", "WhatsApp", "contact"} {
		if strings.Contains(got, w) {
			t.Fatalf("%q still asks for %s", got, w)
		}
	}
	if !strings.Contains(got, "Pioneer House reception") {
		t.Fatalf("the place was lost: %q", got)
	}
	if v := pictureOnly("A customer taps a phone on the counter to pay"); v != "A customer taps a phone on the counter to pay" {
		t.Fatalf("a plain action changed: %q", v)
	}
}
