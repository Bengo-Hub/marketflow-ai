package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/compliance"
	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// Video for marketflow-api's creative module (Sprint 15): motion posters and
// template video ads run as media jobs whose result is pushed back to
// marketflow-api; the script is written synchronously.

// SetVideo wires the media jobs and the pipelines that run in them; clips or
// ads may be nil when their tools are missing on this server.
func (h *OSHandler) SetVideo(jobs *media.Jobs, clips *media.Clips, ads *media.Ads) {
	h.jobs, h.clips, h.ads = jobs, clips, ads
}

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
// version without charging again. 202 while running, 200 when done.
func (h *OSHandler) Clip(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() || !h.clips.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	var req clipRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, ok := jobIDs(w, req.TenantID, req.VersionID)
	if !ok {
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
	creq := media.ClipRequest{
		TenantID: tenantID, VersionID: req.VersionID, SceneURL: req.SceneURL, Prompt: strings.TrimSpace(req.Prompt),
		Format: req.Format, Seconds: req.Seconds, Tier: req.Tier, LogoURL: req.LogoURL,
		Overlay: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Accent: accent},
	}
	price := func(reg registry.Registry) (int, error) { return reg.CreditsFor(output, req.Seconds) }
	h.startJob(w, r, tenantID, req.VersionID, req.Charge, price, "Motion poster",
		func(ctx context.Context) (*media.MediaResult, error) { return h.clips.Make(ctx, creq) })
}

// scriptRequest asks for a template video ad script.
type scriptRequest struct {
	TenantID  string `json:"tenant_id"`
	VersionID string `json:"version_id"` // names the charge: script:<version_id>
	Offering  string `json:"offering"`
	Facts     string `json:"facts"`
	Objective string `json:"objective"`
	Audience  string `json:"audience"`
	Language  string `json:"language"`
	Seconds   int    `json:"seconds"`
	Photos    int    `json:"photos"`
	Footage   int    `json:"footage"`
	Charge    bool   `json:"charge"`
}

// Script handles POST /api/v1/internal/creative/script (service key): the
// Content Creator writes one template video ad script and the review checks it.
// It costs the registry's script price once per version and is refunded when
// no usable script comes back. 422 carries the review's findings.
func (h *OSHandler) Script(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "the team is not available")
		return
	}
	var req scriptRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, ok := jobIDs(w, req.TenantID, req.VersionID)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Offering) == "" || utf8.RuneCountInString(req.Offering) > 300 || utf8.RuneCountInString(req.Facts) > 3000 {
		writeError(w, http.StatusBadRequest, "offering is required (max 300 characters) and facts are limited to 3000")
		return
	}
	if req.Photos < 0 || req.Footage < 0 || req.Photos+req.Footage == 0 {
		writeError(w, http.StatusBadRequest, "a video ad needs at least one product photo or clip")
		return
	}
	chargeKey, credits := "", 0
	if req.Charge && h.credits != nil {
		price, err := h.registry(r.Context()).Credits(registry.Script)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "script price is not configured")
			return
		}
		key := "script:" + req.VersionID
		dec, err := h.spend(r.Context(), tenantID, int64(price), key, "Video ad script")
		switch {
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, "could not check AI credits; nothing was written or charged")
			return
		case !dec.Allowed:
			writeError(w, http.StatusPaymentRequired, "not enough AI credits for this script")
			return
		case !dec.Exempt:
			chargeKey, credits = key, price
		}
	}
	res, err := h.runtime.WriteVideoScript(r.Context(), team.VideoScriptRequest{TenantID: tenantID, Offering: req.Offering,
		Facts: req.Facts, Objective: req.Objective, Audience: req.Audience, Language: req.Language,
		Seconds: req.Seconds, Photos: req.Photos, Footage: req.Footage})
	if err != nil {
		h.refundKey(tenantID, chargeKey)
		if errors.Is(err, team.ErrScriptBlocked) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "the script did not pass the review; nothing was charged", "findings": res.Findings})
			return
		}
		h.logger.Warn("video script failed", zap.String("version_id", req.VersionID), zap.Error(err))
		writeError(w, http.StatusBadGateway, "the script could not be written right now; nothing was charged")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"script": res.Script, "findings": res.Findings,
		"credits_charged": credits, "cost_usd": res.CostUSD})
}

