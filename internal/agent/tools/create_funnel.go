package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
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
		http:       serviceHTTP(15 * time.Second),
		logger:     logger.Named("tool.create_funnel"),
	}
}

func (*CreateFunnel) Name() string { return "create_funnel" }
func (*CreateFunnel) Description() string {
	return "Saves a new landing page funnel as a DRAFT for the owner to review and publish. Provide name, slug, headline, subheadline, and steps (form fields, WhatsApp block, product block, etc.). After the owner publishes it, it is live at /f/{slug} and captures leads via the built-in form. Use get_fbo_profile first to get distribID for product links."
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

// funnelArgs are the only fields the model may set.
type funnelArgs struct {
	Name        string           `json:"name"`
	Slug        string           `json:"slug"`
	Headline    string           `json:"headline"`
	Subheadline string           `json:"subheadline"`
	Description string           `json:"description"`
	FunnelType  string           `json:"funnel_type"`
	BrandColor  string           `json:"brand_color"`
	Steps       []map[string]any `json:"steps"`
}

var (
	funnelSlugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)
	hexColorRe    = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)
	funnelTypes   = map[string]bool{"lead_capture": true, "product_landing": true, "recruitment": true, "event": true}
	funnelBlocks  = map[string]bool{"form": true, "whatsapp": true, "product": true, "recruit": true, "google_calendar": true, "content": true}
	maxFunnelStep = 12
)

// funnelPayload validates the model's arguments and builds marketflow-api's create body. The
// funnel is saved as a draft: /f/{slug} serves published funnels only, and publishing is the
// owner's action (owner decision G5, 2026-11-16). The API reads name, slug, funnel_type, steps
// and metadata; the hero text and theme go in metadata.
func funnelPayload(a funnelArgs) (map[string]any, error) {
	a.Name = strings.TrimSpace(a.Name)
	a.Slug = strings.ToLower(strings.TrimSpace(a.Slug))
	if a.Name == "" || len([]rune(a.Name)) > 120 {
		return nil, fmt.Errorf("name is required (at most 120 characters)")
	}
	if !funnelSlugRe.MatchString(a.Slug) {
		return nil, fmt.Errorf("slug must be 3 to 64 lowercase letters, digits or dashes")
	}
	if a.FunnelType == "" {
		a.FunnelType = "lead_capture"
	}
	if !funnelTypes[a.FunnelType] {
		return nil, fmt.Errorf("funnel_type must be lead_capture, product_landing, recruitment or event")
	}
	if len(a.Steps) > maxFunnelStep {
		return nil, fmt.Errorf("at most %d steps", maxFunnelStep)
	}
	for _, s := range a.Steps {
		if bt, _ := s["block_type"].(string); !funnelBlocks[bt] {
			return nil, fmt.Errorf("unknown block_type %q", bt)
		}
	}
	meta := map[string]any{
		"created_by":   "ai_assistant",
		"needs_review": true,
		"headline":     trimText(a.Headline, 200),
		"subheadline":  trimText(a.Subheadline, 300),
		"description":  trimText(a.Description, 1000),
	}
	if hexColorRe.MatchString(a.BrandColor) {
		meta["theme"] = map[string]any{"brand_color": a.BrandColor}
	}
	payload := map[string]any{
		"name":        a.Name,
		"slug":        a.Slug,
		"funnel_type": a.FunnelType,
		"metadata":    meta,
	}
	if len(a.Steps) > 0 {
		payload["steps"] = a.Steps
	}
	return payload, nil
}

func (t *CreateFunnel) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var a funnelArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	payload, err := funnelPayload(a)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error()), nil
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
		t.logger.Warn("funnel creation failed", zap.Int("status", resp.StatusCode))
		return fmt.Sprintf(`{"error":"funnel creation failed (HTTP %d)"}`, resp.StatusCode), nil
	}

	var created map[string]any
	if json.Unmarshal(rb, &created) == nil {
		slug, _ := created["slug"].(string)
		if slug != "" {
			created["public_url_after_publish"] = fmt.Sprintf("https://marketflow.codevertexafrica.com/f/%s", slug)
		}
		created["next_step"] = "Saved as a draft. It goes live at the public link only after the owner reviews and publishes it from Funnels."
		enriched, _ := json.Marshal(created)
		t.logger.Info("funnel created", zap.String("tenant", tenantID.String()), zap.String("slug", slug))
		return string(enriched), nil
	}
	return string(rb), nil
}
