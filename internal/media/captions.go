package media

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
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

// ComposeCaption draws one caption line on a transparent canvas of the format:
// bold white text on a dark rounded-corner box, centred in the lower third,
// clear of the end card's band.
func ComposeCaption(format, text string) (*image.RGBA, error) {
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
	margin := W * 8 / 100
	maxW := W - 2*margin
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
	padX, padY := int(fontSize*0.5), int(fontSize*0.3)
	boxW := 0
	for _, l := range lines {
		boxW = max(boxW, font.MeasureString(fc, l).Ceil())
	}
	boxW += 2 * padX
	boxH := lineH*len(lines) + 2*padY
	bottom := H * 72 / 100
	box := image.Rect((W-boxW)/2, bottom-boxH, (W+boxW)/2, bottom)
	fillRounded(canvas, box, int(fontSize*0.35), color.RGBA{0, 0, 0, 0xb4})
	d := &font.Drawer{Dst: canvas, Src: image.NewUniform(color.White), Face: fc}
	for i, l := range lines {
		tw := font.MeasureString(fc, l).Ceil()
		d.Dot = fixed.P((W-tw)/2, box.Min.Y+padY+lineH*(i+1)-int(fontSize*0.25))
		d.DrawString(l)
	}
	return canvas, nil
}

// fillRounded fills r with c, its corners rounded by radius.
func fillRounded(dst *image.RGBA, r image.Rectangle, radius int, c color.RGBA) {
	src := image.NewUniform(c)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		inset := 0
		if dy := min(y-r.Min.Y, r.Max.Y-1-y); dy < radius {
			// Circle: x inset for this row of the corner.
			off := radius - dy
			for inset < radius && (radius-inset)*(radius-inset)+off*off > radius*radius {
				inset++
			}
		}
		draw.Draw(dst, image.Rect(r.Min.X+inset, y, r.Max.X-inset, y+1), src, image.Point{}, draw.Over)
	}
}
