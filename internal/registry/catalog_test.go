package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The bundles on sale come from subscriptions-api's credits catalog; the registry's own list is
// only the fallback (2026-11-12, Sprint 15 flag: no copy of prices in marketflow-ai).
func TestCatalogBundles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/credits/catalog" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"services":[
			{"service":"sms","bundles":[{"code":"sms_100","units":100,"price_kes":100}]},
			{"service":"ai_credits","bundles":[
				{"code":"bundle_150","units":150,"price_kes":1750},
				{"code":"bundle_60","units":60,"price_kes":750},
				{"code":"broken","units":0,"price_kes":10}]}]}`))
	}))
	defer srv.Close()

	got, err := CatalogBundles(context.Background(), srv.Client(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Code != "bundle_60" || got[1].Code != "bundle_150" || got[0].PriceKES != 750 {
		t.Fatalf("bundles %+v", got)
	}
	if _, err := CatalogBundles(context.Background(), srv.Client(), ""); err == nil {
		t.Fatal("no address is an error")
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if _, err := CatalogBundles(context.Background(), down.Client(), down.URL); err == nil {
		t.Fatal("an outage is an error, so the registry keeps its fallback")
	}
}
