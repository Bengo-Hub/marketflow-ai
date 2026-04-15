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

// BookMeeting is the agentic-RAG tool that schedules a meeting for a lead.
//
// The agent collects the attendee email/name + ISO-8601 datetime from the
// conversation, calls this tool, and the tool POSTs to marketflow-api
// /api/v1/meetings with the platform X-API-Key. marketflow-api then pushes
// the event to the tenant's Google Calendar (when linked) and returns the
// meeting URL / event link that the agent can quote back to the user.
//
// Never exposes calendar credentials — the tenant's Google refresh_token
// lives platform-side in the ProviderSetting store.
type BookMeeting struct {
	apiBaseURL string
	apiKey     string
	http       *http.Client
	logger     *zap.Logger
}

// BookMeetingConfig configures the tool.
type BookMeetingConfig struct {
	APIBaseURL string
	APIKey     string
	Logger     *zap.Logger
}

func NewBookMeeting(cfg BookMeetingConfig) *BookMeeting {
	return &BookMeeting{
		apiBaseURL: strings.TrimSuffix(cfg.APIBaseURL, "/"),
		apiKey:     cfg.APIKey,
		http:       &http.Client{Timeout: 15 * time.Second},
		logger:     cfg.Logger.Named("tool.book_meeting"),
	}
}

func (*BookMeeting) Name() string { return "book_meeting" }

func (*BookMeeting) Description() string {
	return "Books a meeting with the FBO for the lead. The meeting is created in marketflow-api and pushed to the tenant's Google Calendar (if linked). A Google Meet link is generated automatically unless meeting_url is provided. Call this when the user asks to schedule a call / demo / consultation and you have collected: their email, name, a desired start datetime (ISO 8601), and the meeting topic."
}

func (*BookMeeting) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"attendee_email": map[string]any{
				"type":        "string",
				"description": "Lead's email address for the calendar invite",
			},
			"attendee_name": map[string]any{
				"type":        "string",
				"description": "Lead's full name",
			},
			"starts_at": map[string]any{
				"type":        "string",
				"description": "ISO-8601 datetime in UTC, e.g. 2026-04-20T15:00:00Z",
			},
			"duration_minutes": map[string]any{
				"type":        "integer",
				"description": "Meeting length; default 30",
				"default":     30,
			},
			"meeting_type": map[string]any{
				"type":        "string",
				"description": "product_demo | business_opportunity | follow_up | onboarding",
				"default":     "product_demo",
			},
			"notes": map[string]any{
				"type":        "string",
				"description": "Brief description / agenda that appears in the calendar invite",
			},
			"lead_id": map[string]any{
				"type":        "string",
				"description": "Optional MarketFlow lead UUID to attach the meeting to",
			},
		},
		"required": []string{"attendee_email", "starts_at"},
	}
}

func (t *BookMeeting) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if t.apiBaseURL == "" {
		return "", fmt.Errorf("book_meeting: marketflow-api URL not configured")
	}
	if t.apiKey == "" {
		return "", fmt.Errorf("book_meeting: platform X-API-Key not configured")
	}

	var args struct {
		AttendeeEmail   string `json:"attendee_email"`
		AttendeeName    string `json:"attendee_name"`
		StartsAt        string `json:"starts_at"`
		DurationMinutes int    `json:"duration_minutes"`
		MeetingType     string `json:"meeting_type"`
		Notes           string `json:"notes"`
		LeadID          string `json:"lead_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.AttendeeEmail == "" || args.StartsAt == "" {
		return "", fmt.Errorf("attendee_email and starts_at are required")
	}
	starts, err := time.Parse(time.RFC3339, args.StartsAt)
	if err != nil {
		return "", fmt.Errorf("starts_at must be ISO-8601 RFC3339: %w", err)
	}
	if args.DurationMinutes <= 0 {
		args.DurationMinutes = 30
	}
	if args.MeetingType == "" {
		args.MeetingType = "product_demo"
	}

	body := map[string]any{
		"scheduled_at":     starts.UTC().Format(time.RFC3339),
		"duration_minutes": args.DurationMinutes,
		"meeting_type":     args.MeetingType,
		"attendee_email":   args.AttendeeEmail,
	}
	if args.AttendeeName != "" {
		body["attendee_name"] = args.AttendeeName
	}
	if args.Notes != "" {
		body["notes"] = args.Notes
	}
	if args.LeadID != "" {
		if _, err := uuid.Parse(args.LeadID); err == nil {
			body["lead_id"] = args.LeadID
		}
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.apiBaseURL+"/api/v1/meetings", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.apiKey)
	req.Header.Set("X-Tenant-ID", tenantID.String())

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("book_meeting http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.logger.Warn("book_meeting api error",
			zap.Int("status", resp.StatusCode), zap.ByteString("body", respBody))
		return "", fmt.Errorf("marketflow-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	// Return a summarised JSON for the LLM — it will quote the meet_url /
	// google_event_link back to the user.
	var m map[string]any
	_ = json.Unmarshal(respBody, &m)
	summary := map[string]any{
		"success":        true,
		"meeting_id":     m["id"],
		"scheduled_at":   m["scheduled_at"],
		"meeting_url":    m["meeting_url"],
		"calendar_link":  m["google_event_link"],
		"duration_mins":  m["duration_minutes"],
	}
	out, _ := json.Marshal(summary)
	t.logger.Info("meeting booked via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("attendee", args.AttendeeEmail),
	)
	return string(out), nil
}
