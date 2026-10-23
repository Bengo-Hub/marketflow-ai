package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type fakePrep struct {
	out string
	err error
}

func (f fakePrep) Invoke(context.Context, uuid.UUID, string) (string, error) { return f.out, f.err }

func meetingPrepCall(h *OSHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.MeetingPrep(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/os/meeting-prep", strings.NewReader(body)))
	return rec
}

func TestMeetingPrepEndpoint(t *testing.T) {
	tid, cid := uuid.NewString(), uuid.NewString()
	ok := `{"tenant_id":"` + tid + `","contact_id":"` + cid + `","purpose":"demo"}`

	h := &OSHandler{logger: zap.NewNop()}
	if rec := meetingPrepCall(h, ok); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d", rec.Code)
	}

	h.SetMeetingPrep(fakePrep{out: `{"talking_points":["Ask about the C9 order"]}`})
	rec := meetingPrepCall(h, ok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "talking_points") {
		t.Fatalf("ok = %d %s", rec.Code, rec.Body.String())
	}
	if rec := meetingPrepCall(h, `{"tenant_id":"`+tid+`","contact_id":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad contact = %d", rec.Code)
	}

	// A failing tool is an error status, so the worker sends its reminder without talking points.
	h.SetMeetingPrep(fakePrep{err: errors.New("model down")})
	if rec := meetingPrepCall(h, ok); rec.Code != http.StatusBadGateway {
		t.Fatalf("tool failure = %d", rec.Code)
	}
}
