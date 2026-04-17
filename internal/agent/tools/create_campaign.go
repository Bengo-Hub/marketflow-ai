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
	return "Creates a new marketing campaign. Provide the campaign name, platform (meta/google/tiktok/linkedin/manual), type (lead_gen/conversion/awareness), daily budget (USD), currency, and duration in days. The campaign is created in draft status; user must approve before it goes live. Use check_integrations first to verify the ad platform is connected."
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
