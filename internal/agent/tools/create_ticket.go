package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/support"
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
		http:         serviceHTTP(15 * time.Second),
		logger:       cfg.Logger.Named("tool.create_ticket"),
	}
}

func (*CreateTicket) Name() string { return "create_ticket" }

// ticketPriority maps the model's priority to a ticketing-api priority (low, normal, high,
// urgent); anything else, including the older "medium", is normal.
func ticketPriority(p string) string {
	switch p {
	case "low", "high", "urgent":
		return p
	default:
		return "normal"
	}
}

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
				"enum":        []string{"low", "normal", "high", "urgent"},
				"description": "Ticket priority; default 'normal'",
				"default":     "normal",
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
	args.Priority = ticketPriority(args.Priority)
	if args.Category == "" {
		args.Category = "general"
	}

	// ticketing-api internal create: the tenant travels in X-Tenant-ID, the title is "subject",
	// and a Vera chat ticket is on the chat channel.
	body := support.TicketRequest{
		Subject:        args.Title,
		Description:    args.Description,
		Priority:       args.Priority,
		Category:       args.Category,
		Source:         "vera_ai",
		Channel:        "chat",
		RequesterName:  args.RequesterName,
		RequesterEmail: args.RequesterEmail,
	}

	// The shared support client (internal/support) is the one ticketing-api caller in this
	// service. Without a source_ref the create is not idempotent, so it is sent once.
	client := &support.Ticketing{BaseURL: t.ticketingURL, ServiceKey: t.serviceKey, HTTP: t.http}
	created, err := client.Create(ctx, tenantID, body)
	if err != nil {
		t.logger.Warn("create_ticket api error", zap.Error(err))
		return "", fmt.Errorf("create_ticket: %w", err)
	}
	ticketRef := created.TicketNumber

	// The reply promises no response time: the tenant's SLA policy, not this tool, sets it.
	summary := map[string]any{
		"success":    true,
		"ticket_id":  created.ID,
		"ticket_ref": ticketRef,
		"status":     created.Status,
		"message":    fmt.Sprintf("Ticket %s created. Our team will follow up on it.", ticketRef),
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
