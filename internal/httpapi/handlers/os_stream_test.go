package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/team"
)

// The stream is SSE "data:" events: progress, then one result (or error),
// then [DONE], with headers that keep proxies from buffering it.
func TestSSESinkWireFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	s, ok := newSSESink(rec)
	if !ok {
		t.Fatal("recorder supports flushing")
	}
	s.progress(team.Progress{Stage: "step", Member: "strategist", Label: "Amani is working on: Marketing plan"})
	s.done(askResponse{RunID: "r1", Result: &team.Result{Route: team.RouteDone}, CreditsCharged: 1})

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers = %d %v", rec.Code, rec.Header())
	}
	var events []map[string]any
	done := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("event is not JSON: %q", data)
		}
		events = append(events, ev)
	}
	if !done || len(events) != 2 || events[0]["type"] != "progress" || events[1]["type"] != "result" {
		t.Fatalf("events = %+v done=%v", events, done)
	}
	if res := events[1]["data"].(map[string]any); res["run_id"] != "r1" || res["credits_charged"].(float64) != 1 {
		t.Fatalf("result = %+v", res)
	}

	rec = httptest.NewRecorder()
	s, _ = newSSESink(rec)
	s.fail(http.StatusInternalServerError, "the marketing team could not finish this request")
	if !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("error stream = %s", rec.Body.String())
	}
}
