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

// PresenterRoutes bill per second of video. The lowest tier runs fal VEED Fabric
// first (its docs price) with Higgsfield Seedance 2.5 reference-to-video at 480p as
// the fallback (about USD 0.21/s, /estimate 2026-11-03); premium runs Seedance 2.5
// at 720p (about USD 0.46/s). Seedance takes the actor still as image_urls and the
// scene's voice-over as audio_urls and lip-syncs to it. Creatify Aurora stays as a
// further fallback.
var PresenterRoutes = []Route{
	{Key: "fal/veed-fabric", Provider: "fal", Capability: CapPresenter, PriceUSD: 0.08, PerSecond: true, Quality: 4, PAYG: true, Timeout: 10 * time.Minute, MaxTier: TierStandard},
	{Key: "higgsfield/seedance-2.5-480p", Provider: "higgsfield", Capability: CapPresenter, PriceUSD: 0.2056, PerSecond: true, MaxSeconds: seedanceMaxSeconds, Quality: 4, PAYG: true, Timeout: 10 * time.Minute},
	{Key: "higgsfield/seedance-2.5-720p", Provider: "higgsfield", Capability: CapPresenter, PriceUSD: 0.4622, PerSecond: true, MaxSeconds: seedanceMaxSeconds, Quality: 4.8, PAYG: true, Timeout: 10 * time.Minute, MinTier: TierPremium},
	{Key: "fal/creatify-aurora-480p", Provider: "fal", Capability: CapPresenter, PriceUSD: 0.07, PerSecond: true, Quality: 3.5, PAYG: true, Timeout: 10 * time.Minute, Fallback: true},
	{Key: "fal/creatify-aurora-720p", Provider: "fal", Capability: CapPresenter, PriceUSD: 0.14, PerSecond: true, Quality: 4.2, PAYG: true, Timeout: 10 * time.Minute, MinTier: TierPremium, Fallback: true},
}

// Seedance 2.5 reference-to-video limits: at least 4 s (shorter scenes are billed as 4),
// and at most one scene of seedanceMaxSeconds per call.
const (
	seedanceMinSeconds = 4
	seedanceMaxSeconds = 15
)

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
	Prompt    string // how the presenter moves; "" for a plain talking-to-camera shot
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
	router   *Router
	keys     KeySource
	queue    *queueClient
	logger   *zap.Logger
	estimate *Estimator
}

// NewPresenters builds the presenter pipeline.
func NewPresenters(keys KeySource, logger *zap.Logger) *Presenters {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Presenters{router: NewRouter(PresenterRoutes), keys: keys, queue: newQueueClient(), logger: logger.Named("media.presenters"), estimate: NewEstimator()}
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
	routes := p.router.OrderFor(CapPresenter, req.Tier, req.Seconds, func(rt Route) bool { return usable(ctx, p.keys, rt, presenterCost(rt, req.Seconds)) })
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
		return &PresenterResult{URL: url, Route: rt.Key, CostUSD: presenterCost(rt, req.Seconds), Attempts: i + 1}, nil
	}
	if lastErr == nil {
		return nil, ErrNoRoute
	}
	return nil, fmt.Errorf("%w: %w", ErrNoRoute, lastErr)
}

// Quote is the provider cost of a presenter scene on the route a render would use first.
// Seedance 2.5 is token metered, so /estimate answers with a description and the route's
// measured per-second price is used; a priced answer is used when Higgsfield gives one.
func (p *Presenters) Quote(ctx context.Context, tier string, seconds float64) (usd float64, route string, estimated bool) {
	if tier == "" {
		tier = TierStandard
	}
	routes := p.router.OrderFor(CapPresenter, tier, seconds, func(rt Route) bool { return usable(ctx, p.keys, rt, presenterCost(rt, seconds)) })
	if len(routes) == 0 {
		return 0, "", false
	}
	rt := routes[0]
	price := presenterCost(rt, seconds)
	if rt.Provider != "higgsfield" {
		return price, rt.Key, false
	}
	res := "480p"
	if rt.Key == "higgsfield/seedance-2.5-720p" {
		res = "720p"
	}
	input := map[string]any{"image_urls": []string{quoteImage}, "audio_urls": []string{"https://example.com/voice.mp3"},
		"prompt": presenterPrompt(""), "duration": seedanceDuration(seconds), "resolution": res, "generate_audio": false}
	usd, estimated = p.estimate.USD(ctx, p.keys(ctx, rt.Provider), "bytedance/seedance-2.5/reference-to-video", input, price)
	return usd, rt.Key, estimated
}

// presenterCost is what a scene costs on a route: whole seconds, and never below
// Seedance's 4 s minimum on its routes.
func presenterCost(rt Route, seconds float64) float64 {
	if rt.Provider == "higgsfield" && seconds < seedanceMinSeconds {
		seconds = seedanceMinSeconds
	}
	return rt.Cost(seconds)
}

func (p *Presenters) run(ctx context.Context, rt Route, req PresenterRequest) (string, error) {
	key := p.keys(ctx, rt.Provider)
	var (
		job   queueJob
		input map[string]any
	)
	switch rt.Key {
	case "fal/veed-fabric":
		job = falJob(key, "veed/fabric-1.0")
		input = map[string]any{"image_url": req.ImageURL, "audio_url": req.AudioURL, "resolution": "720p"}
	case "fal/creatify-aurora-480p", "fal/creatify-aurora-720p":
		res := "480p"
		if rt.Key == "fal/creatify-aurora-720p" {
			res = "720p"
		}
		job = falJob(key, "fal-ai/creatify/aurora")
		input = map[string]any{"image_url": req.ImageURL, "audio_url": req.AudioURL, "resolution": res}
	case "higgsfield/seedance-2.5-480p", "higgsfield/seedance-2.5-720p":
		res := "480p"
		if rt.Key == "higgsfield/seedance-2.5-720p" {
			res = "720p"
		}
		job = higgsfieldJob(key, "bytedance/seedance-2.5/reference-to-video")
		input = map[string]any{
			"image_urls": []string{req.ImageURL}, "audio_urls": []string{req.AudioURL},
			"prompt": presenterPrompt(req.Prompt), "duration": seedanceDuration(req.Seconds),
			"resolution": res, "generate_audio": false,
		}
	default:
		return "", fmt.Errorf("media: no builder for route %s", rt.Key)
	}
	var out videoOut
	if err := p.queue.run(ctx, job, input, &out); err != nil {
		return "", err
	}
	if out.Video.URL == "" {
		return "", &errJobFailed{provider: rt.Provider, status: "completed", detail: "no video in the result"}
	}
	return out.Video.URL, nil
}

// seedanceDuration is the whole-second length Seedance is asked for (minimum 4).
func seedanceDuration(seconds float64) int {
	d := int(math.Ceil(seconds - 1e-9))
	if d < seedanceMinSeconds {
		d = seedanceMinSeconds
	}
	return d
}

// presenterPrompt keeps the person from the reference image talking to camera in
// time with the audio, with no text in the frame (our captions go on top).
func presenterPrompt(p string) string {
	base := "The person in the reference image speaks to the camera in time with the reference audio, natural expressions and small hand gestures, steady framing"
	if p = strings.TrimSpace(p); p != "" {
		base = p + ". " + base
	}
	return base + ". Same face, hair and clothes throughout. No text, no letters, no logos."
}
