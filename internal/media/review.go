package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
)

// The self-review (owner, 2026-11-20): before a storyboard, a poster or a video reaches the owner,
// a cheap vision model looks at the finished pictures the way the owner would: is the logo placed
// well and readable, is the text legible, is there garbled writing, is it the right brand, does
// the scene fit the offering, is it in the business's own town. A failing part is made again
// inside the same budget (a still or a clip redrawn, the end card laid out again), at most
// max_retries times, so the first version the owner sees is close to final. Anything still wrong
// after the retries is reported with the result, never hidden.
//
// Settings: ProviderSetting platform / media / review (ReviewSettingsFromMap). The model is a
// list of targets (provider and model id) in config, tried in order.

// Review issue kinds.
const (
	IssueLogo         = "logo"          // logo missing, too small, on a face or busy area, unreadable
	IssueLegibility   = "legibility"    // text hard to read on its background
	IssueTextArtefact = "text_artefact" // garbled or invented writing in the picture
	IssueWrongBrand   = "wrong_brand"   // another business's brand or logo
	IssueIrrelevant   = "irrelevant"    // the scene does not show the offering or its use
	IssueOffLocation  = "off_location"  // the place does not fit the business's town or setting
	IssueOther        = "other"
)

// ReviewIssue is one problem the reviewer found.
type ReviewIssue struct {
	Frame    string `json:"frame"`
	Kind     string `json:"kind"`
	Severity string `json:"severity,omitempty"` // high | low
	Note     string `json:"note,omitempty"`
}

// ReviewReport is what the self-review did, returned with the result for the owner and the logs.
type ReviewReport struct {
	Model   string        `json:"model,omitempty"`
	Passed  bool          `json:"passed"`
	Rounds  int           `json:"rounds"`
	Issues  []ReviewIssue `json:"issues,omitempty"` // what is still wrong after the last round
	Fixed   []string      `json:"fixed,omitempty"`  // what was made again
	CostUSD float64       `json:"cost_usd"`         // the review calls and the redone parts
	Skipped string        `json:"skipped,omitempty"`
}

// ReviewFrame is one picture to review.
type ReviewFrame struct {
	Label string // a scene id, "actor", "poster" or "end_card"
	Image image.Image
	// Expect is what this frame should show (its visual and setting, or the end card's words).
	Expect string
}

// ReviewBrief is what the pictures should be.
type ReviewBrief struct {
	Kind     string // storyboard | poster | video
	Brand    string // the business's name
	Offering string
	// Place is where the business is (its address or the setting brief's home base).
	Place string
	// Words are the exact words drawn on the picture (headline, CTA, contacts), so the reviewer
	// can tell drawn text from garbled writing in the scene.
	Words string
}

// ReviewVerdict is one review round's answer.
type ReviewVerdict struct {
	Pass    bool          `json:"pass"`
	Issues  []ReviewIssue `json:"issues"`
	Model   string        `json:"-"`
	CostUSD float64       `json:"-"`
}

// VisionCall asks a vision model one question with pictures (data URLs) and returns its reply.
// The app wires it to the LLM router with the review targets from the settings.
type VisionCall func(ctx context.Context, system, user string, images []string) (reply, model string, costUSD float64, err error)

// ReviewSettings is the review config (ProviderSetting platform / media / review).
type ReviewSettings struct {
	Enabled bool `json:"enabled"`
	// MaxRetries is how many times a failing part is made again (0: review and report only).
	MaxRetries int `json:"max_retries"`
	// PosterRetryUSD is the most a poster's redrawn scenes may add at provider prices.
	PosterRetryUSD float64 `json:"poster_retry_usd"`
	// StillRetryUSD is the most a storyboard's redrawn stills may add.
	StillRetryUSD float64 `json:"still_retry_usd"`
	// VideoFrames caps the frames sent from one video (one per scene and the end card).
	VideoFrames int `json:"video_frames"`
	// Prompt replaces the reviewer's instructions when set.
	Prompt string `json:"prompt,omitempty"`
	// Targets are the vision models, tried in order (JSON list of {"provider","model"}); the app
	// reads them, the media package only passes them through.
	Targets string `json:"targets,omitempty"`
}

// Review setting fallbacks, used only when the platform has not set them.
const (
	DefaultReviewRetries   = 1
	DefaultPosterRetryUSD  = 0.10
	DefaultStillRetryUSD   = 0.10
	DefaultReviewFrames    = 8
	maxReviewRetries       = 3
	reviewImageLongSide    = 768
	reviewCallTimeout      = 40 * time.Second
	reviewFrameExtractTime = 30 * time.Second
)

