package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AnalyzeDeal analyzes a deal's health, identifies risks and stalled signals,
// and suggests next actions using LLM analysis.
type AnalyzeDeal struct {
	apiBaseURL string
	serviceKey string
	llm        ai.ChatClient
	llmModel   string
	http       *http.Client
	logger     *zap.Logger
}

// AnalyzeDealConfig configures the tool.
type AnalyzeDealConfig struct {
	APIBaseURL string
	ServiceKey string
	LLMClient  ai.ChatClient
	LLMModel   string
	Logger     *zap.Logger
}

func NewAnalyzeDeal(cfg AnalyzeDealConfig) *AnalyzeDeal {
	return &AnalyzeDeal{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		llm:        cfg.LLMClient,
		llmModel:   cfg.LLMModel,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.analyze_deal"),
	}
}

func (*AnalyzeDeal) Name() string { return "analyze_deal" }

func (*AnalyzeDeal) Description() string {
	return "Analyzes a CRM deal's health, identifies stall risks and warning signals, and suggests next actions. Returns a health score 0-100, key concerns, and recommended next step."
}

func (*AnalyzeDeal) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"deal_id": map[string]any{
				"type":        "string",
				"description": "ID of the deal to analyze",
			},
		},
		"required": []string{"deal_id"},
	}
}

func (t *AnalyzeDeal) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		DealID string `json:"deal_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.DealID == "" {
		return "", fmt.Errorf("deal_id is required")
	}

	// Fetch deal details.
	dealJSON := t.fetchDeal(ctx, tenantID, args.DealID)
	// Fetch deal timeline.
	timelineJSON := t.fetchTimeline(ctx, tenantID, args.DealID)

	// Run LLM analysis.
	prompt := fmt.Sprintf(
		`You are a CRM sales analyst. Analyze this deal and provide a structured assessment.

Deal: %s
Recent activities timeline: %s

Analyze and respond with valid JSON only (no markdown, no extra text):
{
  "health_score": <integer 0-100>,
  "stall_risk": "<low|medium|high>",
  "next_action": "<specific recommended next action>",
  "concerns": ["<concern1>", "<concern2>"],
  "summary": "<2-3 sentence deal summary and assessment>"
}`,
		truncate(dealJSON, 600),
		truncate(timelineJSON, 600),
	)

	resp, err := t.llm.Chat(ctx, ai.ChatRequest{
		Model: t.llmModel,
		Messages: []ai.Message{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.3,
		MaxTokens:   500,
	})
	if err != nil {
		return "", fmt.Errorf("analyze_deal llm: %w", err)
	}

	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}
	content = strings.TrimSpace(content)
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}

	var analysis struct {
		HealthScore int      `json:"health_score"`
		StallRisk   string   `json:"stall_risk"`
		NextAction  string   `json:"next_action"`
		Concerns    []string `json:"concerns"`
		Summary     string   `json:"summary"`
	}
	if err := json.Unmarshal([]byte(content), &analysis); err != nil {
		// Return partial result if LLM parsing fails.
		out, _ := json.Marshal(map[string]any{
			"success": true,
			"deal_id": args.DealID,
			"summary": content,
		})
		return string(out), nil
	}

	out, _ := json.Marshal(map[string]any{
		"success":      true,
		"deal_id":      args.DealID,
		"health_score": analysis.HealthScore,
		"stall_risk":   analysis.StallRisk,
		"next_action":  analysis.NextAction,
		"concerns":     analysis.Concerns,
		"summary":      analysis.Summary,
	})
	t.logger.Info("deal analyzed via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("deal_id", args.DealID),
		zap.Int("health_score", analysis.HealthScore),
	)
	return string(out), nil
}

func (t *AnalyzeDeal) fetchDeal(ctx context.Context, tenantID uuid.UUID, dealID string) string {
	url := fmt.Sprintf("%s/api/v1/deals/%s", t.apiBaseURL, dealID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "{}"
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	resp, err := t.http.Do(req)
	if err != nil {
		return "{}"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "{}"
	}
	return string(body)
}

func (t *AnalyzeDeal) fetchTimeline(ctx context.Context, tenantID uuid.UUID, dealID string) string {
	url := fmt.Sprintf("%s/api/v1/deals/%s/timeline", t.apiBaseURL, dealID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "[]"
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	resp, err := t.http.Do(req)
	if err != nil {
		return "[]"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "[]"
	}
	return string(body)
}
