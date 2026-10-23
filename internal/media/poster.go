package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // decoders for provider outputs and logos
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	_ "golang.org/x/image/webp"
)

// PosterRoutes are the poster scene routes, confirmed 2026-10-24
// (docs/integrations/video-generation-providers.md, routes 1 and 2). Quality
// scores are provisional until the golden-brief eval sets them.
var PosterRoutes = []Route{
	{Key: "fal/seedream-v5-lite", Provider: "fal", Capability: CapPosterImage, PriceUSD: 0.035, Quality: 4, PAYG: true, Timeout: 60 * time.Second},
	{Key: "fal/nano-banana-2", Provider: "fal", Capability: CapPosterImage, PriceUSD: 0.08, Quality: 4.5, PAYG: true, Timeout: 60 * time.Second},
	// Cross-provider fallback: a clean graphic model that takes the brand palette.
	// Its price is not published per model; the estimate is checked by the first paid render.
	{Key: "higgsfield/recraft-v4.1", Provider: "higgsfield", Capability: CapPosterImage, PriceUSD: 0.04, Quality: 3.5, PAYG: true, Timeout: 60 * time.Second},
	{Key: "fal/nano-banana-pro", Provider: "fal", Capability: CapPosterImage, PriceUSD: 0.15, Quality: 5, PAYG: true, Timeout: 90 * time.Second, MinTier: TierPremium},
}

// PosterRequest is one poster to make.
type PosterRequest struct {
	TenantID  uuid.UUID
	VersionID string   // the creative version; names the stored file
	Prompt    string   // the scene, in plain words (the copywriter's visual line)
	PhotoURLs []string // the owner's product photos (public URLs), optional
	Format    string   // a key of Formats
	Tier      string   // TierStandard or TierPremium
	Palette   [][3]int // brand colours, optional
	Overlay   Overlay
	LogoURL   string // optional; downloaded and drawn top left
}

