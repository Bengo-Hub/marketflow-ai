package support

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxAnswerBody = 64 << 10

// ── ticketing-api ────────────────────────────────────────────────────────────

// SourceRef is ticketing-api's source_ref: the record a ticket was raised for. With it the
// internal create is idempotent on (tenant, source, type, id), which is what makes a retry safe.
type SourceRef struct {
	Type   string     `json:"type"`
	ID     string     `json:"id"`
	Parent *ParentRef `json:"parent,omitempty"`
}

// ParentRef is source_ref.parent, context only.
type ParentRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// TicketRequest is the body of ticketing-api POST /api/v1/internal/tickets (its
// docs/integrations.md, "Internal create"). The tenant travels in X-Tenant-ID, never the body.
type TicketRequest struct {
	Subject        string     `json:"subject"`
	Description    string     `json:"description"`
	Source         string     `json:"source"`
	Priority       string     `json:"priority,omitempty"`
	Channel        string     `json:"channel,omitempty"`
	Category       string     `json:"category,omitempty"`
	SourceRef      *SourceRef `json:"source_ref,omitempty"`
	RequesterName  string     `json:"requester_name,omitempty"`
	RequesterEmail string     `json:"requester_email,omitempty"`
	RequesterPhone string     `json:"requester_phone,omitempty"`
}

// Ticket is ticketing-api's answer (201 new, 200 an existing ticket for the same source_ref).
type Ticket struct {
	ID           string `json:"id"`
	TicketNumber string `json:"ticket_number"`
	Status       string `json:"status"`
}

// Ticketing creates tickets through ticketing-api's internal create.
type Ticketing struct {
	BaseURL    string
	ServiceKey string
	HTTP       *http.Client
	Backoff    Backoff
}

// Create opens a ticket on tenantID's desk. A request with a SourceRef is retried with backoff;
// one without is sent once, since a repeat would open a second ticket.
func (t *Ticketing) Create(ctx context.Context, tenantID uuid.UUID, in TicketRequest) (Ticket, error) {
	if t == nil || t.BaseURL == "" || t.ServiceKey == "" {
		return Ticket{}, permanent(fmt.Errorf("ticketing-api is not configured"))
	}
	if tenantID == uuid.Nil {
		return Ticket{}, permanent(fmt.Errorf("ticketing: tenant id required"))
	}
	if strings.TrimSpace(in.Subject) == "" || strings.TrimSpace(in.Description) == "" || in.Source == "" {
		return Ticket{}, permanent(fmt.Errorf("ticketing: subject, description and source are required"))
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return Ticket{}, permanent(err)
	}
	b := t.Backoff
	if b.Attempts == 0 {
		b = DefaultBackoff
	}
	if in.SourceRef == nil {
		b.Attempts = 1
	}
	var out Ticket
	err = b.do(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimSuffix(t.BaseURL, "/")+"/api/v1/internal/tickets", bytes.NewReader(payload))
		if err != nil {
			return permanent(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", t.ServiceKey)
		req.Header.Set("X-Tenant-ID", tenantID.String())
		resp, err := client(t.HTTP).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBody))
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return &statusError{Service: "ticketing-api", Status: resp.StatusCode, Body: clip(string(body), 200)}
		}
		var created Ticket
		if json.Unmarshal(body, &created) != nil || created.TicketNumber == "" {
			return permanent(fmt.Errorf("ticketing-api: unexpected answer: %s", clip(string(body), 200)))
		}
		out = created
		return nil
	})
	return out, err
}

// ── notifications-api ───────────────────────────────────────────────────────

