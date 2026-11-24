package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"regexp"
	"strings"
)

// In-scene branding (owner, 2026-11-24): the tenant's real logo appears on a believable surface in
// two or three key scenes (the opening establishing shot, the presenter, the end card), never drawn
// by the image model, which garbles logos and writing. The still is generated with one plain,
// blank surface in the brand's plate colour (a roll-up banner, a wall panel); the vision model
// finds that surface's four corners; the real logo file is warped onto it in perspective, shaded
// by the surface's own light, and the self-review checks the result. Anything that fails (no
// surface found, the reviewer unavailable or unhappy) keeps the plain scene: branding never blocks
// a plan. The rules are service config creative.branding (platform default, tenant override).

// Placement kinds.
const (
	BrandEstablishing = "establishing"
	BrandPresenter    = "presenter"
	BrandEndCard      = "end_card"
)

// BrandingRules are the in-scene branding settings.
type BrandingRules struct {
	Enabled   bool     `json:"enabled"`
	MaxScenes int      `json:"max_scenes"`
	Kinds     []string `json:"kinds"`
	// MaxWidth caps the logo's width as a share of the frame's width.
	MaxWidth float64 `json:"max_width"`
}

// DefaultBrandingRules are the fallbacks while the platform sets none.
func DefaultBrandingRules() BrandingRules {
	return BrandingRules{Enabled: true, MaxScenes: 3, Kinds: []string{BrandEstablishing, BrandPresenter, BrandEndCard}, MaxWidth: 0.18}
}

// normal fills unset or out-of-range values from the defaults.
func (b BrandingRules) normal() BrandingRules {
	d := DefaultBrandingRules()
	if b.MaxScenes <= 0 || b.MaxScenes > 3 {
		b.MaxScenes = d.MaxScenes
	}
	if len(b.Kinds) == 0 {
		b.Kinds = d.Kinds
	}
	if b.MaxWidth < 0.05 || b.MaxWidth > 0.3 {
		b.MaxWidth = d.MaxWidth
	}
	return b
}

func (b BrandingRules) wants(kind string) bool {
	for _, k := range b.Kinds {
		if strings.EqualFold(strings.TrimSpace(k), kind) {
			return true
		}
	}
	return false
}

// Placement is one planned logo placement: the scene ("actor" for the presenter's still), its
// kind and the blank surface the still is asked to include.
type Placement struct {
	SceneID string `json:"scene_id"`
	Kind    string `json:"kind"`
	Surface string `json:"surface"`
}

// PlacedLogo records a placement that was made, on the still it was made on.
type PlacedLogo struct {
	Kind    string        `json:"kind"`
	Surface string        `json:"surface"`
	Quad    [4][2]float64 `json:"quad"` // top-left, top-right, bottom-right, bottom-left, 0 to 1
}

// outdoorWord marks a scene that happens outside, the natural establishing shot.
var outdoorWord = regexp.MustCompile(`(?i)\b(road|street|outside|exterior|entrance|pavement|sidewalk|skyline|matatus?|boda|lakeside|shopfront|storefront|building|plaza|town centre|cbd)\b`)

// planPlacements picks the scenes that carry the logo: the opening establishing shot (the first
// outdoor b-roll scene among the first two, else the first b-roll scene among them), the presenter
// when a new actor is drawn, and the end card. Product close-ups never carry one: a logo would
// compete with the product. generated reports which scenes get a generated still.
func planPlacements(scenes []ScriptScene, rules BrandingRules, newActor bool, generated func(sceneID string) bool) []Placement {
	if !rules.Enabled {
		return nil
	}
	rules = rules.normal()
	var out []Placement
	add := func(p Placement) {
		if len(out) < rules.MaxScenes {
			out = append(out, p)
		}
	}
	if rules.wants(BrandEstablishing) {
		pick := ""
		for i, sc := range scenes {
			if i >= 2 || sc.Shot != ShotBRoll || !generated(sc.ID) {
				continue
			}
			if pick == "" || outdoorWord.MatchString(sc.Setting+" "+sc.Visual) {
				pick = sc.ID
				if outdoorWord.MatchString(sc.Setting + " " + sc.Visual) {
					break
				}
			}
		}
		if pick != "" {
			add(Placement{SceneID: pick, Kind: BrandEstablishing})
		}
	}
	if rules.wants(BrandPresenter) && newActor {
		add(Placement{SceneID: "actor", Kind: BrandPresenter})
	}
	if rules.wants(BrandEndCard) {
		for _, sc := range scenes {
			if sc.Shot == ShotEndCard && generated(sc.ID) {
				add(Placement{SceneID: sc.ID, Kind: BrandEndCard})
			}
		}
	}
	return out
}

