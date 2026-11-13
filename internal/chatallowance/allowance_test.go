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

// The usage card reads today's turns without counting one, and the reset is the next UTC
// midnight (03:00 in Nairobi).
func TestStatus(t *testing.T) {
	mr := miniredis.RunT(t)
	a := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "mf", 50, 5)
	ctx := context.Background()
	tid := uuid.New()
	now := time.Date(2026, 11, 12, 21, 30, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		a.NeedsCreditFor(ctx, tid, now, 10)
	}
	st := a.Status(ctx, tid, now, 10)
	if st.UsedToday != 3 || st.FreePerDay != 10 || st.FreeLeft != 7 || st.TurnsPerCredit != 5 {
		t.Fatalf("status %+v", st)
	}
	if want := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC); !st.ResetsAt.Equal(want) {
		t.Fatalf("resets at %v, want %v", st.ResetsAt, want)
	}
	if again := a.Status(ctx, tid, now, 10); again.UsedToday != 3 {
		t.Fatal("reading the status must not count a turn")
	}
	if st := New(nil, "mf", 50, 5).Status(ctx, tid, now, 0); st.FreeLeft != 50 {
		t.Fatalf("without Redis: %+v", st)
	}
}

// The free allowance follows the business's plan (subscriptions limit chat_turns_daily).
func TestNeedsCreditPerPlan(t *testing.T) {
	mr := miniredis.RunT(t)
	a := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "mf", 50, 5)
	ctx := context.Background()
	day := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	free, paid := uuid.New(), uuid.New()
	// A free plan allowing 2 turns: the third costs a credit.
	if a.NeedsCreditFor(ctx, free, day, 2) || a.NeedsCreditFor(ctx, free, day, 2) || !a.NeedsCreditFor(ctx, free, day, 2) {
		t.Fatal("a plan's own allowance applies")
	}
	// No limit from the plan: the default allowance.
	for i := 0; i < 50; i++ {
		if a.NeedsCreditFor(ctx, paid, day, 0) {
			t.Fatalf("turn %d is inside the default allowance", i+1)
		}
	}
	if !a.NeedsCreditFor(ctx, paid, day, 0) {
		t.Fatal("past the default allowance a turn costs a credit")
	}
}
