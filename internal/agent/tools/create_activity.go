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

// CreateActivity logs an activity (call, email, note, meeting, sms) to a
// CRM entity's timeline via S2S call to marketflow-api.
type CreateActivity struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// CreateActivityConfig configures the tool.
type CreateActivityConfig struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewCreateActivity(cfg CreateActivityConfig) *CreateActivity {
	return &CreateActivity{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.create_activity"),
	}
}

func (*CreateActivity) Name() string { return "create_activity" }

func (*CreateActivity) Description() string {
	return "Logs an activity (call, email, note, meeting, sms) to a CRM entity's timeline. Use this to record interactions with leads, contacts, or deals so the FBO has a complete history."
}

func (*CreateActivity) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entity_type": map[string]any{
				"type":        "string",
				"enum":        []string{"lead", "contact", "deal"},
				"description": "Type of CRM entity to log the activity against",
			},
			"entity_id": map[string]any{
				"type":        "string",
				"description": "ID of the lead, contact, or deal",
			},
			"activity_type": map[string]any{
				"type":        "string",
				"enum":        []string{"call", "email", "note", "meeting", "sms"},
				"description": "Type of activity performed",
			},
			"subject": map[string]any{
				"type":        "string",
				"description": "Short subject/title of the activity",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Detailed notes or content of the activity",
			},
			"outcome": map[string]any{
				"type":        "string",
				"description": "Outcome of the activity (e.g. 'interested', 'no answer', 'follow up scheduled')",
			},
			"duration_minutes": map[string]any{
				"type":        "number",
				"description": "Duration of the activity in minutes (for calls and meetings)",
			},
		},
		"required": []string{"entity_type", "entity_id", "activity_type"},
	}
}

func (t *CreateActivity) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		EntityType      string  `json:"entity_type"`
		EntityID        string  `json:"entity_id"`
		ActivityType    string  `json:"activity_type"`
		Subject         string  `json:"subject"`
		Body            string  `json:"body"`
		Outcome         string  `json:"outcome"`
		DurationMinutes float64 `json:"duration_minutes"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.EntityType == "" || args.EntityID == "" || args.ActivityType == "" {
		return "", fmt.Errorf("entity_type, entity_id, and activity_type are required")
	}

	body := map[string]any{
		"entity_type":   args.EntityType,
		"entity_id":     args.EntityID,
		"activity_type": args.ActivityType,
	}
	if args.Subject != "" {
		body["subject"] = args.Subject
	}
	if args.Body != "" {
		body["body"] = args.Body
	}
	if args.Outcome != "" {
		body["outcome"] = args.Outcome
	}
	if args.DurationMinutes > 0 {
		body["duration_minutes"] = args.DurationMinutes
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.apiBaseURL+"/api/v1/activities", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create_activity http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("create_activity api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var m map[string]any
	_ = json.Unmarshal(respBody, &m)

	activityID, _ := m["id"].(string)
	out, _ := json.Marshal(map[string]any{
		"success":     true,
		"activity_id": activityID,
		"message":     fmt.Sprintf("Activity '%s' logged successfully.", args.ActivityType),
	})
	t.logger.Info("activity created via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("entity_type", args.EntityType),
		zap.String("entity_id", args.EntityID),
		zap.String("activity_type", args.ActivityType),
	)
	return string(out), nil
}