// surfaceFor is the blank surface a still is asked to include, in the plate colour so the logo
// reads on it. It is upright and still, so a clip made from it keeps the logo steady.
func surfaceFor(kind string, colour color.RGBA) string {
	name := colourName(colour)
	switch kind {
	case BrandPresenter:
		return fmt.Sprintf("a plain upright roll-up banner stand in solid %s behind the person to one side, completely blank with nothing printed on it", name)
	case BrandEndCard:
		return fmt.Sprintf("a plain flat wall panel in solid %s on the back wall, completely blank with nothing on it, facing the camera", name)
	}
	return fmt.Sprintf("a plain upright roll-up banner stand in solid %s beside the entrance, completely blank with nothing printed on it, facing the camera", name)
}

// colourName describes a colour in words an image model follows better than a hex code.
func colourName(c color.RGBA) string {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l := (mx + mn) / 2
	if mx-mn < 0.08 {
		switch {
		case l > 0.9:
			return "white"
		case l > 0.6:
			return "light grey"
		case l > 0.25:
			return "grey"
		}
		return "charcoal"
	}
	var h float64
	switch mx {
	case r:
		h = math.Mod((g-b)/(mx-mn), 6)
	case g:
		h = (b-r)/(mx-mn) + 2
	default:
		h = (r-g)/(mx-mn) + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	hues := []struct {
		upTo float64
		name string
	}{{15, "red"}, {40, "orange"}, {65, "yellow"}, {160, "green"}, {200, "teal"}, {250, "blue"}, {290, "purple"}, {335, "magenta"}, {360, "red"}}
	name := "red"
	for _, x := range hues {
		if h <= x.upTo {
			name = x.name
			break
		}
	}
	switch {
	case l > 0.85:
		if name == "purple" || name == "magenta" {
			return "soft lavender"
		}
		return "pale " + name
	case l > 0.65:
		return "light " + name
	case l < 0.25:
		return "deep " + name
	}
	return name
}

// locatePrompt asks the vision model for the blank surface's corners.
const locatePrompt = `You find one flat surface in a photo so a logo can be printed on it.
Reply with one JSON object only: {"found":true|false,"corners":[[x,y],[x,y],[x,y],[x,y]]}
The corners are the surface's printable face: top-left, top-right, bottom-right, bottom-left, as fractions of the image width and height (0 to 1).
found is false when the surface is missing, mostly hidden, covered by a person, or already has writing or a logo on it.`

// Locate asks the vision model where the surface is. ok is false when it is not found, the answer
// is unreadable, or the quad is implausible (too small, too large, not convex).
func (r *Reviewer) Locate(ctx context.Context, img image.Image, surface string) (quad [4][2]float64, cost float64, ok bool) {
	if r == nil || r.call == nil || img == nil {
		return quad, 0, false
	}
	data, err := reviewJPEG(img)
	if err != nil {
		return quad, 0, false
	}
	cctx, cancel := context.WithTimeout(ctx, reviewCallTimeout)
	defer cancel()
	reply, _, cost, err := r.call(cctx, locatePrompt, "The surface: "+surface, []string{"data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return quad, 0, false
	}
	quad, ok = parseQuad(reply)
	return quad, cost, ok
}

// parseQuad reads the locate reply and checks the quad is plausible for a banner or panel.
func parseQuad(reply string) ([4][2]float64, bool) {
	var q [4][2]float64
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return q, false
	}
	var v struct {
		Found   bool        `json:"found"`
		Corners [][]float64 `json:"corners"`
	}
	if json.Unmarshal([]byte(reply[start:end+1]), &v) != nil || !v.Found || len(v.Corners) != 4 {
		return q, false
	}
	for i, c := range v.Corners {
		if len(c) != 2 || c[0] < 0 || c[0] > 1 || c[1] < 0 || c[1] > 1 {
			return q, false
		}
		q[i] = [2]float64{c[0], c[1]}
	}
	area, convex := quadArea(q)
	return q, convex && area >= 0.004 && area <= 0.3
}

// quadArea is the quad's area (shoelace) and whether it is convex with a consistent winding.
func quadArea(q [4][2]float64) (float64, bool) {
	var a float64
	sign := 0
	for i := 0; i < 4; i++ {
		p, n, nn := q[i], q[(i+1)%4], q[(i+2)%4]
		a += p[0]*n[1] - n[0]*p[1]
		cross := (n[0]-p[0])*(nn[1]-n[1]) - (n[1]-p[1])*(nn[0]-n[0])
		s := 1
		if cross < 0 {
			s = -1
		}
		if cross == 0 || (sign != 0 && s != sign) {
			return math.Abs(a / 2), false
		}
		sign = s
	}
	return math.Abs(a / 2), true
}

// homography maps the unit square's corners (0,0),(1,0),(1,1),(0,1) to the quad (Heckbert).
type homography [9]float64

func squareToQuad(q [4][2]float64) homography {
	x0, y0, x1, y1, x2, y2, x3, y3 := q[0][0], q[0][1], q[1][0], q[1][1], q[2][0], q[2][1], q[3][0], q[3][1]
	dx1, dx2, dx3 := x1-x2, x3-x2, x0-x1+x2-x3
	dy1, dy2, dy3 := y1-y2, y3-y2, y0-y1+y2-y3
	var g, h float64
	if den := dx1*dy2 - dx2*dy1; den != 0 {
		g = (dx3*dy2 - dx2*dy3) / den
		h = (dx1*dy3 - dx3*dy1) / den
	}
	return homography{x1 - x0 + g*x1, x3 - x0 + h*x3, x0, y1 - y0 + g*y1, y3 - y0 + h*y3, y0, g, h, 1}
}

func (m homography) apply(u, v float64) (float64, float64) {
	w := m[6]*u + m[7]*v + m[8]
	return (m[0]*u + m[1]*v + m[2]) / w, (m[3]*u + m[4]*v + m[5]) / w
}

// inverse is the adjugate (a homography is defined up to scale).
func (m homography) inverse() homography {
	a, b, c, d, e, f, g, h, i := m[0], m[1], m[2], m[3], m[4], m[5], m[6], m[7], m[8]
	return homography{e*i - f*h, c*h - b*i, b*f - c*e, f*g - d*i, a*i - c*g, c*d - a*f, d*h - e*g, b*g - a*h, a*e - b*d}
}

// bilerpQuad is the point at (u, v) of the quad by bilinear interpolation, to place the logo's
// inner box on the surface.
func bilerpQuad(q [4][2]float64, u, v float64) [2]float64 {
	top := [2]float64{q[0][0] + (q[1][0]-q[0][0])*u, q[0][1] + (q[1][1]-q[0][1])*u}
	bot := [2]float64{q[3][0] + (q[2][0]-q[3][0])*u, q[3][1] + (q[2][1]-q[3][1])*u}
	return [2]float64{top[0] + (bot[0]-top[0])*v, top[1] + (bot[1]-top[1])*v}
}

// placeOnSurface warps the logo onto the surface quad (fractions of the frame) in dst: the logo
// keeps its shape inside a 14% margin, no wider than maxWidth of the frame, is shaded by the
// surface's own light, and the plate colour of the surface stays around it.
func placeOnSurface(dst *image.RGBA, logo image.Image, q [4][2]float64, maxWidth float64) bool {
	if logo == nil {
		return false
	}
	W, H := float64(dst.Bounds().Dx()), float64(dst.Bounds().Dy())
	px := q
	for i := range px {
		px[i] = [2]float64{q[i][0] * W, q[i][1] * H}
	}
	sw := (math.Hypot(px[1][0]-px[0][0], px[1][1]-px[0][1]) + math.Hypot(px[2][0]-px[3][0], px[2][1]-px[3][1])) / 2
	sh := (math.Hypot(px[3][0]-px[0][0], px[3][1]-px[0][1]) + math.Hypot(px[2][0]-px[1][0], px[2][1]-px[1][1])) / 2
	lb := logo.Bounds()
	if sw < 8 || sh < 8 || lb.Dx() <= 0 || lb.Dy() <= 0 {
		return false
	}
	// The logo's box on the surface, in surface fractions, centred in the upper part of a tall
	// banner (where a printed roll-up carries its logo) or the middle of a wide panel.
	bw, bh := sw*0.72, sh*0.72
	if mw := maxWidth * W; bw > mw {
		bw = mw
	}
	aspect := float64(lb.Dx()) / float64(lb.Dy())
	if bw/aspect > bh {
		bw = bh * aspect
	} else {
		bh = bw / aspect
	}
	fu, fv := bw/sw, bh/sh
	u0, v0 := (1-fu)/2, (1-fv)/2
	if sh > sw*1.6 {
		v0 = 0.18
	}
	inner := [4][2]float64{bilerpQuad(px, u0, v0), bilerpQuad(px, u0+fu, v0), bilerpQuad(px, u0+fu, v0+fv), bilerpQuad(px, u0, v0+fv)}
	inv := squareToQuad(inner).inverse()
	// The surface's mean light, so the logo is shaded like the banner it is printed on.
	minX, minY, maxX, maxY := W, H, 0.0, 0.0
	for _, p := range inner {
		minX, minY, maxX, maxY = math.Min(minX, p[0]), math.Min(minY, p[1]), math.Max(maxX, p[0]), math.Max(maxY, p[1])
	}
	r := image.Rect(int(minX), int(minY), int(math.Ceil(maxX)), int(math.Ceil(maxY))).Intersect(dst.Bounds())
	mean := 0.0
	n := 0
	for y := r.Min.Y; y < r.Max.Y; y += 2 {
		for x := r.Min.X; x < r.Max.X; x += 2 {
			mean += pixelLuma(dst.RGBAAt(x, y))
			n++
		}
	}
	if n == 0 {
		return false
	}
	mean /= float64(n)
	lw, lh := float64(lb.Dx()), float64(lb.Dy())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			u, v := inv.apply(float64(x)+0.5, float64(y)+0.5)
			if u < 0 || u >= 1 || v < 0 || v >= 1 {
				continue
			}
			sr, sg, sb, sa := bilinearAt(logo, lb.Min.X, lb.Min.Y, u*lw, v*lh)
			if sa <= 0 {
				continue
			}
			bg := dst.RGBAAt(x, y)
			shade := 1.0
			if mean > 0.02 {
				shade = math.Min(1.15, math.Max(0.7, pixelLuma(bg)/mean))
			}
			a := sa * 0.95
			mix := func(c, b float64) uint8 { return uint8(math.Max(0, math.Min(255, c*shade*a+b*(1-a)))) }
			dst.SetRGBA(x, y, color.RGBA{mix(sr, float64(bg.R)), mix(sg, float64(bg.G)), mix(sb, float64(bg.B)), 255})
		}
	}
	return true
}

