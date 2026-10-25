package media

import (
	"context"
	"fmt"
	"image"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Assembly runs here with ffmpeg (owner's choice, 2026-10-24): free, no upload
// round trips, and the captions and end card are our compositor's exact words.

// Ad limits.
const (
	MaxAdScenes     = 12
	MaxAdSeconds    = 60
	minSceneSeconds = 1.0
	adFPS           = 30
	maxSceneBytes   = 300 << 20
	maxAudioBytes   = 32 << 20
	// endCardSeconds is how long the closing words stay on screen.
	endCardSeconds = 3.0
	// musicGain keeps the bed under the voice.
	musicGain = 0.18
)

// AdScene is one scene in order: a clip (presenter, product motion) or a still
// shown with a slow push-in.
type AdScene struct {
	VideoURL string  `json:"video_url,omitempty"`
	ImageURL string  `json:"image_url,omitempty"`
	Seconds  float64 `json:"seconds"`
	// StartAt takes the scene from this point of a longer clip (owner footage,
	// a screen recording).
	StartAt float64 `json:"start_at,omitempty"`
	// Fit shows the whole frame over a blurred fill instead of cropping to cover:
	// for landscape footage and screen recordings in a portrait ad.
	Fit bool `json:"fit,omitempty"`
}

// AssembleRequest is a whole ad.
type AssembleRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Format    string
	Scenes    []AdScene
	VoiceURL  string       // the full voice-over, optional
	Words     []WordTiming // its timings, for captions
	Captions  bool
	MusicURL  string   // optional bed
	EndCard   *Overlay // the closing words (headline, price, CTA, logo), over the last seconds
	LogoURL   string   // fetched into EndCard.Logo when set
}

// AssembleResult is the stored ad.
type AssembleResult struct {
	URL     string  `json:"url"`
	Key     string  `json:"key"`
	Bytes   int64   `json:"bytes"`
	Seconds float64 `json:"seconds"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	SHA256  string  `json:"sha256"`
	Lines   int     `json:"caption_lines"`
}

// AdStore keeps finished ads (httpware's video-drafts type).
type AdStore interface {
	PutAd(ctx context.Context, tenantID uuid.UUID, versionID, path string) (key, url string, size int64, err error)
}

// Assembler joins scenes, voice, music, captions and the end card into one ad.
type Assembler struct {
	store  AdStore
	fetch  *http.Client
	ffmpeg string
	posts  *Posters // logo download
	logger *zap.Logger
}

// NewAssembler builds the assembler; ffmpeg is looked up on PATH once.
func NewAssembler(store AdStore, posters *Posters, logger *zap.Logger) *Assembler {
	if logger == nil {
		logger = zap.NewNop()
	}
	path, _ := exec.LookPath("ffmpeg")
	return &Assembler{store: store, fetch: &http.Client{Timeout: 3 * time.Minute}, ffmpeg: path, posts: posters,
		logger: logger.Named("media.assemble")}
}

// Ready reports whether ads can be assembled here.
func (a *Assembler) Ready() bool { return a != nil && a.ffmpeg != "" && a.store != nil }

func (r *AssembleRequest) validate() (size image.Point, total float64, err error) {
	size, ok := Formats[r.Format]
	if !ok {
		return size, 0, fmt.Errorf("media: unknown format %q", r.Format)
	}
	if len(r.Scenes) == 0 || len(r.Scenes) > MaxAdScenes {
		return size, 0, fmt.Errorf("media: an ad has 1 to %d scenes", MaxAdScenes)
	}
	for i, s := range r.Scenes {
		src := s.VideoURL
		if src == "" {
			src = s.ImageURL
		}
		if (s.VideoURL == "") == (s.ImageURL == "") || (!strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "http://")) {
			return size, 0, fmt.Errorf("media: scene %d needs one http(s) video or image link", i+1)
		}
		if s.Seconds < minSceneSeconds {
			return size, 0, fmt.Errorf("media: scene %d is shorter than %.0f s", i+1, minSceneSeconds)
		}
		if s.StartAt < 0 || (s.StartAt > 0 && s.VideoURL == "") {
			return size, 0, fmt.Errorf("media: scene %d: start_at needs a video and cannot be negative", i+1)
		}
		total += s.Seconds
	}
	if total > MaxAdSeconds {
		return size, 0, fmt.Errorf("media: an ad is up to %d seconds", MaxAdSeconds)
	}
	return size, total, nil
}

// Make assembles and stores the ad. Every input is downloaded first, so a
// missing scene fails before any encoding.
func (a *Assembler) Make(ctx context.Context, req AssembleRequest) (*AssembleResult, error) {
	if !a.Ready() {
		return nil, ErrNoFFmpeg
	}
	size, total, err := req.validate()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "ad-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	// 1. Each scene to the format, 30 fps, its exact length.
	var list strings.Builder
	for i, s := range req.Scenes {
		raw := filepath.Join(dir, fmt.Sprintf("raw%d", i))
		url := s.VideoURL
		if url == "" {
			url = s.ImageURL
		}
		if err := downloadTo(ctx, a.fetch, url, raw, maxSceneBytes, fmt.Sprintf("scene %d", i+1)); err != nil {
			return nil, err
		}
		out := filepath.Join(dir, fmt.Sprintf("s%d.mp4", i))
		if err := a.normalize(ctx, raw, out, s, size.X, size.Y); err != nil {
			return nil, fmt.Errorf("media: scene %d: %w", i+1, err)
		}
		fmt.Fprintf(&list, "file '%s'\n", filepath.ToSlash(out))
	}
	listPath := filepath.Join(dir, "scenes.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o600); err != nil {
		return nil, err
	}
	joined := filepath.Join(dir, "joined.mp4")
	if err := a.run(ctx, "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", joined); err != nil {
		return nil, fmt.Errorf("media: join scenes: %w", err)
	}

	// 2. Layers: one per caption line, then the end card.
	type layer struct {
		path       string
		start, end float64
	}
	var layers []layer
	lines := 0
	if req.Captions {
		for i, l := range CaptionLines(req.Words) {
			if l.Start >= total {
				break
			}
			img, err := ComposeCaption(req.Format, l.Text)
			if err != nil {
				return nil, err
			}
			p := filepath.Join(dir, fmt.Sprintf("cap%d.png", i))
			if err := writePNG(p, img); err != nil {
				return nil, err
			}
			layers = append(layers, layer{p, l.Start, math.Min(l.End, total)})
			lines++
		}
	}
	if req.EndCard != nil {
		card := *req.EndCard
		if req.LogoURL != "" && card.Logo == nil && a.posts != nil {
			if logo, err := a.posts.download(ctx, req.LogoURL); err == nil {
				card.Logo = logo
			}
		}
		img, err := ComposeOverlay(req.Format, card)
		if err != nil {
			return nil, err
		}
		p := filepath.Join(dir, "endcard.png")
		if err := writePNG(p, img); err != nil {
			return nil, err
		}
		start := math.Max(0, total-endCardSeconds)
		// Captions give way to the end card.
		kept := layers[:0]
		for _, l := range layers {
			if l.start < start {
				l.end = math.Min(l.end, start)
				kept = append(kept, l)
			}
		}
		layers = append(kept, layer{p, start, total})
	}

	// 3. Audio inputs.
	args := []string{"-i", joined}
	audioIn := []int{}
	next := 1
	for _, src := range []struct{ url, name string }{{req.VoiceURL, "voice"}, {req.MusicURL, "music"}} {
		if src.url == "" {
			audioIn = append(audioIn, -1)
			continue
		}
		p := filepath.Join(dir, src.name+".audio")
		if err := downloadTo(ctx, a.fetch, src.url, p, maxAudioBytes, src.name); err != nil {
			return nil, err
		}
		args = append(args, "-i", p)
		audioIn = append(audioIn, next)
		next++
	}
	for _, l := range layers {
		args = append(args, "-i", l.path)
	}

	// 4. One filter graph: layers over the video, voice and music mixed and
	// brought to the social loudness target.
	var graph []string
	v := "[0:v]"
	for i, l := range layers {
		outLabel := fmt.Sprintf("[v%d]", i)
		graph = append(graph, fmt.Sprintf("%s[%d:v]overlay=0:0:format=auto:enable='between(t,%.3f,%.3f)'%s", v, next+i, l.start, l.end, outLabel))
		v = outLabel
	}
	graph = append(graph, v+"format=yuv420p[vout]")
	voice, music := audioIn[0], audioIn[1]
	hasAudio := voice >= 0 || music >= 0
	// apad holds silence after a voice that ends before the scenes; -t below cuts
	// the ad to the scenes' length.
	const loud = "loudnorm=I=-14:TP=-1.5:LRA=11,apad"
	switch {
	case voice >= 0 && music >= 0:
		graph = append(graph,
			fmt.Sprintf("[%d:a]aresample=48000,volume=%.2f[m]", music, musicGain),
			fmt.Sprintf("[%d:a]aresample=48000,apad[vo]", voice),
			"[vo][m]amix=inputs=2:duration=shortest:dropout_transition=0:normalize=0,"+loud+"[aout]")
	case voice >= 0:
		graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,%s[aout]", voice, loud))
	case music >= 0:
		graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,volume=%.2f,%s[aout]", music, musicGain*3, loud))
	}
	out := filepath.Join(dir, "ad.mp4")
	args = append(args, "-filter_complex", strings.Join(graph, ";"), "-map", "[vout]")
	if hasAudio {
		args = append(args, "-map", "[aout]", "-c:a", "aac", "-b:a", "160k", "-ar", "48000")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-t", strconv.FormatFloat(total, 'f', 3, 64),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "21", "-r", strconv.Itoa(adFPS),
		"-movflags", "+faststart", out)
	if err := a.run(ctx, args...); err != nil {
		return nil, fmt.Errorf("media: assemble: %w", err)
	}

	sum, err := fileSHA256(out)
	if err != nil {
		return nil, err
	}
	key, url, n, err := a.store.PutAd(ctx, req.TenantID, req.VersionID, out)
	if err != nil {
		return nil, fmt.Errorf("media: store ad: %w", err)
	}
	return &AssembleResult{URL: url, Key: key, Bytes: n, Seconds: total, Width: size.X, Height: size.Y, SHA256: sum, Lines: lines}, nil
}

// cleanPTS renumbers a scene's frames from zero at the ad's frame rate. Inputs
// carry their own timing (a WebP still's decoder, a phone clip's variable rate),
// and after the scenes are joined any odd timestamp shifts the clock that
// switches the captions and the end card on and off.
var cleanPTS = fmt.Sprintf("setpts=N/(%d*TB)", adFPS)

// sceneColour converts every scene to one colour standard (BT.709, TV range),
// and sceneColourTags writes it into each piece. Sources differ: a JPEG or WebP
// still decodes as full or TV range BT.601, a PNG or phone clip carries none.
// Joined pieces with different colour parameters make ffmpeg rebuild its
// filter graph mid-ad, and the caption and end card layers (single images,
// already consumed) then vanish for the rest of the ad.
const sceneColour = "scale=out_color_matrix=bt709:out_range=tv,format=yuv420p"

var sceneColourTags = []string{"-color_range", "tv", "-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709"}

// normalize turns one scene into a silent H.264 piece of the format: a clip is
// cropped to cover (or fitted over a blurred fill) and held on its last frame
// if it is short; a still gets a slow push-in.
func (a *Assembler) normalize(ctx context.Context, in, out string, s AdScene, w, h int) error {
	dur := strconv.FormatFloat(s.Seconds, 'f', 3, 64)
	// frame is the scene at w x h before the push-in or hold.
	frame := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", w, h, w, h)
	if s.Fit {
		// The fill is blurred at a quarter size (cheap), kept even for 4:2:0, then
		// scaled back up and darkened so the fitted frame stands out.
		bw, bh := w/8*2, h/8*2
		frame = fmt.Sprintf("split[bgsrc][fgsrc];"+
			"[bgsrc]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=12:2,scale=%d:%d,eq=brightness=-0.12[bg];"+
			"[fgsrc]scale=%d:%d:force_original_aspect_ratio=decrease[fg];[bg][fg]overlay=(W-w)/2:(H-h)/2",
			bw, bh, bw, bh, w, h, w, h)
	}
	var args []string
	if s.VideoURL == "" {
		frames := int(math.Ceil(s.Seconds * adFPS))
		// zoompan turns the single input frame into exactly `frames` frames with
		// clean timestamps. The still must not be looped (-loop 1): zoompan would
		// restart its zoom on every looped frame (no motion), make d frames for
		// each (slow), and leave timestamps that break the overlays' clock after
		// the scenes are joined. The push-in works on a 2x frame so it does not judder.
		zoomStep := 0.08 / float64(frames)
		vf := fmt.Sprintf("%s,scale=%d:%d,zoompan=z='min(zoom+%.6f,1.08)':x='iw/2-(iw/zoom/2)':y='ih/2-(ih/zoom/2)':d=%d:s=%dx%d:fps=%d,setsar=1,%s,%s",
			frame, w*2, h*2, zoomStep, frames, w, h, adFPS, cleanPTS, sceneColour)
		args = []string{"-i", in, "-filter_complex", vf, "-frames:v", strconv.Itoa(frames)}
	} else {
		vf := fmt.Sprintf("%s,setsar=1,fps=%d,tpad=stop_mode=clone:stop_duration=%s,%s,%s", frame, adFPS, dur, cleanPTS, sceneColour)
		args = []string{"-ss", strconv.FormatFloat(s.StartAt, 'f', 3, 64), "-i", in, "-filter_complex", vf, "-t", dur}
	}
	args = append(args, "-an", "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-r", strconv.Itoa(adFPS))
	args = append(append(args, sceneColourTags...), out)
	return a.run(ctx, args...)
}

func (a *Assembler) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, a.ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, truncate(string(b), 400))
	}
	return nil
}
