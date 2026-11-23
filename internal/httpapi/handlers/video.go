package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// Video for marketflow-api's creative module (Sprint 15): motion posters and
// template video ads run as media jobs whose result is pushed back to
// marketflow-api; the script is written synchronously.

// SetVideo wires the media jobs and the pipelines that run in them; clips, ads or
// storyboards may be nil when their tools are missing on this server.
func (h *OSHandler) SetVideo(jobs *media.Jobs, clips *media.Clips, ads *media.Ads, storyboards *media.Storyboards) {
	h.jobs, h.clips, h.ads, h.storyboards = jobs, clips, ads, storyboards
}

// clipRequest is marketflow-api's creative module asking for a motion poster.
type clipRequest struct {
	TenantID  string          `json:"tenant_id"`
	VersionID string          `json:"version_id"` // CreativeVersion id: names the file and the charge
	SceneURL  string          `json:"scene_url"`  // the source poster's stored scene without text
	Prompt    string          `json:"prompt"`     // the motion, optional
	Format    string          `json:"format"`
	Seconds   int             `json:"seconds"`
	Tier      string          `json:"tier"`
	Accent    string          `json:"accent"`
	LogoURL   string          `json:"logo_url"`
	Headline  string          `json:"headline"`
	Subline   string          `json:"subline"`
	Price     string          `json:"price"`
	CTA       string          `json:"cta"`
	Contacts  []media.Contact `json:"contacts"`
	Charge    bool            `json:"charge"`
	// LogoPlate is how the logo is backed (creative.logo_plate_* settings and the brand tint).
	LogoPlate media.PlateStyle `json:"logo_plate"`
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
		writeError(w, http.StatusBadRequest, "format must be "+media.FormatsText)
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
	// The platform owner's tenant gets the premium motion route by default (routing policy).
	req.Tier = h.premiumTier(r.Context(), tenantID, req.Tier)
	output := registry.MotionPoster
	if req.Tier == media.TierPremium {
		output = registry.MotionPosterPremium
	} else {
		req.Tier = media.TierStandard
	}
	creq := media.ClipRequest{
		TenantID: tenantID, VersionID: req.VersionID, SceneURL: req.SceneURL, Prompt: strings.TrimSpace(req.Prompt),
		Format: req.Format, Seconds: req.Seconds, Tier: req.Tier, LogoURL: req.LogoURL,
		Overlay: media.Overlay{Headline: req.Headline, Subline: req.Subline, Price: req.Price, CTA: req.CTA, Contacts: req.Contacts, Accent: accent, Plate: req.LogoPlate},
	}
	price := func(reg registry.Registry) (int, error) { return reg.CreditsFor(output, req.Seconds) }
	h.startJob(w, r, tenantID, req.VersionID, req.Charge, price, "Motion poster",
		func(ctx context.Context) (*media.MediaResult, error) { return h.clips.Make(ctx, creq) })
}

