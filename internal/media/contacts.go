package media

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Posters and end cards never draw a button (owner, 2026-11-20): a picture cannot be tapped, so
// a button shape only promises a click that does nothing. The call to action is a plain line of
// words, and the real ways to reach the business are drawn under it, each with its icon:
// WhatsApp, phone, website, email or a product link, all from the tenant's own data.

// Contact kinds.
const (
	ContactWhatsApp = "whatsapp"
	ContactPhone    = "phone"
	ContactWeb      = "web"
	ContactEmail    = "email"
	ContactLink     = "link"
	// ContactPage is the ad's lead page, on end cards only: one line of its own under the others
	// (drawPage), whole or not at all.
	ContactPage = "page"
)

// Contact is one real way to reach the business, drawn as an icon and its value.
type Contact struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// maxContacts keeps the strip to one or two readable lines.
const maxContacts = 4

// whatsAppGreen is WhatsApp's own brand green, so the icon reads as WhatsApp at a glance.
var whatsAppGreen = color.RGBA{0x25, 0xd3, 0x66, 0xff}

// CleanContacts keeps known kinds with a short, non-empty value, in order, without repeats.
func CleanContacts(list []Contact) []Contact {
	var out []Contact
	seen := map[string]bool{}
	for _, c := range list {
		c.Kind = strings.ToLower(strings.TrimSpace(c.Kind))
		c.Value = strings.TrimSpace(c.Value)
		switch c.Kind {
		case ContactWhatsApp, ContactPhone, ContactWeb, ContactEmail, ContactLink:
		default:
			continue
		}
		if c.Value == "" || len(c.Value) > 60 || seen[c.Kind+"|"+strings.ToLower(c.Value)] {
			continue
		}
		seen[c.Kind+"|"+strings.ToLower(c.Value)] = true
		// A web address reads better without its scheme.
		if c.Kind == ContactWeb || c.Kind == ContactLink {
			c.Value = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(c.Value, "https://"), "http://"), "/")
		}
		out = append(out, c)
		if len(out) == maxContacts {
			break
		}
	}
	return out
}

// pageOf is the lead page to draw, "" for none: it is left out when a contact already shows it.
func pageOf(list []Contact) string {
	trim := func(v string) string { return strings.ToLower(strings.TrimPrefix(v, "www.")) }
	for _, c := range list {
		if strings.ToLower(strings.TrimSpace(c.Kind)) != ContactPage {
			continue
		}
		c.Kind = ContactWeb // cleaned like a web address
		clean := CleanContacts([]Contact{c})
		if len(clean) == 0 {
			return ""
		}
		for _, o := range CleanContacts(list) {
			if trim(o.Value) == trim(clean[0].Value) {
				return ""
			}
		}
		return clean[0].Value
	}
	return ""
}

// withoutPage is the overlay without its lead page.
func (o Overlay) withoutPage() Overlay {
	var keep []Contact
	for _, c := range o.Contacts {
		if strings.ToLower(strings.TrimSpace(c.Kind)) != ContactPage {
			keep = append(keep, c)
		}
	}
	o.Contacts = keep
	return o
}

// minPageScale is how far the lead page's letters may shrink to fit in one line.
const minPageScale = 0.7

// drawPage draws the lead page on one line ending at bottom, smaller when it is long. An address cut
// short would lead nowhere, so one that does not fit is not drawn; it returns the top.
func drawPage(dst *image.RGBA, f *opentype.Font, page string, x, bottom, width, unit int, accent color.RGBA) int {
	full := float64(unit) / 26
	for size := full; size >= full*minPageScale; size *= 0.95 {
		fc, err := face(f, size)
		if err != nil {
			return bottom
		}
		icon, gap, pad := int(size*1.35), int(size*0.45), int(size*0.55)
		w := icon + gap + font.MeasureString(fc, page).Ceil()
		if w > width {
			continue
		}
		top := bottom - icon
		fillRounded(dst, image.Rect(x-pad, top-pad/2, x+w+pad, bottom+pad/2), pad, color.RGBA{0, 0, 0, 0xb4})
		drawContactIcon(dst, ContactPage, image.Rect(x, top, x+icon, bottom), accent)
		d := &font.Drawer{Dst: dst, Src: image.NewUniform(color.White), Face: fc, Dot: fixed.P(x+icon+gap, top+icon/2+int(size*0.36))}
		d.DrawString(page)
		return top - pad
	}
	return bottom
}

