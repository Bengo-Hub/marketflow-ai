package tools

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type stubSearcher struct{ calls int }

func (s *stubSearcher) Invoke(context.Context, uuid.UUID, string) (string, error) {
	s.calls++
	return `{"results":[{"title":"from tavily"}]}`, nil
}

const vertexReply = `{"results":[{"document":{"derivedStructData":{"title":"Nairobi cafes","link":"https://example.com/a",
	"snippets":[{"snippet":"Best coffee"},{"snippet":"in town"}]}}}]}`

// With only an API key the searchLite method is called with ?key=; results come back as
// title, url and content, the same shape Tavily gives the model.
func TestVertexSearchLiteWithAnAPIKey(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.URL.Query().Get("key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(vertexReply))
	}))
	defer srv.Close()
	v := NewVertexSearcher(VertexConfig{ProjectID: "p1", EngineID: "web-engine", APIKey: "AIza-test", Endpoint: srv.URL})
	out, err := v.Invoke(context.Background(), testTenant, `{"query":"cafes","max_results":3}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "/v1/projects/p1/locations/global/collections/default_collection/engines/web-engine/servingConfigs/default_search:searchLite"
	if gotPath != want || gotKey != "AIza-test" || gotBody["query"] != "cafes" || gotBody["pageSize"] != float64(3) {
		t.Fatalf("path %q key %q body %v", gotPath, gotKey, gotBody)
	}
	if !strings.Contains(out, `"url":"https://example.com/a"`) || !strings.Contains(out, "Best coffee in town") {
		t.Fatalf("out = %s", out)
	}
}

// A service account signs a JWT assertion, trades it for a token once, and calls search.
func TestVertexSearchWithAServiceAccount(t *testing.T) {
	pk, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(pk)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	tokens := 0
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || strings.Count(r.Form.Get("assertion"), ".") != 2 {
				http.Error(w, "bad grant", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"ya29.test","expires_in":3600}`))
			return
		}
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(vertexReply))
	}))
	defer srv.Close()
	sa, _ := json.Marshal(map[string]string{"client_email": "vera@p1.iam.gserviceaccount.com", "private_key": pemKey})
	v := NewVertexSearcher(VertexConfig{ProjectID: "p1", Location: "eu", EngineID: "e", ServiceAccountJSON: string(sa),
		Endpoint: srv.URL, TokenURL: srv.URL + "/token"})
	for i := 0; i < 2; i++ {
		if _, err := v.Invoke(context.Background(), testTenant, `{"query":"x"}`); err != nil {
			t.Fatal(err)
		}
	}
	if auth != "Bearer ya29.test" || !strings.HasSuffix(path, "/locations/eu/collections/default_collection/engines/e/servingConfigs/default_search:search") || tokens != 1 {
		t.Fatalf("auth %q path %q tokens %d", auth, path, tokens)
	}
	if NewVertexSearcher(VertexConfig{Location: "eu"}).endpoint() != "https://eu-discoveryengine.googleapis.com" {
		t.Fatal("regional host")
	}
}

// Tavily answers when Vertex is not configured or fails, so existing tenants keep working.
func TestVertexFallsBackToTavily(t *testing.T) {
	tav := &stubSearcher{}
	out, err := NewVertexSearcher(VertexConfig{Fallback: tav}).Invoke(context.Background(), testTenant, `{"query":"x"}`)
	if err != nil || tav.calls != 1 || !strings.Contains(out, "from tavily") {
		t.Fatalf("unconfigured: %s %v", out, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"status":"FAILED_PRECONDITION"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := NewVertexSearcher(VertexConfig{ProjectID: "p", EngineID: "e", APIKey: "k", Endpoint: srv.URL, Fallback: tav}).
		Invoke(context.Background(), testTenant, `{"query":"x"}`); err != nil || tav.calls != 2 {
		t.Fatalf("failure fallback: %v calls %d", err, tav.calls)
	}
	if _, err := NewVertexSearcher(VertexConfig{}).Invoke(context.Background(), testTenant, `{"query":"x"}`); err == nil {
		t.Fatal("no fallback and no config must fail")
	}
}
