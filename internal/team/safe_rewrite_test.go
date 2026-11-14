package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
)

// A draft that breaks the business type's rules is written once more with the broken rules
// spelled out; a clean rewrite replaces it, a second bad draft leaves the first for the review,
// and a clean first draft costs no second call (owner, 2026-11-14).
func TestRunStepSafeRewritesOnce(t *testing.T) {
	bad := "Get 20% off amoxicillin this week."
	good := "Visit our pharmacy this week for friendly advice."
	cases := []struct {
		name      string
		replies   []string
		wantText  string
		wantCalls int
	}{
		{"clean first time", []string{good}, good, 1},
		{"clean on rewrite", []string{bad, good}, good, 2},
		{"bad twice", []string{bad, bad}, bad, 2},
	}
	for _, c := range cases {
		calls := 0
		var lastUser string
		r, _ := newRuntime(promptopt.TaskSpec{}, func(system, user string) string {
			lastUser = user
			reply := c.replies[min(calls, len(c.replies)-1)]
			calls++
			return reply
		}, nil)
		pb, ok := r.lib.Playbook("marketing-plan")
		if !ok {
			t.Fatal("marketing-plan playbook missing")
		}
		pb.OutputContract = "" // a plain-text step, checked as one text
		m, _ := r.lib.Member("manager")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		st := r.runStepSafe(ctx, uuid.New(), plannedStep{member: m, playbook: pb, task: "Write a promo line."},
			&promptopt.TaskSpec{}, TenantContext{BusinessType: "pharmacy"}, "", 0)
		cancel()
		if strings.TrimSpace(st.Text) != c.wantText || calls != c.wantCalls {
			t.Errorf("%s: text %q after %d calls", c.name, st.Text, calls)
		}
		if c.wantCalls == 2 && !strings.Contains(lastUser, "YOUR FIRST DRAFT BROKE THESE RULES") {
			t.Errorf("%s: the rewrite was not told which rules broke:\n%s", c.name, lastUser)
		}
		if st.Task != "Write a promo line." {
			t.Errorf("%s: the step's task carries the rewrite note: %q", c.name, st.Task)
		}
	}
}
