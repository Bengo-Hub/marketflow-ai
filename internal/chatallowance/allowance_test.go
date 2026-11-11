package chatallowance

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Owner, 2026-11-11: chat is free up to a daily allowance per business, then one credit
// covers each block of five turns.
func TestNeedsCredit(t *testing.T) {
	mr := miniredis.RunT(t)
	a := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "mf", 3, 5)
	ctx := context.Background()
	tid := uuid.New()
	day := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	var got []bool
	for i := 0; i < 14; i++ {
		got = append(got, a.NeedsCredit(ctx, tid, day))
	}
	// 3 free, then a charge on turns 4 and 9 and 14 (one per five).
	want := []bool{false, false, false, true, false, false, false, false, true, false, false, false, false, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn %d: got %v want %v (all %v)", i+1, got[i], want[i], got)
		}
	}
	// A new day starts a new allowance; another tenant has its own.
	if a.NeedsCredit(ctx, tid, day.Add(24*time.Hour)) || a.NeedsCredit(ctx, uuid.New(), day) {
		t.Fatal("the allowance is per business per day")
	}
}

func TestNeedsCreditWithoutRedis(t *testing.T) {
	if !New(nil, "mf", 3, 5).NeedsCredit(context.Background(), uuid.New(), time.Now()) {
		t.Fatal("without Redis every turn is charged, as before")
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close()
	if !New(rdb, "mf", 3, 5).NeedsCredit(context.Background(), uuid.New(), time.Now()) {
		t.Fatal("a Redis error charges the turn")
	}
}
