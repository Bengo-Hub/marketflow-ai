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

// CreateTicket is the agent tool that creates a support ticket in the
// ticketing-service via S2S call. It is triggered when the agent cannot
// resolve a support request or when the user explicitly asks for a ticket.
type CreateTicket struct {
	ticketingURL string
	serviceKey   string
	http         *http.Client
	logger       *zap.Logger
}

// CreateTicketConfig configures the tool.
type CreateTicketConfig struct {
	TicketingURL string // internal k8s URL for ticketing-service
	ServiceKey   string // INTERNAL_SERVICE_KEY (X-API-Key header)
	Logger       *zap.Logger
}

func NewCreateTicket(cfg CreateTicketConfig) *CreateTicket {
	return &CreateTicket{
		ticketingURL: strings.TrimSuffix(cfg.TicketingURL, "/"),
		serviceKey:   cfg.ServiceKey,
		http:         &http.Client{Timeout: 15 * time.Second},
		logger:       cfg.Logger.Named("tool.create_ticket"),
	}
}

func (*CreateTicket) Name() string { return "create_ticket" }

func (*CreateTicket) Description() string {
	return "Creates a support ticket in the Codevertex ticketing system. Use this when: (1) a user explicitly asks to open/create/submit a ticket, (2) you cannot resolve a technical issue from the knowledge base, or (3) the intent is classified as 'escalate' or 'support_request'. Always collect a title, description, and optionally the requester's name and email before calling this tool."
}

func (*CreateTicket) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "Short, descriptive title for the ticket (max 120 chars)",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Detailed description of the issue or request",
			},
			"priority": map[string]any{
				"type":        "string",
				"enum":        []string{"low", "medium", "high", "urgent"},
				"description": "Ticket priority; default 'medium'",
				"default":     "medium",
			},
			"category": map[string]any{
				"type":        "string",
				"enum":        []string{"billing", "technical", "sales", "general", "course_support"},
				"description": "Ticket category; default 'general'",
				"default":     "general",
			},
			"requester_name": map[string]any{
				"type":        "string",
				"description": "Full name of the person requesting support (optional)",
			},
			"requester_email": map[string]any{
				"type":        "string",
				"description": "Email address of the requester for follow-up (optional)",
			},
		},
		"required": []string{"title", "description"},
	}
}

func (t *CreateTicket) Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error) {
	if t.ticketingURL == "" {
		return "", fmt.Errorf("create_ticket: ticketing service URL not configured")
	}
	if t.serviceKey == "" {
		return "", fmt.Errorf("create_ticket: internal service key not configured")
	}

	var args struct {
		Title          string `json:"title"`
		Description    string `json:"description"`
		Priority       string `json:"priority"`
		Category       string `json:"category"`
		RequesterName  string `json:"requester_name"`
		RequesterEmail string `json:"requester_email"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if args.Title == "" || args.Description == "" {
		return "", fmt.Errorf("title and description are required")
	}
	if len(args.Title) > 120 {
		args.Title = args.Title[:120]
	}
	if args.Priority == "" {
		args.Priority = "medium"
	}
	if args.Category == "" {
		args.Category = "general"
	}

	body := map[string]any{
		"title":       args.Title,
		"description": args.Description,
		"priority":    args.Priority,
		"category":    args.Category,
		"source":      "vera_ai",
		"tenant_id":   tenantID.String(),
	}
	if args.RequesterName != "" {
		body["requester_name"] = args.RequesterName
	}
	if args.RequesterEmail != "" {
		body["requester_email"] = args.RequesterEmail
	}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.ticketingURL+"/api/v1/internal/tickets", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.serviceKey)

	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("create_ticket http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		t.logger.Warn("create_ticket api error",
			zap.Int("status", resp.StatusCode),
			zap.ByteString("body", respBody))
		return "", fmt.Errorf("ticketing-api %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var m map[string]any
	_ = json.Unmarshal(respBody, &m)

	ticketRef, _ := m["ticket_ref"].(string)
	if ticketRef == "" {
		if id, ok := m["id"].(string); ok {
			ticketRef = "CVX-" + id[:8]
		} else {
			ticketRef = "CVX-???"
		}
	}

	summary := map[string]any{
		"success":        true,
		"ticket_id":      m["id"],
		"ticket_ref":     ticketRef,
		"status":         m["status"],
		"message":        fmt.Sprintf("Ticket %s created. Our team will respond within 2–4 business hours.", ticketRef),
	}
	if args.RequesterEmail != "" {
		summary["confirmation_sent_to"] = args.RequesterEmail
	}

	out, _ := json.Marshal(summary)
	t.logger.Info("ticket created via agent tool",
		zap.String("tenant", tenantID.String()),
		zap.String("ref", ticketRef),
		zap.String("priority", args.Priority),
	)
	return string(out), nil
}
