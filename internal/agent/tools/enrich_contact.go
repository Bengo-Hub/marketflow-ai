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

// EnrichContact marks a CRM contact for enrichment by setting enrichment
// metadata via S2S PATCH to marketflow-api. The actual web search and profile
// update is handled downstream by the enrichment pipeline.
type EnrichContact struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// EnrichContactConfig configures the tool.
type EnrichContactConfig struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewEnrichContact(cfg EnrichContactConfig) *EnrichContact {
	return &EnrichContact{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.enrich_contact"),
	}
}

func (*EnrichContact) Name() string { return "enrich_contact" }

func (*EnrichContact) Description() string {
	return "Enriches a CRM contact with publicly available information by requesting the enrichment pipeline to search the web for their company and professional background. Marks the contact for enrichment — Vera will search and update the profile."
}

func (*EnrichContact) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"contact_id": map[string]any{
				"type":        "string",
				"description": "ID of the contact to enrich",
			},
			"search_query": map[string]any{
				"type":        "string",
				"description": "Custom search query (optional — defaults to '{first_name} {last_name} {company}')",
			},
		},
		"required": []string{"contact_id"},
	}
}

func (t *EnrichContact) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		ContactID   string `json:"contact_id"`
		SearchQuery string `json:"search_query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.ContactID == "" {
		return "", fmt.Errorf("contact_id is required")
	}

	// Fetch basic contact info to build default search query if not provided.
	query := args.SearchQuery
	if query == "" {
		query = t.buildDefaultQuery(ctx, tenantID, args.ContactID)
	}

	// PATCH the contact with enrichment metadata to trigger the pipeline.
	patchBody := map[string]any{
		"enrichment_data": map[string]any{
			"enrichment_requested": true,
			"enrichment_query":     query,
		},
	}
	payload, _ := json.Marshal(patchBody)

	url := fmt.Sprintf("%s/api/v1/contacts/%s", t.apiBaseURL, args.ContactID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("enrich_contact http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("enrich_contact api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	out, _ := json.Marshal(map[string]any{
		"success":    true,
		"contact_id": args.ContactID,
		"message":    "Enrichment requested. Vera will search and update the contact profile.",
	})
	t.logger.Info("contact enrichment requested via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("contact_id", args.ContactID),
		zap.String("query", query),
	)
	return string(out), nil
}

// buildDefaultQuery fetches the contact's name and company to build a search query.
func (t *EnrichContact) buildDefaultQuery(ctx context.Context, tenantID uuid.UUID, contactID string) string {
	url := fmt.Sprintf("%s/api/v1/contacts/%s", t.apiBaseURL, contactID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return contactID
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return contactID
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return contactID
	}

	var contact map[string]any
	if err := json.Unmarshal(body, &contact); err != nil {
		return contactID
	}

	first, _ := contact["first_name"].(string)
	last, _ := contact["last_name"].(string)
	company, _ := contact["company"].(string)

	parts := []string{}
	if first != "" {
		parts = append(parts, first)
	}
	if last != "" {
		parts = append(parts, last)
	}
	if company != "" {
		parts = append(parts, company)
	}
	if len(parts) == 0 {
		return contactID
	}
	return strings.Join(parts, " ")
}
