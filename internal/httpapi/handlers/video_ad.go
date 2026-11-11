package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// Video ads for marketflow-api's creative module: the storyboard, the quote, the render
// and the refund of a job lost with its pod.

// adInput reads and checks an ad request, shared by the render and its quote. The render
// rules and the compliance filter run first, so a script that cannot be made costs nothing.
func (h *OSHandler) adInput(w http.ResponseWriter, r *http.Request) (adRequest, media.AdRequest, uuid.UUID, bool) {
	var req adRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return req, media.AdRequest{}, uuid.Nil, false
	}
	tenantID, ok := jobIDs(w, req.TenantID, req.VersionID)
	if !ok {
		return req, media.AdRequest{}, uuid.Nil, false
	}
	accent, err := parseHexColor(req.Accent)
	if err != nil {
		writeError(w, http.StatusBadRequest, "accent must be a #rrggbb colour")
		return req, media.AdRequest{}, uuid.Nil, false
	}
	if req.Tier != media.TierPremium {
		req.Tier = media.TierStandard
	}
	if req.Kind == "" {
		req.Kind = media.AdTemplate
	}
	cache := make(map[string]media.SceneCache, len(req.SceneCache))
	for _, c := range req.SceneCache {
		if c.Hash != "" {
			cache[c.Hash] = c
		}
	}
	areq := media.AdRequest{TenantID: tenantID, VersionID: req.VersionID, Format: req.Format, Kind: req.Kind, Tier: req.Tier,
		Voice: req.Voice, Delivery: req.Delivery, Prefs: media.VoicePrefs{Gender: req.VoiceGender, Accent: req.VoiceAccent},
		Language: req.Language, Scenes: req.Scenes, Photos: req.PhotoURLs, Footage: req.FootageURLs,
		ActorURL: req.ActorURL, Seed: req.Seed, Stills: req.Stills, Cache: cache, LogoURL: req.LogoURL, MusicURL: req.MusicURL,
		EndCard: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Accent: accent}}
	if err := h.ads.Validate(areq); err != nil {
		writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "media: "))
		return req, areq, tenantID, false
	}
	var words strings.Builder
	for _, s := range req.Scenes {
		words.WriteString(s.Voiceover)
		words.WriteString("\n")
		words.WriteString(s.Caption)
		words.WriteString("\n")
	}
	words.WriteString(strings.Join([]string{req.Headline, req.Subline, req.Price, req.CTA}, "\n"))
	if _, blocked, reason := compliance.NewFilter().CheckForTenant(words.String(), h.businessType(r.Context(), tenantID)); blocked {
		writeError(w, http.StatusUnprocessableEntity, "the script did not pass the compliance check: "+reason)
		return req, areq, tenantID, false
	}
	// The business's own banned claims (brand context), enforced before anything is made.
	if claim := team.MatchBanned(words.String(), h.bannedClaims(r.Context(), tenantID)); claim != "" {
		writeError(w, http.StatusUnprocessableEntity, "the script makes a claim your brand context bans: \""+claim+"\". Edit the script and try again.")
		return req, areq, tenantID, false
	}
	return req, areq, tenantID, true
}

// adCredits prices an ad from its plan. A template ad is the registry's template price per
// 30 s block of words still to voice (at least one block). A presenter ad is priced from
// the provider cost of the scenes still to make (presenter and clip quotes, and the voice)
// with the registry's credit formula, so an unchanged scene is never paid for twice.
func adCredits(reg registry.Registry, plan media.AdPlan) (int, error) {
	if plan.Kind == media.AdTemplate {
		c, err := reg.Credits(registry.TemplateVideo)
		blocks := max(1, (plan.VoiceChars+adBlockChars-1)/adBlockChars)
		return c * blocks, err
	}
	return reg.CreditsForUSD(plan.CostUSD), nil
}

// adPlanBudget bounds the plan: cached estimates answer at once, a fresh one in 3 s.
const adPlanBudget = 20 * time.Second

