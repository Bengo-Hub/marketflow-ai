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

// ScoreLead triggers the lead scoring engine to recalculate a lead's score
// based on CRM rules via S2S call to marketflow-api.
type ScoreLead struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// ScoreLeadConfig configures the tool.
type ScoreLeadConfig struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewScoreLead(cfg ScoreLeadConfig) *ScoreLead {
	return &ScoreLead{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.score_lead"),
	}
}

func (*ScoreLead) Name() string { return "score_lead" }

func (*ScoreLead) Description() string {
	return "Triggers the lead scoring engine to recalculate a lead's score based on CRM rules, engagement history, and profile completeness. Use this after updating a lead's profile or logging new activities."
}

func (*ScoreLead) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"lead_id": map[string]any{
				"type":        "string",
				"description": "ID of the lead to rescore",
			},
		},
		"required": []string{"lead_id"},
	}
}

func (t *ScoreLead) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		LeadID string `json:"lead_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.LeadID == "" {
		return "", fmt.Errorf("lead_id is required")
	}

	url := fmt.Sprintf("%s/api/v1/leads/%s/rescore", t.apiBaseURL, args.LeadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("score_lead http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("score_lead api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var m map[string]any
	_ = json.Unmarshal(respBody, &m)

	out, _ := json.Marshal(map[string]any{
		"success":   true,
		"lead_id":   args.LeadID,
		"new_score": m["score"],
		"breakdown": m["breakdown"],
	})
	t.logger.Info("lead rescored via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("lead_id", args.LeadID),
	)
	return string(out), nil
}
