package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

func TestButtonBuilders(t *testing.T) {
	q := quoteButtons("run-1", "4821", true)
	if len(q) != 3 || q[0].Title != btnApprove || q[1].Title != btnCheaper || q[2].Title != btnCancel {
		t.Fatalf("quote buttons: %+v", q)
	}
	if r, ok := osrun.ParseReply(q[0].ID); !ok || r.Kind != osrun.ReplyYes || r.RunID != "run-1" || r.Code != "4821" {
		t.Fatalf("approve id %q = %+v", q[0].ID, r)
	}
	if q2 := quoteButtons("run-1", "4821", false); len(q2) != 2 || q2[1].Title != btnCancel {
		t.Fatalf("no cheaper way: %+v", q2)
	}
	s := storyboardButtons("run-2", "1234")
	if len(s) != 3 || s[0].Title != btnMakeVideo || s[1].Title != btnChange || s[2].Title != btnDiscard {
		t.Fatalf("storyboard buttons: %+v", s)
	}
	for _, b := range append(q, s...) {
		if len([]rune(b.Title)) > 20 {
			t.Fatalf("button title over 20 characters: %q", b.Title)
		}
	}
	if r, _ := osrun.ParseReply(s[2].ID); r.Kind != osrun.ReplyDiscard || r.RunID != "run-2" {
		t.Fatalf("discard id: %+v", r)
	}
}

func TestClarifyListAndMedia(t *testing.T) {
	l := clarifyList("objective")
	if l == nil || len(l.Rows) == 0 || len(l.Rows) > 10 || l.Button == "" {
		t.Fatalf("objective list: %+v", l)
	}
	if r, ok := osrun.ParseReply(l.Rows[0].ID); !ok || r.Kind != osrun.ReplyField || r.Field != "objective" || r.Row != 0 {
		t.Fatalf("row id %q = %+v", l.Rows[0].ID, r)
	}
	if clarifyList("offering") != nil {
		t.Fatal("the offering stays a typed question")
	}
	opts := cheaperOptions(&registry.CreativePlan{Recommended: registry.Option{Credits: 14},
		Alternatives: []registry.Option{{Label: "Posters instead of video", Credits: 6}, {Label: "Bigger", Credits: 20}}})
	if len(opts) != 1 || opts[0].Credits != 6 {
		t.Fatalf("cheaper options: %+v", opts)
	}
}

func (f *mediaFixture) tap(t *testing.T, replyID, title string) WhatsAppAskResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"tenant_id": f.tenant.String(), "tenant_slug": "boi", "user_id": "user-1",
		"sender": "+254700000001", "text": title, "reply_id": replyID, "portal_url": "https://mf.example"})
	rec := httptest.NewRecorder()
	f.h.WhatsAppAsk(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out WhatsAppAskResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// A tapped Approve goes through the same code check as a typed YES: once, for its own run only.
func TestTappedApproveUsesTheCodeCheck(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":22,"enough":true}`}
	code, err := f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1", Credits: 22})
	if err != nil {
		t.Fatal(err)
	}
	// A button from an older run spends nothing.
	if out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, "old-run", code), btnMakeVideo); !strings.Contains(out.Reply, "older message") || len(f.queued) != 0 {
		t.Fatalf("old button: %+v", out)
	}
	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, code), btnMakeVideo)
	if !strings.Contains(out.Reply, "Making your video now") || f.runQueued() != 1 {
		t.Fatalf("make the video: %+v", out)
	}
	if out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, code), btnMakeVideo); !strings.Contains(out.Reply, "expired or was already used") {
		t.Fatalf("second tap: %+v", out)
	}
	// The typed fallback still works for a new code.
	code, _ = f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1"})
	if out := f.whatsApp(t, "ndiyo "+code); !strings.Contains(out.Reply, "Making your video now") {
		t.Fatalf("typed ndiyo: %+v", out)
	}
}

// Cancel and Discard drop the code of their own run only; Cheaper option and Change keep it and
// show the Approve or Make the video button again.
func TestTappedCancelCheaperChange(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	run.Plan.Alternatives = []registry.Option{{Label: "Posters instead of video", Credits: 6, Description: "Two posters."}}
	run.BriefID = "brief-1"
	_ = f.runs.Update(ctx, run)
	issue := func() string {
		code, err := f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionApprove, RunID: run.RunID, TenantID: f.tenant.String(),
			UserID: "user-1", Sender: "+254700000001", Credits: 14})
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	code := issue()
	out := f.tap(t, osrun.ReplyID(osrun.ReplyCheaper, run.RunID, ""), btnCheaper)
	if !strings.Contains(out.Reply, "Posters instead of video: 6 credits") || !strings.Contains(out.Reply, "reply YES "+code) ||
		out.Interactive == nil || out.Interactive.Buttons[0].ID != osrun.ReplyID(osrun.ReplyYes, run.RunID, code) {
		t.Fatalf("cheaper: %+v", out)
	}
	if !f.codes.Pending(ctx, f.tenant.String(), "user-1") {
		t.Fatal("cheaper option must keep the quote's code")
	}

	out = f.tap(t, osrun.ReplyID(osrun.ReplyChange, run.RunID, ""), btnChange)
	if !strings.Contains(out.Reply, "https://mf.example/boi/studio/brief-1") || out.Interactive == nil || out.Interactive.Buttons[0].Title != btnMakeVideo {
		t.Fatalf("change: %+v", out)
	}

	// Cancel for another run leaves this code alone.
	f.tap(t, osrun.ReplyID(osrun.ReplyCancel, "other-run", ""), btnCancel)
	if !f.codes.Pending(ctx, f.tenant.String(), "user-1") {
		t.Fatal("another run's cancel must not drop this code")
	}
	if out := f.tap(t, osrun.ReplyID(osrun.ReplyCancel, run.RunID, ""), btnCancel); !strings.Contains(out.Reply, "Cancelled") {
		t.Fatalf("cancel: %+v", out)
	}
	if f.codes.Pending(ctx, f.tenant.String(), "user-1") {
		t.Fatal("cancel must drop the code")
	}
	issue()
	if out := f.tap(t, osrun.ReplyID(osrun.ReplyDiscard, run.RunID, ""), btnDiscard); !strings.Contains(out.Reply, "Discarded") || f.codes.Pending(ctx, f.tenant.String(), "user-1") {
		t.Fatalf("discard: %+v", out)
	}
}

