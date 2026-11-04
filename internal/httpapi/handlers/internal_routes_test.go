package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func post(h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b)))
	return rec
}

func TestChargeCredits(t *testing.T) {
	credits := &balanceCredits{n: 50}
	h := NewOSHandler(OSConfig{Credits: credits, Logger: zap.NewNop()})
	if rec := post(h.ChargeCredits, map[string]any{"tenant_id": "x", "units": 10, "key": "k"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad tenant id: %d", rec.Code)
	}
	if rec := post(h.ChargeCredits, map[string]any{"tenant_id": uuid.NewString(), "units": 0, "key": "k"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("zero units: %d", rec.Code)
	}
	rec := post(h.ChargeCredits, map[string]any{"tenant_id": uuid.NewString(), "units": 10, "key": "ads-launch:1"})
	var dec CreditDecision
	_ = json.Unmarshal(rec.Body.Bytes(), &dec)
	if rec.Code != http.StatusOK || !dec.Allowed || credits.spends != 1 {
		t.Fatalf("charge: %d %+v spends=%d", rec.Code, dec, credits.spends)
	}
	// Unmetered (no credits client): allowed and exempt, as for local development.
	rec = post(NewOSHandler(OSConfig{Logger: zap.NewNop()}).ChargeCredits, map[string]any{"tenant_id": uuid.NewString(), "units": 10, "key": "k"})
	_ = json.Unmarshal(rec.Body.Bytes(), &dec)
	if !dec.Allowed || !dec.Exempt {
		t.Fatalf("unmetered: %+v", dec)
	}
}

func TestInboxIntentValidation(t *testing.T) {
	h := NewInboxIntentHandler(nil, zap.NewNop())
	if rec := post(h.Tag, map[string]any{"text": ""}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text: %d", rec.Code)
	}
	// No decider: the worker gets a 503 and leaves the item for the owner, never a guessed tag.
	if rec := post(h.Tag, map[string]any{"text": "How much?"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no decider: %d", rec.Code)
	}
}