// scriptRequest asks for a template video ad script.
type scriptRequest struct {
	TenantID string `json:"tenant_id"`
	// Format is the ad's format; the writer is told its shape (landscape, square or vertical).
	Format    string `json:"format"`
	VersionID string `json:"version_id"` // names the charge: script:<version_id>
	Offering  string `json:"offering"`
	Facts     string `json:"facts"`
	Objective string `json:"objective"`
	Audience  string `json:"audience"`
	Language  string `json:"language"`
	Seconds   int    `json:"seconds"`
	Photos    int    `json:"photos"`
	Footage   int    `json:"footage"`
	// Kind is the video type: template (default), presenter or presenter_premium. A
	// presenter type lets the script use presenter scenes.
	Kind   string `json:"kind"`
	Charge bool   `json:"charge"`
	// BrandPhotos is how many of the Photos (the last ones) are the business's own brand
	// images rather than product photos.
	BrandPhotos int `json:"brand_photos"`
	// ActorNameStyle and ActorNames are the business's choice of names for on-screen people
	// (owner, 2026-11-12): a naming style, an exact list, or both.
	ActorNameStyle string   `json:"actor_name_style"`
	ActorNames     []string `json:"actor_names"`
	// Delivery is the voice delivery the owner chose; "ugc" writes a creator-style script,
	// which may be read by two voices taking turns (owner, 2026-11-14).
	Delivery string `json:"delivery"`
	// Approach is an ad template key (educate, day-in-the-life, myth-fact, ...); "" lets the
	// objective and audience choose.
	Approach string `json:"approach"`
	// Reviews are real reviews already published on the business's public page, each as
	// "quote | who said it"; the testimonial approach needs them.
	Reviews []string `json:"reviews"`
	// PresenterSpeaker is the gender of the actor already saved on the brief ("male",
	// "female", or "" when none): presenter lines follow it.
	PresenterSpeaker string `json:"presenter_speaker"`
	// Place is where the business is and how it looks (address, website pages, picture captions),
	// for the setting brief that places every scene (owner, 2026-11-20).
	Place team.PlaceFacts `json:"place"`
	// ActorPrompt is the presenter the owner described, so presenter visuals match the actor drawn.
	ActorPrompt string `json:"actor_prompt"`
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 96<<10)).Decode(&req); err != nil {
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
		Facts: req.Facts, Format: req.Format, Objective: req.Objective, Audience: req.Audience, Language: req.Language,
		Seconds: req.Seconds, Photos: req.Photos, Footage: req.Footage,
		Presenter:   req.Kind == media.AdPresenter || req.Kind == media.AdPresenterPremium,
		BrandPhotos: req.BrandPhotos, ActorNameStyle: req.ActorNameStyle, ActorNames: req.ActorNames,
		Delivery: req.Delivery, Approach: req.Approach, Reviews: req.Reviews, PresenterSpeaker: req.PresenterSpeaker,
		Place: req.Place, ActorPrompt: req.ActorPrompt})
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
	writeJSON(w, http.StatusOK, map[string]any{"script": res.Script, "findings": res.Findings, "setting_brief": res.SettingBrief,
		"credits_charged": credits, "cost_usd": res.CostUSD})
}

// adRequest is marketflow-api asking for a template video ad from an approved script.
type adRequest struct {
	// LogoEveryScene lays the logo small over every scene (creative.logo_every_scene); unset is on.
	LogoEveryScene *bool `json:"logo_every_scene"`
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
	// PhotoInfo describes photo_urls by index (offering photo or brand-library picture).
	PhotoInfo []media.PhotoInfo `json:"photo_info"`
	Accent    string            `json:"accent"`
	LogoURL   string            `json:"logo_url"`
	// MusicURL is the owner's licensed music bed (marketflow-api checks the confirmation).
	MusicURL string `json:"music_url"`
	// Mood is the ad's feel for generated music at the score level, e.g. the script's
	// strategy or tone ("warm, hopeful"); optional.
	Mood     string `json:"mood"`
	Headline string `json:"headline"`
	Subline  string `json:"subline"`
	Price    string `json:"price"`
	CTA      string `json:"cta"`
	// Contacts are the real ways to reach the business, drawn with icons on the end card.
	Contacts []media.Contact `json:"contacts"`
	reviewFacts
	Charge bool `json:"charge"`
	// Kind is the video type: template (default), presenter or presenter_premium.
	Kind string `json:"kind"`
	// ActorURL and Seed are the approved storyboard's actor (presenter types).
	ActorURL string `json:"actor_url"`
	Seed     int64  `json:"seed"`
	// Stills are the approved storyboard frames by scene id.
	Stills map[string]string `json:"stills"`
	// SceneCache is the scenes of earlier versions of this ad, reused when unchanged.
	SceneCache []media.SceneCache `json:"scene_cache"`
	// MusicCache is the generated music of the version this one was revised from.
	MusicCache *media.MusicCache `json:"music_cache,omitempty"`
	// AILabel is the owner's AI label choice (owner 2026-11-14): false leaves out the on-screen
	// "AI-generated" note on presenter ads. Missing (an older marketflow-api) keeps the note.
	AILabel *bool `json:"ai_label,omitempty"`
}

