package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
)

// SetPosters wires the poster pipeline (Sprint 15, creative studio).
func (h *OSHandler) SetPosters(p *media.Posters) { h.posters = p }

// posterRequest is marketflow-api's creative module asking for one poster.
type posterRequest struct {
	TenantID  string   `json:"tenant_id"`
	VersionID string   `json:"version_id"` // CreativeVersion id: the charge key and the file name
	Prompt    string   `json:"prompt"`     // the visual line of the approved copy variant
	PhotoURLs []string `json:"photo_urls"` // the owner's product photos
	Format    string   `json:"format"`     // 1080x1080 | 1080x1350 | 1080x1920
	Tier      string   `json:"tier"`       // standard | premium
	Palette   [][3]int `json:"palette"`
	Accent    string   `json:"accent"` // brand colour, #rrggbb
	LogoURL   string   `json:"logo_url"`
	Headline  string   `json:"headline"`
	Subline   string   `json:"subline"`
	Price     string   `json:"price"`
	CTA       string   `json:"cta"`
	// Charge is false only when the caller already paid for this version.
	Charge bool `json:"charge"`
	// SceneURL asks for a text-only revision over this stored scene (the
	// scene_url of an earlier poster): no generation and never a charge.
	SceneURL string `json:"scene_url"`
}

// posterBudget sits under the internal route's 120 s request timeout.
const posterBudget = 110 * time.Second

// Poster handles POST /api/v1/internal/creative/poster (service key). It charges
// the poster price once per version (a retry of the same version is not charged
// again), makes the poster on the best route with fallbacks, and refunds when no
// route could make it.
func (h *OSHandler) Poster(w http.ResponseWriter, r *http.Request) {
	if h.posters == nil {
		writeError(w, http.StatusServiceUnavailable, "poster generation is not configured")
		return
	}
	var req posterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	if _, err := uuid.Parse(req.VersionID); err != nil {
		writeError(w, http.StatusBadRequest, "version_id must be a UUID")
		return
	}
	if _, ok := media.Formats[req.Format]; !ok {
		writeError(w, http.StatusBadRequest, "format must be 1080x1080, 1080x1350 or 1080x1920")
		return
	}
	if req.SceneURL != "" {
		h.recompose(w, r, tenantID, req)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" || len([]rune(req.Prompt)) > 2000 {
		writeError(w, http.StatusBadRequest, "prompt is required (max 2000 characters)")
		return
	}
	output := registry.Poster
	if req.Tier == media.TierPremium {
		output = registry.PosterPremium
	} else {
		req.Tier = media.TierStandard
	}
	accent, err := parseHexColor(req.Accent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "accent must be a #rrggbb colour")
		return
	}

	chargeKey := ""
	credits := 0
	if req.Charge && h.credits != nil {
		price, err := h.registry(r.Context()).Credits(output)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "poster price is not configured")
			return
		}
		dec, err := h.spend(r.Context(), tenantID, int64(price), "render:"+req.VersionID, "Poster")
		switch {
		case err != nil:
			h.logger.Warn("poster credit check failed", zap.String("version_id", req.VersionID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "could not check AI credits; nothing was made or charged")
			return
		case !dec.Allowed:
			writeError(w, http.StatusPaymentRequired, "not enough AI credits for this poster")
			return
		case !dec.Exempt:
			chargeKey, credits = "render:"+req.VersionID, price
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), posterBudget)
	defer cancel()
	res, err := h.posters.Make(ctx, media.PosterRequest{
		TenantID: tenantID, VersionID: req.VersionID, Prompt: req.Prompt, PhotoURLs: req.PhotoURLs,
		Format: req.Format, Tier: req.Tier, Palette: req.Palette, LogoURL: req.LogoURL,
		Overlay: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Accent: accent},
	})
	if err != nil {
		h.refund(r, tenantID, chargeKey)
		h.logger.Warn("poster failed", zap.String("version_id", req.VersionID), zap.Error(err))
		status := http.StatusBadGateway
		if errors.Is(err, media.ErrNoRoute) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, "the poster could not be made right now; nothing was charged")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"poster": res, "credits_charged": credits})
}

// SetClips wires the motion-poster jobs (Sprint 15, video).
func (h *OSHandler) SetClips(j *media.ClipJobs) { h.clips = j }