// drawContacts lays the contacts out bottom-up from bottom, wrapping to a new line when the next
// one does not fit, over a dark plate so the white text reads on any scene. It returns the top.
func drawContacts(dst *image.RGBA, f *opentype.Font, list []Contact, x, bottom, width, unit int, accent color.RGBA) int {
	size := float64(unit) / 26
	fc, _ := face(f, size)
	icon := int(size * 1.35)
	gap := int(size * 0.45)    // icon to text
	between := int(size * 1.3) // one contact to the next
	lineH := icon + int(size*0.5)

	type item struct {
		c Contact
		w int
	}
	var lines [][]item
	cur, curW := []item(nil), 0
	for _, c := range list {
		text := ellipsize(fc, c.Value, width-icon-gap)
		w := icon + gap + font.MeasureString(fc, text).Ceil()
		next := curW + w
		if len(cur) > 0 {
			next += between
		}
		if len(cur) > 0 && next > width {
			lines = append(lines, cur)
			cur, curW = nil, 0
			next = w
		}
		cur = append(cur, item{Contact{Kind: c.Kind, Value: text}, w})
		curW = next
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		return bottom
	}
	if len(lines) > 2 {
		lines = lines[:2]
	}
	top := bottom - lineH*len(lines)
	widest := 0
	for _, l := range lines {
		w := 0
		for i, it := range l {
			if i > 0 {
				w += between
			}
			w += it.w
		}
		widest = max(widest, w)
	}
	pad := int(size * 0.55)
	fillRounded(dst, image.Rect(x-pad, top-pad, x+widest+pad, bottom+pad/2), pad, color.RGBA{0, 0, 0, 0xb4})
	for li, l := range lines {
		cx := x
		cy := top + li*lineH + (lineH-icon)/2
		for _, it := range l {
			drawContactIcon(dst, it.c.Kind, image.Rect(cx, cy, cx+icon, cy+icon), accent)
			d := &font.Drawer{Dst: dst, Src: image.NewUniform(color.White), Face: fc,
				Dot: fixed.P(cx+icon+gap, cy+icon/2+int(size*0.36))}
			d.DrawString(it.c.Value)
			cx += it.w + between
		}
	}
	return top - pad
}

// drawCTA draws the call to action as a line of words (bold, white, with a short accent rule
// above it), never as a button. It returns the top.
func drawCTA(dst *image.RGBA, f *opentype.Font, text string, x, bottom, width, unit int, accent color.RGBA) int {
	size := float64(unit) / 22
	top := drawBlock(dst, f, text, x, bottom, width, size, 2, color.White)
	rule := image.Rect(x, top-int(size*0.35), x+int(size*2.2), top-int(size*0.2))
	draw.Draw(dst, rule, image.NewUniform(accent), image.Point{}, draw.Src)
	return rule.Min.Y
}

// drawContactIcon draws one round icon: WhatsApp in its own green, the others in the brand colour,
// with a white glyph.
func drawContactIcon(dst *image.RGBA, kind string, r image.Rectangle, accent color.RGBA) {
	s := float32(r.Dx())
	cx, cy := float32(r.Min.X)+s/2, float32(r.Min.Y)+s/2
	bg := accent
	if kind == ContactWhatsApp {
		bg = whatsAppGreen
	}
	fillPath(dst, func(z *vector.Rasterizer) { circle(z, cx, cy, s/2, false) }, bg)
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	switch kind {
	case ContactWhatsApp:
		// A speech bubble ring with its tail, and the handset inside.
		fillPath(dst, func(z *vector.Rasterizer) {
			circle(z, cx, cy, s*0.33, false)
			circle(z, cx, cy, s*0.26, true)
		}, white)
		fillPath(dst, func(z *vector.Rasterizer) {
			z.MoveTo(cx-s*0.26, cy+s*0.12)
			z.LineTo(cx-s*0.36, cy+s*0.36)
			z.LineTo(cx-s*0.10, cy+s*0.27)
			z.ClosePath()
		}, white)
		handset(dst, cx, cy, s*0.42, white)
	case ContactPhone:
		handset(dst, cx, cy, s*0.62, white)
	case ContactEmail:
		w, h, t := s*0.52, s*0.36, s*0.06
		x0, y0 := cx-w/2, cy-h/2
		fillPath(dst, func(z *vector.Rasterizer) {
			rect(z, x0, y0, x0+w, y0+h, false)
			rect(z, x0+t, y0+t, x0+w-t, y0+h-t, true)
		}, white)
		fillPath(dst, func(z *vector.Rasterizer) { thickLine(z, x0+t, y0+t, cx, cy+h*0.12, t) }, white)
		fillPath(dst, func(z *vector.Rasterizer) { thickLine(z, cx, cy+h*0.12, x0+w-t, y0+t, t) }, white)
	default: // web and link: a globe
		R, t := s*0.30, s*0.055
		fillPath(dst, func(z *vector.Rasterizer) {
			circle(z, cx, cy, R, false)
			circle(z, cx, cy, R-t, true)
		}, white)
		fillPath(dst, func(z *vector.Rasterizer) {
			ellipse(z, cx, cy, R*0.45, R, false)
			ellipse(z, cx, cy, R*0.45-t, R-t, true)
		}, white)
		fillPath(dst, func(z *vector.Rasterizer) { rect(z, cx-R, cy-t/2, cx+R, cy+t/2, false) }, white)
	}
}