// bilinearAt samples img at (x, y) relative to its origin: un-premultiplied 0-255 channels and
// alpha 0-1.
func bilinearAt(img image.Image, ox, oy int, x, y float64) (r, g, b, a float64) {
	x0, y0 := int(math.Floor(x-0.5)), int(math.Floor(y-0.5))
	fx, fy := x-0.5-float64(x0), y-0.5-float64(y0)
	bb := img.Bounds()
	at := func(ix, iy int) (float64, float64, float64, float64) {
		ix, iy = min(max(ix+ox, bb.Min.X), bb.Max.X-1), min(max(iy+oy, bb.Min.Y), bb.Max.Y-1)
		cr, cg, cb, ca := img.At(ix, iy).RGBA()
		return float64(cr), float64(cg), float64(cb), float64(ca)
	}
	var pr, pg, pb, pa float64
	for _, s := range [4]struct {
		dx, dy int
		w      float64
	}{{0, 0, (1 - fx) * (1 - fy)}, {1, 0, fx * (1 - fy)}, {0, 1, (1 - fx) * fy}, {1, 1, fx * fy}} {
		cr, cg, cb, ca := at(x0+s.dx, y0+s.dy)
		pr, pg, pb, pa = pr+cr*s.w, pg+cg*s.w, pb+cb*s.w, pa+ca*s.w
	}
	if pa <= 0 {
		return 0, 0, 0, 0
	}
	// Premultiplied 16-bit sums back to straight 8-bit colour.
	return pr / pa * 255, pg / pa * 255, pb / pa * 255, pa / 0xffff
}

