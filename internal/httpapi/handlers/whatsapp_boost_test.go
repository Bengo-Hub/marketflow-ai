package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/osrun"
)

const (
	boostPath       = "POST /api/v1/internal/ads/os-turn/boost"
	runAdsPlanPathT = "POST " + runAdsPlanPath
	runAdsPathT     = "POST " + runAdsPath
	runAdsOTPPathT  = "POST " + runAdsOTPPath
)

func (f *mediaFixture) callsTo(path string) []studioCall {
	f.studio.mu.Lock()
	defer f.studio.mu.Unlock()
	var out []studioCall
	for _, c := range f.studio.calls {
		if "POST "+c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// buttonCode is the approval code on the reply's button with the given title.
func buttonCode(t *testing.T, out WhatsAppAskResponse, title string) string {
	t.Helper()
	if out.Interactive == nil {
		t.Fatalf("no buttons: %+v", out)
	}
	for _, b := range out.Interactive.Buttons {
		if b.Title == title {
			rep, ok := osrun.ParseReply(b.ID)
			if !ok || rep.Code == "" {
				t.Fatalf("button %q has no code: %q", title, b.ID)
			}
			return rep.Code
		}
	}
	t.Fatalf("no %q button in %+v", title, out.Interactive.Buttons)
	return ""
}

// postedAd posts the finished ad and returns its run and the Boost it code.
func (f *mediaFixture) postedAd(t *testing.T) (*osrun.Run, WhatsAppAskResponse) {
	t.Helper()
	f.studio.replies["POST /api/v1/internal/creative/os-turn/publish"] = studioReply{http.StatusOK,
		`{"posted":[{"platform":"facebook","display_name":"Boi Page"}],"skipped":[]}`}
	run, code := f.finishedAd(t)
	return run, f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, code), btnApprovePost)
}

const planOK = `{"text":"Ad plan for 7 days, total KES 21,000:\n\nMeta: KES 3,000 a day, boosted automatically","ready":true,"fingerprint":"fp-1"}`

// Run ads fetches the plan only; Confirm and run ads runs exactly that plan (its fingerprint) as
// the user, once.
func TestRunAdsFromWhatsApp(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies[runAdsPlanPathT] = studioReply{http.StatusOK, planOK}
	f.studio.replies[runAdsPathT] = studioReply{http.StatusCreated, `{"text":"Meta: Meta boost created.\n\nTikTok: follow the steps"}`}
	run, posted := f.postedAd(t)
	if !strings.Contains(posted.Reply, "Boi Page (facebook)") || !strings.Contains(posted.Reply, "ad plan") {
		t.Fatalf("post reply: %+v", posted)
	}
	plan := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	if q := f.callsTo(runAdsPlanPathT); len(q) != 1 || q[0].Body["version_id"] != "version-1" || q[0].Body["user_id"] != "user-1" || q[0].Key != "int-key" {
		t.Fatalf("plan calls: %+v", q)
	}
	if len(f.callsTo(runAdsPathT)) != 0 || len(f.callsTo(boostPath)) != 0 {
		t.Fatal("the Run ads tap must not run anything")
	}
	if !strings.Contains(plan.Reply, "total KES 21,000") {
		t.Fatalf("plan reply: %+v", plan)
	}
	confirm := buttonCode(t, plan, btnConfirmBoost)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, confirm), btnConfirmBoost)
	calls := f.callsTo(runAdsPathT)
	if len(calls) != 1 || calls[0].Body["fingerprint"] != "fp-1" || calls[0].Body["user_id"] != "user-1" {
		t.Fatalf("run calls: %+v", calls)
	}
	if !strings.Contains(out.Reply, "Meta boost created") || !strings.Contains(out.Reply, "/boi/campaigns") {
		t.Fatalf("run reply: %+v", out)
	}
	if again := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, confirm), btnConfirmBoost); len(f.callsTo(runAdsPathT)) != 1 || !strings.Contains(again.Reply, "expired") {
		t.Fatalf("a run code works once: %+v", again)
	}
}

// Above the tenant's limit marketflow-api asks for its code; the next 6-digit reply runs the plan
// with that code, and a wrong code keeps it waiting.
func TestRunAdsNeedsCode(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies[runAdsPlanPathT] = studioReply{http.StatusOK, planOK}
	f.studio.replies[runAdsPathT] = studioReply{http.StatusPreconditionRequired, `{"error":"otp_required","code":"otp_required","threshold_kes":2000}`}
	f.studio.replies[runAdsOTPPathT] = studioReply{http.StatusOK, `{"otp_id":"otp-9","to_masked":"+2547****001"}`}
	run, posted := f.postedAd(t)
	plan := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, plan, btnConfirmBoost)), btnConfirmBoost)
	if c := f.callsTo(runAdsOTPPathT); len(c) != 1 || c[0].Body["fingerprint"] != "fp-1" || !strings.Contains(out.Reply, "+2547****001") {
		t.Fatalf("code request: %+v", out)
	}

	f.studio.replies[runAdsPathT] = studioReply{http.StatusUnprocessableEntity, `{"error":"the code does not match","code":"otp_invalid"}`}
	if out := f.whatsApp(t, "111 111"); !strings.Contains(out.Reply, "does not match") {
		t.Fatalf("wrong code: %+v", out)
	}
	f.studio.replies[runAdsPathT] = studioReply{http.StatusCreated, `{"text":"Google Ads: campaign created."}`}
	out = f.whatsApp(t, "482 915")
	calls := f.callsTo(runAdsPathT)
	last := calls[len(calls)-1]
	if last.Body["otp_id"] != "otp-9" || last.Body["otp_code"] != "482915" || !strings.Contains(out.Reply, "campaign created") {
		t.Fatalf("run with code: %+v %+v", last, out)
	}
	if out := f.whatsApp(t, "482915"); strings.Contains(out.Reply, "campaign created") {
		t.Fatalf("the wait ends once the plan ran: %+v", out)
	}
}

// A plan that cannot run yet is shown with what to fix, and nothing is offered to confirm; a plan
// that changed before the confirm spends nothing.
func TestRunAdsNotReadyAndChanged(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies[runAdsPlanPathT] = studioReply{http.StatusOK, `{"text":"Google Ads: To do first: Connect your Google Ads account","ready":false,"fingerprint":"fp-2"}`}
	run, posted := f.postedAd(t)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	if !strings.Contains(out.Reply, "Connect your Google Ads account") || out.Interactive != nil {
		t.Fatalf("not ready: %+v", out)
	}

	g := newMediaFixture(t)
	g.studio.replies[runAdsPlanPathT] = studioReply{http.StatusOK, planOK}
	g.studio.replies[runAdsPathT] = studioReply{http.StatusConflict, `{"error":"the plan changed","code":"plan_changed"}`}
	run, posted = g.postedAd(t)
	plan := g.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	out = g.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, plan, btnConfirmBoost)), btnConfirmBoost)
	if !strings.Contains(out.Reply, "changed since I sent it") || !strings.Contains(out.Reply, "nothing was spent") {
		t.Fatalf("changed: %+v", out)
	}
}
