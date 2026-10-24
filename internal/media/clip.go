package media

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CapImageToVideo animates a still (a poster's clean scene) into a short clip.
const CapImageToVideo Capability = "image_to_video"

// ClipRoutes are the image-to-video routes in fallback order before scoring.
// PriceUSD is per second (the router compares routes of one capability, so
// per-second prices order them correctly); request shapes were confirmed
// 2026-10-24, see docs/integrations/video-generation-providers.md section 3.
var ClipRoutes = []Route{
	{Key: "higgsfield/kling-v3-std", Provider: "higgsfield", Capability: CapImageToVideo, PriceUSD: 0.084, Quality: 4, PAYG: true, Timeout: 6 * time.Minute},
	{Key: "fal/grok-imagine-video", Provider: "fal", Capability: CapImageToVideo, PriceUSD: 0.05, Quality: 3, PAYG: true, Timeout: 6 * time.Minute},
	{Key: "fal/kling-v3-pro", Provider: "fal", Capability: CapImageToVideo, PriceUSD: 0.14, Quality: 4, PAYG: true, Timeout: 6 * time.Minute},
	{Key: "higgsfield/seedance-2.0", Provider: "higgsfield", Capability: CapImageToVideo, PriceUSD: 0.141, Quality: 4.5, PAYG: true, Timeout: 8 * time.Minute, MinTier: TierPremium},
}

// Clip limits.
const (
	MinClipSeconds = 4  // Seedance's minimum; Kling allows 3
	MaxClipSeconds = 10 // a motion poster, not a film
	maxClipBytes   = 300 << 20
)

// ErrNoFFmpeg means the text overlay cannot be drawn here, so no clip is made
// (and nothing is paid for): a clip without its exact words is not an ad.
var ErrNoFFmpeg = errors.New("media: ffmpeg is not installed")

// ClipRequest is one motion poster: the clean scene animated, the poster's
// words laid over it.
type ClipRequest struct {
	TenantID  uuid.UUID
	VersionID string // names the stored file
	SceneURL  string // the poster's stored scene without text
	Prompt    string // the motion, in plain words
	Format    string // a key of Formats
	Seconds   int
	Tier      string
	Overlay   Overlay
	LogoURL   string
}

// ClipResult is the stored clip.
type ClipResult struct {
	URL       string  `json:"url"`
	Key       string  `json:"key"`
	Bytes     int64   `json:"bytes"`
	Seconds   int     `json:"seconds"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Route     string  `json:"route"`
	CostUSD   float64 `json:"cost_usd"`
	SourceURL string  `json:"source_url"` // the provider's clip link (expires)
	Attempts  int     `json:"attempts"`
}

// VideoStore keeps finished clips (httpware/objectstore's video-drafts type).
type VideoStore interface {
	PutClip(ctx context.Context, tenantID uuid.UUID, versionID, path string) (key, url string, size int64, err error)
}

// Clips makes motion posters.
type Clips struct {
	router *Router
	keys   KeySource
	store  VideoStore
	queue  *queueClient
	fetch  *http.Client
	ffmpeg string
	logger *zap.Logger
	posts  *Posters // logo download and the shared fetch rules
}

// NewClips builds the clip pipeline; ffmpeg is looked up on PATH once.
func NewClips(keys KeySource, store VideoStore, posters *Posters, logger *zap.Logger) *Clips {
	if logger == nil {
		logger = zap.NewNop()
	}
	path, _ := exec.LookPath("ffmpeg")
	return &Clips{router: NewRouter(ClipRoutes), keys: keys, store: store, queue: newQueueClient(),
		fetch: &http.Client{Timeout: 3 * time.Minute}, ffmpeg: path, logger: logger.Named("media.clips"), posts: posters}
}

// Ready reports whether clips can be made here (ffmpeg and storage present).
func (c *Clips) Ready() bool { return c != nil && c.ffmpeg != "" && c.store != nil }

// Make animates the scene on the best route with fallbacks, lays the exact
// words over it and stores the clip. ctx should allow several minutes.
func (c *Clips) Make(ctx context.Context, req ClipRequest) (*ClipResult, error) {
	if !c.Ready() {
		return nil, ErrNoFFmpeg
	}
	size, ok := Formats[req.Format]
	if !ok {
		return nil, fmt.Errorf("media: unknown format %q", req.Format)
	}
	if req.Seconds < MinClipSeconds || req.Seconds > MaxClipSeconds {
		return nil, fmt.Errorf("media: a clip is %d to %d seconds", MinClipSeconds, MaxClipSeconds)
	}
	if req.Tier == "" {
		req.Tier = TierStandard
	}
	// The overlay is drawn before any provider is paid, so a layout failure costs nothing.
	if req.LogoURL != "" && req.Overlay.Logo == nil && c.posts != nil {
		if logo, err := c.posts.download(ctx, req.LogoURL); err == nil {
			req.Overlay.Logo = logo
		}
	}
	layer, err := ComposeOverlay(req.Format, req.Overlay)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "clip-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	overlayPath := filepath.Join(dir, "overlay.png")
	if err := writePNG(overlayPath, layer); err != nil {
		return nil, err
	}

	routes := c.router.Order(CapImageToVideo, req.Tier, func(rt Route) bool { return c.keys(ctx, rt.Provider) != "" })
	var lastErr error
	for i, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		src, err := c.animate(actx, rt, req)
		raw := filepath.Join(dir, "raw.mp4")
		if err == nil {
			err = c.save(actx, src, raw)
		}
		cancel()
		if err != nil {
			lastErr = err
			c.router.Failed(rt.Key, err, ctx.Err() != nil)
			c.logger.Warn("clip route failed, trying the next", zap.String("route", rt.Key), zap.Error(err))
			continue
		}
		c.router.Succeeded(rt.Key)

		out := filepath.Join(dir, "clip.mp4")
		if err := c.overlay(ctx, raw, overlayPath, out, size.X, size.Y, req.Seconds); err != nil {
			return nil, fmt.Errorf("media: text overlay: %w", err)
		}
		key, url, n, err := c.store.PutClip(ctx, req.TenantID, req.VersionID, out)
		if err != nil {
			return nil, fmt.Errorf("media: store clip: %w", err)
		}
		return &ClipResult{URL: url, Key: key, Bytes: n, Seconds: req.Seconds, Width: size.X, Height: size.Y,
			Route: rt.Key, CostUSD: rt.PriceUSD * float64(req.Seconds), SourceURL: src, Attempts: i + 1}, nil
	}
	if lastErr == nil {
		return nil, ErrNoRoute
	}
	return nil, fmt.Errorf("%w: %v", ErrNoRoute, lastErr)
}

type videoOut struct {
	Video struct {
		URL string `json:"url"`
	} `json:"video"`
}

// animate runs one route and returns the provider's clip URL.
func (c *Clips) animate(ctx context.Context, rt Route, req ClipRequest) (string, error) {
	key := c.keys(ctx, rt.Provider)
	prompt := clipPrompt(req.Prompt)
	var (
		job   queueJob
		input map[string]any
	)
	switch rt.Key {
	case "higgsfield/kling-v3-std":
		input = map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "sound": "off"}
		job = higgsfieldJob(key, "kling-video/v3.0/std/image-to-video")
	case "higgsfield/seedance-2.0":
		input = map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "resolution": "1080p", "generate_audio": false}
		job = higgsfieldJob(key, "bytedance/seedance-2.0/image-to-video")
	case "fal/grok-imagine-video":
		input = map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "resolution": "720p"}
		job = falJob(key, "xai/grok-imagine-video/image-to-video")
	case "fal/kling-v3-pro":
		input = map[string]any{"start_image_url": req.SceneURL, "prompt": prompt, "duration": strconv.Itoa(req.Seconds), "generate_audio": false}
		job = falJob(key, "fal-ai/kling-video/v3/pro/image-to-video")
	default:
		return "", fmt.Errorf("media: no builder for route %s", rt.Key)
	}
	var out videoOut
	if err := c.queue.run(ctx, job, input, &out); err != nil {
		return "", err
	}
	if out.Video.URL == "" {
		return "", &errJobFailed{provider: rt.Provider, status: "completed", detail: "no video in the result"}
	}
	return out.Video.URL, nil
}

// clipPrompt keeps the motion gentle and the frame free of text: our words go on top.
func clipPrompt(p string) string {
	if p == "" {
		p = "Slow, smooth camera push-in; subtle natural motion in the scene"
	}
	return p + ". No text, no letters, no logos added. Keep the product unchanged."
}

// save downloads the provider's clip to path (bounded).
func (c *Clips) save(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.fetch.Do(req)
	if err != nil {
		return fmt.Errorf("download clip: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download clip: status %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxClipBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download clip: %w", err)
	}
	if n > maxClipBytes {
		return fmt.Errorf("download clip: larger than %d MB", maxClipBytes>>20)
	}
	return nil
}

// overlay scales the clip to cover the format, lays the text layer on top and
// encodes H.264 for every feed (yuv420p, fast start, no audio).
func (c *Clips) overlay(ctx context.Context, raw, layer, out string, w, h, seconds int) error {
	filter := fmt.Sprintf("[0:v]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1[bg];[bg][1:v]overlay=0:0:format=auto,format=yuv420p[v]", w, h, w, h)
	cmd := exec.CommandContext(ctx, c.ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-i", raw, "-i", layer, "-filter_complex", filter, "-map", "[v]", "-an",
		"-t", strconv.Itoa(seconds), "-c:v", "libx264", "-preset", "veryfast", "-crf", "22",
		"-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, truncate(string(b), 300))
	}
	return nil
}

// writePNG saves the transparent text layer for ffmpeg.
func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
