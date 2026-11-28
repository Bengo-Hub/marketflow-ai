package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// widgetConfig is what the Vera widget may change about itself for a tenant, read from the
// tenant's public profile in marketflow-api:
//   - white_label: the Business plan's feature (owner 2026-11-12): the widget hides "Powered by
//     Codevertex" the same way the /p, /f and /book pages do;
//   - logo_url and name (owner 2026-11-28): the tenant's own logo for the launcher and header and
//     its name, used when the embedding page passes none, so a site that only names its tenant
//     still shows its brand instead of the assistant's initial.
type widgetConfig struct {
	WhiteLabel bool   `json:"white_label"`
	LogoURL    string `json:"logo_url,omitempty"`
	Name       string `json:"name,omitempty"`
}

// WidgetConfig handles GET /api/v1/public/widget-config?tenant={slug}. marketflow-api decides
// white label fail closed and caches the profile, so this only reads it; any failure answers
// white_label false (the badge stays) and no logo (the widget keeps its initial).
//
// The slug is a query parameter, not X-Tenant-ID, so the browser sends a simple GET without a
// preflight and may cache the answer for a few minutes.
func (h *PublicFormsHandler) WidgetConfig(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tenant")))
	if !widgetSlugRe.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "tenant must be the tenant slug")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(h.profileConfig(r, slug))
}

// profileConfig reads marketflow-api's GET /api/v1/public/profile/{slug}. An error, an unknown
// tenant or an unreadable reply answers the zero config. Only an https logo link is passed on.
func (h *PublicFormsHandler) profileConfig(r *http.Request, slug string) widgetConfig {
	target := h.marketflowURL + "/api/v1/public/profile/" + url.PathEscape(slug)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return widgetConfig{}
	}
	req.Header.Set("Accept", "application/json")
	if ip := remoteIP(r); ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.logger.Warn("widget config: profile unreachable", zap.String("tenant", slug), zap.Error(err))
		return widgetConfig{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxUpstreamBody))
		return widgetConfig{}
	}
	var profile struct {
		WhiteLabel bool   `json:"white_label"`
		Name       string `json:"name"`
		Metadata   struct {
			LogoURL string `json:"logo_url"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamBody)).Decode(&profile); err != nil {
		return widgetConfig{}
	}
	out := widgetConfig{WhiteLabel: profile.WhiteLabel, Name: strings.TrimSpace(profile.Name)}
	if len([]rune(out.Name)) > 80 {
		out.Name = string([]rune(out.Name)[:80])
	}
	if u, err := url.Parse(strings.TrimSpace(profile.Metadata.LogoURL)); err == nil && u.Scheme == "https" && u.Host != "" && len(profile.Metadata.LogoURL) <= 1000 {
		out.LogoURL = u.String()
	}
	return out
}
