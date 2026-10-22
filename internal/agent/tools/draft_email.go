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

// DraftEmail generates a personalized email draft for a lead/contact based on
// their CRM profile. It does NOT send — it returns subject + body for FBO review.
type DraftEmail struct {
	apiBaseURL string
	serviceKey string
	llm        ai.ChatClient
	llmModel   string
	http       *http.Client
	logger     *zap.Logger
}

// DraftEmailConfig configures the tool.
type DraftEmailConfig struct {
	APIBaseURL string
	ServiceKey string
	LLMClient  ai.ChatClient
	LLMModel   string
	Logger     *zap.Logger
}

func NewDraftEmail(cfg DraftEmailConfig) *DraftEmail {
	return &DraftEmail{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		llm:        cfg.LLMClient,
		llmModel:   cfg.LLMModel,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.draft_email"),
	}
}

func (*DraftEmail) Name() string { return "draft_email" }

func (*DraftEmail) Description() string {
	return "Generates a personalized email draft for a lead or contact based on their CRM profile. Does NOT send — returns subject + body for FBO review before sending. Use get_contact_360 data to personalize the message."
}

func (*DraftEmail) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entity_type": map[string]any{
				"type":        "string",
				"description": "Type of CRM entity: lead or contact",
			},
			"entity_id": map[string]any{
				"type":        "string",
				"description": "ID of the lead or contact",
			},
			"purpose": map[string]any{
				"type":        "string",
				"description": "Purpose of the email (e.g. 'follow up after call', 'introduce C9 product', 'welcome new member')",
			},
			"tone": map[string]any{
				"type":        "string",
				"enum":        []string{"professional", "friendly", "casual"},
				"description": "Tone of the email (default 'friendly')",
				"default":     "friendly",
			},
		},
		"required": []string{"entity_type", "entity_id", "purpose"},
	}
}

func (t *DraftEmail) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		EntityType string `json:"entity_type"`
		EntityID   string `json:"entity_id"`
		Purpose    string `json:"purpose"`
		Tone       string `json:"tone"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.EntityType == "" || args.EntityID == "" || args.Purpose == "" {
		return "", fmt.Errorf("entity_type, entity_id, and purpose are required")
	}
	if args.Tone == "" {
		args.Tone = "friendly"
	}

	// Fetch contact/lead 360 profile for context.
	profileJSON := t.fetch360(ctx, tenantID, args.EntityType, args.EntityID)

	// Parse some basic fields from the profile for the prompt.
	var profile map[string]any
	_ = json.Unmarshal([]byte(profileJSON), &profile)
	contactName := extractName(profile)

	// Build LLM prompt.
	prompt := fmt.Sprintf(
		`You are an AI assistant helping a Forever Living Products FBO (Field Business Owner) draft a sales/relationship email.

Write a short (3 paragraphs max) %s email for the FBO to send to %s.
Purpose: %s
Contact context: %s

Format your response as valid JSON with exactly these fields:
{
  "subject": "<email subject line>",
  "body": "<full email body with greeting, main message, and sign-off>"
}

Do not include any text outside the JSON.`,
		args.Tone, contactName, args.Purpose, truncate(profileJSON, 800),
	)

	resp, err := t.llm.Chat(ctx, ai.ChatRequest{
		Model: t.llmModel,
		Messages: []ai.Message{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.7,
		MaxTokens:   600,
	})
	if err != nil {
		return "", fmt.Errorf("draft_email llm: %w", err)
	}

	content := ""
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
	}

	// Parse LLM response.
	var draft struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	content = strings.TrimSpace(content)
	// Strip markdown code fences if present.
	if idx := strings.Index(content, "{"); idx > 0 {
		content = content[idx:]
	}
	if idx := strings.LastIndex(content, "}"); idx >= 0 && idx < len(content)-1 {
		content = content[:idx+1]
	}
	if err := json.Unmarshal([]byte(content), &draft); err != nil {
		// Return raw content wrapped in JSON if parsing fails.
		out, _ := json.Marshal(map[string]any{
			"success":      true,
			"subject":      fmt.Sprintf("Message for %s", contactName),
			"body":         content,
			"contact_name": contactName,
			"note":         "Review before sending",
		})
		return string(out), nil
	}

	out, _ := json.Marshal(map[string]any{
		"success":      true,
		"subject":      draft.Subject,
		"body":         draft.Body,
		"contact_name": contactName,
		"note":         "Review before sending",
	})
	t.logger.Info("email drafted via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("entity_id", args.EntityID),
		zap.String("purpose", args.Purpose),
	)
	return string(out), nil
}

// fetch360 calls the 360 endpoint for a contact or lead profile.
func (t *DraftEmail) fetch360(ctx context.Context, tenantID uuid.UUID, entityType, entityID string) string {
	segment := "contacts"
	if entityType == "lead" {
		segment = "leads"
	}
	url := fmt.Sprintf("%s/api/v1/%s/%s/360", t.apiBaseURL, segment, entityID)
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

// extractName attempts to pull first_name + last_name from a profile map.
func extractName(profile map[string]any) string {
	first, _ := profile["first_name"].(string)
	last, _ := profile["last_name"].(string)
	if first == "" && last == "" {
		if name, ok := profile["name"].(string); ok {
			return name
		}
		return "the contact"
	}
	return strings.TrimSpace(first + " " + last)
}
