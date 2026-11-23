package media

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"sort"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// The logo plate (owner, 2026-11-24). Every logo MarketFlow draws (posters, motion posters, the
// end card and the small corner logo on every video scene) goes through these helpers, for every
// tenant, from the tenant's own logo and brand colours (auth owns both; marketflow-api sends them):
//   - the logo is trimmed to its visible pixels, so a logo file with wide transparent margins is
//     not drawn tiny;
//   - the plate behind it is the one that keeps more of the logo's ink readable (a contrast ratio
//     of at least 3 to 1): a light plate for a dark logo, a dark plate for a light one, and for a
//     mid-tone or mixed logo whichever suits more of it;
//   - the plate takes the tenant's brand colour (its lightest for a light plate, its darkest for
//     a dark one, else a tint of it) only while the logo stays as readable on it as on the
//     neutral plate; with no brand colours the plate stays white or near-black;
//   - it is drawn rounded, about 86% opaque, with a soft shadow, so it reads on any scene;
//   - a logo with no transparency (a JPEG or a boxed PNG) already carries its own background, so
//     it gets a thin border instead of being boxed twice;
//   - the logo's size is capped by width, height and area, so a very wide or very tall logo
//     takes no more room than a square one.
// The style and opacity are platform and tenant settings (service-config creative.logo_plate_style
// and creative.logo_plate_opacity, sent by marketflow-api); the values here are only fallbacks.

// Plate styles.
const (
	PlateAuto  = "auto"
	PlateLight = "light"
	PlateDark  = "dark"
	PlateNone  = "none"
)

// DefaultPlateOpacity is the plate's opacity when the platform sets none.
const DefaultPlateOpacity = 0.86

// readableContrast is the contrast ratio a logo pixel needs against its plate (WCAG's ratio for
// graphics and large text).
const readableContrast = 3.0

var (
	neutralLight = color.RGBA{0xff, 0xff, 0xff, 0xff}
	neutralDark  = color.RGBA{0x14, 0x14, 0x18, 0xff}
)

// PlateStyle is how logos are backed.
type PlateStyle struct {
	Style   string  `json:"style,omitempty"`   // auto (default), light, dark or none
	Opacity float64 `json:"opacity,omitempty"` // 0.5 to 1; anything else is the default
	// Tints are the tenant's brand colours as #rrggbb; none keeps the plate neutral.
	Tints []string `json:"tints,omitempty"`
}

// logoInfo is what the plate needs to know about a logo.
type logoInfo struct {
	img       image.Image // trimmed to its visible pixels
	opaqueBox bool        // the logo has no transparency (its own background)
	ink       []float64   // relative luminance of a sample of its visible pixels
}

// readableOn is the share of the logo's ink that reads on a plate of this colour.
func (l logoInfo) readableOn(plate color.RGBA) float64 {
	if len(l.ink) == 0 {
		return 1
	}
	p := relLum(plate)
	n := 0
	for _, v := range l.ink {
		if contrast(v, p) >= readableContrast {
			n++
		}
	}
	return float64(n) / float64(len(l.ink))
}

// analyseLogo trims a logo to its visible pixels and samples its ink.
func analyseLogo(img image.Image) logoInfo {
	if img == nil {
		return logoInfo{}
	}
	b := img.Bounds()
	if b.Empty() {
		return logoInfo{img: img}
	}
	step := max(1, max(b.Dx(), b.Dy())/200)
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X, b.Min.Y
	var transparent, total float64
	info := logoInfo{img: img}
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			total++
			r, g, bl, a := img.At(x, y).RGBA()
			if a < 0x2000 {
				transparent++
				continue
			}
			minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x+step), max(maxY, y+step)
			if a < 0x8000 {
				continue
			}
			// Un-premultiply to the pixel's own colour.
			c := color.RGBA{uint8(r * 0xff / a), uint8(g * 0xff / a), uint8(bl * 0xff / a), 0xff}
			info.ink = append(info.ink, relLum(c))
		}
	}
	if total == 0 {
		return info
	}
	// Under 2% transparent pixels: a rectangular logo with its own background.
	info.opaqueBox = transparent/total < 0.02
	if !info.opaqueBox && maxX > minX && maxY > minY {
		crop := image.Rect(minX, minY, min(maxX, b.Max.X), min(maxY, b.Max.Y))
		if crop.Dx() < b.Dx() || crop.Dy() < b.Dy() {
			dst := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
			draw.Draw(dst, dst.Bounds(), img, crop.Min, draw.Src)
			info.img = dst
		}
	}
	return info
}