// clipRequest is marketflow-api's creative module asking for a motion poster.
type clipRequest struct {
	TenantID  string `json:"tenant_id"`
	VersionID string `json:"version_id"` // CreativeVersion id: names the file and the charge
	SceneURL  string `json:"scene_url"`  // the source poster's stored scene without text
	Prompt    string `json:"prompt"`     // the motion, optional
	Format    string `json:"format"`
	Seconds   int    `json:"seconds"`
	Tier      string `json:"tier"`
	Accent    string `json:"accent"`
	LogoURL   string `json:"logo_url"`
	Headline  string `json:"headline"`
	Subline   string `json:"subline"`
	Price     string `json:"price"`
	CTA       string `json:"cta"`
	Charge    bool   `json:"charge"`
}

// Clip handles POST /api/v1/internal/creative/clip (service key): starts a
// motion poster job, or reports the one already running or done for this
// version without charging again. 202 while running, 200 when done; poll
// GET /internal/creative/clip/{version_id}.
func (h *OSHandler) Clip(w http.ResponseWriter, r *http.Request) {
	if !h.clips.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	var req clipRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	if _, err := uuid.Parse(req.VersionID); err != nil {
		writeError(w, http.StatusBadRequest, "version_id must be a UUID")
		return
	}
	if _, ok := media.Formats[req.Format]; !ok {
		writeError(w, http.StatusBadRequest, "format must be 1080x1080, 1080x1350 or 1080x1920")
		return
	}
	if req.Seconds == 0 {
		req.Seconds = 5
	}
	if req.Seconds < media.MinClipSeconds || req.Seconds > media.MaxClipSeconds {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("seconds must be %d to %d", media.MinClipSeconds, media.MaxClipSeconds))
		return
	}
	if !strings.HasPrefix(req.SceneURL, "https://") && !strings.HasPrefix(req.SceneURL, "http://") {
		writeError(w, http.StatusBadRequest, "scene_url must be an http(s) link")
		return
	}
	if len([]rune(req.Prompt)) > 1000 {
		writeError(w, http.StatusBadRequest, "prompt is limited to 1000 characters")
		return
	}
	accent, err := parseHexColor(req.Accent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "accent must be a #rrggbb colour")
		return
	}
	output := registry.MotionPoster
	if req.Tier == media.TierPremium {
		output = registry.MotionPosterPremium
	} else {
		req.Tier = media.TierStandard
	}

	prev, ok, err := h.clips.Get(r.Context(), req.VersionID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now")
		return
	}
	if ok && prev.Status == media.ClipRunning {
		writeJSON(w, http.StatusAccepted, prev)
		return
	}
	if ok && prev.Status == media.ClipDone {
		writeJSON(w, http.StatusOK, prev)
		return
	}
	release, err := h.clips.Reserve()
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	attempt, key := media.NextCharge(req.VersionID, prev, ok)
	chargeKey, credits := "", 0
	if req.Charge && h.credits != nil {
		price, err := h.registry(r.Context()).CreditsFor(output, req.Seconds)
		if err != nil {
			release()
			writeError(w, http.StatusInternalServerError, "video price is not configured")
			return
		}
		dec, err := h.spend(r.Context(), tenantID, int64(price), key, "Motion poster")
		switch {
		case err != nil:
			release()
			writeError(w, http.StatusServiceUnavailable, "could not check AI credits; nothing was made or charged")
			return
		case !dec.Allowed:
			release()
			writeError(w, http.StatusPaymentRequired, "not enough AI credits for this video")
			return
		case !dec.Exempt:
			chargeKey, credits = key, price
		}
	}
	creq := media.ClipRequest{
		TenantID: tenantID, VersionID: req.VersionID, SceneURL: req.SceneURL, Prompt: strings.TrimSpace(req.Prompt),
		Format: req.Format, Seconds: req.Seconds, Tier: req.Tier, LogoURL: req.LogoURL,
		Overlay: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Accent: accent},
	}
	// The job outlives this request, so its refund uses a detached context.
	refund := func(key string) { h.refundKey(tenantID, key) }
	if err := h.clips.Start(req.VersionID, creq, credits, attempt, chargeKey, release, refund); err != nil {
		h.refundKey(tenantID, chargeKey)
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now; nothing was charged")
		return
	}
	writeJSON(w, http.StatusAccepted, media.ClipJob{Status: media.ClipRunning, CreditsCharged: credits, Attempt: attempt})
}

