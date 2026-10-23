package team

import (
	"context"
	"errors"
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

// A guided flow pins its playbook whatever the decision picked; media
// playbooks cannot be pinned past the Creative Plan approval.
func TestPinnedPlaybook(t *testing.T) {
	spec := campaignSpec() // the decision picked a campaign plan
	rt, f := newRuntime(spec, func(sys, _ string) string {
		return `{"positioning":"Shea lotion from Kisumu","audience":["women 25-45"],"offers":["Lotion KES 850"],"proof":[],"voice":"warm","voice_examples":["Karibu"],"banned_claims":["cures eczema"],"learnings":[]}`
	}, fitDecider{})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "We sell shea lotion", Playbook: "brand-context", CreditsAvailable: -1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Spec.Playbook != "brand-context" || res.Spec.Route != "pinned" || len(res.Steps) != 1 || res.Steps[0].Contract != "brand_context" || res.Steps[0].Output == nil {
		t.Fatalf("pinned run = spec %+v steps %+v", res.Spec, res.Steps)
	}
	if strings.Contains(f.systems[0], "Break the owner's request") {
		t.Fatal("a pinned flow must not call the manager planner")
	}
	if _, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", Playbook: "ad-creative"}); !errors.Is(err, ErrBadPlaybook) {
		t.Fatalf("pinning a media playbook = %v", err)
	}
	if _, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", Playbook: "nope"}); !errors.Is(err, ErrBadPlaybook) {
		t.Fatalf("pinning an unknown playbook = %v", err)
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