// plateColour is the plate behind a logo, premultiplied for drawing, and whether one is drawn.
func plateColour(info logoInfo, st PlateStyle) (color.RGBA, bool) {
	style := strings.ToLower(strings.TrimSpace(st.Style))
	if style == PlateNone {
		return color.RGBA{}, false
	}
	opacity := st.Opacity
	if opacity < 0.5 || opacity > 1 {
		opacity = DefaultPlateOpacity
	}
	var light bool
	switch style {
	case PlateLight:
		light = true
	case PlateDark:
		light = false
	default:
		// Ties go to the light plate: most logos are drawn for a white page.
		light = info.readableOn(neutralLight) >= info.readableOn(neutralDark)
	}
	return premul(brandPlate(info, light, st.Tints), opacity), true
}

// brandPlate is the plate colour on the chosen side: the brand's lightest (or darkest) colour when
// it is light (or dark) enough, else a tint of a brand colour, else the neutral plate. A brand
// colour is used only while the logo reads on it about as well as on the neutral plate.
func brandPlate(info logoInfo, light bool, tints []string) color.RGBA {
	base := neutralDark
	if light {
		base = neutralLight
	}
	var brand []color.RGBA
	for _, t := range tints {
		if c, err := parseHex(t); err == nil {
			brand = append(brand, c)
		}
	}
	if len(brand) == 0 {
		return base
	}
	// Lightest first for a light plate, darkest first for a dark one.
	sort.SliceStable(brand, func(i, j int) bool {
		if light {
			return relLum(brand[i]) > relLum(brand[j])
		}
		return relLum(brand[i]) < relLum(brand[j])
	})
	want := info.readableOn(base) * 0.95
	var candidates []color.RGBA
	for _, c := range brand {
		if (light && relLum(c) >= 0.7) || (!light && relLum(c) <= 0.02) {
			candidates = append(candidates, c) // the brand colour itself is a plate
		}
	}
	for _, c := range brand {
		if light {
			candidates = append(candidates, blend(base, c, 0.12))
		} else {
			candidates = append(candidates, blend(base, c, 0.25))
		}
	}
	for _, c := range candidates {
		l := relLum(c)
		if ((light && l >= 0.7) || (!light && l <= 0.02)) && info.readableOn(c) >= want {
			return c
		}
	}
	return base
}

// logoBox is where a logo of this shape goes inside maxW by maxH, its area capped at one and a
// half times the square the limits allow, so a very wide or tall logo is no louder than a square.
func logoBox(logo image.Image, maxW, maxH int) image.Point {
	if logo == nil {
		return image.Point{}
	}
	lb := logo.Bounds()
	if lb.Dx() <= 0 || lb.Dy() <= 0 || maxW <= 0 || maxH <= 0 {
		return image.Point{}
	}
	w := maxW
	h := lb.Dy() * w / lb.Dx()
	if h > maxH {
		h = maxH
		w = lb.Dx() * h / lb.Dy()
	}
	side := float64(min(maxW, maxH))
	if maxArea := 1.5 * side * side; float64(w*h) > maxArea {
		k := math.Sqrt(maxArea / float64(w*h))
		w, h = int(float64(w)*k), int(float64(h)*k)
	}
	return image.Pt(max(w, 1), max(h, 1))
}

