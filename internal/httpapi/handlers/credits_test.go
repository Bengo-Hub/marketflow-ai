package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/rag"
)

type fakeSpender struct {
	decision CreditDecision
	err      error
	spent    int
	refunded []string
}

func (f *fakeSpender) Spend(_ context.Context, _ uuid.UUID, units int64, _ string) (CreditDecision, error) {
	f.spent += int(units)
	return f.decision, f.err
}

func (f *fakeSpender) Refund(_ context.Context, _ uuid.UUID, key string) error {
	f.refunded = append(f.refunded, key)
	return nil
}

func newChat(c CreditSpender) *ChatHandler {
	return &ChatHandler{credits: c, logger: zap.NewNop()}
}

func TestChargeMessageRefusalIsA402(t *testing.T) {
	f := &fakeSpender{decision: CreditDecision{Allowed: false, Reason: "insufficient_credits"}}
	rec := httptest.NewRecorder()
	key, ok := newChat(f).chargeMessage(rec, httptest.NewRequest(http.MethodPost, "/chat", nil), uuid.New(), true)
	if ok || key != "" || rec.Code != http.StatusPaymentRequired {
		t.Fatalf("a refused message must stop with 402: ok=%v key=%q code=%d", ok, key, rec.Code)
	}
}

func TestChargeMessageFailsClosedWhenBillingIsUnreachable(t *testing.T) {
	f := &fakeSpender{err: errors.New("down")}
	rec := httptest.NewRecorder()
	if _, ok := newChat(f).chargeMessage(rec, httptest.NewRequest(http.MethodPost, "/chat", nil), uuid.New(), true); ok || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown balance must not run the message: ok=%v code=%d", ok, rec.Code)
	}
}

func TestChargeMessageChargesOneCreditAndRefundsOnFailure(t *testing.T) {
	f := &fakeSpender{decision: CreditDecision{Allowed: true}}
	h := newChat(f)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	tid := uuid.New()
	key, ok := h.chargeMessage(rec, req, tid, true)
	if !ok || key == "" || f.spent != 1 {
		t.Fatalf("one credit per message: ok=%v key=%q spent=%d", ok, key, f.spent)
	}
	h.refund(req, tid, key)
	if len(f.refunded) != 1 || f.refunded[0] != key {
		t.Fatalf("refund: %v", f.refunded)
	}
	h.refund(req, tid, "")
	if len(f.refunded) != 1 {
		t.Fatal("nothing was charged, so nothing to refund")
	}
}

func TestExemptAndUnidentifiedTenantsAreNotCharged(t *testing.T) {
	f := &fakeSpender{decision: CreditDecision{Allowed: true, Exempt: true}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	if key, ok := newChat(f).chargeMessage(rec, req, uuid.New(), true); !ok || key != "" {
		t.Fatalf("an exempt tenant has nothing to refund: key=%q ok=%v", key, ok)
	}
	f2 := &fakeSpender{}
	if key, ok := newChat(f2).chargeMessage(rec, req, uuid.New(), false); !ok || key != "" || f2.spent != 0 {
		t.Fatal("a tenant that could not be identified is not billed")
	}
	if key, ok := newChat(nil).chargeMessage(rec, req, uuid.New(), true); !ok || key != "" {
		t.Fatal("no billing configured means unmetered")
	}
}

// Owner rule (2026-10-24): a free model's answer is charged; only a reply flagged
// as made without a model is refunded, and only when no paid model was used.
func TestRefundableOnlyFlaggedAndUnpaid(t *testing.T) {
	kb := &rag.ChatResult{Model: rag.ModelKnowledgeBase}
	free := &llm.Meter{}
	free.Add(llm.Call{Provider: "ollama", Paid: false})
	paid := &llm.Meter{}
	paid.Add(llm.Call{Provider: "openai", Paid: true})

	if !refundable(kb, nil) || !refundable(kb, free) {
		t.Fatal("a knowledge-base answer with no paid call is refunded")
	}
	if refundable(kb, paid) {
		t.Fatal("paid tokens were spent (e.g. the classifier): no refund")
	}
	if refundable(&rag.ChatResult{Model: "llama3.1:8b"}, free) {
		t.Fatal("a free model's answer is charged")
	}
}