// A missing field becomes a list; a tapped row completes the waiting request, which then runs
// as a normal turn, and an expired one says so.
func TestClarifyListCompletesTheRequest(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	req := whatsAppAskRequest{UserID: "user-1", Sender: "+254700000001", Text: "make a video ad for my shea butter"}
	out := askResponse{Result: &team.Result{Route: team.RouteClarify, Question: "What result do you want?",
		Spec: &promptopt.TaskSpec{MissingFields: []string{"objective"}}}}
	ia := f.h.offerClarifyList(ctx, f.tenant, req, out, "What result do you want?")
	if ia == nil || ia.List == nil || len(ia.List.Rows) == 0 || !strings.HasPrefix(ia.Body, "What result do you want?") {
		t.Fatalf("clarify list: %+v", ia)
	}

	tap := whatsAppAskRequest{UserID: "user-1", Sender: "+254700000001", Text: "Sales", ReplyID: ia.List.Rows[0].ID}
	if _, handled := f.h.replyTap(ctx, httptest.NewRequest(http.MethodPost, "/", nil), f.tenant, &tap); handled {
		t.Fatal("a completed request must run as a normal turn")
	}
	if tap.Text != "make a video ad for my shea butter\nobjective: sales" || !tap.SessionActive {
		t.Fatalf("merged request: %+v", tap)
	}

	again := whatsAppAskRequest{UserID: "user-1", Sender: "+254700000001", Text: "Sales", ReplyID: ia.List.Rows[0].ID}
	res, handled := f.h.replyTap(ctx, httptest.NewRequest(http.MethodPost, "/", nil), f.tenant, &again)
	if !handled || !strings.Contains(res.Reply, "expired") {
		t.Fatalf("second tap: %+v %v", res, handled)
	}

	// A field that needs the owner's words stays a typed question.
	out.Result.Spec.MissingFields = []string{"offering"}
	if ia := f.h.offerClarifyList(ctx, f.tenant, req, out, "Which product?"); ia != nil {
		t.Fatalf("offering list: %+v", ia)
	}
}

// The storyboard comes as images then Make the video, Change and Discard; the finished ad as a
// video; the plain text stays for outside the 24-hour window.
func TestWhatsAppNotifyInteractive(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":22,"enough":true}`}
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	run.BriefID, run.VersionID = "brief-1", "version-1"
	_ = f.runs.Update(ctx, run)

	rec := post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID, "version_id": "version-1",
		"event": "storyboard_ready", "links": []string{"https://media.example/s1.jpg", "https://media.example/s2.jpg"}})
	var msg whatsAppNotifyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	code := yesLine.FindStringSubmatch(msg.Text)[1]
	ia := msg.Interactive
	// One message with the Studio link and the buttons; no stills as images (owner, 2026-11-14).
	if ia == nil || len(ia.Media) != 0 || !strings.Contains(ia.Body, "/boi/studio/brief-1") || strings.Contains(ia.Body, "s2.jpg") ||
		len(ia.Buttons) != 3 || ia.Buttons[0].ID != osrun.ReplyID(osrun.ReplyYes, run.RunID, code) || !strings.Contains(ia.Body, "reply YES "+code) {
		t.Fatalf("storyboard interactive: %+v", ia)
	}

	rec = post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID, "version_id": "version-1",
		"event": "render_done", "links": []string{"https://media.example/ad.mp4"}})
	msg = whatsAppNotifyResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg.Interactive == nil || len(msg.Interactive.Media) != 1 || msg.Interactive.Media[0].Type != "video" ||
		msg.Interactive.Media[0].Link != "https://media.example/ad.mp4" || !strings.Contains(msg.Interactive.Media[0].Caption, "/boi/studio/brief-1") ||
		!strings.Contains(msg.Text, "https://media.example/ad.mp4") {
		t.Fatalf("finished interactive: %+v", msg)
	}
}

// approval-tenant routes a tap from someone with several businesses by its run or code.
func TestApprovalTenantForTaps(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	code, _ := f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionApprove, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001"})
	ask := func(replyID string) approvalTenantResponse {
		rec := post(f.h.ApprovalTenant, map[string]any{"user_id": "user-1", "sender": "+254700000001", "text": "Approve",
			"reply_id": replyID, "tenant_ids": []string{"00000000-0000-0000-0000-000000000001", f.tenant.String()}})
		var out approvalTenantResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := ask(osrun.ReplyID(osrun.ReplyYes, run.RunID, code)); !out.Yes || out.TenantID != f.tenant.String() {
		t.Fatalf("approve tap: %+v", out)
	}
	if out := ask(osrun.ReplyID(osrun.ReplyCancel, run.RunID, "")); !out.Yes || out.TenantID != f.tenant.String() {
		t.Fatalf("cancel tap: %+v", out)
	}
	if !f.codes.Pending(ctx, f.tenant.String(), "user-1") {
		t.Fatal("routing must not use the code")
	}
}
