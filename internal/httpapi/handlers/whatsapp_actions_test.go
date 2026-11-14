package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// fakeWallet is subscriptions-api for these tests: a balance and the prompt purchases asked for.
type fakeWallet struct {
	mu        sync.Mutex
	balance   int64
	purchases []string // "code phone ref"
	intent    string
}

func (w *fakeWallet) Spend(context.Context, uuid.UUID, int64, string) (CreditDecision, error) {
	return CreditDecision{Allowed: true}, nil
}
func (w *fakeWallet) Refund(context.Context, uuid.UUID, string) error { return nil }
func (w *fakeWallet) Balance(context.Context, uuid.UUID) (int64, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.balance, false, nil
}
func (w *fakeWallet) PurchaseBundle(_ context.Context, _ uuid.UUID, code, phone, ref string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.purchases = append(w.purchases, code+" "+phone+" "+ref)
	return w.intent, nil
}

// videoRegistry has one video tier ("tv"), a poster tier and the AI bundles on sale.
func videoRegistry(context.Context) registry.Registry {
	return registry.Registry{
		Outputs: map[string]registry.Output{"tv": {Kind: "media", NeedsStoryboard: true}, "poster": {Kind: "media"}},
		Bundles: []registry.Bundle{{Code: "bundle_60", Units: 60, PriceKES: 750}, {Code: "bundle_150", Units: 150, PriceKES: 1750}},
	}
}

var postLine = regexp.MustCompile(`Reply POST (\d{4}) to approve it`)

func (f *mediaFixture) finishedAd(t *testing.T) (*osrun.Run, string) {
	t.Helper()
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	run.BriefID, run.VersionID = "brief-1", "version-1"
	_ = f.runs.Update(ctx, run)
	rec := post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID, "version_id": "version-1",
		"event": "render_done", "links": []string{"https://media.example/ad.mp4"}})
	var msg whatsAppNotifyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	m := postLine.FindStringSubmatch(msg.Text)
	if m == nil {
		t.Fatalf("no POST code in %q", msg.Text)
	}
	return run, m[1]
}

func (f *mediaFixture) publishCalls() []studioCall {
	f.studio.mu.Lock()
	defer f.studio.mu.Unlock()
	var out []studioCall
	for _, c := range f.studio.calls {
		if c.Path == "/api/v1/internal/creative/os-turn/publish" {
			out = append(out, c)
		}
	}
	return out
}

// Approve and post: the tap runs the Studio's approve and organic post through marketflow-api's
// internal route, with the internal key, the version and the approving user; once only.
func TestApproveAndPostFromWhatsApp(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies["POST /api/v1/internal/creative/os-turn/publish"] = studioReply{http.StatusOK,
		`{"posted":[{"platform":"facebook","display_name":"Boi Page"},{"platform":"instagram","display_name":"@boi"}],"skipped":[{"display_name":"Boi TikTok","reason":"posting to TikTok is not switched on yet"}]}`}
	run, code := f.finishedAd(t)

	out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, code), btnApprovePost)
	calls := f.publishCalls()
	if len(calls) != 1 || calls[0].Key != "int-key" || calls[0].Body["version_id"] != "version-1" || calls[0].Body["user_id"] != "user-1" ||
		calls[0].Body["tenant_id"] != f.tenant.String() || calls[0].Body["sender"] != "+254700000001" {
		t.Fatalf("publish calls: %+v", calls)
	}
	if !strings.Contains(out.Reply, "Boi Page (facebook), @boi (instagram)") || !strings.Contains(out.Reply, "Not posted to Boi TikTok") ||
		!strings.Contains(out.Reply, "/boi/publishing") {
		t.Fatalf("publish reply: %+v", out)
	}
	if out := f.tap(t, osrun.ReplyID(osrun.ReplyYes, run.RunID, code), btnApprovePost); len(f.publishCalls()) != 1 || !strings.Contains(out.Reply, "expired") {
		t.Fatalf("a publish code works once: %+v", out)
	}
}

