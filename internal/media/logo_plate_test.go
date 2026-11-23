package media

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
)

// inkLogo is a logo of one ink colour on a transparent background, with wide empty margins.
func inkLogo(ink color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 400, 240))
	draw.Draw(img, image.Rect(100, 60, 300, 180), image.NewUniform(ink), image.Point{}, draw.Src)
	return img
}

// unpremul is a drawn plate colour back as the colour people see.
func unpremul(c color.RGBA) color.RGBA {
	if c.A == 0 {
		return c
	}
	k := 255 / float64(c.A)
	return color.RGBA{uint8(min(255, float64(c.R)*k+0.5)), uint8(min(255, float64(c.G)*k+0.5)), uint8(min(255, float64(c.B)*k+0.5)), 0xff}
}

var (
	codevertexPurple = color.RGBA{106, 36, 86, 255}
	white            = color.RGBA{255, 255, 255, 255}
)

func TestPlateFollowsTheLogosInk(t *testing.T) {
	cases := []struct {
		name  string
		ink   color.RGBA
		light bool
	}{
		{"dark purple logo", codevertexPurple, true},
		{"black logo", color.RGBA{0, 0, 0, 255}, true},
		{"white logo", white, false},
		{"pale yellow logo", color.RGBA{250, 240, 160, 255}, false},
	}
	for _, c := range cases {
		info := analyseLogo(inkLogo(c.ink))
		plate, ok := plateColour(info, PlateStyle{})
		if !ok {
			t.Fatalf("%s: no plate", c.name)
		}
		if got := relLum(unpremul(plate)) > 0.5; got != c.light {
			t.Fatalf("%s: light plate %v, want %v (plate %+v)", c.name, got, c.light, plate)
		}
		if r := info.readableOn(unpremul(plate)); r < 0.99 {
			t.Fatalf("%s: only %.2f of the logo reads on its plate", c.name, r)
		}
	}
}

// A mid-tone logo and a mixed one (dark mark, mid-grey strapline) get the plate most of it reads on.
func TestPlateForMidAndMixedLogos(t *testing.T) {
	mid := analyseLogo(inkLogo(color.RGBA{128, 128, 128, 255}))
	plate, _ := plateColour(mid, PlateStyle{})
	if r := mid.readableOn(unpremul(plate)); r < 0.99 {
		t.Fatalf("a grey logo reads %.2f on its plate", r)
	}
	mixed := inkLogo(codevertexPurple)
	draw.Draw(mixed, image.Rect(100, 150, 300, 180), image.NewUniform(color.RGBA{120, 120, 120, 255}), image.Point{}, draw.Src)
	info := analyseLogo(mixed)
	plate, _ = plateColour(info, PlateStyle{})
	if relLum(unpremul(plate)) < 0.7 {
		t.Fatalf("a mostly dark logo with a grey strapline got a dark plate %+v", plate)
	}
}

// Owner, 2026-11-24: codevertex's dark logo sat on a near-black plate. The old plate was an
// invalid premultiplied colour, which the PNG encoder wraps to near black; the plate must stay
// light through the PNG the render overlays.
func TestLightPlateSurvivesPNG(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 600, 400))
	info := analyseLogo(inkLogo(codevertexPurple))
	r := image.Rect(100, 100, 300, 220)
	drawLogo(canvas, info, r, PlateStyle{})
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// Inside the plate's padding, outside the logo.
	c := color.NRGBAModel.Convert(img.At(r.Min.X-8, r.Min.Y+40)).(color.NRGBA)
	if c.R < 230 || c.G < 230 || c.B < 230 || c.A < 200 || c.A > 235 {
		t.Fatalf("plate after PNG is %+v, want white at about 86%%", c)
	}
}

