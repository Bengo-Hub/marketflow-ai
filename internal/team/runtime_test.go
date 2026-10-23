package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/decide"
	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/google/uuid"
)

// failReply makes fakeLLM answer with a provider outage instead of text.
const failReply = "\x00fail"

// fakeLLM answers by inspecting the system prompt.
type fakeLLM struct {
	mu      sync.Mutex
	reply   func(system, user string) string
	systems []string
}

func (f *fakeLLM) Chat(_ context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	sys, user := "", ""
	for _, m := range req.Messages {
		if m.Role == "system" {
			sys = m.Content
		}
		if m.Role == "user" {
			user = m.Content
		}
	}
	f.mu.Lock()
	f.systems = append(f.systems, sys)
	f.mu.Unlock()
	content := f.reply(sys, user)
	if content == failReply {
		return nil, &ai.StatusError{Provider: "fake", Code: 503, Body: "down"}
	}
	r := &ai.ChatResponse{}
	r.Choices = append(r.Choices, struct {
		Index   int        `json:"index"`
		Message ai.Message `json:"message"`
		Finish  string     `json:"finish_reason"`
	}{Message: ai.Message{Content: content}})
	return r, nil
}

func (f *fakeLLM) ChatStream(context.Context, ai.ChatRequest) (<-chan ai.StreamDelta, error) {
	return nil, errors.New("unused")
}

type oneSource struct{ c ai.ChatClient }

func (s oneSource) Provider(context.Context, string) (llm.Provider, bool) {
	return llm.Provider{Client: s.c, Models: llm.Models{Main: "m", Fallback: "m", Router: "m"}}, true
}

type fixedOpt struct{ spec promptopt.TaskSpec }

func (f fixedOpt) Optimize(context.Context, string, string, promptopt.Context) (*promptopt.TaskSpec, error) {
	s := f.spec
	return &s, nil
}

// fitDecider scores fit from a map and says yes to nothing else.
type fitDecider struct{ fit map[string]float64 }

func (d fitDecider) Decide(_ context.Context, _ string, _ string, qs map[string]decide.Question) (decide.Answers, decide.Usage, error) {
	out := decide.Answers{}
	for id, q := range qs {
		switch q.Type {
		case decide.KindScore:
			out[id] = decide.Answer{Type: q.Type, Score: d.fit[strings.TrimPrefix(id, "fit_")], Confidence: 1}
		case decide.KindNoul:
			out[id] = decide.Answer{Type: q.Type, Prob: 0.05, Confidence: 0.9}
		}
	}
	return out, decide.Usage{}, nil
}

type ctxSource struct{ tc TenantContext }

func (c ctxSource) Load(context.Context, uuid.UUID) (TenantContext, error) { return c.tc, nil }

func newRuntime(spec promptopt.TaskSpec, llmFn func(string, string) string, d promptopt.Decider) (*Runtime, *fakeLLM) {
	f := &fakeLLM{reply: llmFn}
	router := llm.NewRouter(llm.Config{Providers: oneSource{f}, Defaults: llm.DefaultPolicy(llm.Defaults{})})
	return New(Config{
		Library: library.MustLoad(),
		Opt:     fixedOpt{spec},
		Router:  router,
		Decider: d,
		Source:  ctxSource{TenantContext{BusinessType: "generic", MarketingContext: `{"positioning":"Shea butter lotion made in Kisumu","offers":["Shea Butter Body Lotion KES 850"]}`, HasPlan: true}},
	}), f
}

func ugcSpec() promptopt.TaskSpec {
	return promptopt.TaskSpec{
		Playbook: "ugc-video", Specialist: "content_creator", Complexity: "few",
		Entities:       promptopt.Entities{Offering: "Shea Butter Body Lotion", BudgetKES: 6000, PeriodDays: 7, Platforms: []string{"facebook", "instagram"}, Language: "sw"},
		OptimisedBrief: "Task: UGC-style video ad.\nOffering: Shea Butter Body Lotion\n", Original: "nataka video",
	}
}

func TestMediaJobWaitsForPlanApproval(t *testing.T) {
	rt, f := newRuntime(ugcSpec(), func(string, string) string { t.Fatal("no model call before approval"); return "" }, fitDecider{})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", CreditsAvailable: 430})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != RouteQuote || res.Plan == nil || res.Plan.Recommended.Credits != 91 || res.Plan.Recommended.Shortlist != 2 {
		t.Fatalf("quote = %+v plan=%+v", res.Route, res.Plan)
	}
	if len(f.systems) != 0 {
		t.Fatal("model was called")
	}
	// Short on credits after approval: still a quote, with a bundle.
	res, _ = rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", ApprovePlan: true, CreditsAvailable: 40})
	if res.Route != RouteQuote || res.Plan.SuggestedBundle == nil {
		t.Fatalf("short balance = %+v", res.Plan)
	}
}