// noAILabel reports whether the owner turned the AI label off; a missing value keeps it on.
func (r adRequest) noAILabel() bool { return r.AILabel != nil && !*r.AILabel }

// adBlockChars is about 30 seconds of speech: a longer voice-over costs a
// second block of the template video price.
const adBlockChars = 420

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
		deliveries = append(deliveries, h.ads.Voices().Delivery(k)) // with the configured overrides
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": h.ads.Voices().List(r.Context()), "deliveries": deliveries})
}

// VoiceLibrary handles GET /api/v1/creative/voices/library (platform owner only): searches
// ElevenLabs' shared voice library with the platform key (query: search, gender, accent,
// language, use_case, page_size). A voice the owner likes is added to the account's collection
// in ElevenLabs; then the voices list picks it up.
func (h *OSHandler) VoiceLibrary(w http.ResponseWriter, r *http.Request) {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !claims.IsPlatformOwner {
		writeError(w, http.StatusForbidden, "only the platform owner can search the voice library")
		return
	}
	if h.ads == nil || h.ads.Voices() == nil {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	q := r.URL.Query()
	size, _ := strconv.Atoi(q.Get("page_size"))
	voices, err := h.ads.Voices().SearchLibrary(r.Context(), media.LibraryQuery{Search: q.Get("search"), Gender: q.Get("gender"),
		Accent: q.Get("accent"), Language: q.Get("language"), UseCase: q.Get("use_case"), PageSize: size})
	switch {
	case errors.Is(err, media.ErrNoVoiceKey):
		writeError(w, http.StatusServiceUnavailable, "save the ElevenLabs key on Platform > Providers first")
		return
	case err != nil:
		h.logger.Warn("voice library search failed", zap.Error(err))
		writeError(w, http.StatusBadGateway, "the voice library did not answer; try again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voices": voices})
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

// bannedClaims is the business's banned claims list (brand context), or none.
func (h *OSHandler) bannedClaims(ctx context.Context, tenantID uuid.UUID) []string {
	if h.source == nil {
		return nil
	}
	tc, err := h.source.Load(ctx, tenantID)
	if err != nil {
		return nil
	}
	return team.BannedClaims(tc.MarketingContext)
}

// JobStatus handles GET /api/v1/internal/creative/jobs/{version_id} (service
// key): the fallback read for a job whose pushed result did not arrive.
func (h *OSHandler) JobStatus(w http.ResponseWriter, r *http.Request) {
	if !h.jobs.Ready() {
		writeError(w, http.StatusServiceUnavailable, "video is not available on this server yet")
		return
	}
	job, ok, err := h.jobs.Get(r.Context(), jobID(r))
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
	price func(registry.Registry) (int, error), label string, work func(ctx context.Context) (*media.MediaResult, error), restartDone ...bool) {
	prev, ok, err := h.jobs.Get(r.Context(), versionID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "video jobs are unavailable right now")
		return
	}
	if ok && prev.Status == media.JobRunning {
		writeJSON(w, http.StatusAccepted, prev)
		return
	}
	again := ok && prev.Status == media.JobDone && len(restartDone) > 0 && restartDone[0]
	if ok && prev.Status == media.JobDone && !again {
		writeJSON(w, http.StatusOK, prev)
		return
	}
	release, err := h.jobs.Reserve()
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	attempt, key := media.NextCharge(versionID, prev, ok)
	if again {
		// A storyboard made again (a new script, a redrawn actor) is a new attempt with its own charge.
		attempt = max(prev.Attempt, 1) + 1
		key = fmt.Sprintf("render:%s:%d", versionID, attempt)
	}
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
