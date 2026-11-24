package media

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"unicode"

	"github.com/google/uuid"
	xdraw "golang.org/x/image/draw"
)

// Start frames (2026-11-19 audit, finding 4). Every scene that is not the presenter starts from
// a frame chosen by one rule, used by the storyboard, the quote and the render alike:
//   - a product shot (product_clip, end_card) starts from one of the offering's own photos;
//   - a brand-library picture is used only when its stored caption or tags clearly match the
//     scene's visual; a picture without a caption or tags is never used;
//   - a logo is never a start frame (logos go on the end card only);
//   - every other scene gets a still generated from its visual.
// Before the round robin this replaces, the tenant's whole library filled b-roll scenes in turn,
// so client logos and unrelated shop photos became the first frame of animated clips.

// Frame sources of a scene (Still.Source and ScenePlan.Frame).
const (
	FrameActor     = StillActor
	FramePhoto     = StillPhoto
	FrameGenerated = StillGenerated
	FrameFootage   = StillFootage
)

// Photo origins.
const (
	OriginOffering = "offering"
	OriginLibrary  = "library"
)

// PhotoInfo describes one entry of a request's photos, by the same index. Library pictures are
// the tenant's brand images; Caption and Tags are what the owner (or the import) stored for them.
type PhotoInfo struct {
	Library bool     `json:"library,omitempty"`
	Logo    bool     `json:"logo,omitempty"`
	Caption string   `json:"caption,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// FrameChoice is where a scene's first frame comes from.
type FrameChoice struct {
	Source string // FrameActor, FramePhoto, FrameGenerated or FrameFootage
	URL    string // the photo or footage link ("" for a generated still or the actor)
	Photo  int    // the 1-based photo index for FramePhoto
	Origin string // OriginOffering or OriginLibrary for FramePhoto
}

// photoInfo is the description of photo i; a request without descriptions counts every photo
// as the offering's own (marketflow-api sends them since 2026-11-19).
func photoInfo(info []PhotoInfo, i int) PhotoInfo {
	if i < len(info) {
		return info[i]
	}
	return PhotoInfo{}
}

// frameChooser picks scene frames in script order; offering photos without a scripted asset
// are taken in turn.
type frameChooser struct {
	photos  []string
	footage []string
	info    []PhotoInfo
	nextOff int
	nextFtg int
}

func newFrameChooser(photos, footage []string, info []PhotoInfo) *frameChooser {
	return &frameChooser{photos: photos, footage: footage, info: info}
}

// offering lists the indexes of the offering's own photos.
func (c *frameChooser) offering() []int {
	var out []int
	for i := range c.photos {
		if in := photoInfo(c.info, i); !in.Library && !in.Logo {
			out = append(out, i)
		}
	}
	return out
}

// choose returns the frame for one scene.
func (c *frameChooser) choose(sc ScriptScene) FrameChoice {
	switch sc.Shot {
	case ShotPresenter:
		return FrameChoice{Source: FrameActor}
	case ShotFootage:
		return FrameChoice{Source: FrameFootage, URL: pick(c.footage, sc.Asset, &c.nextFtg)}
	case ShotProduct, ShotEndCard:
		if off := c.offering(); len(off) > 0 {
			i := -1
			for _, k := range off {
				if k == sc.Asset-1 {
					i = k
				}
			}
			if i < 0 {
				i = off[c.nextOff%len(off)]
				c.nextOff++
			}
			return FrameChoice{Source: FramePhoto, URL: c.photos[i], Photo: i + 1, Origin: OriginOffering}
		}
	}
	if i := c.libraryMatch(sc); i >= 0 {
		return FrameChoice{Source: FramePhoto, URL: c.photos[i], Photo: i + 1, Origin: OriginLibrary}
	}
	return FrameChoice{Source: FrameGenerated}
}

// libraryMatch is the brand-library picture whose caption or tags clearly match the scene, or
// -1. A tag must equal a word of the visual, or the caption must share at least two content
// words with it; the scripted asset wins when it matches.
func (c *frameChooser) libraryMatch(sc ScriptScene) int {
	words := contentWords(sc.Visual + " " + sc.Caption)
	if len(words) == 0 {
		return -1
	}
	best, bestScore := -1, 0
	for i := range c.photos {
		in := photoInfo(c.info, i)
		if !in.Library || in.Logo {
			continue
		}
		score := 0
		for _, t := range in.Tags {
			for w := range contentWords(t) {
				if words[w] {
					score += 2
				}
			}
		}
		shared := 0
		for w := range contentWords(in.Caption) {
			if words[w] {
				shared++
			}
		}
		if shared >= 2 {
			score += shared
		}
		if score >= 2 && (score > bestScore || (score == bestScore && i == sc.Asset-1)) {
			best, bestScore = i, score
		}
	}
	return best
}

// stopWords are left out of a caption match.
var stopWords = map[string]bool{"the": true, "and": true, "with": true, "for": true, "from": true, "into": true,
	"over": true, "their": true, "they": true, "this": true, "that": true, "while": true, "some": true, "shot": true,
	"close": true, "view": true, "scene": true, "photo": true, "image": true, "picture": true, "showing": true,
	"shows": true, "background": true, "kenyan": true, "african": true, "people": true, "person": true}

// contentWords are the lower-case words of at least four letters that are not stop words, with a
// plural s dropped, so "shops" matches "shop".
func contentWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		if len(w) >= 4 && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// orientation names a format's shape for a photo prompt.
func orientation(format string) string {
	s, ok := Formats[format]
	switch {
	case !ok || s.X == s.Y:
		return "square"
	case s.X > s.Y:
		return "landscape 16:9"
	}
	return "vertical"
}

// realPeople keeps generated people and places photographic instead of polished (owner, 2026-11-24:
// the reference pictures showed real skin, working people and lived-in rooms).
const realPeople = "Natural skin texture, ordinary clothes, hands in a natural position, real working surroundings with everyday clutter."

// StillPrompt is the exact prompt a scene's generated still is drawn from: a candid phone photo
// for a UGC ad, an advertising photo otherwise, in the ad's own shape.
func StillPrompt(format, delivery, visual string) string {
	visual = strings.TrimSpace(visual)
	if delivery == DeliveryUGC {
		return ugcStillPrompt(format, visual) + " " + silentPeople + " " + realPeople
	}
	return scenePrompt(visual) + " " + silentPeople + " " + realPeople + " " + capitalise(orientation(format)) + " frame."
}

// ActorPrompt is the exact prompt the presenter's actor still is drawn from, in the ad's shape:
// a landscape ad gets a seated, eye-level shot with room around the person (the presenter route
// keeps the image's frame), a vertical ad the selfie framing it always had.
func ActorPrompt(format, desc string) string { return ActorPromptIn(format, desc, "") }

// ActorPromptIn is ActorPrompt with the presenter placed in a setting from the setting brief: the
// background is that real place, softly out of focus, instead of a plain wall.
func ActorPromptIn(format, desc, setting string) string {
	background := "plain uncluttered background"
	if setting = strings.TrimSpace(setting); setting != "" {
		background = "in " + setting + ", the background softly out of focus and free of any writing"
	}
	tail := ", upper body, looking straight at the camera with a relaxed smile, mouth closed, soft natural daylight, " + background + ". Real photo, no text, no letters, no logos."
	switch orientation(format) {
	case "landscape 16:9":
		return "Landscape 16:9 eye-level video-call style photo of " + desc + ", centred in the frame with room on both sides" + tail
	case "square":
		return "Square photo of " + desc + tail
	}
	return "Vertical selfie-style photo of " + desc + tail
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// aspectClose reports whether an image of w x h already has the format's shape (within 2%).
func aspectClose(w, h int, size image.Point) bool {
	if w <= 0 || h <= 0 {
		return false
	}
	a, b := float64(w)/float64(h), float64(size.X)/float64(size.Y)
	return math.Abs(a-b)/b < 0.02
}

// FitFrame places the whole image in the format over a blurred, darkened fill of itself: nothing
// is cropped, and every clip made from it comes out in the ad's shape.
func FitFrame(src image.Image, size image.Point) *image.RGBA {
	canvas := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
	// The fill: the image covering the frame at a sixteenth of the size, scaled back up (a cheap
	// blur), then darkened so the fitted picture stands out.
	small := image.NewRGBA(image.Rect(0, 0, max(1, size.X/16), max(1, size.Y/16)))
	cover(small, src)
	xdraw.ApproxBiLinear.Scale(canvas, canvas.Bounds(), small, small.Bounds(), draw.Src, nil)
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.RGBA{0, 0, 0, 0x50}), image.Point{}, draw.Over)
	sb := src.Bounds()
	scale := math.Min(float64(size.X)/float64(sb.Dx()), float64(size.Y)/float64(sb.Dy()))
	w, h := int(float64(sb.Dx())*scale), int(float64(sb.Dy())*scale)
	x0, y0 := (size.X-w)/2, (size.Y-h)/2
	xdraw.CatmullRom.Scale(canvas, image.Rect(x0, y0, x0+w, y0+h), src, sb, draw.Over, nil)
	return canvas
}

// frameFitter stores start frames fitted to the ad's format.
type frameFitter struct {
	store Store
	posts *Posters
}

// fit returns a link to the scene's start frame in the format's shape: the link itself when the
// picture already has that shape, else a fitted copy stored with the version's stills.
func (f *frameFitter) fit(ctx context.Context, tenantID uuid.UUID, versionID, name, link, format string) (string, error) {
	size, ok := Formats[format]
	if !ok || f == nil || f.store == nil || f.posts == nil {
		return link, nil
	}
	img, err := f.posts.download(ctx, link)
	if err != nil {
		return "", fmt.Errorf("media: start frame: %w", err)
	}
	if b := img.Bounds(); aspectClose(b.Dx(), b.Dy(), size) {
		return link, nil
	}
	data, err := EncodeJPEG(FitFrame(img, size))
	if err != nil {
		return "", err
	}
	_, url, err := f.store.PutPoster(ctx, tenantID, versionID+"-"+name, data)
	if err != nil {
		return "", fmt.Errorf("media: store start frame: %w", err)
	}
	return url, nil
}