// The typed POST <code> works like the button; a held ad (compliance or restricted category) and a
// business with no connected account post nothing and say why.
func TestTypedPostAndRefusals(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies["POST /api/v1/internal/creative/os-turn/publish"] = studioReply{http.StatusConflict,
		`{"error":"it touches a restricted category (health)","code":"needs_review"}`}
	_, code := f.finishedAd(t)
	if out := f.whatsApp(t, "POST "+code); !strings.Contains(out.Reply, "needs a look") || !strings.Contains(out.Reply, "restricted category (health)") {
		t.Fatalf("held: %+v", out)
	}

	f.studio.replies["POST /api/v1/internal/creative/os-turn/publish"] = studioReply{http.StatusConflict, `{"error":"no accounts","code":"no_accounts"}`}
	_, code = f.finishedAd(t)
	if out := f.whatsApp(t, "post "+code); !strings.Contains(out.Reply, "no connected account") {
		t.Fatalf("no accounts: %+v", out)
	}

	f.studio.replies["POST /api/v1/internal/creative/os-turn/publish"] = studioReply{http.StatusForbidden, `{"error":"forbidden","code":"forbidden"}`}
	_, code = f.finishedAd(t)
	if out := f.whatsApp(t, "POST "+code); !strings.Contains(out.Reply, "Only the business owner") {
		t.Fatalf("role: %+v", out)
	}
}

// Change on the finished ad keeps the publish code and shows Approve and post again.
func TestChangeOnFinishedAd(t *testing.T) {
	f := newMediaFixture(t)
	run, code := f.finishedAd(t)
	out := f.tap(t, osrun.ReplyID(osrun.ReplyChange, run.RunID, ""), btnChange)
	if out.Interactive == nil || len(out.Interactive.Buttons) != 1 || out.Interactive.Buttons[0].ID != osrun.ReplyID(osrun.ReplyYes, run.RunID, code) ||
		!strings.Contains(out.Reply, "reply POST "+code) {
		t.Fatalf("change: %+v", out)
	}
}

var otpDigits = regexp.MustCompile(`^\d{6}$`)

// Above the threshold the approval asks for a one-time code sent to the same number; nothing
// starts until it comes back right.
func TestOTPAboveThreshold(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":140,"enough":true}`}
	code, err := f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1", Credits: 140})
	if err != nil {
		t.Fatal(err)
	}
	out := f.whatsApp(t, "YES "+code)
	if out.Route != "otp" || out.OTP == nil || out.OTP.To != "+254700000001" || !otpDigits.MatchString(out.OTP.Code) ||
		strings.Contains(out.Reply, out.OTP.Code) || len(f.queued) != 0 {
		t.Fatalf("otp step: %+v (queued %d)", out, len(f.queued))
	}
	wrong := "111111"
	if out.OTP.Code == wrong {
		wrong = "222222"
	}
	if res := f.whatsApp(t, wrong); !strings.Contains(res.Reply, "does not match") || len(f.queued) != 0 {
		t.Fatalf("wrong otp: %+v", res)
	}
	if res := f.whatsApp(t, out.OTP.Code); !strings.Contains(res.Reply, "Making your video now") || len(f.queued) != 1 {
		t.Fatalf("right otp: %+v", res)
	}
	if res := f.whatsApp(t, out.OTP.Code); strings.Contains(res.Reply, "Making your video") {
		t.Fatalf("an otp works once: %+v", res)
	}
	// Below the threshold no code is asked.
	code, _ = f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1", Credits: 22})
	if res := f.whatsApp(t, "YES "+code); res.OTP != nil || !strings.Contains(res.Reply, "Making your video now") {
		t.Fatalf("below threshold: %+v", res)
	}
}

