package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	return "Creates a new marketing campaign. Provide the campaign name, platform (meta/google/tiktok/linkedin/manual), type (lead_gen/conversion/awareness), daily budget (USD), currency, and duration in days. IMPORTANT: Before calling this, use calculate_ad_cost to show the quote: the ad budget, which the platform bills to the tenant's own ad account at its own price with no MarketFlow fee, and the AI credits for making the ads. Use check_integrations first to verify the ad platform is connected."
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
		errMsg := string(rb)
		// Translate known ad-platform account errors into actionable guidance
		// so the agent can explain the specific blocker to the user.
		actionable := classifyCampaignError(platform, resp.StatusCode, errMsg)
		t.logger.Warn("campaign creation failed",
			zap.String("platform", platform),
			zap.Int("status", resp.StatusCode),
			zap.String("error", errMsg),
		)
		return actionable, nil
	}

	t.logger.Info("campaign created", zap.String("tenant", tenantID.String()))
	return string(rb), nil
}

// classifyCampaignError converts raw ad-platform API errors into structured
// JSON the agent can cite to give the user a specific, actionable next step.
func classifyCampaignError(platform string, status int, body string) string {
	lower := strings.ToLower(body)
	var code, message, action string

	switch {
	// TikTok: manager account not linked to a billing method — cannot create
	// child advertiser accounts. User must add a credit card to the manager
	// account directly in TikTok Ads Manager before tenants can run ads.
	case strings.Contains(lower, "advertiser") && strings.Contains(lower, "credit") ||
		strings.Contains(lower, "billing") && strings.Contains(lower, "manager") ||
		strings.Contains(lower, "no payment") ||
		strings.Contains(lower, "payment method"):
		code = "billing_not_configured"
		message = "The connected ad account manager does not have a billing method attached. Individual advertiser accounts cannot be created until the manager account has a valid credit card."
		switch platform {
		case "tiktok":
			action = "Add a credit card to your TikTok Ads Manager account at ads.tiktok.com → Account → Billing & Payment, then reconnect at /settings/integrations."
		case "meta":
			action = "Add a payment method to your Meta Business Suite at business.facebook.com → Billing → Payment Methods, then reconnect at /settings/integrations."
		case "google":
			action = "Add billing to your Google Ads manager account at ads.google.com → Billing → Payment Methods."
		default:
			action = "Add a billing method to the connected ad platform manager account, then try again."
		}

	// Account suspended / policy violation
	case strings.Contains(lower, "suspended") || strings.Contains(lower, "banned") ||
		strings.Contains(lower, "policy") || strings.Contains(lower, "violation"):
		code = "account_suspended"
		message = "The connected ad account has been suspended or flagged for a policy violation."
		action = "Review the account status in the ad platform's dashboard and resolve any policy issues before running campaigns."

	// MarketFlow AI credits used up (402 from marketflow-api's plan checks).
	case status == http.StatusPaymentRequired || strings.Contains(lower, "ai credit"):
		code = "ai_credits_exhausted"
		message = "Your MarketFlow AI credits are used up, so the campaign could not be prepared."
		action = "Buy a credit bundle or upgrade your plan at /settings/billing, then try again."

	// The ad account cannot pay (the platform bills the tenant's own account).
	case strings.Contains(lower, "insufficient") || strings.Contains(lower, "balance") ||
		strings.Contains(lower, "funds"):
		code = "ad_account_funds"
		message = "The connected ad account cannot pay for this campaign (no funds or no valid payment method)."
		action = "Add a payment method or funds in the ad platform's billing settings for your own ad account, then try again."

	// Permission / scope error
	case status == 403 || strings.Contains(lower, "permission") || strings.Contains(lower, "unauthorized"):
		code = "permission_denied"
		message = "The connected ad account does not have permission to create campaigns through this integration."
		action = "Reconnect the ad platform at /settings/integrations and ensure the account has Campaign Manager or Advertiser role."

	// Generic fallback
	default:
		code = "campaign_creation_failed"
		message = fmt.Sprintf("Campaign creation failed (HTTP %d).", status)
		action = "Check /settings/integrations to verify the platform is correctly connected and try again."
	}

	out, _ := json.Marshal(map[string]string{
		"error":   code,
		"message": message,
		"action":  action,
	})
	return string(out)
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
