package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/sessions"
)

// Sprint 23 L25/L26 (away decision 25): a lead's WhatsApp reply after the instant follow-up is
// answered by the same customer-safe assistant as the website widget (public tool allowlist:
// knowledge base, catalogue, booking), one conversation per lead. A lead who asks for a person,
// or names a budget at or above the business's hand-off value, goes to the owner instead.

type leadReplyRequest struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	LeadID     string `json:"lead_id"`
	Text       string `json:"text"`
	// HandoffValue is the business's hand-off amount (0: amounts never hand off).
	HandoffValue float64 `json:"handoff_value"`
}

// LeadReplyResponse is what marketflow-worker sends back on WhatsApp.
type LeadReplyResponse struct {
	Reply         string `json:"reply"`
	Handoff       bool   `json:"handoff"`
	HandoffReason string `json:"handoff_reason,omitempty"`
}

const handoffReply = "Thank you. I have asked the team to get back to you personally shortly."

var humanAsk = regexp.MustCompile(`(?i)\b(human|real person|a person|someone real|agent|call me|phone me|ring me|speak to|talk to (someone|the owner|a manager)|manager|the owner|customer care)\b`)

// amountPattern finds amounts like 150k, 1.2m, KES 120,000 or 120000.
var amountPattern = regexp.MustCompile(`(?i)(\d[\d,]*(?:\.\d+)?)\s*(k|m|million|thousand)?\b`)

// detectHandoff reports whether the lead should go to the owner, and why.
func detectHandoff(text string, threshold float64) (bool, string) {
	if humanAsk.MatchString(text) {
		return true, "asked for a person"
	}
	if threshold <= 0 {
		return false, ""
	}
	for _, m := range amountPattern.FindAllStringSubmatch(text, -1) {
		n, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "k", "thousand":
			n *= 1_000
		case "m", "million":
			n *= 1_000_000
		}
		if n >= threshold {
			return true, "budget at or above the hand-off value"
		}
	}
	return false, ""
}

// leadSessionNS scopes the per-lead conversation ids.
var leadSessionNS = uuid.MustParse("8a0d6f3c-1b8e-4d55-9a7e-5f2c0d4b9e17")

// leadSessionID is the one conversation of a lead with the business.
func leadSessionID(tenantID, leadID string) uuid.UUID {
	return uuid.NewSHA1(leadSessionNS, []byte(tenantID+"/"+leadID))
}

// LeadReply handles POST /api/v1/internal/os/lead-reply (S2S, marketflow-worker).
func (h *ChatHandler) LeadReply(w http.ResponseWriter, r *http.Request) {
	var req leadReplyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil || req.LeadID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id and lead_id are required")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeJSON(w, http.StatusOK, LeadReplyResponse{})
		return
	}
	if ok, why := detectHandoff(req.Text, req.HandoffValue); ok {
		writeJSON(w, http.StatusOK, LeadReplyResponse{Reply: handoffReply, Handoff: true, HandoffReason: why})
		return
	}
	if blocked, reply := rag.ScreenInput(req.Text); blocked {
		writeJSON(w, http.StatusOK, LeadReplyResponse{Reply: reply})
		return
	}
	ctx := r.Context()
	// One credit per answered message; without credits the owner takes over, so the lead is
	// never left unanswered.
	chargeKey := ""
	if h.credits != nil {
		key := "lead-reply:" + uuid.NewString()
		dec, err := h.credits.Spend(ctx, tenantID, 1, key)
		if err != nil || !dec.Allowed {
			writeJSON(w, http.StatusOK, LeadReplyResponse{Reply: handoffReply, Handoff: true, HandoffReason: "no AI credits"})
			return
		}
		if !dec.Exempt {
			chargeKey = key
		}
	}
	sid := leadSessionID(tenantID.String(), req.LeadID)
	var history []ai.Message
	if h.sessions != nil {
		if hist, err := h.sessions.LoadHistory(ctx, tenantID, sid); err == nil {
			history = hist
		} else {
			_, _ = h.sessions.CreateSessionWithID(ctx, tenantID, sid, "whatsapp_lead")
		}
	}
	result, err := h.pipeline.Chat(ctx, rag.ChatInput{
		TenantID: tenantID, TenantSlug: req.TenantSlug, SessionID: sid, Question: req.Text,
		History: history, PublicMode: true,
	})
	if err != nil || result == nil || strings.TrimSpace(result.Response) == "" {
		h.logger.Warn("lead reply failed; handing to the owner", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		h.refund(r, tenantID, chargeKey)
		writeJSON(w, http.StatusOK, LeadReplyResponse{Reply: handoffReply, Handoff: true, HandoffReason: "the assistant could not answer"})
		return
	}
	if h.sessions != nil {
		saved := result.RawResponse
		if saved == "" {
			saved = result.Response
		}
		_ = h.sessions.AppendMessages(ctx, tenantID, sid,
			ai.Message{Role: "user", Content: req.Text}, ai.Message{Role: "assistant", Content: saved},
			sessions.AnswerMeta{Route: rag.RouteOf(result.Model), Intent: result.Intent})
	}
	writeJSON(w, http.StatusOK, LeadReplyResponse{Reply: result.Response})
}
