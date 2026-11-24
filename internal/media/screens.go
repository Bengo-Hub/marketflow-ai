package media

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"math"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

// Real product screens (owner, 2026-11-24, plan item 66 e): when a scene shows a laptop, tablet,
// POS or phone, the tenant's own screenshot goes on its screen, the way the real logo goes on a
// banner (branding.go: the same planning, Locate, warp and review). The still is drawn with the
// screen blank and evenly lit; nothing on it is ever drawn by the image model. On any failure the
// scene is drawn again with no screen in view, so a blank or invented screen never ships. The
// scene is then shown as a still with a slow push-in, because a clip route would smear the
// screenshot's small writing into an invented screen. Off when the tenant has no screenshots. The
// rules are service config creative.screens (platform default, tenant override).

// Device kinds a screenshot can be shown on.
const (
	DeviceLaptop = "laptop"
	DeviceTablet = "tablet"
	DevicePOS    = "pos"
	DevicePhone  = "phone"
)

// PlaceScreen is the placement kind of a screenshot on a device.
const PlaceScreen = "screen"

// maxScreenScenes is the most scenes one ad may carry a screenshot in, whatever the setting.
const maxScreenScenes = 4

// maxScreenshots is the most screenshots read for one storyboard.
const maxScreenshots = 6

// ScreenRules are the screen settings.
type ScreenRules struct {
	Enabled   bool `json:"enabled"`
	MaxScenes int  `json:"max_scenes"`
}

// DefaultScreenRules are the fallbacks while the platform sets none.
func DefaultScreenRules() ScreenRules { return ScreenRules{Enabled: true, MaxScenes: 2} }

func (r ScreenRules) limit() int {
	if r.MaxScenes <= 0 || r.MaxScenes > maxScreenScenes {
		return DefaultScreenRules().MaxScenes
	}
	return r.MaxScenes
}

// Screenshot is one of the tenant's own screenshots.
type Screenshot struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Device string `json:"device,omitempty"` // DeviceLaptop, ... ; "" goes by the picture's shape
}

// PlacedScreen records a screenshot that was placed, on the still it was placed on.
type PlacedScreen struct {
	ID      string        `json:"id"`
	Device  string        `json:"device"`
	Surface string        `json:"surface"`
	Quad    [4][2]float64 `json:"quad"`
}

// deviceWords find the device a scene shows, the most specific first.
var deviceWords = []struct {
	device string
	re     *regexp.Regexp
}{
	{DevicePOS, regexp.MustCompile(`(?i)\b(pos|point[- ]of[- ]sale|cash register|checkout (screen|terminal|computer)|till (screen|computer))\b`)},
	{DeviceTablet, regexp.MustCompile(`(?i)\b(tablets?|ipads?)\b`)},
	{DeviceLaptop, regexp.MustCompile(`(?i)\b(laptops?|computers?|desktops?|monitors?)\b`)},
	{DevicePhone, regexp.MustCompile(`(?i)\b(phones?|smartphones?)\b`)},
}

// sceneDevice is the device a scene's visual shows, or "".
func sceneDevice(sc ScriptScene) string {
	for _, d := range deviceWords {
		if d.re.MatchString(sc.Visual) {
			return d.device
		}
	}
	return ""
}

// fitsDevice reports whether a screenshot suits a device: the owner's choice, else its shape (a
// tall one is a phone's, a wide one is not).
func fitsDevice(shot Screenshot, size image.Point, device string) bool {
	if shot.Device != "" {
		return shot.Device == device
	}
	return (size.Y > size.X) == (device == DevicePhone)
}

// screenSurface is the blank screen a still is asked to include.
func screenSurface(device string) string {
	name := map[string]string{DeviceLaptop: "laptop", DeviceTablet: "tablet held or standing upright", DevicePOS: "point of sale touchscreen on the counter",
		DevicePhone: "smartphone held upright close to the camera"}[device]
	return fmt.Sprintf("one %s with its whole screen turned to the camera and not covered by hands, the screen switched on but completely blank, one plain evenly lit pale grey with nothing on it; no other screen is in view", name)
}

// noScreen replaces the blank screen when no screenshot could go on it.
const noScreen = ". Any device in the scene is seen from behind or is out of the frame, so no screen is visible"

// planScreens picks the scenes that show a device one of the screenshots suits, in script order,
// each screenshot once before any repeats. Scenes already carrying a placement are left alone.
func planScreens(scenes []ScriptScene, rules ScreenRules, shots []Screenshot, sizes map[string]image.Point,
	generated func(sceneID string) bool, taken func(sceneID string) bool) []Placement {
	if !rules.Enabled || len(shots) == 0 {
		return nil
	}
	var out []Placement
	used := map[string]int{}
	for _, sc := range scenes {
		if len(out) >= rules.limit() {
			break
		}
		if (sc.Shot != ShotBRoll && sc.Shot != ShotProduct) || !generated(sc.ID) || taken(sc.ID) {
			continue
		}
		device := sceneDevice(sc)
		if device == "" {
			continue
		}
		pick, chosen := "", ""
		for _, shot := range shots {
			size, ok := sizes[shot.ID]
			if !ok || !fitsDevice(shot, size, device) {
				continue
			}
			// The least used first, and among equals one the owner chose for this device.
			if pick == "" || used[shot.ID] < used[pick] || (used[shot.ID] == used[pick] && shot.Device == device && chosen != device) {
				pick, chosen = shot.ID, shot.Device
			}
		}
		if pick == "" {
			continue
		}
		used[pick]++
		out = append(out, Placement{SceneID: sc.ID, Kind: PlaceScreen, Surface: screenSurface(device), Device: device, ScreenID: pick})
	}
	return out
}

