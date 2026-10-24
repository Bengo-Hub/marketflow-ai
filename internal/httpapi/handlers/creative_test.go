package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
)

func posterCall(h *OSHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Poster(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/creative/poster", strings.NewReader(body)))
	return rec
}

func posterBody(extra string) string {
	return `{"tenant_id":"` + uuid.NewString() + `","version_id":"` + uuid.NewString() +
		`","prompt":"A jar of lotion on a table","format":"1080x1350","headline":"Soft skin","price":"KES 850","charge":true` + extra + `}`
}

type noStore struct{}

func (noStore) PutPoster(context.Context, uuid.UUID, string, []byte) (string, string, error) {
	return "", "", nil
}

func TestPosterEndpointGuardsAndRefunds(t *testing.T) {
	h := &OSHandler{logger: zap.NewNop(), registry: func(context.Context) registry.Registry { return registry.Defaults() }}
	if rec := posterCall(h, posterBody("")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d", rec.Code)
	}

	// No provider keys: every route is unavailable, so the render fails cleanly.
	h.SetPosters(media.NewPosters(func(context.Context, string) string { return "" }, noStore{}, nil))
	for _, bad := range []string{
		`{"tenant_id":"x"}`,
		posterBody(`,"format":"800x600"`),
		posterBody(`,"accent":"purple"`),
		strings.Replace(posterBody(""), `"prompt":"A jar of lotion on a table"`, `"prompt":" "`, 1),
	} {
		if rec := posterCall(h, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d, want 400", bad, rec.Code)
		}
	}

	broke := &fakeSpender{decision: CreditDecision{Allowed: false}}
	h.credits = broke
	if rec := posterCall(h, posterBody("")); rec.Code != http.StatusPaymentRequired || broke.spent != 2 {
		t.Fatalf("no credits = %d (asked %d credits, want the poster's 2)", rec.Code, broke.spent)
	}

	paid := &fakeSpender{decision: CreditDecision{Allowed: true}}
	h.credits = paid
	rec := posterCall(h, posterBody(`,"tier":"premium"`))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "nothing was charged") {
		t.Fatalf("failed render = %d %s", rec.Code, rec.Body.String())
	}
	if paid.spent != 5 || len(paid.refunded) != 1 || !strings.HasPrefix(paid.refunded[0], "render:") {
		t.Fatalf("premium charged %d and refunded %v; want 5 then one render refund", paid.spent, paid.refunded)
	}

	// A text-only revision (scene_url) never charges, even with charge:true, and a
	// scene that is gone answers 410.
	free := &fakeSpender{decision: CreditDecision{Allowed: true}}
	h.credits = free
	rec = posterCall(h, posterBody(`,"scene_url":"http://127.0.0.1:1/scene.jpg"`))
	if rec.Code != http.StatusGone || free.spent != 0 {
		t.Fatalf("recompose with a missing scene = %d, charged %d", rec.Code, free.spent)
	}
}

func TestParseHexColor(t *testing.T) {
	c, err := parseHexColor("#6D28D9")
	if err != nil || c.R != 0x6d || c.G != 0x28 || c.B != 0xd9 || c.A != 0xff {
		t.Fatalf("got %v %v", c, err)
	}
	if c, err := parseHexColor(""); err != nil || c.A != 0 {
		t.Fatal("empty must mean the default accent")
	}
	if _, err := parseHexColor("#fff"); err == nil {
		t.Fatal("short colours must be refused")
	}
}
