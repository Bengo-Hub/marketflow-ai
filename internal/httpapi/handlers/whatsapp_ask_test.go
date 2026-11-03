package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/assistgate"
	"github.com/bengobox/marketflow-ai/internal/team"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type balanceCredits struct {
	n      int64
	spends int
}

func (b *balanceCredits) Spend(context.Context, uuid.UUID, int64, string) (CreditDecision, error) {
	b.spends++
	return CreditDecision{Allowed: true}, nil
}
func (*balanceCredits) Refund(context.Context, uuid.UUID, string) error { return nil }
func (b *balanceCredits) Balance(context.Context, uuid.UUID) (int64, bool, error) {
	return b.n, false, nil
}

func whatsAppAsk(t *testing.T, h *OSHandler, text string) WhatsAppAskResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"tenant_id": uuid.NewString(), "tenant_slug": "boi", "text": text, "portal_url": "https://mf.example"})
	rec := httptest.NewRecorder()
	h.WhatsAppAsk(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out WhatsAppAskResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// Plain chat stops at the gate (no credit spent, no reply); a marketing request with no credits
// gets the top-up reply and spends nothing. With no model configured the rules decide.
func TestWhatsAppAskGateAndCredits(t *testing.T) {
	credits := &balanceCredits{n: 0}
	h := NewOSHandler(OSConfig{Credits: credits, Logger: zap.NewNop()}).WithGate(assistgate.New(nil))

	out := whatsAppAsk(t, h, "Habari, is my order on the way?")
	if out.Pass || out.Reply != "" || credits.spends != 0 {
		t.Fatalf("plain chat must not reach the team: %+v", out)
	}

	out = whatsAppAsk(t, h, "Make a TikTok video ad for our holiday classes")
	if !out.Pass || !out.NeedsCredits || !strings.Contains(out.Reply, "https://mf.example/boi/settings/billing") || credits.spends != 0 {
		t.Fatalf("no credits must answer with the top-up link: %+v", out)
	}
}

func TestWhatsAppReplyText(t *testing.T) {
	if got := whatsAppReply(askResponse{Answer: "Hi there"}, "p"); got != "Hi there" {
		t.Fatalf("answer: %q", got)
	}
	got := whatsAppReply(askResponse{Result: &team.Result{Route: team.RouteClarify, Question: "Which product?"}}, "p")
	if got != "Which product?" {
		t.Fatalf("clarify: %q", got)
	}
	long := strings.Repeat("a", maxReply+50)
	if got := whatsAppReply(askResponse{Answer: long}, "https://mf"); !strings.HasSuffix(got, "https://mf") || len([]rune(got)) > maxReply+60 {
		t.Fatalf("long replies are cut with the portal link")
	}
}
