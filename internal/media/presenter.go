package media

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CapPresenter makes a talking presenter: an approved avatar image lip-synced to
// a voice-over (the UGC-style scene).
const CapPresenter Capability = "presenter"

// PresenterRoutes price per second of video (docs/integrations/video-generation-providers.md
// section 4; shapes from fal's published schemas, read 2026-10-24). VEED Fabric
// is the default; Creatify Aurora is the fallback, at 720p only for premium.
var PresenterRoutes = []Route{
	{Key: "fal/veed-fabric", Provider: "fal", Capability: CapPresenter, PriceUSD: 0.08, Quality: 4, PAYG: true, Timeout: 10 * time.Minute},
	{Key: "fal/creatify-aurora-480p", Provider: "fal", Capability: CapPresenter, PriceUSD: 0.07, Quality: 3.5, PAYG: true, Timeout: 10 * time.Minute, Fallback: true},
	{Key: "fal/creatify-aurora-720p", Provider: "fal", Capability: CapPresenter, PriceUSD: 0.14, Quality: 4.2, PAYG: true, Timeout: 10 * time.Minute, MinTier: TierPremium, Fallback: true},
}

// MaxPresenterSeconds bounds one presenter scene (a UGC scene is 5 to 15 s).
const MaxPresenterSeconds = 30

// PresenterRequest is one presenter scene.
type PresenterRequest struct {
	TenantID  uuid.UUID
	VersionID string
	ImageURL  string  // the approved avatar image
	AudioURL  string  // the scene's voice-over (Voices.Make)
	Seconds   float64 // the voice-over's length, for the price
	Tier      string
}

// PresenterResult is the provider's clip; the assembler downloads it.
type PresenterResult struct {
	URL      string  `json:"url"`
	Route    string  `json:"route"`
	CostUSD  float64 `json:"cost_usd"`
	Attempts int     `json:"attempts"`
}

// Presenters makes presenter scenes on the best route with fallbacks.
type Presenters struct {
	router *Router
	keys   KeySource
	queue  *queueClient
	logger *zap.Logger
}

// NewPresenters builds the presenter pipeline.
func NewPresenters(keys KeySource, logger *zap.Logger) *Presenters {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Presenters{router: NewRouter(PresenterRoutes), keys: keys, queue: newQueueClient(), logger: logger.Named("media.presenters")}
}

// Make lip-syncs the image to the audio. ctx should allow several minutes.
func (p *Presenters) Make(ctx context.Context, req PresenterRequest) (*PresenterResult, error) {
	for _, u := range []string{req.ImageURL, req.AudioURL} {
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return nil, fmt.Errorf("media: the presenter needs http(s) image and audio links")
		}
	}
	if req.Seconds <= 0 || req.Seconds > MaxPresenterSeconds {
		return nil, fmt.Errorf("media: a presenter scene is up to %d seconds", MaxPresenterSeconds)
	}
	if req.Tier == "" {
		req.Tier = TierStandard
	}
	routes := p.router.Order(CapPresenter, req.Tier, func(rt Route) bool { return p.keys(ctx, rt.Provider) != "" })
	var lastErr error
	for i, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		url, err := p.run(actx, rt, req)
		cancel()
		if err != nil {
			lastErr = err
			p.router.Failed(rt.Key, err, ctx.Err() != nil)
			p.logger.Warn("presenter route failed, trying the next", zap.String("route", rt.Key), zap.Error(err))
			continue
		}
		p.router.Succeeded(rt.Key)
		// Both providers bill whole seconds.
		return &PresenterResult{URL: url, Route: rt.Key, CostUSD: rt.PriceUSD * math.Ceil(req.Seconds), Attempts: i + 1}, nil
	}
	if lastErr == nil {
		return nil, ErrNoRoute
	}
	return nil, fmt.Errorf("%w: %v", ErrNoRoute, lastErr)
}

func (p *Presenters) run(ctx context.Context, rt Route, req PresenterRequest) (string, error) {
	key := p.keys(ctx, rt.Provider)
	var (
		endpoint string
		input    map[string]any
	)
	switch rt.Key {
	case "fal/veed-fabric":
		endpoint = "veed/fabric-1.0"
		input = map[string]any{"image_url": req.ImageURL, "audio_url": req.AudioURL, "resolution": "720p"}
	case "fal/creatify-aurora-480p", "fal/creatify-aurora-720p":
		res := "480p"
		if rt.Key == "fal/creatify-aurora-720p" {
			res = "720p"
		}
		endpoint = "fal-ai/creatify/aurora"
		input = map[string]any{"image_url": req.ImageURL, "audio_url": req.AudioURL, "resolution": res}
	default:
		return "", fmt.Errorf("media: no builder for route %s", rt.Key)
	}
	var out videoOut
	if err := p.queue.run(ctx, falJob(key, endpoint), input, &out); err != nil {
		return "", err
	}
	if out.Video.URL == "" {
		return "", &errJobFailed{provider: rt.Provider, status: "completed", detail: "no video in the result"}
	}
	return out.Video.URL, nil
}
