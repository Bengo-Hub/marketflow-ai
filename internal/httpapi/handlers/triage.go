package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"go.uber.org/zap"
)

const triageSystemPrompt = `You are a support ticket classifier. Given a ticket subject and description, classify it.

Respond with ONLY a JSON object, nothing else:
{"category": "<category>", "priority": "<priority>", "confidence": <0.0-1.0>, "reasoning": "<one sentence>"}

Categories: billing | technical | product | general
Priorities: low | normal | high | urgent

Priority rules:
- urgent: service completely down, data loss, security breach, payment failure blocking operations
- high: significant feature broken, multiple users affected, revenue impact
- normal: single user issue, non-critical feature malfunction
- low: question, how-to, minor UX issue`

// TriageResult is the AI triage classification result.
type TriageResult struct {
	Category             string  `json:"category"`
	Priority             string  `json:"priority"`
	Confidence           float64 `json:"confidence"`
	Reasoning            string  `json:"reasoning"`
	SuggestedResolution  string  `json:"suggested_resolution,omitempty"`
	RequiresHumanReview  bool    `json:"requires_human_review"`
}

// TriageHandler handles AI-powered ticket triage.
type TriageHandler struct {
	groqClient  ai.ChatClient
	routerModel string
	logger      *zap.Logger
}

// NewTriageHandler creates a new triage handler.
func NewTriageHandler(groqClient ai.ChatClient, routerModel string, logger *zap.Logger) *TriageHandler {
	return &TriageHandler{
		groqClient:  groqClient,
		routerModel: routerModel,
		logger:      logger.Named("triage.handler"),
	}
}

// Triage handles POST /api/v1/internal/triage (X-API-Key auth)
// @Summary AI-classify a support ticket
// @Description Classifies ticket subject+description into category+priority using the fast LLM router
// @Tags Internal
// @Accept json
// @Produce json
// @Param request body object true "Ticket text"
// @Success 200 {object} TriageResult
// @Security apiKey
// @Router /internal/triage [post]
func (h *TriageHandler) Triage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
		TenantID    string `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}

	result, err := h.classify(r.Context(), req.Subject, req.Description)
	if err != nil {
		h.logger.Error("triage classification failed", zap.Error(err))
		// Fail-open: return neutral classification rather than blocking ticket creation
		result = &TriageResult{
			Category:            "general",
			Priority:            "normal",
			Confidence:          0,
			RequiresHumanReview: true,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

// Classify handles POST /api/v1/classify — simpler response for ticketing-api
// @Summary Classify ticket text
// @Tags Internal
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security apiKey
// @Router /classify [post]
func (h *TriageHandler) Classify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	result, err := h.classify(r.Context(), req.Text, "")
	if err != nil {
		h.logger.Error("classify failed", zap.Error(err))
		result = &TriageResult{Category: "general", Priority: "normal", Confidence: 0, RequiresHumanReview: true}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"category":   result.Category,
		"priority":   result.Priority,
		"confidence": result.Confidence,
	})
}

func (h *TriageHandler) classify(ctx context.Context, subject, description string) (*TriageResult, error) {
	text := subject
	if description != "" {
		text = fmt.Sprintf("Subject: %s\n\nDescription: %s", subject, description)
	}

	resp, err := h.groqClient.Chat(ctx, ai.ChatRequest{
		Model: h.routerModel,
		Messages: []ai.Message{
			{Role: "system", Content: triageSystemPrompt},
			{Role: "user", Content: text},
		},
		Temperature: 0.1,
		MaxTokens:   256,
	})
	if err != nil {
		return nil, fmt.Errorf("groq classify: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no response from classifier")
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	// Strip markdown code fence if present
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) > 2 {
			raw = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}

	var parsed struct {
		Category   string  `json:"category"`
		Priority   string  `json:"priority"`
		Confidence float64 `json:"confidence"`
		Reasoning  string  `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		h.logger.Warn("failed to parse triage JSON", zap.String("raw", raw), zap.Error(err))
		return &TriageResult{Category: "general", Priority: "normal", Confidence: 0, RequiresHumanReview: true}, nil
	}

	result := &TriageResult{
		Category:            sanitizeCategory(parsed.Category),
		Priority:            sanitizePriority(parsed.Priority),
		Confidence:          parsed.Confidence,
		Reasoning:           parsed.Reasoning,
		RequiresHumanReview: parsed.Confidence < 0.85,
	}
	return result, nil
}

func sanitizeCategory(c string) string {
	switch c {
	case "billing", "technical", "product", "general":
		return c
	}
	return "general"
}

func sanitizePriority(p string) string {
	switch p {
	case "low", "normal", "high", "urgent":
		return p
	}
	return "normal"
}
