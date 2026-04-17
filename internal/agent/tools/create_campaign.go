package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CreateCampaign calls marketflow-api POST /api/v1/campaigns to create a new
// campaign record. When the tenant has a connected Meta/Google/TikTok account,
// the API will mirror the campaign to that ad platform.
type CreateCampaign struct {
	apiBaseURL string
	apiKey     string
	http       *http.Client
	logger     *zap.Logger
}

func NewCreateCampaign(apiBaseURL, apiKey string, logger *zap.Logger) *CreateCampaign {
	return &CreateCampaign{
		apiBaseURL: apiBaseURL,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     logger.Named("tool.create_campaign"),
	}
}

func (*CreateCampaign) Name() string { return "create_campaign" }
func (*CreateCampaign) Description() string {
	return "Creates a new marketing campaign. Provide the campaign name, platform (meta/google/tiktok/linkedin/manual), type (lead_gen/conversion/awareness), daily budget (USD), currency, and duration in days. IMPORTANT: Before calling this, use calculate_ad_cost to show the tenant the total cost (ads budget + service charge). The total is debited from their wallet in a single payment — MarketFlow pays Meta/Google on their behalf. Use check_integrations first to verify the ad platform is connected."
}
func (*CreateCampaign) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":          map[string]any{"type": "string", "description": "Campaign name (e.g. 'C9 Challenge Lead Ads')"},
			"platform":      map[string]any{"type": "string", "description": "meta | google | tiktok | linkedin | manual"},
			"campaign_type": map[string]any{"type": "string", "description": "lead_gen | conversion | awareness | traffic"},
			"budget_daily":  map[string]any{"type": "number", "description": "Daily budget in USD"},
			"currency":      map[string]any{"type": "string", "description": "USD or KES", "default": "USD"},
			"duration_days": map[string]any{"type": "integer", "description": "How many days to run the campaign"},
			"target_audience": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"age_min":   map[string]any{"type": "integer"},
					"age_max":   map[string]any{"type": "integer"},
					"locations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Country codes or city names"},
					"interests": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
			"description": map[string]any{"type": "string", "description": "Ad copy / campaign description"},
		},
		"required": []string{"name", "platform"},
	}
}

func (t *CreateCampaign) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Prerequisite: verify the target ad platform is connected before creating.
	platform, _ := args["platform"].(string)
	if platform != "" && platform != "manual" {
		connected, checkErr := t.checkPlatformConnected(ctx, tenantID, platform)
		if checkErr != nil {
			t.logger.Warn("pre-flight integration check failed, proceeding anyway",
				zap.String("platform", platform), zap.Error(checkErr))
		} else if !connected {
			return fmt.Sprintf(
				`{"error":"The %s ad platform is not connected. Please connect it at /settings/integrations before creating a campaign.","action":"connect_integration","platform":%q,"setup_path":"/settings/integrations"}`,
				platform, platform,
			), nil
		}
	}

	body, _ := json.Marshal(args)
	url := fmt.Sprintf("%s/api/v1/campaigns", t.apiBaseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.apiKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create_campaign: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Sprintf(`{"error":"campaign creation failed: %s"}`, string(rb)), nil
	}

	t.logger.Info("campaign created", zap.String("tenant", tenantID.String()))
	return string(rb), nil
}

// checkPlatformConnected queries the integrations API to verify the target ad
// platform is connected for this tenant. Returns (true, nil) if connected.
func (t *CreateCampaign) checkPlatformConnected(ctx context.Context, tenantID uuid.UUID, platform string) (bool, error) {
	// Map campaign platform names to provider type names used by the resolve endpoint.
	typeMap := map[string]struct{ ptype, pname string }{
		"meta":     {"meta_ads", "oauth"},
		"google":   {"google_ads", "ads"},
		"tiktok":   {"tiktok_ads", "ads"},
		"linkedin": {"linkedin_ads", "ads"},
	}
	info, ok := typeMap[platform]
	if !ok {
		// Unknown platform — skip the check and let the API handle it.
		return true, nil
	}

	url := fmt.Sprintf("%s/api/v1/admin/providers/resolve?type=%s&name=%s", t.apiBaseURL, info.ptype, info.pname)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-API-Key", t.apiKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("check integration %s: %w", platform, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false, nil
	}
	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Settings map[string]string `json:"settings"`
	}
	if json.Unmarshal(body, &parsed) != nil || len(parsed.Settings) == 0 {
		return false, nil
	}
	return true, nil
}
