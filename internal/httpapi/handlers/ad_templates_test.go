package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/library"
)

// The Studio's picker gets every template from the library file, with each kind's default.
func TestAdTemplatesList(t *testing.T) {
	h := &OSHandler{lib: library.MustLoad()}
	rec := httptest.NewRecorder()
	h.AdTemplates(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/creative/ad-templates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Templates []adTemplateView `json:"templates"`
		Defaults  map[string]string `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Templates) < 10 || out.Defaults["ugc"] == "" {
		t.Fatalf("templates %d, defaults %v", len(out.Templates), out.Defaults)
	}
	keys := map[string]bool{}
	for _, tpl := range out.Templates {
		if tpl.Key == "" || tpl.Name == "" || len(tpl.Kinds) == 0 {
			t.Fatalf("incomplete template %+v", tpl)
		}
		keys[tpl.Key] = true
	}
	for _, k := range out.Defaults {
		if !keys[k] {
			t.Fatalf("default %q is not a template", k)
		}
	}
	rec = httptest.NewRecorder()
	(&OSHandler{}).AdTemplates(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no library: status %d", rec.Code)
	}
}
