package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/assistgate"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/team"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// WithGate turns on the WhatsApp assistant route.
func (h *OSHandler) WithGate(g *assistgate.Gate) *OSHandler {
	h.gate = g
	return h
}

type whatsAppAskRequest struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	UserID     string `json:"user_id"`
	// PortalURL is MarketFlow's web address, for the links in replies.
	PortalURL string `json:"portal_url"`
	Text      string `json:"text"`
	// SessionActive is true when the sender talked to the assistant in the last 30 minutes.
	SessionActive bool `json:"session_active"`
}

// WhatsAppAskResponse is what marketflow-api's worker gets back. Reply is the text to send on
// WhatsApp; it is empty when the message is not for the assistant (it stays in the inbox).
type WhatsAppAskResponse struct {
	Pass         bool               `json:"pass"`
	Verdict      assistgate.Verdict `json:"verdict"`
	NeedsCredits bool               `json:"needs_credits,omitempty"`
	Reply        string             `json:"reply,omitempty"`
	Route        string             `json:"route,omitempty"`
	RunID        string             `json:"run_id,omitempty"`
}

// WhatsAppAsk handles POST /api/v1/internal/os/whatsapp-ask (S2S, marketflow-worker). The worker
// has already matched the sender to a verified user of this tenant. Here the text passes the
// gate (rules, then Jev or the free fallback model), the tenant must have AI credits or an
// unlimited plan, and then the same Marketing Manager turn as the portal runs.
func (h *OSHandler) WhatsAppAsk(w http.ResponseWriter, r *http.Request) {
	var req whatsAppAskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if h.gate == nil {
		writeError(w, http.StatusServiceUnavailable, "the WhatsApp assistant is not configured")
		return
	}
	ctx := r.Context()
	v := h.gate.Check(ctx, req.Text, req.SessionActive)
	out := WhatsAppAskResponse{Pass: v.Pass, Verdict: v}
	if !v.Pass {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if blocked, reply := rag.ScreenInput(req.Text); blocked {
		out.Reply, out.Route = reply, "blocked"
		writeJSON(w, http.StatusOK, out)
		return
	}
	if ok, err := h.hasCredits(ctx, tenantID); err != nil {
		h.logger.Warn("whatsapp ask: credit check failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		out.Reply = "I could not check your AI credits just now. Please try again in a few minutes."
		writeJSON(w, http.StatusOK, out)
		return
	} else if !ok {
		out.NeedsCredits = true
		out.Reply = "Your AI credits are used up. Top up from " + link(req.PortalURL, req.TenantSlug, "/settings/billing") + " and send your request again."
		writeJSON(w, http.StatusOK, out)
		return
	}

	runID := uuid.NewString()
	turnKey := ""
	if h.credits != nil {
		dec, err := h.spend(ctx, tenantID, 1, "os:"+runID+":turn", "Marketing team request (WhatsApp)")
		if err != nil || !dec.Allowed {
			out.NeedsCredits = err == nil
			out.Reply = "Your AI credits are used up. Top up from " + link(req.PortalURL, req.TenantSlug, "/settings/billing") + " and send your request again."
			writeJSON(w, http.StatusOK, out)
			return
		}
		if !dec.Exempt {
			turnKey = "os:" + runID + ":turn"
		}
	}
	sink := &captureSink{}
	h.runTurn(ctx, r, tenantID, askRequest{Text: req.Text, viaWhatsApp: true}, runID, turnKey, sink)
	out.RunID = runID
	if sink.failed != "" {
		out.Reply = "Sorry, the marketing team could not finish that. Please try again, or open MarketFlow: " + link(req.PortalURL, req.TenantSlug, "/dashboard")
		writeJSON(w, http.StatusOK, out)
		return
	}
	if sink.out.Result != nil {
		out.Route = sink.out.Result.Route
	}
	out.Reply = whatsAppReply(sink.out, link(req.PortalURL, req.TenantSlug, "/marketing"))
	writeJSON(w, http.StatusOK, out)
}

// hasCredits is true for an unlimited plan or a positive balance. Without a credits client
// (local development) every tenant may ask.
func (h *OSHandler) hasCredits(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	b, ok := h.credits.(CreditBalance)
	if !ok {
		return true, nil
	}
	n, unlimited, err := b.Balance(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return unlimited || n > 0, nil
}

func link(portal, slug, path string) string {
	portal = strings.TrimSuffix(portal, "/")
	if slug == "" {
		return portal
	}
	return portal + "/" + slug + path
}

// captureSink keeps a turn's outcome for the WhatsApp reply.
type captureSink struct {
	out    askResponse
	failed string
}

func (*captureSink) progress(team.Progress)   {}
func (s *captureSink) fail(_ int, msg string) { s.failed = msg }
func (s *captureSink) done(out askResponse)   { s.out = out }

// maxReply keeps a WhatsApp text well inside Meta's 4,096 character limit.
const maxReply = 3000

// whatsAppReply turns a turn's result into plain WhatsApp text. Spending, posting and approving
// stay in the portal until confirmation buttons ship (Sprint 17 proposals and tokens), so a
// quote ends with the link rather than asking for a text "yes".
func whatsAppReply(out askResponse, portal string) string {
	var b strings.Builder
	res := out.Result
	switch {
	case out.Answer != "":
		b.WriteString(out.Answer)
	case res != nil && res.Route == team.RouteClarify && res.Question != "":
		b.WriteString(res.Question)
	case res != nil && res.Plan != nil:
		p := res.Plan
		fmt.Fprintf(&b, "Here is the plan: %s, %d credits.", p.Recommended.Label, p.Recommended.Credits)
		if p.Recommended.Description != "" {
			b.WriteString("\n" + p.Recommended.Description)
		}
		if p.Shortfall > 0 {
			fmt.Fprintf(&b, "\nYou are %d credits short.", p.Shortfall)
		}
		b.WriteString("\n\nApprove it in MarketFlow: " + portal)
	case res != nil:
		for _, s := range res.Steps {
			if s.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(s.Text)
			}
		}
		if b.Len() == 0 {
			b.WriteString("Done. See the result in MarketFlow: " + portal)
		}
	default:
		b.WriteString("Done. See the result in MarketFlow: " + portal)
	}
	text := b.String()
	if r := []rune(text); len(r) > maxReply {
		text = string(r[:maxReply]) + "...\n\nThe rest is in MarketFlow: " + portal
	}
	return text
}