// adPlan is the scene plan, or the error answer when no route can make a paid scene.
func (h *OSHandler) adPlan(w http.ResponseWriter, r *http.Request, areq media.AdRequest) (media.AdPlan, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), adPlanBudget)
	defer cancel()
	plan, err := h.ads.Plan(ctx, areq)
	if err != nil {
		h.logger.Warn("video ad plan failed", zap.String("version_id", areq.VersionID), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "no video provider can make this ad right now; nothing was charged")
		return plan, false
	}
	return plan, true
}

// Ad handles POST /api/v1/internal/creative/ad (service key): starts a video ad job, or
// reports the one already running or done for this version without charging again.
func (h *OSHandler) Ad(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() || !h.ads.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	req, areq, tenantID, ok := h.adInput(w, r)
	if !ok {
		return
	}
	// A job already running or done answers before any plan or charge.
	if prev, found, err := h.jobs.Get(r.Context(), req.VersionID); err == nil && found && prev.Status != media.JobFailed {
		status := http.StatusOK
		if prev.Status == media.JobRunning {
			status = http.StatusAccepted
		}
		writeJSON(w, status, prev)
		return
	}
	plan, ok := h.adPlan(w, r, areq)
	if !ok {
		return
	}
	price := func(reg registry.Registry) (int, error) { return adCredits(reg, plan) }
	h.startJob(w, r, tenantID, req.VersionID, req.Charge, price, "Video ad",
		func(ctx context.Context) (*media.MediaResult, error) { return h.ads.Make(ctx, areq) })
}

// adQuote is what a video ad will cost, scene by scene, against the tenant's credits.
type adQuote struct {
	creativeQuote
	Plan media.AdPlan `json:"plan"`
}

// AdQuote handles POST /api/v1/internal/creative/ad/quote (service key) with the render's
// body: the credits the render would charge now (the scene cache counted), the balance
// and, when short, the smallest covering bundle. Nothing is made or charged.
func (h *OSHandler) AdQuote(w http.ResponseWriter, r *http.Request) {
	if !h.ads.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	_, areq, tenantID, ok := h.adInput(w, r)
	if !ok {
		return
	}
	plan, ok := h.adPlan(w, r, areq)
	if !ok {
		return
	}
	reg := h.registry(r.Context())
	credits, err := adCredits(reg, plan)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "video price is not configured")
		return
	}
	q := adQuote{creativeQuote: creativeQuote{Tier: areq.Tier, Count: 1, CreditsEach: credits, CreditsNeeded: credits, Balance: -1}, Plan: plan}
	if h.credits != nil {
		q.Balance = h.balance(r.Context(), tenantID)
	}
	q.Enough = q.Balance < 0 || q.Balance >= credits
	if !q.Enough {
		q.Shortfall = credits - q.Balance
		if b, ok := reg.SmallestBundle(q.Shortfall); ok {
			q.SuggestedBundle = &b
		}
	}
	writeJSON(w, http.StatusOK, q)
}

// storyboardRequest asks for a video ad's storyboard.
type storyboardRequest struct {
	TenantID    string              `json:"tenant_id"`
	VersionID   string              `json:"version_id"`
	Format      string              `json:"format"`
	Kind        string              `json:"kind"`
	Scenes      []media.ScriptScene `json:"scenes"`
	PhotoURLs   []string            `json:"photo_urls"`
	Actor       *media.Actor        `json:"actor"`        // the brief's actor, kept when set
	ActorPrompt string              `json:"actor_prompt"` // who the presenter is, for a new actor
	Charge      bool                `json:"charge"`
}

