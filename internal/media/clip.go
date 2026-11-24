package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/guardrail"
)

// CapImageToVideo animates a still (a poster's clean scene) into a short clip.
const CapImageToVideo Capability = "image_to_video"

// ClipRoutes are the image-to-video routes. Higgsfield bills per generation
// (measured 2026-11-03 through /models/usage and /estimate, see
// docs/integrations/ugc-provider-bakeoff.md); the fal fallbacks bill per second at
// their docs prices. The standard tier starts on Hailuo 2.3 std, the least costly
// route, then Kling 3.0 std and Kling 3.0 Turbo; premium is Seedance 2.0 at 720p.
var ClipRoutes = []Route{
	// Owner, 2026-11-11 (paid bake-off): Hailuo 2.3 Fast on fal is the default, USD 0.19 per
	// 6 s (USD 0.32 per 10 s); it makes 6 or 10 s, cut to the asked length.
	{Key: "fal/hailuo-2.3-fast", Provider: "fal", Capability: CapImageToVideo, PriceUSD: 0.19, BaseSeconds: 6, MaxSeconds: 10, Quality: 3.5, PAYG: true, Timeout: 6 * time.Minute},
	// USD 0.28 per 6 s clip; it makes 6 or 10 s, cut to the asked length.
	{Key: "higgsfield/hailuo-2.3-std", Provider: "higgsfield", Capability: CapImageToVideo, PriceUSD: 0.28, BaseSeconds: 6, MaxSeconds: 10, Quality: 3.5, PAYG: true, Timeout: 6 * time.Minute},
	// USD 0.476 a clip, billed the same for 5 and 7 s.
	{Key: "higgsfield/kling-v3-std", Provider: "higgsfield", Capability: CapImageToVideo, PriceUSD: 0.476, BaseSeconds: 7, Quality: 4, PAYG: true, Timeout: 6 * time.Minute},
	// USD 0.56 for 5 s at 720p (/estimate).
	{Key: "higgsfield/kling-v3-turbo", Provider: "higgsfield", Capability: CapImageToVideo, PriceUSD: 0.56, BaseSeconds: 5, Quality: 4, PAYG: true, Timeout: 5 * time.Minute},
	{Key: "fal/grok-imagine-video", Provider: "fal", Capability: CapImageToVideo, PriceUSD: 0.05, PerSecond: true, Quality: 3, PAYG: true, Timeout: 6 * time.Minute, Fallback: true},
	{Key: "fal/kling-v3-pro", Provider: "fal", Capability: CapImageToVideo, PriceUSD: 0.14, PerSecond: true, Quality: 4, PAYG: true, Timeout: 6 * time.Minute, Fallback: true, NegativePrompt: true},
	// Token metered: 1280x720 x 24 fps / 1024 tokens a second at USD 0.014 per 1,000, about USD 0.30/s.
	{Key: "higgsfield/seedance-2.0", Provider: "higgsfield", Capability: CapImageToVideo, PriceUSD: 0.3024, PerSecond: true, Quality: 5, PAYG: true, Timeout: 8 * time.Minute, MinTier: TierPremium},
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
	// AvoidText puts routes that take a negative prompt first: the scene shows screens or signs,
	// which image-to-video models fill with invented writing.
	AvoidText bool
	// Scenario names the job for the routing policy: ScenarioAdClip for a video ad's scenes, else a
	// motion poster.
	Scenario string
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
	// estimate quotes Higgsfield routes (cached, short timeout).
	estimate *Estimator
}

// NewClips builds the clip pipeline; ffmpeg is looked up on PATH once.
func NewClips(keys KeySource, store VideoStore, posters *Posters, logger *zap.Logger) *Clips {
	if logger == nil {
		logger = zap.NewNop()
	}
	path, _ := exec.LookPath("ffmpeg")
	return &Clips{router: NewRouter(ClipRoutes), keys: keys, store: store, queue: newQueueClient(),
		fetch: fetchClient(3 * time.Minute), ffmpeg: path, logger: logger.Named("media.clips"), posts: posters, estimate: NewEstimator()}
}

