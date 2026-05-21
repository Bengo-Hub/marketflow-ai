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

// MovePipelineStage moves a CRM deal to a different pipeline stage.
type MovePipelineStage struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// MovePipelineStageConfig configures the tool.
type MovePipelineStageConfig struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewMovePipelineStage(cfg MovePipelineStageConfig) *MovePipelineStage {
	return &MovePipelineStage{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.move_pipeline_stage"),
	}
}

func (*MovePipelineStage) Name() string { return "move_pipeline_stage" }

func (*MovePipelineStage) Description() string {
	return "Moves a CRM deal to a different pipeline stage. Use this when progressing a deal through the sales funnel (e.g. from 'contacted' to 'proposal sent' or 'closed won')."
}

func (*MovePipelineStage) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deal_id": map[string]any{
				"type":        "string",
				"description": "ID of the deal to move",
			},
			"stage_id": map[string]any{
				"type":        "string",
				"description": "ID of the pipeline stage to move the deal to",
			},
		},
		"required": []string{"deal_id", "stage_id"},
	}
}

func (t *MovePipelineStage) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		DealID  string `json:"deal_id"`
		StageID string `json:"stage_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.DealID == "" || args.StageID == "" {
		return "", fmt.Errorf("deal_id and stage_id are required")
	}

	payload, _ := json.Marshal(map[string]any{"stage_id": args.StageID})
	url := fmt.Sprintf("%s/api/v1/deals/%s/move", t.apiBaseURL, args.DealID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("move_pipeline_stage http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("move_pipeline_stage api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var m map[string]any
	_ = json.Unmarshal(respBody, &m)

	newStage, _ := m["stage_name"].(string)
	out, _ := json.Marshal(map[string]any{
		"success":   true,
		"deal_id":   args.DealID,
		"new_stage": newStage,
		"message":   fmt.Sprintf("Deal moved to stage '%s'.", args.StageID),
	})
	t.logger.Info("deal moved via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("deal_id", args.DealID),
		zap.String("stage_id", args.StageID),
	)
	return string(out), nil
}
