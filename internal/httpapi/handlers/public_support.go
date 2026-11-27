package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/support"
)

// SupportRouter is the part of support.Router the handler uses.
type SupportRouter interface {
	Settings(ctx context.Context, slug string) support.Settings
	Route(ctx context.Context, req support.Request, s support.Settings) support.Outcome
	ContactOnly(ctx context.Context, slug string) support.Outcome
}

// EmailCodeValidator trades the widget's X-OTP-Token for the verified email (support.OTPValidator).
type EmailCodeValidator interface {
	Validate(ctx context.Context, token string) (string, error)
}

// PublicSupportHandler is the Vera widget's support form (owner, 2026-11-28). The visitor
// verifies their email first, exactly as for the old ticket form, then this route sends the
// request where it belongs (support.Router): a Codevertex platform ticket for a technical
// problem, a ticket in the business's own queue when its plan includes ticketing, or an email
// and WhatsApp notice to the business's team. A tenant with the routing switched off keeps the
// old behaviour: the ticket opens on its own desk through ticketing-api's public route.
//
// The model never calls this: like every widget write it stays outside the public tool
// allowlist, and the visitor checks the form and taps Send.
type PublicSupportHandler struct {
	forms   *PublicFormsHandler
	router  SupportRouter
	codes   EmailCodeValidator
	resolve func(ctx context.Context, raw string) (uuid.UUID, bool)
	logger  *zap.Logger
}

// NewPublicSupportHandler wires the support form. resolve maps the widget's slug to the tenant's
// id (the chat's own resolver).
func NewPublicSupportHandler(forms *PublicFormsHandler, router SupportRouter, codes EmailCodeValidator,
	resolve func(ctx context.Context, raw string) (uuid.UUID, bool), logger *zap.Logger) *PublicSupportHandler {
	return &PublicSupportHandler{forms: forms, router: router, codes: codes, resolve: resolve, logger: logger.Named("public-support")}
}

// supportForm is the widget's body for POST /api/v1/public/support.
type supportForm struct {
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	Page        string `json:"page"`
}

// Submit handles POST /api/v1/public/support for the widget's tenant (X-Tenant-ID slug) with
// the visitor's X-OTP-Token. It answers 200 with a support.Outcome whenever the form was valid,
// including when nothing could be sent (route contact_only), so the chat always has something
// to say. A tenant with routing off gets ticketing-api's own answer, as the old /tickets did.
// @Summary Send a visitor's support request where it belongs
// @Tags Public
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant slug"
// @Param X-OTP-Token header string true "The verified email code's token"
// @Success 200 {object} support.Outcome
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Router /public/support [post]
func (h *PublicSupportHandler) Submit(w http.ResponseWriter, r *http.Request) {
	slug, ok := widgetSlug(w, r)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-OTP-Token"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "verify your email first")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFormBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "form is too large")
		return
	}
	var form supportForm
	if json.Unmarshal(raw, &form) != nil {
		writeError(w, http.StatusBadRequest, "form must be a JSON object")
		return
	}
	form.Name = strings.TrimSpace(form.Name)
	form.Phone = strings.TrimSpace(form.Phone)
	form.Description = strings.TrimSpace(form.Description)
	switch {
	case form.Description == "":
		writeError(w, http.StatusBadRequest, "describe what you need help with")
		return
	case utf8.RuneCountInString(form.Description) > 4000, utf8.RuneCountInString(form.Name) > 120,
		utf8.RuneCountInString(form.Phone) > 32, utf8.RuneCountInString(form.Subject) > 200, len(form.Page) > 500:
		writeError(w, http.StatusBadRequest, "one of the fields is too long")
		return
	}
	if form.Page != "" {
		if u, err := url.Parse(form.Page); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			form.Page = ""
		}
	}

	tenantID, known := uuid.Nil, false
	if h.resolve != nil {
		tenantID, known = h.resolve(r.Context(), slug)
	}
	if !known {
		writeError(w, http.StatusNotFound, "unknown business")
		return
	}

	settings := h.router.Settings(r.Context(), slug)
	if !settings.Enabled {
		h.legacyTicket(w, r, slug, token, form)
		return
	}

	email, err := h.codes.Validate(r.Context(), token)
	switch {
	case errors.Is(err, support.ErrOTPInvalid):
		writeError(w, http.StatusUnauthorized, "your email code has expired, please verify your email again")
		return
	case err != nil:
		// auth-api did not answer: the request cannot be tied to a verified email, so nothing is
		// sent, but the visitor still gets the business's own contact details at once.
		h.logger.Warn("support: email code could not be checked", zap.String("tenant", slug), zap.Error(err))
		writeJSON(w, http.StatusOK, h.router.ContactOnly(r.Context(), slug))
		return
	}

	out := h.router.Route(r.Context(), support.Request{
		TenantID: tenantID, TenantSlug: slug, RequestID: uuid.NewString(),
		Name: form.Name, Email: email, Phone: form.Phone,
		Subject: strings.TrimSpace(form.Subject), Message: form.Description,
		Priority: form.Priority, Page: form.Page,
	}, settings)
	writeJSON(w, http.StatusOK, out)
}

// legacyTicket is the old ticket form's path for a tenant with routing off: the same body the
// widget used to post to /tickets, forwarded to ticketing-api's public route with the code.
func (h *PublicSupportHandler) legacyTicket(w http.ResponseWriter, r *http.Request, slug, token string, f supportForm) {
	subject := strings.TrimSpace(f.Subject)
	if subject == "" {
		subject = support.SubjectFrom(f.Description)
	}
	desc := f.Description
	if f.Priority != "" {
		desc += "\n\nPriority given by the customer: " + f.Priority
	}
	if f.Phone != "" {
		desc += "\n\nPhone: " + f.Phone
	}
	if u, err := url.Parse(f.Page); err == nil && u.Host != "" {
		desc += "\n\nSent from the chat on " + u.Host + "."
	}
	body, _ := json.Marshal(map[string]string{"name": f.Name, "subject": subject, "description": desc})
	h.forms.forwardBody(w, r, h.forms.ticketingURL+"/api/v1/"+url.PathEscape(slug)+"/public/tickets", body,
		map[string]string{"X-OTP-Token": token})
}
