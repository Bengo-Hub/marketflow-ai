package fxrate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// The credit engine's KES per USD comes from treasury-api, the platform's one source of
// exchange rates, cached here; a treasury outage keeps the last good rate.
func TestKESPerUSD(t *testing.T) {
	var calls atomic.Int32
	rate := "130.25"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/s2s/codevertex/exchange-rates/convert" || r.Header.Get("X-API-Key") != "k" ||
			r.URL.Query().Get("from") != "USD" || r.URL.Query().Get("to") != "KES" || r.URL.Query().Get("amount") != "1" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if rate == "" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"from":"USD","to":"KES","amount":"1","converted":"` + rate + `"}`))
	}))
	defer srv.Close()
	mr := miniredis.RunT(t)
	s := New(srv.URL, "k", "codevertex", redis.NewClient(&redis.Options{Addr: mr.Addr()}), "mf")
	ctx := context.Background()

	if r, ok := s.KESPerUSD(ctx); !ok || r != 130.25 {
		t.Fatalf("rate from treasury: %v %v", r, ok)
	}
	if r, _ := s.KESPerUSD(ctx); r != 130.25 || calls.Load() != 1 {
		t.Fatalf("a cached rate is reused: %v after %d calls", r, calls.Load())
	}
	// Treasury down after the cache expires: the last good rate is kept.
	mr.FastForward(defaultCacheFor + 1)
	rate = ""
	if r, ok := s.KESPerUSD(ctx); !ok || r != 130.25 {
		t.Fatalf("last good rate on an outage: %v %v", r, ok)
	}
}

func TestKESPerUSDRejectsNonsense(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"converted":"1"}`)) // no rate configured: treasury returns the amount
	}))
	defer srv.Close()
	if _, ok := New(srv.URL, "k", "codevertex", nil, "mf").KESPerUSD(context.Background()); ok {
		t.Fatal("a rate outside 50-500 KES per USD is not used")
	}
	if _, ok := New("", "", "codevertex", nil, "mf").KESPerUSD(context.Background()); ok {
		t.Fatal("no treasury configured, no rate")
	}
}

// With treasury never answering, the configured default is used; an implausible default is not.
func TestKESPerUSDConfiguredDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	if r, ok := New(srv.URL, "k", "codevertex", rdb, "mf").WithPolicy(0, 129).KESPerUSD(ctx); !ok || r != 129 {
		t.Fatalf("configured default: %v %v", r, ok)
	}
	if _, ok := New(srv.URL, "k", "codevertex", rdb, "mf").WithPolicy(0, 5).KESPerUSD(ctx); ok {
		t.Fatal("an implausible default is ignored")
	}
	if _, ok := New(srv.URL, "k", "codevertex", rdb, "mf").KESPerUSD(ctx); ok {
		t.Fatal("no default configured and no rate ever read")
	}
}
