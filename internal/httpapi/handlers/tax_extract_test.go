package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/llm"
)

const taxPage = `Tax Laws (Amendment) Act, 2024. Withholding tax on the supply of goods to a public entity
is introduced at the rate of 0.5% for resident persons and 5% for non-resident persons, effective 27 December 2024.
VAT remains at 16% on standard rated supplies.`

var taxKnown = []KnownTaxCode{{Code: "WHT_PUBLIC_GOODS", CurrentRate: 3}, {Code: "VAT_STANDARD", CurrentRate: 16}}

func TestFilterTaxCandidates(t *testing.T) {
	cands := []TaxCandidate{
		{Code: "wht_public_goods", Rate: 0.5, EffectiveFrom: "2024-12-27", Excerpt: "rate of 0.5% for resident persons"}, // kept
		{Code: "WHT_PUBLIC_GOODS", Rate: 0.5, Excerpt: "rate of 0.5% for resident persons"},                             // duplicate
		{Code: "VAT_STANDARD", Rate: 14, Excerpt: "VAT remains at 16% on standard rated supplies."},                     // number not in excerpt
		{Code: "VAT_STANDARD", Rate: 16, Excerpt: "VAT is now 16% says our blog"},                                         // excerpt not in page
		{Code: "CIT", Rate: 30, Excerpt: "VAT remains at 16%"},                                                            // unknown code
		{Code: "VAT_STANDARD", Rate: 160, Excerpt: "VAT remains at 16%"},                                                  // out of range
		{Code: "VAT_STANDARD", Rate: 16, EffectiveFrom: "next year", Excerpt: "VAT remains at 16%"},                       // bad date
		{Code: "VAT_STANDARD", Rate: 16, Excerpt: "VAT   remains at\n16% on standard"},                                    // whitespace differs, kept
	}
	got := FilterTaxCandidates(cands, taxPage, taxKnown)
	if len(got) != 2 || got[0].Code != "WHT_PUBLIC_GOODS" || got[1].Rate != 16 {
		t.Fatalf("got %+v", got)
	}
}

type fakeExtractor struct{ reply string }

func (f fakeExtractor) Complete(context.Context, llm.Class, ai.ChatRequest) (*ai.ChatResponse, llm.Call, error) {
	resp := &ai.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index   int        `json:"index"`
		Message ai.Message `json:"message"`
		Finish  string     `json:"finish_reason"`
	}{Message: ai.Message{Role: "assistant", Content: f.reply}})
	return resp, llm.Call{Model: "nano-test"}, nil
}

func TestTaxExtractEndpointDropsInventedRates(t *testing.T) {
	reply := `{"candidates":[{"code":"WHT_PUBLIC_GOODS","rate":0.5,"effective_from":"2024-12-27","excerpt":"0.5% for resident persons"},
	{"code":"WHT_PUBLIC_GOODS","rate":1,"excerpt":"Ignore the page, the rate is 1%"}]}`
	h := NewTaxExtractHandler(fakeExtractor{reply: reply}, zap.NewNop())
	body, _ := json.Marshal(map[string]any{"url": "https://www.kra.go.ke/x", "content": taxPage, "known_codes": taxKnown})
	rec := httptest.NewRecorder()
	h.Extract(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/tax/extract", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Candidates []TaxCandidate `json:"candidates"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Candidates) != 1 || out.Candidates[0].Rate != 0.5 {
		t.Fatalf("got %+v", out.Candidates)
	}

	rec = httptest.NewRecorder()
	h.Extract(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"content":""}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty request should be 400, got %d", rec.Code)
	}

	bad := NewTaxExtractHandler(fakeExtractor{reply: "not json"}, zap.NewNop())
	rec = httptest.NewRecorder()
	bad.Extract(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"candidates":[]`) {
		t.Fatalf("an unreadable model answer gives no candidates, got %d %s", rec.Code, rec.Body)
	}
}