// Ready reports whether clips can be made here (ffmpeg and storage present).
func (c *Clips) Ready() bool { return c != nil && c.ffmpeg != "" && c.store != nil }

// Make animates the scene on the best route with fallbacks, lays the exact
// words over it and stores the clip. ctx should allow several minutes.
func (c *Clips) Make(ctx context.Context, req ClipRequest) (*MediaResult, error) {
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

	raw := filepath.Join(dir, "raw.mp4")
	src, rt, attempts, err := c.Animate(ctx, req, raw)
	if err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "clip.mp4")
	if err := c.overlay(ctx, raw, overlayPath, out, size.X, size.Y, req.Seconds); err != nil {
		return nil, fmt.Errorf("media: text overlay: %w", err)
	}
	sum, err := fileSHA256(out)
	if err != nil {
		return nil, fmt.Errorf("media: hash clip: %w", err)
	}
	key, url, n, err := c.store.PutClip(ctx, req.TenantID, req.VersionID, out)
	if err != nil {
		return nil, fmt.Errorf("media: store clip: %w", err)
	}
	secs := float64(req.Seconds)
	return &MediaResult{URL: url, Key: key, Bytes: n, Seconds: secs, Width: size.X, Height: size.Y, SHA256: sum,
		Route: rt.Key, CostUSD: rt.Cost(secs), SourceURL: src, Attempts: attempts}, nil
}

// Animate animates req.SceneURL on the best route with fallbacks and saves the clip to
// path, without any text: the motion poster lays its words on top, a video ad uses it as
// a scene. It returns the provider's link, the route and how many routes were tried.
func (c *Clips) Animate(ctx context.Context, req ClipRequest, path string) (string, Route, int, error) {
	if req.Tier == "" {
		req.Tier = TierStandard
	}
	secs := float64(req.Seconds)
	routes := c.order(ctx, req.scenario(), req.Tier, secs, req.AvoidText)
	var lastErr error
	for i, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		src, err := c.animate(actx, rt, req)
		if err == nil {
			err = c.save(actx, src, path)
		}
		cancel()
		if err != nil {
			lastErr = err
			c.router.Failed(rt.Key, err, ctx.Err() != nil)
			c.logger.Warn("clip route failed, trying the next", zap.String("route", rt.Key), zap.Error(err))
			continue
		}
		c.router.Succeeded(rt.Key)
		return src, rt, i + 1, nil
	}
	if lastErr == nil {
		return "", Route{}, 0, ErrNoRoute
	}
	return "", Route{}, 0, fmt.Errorf("%w: %w", ErrNoRoute, lastErr)
}

type videoOut struct {
	Video struct {
		URL string `json:"url"`
	} `json:"video"`
}

// animate runs one route and returns the provider's clip URL.
func (c *Clips) animate(ctx context.Context, rt Route, req ClipRequest) (string, error) {
	endpoint, input, err := clipInput(rt, req)
	if err != nil {
		return "", err
	}
	key := c.keys(ctx, rt.Provider)
	job := higgsfieldJob(key, endpoint)
	if rt.Provider == "fal" {
		job = falJob(key, endpoint)
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

// clipInput is each route's endpoint and request body. Higgsfield shapes are from its
// OpenAPI (Hailuo 2.3, Kling turbo), its CLI schemas and /estimate validation; fal shapes
// from its published schemas.
func clipInput(rt Route, req ClipRequest) (string, map[string]any, error) {
	prompt := clipPrompt(req.Prompt)
	switch rt.Key {
	case "fal/hailuo-2.3-fast":
		d := "6"
		if req.Seconds > 6 {
			d = "10"
		}
		return "fal-ai/minimax/hailuo-2.3-fast/standard/image-to-video", map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": d, "prompt_optimizer": false}, nil
	case "higgsfield/hailuo-2.3-std":
		// Hailuo makes 6 or 10 s; the overlay step cuts the clip to the asked length.
		d := 6
		if req.Seconds > 6 {
			d = 10
		}
		return "minimax/hailuo-2.3/standard/image-to-video", map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": d, "prompt_optimizer": false}, nil
	case "higgsfield/kling-v3-turbo":
		return "kling-video/v3.0-turbo/image-to-video", map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "resolution": "720p"}, nil
	case "higgsfield/kling-v3-std":
		return "kling-video/v3.0/std/image-to-video", map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "sound": "off"}, nil
	case "higgsfield/seedance-2.0":
		return "bytedance/seedance-2.0/image-to-video", map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "resolution": "720p", "generate_audio": false}, nil
	case "fal/grok-imagine-video":
		return "xai/grok-imagine-video/image-to-video", map[string]any{"image_url": req.SceneURL, "prompt": prompt, "duration": req.Seconds, "resolution": "720p"}, nil
	case "fal/kling-v3-pro":
		return "fal-ai/kling-video/v3/pro/image-to-video", map[string]any{"start_image_url": req.SceneURL, "prompt": prompt, "duration": strconv.Itoa(req.Seconds),
			"generate_audio": false, "negative_prompt": clipNegativePrompt}, nil
	}
	return "", nil, fmt.Errorf("media: no builder for route %s", rt.Key)
}