// drawLogo draws a prepared logo in r with its plate (or, for a logo with its own background, a
// thin border) on canvas.
func drawLogo(canvas *image.RGBA, info logoInfo, r image.Rectangle, st PlateStyle) {
	if info.img == nil || r.Empty() {
		return
	}
	plate, ok := plateColour(info, st)
	pad := max(r.Dx(), r.Dy()) / 9
	switch {
	case !ok:
	case info.opaqueBox:
		// The logo brings its own background: a thin border of the plate colour, no second box.
		t := max(2, pad/4)
		fillRounded(canvas, r.Inset(-t), t, plate)
	default:
		outer := r.Inset(-pad)
		// A soft shadow under the plate, then the plate.
		fillRounded(canvas, outer.Add(image.Pt(0, max(2, pad/4))), pad, color.RGBA{0, 0, 0, 0x30})
		fillRounded(canvas, outer, pad, plate)
	}
	xdraw.CatmullRom.Scale(canvas, r, info.img, info.img.Bounds(), draw.Over, nil)
}

// watermarkBox is the scene logo's size in a frame: about 9% of the frame's width, no taller than
// 9% of its short side.
func watermarkBox(info logoInfo, size image.Point) (image.Point, int) {
	margin := layoutUnit(size.X, size.Y) * 5 / 100
	return logoBox(info.img, size.X*9/100, min(size.X, size.Y)*9/100), margin
}

// ComposeWatermark is the small corner logo laid over every scene of a video, in the given top
// corner inside the safe margin (captions sit at the bottom), on the same plate as the end card.
// A nil logo or an unknown format gives nil.
func ComposeWatermark(format string, logo image.Image, corner string, st PlateStyle) (*image.RGBA, error) {
	size, ok := Formats[format]
	if !ok || logo == nil {
		return nil, nil
	}
	info := analyseLogo(logo)
	box, margin := watermarkBox(info, size)
	if box.X == 0 {
		return nil, nil
	}
	canvas := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
	drawLogo(canvas, info, watermarkRect(corner, size, box, margin), st)
	return canvas, nil
}

func watermarkRect(corner string, size, box image.Point, margin int) image.Rectangle {
	if corner == LogoTopRight {
		return image.Rect(size.X-margin-box.X, margin, size.X-margin, margin+box.Y)
	}
	return image.Rect(margin, margin, margin+box.X, margin+box.Y)
}

// WatermarkCorner is the calmer top corner over a set of frames (fewer edges and less skin
// summed over all of them, the same measure as logoCorner), so the scene logo stays off faces;
// with no frames, top right.
func WatermarkCorner(frames []image.Image, format string, logo image.Image) string {
	size, ok := Formats[format]
	if !ok || logo == nil {
		return LogoTopRight
	}
	box, margin := watermarkBox(analyseLogo(logo), size)
	left := watermarkRect(LogoTopLeft, size, box, margin).Inset(-margin / 2)
	right := watermarkRect(LogoTopRight, size, box, margin).Inset(-margin / 2)
	var l, r float64
	n := 0
	for _, f := range frames {
		if f == nil {
			continue
		}
		canvas := FitFrame(f, size)
		bl, okL := busyness(canvas, left)
		br, okR := busyness(canvas, right)
		if okL && okR {
			l, r = l+bl, r+br
			n++
		}
	}
	if n > 0 && l+0.02*float64(n) < r {
		return LogoTopLeft
	}
	return LogoTopRight
}

// premul is c at opacity a as the premultiplied colour image/draw expects. A colour.RGBA whose
// channels exceed its alpha is invalid: the PNG encoder wraps it to a near-black plate.
func premul(c color.RGBA, a float64) color.RGBA {
	alpha := math.Round(a * 255)
	k := alpha / 255
	return color.RGBA{uint8(math.Round(float64(c.R) * k)), uint8(math.Round(float64(c.G) * k)), uint8(math.Round(float64(c.B) * k)), uint8(alpha)}
}

func blend(a, b color.RGBA, t float64) color.RGBA {
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x)*(1-t) + float64(y)*t)) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xff}
}

// relLum is a colour's WCAG relative luminance (0 black, 1 white).
func relLum(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// contrast is the WCAG contrast ratio of two relative luminances.
func contrast(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// parseHex reads a #rrggbb colour.
func parseHex(s string) (color.RGBA, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	var r, g, b uint8
	if len(s) != 6 {
		return color.RGBA{}, errBadHex
	}
	if _, err := fmt.Sscanf(s, "%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{}, errBadHex
	}
	return color.RGBA{r, g, b, 0xff}, nil
}

var errBadHex = errors.New("media: colour must be #rrggbb")
