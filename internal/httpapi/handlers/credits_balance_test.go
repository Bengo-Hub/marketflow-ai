package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Balance reads subscriptions-api's check answer (units=0): bought plus included credits, -1 or
// exempt as unlimited. The bodies below are what credits.Check encodes since 2026-11-14 (G3 of
// the billing audit: before, the balances were never filled and every tenant read 0).
func TestHTTPCreditSpenderBalance(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		want      int64
		unlimited bool
	}{
		{"bought plus included", `{"allowed":true,"from_quota":0,"from_bundle":0,"overage_units":0,"overage_kes":0,"bundle_balance":60,"quota_balance":5}`, 65, false},
		{"bought only (PowerSuite)", `{"allowed":true,"bundle_balance":150,"quota_balance":0}`, 150, false},
		{"nothing left", `{"allowed":true,"bundle_balance":0,"quota_balance":0}`, 0, false},
		{"refused, no subscription", `{"allowed":false,"reason":"no_subscription","bundle_balance":0,"quota_balance":0}`, 0, false},
		{"unlimited allowance", `{"allowed":true,"bundle_balance":0,"quota_balance":-1}`, 0, true},
		{"platform tenant", `{"allowed":true,"exempt":true,"bundle_balance":0,"quota_balance":0}`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tid := uuid.New()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/s2s/credits/ai_credits/check" || r.URL.Query().Get("units") != "0" ||
					r.URL.Query().Get("tenant_id") != tid.String() || r.Header.Get("X-API-Key") != "k" {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			n, unlimited, err := NewHTTPCreditSpender(srv.URL, "k", zap.NewNop()).Balance(context.Background(), tid)
			if err != nil || n != c.want || unlimited != c.unlimited {
				t.Fatalf("got %d unlimited=%v err=%v, want %d unlimited=%v", n, unlimited, err, c.want, c.unlimited)
			}
		})
	}
}
