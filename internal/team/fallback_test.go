package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/google/uuid"
)

func campaignSpec() promptopt.TaskSpec {
	return promptopt.TaskSpec{Playbook: "marketing-plan", Specialist: "manager", Complexity: "campaign",
		OptimisedBrief: "Get 100 customers in 90 days", Original: "plan"}
}

// When every planning model fails, the request still gets an answer from the
// strategist instead of an error.
func TestManagerPlanOutageFallsBackToStrategist(t *testing.T) {
	rt, _ := newRuntime(campaignSpec(), func(sys, _ string) string {
		if strings.Contains(sys, "Break the owner's request") {
			return failReply
		}
		return "## 90-day plan\n- Week 1: set up the WhatsApp catalog."
	}, fitDecider{})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "plan", CreditsAvailable: -1})
	if err != nil {
		t.Fatalf("a planning outage failed the request: %v", err)
	}
	if res.Route != RouteDone || len(res.Steps) != 1 || res.Steps[0].Member != "strategist" || res.Steps[0].Text == "" {
		t.Fatalf("want the strategist fallback, got %+v", res.Steps)
	}
}

// Steps that would start with too little time left are skipped with a note,
// and the request returns in time with what was done.
func TestStepsSkippedWhenOutOfTime(t *testing.T) {
	prevBudget, prevStep := askBudget, minStepTime
	askBudget, minStepTime = 2*time.Second, 1500*time.Millisecond
	t.Cleanup(func() { askBudget, minStepTime = prevBudget, prevStep })

	rt, _ := newRuntime(campaignSpec(), func(sys, _ string) string {
		if strings.Contains(sys, "Break the owner's request") {
			return `{"steps":[{"specialist":"strategist","playbook":"marketing-plan","task":"plan"},{"specialist":"content_creator","playbook":"content-strategy","task":"calendar"}]}`
		}
		time.Sleep(700 * time.Millisecond) // a slow specialist
		return "## plan"
	}, fitDecider{})
	start := time.Now()
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "plan", CreditsAvailable: -1})
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > askBudget+time.Second {
		t.Fatalf("request ran %v past its budget", el)
	}
	if len(res.Steps) != 2 || res.Steps[1].Error != skippedForTime {
		t.Fatalf("second step should be skipped for time: %+v", res.Steps)
	}
}
