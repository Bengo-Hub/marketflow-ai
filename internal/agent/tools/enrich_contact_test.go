package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The tool reads the contact with a GET (the old version sent a PATCH to a PUT-only route) and
// answers with metadata.enrichment, never with a web search.
func TestEnrichContactReadsStoredProfile(t *testing.T) {
	tenant := uuid.New()
	var method, path, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, gotTenant = r.Method, r.URL.Path, r.Header.Get("X-Tenant-ID")
		_, _ = w.Write([]byte(`{"id":"c1","first_name":"Jane","last_name":"Wanjiru","metadata":{"enrichment":{"segment":"lapsed_customer","purchases":{"orders":3}}}}`))
	}))
	defer srv.Close()
	tool := NewEnrichContact(EnrichContactConfig{APIBaseURL: srv.URL, ServiceKey: "k", Logger: zap.NewNop()})
	out, err := tool.Invoke(context.Background(), tenant, `{"contact_id":"6f1c2d0e-0000-4000-8000-0000000000c1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/v1/contacts/6f1c2d0e-0000-4000-8000-0000000000c1" || gotTenant != tenant.String() {
		t.Fatalf("request %s %s tenant %s", method, path, gotTenant)
	}
	var res struct {
		Name    string         `json:"name"`
		Profile map[string]any `json:"profile"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Name != "Jane Wanjiru" || res.Profile["segment"] != "lapsed_customer" {
		t.Fatalf("got %s", out)
	}
	if strings.Contains(strings.ToLower(tool.Description()), "search the web") {
		t.Fatal("the description must not promise a web search")
	}
}

func TestEnrichContactWithoutProfileSaysSo(t *testing.T) {
	out, err := profileResult("c2", []byte(`{"first_name":"Otieno","metadata":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"profile":null`) || !strings.Contains(out, "no profile yet") {
		t.Fatalf("got %s", out)
	}
}