// quoteImage stands in for the scene in an estimate request; /estimate prices the shape
// and never fetches it.
const quoteImage = "https://example.com/scene.png"

// Quote is the provider cost of a clip on the route a render would use first: Higgsfield's
// /estimate when it answers in time, the route's price otherwise. It never stalls a quote.
func (c *Clips) Quote(ctx context.Context, tier string, seconds int) (usd float64, route string, estimated bool) {
	return c.QuoteFor(ctx, tier, seconds, false)
}

// QuoteFor is Quote for a scene that may need a negative prompt (ClipRequest.AvoidText).
func (c *Clips) QuoteFor(ctx context.Context, tier string, seconds int, avoidText bool) (usd float64, route string, estimated bool) {
	if tier == "" {
		tier = TierStandard
	}
	secs := float64(seconds)
	routes := c.order(ctx, ScenarioAdClip, tier, secs, avoidText)
	if len(routes) == 0 {
		return 0, "", false
	}
	rt := routes[0]
	if rt.Provider != "higgsfield" {
		return rt.Cost(secs), rt.Key, false
	}
	endpoint, input, err := clipInput(rt, ClipRequest{SceneURL: quoteImage, Seconds: seconds})
	if err != nil {
		return rt.Cost(secs), rt.Key, false
	}
	usd, estimated = c.estimate.USD(ctx, c.keys(ctx, rt.Provider), endpoint, input, rt.Cost(secs))
	return usd, rt.Key, estimated
}

// order is the routes to try for a clip, best first. With avoidText the routes that take a
// negative prompt come first (still in score order among themselves), whatever their price.
func (c *Clips) order(ctx context.Context, scenario, tier string, secs float64, avoidText bool) []Route {
	ok := func(rt Route) bool { return usable(ctx, c.keys, rt, rt.Cost(secs)) }
	routes := c.router.OrderPreferred(CapImageToVideo, tier, secs, ok, prefer(ctx, scenario, tier))
	if len(routes) == 0 && tier == TierPremium {
		// No premium clip route can take it (offline, unpaid or low): the standard ones do.
		routes = c.router.OrderPreferred(CapImageToVideo, TierStandard, secs, ok, prefer(ctx, scenario, TierStandard))
	}
	if !avoidText {
		return routes
	}
	out := make([]Route, 0, len(routes))
	for _, rt := range routes {
		if rt.NegativePrompt {
			out = append(out, rt)
		}
	}
	for _, rt := range routes {
		if !rt.NegativePrompt {
			out = append(out, rt)
		}
	}
	return out
}

// clipRoute is the registry's clip route with this key.
func clipRoute(key string) (Route, bool) {
	for _, rt := range ClipRoutes {
		if rt.Key == key {
			return rt, true
		}
	}
	return Route{}, false
}

