package tools

import (
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

// EnrichContact returns the profile marketflow-api's contact enricher keeps on a contact
// (Contact.metadata.enrichment): how the person found the business, their orders and payments,
// deals, engagement, the kind of email address and the company account they are linked to. The
// profile comes only from the business's own records and, when the owner allows it, the home page
// of that company account. Nothing about the person is searched on the web, and reading it costs
// no credits. The enricher keeps it current on its own (new or changed contact, new timeline
// entry, weekly refresh), so the tool only reads.
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
		http:       serviceHTTP(10 * time.Second),
		logger:     cfg.Logger.Named("tool.enrich_contact"),
	}
}

func (*EnrichContact) Name() string { return "enrich_contact" }

func (*EnrichContact) Description() string {
	return "Reads a CRM contact's profile, built from the business's own records: how they found the business, orders and payments, open and won deals, recent engagement, a customer status (active customer, lapsed customer, in pipeline, engaged or quiet) and the company they are linked to. Use it to understand who a contact is before advising on them. It never looks the person up on the internet."
}

func (*EnrichContact) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"contact_id": map[string]any{
				"type":        "string",
				"description": "ID of the contact",
			},
		},
		"required": []string{"contact_id"},
	}
}

func (t *EnrichContact) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		ContactID string `json:"contact_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.ContactID == "" {
		return "", fmt.Errorf("contact_id is required")
	}

	url := fmt.Sprintf("%s/api/v1/contacts/%s", t.apiBaseURL, idPath(args.ContactID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("enrich_contact http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return `{"error":"contact not found"}`, nil
	}
	if resp.StatusCode >= 300 {
		t.logger.Warn("enrich_contact api error", zap.Int("status", resp.StatusCode))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return profileResult(args.ContactID, body)
}

// profileResult turns marketflow-api's contact JSON into the tool's answer: the profile when one
// is stored, else a short note the model can pass on.
func profileResult(contactID string, contactJSON []byte) (string, error) {
	var c struct {
		FirstName *string        `json:"first_name"`
		LastName  *string        `json:"last_name"`
		Metadata  map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(contactJSON, &c); err != nil {
		return "", fmt.Errorf("decode contact: %w", err)
	}
	name := strings.TrimSpace(deref(c.FirstName) + " " + deref(c.LastName))
	profile, ok := c.Metadata["enrichment"].(map[string]any)
	if !ok {
		out, _ := json.Marshal(map[string]any{
			"contact_id": contactID,
			"name":       name,
			"profile":    nil,
			"message":    "This contact has no profile yet. Profiles are built within minutes of a contact being added and in a nightly pass, unless the business switched contact profiles off in Settings, Marketing context.",
		})
		return string(out), nil
	}
	out, _ := json.Marshal(map[string]any{"contact_id": contactID, "name": name, "profile": profile})
	return string(out), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
