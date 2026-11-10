package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

var testTenant = uuid.MustParse("6a4f0ad8-2c1e-4b8e-9d55-1a2b3c4d5e6f")

// Decision 61: reads of money and logs are never offered to Vera.
func TestStaticResourcesOfferNoMoneyOrLogs(t *testing.T) {
	for svc, paths := range resourcePaths {
		for res := range paths {
			for _, bad := range []string{"invoice", "transaction", "balance", "log", "payment", "subscriptions"} {
				if strings.Contains(res, bad) {
					t.Errorf("%s/%s is a sensitive read", svc, res)
				}
			}
		}
	}
}

// An approved discovered route is called with the service key and the tenant headers; its path
// params come from the agent's params, which cannot add path segments.
func TestDiscoveredRoutes(t *testing.T) {
	var gotPath, gotKey, gotTenant string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotTenant = r.URL.EscapedPath(), r.Header.Get("X-API-Key"), r.Header.Get("X-Tenant-ID")
		_, _ = w.Write([]byte(`[{"title":"2 for 1"}]`))
	}))
	defer svc.Close()

	var toolCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolCalls.Add(1)
		if r.URL.Path != "/api/v1/internal/endpoints/tools" || r.URL.Query().Get("tenant_id") != testTenant.String() || r.Header.Get("X-API-Key") != "api-key" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tools": []map[string]any{
			{"service": "ordering", "resource": "promotions_deals", "url": svc.URL + "/api/v1/urban-loft/promotions/deals"},
			{"service": "ordering", "resource": "catalog_items_by_itemid", "url": svc.URL + "/api/v1/urban-loft/catalog/items/{itemId}", "params": []string{"itemId"}},
			{"service": "ordering", "resource": "elsewhere", "url": "http://evil.example/api"},
		}})
	}))
	defer api.Close()

	q := NewQueryServiceData(QueryServiceDataConfig{ServiceKey: "svc-key", Logger: zap.NewNop(), APIBaseURL: api.URL, APIKey: "api-key"})
	q.allowedBases = []string{svc.URL}
	ctx := context.Background()

	out, err := q.Invoke(ctx, testTenant, `{"service":"ordering","resource":"promotions_deals"}`)
	if err != nil || !strings.Contains(out, "2 for 1") {
		t.Fatalf("out %q err %v", out, err)
	}
	if gotPath != "/api/v1/urban-loft/promotions/deals" || gotKey != "svc-key" || gotTenant != testTenant.String() {
		t.Fatalf("path %q key %q tenant %q", gotPath, gotKey, gotTenant)
	}

	if _, err := q.Invoke(ctx, testTenant, `{"service":"ordering","resource":"catalog_items_by_itemid","params":{"itemId":"42"}}`); err != nil || gotPath != "/api/v1/urban-loft/catalog/items/42" {
		t.Fatalf("param fill: path %q err %v", gotPath, err)
	}
	for _, bad := range []string{`{}`, `{"itemId":"../../admin"}`, `{"itemId":"a/b"}`} {
		if _, err := q.Invoke(ctx, testTenant, `{"service":"ordering","resource":"catalog_items_by_itemid","params":`+bad+`}`); err == nil {
			t.Errorf("params %s must be refused", bad)
		}
	}
	if _, err := q.Invoke(ctx, testTenant, `{"service":"ordering","resource":"elsewhere"}`); err == nil {
		t.Fatal("a discovered URL outside the known services must be refused")
	}
	if n := toolCalls.Load(); n != 1 {
		t.Fatalf("the tool list should be cached per tenant, fetched %d times", n)
	}
}

// Owner 2026-11-10: course enrollments reach Vera as counts only, never student details or
// their payments.
func TestEnrollmentsAreCountsOnly(t *testing.T) {
	var gotLimit, gotCourse string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit, gotCourse = r.URL.Query().Get("limit"), r.URL.Query().Get("courseId")
		_, _ = w.Write([]byte(`{"total":42,"page":1,"pages":42,"items":[{"fullName":"Jane Doe","email":"jane@example.com","installments":[{"amount":5000}]}]}`))
	}))
	defer srv.Close()
	old := serviceRegistry["codevertex"]
	serviceRegistry["codevertex"] = srv.URL
	defer func() { serviceRegistry["codevertex"] = old }()

	q := NewQueryServiceData(QueryServiceDataConfig{ServiceKey: "k", Logger: zap.NewNop()})
	out, err := q.Invoke(context.Background(), testTenant, `{"service":"codevertex","resource":"enrollments","filters":{"courseId":"c-1"},"limit":50}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Jane") || strings.Contains(out, "jane@") || strings.Contains(out, "5000") || !strings.Contains(out, `"total":42`) {
		t.Fatalf("out = %s", out)
	}
	if gotLimit != "1" || gotCourse != "c-1" {
		t.Fatalf("limit %q course %q", gotLimit, gotCourse)
	}
}

// A huge response is never read whole.
func TestResponseIsCapped(t *testing.T) {
	big := strings.Repeat("x", 4<<20)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`["` + big + `"]`))
	}))
	defer svc.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tools": []map[string]any{{"service": "ordering", "resource": "big", "url": svc.URL + "/big"}}})
	}))
	defer api.Close()
	q := NewQueryServiceData(QueryServiceDataConfig{ServiceKey: "k", Logger: zap.NewNop(), APIBaseURL: api.URL, APIKey: "k"})
	q.allowedBases = []string{svc.URL}
	out, err := q.Invoke(context.Background(), testTenant, `{"service":"ordering","resource":"big"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 9000 {
		t.Fatalf("output %d bytes", len(out))
	}
}