// A cheaper option is chosen from the list: a video option gets its own code bound to its tier
// and price; a poster or copy option ends with the MarketFlow link and spends nothing.
func TestChooseCheaperOption(t *testing.T) {
	f := newMediaFixture(t)
	f.h.registry = videoRegistry
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	run.Plan.Alternatives = []registry.Option{{Label: "Shorter video", Tier: "tv", Credits: 9}, {Label: "Posters instead of video", Tier: "poster", Credits: 6}}
	_ = f.runs.Update(ctx, run)

	out := f.tap(t, osrun.ReplyID(osrun.ReplyCheaper, run.RunID, ""), btnCheaper)
	if out.Interactive == nil || out.Interactive.List == nil || len(out.Interactive.List.Rows) != 2 {
		t.Fatalf("choices: %+v", out.Interactive)
	}
	out = f.tap(t, osrun.AltID(run.RunID, 0), "Shorter video")
	p, ok := f.codes.PendingApproval(ctx, f.tenant.String(), "user-1")
	if !ok || p.Tier != "tv" || p.Credits != 9 || p.Option != "Shorter video" || p.RunID != run.RunID ||
		out.Interactive == nil || out.Interactive.Buttons[0].ID != osrun.ReplyID(osrun.ReplyYes, run.RunID, p.Code) ||
		!strings.Contains(out.Reply, "Reply YES "+p.Code+" to approve it (about 9 credits)") {
		t.Fatalf("video option: %+v %+v", out, p)
	}
	out = f.tap(t, osrun.AltID(run.RunID, 1), "Posters instead of video")
	if !strings.Contains(out.Reply, "made in MarketFlow") || !strings.Contains(out.Reply, "/boi/marketing") || out.Interactive != nil {
		t.Fatalf("poster option: %+v", out)
	}
	if out := f.tap(t, osrun.AltID(run.RunID, 5), "Gone"); !strings.Contains(out.Reply, "no longer on offer") {
		t.Fatalf("unknown option: %+v", out)
	}
}

// Short of credits for an approved video: the failure message carries one Buy button for the
// smallest covering bundle; the tap starts the M-Pesa prompt to the chat number; the payment
// resumes the render once the credits are there, and only once.
func TestBuyCreditsAndResume(t *testing.T) {
	f := newMediaFixture(t)
	wallet := &fakeWallet{balance: 10, intent: "intent-9"}
	f.h.credits, f.h.registry = wallet, videoRegistry
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	run.VersionID = "version-1"
	_ = f.runs.Update(ctx, run)
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":62,"enough":false,"balance":10,"shortfall":52}`}
	code, _ := f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1", Credits: 62})
	f.whatsApp(t, "YES "+code)
	f.runQueued()
	ev := f.events()
	if len(ev) != 1 {
		t.Fatalf("events: %v", f.studio.paths())
	}
	text, _ := ev[0].Body["error"].(string)
	if !strings.Contains(text, "Buy 60 credits for KES 750") || !strings.Contains(text, "reply BUY") {
		t.Fatalf("short text: %q", text)
	}
	rec := post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID, "event": "media_failed", "error": text})
	var msg whatsAppNotifyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg.Interactive == nil || len(msg.Interactive.Buttons) != 1 || msg.Interactive.Buttons[0].Title != "Buy 60 credits" {
		t.Fatalf("buy button: %+v", msg)
	}
	if len(wallet.purchases) != 0 {
		t.Fatal("nothing is bought before the tap")
	}

	out := f.tap(t, msg.Interactive.Buttons[0].ID, "Buy 60 credits")
	if len(wallet.purchases) != 1 || !strings.HasPrefix(wallet.purchases[0], "bundle_60 254700000001 ") || !strings.Contains(out.Reply, "M-Pesa prompt for KES 750") {
		t.Fatalf("purchase: %v %+v", wallet.purchases, out)
	}
	if out := f.tap(t, msg.Interactive.Buttons[0].ID, "Buy 60 credits"); len(wallet.purchases) != 1 || !strings.Contains(out.Reply, "nothing was charged") {
		t.Fatalf("a second tap must not buy again: %+v", out)
	}

	// The payment event before the credits land: retry later; after: the render starts, once.
	paid := map[string]any{"tenant_id": f.tenant.String(), "intent_id": "intent-9"}
	if rec := post(f.h.WhatsAppTopupPaid, paid); rec.Code != http.StatusConflict {
		t.Fatalf("not landed: %d", rec.Code)
	}
	wallet.balance = 70
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":62,"enough":true,"balance":70}`}
	rec = post(f.h.WhatsAppTopupPaid, paid)
	msg = whatsAppNotifyResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if rec.Code != http.StatusOK || msg.To != "+254700000001" || !strings.Contains(msg.Text, "60 credits added") || !strings.Contains(msg.Text, "Making your video now") {
		t.Fatalf("resume: %d %+v", rec.Code, msg)
	}
	f.runQueued()
	if n := strings.Count(strings.Join(f.studio.paths(), ","), "POST /api/v1/creative/versions/version-1/render"); n != 1 {
		t.Fatalf("the render starts once after the payment: %v", f.studio.paths())
	}
	rec = post(f.h.WhatsAppTopupPaid, paid)
	msg = whatsAppNotifyResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg.To != "" {
		t.Fatalf("a repeated payment event resumes nothing: %+v", msg)
	}
	// Another tenant's payment is never ours.
	rec = post(f.h.WhatsAppTopupPaid, map[string]any{"tenant_id": uuid.NewString(), "intent_id": "intent-9"})
	if rec.Code != http.StatusOK {
		t.Fatalf("foreign: %d", rec.Code)
	}
}

