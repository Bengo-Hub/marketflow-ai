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

// CreateFunnel calls marketflow-api POST /api/v1/funnels to create a new
// landing page funnel. The funnel is publicly accessible at /f/{slug} and
// can be linked to a campaign for conversion tracking.
type CreateFunnel struct {
	apiBaseURL string
	apiKey     string
	http       *http.Client
	logger     *zap.Logger
}

func NewCreateFunnel(apiBaseURL, apiKey string, logger *zap.Logger) *CreateFunnel {
	return &CreateFunnel{
		apiBaseURL: apiBaseURL,
		apiKey:     apiKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     logger.Named("tool.create_funnel"),
	}
}

func (*CreateFunnel) Name() string { return "create_funnel" }
func (*CreateFunnel) Description() string {
	return "Creates a new landing page funnel. Provide name, slug, headline, subheadline, and steps (form fields, WhatsApp block, product block, etc.). The funnel is published at /f/{slug} and can capture leads via the built-in form. Use get_fbo_profile first to get distribID for product links."
}
func (*CreateFunnel) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "Funnel name (e.g. 'C9 Challenge Signup')"},
			"slug":        map[string]any{"type": "string", "description": "URL slug (e.g. 'c9-challenge'). Will be accessible at /f/{slug}"},
			"headline":    map[string]any{"type": "string", "description": "Hero headline text"},
			"subheadline": map[string]any{"type": "string", "description": "Hero subheadline text"},
			"description": map[string]any{"type": "string", "description": "Internal description of this funnel's purpose"},
			"funnel_type": map[string]any{"type": "string", "description": "lead_capture | product_landing | recruitment | event", "default": "lead_capture"},
			"brand_color": map[string]any{"type": "string", "description": "Hex color for CTA buttons (e.g. '#16a34a')"},
			"steps": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"block_type":  map[string]any{"type": "string", "description": "form | whatsapp | product | recruit | google_calendar | content"},
						"title":       map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"cta_text":    map[string]any{"type": "string"},
						"fields": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"key":         map[string]any{"type": "string"},
									"label":       map[string]any{"type": "string"},
									"type":        map[string]any{"type": "string", "default": "text"},
									"required":    map[string]any{"type": "boolean"},
									"placeholder": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			},
		},
		"required": []string{"name", "slug"},
	}
}

func (t *CreateFunnel) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	// Build content_json from steps + theme
	contentJSON := map[string]any{}
	if steps, ok := args["steps"]; ok {
		contentJSON["steps"] = steps
	}
	if color, ok := args["brand_color"].(string); ok {
		contentJSON["theme"] = map[string]any{"brand_color": color}
	}

	payload := map[string]any{
		"name":         args["name"],
		"slug":         args["slug"],
		"funnel_type":  args["funnel_type"],
		"content_json": contentJSON,
	}
	if h, ok := args["headline"].(string); ok {
		payload["headline"] = h
	}
	if s, ok := args["subheadline"].(string); ok {
		payload["subheadline"] = s
	}
	if d, ok := args["description"].(string); ok {
		payload["description"] = d
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/v1/funnels", t.apiBaseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.apiKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create_funnel: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Sprintf(`{"error":"funnel creation failed: %s"}`, string(rb)), nil
	}

	var created map[string]any
	if json.Unmarshal(rb, &created) == nil {
		slug, _ := created["slug"].(string)
		if slug != "" {
			created["public_url"] = fmt.Sprintf("https://marketflow.codevertexitsolutions.com/f/%s", slug)
		}
		enriched, _ := json.Marshal(created)
		t.logger.Info("funnel created", zap.String("tenant", tenantID.String()), zap.String("slug", slug))
		return string(enriched), nil
	}
	return string(rb), nil
}
