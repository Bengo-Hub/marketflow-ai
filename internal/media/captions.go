package media

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// Captions are drawn by our compositor, like poster text, so every word on
// screen is exactly the script's (most viewers watch muted).

// CaptionLine is one caption shown from Start to End.
type CaptionLine struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Caption pacing: short lines that a viewer reads at a glance.
const (
	captionMaxWords   = 4
	captionMaxSeconds = 1.8
	captionMinSeconds = 0.6
)

// CaptionLines groups word timings into lines of up to captionMaxWords words
// or captionMaxSeconds, breaking after a sentence end. Each line stays on
// screen until the next starts (short gaps are bridged), so captions never
// flicker between words.
func CaptionLines(words []WordTiming) []CaptionLine {
	var lines []CaptionLine
	var cur []WordTiming
	flush := func() {
		if len(cur) == 0 {
			return
		}
		texts := make([]string, len(cur))
		for i, w := range cur {
			texts[i] = w.Text
		}
		lines = append(lines, CaptionLine{Text: strings.Join(texts, " "), Start: cur[0].Start, End: cur[len(cur)-1].End})
		cur = cur[:0]
	}
	for _, w := range words {
		if strings.TrimSpace(w.Text) == "" {
			continue
		}
		if len(cur) > 0 && (len(cur) >= captionMaxWords || w.End-cur[0].Start > captionMaxSeconds) {
			flush()
		}
		cur = append(cur, w)
		if strings.HasSuffix(w.Text, ".") || strings.HasSuffix(w.Text, "!") || strings.HasSuffix(w.Text, "?") {
			flush()
		}
	}
	flush()
	// A short line is held for captionMinSeconds and a short gap is bridged, but a
	// line never runs into the next: two captions are never on screen at once.
	for i := range lines {
		if lines[i].End-lines[i].Start < captionMinSeconds {
			lines[i].End = lines[i].Start + captionMinSeconds
		}
		if i+1 < len(lines) {
			if next := lines[i+1].Start; lines[i].End > next || next-lines[i].End < 0.4 {
				lines[i].End = next
			}
		}
	}
	return lines
}

// Caption placement, as fractions of the frame. Every caption sits inside the 9:16 safe
// band (a 720x1200 window centred in 1080x1920: two thirds of the width, the middle
// 62.5% of the height), clear of the feed's own buttons and the end card's band.
const (
	safeBandWidth   = 2.0 / 3
	safeBandTop     = 0.1875
	captionBottomY  = 0.70 // spoken-word captions end here, as in the bake-off cut
	sceneCaptionTop = 0.20 // a scene's on-screen caption starts here
)

// ComposeCaption draws one spoken-word caption on a transparent canvas of the format:
// bold white text with a black outline, at most 2 lines, centred in the lower part of
// the safe band.
func ComposeCaption(format, text string) (*image.RGBA, error) {
	return composeCaptionAt(format, text, captionBottomY, false)
}

// ComposeSceneCaption draws a scene's on-screen caption (the script's caption field) in
// the same style near the top of the safe band, so it never covers the spoken words.
func ComposeSceneCaption(format, text string) (*image.RGBA, error) {
	return composeCaptionAt(format, text, sceneCaptionTop, true)
}

// composeCaptionAt lays the text out within the safe band's width, shrinking it to fit
// two lines, and anchors its block at y (the bottom edge, or the top edge when fromTop).
func composeCaptionAt(format, text string, y float64, fromTop bool) (*image.RGBA, error) {
	size, ok := Formats[format]
	if !ok {
		return nil, fmt.Errorf("media: unknown format %q", format)
	}
	bold, _, err := fonts()
	if err != nil {
		return nil, fmt.Errorf("media: fonts: %w", err)
	}
	W, H := size.X, size.Y
	canvas := image.NewRGBA(image.Rect(0, 0, W, H))
	text = strings.TrimSpace(text)
	if text == "" {
		return canvas, nil
	}
	maxW := int(float64(W) * safeBandWidth)
	fontSize := float64(W) / 16
	var lines []string
	var fc font.Face
	for ; fontSize >= 20; fontSize *= 0.92 {
		fc, _ = face(bold, fontSize)
		lines = wrap(fc, text, maxW)
		if len(lines) <= 2 {
			break
		}
	}
	if len(lines) > 2 {
		lines = lines[:2]
		lines[1] = ellipsize(fc, lines[1], maxW)
	}
	lineH := int(fontSize * 1.2)
	blockH := lineH * len(lines)
	top := int(float64(H)*y) - blockH
	if fromTop {
		top = int(float64(H) * y)
	}
	if minTop := int(float64(H) * safeBandTop); top < minTop {
		top = minTop
	}
	outline := max(2, int(fontSize*0.09))
	black := image.NewUniform(color.Black)
	white := image.NewUniform(color.White)
	for i, l := range lines {
		tw := font.MeasureString(fc, l).Ceil()
		base := fixed.P((W-tw)/2, top+lineH*(i+1)-int(fontSize*0.25))
		// The outline: the line drawn in black around a ring, then white on top.
		for dy := -outline; dy <= outline; dy++ {
			for dx := -outline; dx <= outline; dx++ {
				if dx*dx+dy*dy > outline*outline {
					continue
				}
				d := &font.Drawer{Dst: canvas, Src: black, Face: fc, Dot: base.Add(fixed.P(dx, dy))}
				d.DrawString(l)
			}
		}
		d := &font.Drawer{Dst: canvas, Src: white, Face: fc, Dot: base}
		d.DrawString(l)
	}
	return canvas, nil
}
