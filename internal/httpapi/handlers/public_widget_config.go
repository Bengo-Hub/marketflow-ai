package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// WidgetConfig handles GET /api/v1/public/widget-config?tenant={slug}: what the Vera widget may
// change about itself for a tenant, read from the tenant's public profile in marketflow-api.
// Today that is white_label (the Business plan's feature, owner 2026-11-12): the widget hides
// "Powered by Codevertex" the same way the /p, /f and /book pages do. marketflow-api decides it
// fail closed and caches the profile, so this only reads it; any failure answers
// white_label false and the badge stays.
//
// The slug is a query parameter, not X-Tenant-ID, so the browser sends a simple GET without a
// preflight and may cache the answer for a few minutes.
func (h *PublicFormsHandler) WidgetConfig(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tenant")))
	if !widgetSlugRe.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "tenant must be the tenant slug")
		return
	}
	cfg := map[string]bool{"white_label": h.whiteLabel(r, slug)}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(cfg)
}

// whiteLabel reads white_label from marketflow-api's GET /api/v1/public/profile/{slug}. False
// on any error, an unknown tenant or a reply without the flag.
func (h *PublicFormsHandler) whiteLabel(r *http.Request, slug string) bool {
	target := h.marketflowURL + "/api/v1/public/profile/" + url.PathEscape(slug)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	if ip := remoteIP(r); ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.logger.Warn("widget config: profile unreachable", zap.String("tenant", slug), zap.Error(err))
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUpstreamBody))
		return false
	}
	var profile struct {
		WhiteLabel bool `json:"white_label"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&profile); err != nil {
		return false
	}
	return profile.WhiteLabel
}