// DefaultReviewSettings is the fallback config: on, one retry.
func DefaultReviewSettings() ReviewSettings {
	return ReviewSettings{Enabled: true, MaxRetries: DefaultReviewRetries, PosterRetryUSD: DefaultPosterRetryUSD,
		StillRetryUSD: DefaultStillRetryUSD, VideoFrames: DefaultReviewFrames}
}

// ReviewSettingsFromMap reads the platform's review settings; anything unreadable keeps the default.
func ReviewSettingsFromMap(s map[string]string) ReviewSettings {
	out := DefaultReviewSettings()
	if v, err := strconv.ParseBool(strings.TrimSpace(s["enabled"])); err == nil {
		out.Enabled = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s["max_retries"])); err == nil && v >= 0 {
		out.MaxRetries = min(v, maxReviewRetries)
	}
	for key, dst := range map[string]*float64{"poster_retry_usd": &out.PosterRetryUSD, "still_retry_usd": &out.StillRetryUSD} {
		if v, err := strconv.ParseFloat(strings.TrimSpace(s[key]), 64); err == nil && v >= 0 {
			*dst = v
		}
	}
	if v, err := strconv.Atoi(strings.TrimSpace(s["video_frames"])); err == nil && v > 0 {
		out.VideoFrames = min(v, 12)
	}
	out.Prompt = strings.TrimSpace(s["prompt"])
	out.Targets = strings.TrimSpace(s["targets"])
	return out
}

// Reviewer runs review rounds with the vision model.
type Reviewer struct {
	call     VisionCall
	settings func(ctx context.Context) ReviewSettings
}

// NewReviewer builds a reviewer; settings nil uses the defaults.
func NewReviewer(call VisionCall, settings func(ctx context.Context) ReviewSettings) *Reviewer {
	return &Reviewer{call: call, settings: settings}
}

var (
	reviewerMu sync.RWMutex
	reviewer   *Reviewer
)

// SetReviewer wires the self-review for every pipeline (nil turns it off).
func SetReviewer(r *Reviewer) {
	reviewerMu.Lock()
	reviewer = r
	reviewerMu.Unlock()
}

func activeReviewer() *Reviewer {
	reviewerMu.RLock()
	defer reviewerMu.RUnlock()
	return reviewer
}

// Settings is the review config in force.
func (r *Reviewer) Settings(ctx context.Context) ReviewSettings {
	if r == nil {
		return ReviewSettings{}
	}
	if r.settings == nil {
		return DefaultReviewSettings()
	}
	return r.settings(ctx)
}

// on reports whether reviews run now, with the settings.
func reviewOn(ctx context.Context) (*Reviewer, ReviewSettings, bool) {
	r := activeReviewer()
	if r == nil || r.call == nil {
		return nil, ReviewSettings{}, false
	}
	s := r.Settings(ctx)
	return r, s, s.Enabled
}

// defaultReviewPrompt is the reviewer's instructions when the settings give none.
const defaultReviewPrompt = `You check marketing pictures for a small business before the owner sees them, like a careful art director.
For each picture, compare it with what it should show and report only real problems:
- logo: the brand logo is missing where expected, too small to read, cut off, sits on a face or a busy area, or has no contrast with what is behind it.
- legibility: words drawn on the picture are hard to read on their background, cut off or overlapping.
- text_artefact: garbled, misspelt or invented writing, signs or captions in the photo itself (drawn words listed as exact words are fine).
- wrong_brand: another company's logo or brand name is visible.
- irrelevant: the scene does not show the offering, its use or its buyer as described.
- off_location: the place does not fit the business's town or setting (for example a foreign city, a generic studio, snow, or a village where a modern town is expected).
Severity is high when the owner would reject the picture, low when it is a small blemish.
Reply with one JSON object only: {"pass":true|false,"issues":[{"frame":"<label>","kind":"logo|legibility|text_artefact|wrong_brand|irrelevant|off_location|other","severity":"high|low","note":"<under 20 words>"}]}.
pass is false when any issue is high.`

