package adintel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeSerpApi answers every search with one ad and counts the outbound calls.
func fakeSerpApi(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ad_creatives":[{"advertiser_id":"AR1","advertiser":"Rival","ad_creative_id":"CR1","format":"video","first_shown":1700000000,"last_shown":1700500000,"total_days_shown":30}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testSearcher(t *testing.T, base string, cfg Settings) *Searcher {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := NewSearcher(func(context.Context) Settings { return cfg }, rdb, "test")
	s.Base = base
	return s
}

// The platform pool refuses the search past the cap without an outbound call.
func TestPoolCapRefusesWithoutCall(t *testing.T) {
	srv, calls := fakeSerpApi(t)
	s := testSearcher(t, srv.URL, Settings{Key: "k", PaidCap: 2, TenantShare: 10})
	ctx := context.Background()
	for i, q := range []string{"a.co.ke", "b.co.ke"} {
		if _, err := s.Search(ctx, "t1", q, ""); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if _, err := s.Search(ctx, "t1", "c.co.ke", ""); !errors.Is(err, ErrQuota) {
		t.Fatalf("the search past the pool must be refused: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("the refused search must not call SerpApi: %d calls", n)
	}
}

// Two tenants asking for the same advertiser in a week cause one outbound search.
func TestSharedCacheOneSearch(t *testing.T) {
	srv, calls := fakeSerpApi(t)
	s := testSearcher(t, srv.URL, Settings{Key: "k"})
	ctx := context.Background()
	for _, tenant := range []string{"t1", "t2"} {
		ads, err := s.Search(ctx, tenant, "Rival.co.ke", "")
		if err != nil || len(ads) != 1 {
			t.Fatalf("%s: %v %v", tenant, ads, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("one outbound search expected, got %d", n)
	}
}

// A tenant that used its share stops only itself, and its cached answers stay free.
func TestTenantShareStopsOnlyThatTenant(t *testing.T) {
	srv, calls := fakeSerpApi(t)
	s := testSearcher(t, srv.URL, Settings{Key: "k", TenantShare: 1})
	ctx := context.Background()
	if _, err := s.Search(ctx, "t1", "a.co.ke", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(ctx, "t1", "b.co.ke", ""); !errors.Is(err, ErrTenantShare) || errors.Is(err, ErrQuota) {
		t.Fatalf("the second search is the tenant's own limit, not the pool's: %v", err)
	}
	if _, err := s.Search(ctx, "t1", "a.co.ke", ""); err != nil {
		t.Fatalf("a cached answer is free even with the share used: %v", err)
	}
	if _, err := s.Search(ctx, "t2", "b.co.ke", ""); err != nil {
		t.Fatalf("another tenant still searches: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("expected 2 outbound searches, got %d", n)
	}
}