func TestUGCScriptWithPersonalClaimIsBlocked(t *testing.T) {
	script := `{"format":"presenter","hooks":["Ngozi kavu?","",""],"scenes":[
	 {"id":"s1","duration_s":3,"shot":"presenter","visual":"presenter","voiceover":"I have been using this lotion for a month","caption":"Ngozi kavu?"},
	 {"id":"s2","duration_s":4,"shot":"product_clip","visual":"lotion spread","voiceover":"","caption":"KES 850"}],"cta":"WhatsApp"}`
	rt, f := newRuntime(ugcSpec(), func(string, string) string { return script }, fitDecider{})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", ApprovePlan: true, CreditsAvailable: 430})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != RouteDone || res.Steps[0].Error == "" || res.Steps[0].Output != nil {
		t.Fatalf("personal-use script not blocked: %+v", res.Steps[0])
	}
	found := false
	for _, fd := range res.Findings {
		if fd.Severity == "high" && strings.Contains(fd.Issue, "personal use") {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v", res.Findings)
	}
	sys := f.systems[0]
	for _, want := range []string{"HOUSE RULES", "PLAYBOOK: UGC-style video ad", "never claims to be a customer", `"shot"`} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system prompt missing %q", want)
		}
	}
}

func TestAdCopyBatchShortlistAndCompliance(t *testing.T) {
	spec := ugcSpec()
	spec.Playbook, spec.Specialist = "ad-creative", "ads_specialist"
	batch := `{"variants":[
	 {"id":"v1","angle":"pain","hook":"Dry skin this cold?","headline":"Soft skin all day","primary_text":"Shea butter lotion, KES 850.","cta":"Order on WhatsApp","hypothesis":"beats v2 on CTR"},
	 {"id":"v2","angle":"outcome","hook":"Miracle lotion","headline":"Cures dry skin","primary_text":"It cures everything.","cta":"Buy","hypothesis":"x"},
	 {"id":"v3","angle":"price","hook":"Under KES 1,000","headline":"Made in Kisumu","primary_text":"Local shea butter.","cta":"Order now","hypothesis":"y"},
	 {"id":"v4","angle":"proof","hook":"Locals love it","headline":"Kisumu's lotion","primary_text":"Rich shea butter.","cta":"Order","hypothesis":"z"}]}`
	var users []string
	rt, _ := newRuntime(spec, func(_, user string) string { users = append(users, user); return batch }, fitDecider{fit: map[string]float64{"v1": 1, "v3": 2, "v4": 0.5}})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", ApprovePlan: true, CreditsAvailable: 430, Tier: "poster"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.CopyBatchRun || len(res.Shortlist) != 2 || res.Shortlist[0] != "v3" || res.Shortlist[1] != "v1" {
		t.Fatalf("shortlist = %v", res.Shortlist)
	}
	var out AdCopyBatch
	_ = json.Unmarshal(res.Steps[0].Output, &out)
	for _, v := range out.Variants {
		if v.ID == "v2" {
			t.Fatal("the cure claim was not dropped")
		}
	}
	if !strings.Contains(users[0], "Write exactly 6 variants") {
		t.Fatalf("variant count not passed: %s", users[0])
	}
	if !strings.Contains(users[0], "BRAND CONTEXT") {
		t.Fatal("brand context missing")
	}
}

func TestClarifyAndChatRoutes(t *testing.T) {
	spec := ugcSpec()
	spec.MissingFields, spec.ClarifyingQuestion = []string{"offering"}, "Ni bidhaa gani?"
	rt, _ := newRuntime(spec, func(string, string) string { return "" }, fitDecider{})
	res, _ := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x"})
	if res.Route != RouteClarify || res.Question != "Ni bidhaa gani?" {
		t.Fatalf("clarify = %+v", res)
	}
	spec = ugcSpec()
	spec.MissingFields, spec.Playbook, spec.Specialist = nil, promptopt.GeneralQuestion, "manager"
	rt, _ = newRuntime(spec, func(string, string) string { return "" }, fitDecider{})
	res, _ = rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x"})
	if res.Route != RouteChat {
		t.Fatalf("chat = %+v", res)
	}
}

