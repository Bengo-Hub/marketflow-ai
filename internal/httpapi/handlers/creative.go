package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"net/http"
	"strconv"
	"strings"
	"time"

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
