package chatallowance

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Owner, 2026-11-14: a plan that includes no free chat turns (PowerSuite Basic) gets none, while an
// unknown plan keeps the default; the plan's own number wins when set.
func TestPlanFreeTurns(t *testing.T) {
	cases := []struct {
		name            string
		hasPlan, exempt bool
		turns, want     int
	}{
		{"plan sets turns", true, false, 60, 60},
		{"plan sets none", true, false, 0, NoFreeTurns},
		{"platform owner", true, true, 0, 0},
		{"plan unknown", false, false, 0, 0},
	}
	for _, c := range cases {
		if got := PlanFreeTurns(c.hasPlan, c.exempt, c.turns); got != c.want {
			t.Fatalf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestNoFreeTurnsChargesFromTheFirstTurn(t *testing.T) {
	mr := miniredis.RunT(t)
	a := New(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "mf", DefaultFreePerDay, 5)
	ctx := context.Background()
	tid := uuid.New()
	day := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	var got []bool
	for i := 0; i < 6; i++ {
		got = append(got, a.NeedsCreditFor(ctx, tid, day, NoFreeTurns))
	}
	// One credit covers turns 1 to 5, the next one turn 6.
	want := []bool{true, false, false, false, false, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn %d: got %v want %v (all %v)", i+1, got[i], want[i], got)
		}
	}
	if st := a.Status(ctx, tid, day, NoFreeTurns); st.FreePerDay != 0 || st.FreeLeft != 0 {
		t.Fatalf("status with no free turns: %+v", st)
	}
}
