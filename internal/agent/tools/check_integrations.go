package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CheckIntegrations queries marketflow-api for the tenant's connected ad
// platforms, calendar, and messaging channels. The agent calls this BEFORE
// attempting to set up a campaign so it can tell the user what's missing.
type CheckIntegrations struct {
	apiBaseURL string
	apiKey     string
	http       *http.Client
	logger     *zap.Logger
}

func NewCheckIntegrations(apiBaseURL, apiKey string, logger *zap.Logger) *CheckIntegrations {
	return &CheckIntegrations{
		apiBaseURL: apiBaseURL,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 10 * time.Second},
		logger:     logger.Named("tool.check_integrations"),
	}
}

func (*CheckIntegrations) Name() string { return "check_integrations" }
func (*CheckIntegrations) Description() string {
	return "Checks which ad platforms the tenant has connected (Meta, Google Ads, TikTok, LinkedIn, X) and whether Google Calendar is linked. Call this before suggesting campaign creation to verify the required platform is connected. Returns a JSON object with {platform: connected_bool} for each."
}
func (*CheckIntegrations) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"platform": map[string]any{
				"type":        "string",
				"description": "Optional: check a specific platform only (meta, google_ads, tiktok, linkedin, x, google_calendar). Omit to check all.",
			},
		},
	}
}

func (t *CheckIntegrations) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	// Query /admin/providers/resolve for each known integration type.
	// The resolve endpoint returns settings if configured; 404 otherwise.
	platforms := []struct {
		key      string
		ptype    string
		pname    string
		label    string
		authPath string
	}{
		{"meta", "meta_ads", "oauth", "Meta (Facebook + Instagram)", "/settings/integrations"},
		{"google_ads", "google_ads", "ads", "Google Ads", "/settings/integrations"},
		{"tiktok", "tiktok_ads", "ads", "TikTok for Business", "/settings/integrations"},
		{"linkedin", "linkedin_ads", "ads", "LinkedIn Ads", "/settings/integrations"},
		{"x", "x_ads", "ads", "X (Twitter) Ads", "/settings/integrations"},
		{"google_calendar", "google_calendar", "oauth", "Google Calendar", "/settings/integrations"},
		{"calcom", "calcom", "default", "Cal.com", "/settings/integrations"},
	}

	result := map[string]any{}
	for _, p := range platforms {
		url := fmt.Sprintf("%s/api/v1/admin/providers/resolve?type=%s&name=%s", t.apiBaseURL, p.ptype, p.pname)
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("X-API-Key", t.apiKey)
		req.Header.Set("X-Tenant-ID", tenantID.String())
		resp, err := t.http.Do(req)
		connected := false
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				body, _ := io.ReadAll(resp.Body)
				var parsed struct {
					Settings map[string]string `json:"settings"`
				}
				if json.Unmarshal(body, &parsed) == nil && len(parsed.Settings) > 0 {
					connected = true
				}
			}
		}
		result[p.key] = map[string]any{
			"connected":  connected,
			"label":      p.label,
			"setup_path": p.authPath,
		}
	}

	out, _ := json.Marshal(result)
	t.logger.Debug("check_integrations", zap.String("tenant", tenantID.String()))
	return string(out), nil
}
