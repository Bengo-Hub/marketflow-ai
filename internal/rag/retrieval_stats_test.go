package rag

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestTrustedRule(t *testing.T) {
	cases := []struct {
		up, down int64
		roll     float64
		want     bool
	}{
		{0, 0, 0.9, true},  // nothing known yet
		{1, 8, 0.9, true},  // under ten ratings
		{6, 4, 0.9, true},  // mostly helpful
		{2, 8, 0.9, false}, // mostly unhelpful
		{2, 8, 0.05, true}, // still tried on the exploring share
	}
	for _, c := range cases {
		if got := trusted(c.up, c.down, c.roll); got != c.want {
			t.Errorf("trusted(%d, %d, %.2f) = %v, want %v", c.up, c.down, c.roll, got, c.want)
		}
	}
}

func TestRecordRatingMovesOneCount(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	p := &Pipeline{redis: rdb, redisNamespace: "test"}
	ctx := context.Background()
	tid := uuid.New()

	p.RecordRating(ctx, tid, "product_qa", RouteKBDirect, "", true)
	p.RecordRating(ctx, tid, "product_qa", RouteKBDirect, "up", true)  // same rating again: no change
	p.RecordRating(ctx, tid, "product_qa", RouteKBDirect, "up", false) // changed its mind
	key := p.statsKey(tid)
	if up, down := mr.HGet(key, "product_qa|kb_direct|up"), mr.HGet(key, "product_qa|kb_direct|down"); up != "0" || down != "1" {
		t.Fatalf("up %q down %q", up, down)
	}

	// Ten unhelpful ratings switch the direct route off, apart from the exploring share.
	for i := 0; i < 9; i++ {
		p.RecordRating(ctx, tid, "product_qa", RouteKBDirect, "", false)
	}
	off := 0
	for i := 0; i < 200; i++ {
		if !p.routeTrusted(ctx, tid, "product_qa", RouteKBDirect) {
			off++
		}
	}
	if off < 150 {
		t.Fatalf("a distrusted route answered %d of 200 questions", 200-off)
	}
	if !p.routeTrusted(ctx, tid, "faq", RouteKBDirect) {
		t.Fatal("another question kind was affected")
	}
}
