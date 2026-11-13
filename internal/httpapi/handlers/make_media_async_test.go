package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/osrun"
)

// quoteFor is a WhatsApp quote whose approval already produced the approved run, so the
// background approval goes straight to make-media (the team runtime is not in these tests).
func (f *mediaFixture) quoteFor(t *testing.T, approved *osrun.Run) *osrun.Run {
	t.Helper()
	q := *approved
	q.RunID, q.Approved, q.Steps, q.ApprovedRunID = uuid.NewString(), false, nil, approved.RunID
	if err := f.runs.Save(context.Background(), &q); err != nil {
		t.Fatal(err)
	}
	return &q
}

func (f *mediaFixture) approveCode(t *testing.T, quote *osrun.Run) string {
	t.Helper()
	code, err := f.codes.Issue(context.Background(), osrun.Approval{Action: osrun.ActionApprove, RunID: quote.RunID,
		TenantID: f.tenant.String(), UserID: "user-1", Sender: "+254700000001", Credits: 14})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// A right approval code answers at once; the brief, video ad and storyboard are made in the
// background. While one approval of the quote runs, another makes nothing; once made, another
// YES makes nothing new.
func TestWhatsAppApproveInBackground(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	approved := f.approvedRun(t, osrun.ChannelWhatsApp)
	quote := f.quoteFor(t, approved)

	out := f.whatsApp(t, "YES "+f.approveCode(t, quote))
	if out.Reply != approvedReply || !out.Pass || len(f.studio.calls) != 0 || len(f.queued) != 1 {
		t.Fatalf("the approval must answer before any work: %+v %v", out, f.studio.paths())
	}
	first := f.queued
	f.queued = nil

	// A second YES while the first is still running (its lock held) makes nothing.
	release, ok, _ := f.runs.Lock(ctx, "approve:"+quote.RunID, time.Minute)
	if !ok {
		t.Fatal("lock")
	}
	f.whatsApp(t, "YES "+f.approveCode(t, quote))
	if n := f.runQueued(); n != 1 || len(f.studio.calls) != 0 {
		t.Fatalf("a second approval while one runs made: %d %v", n, f.studio.paths())
	}
	release()

	// The first approval's work.
	f.queued = first
	f.runQueued()
	want := []string{"POST /api/v1/creative/briefs", "POST /api/v1/creative/briefs/brief-1/video-ads", "POST /api/v1/creative/versions/version-1/storyboard"}
	if got := f.studio.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("background studio calls = %v", got)
	}
	meta, _ := f.studio.calls[0].Body["metadata"].(map[string]any)
	if meta["channel"] != "whatsapp" || meta["run_id"] != approved.RunID {
		t.Fatalf("brief metadata = %v", meta)
	}

	// Done: another YES (or a retry) makes no second brief and reports nothing.
	f.whatsApp(t, "YES "+f.approveCode(t, quote))
	f.runQueued()
	if len(f.studio.calls) != 3 || len(f.events()) != 0 {
		t.Fatalf("a repeated approval made more: %v", f.studio.paths())
	}
}

// A background failure (brand images missing) reaches the requester through media_failed with
// the plain reason and the upload link, and marketflow-api's ask-back returns it as the message.
func TestWhatsAppApproveFailureReachesNotify(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies["POST /api/v1/creative/briefs/brief-1/video-ads"] = studioReply{http.StatusConflict,
		`{"error":"Upload at least 3 brand images first.","code":"brand_images_required","details":{"min":3}}`}
	approved := f.approvedRun(t, osrun.ChannelWhatsApp)
	quote := f.quoteFor(t, approved)

	if out := f.whatsApp(t, "YES "+f.approveCode(t, quote)); out.Reply != approvedReply {
		t.Fatalf("reply: %+v", out)
	}
	f.runQueued()
	ev := f.events()
	if len(ev) != 1 {
		t.Fatalf("events: %v", f.studio.paths())
	}
	text, _ := ev[0].Body["error"].(string)
	if ev[0].Key != "int-key" || ev[0].Body["event"] != "media_failed" || ev[0].Body["run_id"] != approved.RunID ||
		ev[0].Body["tenant_id"] != f.tenant.String() ||
		!strings.Contains(text, "at least 3 brand images") || !strings.Contains(text, "https://mf.example/boi/onboarding") {
		t.Fatalf("failure event: %+v", ev[0])
	}

	rec := post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": approved.RunID,
		"event": "media_failed", "error": text})
	var msg whatsAppNotifyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if rec.Code != http.StatusOK || msg.To != "+254700000001" || msg.Text != text {
		t.Fatalf("media_failed message: %d %+v", rec.Code, msg)
	}
}

// The worker finds the business a bare "YES <code>" is for, without using the code.
func TestApprovalTenant(t *testing.T) {
	f := newMediaFixture(t)
	other := uuid.NewString()
	approved := f.approvedRun(t, osrun.ChannelWhatsApp)
	code := f.approveCode(t, f.quoteFor(t, approved))
	ask := func(text string) approvalTenantResponse {
		rec := post(f.h.ApprovalTenant, map[string]any{"user_id": "user-1", "sender": "+254700000001", "text": text,
			"tenant_ids": []string{other, f.tenant.String()}})
		var out approvalTenantResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if got := ask("YES " + code); !got.Yes || got.TenantID != f.tenant.String() {
		t.Fatalf("found = %+v", got)
	}
	if got := ask("YES " + wrongCode(code)); !got.Yes || got.TenantID != "" {
		t.Fatalf("wrong code = %+v", got)
	}
	if got := ask("make a video"); got.Yes {
		t.Fatalf("not a yes = %+v", got)
	}
	if !f.codes.Pending(context.Background(), f.tenant.String(), "user-1") {
		t.Fatal("the lookup must not use the code")
	}
}