// pixelLuma is an opaque pixel's luma, 0 to 1.
func pixelLuma(c color.RGBA) float64 {
	return (0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)) / 255
}

// brandPlan is a storyboard's prepared branding: the rules, the trimmed logo and the placements
// by scene id ("actor" for the presenter's still).
type brandPlan struct {
	rules      BrandingRules
	logo       image.Image
	placements map[string]Placement
}

func (b *brandPlan) placement(id string) (Placement, bool) {
	if b == nil {
		return Placement{}, false
	}
	p, ok := b.placements[id]
	return p, ok
}

// PrepareBranding reads the logo and plans the placements before the stills are priced or drawn,
// so the plan, the price and the drawing agree. Without rules, a logo or a readable logo file it
// plans nothing.
func (s *Storyboards) PrepareBranding(ctx context.Context, req *StoryboardRequest) {
	if req == nil || req.brand != nil || req.Branding == nil || !req.Branding.Enabled || req.LogoURL == "" || s == nil || s.posts == nil {
		return
	}
	logo, err := s.posts.download(ctx, req.LogoURL)
	if err != nil {
		return
	}
	info := analyseLogo(logo)
	var tints []string
	if req.EndCard != nil {
		tints = req.EndCard.Plate.Tints
	}
	// The surface is the logo's own plate colour, tinted by the brand, so the logo reads on it.
	colour := brandPlate(info, info.readableOn(neutralLight) >= info.readableOn(neutralDark), tints)
	frames := newFrameChooser(req.Photos, nil, req.PhotoInfo)
	generated := map[string]bool{}
	for _, sc := range req.Scenes {
		if frames.choose(sc).Source == FrameGenerated {
			generated[sc.ID] = true
		}
	}
	newActor := isPresenterKind(req.Kind) && (req.Actor == nil || req.Actor.URL == "")
	bp := &brandPlan{rules: req.Branding.normal(), logo: info.img, placements: map[string]Placement{}}
	for _, p := range planPlacements(req.Scenes, *req.Branding, newActor, func(id string) bool { return generated[id] }) {
		p.Surface = surfaceFor(p.Kind, colour)
		bp.placements[p.SceneID] = p
	}
	req.brand = bp
}