func TestPlateBrandTints(t *testing.T) {
	info := analyseLogo(inkLogo(codevertexPurple))
	// No brand colours: neutral white.
	if p, _ := plateColour(info, PlateStyle{}); unpremul(p) != white {
		t.Fatalf("neutral plate %+v", unpremul(p))
	}
	// The brand's lightest colour, light enough to be a plate, is the plate.
	lavender := color.RGBA{0xf3, 0xe8, 0xff, 0xff}
	p, _ := plateColour(info, PlateStyle{Tints: []string{"#6a2456", "#f3e8ff", "not a colour"}})
	if got := unpremul(p); diff(got, lavender) > 2 {
		t.Fatalf("plate %+v, want the brand's lightest colour %+v", got, lavender)
	}
	// Only dark brand colours: a light tint of them, still light.
	p, _ = plateColour(info, PlateStyle{Tints: []string{"#6a2456"}})
	if got := unpremul(p); got == white || relLum(got) < 0.7 {
		t.Fatalf("tinted plate %+v", got)
	}
	// A dark plate takes a deep shade of the brand colour.
	light := analyseLogo(inkLogo(white))
	p, _ = plateColour(light, PlateStyle{Tints: []string{"#6a2456"}})
	if got := unpremul(p); diff(got, neutralDark) == 0 || relLum(got) > 0.02 {
		t.Fatalf("dark tinted plate %+v", got)
	}
	// A brand colour that would cost the logo its contrast is not used.
	grey := analyseLogo(inkLogo(color.RGBA{140, 140, 140, 255}))
	p, _ = plateColour(grey, PlateStyle{Style: PlateLight, Tints: []string{"#e6e6e6"}})
	if got := unpremul(p); diff(got, color.RGBA{0xe6, 0xe6, 0xe6, 0xff}) <= 2 || grey.readableOn(got) < 0.95*grey.readableOn(white) {
		t.Fatalf("plate %+v loses the grey logo's contrast", got)
	}
}

func diff(a, b color.RGBA) int {
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	return max(abs(int(a.R)-int(b.R)), abs(int(a.G)-int(b.G)), abs(int(a.B)-int(b.B)))
}

func TestPlateSettings(t *testing.T) {
	info := analyseLogo(inkLogo(codevertexPurple))
	if _, ok := plateColour(info, PlateStyle{Style: PlateNone}); ok {
		t.Fatal("style none still draws a plate")
	}
	if p, _ := plateColour(info, PlateStyle{Style: PlateDark}); relLum(unpremul(p)) > 0.1 {
		t.Fatalf("style dark gave %+v", p)
	}
	if p, _ := plateColour(info, PlateStyle{Opacity: 0.8}); p.A != 204 {
		t.Fatalf("opacity 0.8 gave alpha %d", p.A)
	}
	if p, _ := plateColour(info, PlateStyle{Opacity: 3}); p.A != 219 {
		t.Fatalf("an out of range opacity gave alpha %d, want the default", p.A)
	}
}

// A logo with its own background gets a thin border, not a second box; a transparent logo is
// trimmed to its ink.
func TestOpaqueLogoGetsABorderAndTransparentOneIsTrimmed(t *testing.T) {
	boxed := image.NewRGBA(image.Rect(0, 0, 300, 150))
	draw.Draw(boxed, boxed.Bounds(), image.NewUniform(white), image.Point{}, draw.Src)
	draw.Draw(boxed, image.Rect(50, 40, 250, 110), image.NewUniform(codevertexPurple), image.Point{}, draw.Src)
	info := analyseLogo(boxed)
	if !info.opaqueBox || info.img.Bounds() != boxed.Bounds() {
		t.Fatalf("boxed logo: opaque %v bounds %v", info.opaqueBox, info.img.Bounds())
	}
	canvas := image.NewRGBA(image.Rect(0, 0, 600, 400))
	r := image.Rect(200, 200, 380, 290)
	drawLogo(canvas, info, r, PlateStyle{})
	if canvas.RGBAAt(r.Min.X-1, r.Min.Y+40).A == 0 {
		t.Fatal("no border round a boxed logo")
	}
	if canvas.RGBAAt(r.Min.X-15, r.Min.Y+40).A != 0 {
		t.Fatal("a boxed logo got a second box")
	}
	trimmed := analyseLogo(inkLogo(codevertexPurple))
	if b := trimmed.img.Bounds(); b.Dx() > 210 || b.Dy() > 130 || trimmed.opaqueBox {
		t.Fatalf("transparent margins kept: %v", b)
	}
}