// prepareScreens downloads the screenshots and plans their scenes onto the plan.
func (s *Storyboards) prepareScreens(ctx context.Context, req *StoryboardRequest, bp *brandPlan, generated func(string) bool) {
	if req.Screens == nil || !req.Screens.Enabled || len(req.Screenshots) == 0 {
		return
	}
	wanted := false
	for _, sc := range req.Scenes {
		wanted = wanted || (generated(sc.ID) && sceneDevice(sc) != "")
	}
	if !wanted {
		return
	}
	sizes := map[string]image.Point{}
	bp.screens = map[string]image.Image{}
	for i, shot := range req.Screenshots {
		if i >= maxScreenshots {
			break
		}
		if shot.ID == "" || !httpLink(shot.URL) {
			continue
		}
		img, err := s.posts.download(ctx, shot.URL)
		if err != nil {
			s.logger.Info("screenshot not read; scenes go without it", zap.Error(err))
			continue
		}
		bp.screens[shot.ID], sizes[shot.ID] = img, img.Bounds().Size()
	}
	for _, p := range planScreens(req.Scenes, *req.Screens, req.Screenshots, sizes, generated, func(id string) bool {
		_, ok := bp.placements[id]
		return ok
	}) {
		bp.placements[p.SceneID] = p
	}
}

// coverCrop is the part of a screenshot that fills a screen of the given shape: the sides are cut
// evenly, the bottom is cut, the top (where an app's name and menu are) stays. ok is false when
// more than half would be lost.
func coverCrop(b image.Rectangle, aspect float64) (image.Rectangle, bool) {
	w, h := float64(b.Dx()), float64(b.Dy())
	if w <= 0 || h <= 0 || aspect <= 0 {
		return b, false
	}
	nw, nh := w, w/aspect
	if nh > h {
		nw, nh = h*aspect, h
	}
	if nw*nh < w*h*0.5 {
		return b, false
	}
	x := b.Min.X + int((w-nw)/2)
	return image.Rect(x, b.Min.Y, x+int(nw), b.Min.Y+int(nh)).Intersect(b), true
}

// placeOnScreen warps the screenshot over the whole screen quad (fractions of the frame), kept in
// the screen's own light. What differs clearly from the blank screen (a finger, a glare line)
// stays in front of it.
func placeOnScreen(dst *image.RGBA, shot image.Image, q [4][2]float64) bool {
	if shot == nil {
		return false
	}
	px, sw, sh := quadPixels(dst, q)
	if sw < 40 || sh < 40 {
		return false
	}
	crop, ok := coverCrop(shot.Bounds(), sw/sh)
	if !ok {
		return false
	}
	part := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	draw.Draw(part, part.Bounds(), shot, crop.Min, draw.Src)
	return warp(dst, part, px, warpLook{opacity: 1, minShade: 0.8, maxShade: 1.08, keepFront: true})
}

// quadPixels is the quad in the picture's pixels with its mean width and height.
func quadPixels(dst *image.RGBA, q [4][2]float64) (px [4][2]float64, w, h float64) {
	W, H := float64(dst.Bounds().Dx()), float64(dst.Bounds().Dy())
	for i := range q {
		px[i] = [2]float64{q[i][0] * W, q[i][1] * H}
	}
	w = (math.Hypot(px[1][0]-px[0][0], px[1][1]-px[0][1]) + math.Hypot(px[2][0]-px[3][0], px[2][1]-px[3][1])) / 2
	h = (math.Hypot(px[3][0]-px[0][0], px[3][1]-px[0][1]) + math.Hypot(px[2][0]-px[1][0], px[2][1]-px[1][1])) / 2
	return px, w, h
}

// screenExpect is what the reviewer checks a placed screenshot against.
func screenExpect(device string) string {
	return "the business's own app screenshot filling the " + device +
		" screen, straight, in perspective and real-looking, with nothing else drawn on any screen (the small writing inside the screenshot is real and may be too small to read)"
}

// errScreenLeft means a scene still shows a blank screen and could not be drawn without it.
var errScreenLeft = errors.New("a scene with a device could not be drawn")

// withoutScreen draws a scene again with no screen in view, trying twice.
func (s *Storyboards) withoutScreen(ctx context.Context, req StoryboardRequest, res *StoryboardResult, st *Still, p Placement) error {
	plain := strings.Replace(st.Prompt, ". In the scene: "+p.Surface, noScreen, 1)
	var err error
	for try := 0; try < 2; try++ {
		var url, key, route string
		var cost float64
		if url, key, route, cost, err = s.still(ctx, req, "sb-"+st.SceneID+"-noscreen", plain); err == nil {
			st.URL, st.Key, st.Route, st.Prompt = url, key, route, plain
			st.CostUSD += cost
			res.CostUSD += cost
			return nil
		}
	}
	return fmt.Errorf("%w: scene %s: %w", errScreenLeft, st.SceneID, err)
}
