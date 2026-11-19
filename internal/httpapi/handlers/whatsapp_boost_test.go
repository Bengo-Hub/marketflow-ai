package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/osrun"
)

const (
	boostQuotePath = "POST /api/v1/internal/ads/os-turn/boost-quote"
	boostPath      = "POST /api/v1/internal/ads/os-turn/boost"
	boostOTPPath   = "POST /api/v1/internal/ads/os-turn/boost-otp"
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

const quoteOK = `{"quote":{"currency":"KES","budget":3000,"days":7,"shortfall":1000,"launch_credits":10,"ad_account_id":"act_42","account_name":"Boi Ads"},
"objective":"whatsapp","ad_account":"Boi Ads","page":"Boi Page","ready":true}`

// Boost it fetches a quote only; Confirm boost launches exactly the quoted boost as the user, once.
func TestBoostFromWhatsApp(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies[boostQuotePath] = studioReply{http.StatusOK, quoteOK}
	f.studio.replies[boostPath] = studioReply{http.StatusCreated, `{"id":"camp-1","status":"launching","quote":{"shortfall":1000,"currency":"KES","ad_account_id":"act_42"}}`}
	run, posted := f.postedAd(t)
	if !strings.Contains(posted.Reply, "Boi Page (facebook)") {
		t.Fatalf("post reply: %+v", posted)
	}
	boost := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	if q := f.callsTo(boostQuotePath); len(q) != 1 || q[0].Body["version_id"] != "version-1" || q[0].Body["user_id"] != "user-1" || q[0].Key != "int-key" {
		t.Fatalf("quote calls: %+v", q)
	}
	if len(f.callsTo(boostPath)) != 0 {
		t.Fatal("the Boost it tap must not launch anything")
	}
	if !strings.Contains(boost.Reply, "KES 3,000") || !strings.Contains(boost.Reply, "7 days") || !strings.Contains(boost.Reply, "KES 1,000 more") {
		t.Fatalf("quote reply: %+v", boost)
	}
	confirm := buttonCode(t, boost, btnConfirmBoost)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, confirm), btnConfirmBoost)
	calls := f.callsTo(boostPath)
	if len(calls) != 1 || calls[0].Body["budget"] != float64(3000) || calls[0].Body["days"] != float64(7) || calls[0].Body["user_id"] != "user-1" {
		t.Fatalf("boost calls: %+v", calls)
	}
	if !strings.Contains(out.Reply, "paused until Meta approves") || !strings.Contains(out.Reply, "/boi/campaigns/camp-1") ||
		!strings.Contains(out.Reply, "asset_id=42") {
		t.Fatalf("launch reply: %+v", out)
	}
	if again := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, confirm), btnConfirmBoost); len(f.callsTo(boostPath)) != 1 || !strings.Contains(again.Reply, "expired") {
		t.Fatalf("a boost code works once: %+v", again)
	}
}

// Above the tenant's limit marketflow-api asks for its code; the next 6-digit reply launches the
// boost with that code, and a wrong code keeps it waiting.
func TestBoostNeedsCode(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies[boostQuotePath] = studioReply{http.StatusOK, quoteOK}
	f.studio.replies[boostPath] = studioReply{http.StatusPreconditionRequired, `{"error":"otp_required","code":"otp_required","threshold_kes":2000}`}
	f.studio.replies[boostOTPPath] = studioReply{http.StatusOK, `{"otp_id":"otp-9","to_masked":"+2547****001"}`}
	run, posted := f.postedAd(t)
	boost := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, boost, btnConfirmBoost)), btnConfirmBoost)
	if len(f.callsTo(boostOTPPath)) != 1 || !strings.Contains(out.Reply, "+2547****001") {
		t.Fatalf("code request: %+v", out)
	}

	f.studio.replies[boostPath] = studioReply{http.StatusUnprocessableEntity, `{"error":"the code does not match","code":"otp_invalid"}`}
	if out := f.whatsApp(t, "111 111"); !strings.Contains(out.Reply, "does not match") {
		t.Fatalf("wrong code: %+v", out)
	}
	f.studio.replies[boostPath] = studioReply{http.StatusCreated, `{"id":"camp-2","status":"launching","quote":{}}`}
	out = f.whatsApp(t, "482 915")
	calls := f.callsTo(boostPath)
	last := calls[len(calls)-1]
	if last.Body["otp_id"] != "otp-9" || last.Body["otp_code"] != "482915" || !strings.Contains(out.Reply, "Boost created") {
		t.Fatalf("launch with code: %+v %+v", last, out)
	}
	if out := f.whatsApp(t, "482915"); strings.Contains(out.Reply, "Boost created") {
		t.Fatalf("the wait ends once the boost went ahead: %+v", out)
	}
}

// Paid ads switched off: the quote says so and nothing is offered to confirm.
func TestBoostAdsDisabled(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies[boostQuotePath] = studioReply{http.StatusConflict, `{"error":"paid ads are not switched on yet","code":"ads_disabled"}`}
	run, posted := f.postedAd(t)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, buttonCode(t, posted, btnBoost)), btnBoost)
	if !strings.Contains(out.Reply, "not switched on") || out.Interactive != nil {
		t.Fatalf("disabled: %+v", out)
	}
}