// handset draws a phone handset of about size s centred on cx, cy: a thick arc with a round pad
// at each end, turned the way phone icons usually lean.
func handset(dst *image.RGBA, cx, cy, s float32, c color.RGBA) {
	ro, ri := s*0.42, s*0.24
	a0, a1 := deg(55), deg(215)
	fillPath(dst, func(z *vector.Rasterizer) {
		const n = 24
		for i := 0; i <= n; i++ {
			a := a0 + (a1-a0)*float32(i)/n
			px, py := cx+ro*cos32(a), cy+ro*sin32(a)
			if i == 0 {
				z.MoveTo(px, py)
			} else {
				z.LineTo(px, py)
			}
		}
		for i := n; i >= 0; i-- {
			a := a0 + (a1-a0)*float32(i)/n
			z.LineTo(cx+ri*cos32(a), cy+ri*sin32(a))
		}
		z.ClosePath()
	}, c)
	rm := (ro + ri) / 2
	pad := (ro - ri) * 0.78
	for _, a := range []float32{a0, a1} {
		px, py := cx+rm*cos32(a), cy+rm*sin32(a)
		fillPath(dst, func(z *vector.Rasterizer) { circle(z, px, py, pad, false) }, c)
	}
}

// fillPath rasterises the shapes build adds and paints them in c over dst.
func fillPath(dst *image.RGBA, build func(z *vector.Rasterizer), c color.RGBA) {
	b := dst.Bounds()
	z := vector.NewRasterizer(b.Dx(), b.Dy())
	z.DrawOp = draw.Over
	build(z)
	z.Draw(dst, b, image.NewUniform(c), image.Point{})
}

// circle adds a circle; reverse winds it the other way, which cuts a hole in a shape drawn with it.
func circle(z *vector.Rasterizer, cx, cy, r float32, reverse bool) { ellipse(z, cx, cy, r, r, reverse) }

func ellipse(z *vector.Rasterizer, cx, cy, rx, ry float32, reverse bool) {
	const n = 48
	for i := 0; i <= n; i++ {
		k := i
		if reverse {
			k = n - i
		}
		a := 2 * math.Pi * float32(k) / n
		px, py := cx+rx*cos32(a), cy+ry*sin32(a)
		if i == 0 {
			z.MoveTo(px, py)
		} else {
			z.LineTo(px, py)
		}
	}
	z.ClosePath()
}

func rect(z *vector.Rasterizer, x0, y0, x1, y1 float32, reverse bool) {
	z.MoveTo(x0, y0)
	if reverse {
		z.LineTo(x0, y1)
		z.LineTo(x1, y1)
		z.LineTo(x1, y0)
	} else {
		z.LineTo(x1, y0)
		z.LineTo(x1, y1)
		z.LineTo(x0, y1)
	}
	z.ClosePath()
}

// thickLine adds a line of width t from (x0, y0) to (x1, y1) as a filled quad.
func thickLine(z *vector.Rasterizer, x0, y0, x1, y1, t float32) {
	dx, dy := x1-x0, y1-y0
	l := float32(math.Hypot(float64(dx), float64(dy)))
	if l == 0 {
		return
	}
	nx, ny := -dy/l*t/2, dx/l*t/2
	z.MoveTo(x0+nx, y0+ny)
	z.LineTo(x1+nx, y1+ny)
	z.LineTo(x1-nx, y1-ny)
	z.LineTo(x0-nx, y0-ny)
	z.ClosePath()
}

func deg(d float32) float32   { return d * math.Pi / 180 }
func cos32(a float32) float32 { return float32(math.Cos(float64(a))) }
func sin32(a float32) float32 { return float32(math.Sin(float64(a))) }
