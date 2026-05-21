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

// CreateTask creates a follow-up task linked to a CRM entity via S2S call
// to marketflow-api.
type CreateTask struct {
	apiBaseURL string
	serviceKey string
	http       *http.Client
	logger     *zap.Logger
}

// CreateTaskConfig configures the tool.
type CreateTaskConfig struct {
	APIBaseURL string
	ServiceKey string
	Logger     *zap.Logger
}

func NewCreateTask(cfg CreateTaskConfig) *CreateTask {
	return &CreateTask{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		serviceKey: cfg.ServiceKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.create_task"),
	}
}

func (*CreateTask) Name() string { return "create_task" }

func (*CreateTask) Description() string {
	return "Creates a follow-up task linked to a CRM entity (lead, contact, or deal). Use this to schedule reminders, call-backs, and action items for the FBO."
}

func (*CreateTask) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entity_type": map[string]any{
				"type":        "string",
				"description": "Type of CRM entity to link the task to (lead, contact, deal)",
			},
			"entity_id": map[string]any{
				"type":        "string",
				"description": "ID of the entity to link the task to",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Title/name of the task",
			},
			"due_at": map[string]any{
				"type":        "string",
				"description": "Due date and time in RFC3339 format (e.g. '2024-01-15T10:00:00Z')",
			},
			"priority": map[string]any{
				"type":        "string",
				"enum":        []string{"low", "normal", "high", "urgent"},
				"description": "Task priority (default 'normal')",
				"default":     "normal",
			},
			"notes": map[string]any{
				"type":        "string",
				"description": "Additional notes about the task",
			},
		},
		"required": []string{"entity_type", "entity_id", "title"},
	}
}

func (t *CreateTask) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	var args struct {
		EntityType string `json:"entity_type"`
		EntityID   string `json:"entity_id"`
		Title      string `json:"title"`
		DueAt      string `json:"due_at"`
		Priority   string `json:"priority"`
		Notes      string `json:"notes"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.EntityType == "" || args.EntityID == "" || args.Title == "" {
		return "", fmt.Errorf("entity_type, entity_id, and title are required")
	}
	if args.Priority == "" {
		args.Priority = "normal"
	}

	body := map[string]any{
		"entity_type": args.EntityType,
		"entity_id":   args.EntityID,
		"title":       args.Title,
		"priority":    args.Priority,
	}
	if args.DueAt != "" {
		body["due_at"] = args.DueAt
	}
	if args.Notes != "" {
		body["notes"] = args.Notes
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.apiBaseURL+"/api/v1/tasks", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create_task http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("create_task api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var m map[string]any
	_ = json.Unmarshal(respBody, &m)

	taskID, _ := m["id"].(string)
	out, _ := json.Marshal(map[string]any{
		"success": true,
		"task_id": taskID,
		"title":   args.Title,
		"due_at":  args.DueAt,
		"message": fmt.Sprintf("Task '%s' created successfully.", args.Title),
	})
	t.logger.Info("task created via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("task_id", taskID),
		zap.String("title", args.Title),
	)
	return string(out), nil
}