// placeLogos puts the real logo on the planned surfaces of the drawn stills. Each placement needs
// the vision model twice (to find the surface, then to pass the result); when it is off, busy or
// unhappy, the plain still stays.
func (s *Storyboards) placeLogos(ctx context.Context, req StoryboardRequest, res *StoryboardResult) {
	if req.brand == nil || len(req.brand.placements) == 0 || res == nil {
		return
	}
	r, _, on := reviewOn(ctx)
	if !on {
		return
	}
	place := func(name, url string, p Placement) (newURL, key string, logo *PlacedLogo) {
		img, err := s.posts.download(ctx, url)
		if err != nil {
			return "", "", nil
		}
		quad, cost, found := r.Locate(ctx, img, p.Surface)
		res.CostUSD += cost
		if !found {
			return "", "", nil
		}
		b := img.Bounds()
		canvas := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(canvas, canvas.Bounds(), img, b.Min, draw.Src)
		if !placeOnSurface(canvas, req.brand.logo, quad, req.brand.rules.MaxWidth) {
			return "", "", nil
		}
		v, err := r.Review(ctx, ReviewBrief{Kind: "storyboard", Brand: req.Brand, Place: req.Place}, []ReviewFrame{{Label: name, Image: canvas,
			Expect: "the business's own logo printed on " + p.Surface + ", sharp, straight and real-looking, with no other writing"}})
		if err != nil {
			return "", "", nil
		}
		res.CostUSD += v.CostUSD
		if !v.Pass || len(highIssues(v)[name]) > 0 {
			return "", "", nil
		}
		data, err := EncodeJPEG(canvas)
		if err != nil {
			return "", "", nil
		}
		key, newURL, err = s.store.PutPoster(ctx, req.TenantID, req.VersionID+"-"+name+"-brand", data)
		if err != nil {
			return "", "", nil
		}
		return newURL, key, &PlacedLogo{Kind: p.Kind, Surface: p.Surface, Quad: quad}
	}
	if p, ok := req.brand.placement("actor"); ok && res.Actor != nil && res.Actor.URL != "" && res.Actor.Route != "" {
		if u, k, l := place("actor", res.Actor.URL, p); u != "" {
			old := res.Actor.URL
			res.Actor.PlainURL, res.Actor.PlainKey, res.Actor.Logo = old, res.Actor.Key, l
			res.Actor.URL, res.Actor.Key = u, k
			for i := range res.Stills {
				if res.Stills[i].Source == StillActor && res.Stills[i].URL == old {
					res.Stills[i].URL = u
				}
			}
		}
	}
	for i := range res.Stills {
		st := &res.Stills[i]
		p, ok := req.brand.placement(st.SceneID)
		if !ok || st.Source != StillGenerated || st.Reused || st.URL == "" {
			continue
		}
		if u, k, l := place("sb-"+st.SceneID, st.URL, p); u != "" {
			st.PlainURL, st.PlainKey, st.Logo = st.URL, st.Key, l
			st.URL, st.Key = u, k
		}
	}
}
