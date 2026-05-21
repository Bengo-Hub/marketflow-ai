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

// GetContact360 fetches the full 360° profile of a CRM contact: profile,
// open deals, activity timeline, open tasks, upcoming meetings, and loyalty data.
type GetContact360 struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// GetContact360Config configures the tool.
type GetContact360Config struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewGetContact360(cfg GetContact360Config) *GetContact360 {
	return &GetContact360{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.get_contact_360"),
	}
}

func (*GetContact360) Name() string { return "get_contact_360" }

func (*GetContact360) Description() string {
	return "Fetches the full 360° profile of a CRM contact: profile details, open deals, activity timeline, open tasks, upcoming meetings, and loyalty data. Use this to get full context before drafting communications, analyzing relationships, or preparing for meetings."
}

func (*GetContact360) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"contact_id": map[string]any{
				"type":        "string",
				"description": "ID of the contact to fetch the 360° profile for",
			},
		},
		"required": []string{"contact_id"},
	}
}

func (t *GetContact360) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		ContactID string `json:"contact_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.ContactID == "" {
		return "", fmt.Errorf("contact_id is required")
	}

	url := fmt.Sprintf("%s/api/v1/contacts/%s/360", t.apiBaseURL, args.ContactID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get_contact_360 http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("get_contact_360 api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	result := string(respBody)
	t.logger.Info("contact 360 fetched via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("contact_id", args.ContactID),
	)
	return truncate(result, 1500), nil
}