// PosterResult is the stored poster.
type PosterResult struct {
	URL       string  `json:"url"`
	Key       string  `json:"key"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Bytes     int     `json:"bytes"`
	SHA256    string  `json:"sha256"`
	Route     string  `json:"route"`
	CostUSD   float64 `json:"cost_usd"`
	SourceURL string  `json:"source_url"` // the provider's scene, kept for re-composing text edits
	Attempts  int     `json:"attempts"`
}

// Store is where finished posters go: httpware/objectstore's Store satisfies it.
type Store interface {
	PutPoster(ctx context.Context, tenantID uuid.UUID, versionID string, jpeg []byte) (key, url string, err error)
}

// KeySource returns a provider's API key ("" when not configured).
type KeySource func(ctx context.Context, provider string) string

// Posters makes posters.
type Posters struct {
	router *Router
	keys   KeySource
	store  Store
	queue  *queueClient
	fetch  *http.Client
	logger *zap.Logger
	budget time.Duration
}

// NewPosters builds the poster pipeline.
func NewPosters(keys KeySource, store Store, logger *zap.Logger) *Posters {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Posters{router: NewRouter(PosterRoutes), keys: keys, store: store, queue: newQueueClient(),
		fetch: &http.Client{Timeout: 20 * time.Second}, logger: logger.Named("media.posters"), budget: 120 * time.Second}
}

// ErrNoRoute means no poster route could run (no keys, or every route failed).
var ErrNoRoute = errors.New("media: no poster route could make the image")

// maxDownload caps a downloaded scene or logo.
const maxDownload = 25 << 20

// Make generates the scene on the best route (falling back down the order),
// draws the exact overlay and stores the JPEG.
func (p *Posters) Make(ctx context.Context, req PosterRequest) (*PosterResult, error) {
	if _, ok := Formats[req.Format]; !ok {
		return nil, fmt.Errorf("media: unknown poster format %q", req.Format)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("media: a poster needs a scene prompt")
	}
	if req.Tier == "" {
		req.Tier = TierStandard
	}
	ctx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()

	if req.LogoURL != "" && req.Overlay.Logo == nil {
		if logo, err := p.download(ctx, req.LogoURL); err == nil {
			req.Overlay.Logo = logo
		} else {
			p.logger.Info("logo not drawn", zap.Error(err)) // a poster without the logo beats no poster
		}
	}

	routes := p.router.Order(CapPosterImage, req.Tier, func(rt Route) bool { return p.keys(ctx, rt.Provider) != "" })
	var lastErr error
	for i, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, acancel := context.WithTimeout(ctx, rt.Timeout)
		sceneURL, err := p.scene(actx, rt, req)
		var scene image.Image
		if err == nil {
			scene, err = p.download(actx, sceneURL)
		}
		acancel()
		if err != nil {
			lastErr = err
			p.router.Failed(rt.Key, err, ctx.Err() != nil)
			p.logger.Warn("poster route failed, trying the next", zap.String("route", rt.Key), zap.Error(err))
			continue
		}
		p.router.Succeeded(rt.Key)

		img, err := ComposePoster(scene, req.Format, req.Overlay)
		if err != nil {
			return nil, err
		}
		data, err := EncodeJPEG(img)
		if err != nil {
			return nil, err
		}
		key, url, err := p.store.PutPoster(ctx, req.TenantID, req.VersionID, data)
		if err != nil {
			return nil, fmt.Errorf("media: store poster: %w", err)
		}
		sum := sha256.Sum256(data)
		b := img.Bounds()
		return &PosterResult{URL: url, Key: key, Width: b.Dx(), Height: b.Dy(), Bytes: len(data),
			SHA256: hex.EncodeToString(sum[:]), Route: rt.Key, CostUSD: rt.PriceUSD, SourceURL: sceneURL, Attempts: i + 1}, nil
	}
	if lastErr == nil {
		return nil, ErrNoRoute
	}
	return nil, fmt.Errorf("%w: %v", ErrNoRoute, lastErr)
}

// scenePrompt keeps text out of the generated image: our overlay draws it.
func scenePrompt(prompt string) string {
	return strings.TrimSpace(prompt) + ". Advertising photo, no text, no letters, no words, no logos, no watermark. Keep the lower third calm and uncluttered for a text overlay."
}

// aspectFor maps a poster format to the providers' aspect-ratio values.
func aspectFor(format string) string {
	switch format {
	case "1080x1350":
		return "4:5"
	case "1080x1920":
		return "9:16"
	default:
		return "1:1"
	}
}

type imagesOut struct {
	Images []struct {
		URL string `json:"url"`
	} `json:"images"`
}

// scene runs one route and returns the generated image's URL.
func (p *Posters) scene(ctx context.Context, rt Route, req PosterRequest) (string, error) {
	key := p.keys(ctx, rt.Provider)
	prompt := scenePrompt(req.Prompt)
	var (
		job   queueJob
		input map[string]any
	)
	switch rt.Key {
	case "fal/seedream-v5-lite":
		// Seedream needs 2560x1440 to 4096x4096 pixels in total; the compositor scales down.
		input = map[string]any{"prompt": prompt, "image_size": seedreamSize(req.Format), "num_images": 1}
		endpoint := "fal-ai/bytedance/seedream/v5/lite/text-to-image"
		if len(req.PhotoURLs) > 0 {
			endpoint = "fal-ai/bytedance/seedream/v5/lite/edit"
			input["image_urls"] = firstN(req.PhotoURLs, 10)
		}
		job = falJob(key, endpoint)
	case "fal/nano-banana-2":
		input = map[string]any{"prompt": prompt, "aspect_ratio": aspectFor(req.Format), "resolution": "2K", "output_format": "jpeg", "num_images": 1}
		endpoint := "fal-ai/nano-banana-2"
		if len(req.PhotoURLs) > 0 {
			endpoint = "fal-ai/nano-banana-2/edit"
			input["image_urls"] = firstN(req.PhotoURLs, 10)
		}
		job = falJob(key, endpoint)
	case "fal/nano-banana-pro":
		input = map[string]any{"prompt": prompt, "aspect_ratio": aspectFor(req.Format), "resolution": "2K", "output_format": "jpeg", "num_images": 1}
		endpoint := "fal-ai/nano-banana-pro"
		if len(req.PhotoURLs) > 0 {
			endpoint = "fal-ai/nano-banana-pro/edit"
			input["image_urls"] = firstN(req.PhotoURLs, 10)
		}
		job = falJob(key, endpoint)
	case "higgsfield/recraft-v4.1":
		// Text to image only (no photo input); the brand palette steers the colours.
		input = map[string]any{"prompt": prompt, "aspect_ratio": aspectFor(req.Format), "output_format": "jpg"}
		if colors := paletteRGB(req.Palette); len(colors) > 0 {
			input["colors"] = colors
		}
		job = higgsfieldJob(key, "recraft/v4.1/text-to-image")
	default:
		return "", fmt.Errorf("media: no builder for route %s", rt.Key)
	}
	var out imagesOut
	if err := p.queue.run(ctx, job, input, &out); err != nil {
		return "", err
	}
	if len(out.Images) == 0 || out.Images[0].URL == "" {
		return "", &errJobFailed{provider: rt.Provider, status: "completed", detail: "no image in the result"}
	}
	return out.Images[0].URL, nil
}

func seedreamSize(format string) map[string]int {
	switch format {
	case "1080x1350":
		return map[string]int{"width": 2160, "height": 2700}
	case "1080x1920":
		return map[string]int{"width": 2160, "height": 3840}
	default:
		return map[string]int{"width": 2048, "height": 2048}
	}
}

func paletteRGB(p [][3]int) []map[string][]int {
	var out []map[string][]int
	for _, c := range p {
		if len(out) == 5 {
			break
		}
		out = append(out, map[string][]int{"rgb": {clamp(c[0]), clamp(c[1]), clamp(c[2])}})
	}
	return out
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// download fetches and decodes an image, capped at maxDownload.
func (p *Posters) download(ctx context.Context, url string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.fetch.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download image: status %d", resp.StatusCode)
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}