// Review runs one round over the frames. A failed call returns an error and no verdict.
func (r *Reviewer) Review(ctx context.Context, brief ReviewBrief, frames []ReviewFrame) (ReviewVerdict, error) {
	if r == nil || r.call == nil || len(frames) == 0 {
		return ReviewVerdict{Pass: true}, nil
	}
	s := r.Settings(ctx)
	system := defaultReviewPrompt
	if s.Prompt != "" {
		system = s.Prompt
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Kind: %s.\n", brief.Kind)
	if brief.Brand != "" {
		fmt.Fprintf(&b, "Business: %s.\n", brief.Brand)
	}
	if brief.Offering != "" {
		fmt.Fprintf(&b, "Offering: %s.\n", brief.Offering)
	}
	if brief.Place != "" {
		fmt.Fprintf(&b, "Where the business and its buyers are: %s.\n", brief.Place)
	}
	if w := strings.TrimSpace(brief.Words); w != "" {
		fmt.Fprintf(&b, "Exact words drawn on the pictures: %s\n", strings.Join(strings.Fields(w), " "))
	}
	b.WriteString("Pictures, in order:\n")
	var images []string
	for i, f := range frames {
		data, err := reviewJPEG(f.Image)
		if err != nil {
			continue
		}
		images = append(images, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(data))
		fmt.Fprintf(&b, "%d. label %q", i+1, f.Label)
		if e := strings.TrimSpace(f.Expect); e != "" {
			fmt.Fprintf(&b, ": should show %s", truncate(e, 400))
		}
		b.WriteString("\n")
	}
	if len(images) == 0 {
		return ReviewVerdict{Pass: true}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, reviewCallTimeout)
	defer cancel()
	reply, model, cost, err := r.call(cctx, system, b.String(), images)
	if err != nil {
		return ReviewVerdict{}, err
	}
	v, err := parseVerdict(reply)
	if err != nil {
		return ReviewVerdict{}, err
	}
	v.Model, v.CostUSD = model, cost
	return v, nil
}

// parseVerdict reads the reviewer's JSON; unknown kinds become "other" and a high issue fails.
func parseVerdict(reply string) (ReviewVerdict, error) {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return ReviewVerdict{}, fmt.Errorf("media: the review reply has no JSON")
	}
	var v ReviewVerdict
	if err := json.Unmarshal([]byte(reply[start:end+1]), &v); err != nil {
		return ReviewVerdict{}, fmt.Errorf("media: unreadable review: %w", err)
	}
	high := false
	kept := v.Issues[:0]
	for _, is := range v.Issues {
		switch is.Kind {
		case IssueLogo, IssueLegibility, IssueTextArtefact, IssueWrongBrand, IssueIrrelevant, IssueOffLocation:
		default:
			is.Kind = IssueOther
		}
		if is.Severity != "low" {
			is.Severity = "high"
			high = true
		}
		is.Note = truncate(strings.TrimSpace(is.Note), 200)
		kept = append(kept, is)
		if len(kept) == 20 {
			break
		}
	}
	v.Issues = kept
	if high {
		v.Pass = false
	}
	return v, nil
}

// highIssues are the high-severity issues by frame.
func highIssues(v ReviewVerdict) map[string][]ReviewIssue {
	out := map[string][]ReviewIssue{}
	for _, is := range v.Issues {
		if is.Severity == "high" {
			out[is.Frame] = append(out[is.Frame], is)
		}
	}
	return out
}

// layoutOnly reports whether every issue is fixed by laying the text and logo out again.
func layoutOnly(issues []ReviewIssue) bool {
	for _, is := range issues {
		if is.Kind != IssueLogo && is.Kind != IssueLegibility {
			return false
		}
	}
	return len(issues) > 0
}

// otherCorner is the logo corner a layout retry moves to.
func otherCorner(c string) string {
	if c == LogoTopRight {
		return LogoTopLeft
	}
	return LogoTopRight
}

// reviewJPEG is a picture scaled to the review size, as a JPEG.
func reviewJPEG(img image.Image) ([]byte, error) {
	if img == nil {
		return nil, fmt.Errorf("media: no picture")
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > reviewImageLongSide {
		w, h = w*reviewImageLongSide/long, h*reviewImageLongSide/long
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(1, w), max(1, h)))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// frameAt reads one frame of a stored video at a time in seconds.
func (a *Assembler) frameAt(ctx context.Context, url string, at float64) (image.Image, error) {
	if a == nil || a.ffmpeg == "" {
		return nil, ErrNoFFmpeg
	}
	dir, err := os.MkdirTemp("", "review-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "f.jpg")
	cctx, cancel := context.WithTimeout(ctx, reviewFrameExtractTime)
	defer cancel()
	cmd := exec.CommandContext(cctx, a.ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(max(0, at), 'f', 2, 64), "-i", url, "-frames:v", "1", "-q:v", "3", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("media: frame at %.1fs: %w: %s", at, err, truncate(string(b), 200))
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	return img, err
}