// Storyboard handles POST /api/v1/internal/creative/storyboard (service key): starts the
// storyboard job of a version (one still per scene, and a presenter ad's actor) at the
// registry's storyboard price, free when nothing needs generating. Its result is pushed
// like a render's, with kind "storyboard".
func (h *OSHandler) Storyboard(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() || !h.storyboards.Ready() {
		writeError(w, http.StatusServiceUnavailable, "storyboards are not available on this server yet")
		return
	}
	var req storyboardRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil {
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
	if len(req.Scenes) == 0 || len(req.Scenes) > media.MaxAdScenes || utf8.RuneCountInString(req.ActorPrompt) > 300 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("a storyboard has 1 to %d scenes; actor_prompt is up to 300 characters", media.MaxAdScenes))
		return
	}
	if req.Actor != nil && !strings.HasPrefix(req.Actor.URL, "https://") && !strings.HasPrefix(req.Actor.URL, "http://") {
		req.Actor = nil
	}
	sreq := media.StoryboardRequest{TenantID: tenantID, VersionID: req.VersionID, Format: req.Format, Kind: req.Kind,
		Scenes: req.Scenes, Photos: req.PhotoURLs, Actor: req.Actor, ActorPrompt: req.ActorPrompt}
	price := func(reg registry.Registry) (int, error) {
		if _, generated := h.storyboards.Plan(sreq); generated == 0 {
			return 0, nil // the owner's photos and the kept actor only
		}
		return reg.Credits(registry.Storyboard)
	}
	h.startJob(w, r, tenantID, media.StoryboardJobID(req.VersionID), req.Charge, price, "Storyboard",
		func(ctx context.Context) (*media.MediaResult, error) {
			sb, err := h.storyboards.Make(ctx, sreq)
			if err != nil {
				return nil, err
			}
			return &media.MediaResult{Storyboard: sb, CostUSD: sb.CostUSD}, nil
		})
}

// jobID is the job a status or refund call names: the version's render, or with
// ?kind=storyboard its storyboard.
func jobID(r *http.Request) string {
	id := chi.URLParam(r, "version_id")
	if r.URL.Query().Get("kind") == "storyboard" {
		return media.StoryboardJobID(id)
	}
	return id
}

// JobRefund handles POST /api/v1/internal/creative/jobs/{version_id}/refund (service key)
// with {"tenant_id"}: marketflow-api gave up on a job lost with its pod (out of attempts or
// past its deadline) and its charge goes back. A live, finished or already refunded job is
// not refunded; 409 means the job is still within its time.
func (h *OSHandler) JobRefund(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	id := jobID(r)
	vid, _ := media.JobVersion(id)
	if _, err := uuid.Parse(vid); err != nil {
		writeError(w, http.StatusBadRequest, "version_id must be a UUID")
		return
	}
	var body struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id is required")
		return
	}
	tenantID, err := uuid.Parse(body.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	refund := func(key string) error {
		if h.credits == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return h.credits.Refund(ctx, tenantID, key)
	}
	job, ok, err := h.jobs.RefundLost(r.Context(), id, refund)
	switch {
	case err != nil:
		h.logger.Error("lost job not refunded", zap.String("job", id), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "the refund did not go through; try again")
	case !ok && job.Status == "":
		writeError(w, http.StatusNotFound, "no video job for this version")
	case !ok:
		writeJSON(w, http.StatusConflict, job)
	default:
		writeJSON(w, http.StatusOK, job)
	}
}

// HiggsfieldWebhook handles POST /api/v1/webhooks/higgsfield: a finished Higgsfield job,
// signed with the Standard Webhooks scheme. The raw body is verified against the secrets
// saved on the media/higgsfield provider setting, then the result goes to the push box for
// whichever pod waits on it. A bad signature or body is a 400 (Higgsfield does not retry
// a 4xx); with no secret saved the answer is 503, so Higgsfield retries while the owner
// saves it, and polling covers the job meanwhile.
func HiggsfieldWebhook(secrets func(ctx context.Context) []string, box media.PushBox, logger *zap.Logger) http.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "body too large")
			return
		}
		list := secrets(r.Context())
		if len(list) == 0 || box == nil {
			writeError(w, http.StatusServiceUnavailable, "webhook signing is not configured")
			return
		}
		if err := media.VerifyStandardWebhook(list, r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp"),
			r.Header.Get("webhook-signature"), body, time.Now()); err != nil {
			logger.Warn("higgsfield webhook refused", zap.Error(err))
			writeError(w, http.StatusBadRequest, "invalid signature")
			return
		}
		d, err := media.ParseHiggsfieldDelivery(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		status, err := d.StatusBody()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := box.Put(r.Context(), d.RequestID, status); err != nil {
			logger.Warn("higgsfield webhook not stored; polling will pick the job up", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "try again")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