// MessageRequest is notifications-api's POST /api/v1/{tenantId}/notifications/messages body
// (its CreateMessageRequest). The handler decodes without strict fields, so a wrong name is
// dropped silently: these names are the contract (template, to[], data, metadata).
type MessageRequest struct {
	Channel  string         `json:"channel"`
	Tenant   string         `json:"tenant"`
	Template string         `json:"template"`
	Data     map[string]any `json:"data"`
	To       []string       `json:"to"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Notifier queues messages through notifications-api.
type Notifier struct {
	BaseURL    string
	ServiceKey string
	HTTP       *http.Client
	Backoff    Backoff
}

// Send queues one message as tenant. idempotencyKey makes a retry safe (notifications-api keeps
// a client key 24 hours per tenant and answers "duplicate" to a repeat), so it is required.
func (n *Notifier) Send(ctx context.Context, tenant string, msg MessageRequest, idempotencyKey string) error {
	if n == nil || n.BaseURL == "" || n.ServiceKey == "" {
		return permanent(fmt.Errorf("notifications-api is not configured"))
	}
	if tenant == "" || idempotencyKey == "" || len(msg.To) == 0 || msg.Template == "" {
		return permanent(fmt.Errorf("notifications: tenant, idempotency key, recipient and template are required"))
	}
	msg.Tenant = tenant
	payload, err := json.Marshal(msg)
	if err != nil {
		return permanent(err)
	}
	b := n.Backoff
	if b.Attempts == 0 {
		b = DefaultBackoff
	}
	return b.do(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimSuffix(n.BaseURL, "/")+"/api/v1/"+url.PathEscape(tenant)+"/notifications/messages", bytes.NewReader(payload))
		if err != nil {
			return permanent(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", n.ServiceKey)
		req.Header.Set("X-Tenant-ID", tenant)
		req.Header.Set("Idempotency-Key", idempotencyKey)
		resp, err := client(n.HTTP).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBody))
		if resp.StatusCode >= 300 {
			return &statusError{Service: "notifications-api", Status: resp.StatusCode, Body: clip(string(body), 200)}
		}
		return nil
	})
}

// ── auth-api ────────────────────────────────────────────────────────────────

// Admin is one of a tenant's owners or admins with a phone.
type Admin struct {
	FirstName string
	Phone     string
}

// AdminReach reads a tenant's owner and admin phones from auth-api's
// GET /api/v1/s2s/tenants/reach?purpose=broadcast&tenant_ids=<id> (X-API-Key), the list
// notifications-api's broadcasts and gateway alerts read.
type AdminReach struct {
	BaseURL    string
	ServiceKey string
	HTTP       *http.Client
	Backoff    Backoff
}

// Admins returns up to max owner and admin phones, verified ones first.
func (a *AdminReach) Admins(ctx context.Context, tenantID uuid.UUID, max int) ([]Admin, error) {
	if a == nil || a.BaseURL == "" || a.ServiceKey == "" {
		return nil, permanent(fmt.Errorf("auth-api is not configured"))
	}
	q := url.Values{}
	q.Set("purpose", "broadcast")
	q.Set("tenant_ids", tenantID.String())
	q.Set("include_demo", "true")
	q.Set("limit", "5")
	var answer struct {
		Data []struct {
			Phones []struct {
				Address   string `json:"address"`
				Source    string `json:"source"`
				Verified  bool   `json:"verified"`
				FirstName string `json:"first_name"`
			} `json:"phones"`
		} `json:"data"`
	}
	b := a.Backoff
	if b.Attempts == 0 {
		b = DefaultBackoff
	}
	err := b.do(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			strings.TrimSuffix(a.BaseURL, "/")+"/api/v1/s2s/tenants/reach?"+q.Encode(), nil)
		if err != nil {
			return permanent(err)
		}
		req.Header.Set("X-API-Key", a.ServiceKey)
		resp, err := client(a.HTTP).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBody))
		if resp.StatusCode != http.StatusOK {
			return &statusError{Service: "auth-api", Status: resp.StatusCode, Body: clip(string(body), 200)}
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			return permanent(fmt.Errorf("auth-api reach: %w", err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var verified, other []Admin
	seen := map[string]bool{}
	for _, t := range answer.Data {
		for _, p := range t.Phones {
			phone := strings.TrimSpace(p.Address)
			if phone == "" || seen[phone] || (p.Source != "owner" && p.Source != "admin") {
				continue
			}
			seen[phone] = true
			ad := Admin{FirstName: strings.TrimSpace(p.FirstName), Phone: phone}
			if p.Verified {
				verified = append(verified, ad)
			} else {
				other = append(other, ad)
			}
		}
	}
	out := append(verified, other...)
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// ErrOTPInvalid is an unknown, expired or used visitor code.
var ErrOTPInvalid = fmt.Errorf("the email code is not valid any more")

// OTPValidator trades the widget's otp_token for the verified email through auth-api's
// GET /api/v1/s2s/otp/validate?token= (X-API-Key), the call ticketing-api makes for its public
// tickets. A token works once, so this call is never retried.
type OTPValidator struct {
	BaseURL    string
	ServiceKey string
	HTTP       *http.Client
}

// Validate returns the verified email for token.
func (o *OTPValidator) Validate(ctx context.Context, token string) (string, error) {
	if o == nil || o.BaseURL == "" || o.ServiceKey == "" {
		return "", permanent(fmt.Errorf("auth-api is not configured"))
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(o.BaseURL, "/")+"/api/v1/s2s/otp/validate?token="+url.QueryEscape(token), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", o.ServiceKey)
	resp, err := client(o.HTTP).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{Service: "auth-api", Status: resp.StatusCode}
	}
	var body struct {
		Valid bool   `json:"valid"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswerBody)).Decode(&body); err != nil {
		return "", err
	}
	if !body.Valid || strings.TrimSpace(body.Email) == "" {
		return "", ErrOTPInvalid
	}
	return strings.TrimSpace(body.Email), nil
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
