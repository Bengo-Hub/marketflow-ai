package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/adintel"
)

type complaintSpender struct {
	mu      sync.Mutex
	spent   []string
	refunds []string
	deny    bool
}

func (f *complaintSpender) Spend(_ context.Context, _ uuid.UUID, _ int64, key string) (CreditDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deny {
		return CreditDecision{Allowed: false}, nil
	}
	f.spent = append(f.spent, key)
	return CreditDecision{Allowed: true}, nil
}

func (f *complaintSpender) Refund(_ context.Context, _ uuid.UUID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, key)
	return nil
}

func complaintHandler(t *testing.T, serperStatus int, emptyPlaces bool, sp CreditSpender) *AdIntelHandler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serperStatus != http.StatusOK {
			w.WriteHeader(serperStatus)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			_, _ = w.Write([]byte(`{"reviews":[{"rating":1,"snippet":"Waited two hours"}]}`))
			return
		}
		if !emptyPlaces {
			_, _ = w.Write([]byte(`{"places":[{"cid":"1","ratingCount":4}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"places":[]}`))
	}))
	t.Cleanup(srv.Close)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := adintel.NewComplaints(func(context.Context) adintel.ComplaintSettings { return adintel.ComplaintSettings{Key: "k"} }, rdb, "test", nil)
	c.Base = srv.URL
	return (&AdIntelHandler{logger: zap.NewNop()}).WithComplaints(c).WithCredits(sp)
}

func askComplaints(h *AdIntelHandler, tid uuid.UUID) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"tenant_id": tid.String(), "query": "bakery Kisumu"})
	w := httptest.NewRecorder()
	h.Complaints(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(b))))
	return w
}

// A fresh report costs one credit, the same report again is free, and a failed search is refunded.
func TestComplaintsCharging(t *testing.T) {
	tid := uuid.New()
	sp := &complaintSpender{}
	h := complaintHandler(t, http.StatusOK, false, sp)
	if w := askComplaints(h, tid); w.Code != http.StatusOK || len(sp.spent) != 1 || len(sp.refunds) != 0 {
		t.Fatalf("fresh report: %d spent %v", w.Code, sp.spent)
	}
	if w := askComplaints(h, tid); w.Code != http.StatusOK || len(sp.spent) != 1 {
		t.Fatalf("a cached report is free: %d spent %v", w.Code, sp.spent)
	}

	failing := &complaintSpender{}
	h = complaintHandler(t, http.StatusInternalServerError, false, failing)
	if w := askComplaints(h, tid); w.Code != http.StatusBadGateway || len(failing.spent) != 1 || len(failing.refunds) != 1 {
		t.Fatalf("failed search refunded: %d spent %v refunds %v", w.Code, failing.spent, failing.refunds)
	}

	empty := &complaintSpender{}
	h = complaintHandler(t, http.StatusOK, true, empty)
	if w := askComplaints(h, tid); w.Code != http.StatusOK || len(empty.refunds) != 1 {
		t.Fatalf("a search that finds nothing is refunded: %d refunds %v", w.Code, empty.refunds)
	}

	broke := &complaintSpender{deny: true}
	h = complaintHandler(t, http.StatusOK, false, broke)
	if w := askComplaints(h, tid); w.Code != http.StatusPaymentRequired {
		t.Fatalf("no credits: %d", w.Code)
	}
}
