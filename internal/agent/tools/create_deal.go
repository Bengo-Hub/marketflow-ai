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

// CreateDeal creates a CRM deal for a contact via S2S call to marketflow-api.
type CreateDeal struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// CreateDealConfig configures the tool.
type CreateDealConfig struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewCreateDeal(cfg CreateDealConfig) *CreateDeal {
	return &CreateDeal{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.create_deal"),
	}
}

func (*CreateDeal) Name() string { return "create_deal" }

func (*CreateDeal) Description() string {
	return "Creates a CRM deal for a contact via S2S POST to marketflow-api. Use this when a lead or contact is ready to enter the sales pipeline or when an FBO wants to track a sales opportunity."
}

func (*CreateDeal) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"contact_id": map[string]any{
				"type":        "string",
				"description": "ID of the contact to associate the deal with",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Deal title (e.g. 'C9 Challenge Upsell')",
			},
			"value": map[string]any{
				"type":        "number",
				"description": "Monetary value of the deal",
			},
			"currency": map[string]any{
				"type":        "string",
				"description": "Currency code (default KES)",
				"default":     "KES",
			},
			"pipeline_id": map[string]any{
				"type":        "string",
				"description": "ID of the pipeline to add this deal to (optional)",
			},
			"notes": map[string]any{
				"type":        "string",
				"description": "Additional notes about the deal (optional)",
			},
		},
		"required": []string{"contact_id", "title"},
	}
}

func (t *CreateDeal) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		ContactID  string  `json:"contact_id"`
		Title      string  `json:"title"`
		Value      float64 `json:"value"`
		Currency   string  `json:"currency"`
		PipelineID string  `json:"pipeline_id"`
		Notes      string  `json:"notes"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.ContactID == "" || args.Title == "" {
		return "", fmt.Errorf("contact_id and title are required")
	}
	if args.Currency == "" {
		args.Currency = "KES"
	}

	body := map[string]any{
		"contact_id": args.ContactID,
		"title":      args.Title,
		"currency":   args.Currency,
	}
	if args.Value > 0 {
		body["value"] = args.Value
	}
	if args.PipelineID != "" {
		body["pipeline_id"] = args.PipelineID
	}
	if args.Notes != "" {
		body["notes"] = args.Notes
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.apiBaseURL+"/api/v1/deals", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create_deal http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("create_deal api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var m map[string]any
	_ = json.Unmarshal(respBody, &m)

	dealID, _ := m["id"].(string)
	out, _ := json.Marshal(map[string]any{
		"success": true,
		"deal_id": dealID,
		"title":   args.Title,
		"message": fmt.Sprintf("Deal '%s' created successfully.", args.Title),
	})
	t.logger.Info("deal created via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("deal_id", dealID),
	)
	return string(out), nil
}
