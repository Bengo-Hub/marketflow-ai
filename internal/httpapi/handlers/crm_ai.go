package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"go.uber.org/zap"
)

// CRMAIHandler handles CRM-specific AI inference endpoints.
type CRMAIHandler struct {
	groqClient      *ai.Client
	mainModel       string
	marketflowURL   string
	internalKey     string
	logger          *zap.Logger
}

// NewCRMAIHandler creates a new CRM AI handler.
func NewCRMAIHandler(groqClient *ai.Client, mainModel, marketflowURL, internalKey string, logger *zap.Logger) *CRMAIHandler {
	return &CRMAIHandler{
		groqClient:    groqClient,
		mainModel:     mainModel,
		marketflowURL: marketflowURL,
		internalKey:   internalKey,
		logger:        logger.Named("crm_ai.handler"),
	}
}

// PipelineSummary handles POST /api/v1/ai/pipeline-summary
// @Summary AI-generated deal pipeline health summary
// @Tags CRM AI
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ai/pipeline-summary [post]
func (h *CRMAIHandler) PipelineSummary(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid X-Tenant-ID header")
		return
	}

	deals, err := h.fetchDeals(r.Context(), tenantID.String())
	if err != nil {
		h.logger.Error("failed to fetch deals for pipeline summary", zap.Error(err))
		writeError(w, http.StatusBadGateway, "failed to fetch pipeline data")
		return
	}

	dealsJSON, _ := json.Marshal(deals)
	prompt := fmt.Sprintf(`You are a CRM sales analyst. Analyze this deal pipeline data and provide actionable insights.

Pipeline data:
%s

Respond ONLY with a JSON object:
{
  "deal_health": "healthy|at_risk|critical",
  "summary": "<2-3 sentence executive summary>",
  "bottlenecks": ["<stage or issue causing delays>"],
  "at_risk_deals": [{"id": "<id>", "title": "<title>", "reason": "<why at risk>"}],
  "recommended_actions": ["<specific actionable recommendation>"],
  "win_rate_estimate": <0.0-1.0>
}`, string(dealsJSON))

	resp, err := h.groqClient.Chat(r.Context(), ai.ChatRequest{
		Model: h.mainModel,
		Messages: []ai.Message{
			{Role: "system", Content: "You are a CRM sales analyst. Always respond with valid JSON only."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.3,
		MaxTokens:   1024,
	})
	if err != nil || len(resp.Choices) == 0 {
		h.logger.Error("pipeline summary LLM call failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "AI analysis failed")
		return
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &result); err != nil {
		result = map[string]any{
			"deal_health": "unknown",
			"summary":     resp.Choices[0].Message.Content,
		}
	}
	result["generated_at"] = time.Now().UTC().Format(time.RFC3339)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

// ScoreLead handles POST /api/v1/ai/score-lead
// @Summary AI-generated lead score (0–100)
// @Tags CRM AI
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ai/score-lead [post]
func (h *CRMAIHandler) ScoreLead(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid X-Tenant-ID header")
		return
	}

	var req struct {
		LeadID string `json:"lead_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.LeadID == "" {
		writeError(w, http.StatusBadRequest, "lead_id is required")
		return
	}

	lead, err := h.fetchLead(r.Context(), tenantID.String(), req.LeadID)
	if err != nil {
		h.logger.Error("failed to fetch lead for scoring", zap.Error(err))
		writeError(w, http.StatusBadGateway, "failed to fetch lead data")
		return
	}

	leadJSON, _ := json.Marshal(lead)
	prompt := fmt.Sprintf(`You are a CRM lead scoring analyst. Score this lead on a scale of 0-100.

Lead data:
%s

Scoring criteria:
- Contact completeness (email, phone, company): 0-20 points
- Engagement signals (chat sessions, activities, funnel interactions): 0-30 points
- Qualification (source quality, interest level, qualification answers): 0-25 points
- Recency (recent activity vs dormant): 0-15 points
- Contextual fit (business type match, deal size potential): 0-10 points

Respond ONLY with a JSON object:
{
  "score": <0-100>,
  "grade": "A|B|C|D|F",
  "reasoning": "<2-3 sentence explanation>",
  "strengths": ["<positive signal>"],
  "weaknesses": ["<gap or concern>"],
  "recommended_action": "<next best action for this lead>"
}`, string(leadJSON))

	resp, err := h.groqClient.Chat(r.Context(), ai.ChatRequest{
		Model: h.mainModel,
		Messages: []ai.Message{
			{Role: "system", Content: "You are a CRM lead scoring analyst. Always respond with valid JSON only."},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.2,
		MaxTokens:   512,
	})
	if err != nil || len(resp.Choices) == 0 {
		h.logger.Error("lead scoring LLM call failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "AI scoring failed")
		return
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &result); err != nil {
		result = map[string]any{"score": 50, "grade": "C", "reasoning": resp.Choices[0].Message.Content}
	}
	result["lead_id"] = req.LeadID
	result["generated_at"] = time.Now().UTC().Format(time.RFC3339)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

// fetchDeals calls marketflow-api S2S to get open deals for the tenant.
func (h *CRMAIHandler) fetchDeals(ctx context.Context, tenantID string) (any, error) {
	url := fmt.Sprintf("%s/api/v1/deals?status=open&limit=50", h.marketflowURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-API-Key", h.internalKey)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result any
	_ = json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// fetchLead calls marketflow-api S2S to get a single lead.
func (h *CRMAIHandler) fetchLead(ctx context.Context, tenantID, leadID string) (any, error) {
	url := fmt.Sprintf("%s/api/v1/leads/%s", h.marketflowURL, leadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-API-Key", h.internalKey)

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result any
	_ = json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}
