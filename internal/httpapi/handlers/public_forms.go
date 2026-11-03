package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"
)

// PublicFormsHandler is the Vera widget's one public edge for its in-chat forms. The widget
// runs on customer sites whose origins only this service's CORS admits, so the forms post
// here and each is forwarded, unchanged in meaning, to the service that owns the record:
//
//   - integration requests to auth-api (POST /api/v1/integration-requests)
//   - visitor email codes to auth-api (POST /api/v1/auth/public-otp/{send,verify})
//   - support tickets to ticketing-api (POST /api/v1/{tenant}/public/tickets, X-OTP-Token)
//   - leads, call bookings and messages to marketflow-api (POST /api/v1/public/leads/{slug})
//
// Nothing is stored here. The visitor's address goes along as X-Real-IP, so each owner's own
// per-IP limits still apply to the visitor and not to this pod. The /public route group adds
// this service's per-IP and per-tenant limits in front.
type PublicFormsHandler struct {
	authURL       string
	ticketingURL  string
	marketflowURL string
	http          *http.Client
	logger        *zap.Logger
}

// NewPublicFormsHandler builds the handler from the in-cluster service URLs.
func NewPublicFormsHandler(authURL, ticketingURL, marketflowURL string, logger *zap.Logger) *PublicFormsHandler {
	return &PublicFormsHandler{
		authURL:       strings.TrimSuffix(authURL, "/"),
		ticketingURL:  strings.TrimSuffix(ticketingURL, "/"),
		marketflowURL: strings.TrimSuffix(marketflowURL, "/"),
		http:          &http.Client{Timeout: 10 * time.Second},
		logger:        logger.Named("public-forms"),
	}
}

const (
	maxFormBody     = 32 << 10
	maxUpstreamBody = 64 << 10
)

var widgetSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// IntegrationRequest handles POST /api/v1/public/integration-requests.
func (h *PublicFormsHandler) IntegrationRequest(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, h.authURL+"/api/v1/integration-requests", nil)
}

// SendCode handles POST /api/v1/public/otp/send: a code to the visitor's email before a ticket.
func (h *PublicFormsHandler) SendCode(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, h.authURL+"/api/v1/auth/public-otp/send", nil)
}

// VerifyCode handles POST /api/v1/public/otp/verify and returns the otp_token for the ticket.
func (h *PublicFormsHandler) VerifyCode(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, h.authURL+"/api/v1/auth/public-otp/verify", nil)
}

// Ticket handles POST /api/v1/public/tickets for the widget's tenant (X-Tenant-ID slug). The
// visitor's X-OTP-Token goes along; ticketing-api trades it for the verified email.
func (h *PublicFormsHandler) Ticket(w http.ResponseWriter, r *http.Request) {
	slug, ok := widgetSlug(w, r)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.Header.Get("X-OTP-Token"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "verify your email first")
		return
	}
	h.forward(w, r, h.ticketingURL+"/api/v1/"+url.PathEscape(slug)+"/public/tickets",
		map[string]string{"X-OTP-Token": token})
}

// Lead handles POST /api/v1/public/leads for the widget's tenant (X-Tenant-ID slug).
func (h *PublicFormsHandler) Lead(w http.ResponseWriter, r *http.Request) {
	slug, ok := widgetSlug(w, r)
	if !ok {
		return
	}
	h.forward(w, r, h.marketflowURL+"/api/v1/public/leads/"+url.PathEscape(slug), nil)
}

func widgetSlug(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Tenant-ID")))
	if !widgetSlugRe.MatchString(slug) {
		writeError(w, http.StatusBadRequest, "X-Tenant-ID must be the tenant slug")
		return "", false
	}
	return slug, true
}

// forward sends the visitor's JSON object to target and copies the owner's status and JSON
// answer back. Only a JSON object is accepted, and only the named headers go upstream.
func (h *PublicFormsHandler) forward(w http.ResponseWriter, r *http.Request, target string, headers map[string]string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFormBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "form is too large")
		return
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		writeError(w, http.StatusBadRequest, "form must be a JSON object")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "form could not be sent")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if ip := remoteIP(r); ip != "" {
		req.Header.Set("X-Real-IP", ip)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.http.Do(req)
	if err != nil {
		h.logger.Warn("form owner unreachable", zap.String("target", redactTarget(target)), zap.Error(err))
		writeError(w, http.StatusBadGateway, "we could not send this right now, please try again")
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if resp.StatusCode >= 500 {
		h.logger.Warn("form owner failed", zap.String("target", redactTarget(target)), zap.Int("status", resp.StatusCode))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if json.Valid(out) {
		_, _ = w.Write(out)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(resp.StatusCode)})
}

// remoteIP is the visitor's address as set by ratelimit.TrustedRealIP on this router.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}

// redactTarget drops the query string so logs never carry tokens.
func redactTarget(target string) string {
	if i := strings.IndexByte(target, '?'); i >= 0 {
		return target[:i]
	}
	return target
}