// adRequest is marketflow-api asking for a template video ad from an approved script.
type adRequest struct {
	TenantID    string              `json:"tenant_id"`
	VersionID   string              `json:"version_id"`
	Format      string              `json:"format"`
	Tier        string              `json:"tier"`
	Voice       string              `json:"voice"`    // a voice id or name; "" picks the best for the delivery
	Delivery    string              `json:"delivery"` // ad (default) | ugc | kids | story | info
	VoiceGender string              `json:"voice_gender"`
	VoiceAccent string              `json:"voice_accent"` // e.g. african, kenyan, nigerian, british
	Language    string              `json:"language"`
	Scenes      []media.ScriptScene `json:"scenes"`
	PhotoURLs   []string            `json:"photo_urls"`
	FootageURLs []string            `json:"footage_urls"`
	Accent      string              `json:"accent"`
	LogoURL     string              `json:"logo_url"`
	Headline    string              `json:"headline"`
	Subline     string              `json:"subline"`
	Price       string              `json:"price"`
	CTA         string              `json:"cta"`
	Charge      bool                `json:"charge"`
}

// adBlockChars is about 30 seconds of speech: a longer voice-over costs a
// second block of the template video price.
const adBlockChars = 420

// Ad handles POST /api/v1/internal/creative/ad (service key): starts a template
// video ad job, or reports the one already running or done for this version
// without charging again. The script is checked first (render rules and the
// compliance filter, since the owner may have edited it), so a script that
// cannot be made costs nothing.
func (h *OSHandler) Ad(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() || !h.ads.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	var req adRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, ok := jobIDs(w, req.TenantID, req.VersionID)
	if !ok {
		return
	}
	accent, err := parseHexColor(req.Accent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "accent must be a #rrggbb colour")
		return
	}
	if req.Tier != media.TierPremium {
		req.Tier = media.TierStandard
	}
	areq := media.AdRequest{TenantID: tenantID, VersionID: req.VersionID, Format: req.Format, Tier: req.Tier,
		Voice: req.Voice, Delivery: req.Delivery, Prefs: media.VoicePrefs{Gender: req.VoiceGender, Accent: req.VoiceAccent},
		Language: req.Language, Scenes: req.Scenes, Photos: req.PhotoURLs, Footage: req.FootageURLs,
		LogoURL: req.LogoURL, EndCard: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Accent: accent}}
	if err := h.ads.Validate(areq); err != nil {
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "media: "))
		return
	}
	var words strings.Builder
	chars := 0
	for _, s := range req.Scenes {
		words.WriteString(s.Voiceover + "\n" + s.Caption + "\n")
		chars += utf8.RuneCountInString(strings.TrimSpace(s.Voiceover))
	}
	words.WriteString(strings.Join([]string{req.Headline, req.Subline, req.Price, req.CTA}, "\n"))
	if _, blocked, reason := compliance.NewFilter().CheckForTenant(words.String(), h.businessType(r.Context(), tenantID)); blocked {
		writeError(w, http.StatusUnprocessableEntity, "the script did not pass the compliance check: "+reason)
		return
	}
	blocks := 1
	if chars > adBlockChars {
		blocks = 2
	}
	price := func(reg registry.Registry) (int, error) {
		c, err := reg.Credits(registry.TemplateVideo)
		return c * blocks, err
	}
	h.startJob(w, r, tenantID, req.VersionID, req.Charge, price, "Video ad",
		func(ctx context.Context) (*media.MediaResult, error) { return h.ads.Make(ctx, areq) })
}