func TestLogoBoxCapsWideAndTallLogos(t *testing.T) {
	wide := image.NewRGBA(image.Rect(0, 0, 1600, 100))
	tall := image.NewRGBA(image.Rect(0, 0, 100, 1600))
	square := image.NewRGBA(image.Rect(0, 0, 500, 500))
	for _, c := range []struct {
		name string
		img  image.Image
	}{{"wide", wide}, {"tall", tall}, {"square", square}} {
		b := logoBox(c.img, 400, 100)
		if b.X > 400 || b.Y > 100 || b.X*b.Y > 15000 || b.X < 1 || b.Y < 1 {
			t.Fatalf("%s logo box %v", c.name, b)
		}
	}
	// A 4:1 logo in a wide slot is held to the area cap, keeping its shape.
	b := logoBox(image.NewRGBA(image.Rect(0, 0, 800, 200)), 400, 100)
	if b.X*b.Y > 15000 || b.X < 4*b.Y-4 || b.X > 4*b.Y+4 {
		t.Fatalf("4:1 logo box %v", b)
	}
	if logoBox(nil, 100, 100) != (image.Point{}) {
		t.Fatal("a missing logo has a size")
	}
}

// The scene logo is about a tenth of the frame's width, in a top corner, and a missing logo is
// left out quietly.
func TestComposeWatermark(t *testing.T) {
	if wm, err := ComposeWatermark("1920x1080", nil, LogoTopRight, PlateStyle{}); wm != nil || err != nil {
		t.Fatalf("a missing logo gave %v %v", wm, err)
	}
	logo := image.NewRGBA(image.Rect(0, 0, 600, 300))
	draw.Draw(logo, image.Rect(20, 20, 580, 280), image.NewUniform(codevertexPurple), image.Point{}, draw.Src)
	wm, err := ComposeWatermark("1920x1080", logo, LogoTopRight, PlateStyle{})
	if err != nil || wm == nil {
		t.Fatalf("watermark %v", err)
	}
	minX, maxX, maxY := 1920, 0, 0
	for y := 0; y < 1080; y += 2 {
		for x := 0; x < 1920; x += 2 {
			if wm.RGBAAt(x, y).A > 0 {
				minX, maxX, maxY = min(minX, x), max(maxX, x), max(maxY, y)
			}
		}
	}
	w := maxX - minX
	if minX < 1920/2 || maxY > 1080/4 || w < 1920*8/100 || w > 1920*13/100 {
		t.Fatalf("watermark spans x %d..%d, down to y %d", minX, maxX, maxY)
	}
}

// The scene logo keeps to the calmer top corner over the frames.
func TestWatermarkCornerAvoidsBusyCorner(t *testing.T) {
	logo := inkLogo(codevertexPurple)
	frame := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	draw.Draw(frame, frame.Bounds(), image.NewUniform(color.RGBA{200, 200, 200, 255}), image.Point{}, draw.Src)
	for x := 1400; x < 1920; x += 6 {
		draw.Draw(frame, image.Rect(x, 0, x+3, 400), image.NewUniform(color.RGBA{10, 10, 10, 255}), image.Point{}, draw.Src)
	}
	if got := WatermarkCorner([]image.Image{frame}, "1920x1080", logo); got != LogoTopLeft {
		t.Fatalf("busy top right, got %s", got)
	}
	if got := WatermarkCorner(nil, "1920x1080", logo); got != LogoTopRight {
		t.Fatalf("no frames, got %s", got)
	}
	// A nil logo and a broken image never panic.
	drawLogo(image.NewRGBA(image.Rect(0, 0, 10, 10)), analyseLogo(nil), image.Rect(0, 0, 5, 5), PlateStyle{})
}
