package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/agent"
)

func agentRunCall(t *testing.T, h *OSHandler, billable bool) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"run_id": uuid.NewString(), "tenant_id": uuid.NewString(), "billable": billable})
	rec := httptest.NewRecorder()
	h.AgentRun(rec, httptest.NewRequest(http.MethodPost, "/api/v1/agent/run", strings.NewReader(string(body))))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// Automatic runs are refused cleanly, never started, when the tenant has no
// credits or the credit check is down; the worker records the reason.
func TestAutomaticRunNeedsCredits(t *testing.T) {
	// The refusals return before the loop runs, so an empty loop is enough.
	noCredits := &fakeSpender{decision: CreditDecision{Allowed: false}}
	h := &OSHandler{agent: &agent.Loop{}, credits: noCredits, logger: zap.NewNop()}
	out := agentRunCall(t, h, true)
	if out["status"] != "failed" || !strings.Contains(out["error"].(string), "Not enough AI credits") || noCredits.spent != 1 {
		t.Fatalf("no-credit run = %+v (spent %d)", out, noCredits.spent)
	}

	down := &fakeSpender{err: errors.New("subscriptions-api down")}
	h.credits = down
	out = agentRunCall(t, h, true)
	if out["status"] != "failed" || !strings.Contains(out["error"].(string), "Could not check AI credits") {
		t.Fatalf("billing-down run = %+v", out)
	}
	if len(down.refunded) != 0 {
		t.Fatal("nothing was charged, so nothing may be refunded")
	}
}