// ClipStatus handles GET /api/v1/internal/creative/clip/{version_id} (service key).
func (h *OSHandler) ClipStatus(w http.ResponseWriter, r *http.Request) {
	if h.clips == nil {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	job, ok, err := h.clips.Get(r.Context(), chi.URLParam(r, "version_id"))
	switch {
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now")
	case !ok:
		writeError(w, http.StatusNotFound, "no video job for this version")
	default:
		writeJSON(w, http.StatusOK, job)
	}
}

// refundKey returns a charge outside any request (a background job's failure).
func (h *OSHandler) refundKey(tenantID uuid.UUID, key string) {
	if key == "" || h.credits == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.credits.Refund(ctx, tenantID, key); err != nil {
		h.logger.Error("ai credit refund did not go through", zap.String("key", key), zap.Error(err))
	}
}

// creativeQuote is what a set of posters costs against the tenant's credits.
type creativeQuote struct {
	Tier            string           `json:"tier"`
	Count           int              `json:"count"`
	CreditsEach     int              `json:"credits_each"`
	CreditsNeeded   int              `json:"credits_needed"`
	Balance         int              `json:"balance"` // -1: unlimited or unknown
	Enough          bool             `json:"enough"`
	Shortfall       int              `json:"shortfall"`
	SuggestedBundle *registry.Bundle `json:"suggested_bundle,omitempty"`
}

// CreativeQuote handles GET /api/v1/os/creative/quote?tier=standard|premium&count=N:
// credits for N posters, the balance and, when short, the smallest bundle that
// covers the gap (the pre-buy rule: clients without a plan buy a bundle first).
// Text-only revisions are free and never need a quote.
func (h *OSHandler) CreativeQuote(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	q := creativeQuote{Tier: media.TierStandard, Count: 1}
	if r.URL.Query().Get("tier") == media.TierPremium {
		q.Tier = media.TierPremium
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil {
		q.Count = n
	}
	if q.Count < 1 || q.Count > 20 {
		writeError(w, http.StatusBadRequest, "count must be 1 to 20")
		return
	}
	output := registry.Poster
	if q.Tier == media.TierPremium {
		output = registry.PosterPremium
	}
	reg := h.registry(r.Context())
	each, err := reg.Credits(output)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "poster price is not configured")
		return
	}
	q.CreditsEach, q.CreditsNeeded = each, each*q.Count
	q.Balance = -1
	if h.credits != nil {
		q.Balance = h.balance(r.Context(), tenantID)
	}
	q.Enough = q.Balance < 0 || q.Balance >= q.CreditsNeeded
	if !q.Enough {
		q.Shortfall = q.CreditsNeeded - q.Balance
		if b, ok := reg.SmallestBundle(q.Shortfall); ok {
			q.SuggestedBundle = &b
		}
	}
	writeJSON(w, http.StatusOK, q)
}

// recompose is the text-only revision: the stored scene with new words, free.
// 410 when the scene was purged with its draft (the caller then asks for a new render).
func (h *OSHandler) recompose(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, req posterRequest) {
	accent, err := parseHexColor(req.Accent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "accent must be a #rrggbb colour")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), posterBudget)
	defer cancel()
	res, err := h.posters.Recompose(ctx, media.PosterRequest{
		TenantID: tenantID, VersionID: req.VersionID, Format: req.Format, LogoURL: req.LogoURL,
		Overlay: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Accent: accent},
	}, req.SceneURL)
	if err != nil {
		h.logger.Warn("recompose failed", zap.String("version_id", req.VersionID), zap.Error(err))
		if errors.Is(err, media.ErrSceneGone) {
			writeError(w, http.StatusGone, "the original image is no longer stored; make a new poster")
			return
		}
		writeError(w, http.StatusBadGateway, "the poster could not be updated right now")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"poster": res, "credits_charged": 0})
}

// parseHexColor reads #rrggbb; empty means the default accent.
func parseHexColor(s string) (color.RGBA, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if s == "" {
		return color.RGBA{}, nil
	}
	if len(s) != 6 {
		return color.RGBA{}, errors.New("bad colour")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{}, err
	}
	return color.RGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}, nil
}
