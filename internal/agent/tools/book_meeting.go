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

	"github.com/bengobox/marketflow-ai/internal/guardrail"
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
		http:       serviceHTTP(15 * time.Second),
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
	if msg := checkBooking(ctx, args.AttendeeEmail, args.StartsAt, &args.DurationMinutes, &args.MeetingType, time.Now()); msg != "" {
		return fmt.Sprintf(`{"error":%q}`, msg), nil
	}
	starts, _ := time.Parse(time.RFC3339, args.StartsAt)
	args.Notes = trimText(args.Notes, 500)

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
		t.logger.Warn("book_meeting api error", zap.Int("status", resp.StatusCode))
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
	t.logger.Info("meeting booked via agent tool", zap.String("tenant", tenantID.String()))
	return string(out), nil
}

var (
	bookingEmailRe = regexp.MustCompile(`^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`)
	meetingTypes   = map[string]bool{"product_demo": true, "business_opportunity": true, "follow_up": true, "onboarding": true}
)

// checkBooking validates a booking before anything is sent, and returns a message for the model
// when it cannot go ahead. The invite goes to an outside address, so on the public widget the
// email must be one the visitor typed (guardrail.Actor.VisitorWrote): an address found in a web
// page or tool result is refused. The time must be in the next 90 days and the length 15 to 120
// minutes; duration and type get their defaults.
func checkBooking(ctx context.Context, email, startsAt string, duration *int, meetingType *string, now time.Time) string {
	email = strings.TrimSpace(email)
	if email == "" || startsAt == "" {
		return "attendee_email and starts_at are required"
	}
	if !bookingEmailRe.MatchString(email) {
		return "attendee_email is not a valid email address"
	}
	if a, ok := guardrail.ActorFrom(ctx); ok && !a.VisitorWrote(email) {
		return "ask the visitor to type the email address the invite should go to"
	}
	starts, err := time.Parse(time.RFC3339, startsAt)
	if err != nil {
		return "starts_at must be an ISO-8601 (RFC 3339) date and time"
	}
	if starts.Before(now.Add(-5*time.Minute)) || starts.After(now.AddDate(0, 0, 90)) {
		return "starts_at must be in the next 90 days"
	}
	if *duration <= 0 {
		*duration = 30
	}
	if *duration < 15 || *duration > 120 {
		return "duration_minutes must be between 15 and 120"
	}
	if *meetingType == "" {
		*meetingType = "product_demo"
	}
	if !meetingTypes[*meetingType] {
		return "meeting_type must be product_demo, business_opportunity, follow_up or onboarding"
	}
	return ""
}