// A quote the business cannot afford offers the Buy button; once paid, the plan comes back with a
// fresh Approve code instead of running by itself.
func TestBuyFromQuoteComesBackToApprove(t *testing.T) {
	f := newMediaFixture(t)
	wallet := &fakeWallet{balance: 4, intent: "intent-q"}
	f.h.credits, f.h.registry = wallet, videoRegistry
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	plan := &registry.CreativePlan{Recommended: registry.Option{Label: "Recommended", Credits: 14}, Shortfall: 10}
	out := askResponse{RunID: run.RunID, Result: &team.Result{Route: team.RouteQuote, Plan: plan}}
	req := whatsAppAskRequest{UserID: "user-1", Sender: "+254700000001", PortalURL: "https://mf.example", TenantSlug: "boi"}
	text, ia := f.h.offerApprovalCode(ctx, f.tenant, req, out, whatsAppReply(out, "https://mf.example/boi/marketing"))
	if ia == nil || len(ia.Buttons) != 1 || ia.Buttons[0].Title != "Buy 60 credits" || !strings.Contains(text, "reply BUY") || yesLine.MatchString(text) {
		t.Fatalf("short quote: %q %+v", text, ia)
	}
	if res := f.whatsApp(t, "buy"); len(wallet.purchases) != 1 || !strings.Contains(res.Reply, "M-Pesa prompt") {
		t.Fatalf("typed BUY: %+v", res)
	}
	wallet.balance = 64
	rec := post(f.h.WhatsAppTopupPaid, map[string]any{"tenant_id": f.tenant.String(), "intent_id": "intent-q"})
	var msg whatsAppNotifyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg.Interactive == nil || msg.Interactive.Buttons[0].Title != btnApprove || !yesLine.MatchString(msg.Text) || len(f.queued) != 0 {
		t.Fatalf("back to approve: %+v", msg)
	}
}

func TestKES(t *testing.T) {
	for v, want := range map[float64]string{750: "KES 750", 1750: "KES 1,750", 10000: "KES 10,000", 1000000: "KES 1,000,000"} {
		if got := kes(v); got != want {
			t.Errorf("%v = %q", v, got)
		}
	}
	if got := buyTitle(1000); len([]rune(got)) > 20 || got != "Buy 1000 credits" {
		t.Errorf("title %q", got)
	}
}