// clipNegativePrompt is sent to every clip route that takes one.
const clipNegativePrompt = "text, letters, words, subtitles, captions, signs, signage, writing on screens, logos, watermark, blur, distortion, low quality"

// writingWords are things in a scene that carry writing: screens, signs and printed matter.
var writingWords = []string{"screen", "monitor", "laptop", "computer", "tablet", "phone", "smartphone", "dashboard",
	"display", "sign", "signage", "signboard", "billboard", "banner", "poster", "menu", "receipt", "invoice", "label",
	"whiteboard", "blackboard", "chalkboard", "book", "newspaper", "storefront", "shopfront", "till", "pos", "keyboard", "code"}

// showsWriting reports whether a scene's visual names something that carries writing.
func showsWriting(visual string) bool {
	words := " " + strings.Join(strings.FieldsFunc(strings.ToLower(visual), func(r rune) bool {
		return !(r >= 'a' && r <= 'z')
	}), " ") + " "
	for _, w := range writingWords {
		if strings.Contains(words, " "+w+" ") || strings.Contains(words, " "+w+"s ") {
			return true
		}
	}
	return false
}

// silentPeople is the rule for people in every picture and clip that is not a lip-synced presenter.
const silentPeople = "Anyone in view is busy with what they are doing, looking at their work or each other, mouth closed, not talking and not looking at the camera."

// clipPrompt keeps the motion gentle and the frame free of text: our words go on top.
func clipPrompt(p string) string {
	if p == "" {
		p = "Slow, smooth camera push-in; subtle natural motion in the scene"
	}
	// Image-to-video models invent subtitles and signage even when told "no text" (2026-11-19
	// audit: garbled captions in Hailuo clips), so the ban names each kind of writing.
	// A clip is never lip-synced: a face talking or smiling at the camera under the voice-over looks
	// like a broken presenter (owner, 2026-11-24), so people in it keep to what they are doing.
	return p + ". " + silentPeople + " No text, no letters, no subtitles, no captions, no signs, no screen writing, no logos added. Keep the product unchanged."
}

// save downloads the provider's clip to path (bounded).
func (c *Clips) save(ctx context.Context, url, path string) error {
	return downloadTo(ctx, c.fetch, url, path, maxClipBytes, "clip")
}

// downloadTo streams url into path, refusing more than limit bytes; what names
// the file in errors.
func downloadTo(ctx context.Context, hc *http.Client, url, path string, limit int64, what string) error {
	if err := guardrail.CheckFetchURLWith(ctx, url, allowPrivateFetch); err != nil {
		return fmt.Errorf("download %s: %w", what, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", what, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", what, resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", what, err)
	}
	if n > limit {
		return fmt.Errorf("download %s: larger than %d MB", what, limit>>20)
	}
	// ffmpeg reads these files. A playlist or concat list could point it at local files or
	// other hosts, so a download that is one is refused.
	if playlistFile(path) {
		_ = os.Remove(path)
		return fmt.Errorf("download %s: a playlist is not a media file", what)
	}
	return nil
}

// fetchClient is the client for downloading media from URLs that came from data: public
// addresses only (guardrail.SafeHTTPClient). Tests serving files from 127.0.0.1 set
// allowPrivateFetch.
func fetchClient(timeout time.Duration) *http.Client {
	return guardrail.SafeHTTPClient(timeout, allowPrivateFetch)
}

var allowPrivateFetch = false

// playlistFile reports whether a downloaded file is a text playlist (HLS, ffconcat, SDP) rather
// than media.
func playlistFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	s := strings.TrimSpace(strings.TrimPrefix(string(head[:n]), string(rune(0xfeff))))
	for _, p := range []string{"#EXTM3U", "#EXT-X-", "ffconcat", "file '", "v=0"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
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

// fileSHA256 hashes a file without loading it whole.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scenario is the request's routing scenario (a motion poster unless set).
func (r ClipRequest) scenario() string {
	if r.Scenario == "" {
		return ScenarioMotion
	}
	return r.Scenario
}