func TestManagerPlanSkipsMediaAndRunsSteps(t *testing.T) {
	spec := promptopt.TaskSpec{Playbook: "marketing-plan", Specialist: "manager", Complexity: "campaign", OptimisedBrief: "Get 100 customers in 90 days", Original: "plan"}
	rt, f := newRuntime(spec, func(sys, _ string) string {
		if strings.Contains(sys, "Break the owner's request") {
			return `{"steps":[{"specialist":"strategist","playbook":"marketing-plan","task":"Write the 90-day plan"},{"specialist":"ads_specialist","playbook":"ad-creative","task":"ads"},{"specialist":"content_creator","playbook":"content-strategy","task":"Weekly calendar"}],"needs_plan":["ad-creative"]}`
		}
		if strings.Contains(sys, "PLAYBOOK: Content strategy") {
			return `{"pillars":["care"],"items":[{"day":"Mon","platform":"instagram","pillar":"care","format":"reel","hook":"Dry skin?","cta":"WhatsApp","ad_candidate":true}]}`
		}
		return "## 90-day plan\n- Week 1: set up WhatsApp catalog."
	}, fitDecider{})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "plan", CreditsAvailable: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 || res.Steps[0].Playbook != "marketing-plan" || res.Steps[1].Playbook != "content-strategy" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if res.Steps[0].Text == "" || res.Steps[1].Output == nil {
		t.Fatalf("step outputs = %+v", res.Steps)
	}
	if len(f.systems) != 3 {
		t.Fatalf("model calls = %d, want 3 (plan + 2 steps)", len(f.systems))
	}
}

func TestBriefRanksByDecisionAndWords(t *testing.T) {
	d := fitDecider{fit: map[string]float64{"imp_leads": 2, "imp_spend": 1, "imp_cpl": 1.5, "imp_meetings": 0}}
	rt, f := newRuntime(promptopt.TaskSpec{}, func(_, user string) string {
		if strings.Contains(user, "Meetings") {
			t.Errorf("a low-importance metric reached the writer: %s", user)
		}
		return `{"insights":["Leads fell — check the ad","Cost per lead rose","Spend held"],"actions":["Refresh the hook","Reply to comments","Post a poster"]}`
	}, d)
	out, err := rt.Brief(context.Background(), BriefInput{Language: "en", Metrics: []BriefMetric{
		{Key: "meetings", Label: "Meetings booked", Value: 0, Baseline: 5},
		{Key: "leads", Label: "New leads", Value: 4, Baseline: 10},
		{Key: "spend", Label: "Ad spend", Value: 900, Baseline: 857, Unit: "KES"},
		{Key: "cpl", Label: "Cost per lead", Value: 225, Baseline: 86, Unit: "KES"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Highlights, ",") != "leads,cpl,spend" {
		t.Fatalf("highlights = %v", out.Highlights)
	}
	if len(out.Insights) != 3 || strings.Contains(out.Insights[0], "—") || len(out.Actions) != 3 {
		t.Fatalf("brief = %+v", out)
	}
	if len(f.systems) != 1 {
		t.Fatalf("calls = %d", len(f.systems))
	}
}

func TestDisabledMemberAndInvalidJSONRepair(t *testing.T) {
	spec := ugcSpec()
	spec.Playbook, spec.Specialist = "poster-brief", "content_creator"
	calls := 0
	rt, _ := newRuntime(spec, func(string, string) string {
		calls++
		if calls == 1 {
			return "here is your poster idea"
		}
		return `{"headline":"Soft skin, KES 850","subline":"Shea butter from Kisumu","cta":"Order on WhatsApp","layout":"product hero","image_prompt":"lotion jar on wood","sizes":["1080x1350"]}`
	}, fitDecider{})
	res, err := rt.Ask(context.Background(), Request{TenantID: uuid.New(), Text: "x", ApprovePlan: true, CreditsAvailable: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[0].Error != "" || res.Steps[0].Output == nil || calls != 2 {
		t.Fatalf("repair failed: %+v calls=%d", res.Steps[0], calls)
	}
	m, ok := rt.member("seo_specialist", TenantContext{Members: []MemberOverride{{Key: "seo_specialist", Disabled: true}}})
	if ok {
		t.Fatalf("disabled member returned: %+v", m)
	}
	if _, ok := rt.member("manager", TenantContext{Members: []MemberOverride{{Key: "manager", Disabled: true}}}); !ok {
		t.Fatal("the manager cannot be disabled")
	}
}
