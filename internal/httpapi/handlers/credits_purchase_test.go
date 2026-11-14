package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PurchaseBundle calls subscriptions-api's service route with the key, the bundle, the phone and
// the source, and reads the Treasury intent id; a refusal is an error with its reason.
func TestHTTPCreditSpenderPurchaseBundle(t *testing.T) {
	var got map[string]any
	var key, path string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, path = r.Header.Get("X-API-Key"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"intent_id":"intent-1","status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"an active subscription is required"}`))
	}))
	defer srv.Close()
	c := NewHTTPCreditSpender(srv.URL, "svc-key", zap.NewNop())
	var _ CreditPurchaser = c
	tid := uuid.New()

	id, err := c.PurchaseBundle(context.Background(), tid, "bundle_60", "254700000001", "ref-1")
	if err != nil || id != "intent-1" {
		t.Fatalf("purchase: %q %v", id, err)
	}
	if key != "svc-key" || path != "/api/v1/s2s/credits/ai_credits/purchase" || got["tenant_id"] != tid.String() ||
		got["code"] != "bundle_60" || got["phone"] != "254700000001" || got["source"] != "marketflow_whatsapp" || got["source_ref"] != "ref-1" {
		t.Fatalf("request: %s %s %+v", key, path, got)
	}

	status = http.StatusBadRequest
	if _, err := c.PurchaseBundle(context.Background(), tid, "bundle_60", "254700000001", "ref-2"); err == nil {
		t.Fatal("a refusal must be an error")
	}
}