// CreativeVoices handles GET /api/v1/os/creative/voices: the voices a video ad
// can use (the account's, the owner's collection first, then the premade
// ones) and the deliveries (ad, ugc, kids, story, info) that pick and direct
// the voice when none is named. Any signed-in tenant may read it.
func (h *OSHandler) CreativeVoices(w http.ResponseWriter, r *http.Request) {
	if _, status, msg := tenantFromAuth(r); status != 0 {
		writeError(w, status, msg)
		return
	}
	if h.ads == nil || h.ads.Voices() == nil {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	deliveries := make([]media.Delivery, 0, len(media.Deliveries))
	for _, k := range []string{media.DeliveryAd, media.DeliveryUGC, media.DeliveryKids, media.DeliveryStory, media.DeliveryInfo} {
		deliveries = append(deliveries, media.Deliveries[k])
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": h.ads.Voices().List(r.Context()), "deliveries": deliveries})
}

// businessType reads the tenant's business type for the compliance rules; ""
// applies the generic rules.
func (h *OSHandler) businessType(ctx context.Context, tenantID uuid.UUID) string {
	if h.source == nil {
		return ""
	}
	tc, err := h.source.Load(ctx, tenantID)
	if err != nil {
		return ""
	}
	return tc.BusinessType
}

// JobStatus handles GET /api/v1/internal/creative/jobs/{version_id} (service
// key): the fallback read for a job whose pushed result did not arrive.
func (h *OSHandler) JobStatus(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	job, ok, err := h.jobs.Get(r.Context(), chi.URLParam(r, "version_id"))
	switch {
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now")
	case !ok:
		writeError(w, http.StatusNotFound, "no video job for this version")
	default:
		writeJSON(w, http.StatusOK, job)
	}
}

// startJob charges price once per attempt and starts work as the version's job,
// or reports the job already running or done without charging again. A busy pod
// refuses before any charge (429).
func (h *OSHandler) startJob(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, versionID string, charge bool,
	price func(registry.Registry) (int, error), label string, work func(ctx context.Context) (*media.MediaResult, error)) {
	prev, ok, err := h.jobs.Get(r.Context(), versionID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now")
		return
	}
	if ok && prev.Status == media.JobRunning {
		writeJSON(w, http.StatusAccepted, prev)
		return
	}
	if ok && prev.Status == media.JobDone {
		writeJSON(w, http.StatusOK, prev)
		return
	}
	release, err := h.jobs.Reserve()
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	attempt, key := media.NextCharge(versionID, prev, ok)
	chargeKey, credits := "", 0
	if charge && h.credits != nil {
		units, err := price(h.registry(r.Context()))
		if err != nil {
			release()
			writeError(w, http.StatusInternalServerError, "video price is not configured")
			return
		}
		dec, err := h.spend(r.Context(), tenantID, int64(units), key, label)
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
			chargeKey, credits = key, units
		}
	}
	// The job outlives this request, so its refund uses a detached context.
	refund := func(key string) { h.refundKey(tenantID, key) }
	if err := h.jobs.Start(versionID, credits, attempt, chargeKey, release, refund, work); err != nil {
		h.refundKey(tenantID, chargeKey)
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now; nothing was charged")
		return
	}
	writeJSON(w, http.StatusAccepted, media.Job{Status: media.JobRunning, CreditsCharged: credits, Attempt: attempt})
}

// jobIDs parses the tenant and version ids every video request carries.
func jobIDs(w http.ResponseWriter, tenant, version string) (uuid.UUID, bool) {
	tenantID, err := uuid.Parse(tenant)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return uuid.Nil, false
	}
	if _, err := uuid.Parse(version); err != nil {
		writeError(w, http.StatusBadRequest, "version_id must be a UUID")
		return uuid.Nil, false
	}
	return tenantID, true
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
