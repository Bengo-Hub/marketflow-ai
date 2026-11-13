package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/assistgate"
	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// fakeStudio is marketflow-api's Creative Studio: it records each call and answers from replies
// (keyed "METHOD /path"); anything else is 201 with an id.
type fakeStudio struct {
	mu      sync.Mutex
	calls   []studioCall
	replies map[string]studioReply
}

type studioCall struct {
	Method, Path, Key, Tenant string
	Body                      map[string]any
}

type studioReply struct {
	status int
	body   string
}

func (f *fakeStudio) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		f.mu.Lock()
		f.calls = append(f.calls, studioCall{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("X-API-Key"), Tenant: r.Header.Get("X-Tenant-ID"), Body: body})
		rep, ok := f.replies[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if ok {
			w.WriteHeader(rep.status)
			_, _ = w.Write([]byte(rep.body))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/creative/briefs"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"brief-1"}`))
		case strings.HasSuffix(r.URL.Path, "/video-ads"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"version-1","status":"script_ready"}`))
		default:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"version-1"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeStudio) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

type mediaFixture struct {
	h      *OSHandler
	studio *fakeStudio
	runs   *osrun.Store
	codes  *osrun.Codes
	mr     *miniredis.Miniredis
	tenant uuid.UUID
	// queued is the background work a WhatsApp YES started, held until the test runs it, so a
	// test sees the reply come back before any Studio call.
	queued []func()
}

func newMediaFixture(t *testing.T) *mediaFixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	cfg := osrun.DefaultConfig()
	f := &mediaFixture{studio: &fakeStudio{replies: map[string]studioReply{}}, mr: mr, tenant: uuid.New(),
		runs: osrun.NewStore(rdb, "mf-ai", cfg.RunTTL), codes: osrun.NewCodes(rdb, "mf-ai", cfg)}
	srv := f.studio.server(t)
	f.h = NewOSHandler(OSConfig{Logger: zap.NewNop()}).WithGate(assistgate.New(nil)).
		WithMakeMedia(f.runs, f.codes, cfg, srv.URL, "svc-key", "int-key")
	f.h.spawn = func(fn func()) { f.queued = append(f.queued, fn) }
	return f
}

// runQueued runs the background work started so far, in order, and reports how many ran.
func (f *mediaFixture) runQueued() int {
	work := f.queued
	f.queued = nil
	for _, fn := range work {
		fn()
	}
	return len(work)
}

// events are the media_failed reports sent to marketflow-api.
func (f *mediaFixture) events() []studioCall {
	f.studio.mu.Lock()
	defer f.studio.mu.Unlock()
	var out []studioCall
	for _, c := range f.studio.calls {
		if c.Path == "/api/v1/internal/creative/os-turn/events" {
			out = append(out, c)
		}
	}
	return out
}

func turnScript() json.RawMessage {
	b, _ := json.Marshal(team.VideoScript{Format: "demo", Hooks: []string{"Dry skin?"}, CTA: "Order", Scenes: []team.Scene{
		{ID: "s1", DurationS: 4, Shot: "presenter", Voiceover: "Dry skin in the morning?", Caption: "Dry skin?"},
		{ID: "s2", DurationS: 5, Shot: "product_clip", Asset: 4, Voiceover: "Shea butter from Kisumu.", Caption: "Shea butter"},
		{ID: "s3", DurationS: 3, Shot: "end_card", Voiceover: "Order today.", Caption: "Order today"}}})
	return b
}

func (f *mediaFixture) approvedRun(t *testing.T, channel string) *osrun.Run {
	t.Helper()
	run := &osrun.Run{RunID: uuid.NewString(), TenantID: f.tenant.String(), TenantSlug: "boi", PortalURL: "https://mf.example",
		UserID: "user-1", Sender: "+254700000001", Channel: channel, Approved: true,
		Text: "Make a 45 second video ad for our shea butter", Playbook: "video-script",
		Entities: promptopt.Entities{Offering: "Shea butter 250g", Objective: "sales", Language: "en", Audience: "Mothers in Kisumu"},
		Plan:     &registry.CreativePlan{Recommended: registry.Option{Label: "Recommended", Tier: registry.TemplateVideo, Credits: 14}},
		Steps:    []team.Step{{Contract: "video_script", Playbook: "video-script", Output: turnScript()}}, Shortlist: []string{"script"}}
	if err := f.runs.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

// make-media creates the brief with the plan and where it came from, the video ad with the
// turn's script fitted for a template ad (no script job in Studio), then the storyboard; a
// second call makes nothing new.
func TestMakeMediaStudioCalls(t *testing.T) {
	f := newMediaFixture(t)
	run := f.approvedRun(t, osrun.ChannelPortal)

	rec := post(f.h.InternalMakeMedia, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID})
	if rec.Code != http.StatusOK {
		t.Fatalf("make-media: %d %s", rec.Code, rec.Body.String())
	}
	var made madeMedia
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if made.BriefID != "brief-1" || made.VersionID != "version-1" || made.Seconds != 45 || made.Kind != osrun.KindTemplate || made.StudioPath != "/studio/brief-1" {
		t.Fatalf("made = %+v", made)
	}
	want := []string{"POST /api/v1/creative/briefs", "POST /api/v1/creative/briefs/brief-1/video-ads", "POST /api/v1/creative/versions/version-1/storyboard"}
	if got := f.studio.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("studio calls = %v", got)
	}
	for _, c := range f.studio.calls {
		if c.Key != "svc-key" || c.Tenant != f.tenant.String() {
			t.Fatalf("call without the service key and tenant: %+v", c)
		}
	}
	brief := f.studio.calls[0].Body
	meta, _ := brief["metadata"].(map[string]any)
	offering, _ := brief["offering"].(map[string]any)
	if brief["title"] != "Video ad: Shea butter 250g" || offering["name"] != "Shea butter 250g" || brief["objective"] != "sales" ||
		brief["language"] != "en" || meta["source"] != "os_turn" || meta["run_id"] != run.RunID || meta["requested_by"] != "user-1" ||
		meta["channel"] != "portal" || meta["plan"] == nil {
		t.Fatalf("brief body = %v", brief)
	}
	ad := f.studio.calls[1].Body
	script, _ := ad["script"].(map[string]any)
	scenes, _ := script["scenes"].([]any)
	if ad["seconds"] != float64(45) || ad["kind"] != "template" || len(scenes) != 3 {
		t.Fatalf("video ad body = %v", ad)
	}
	first, _ := scenes[0].(map[string]any)
	second, _ := scenes[1].(map[string]any)
	if first["shot"] != "b_roll" || second["asset"] != nil {
		t.Fatalf("script not fitted for a template ad: %v %v", first, second)
	}
	kept, _ := f.runs.Get(context.Background(), f.tenant.String(), run.RunID)
	if kept.BriefID != "brief-1" || kept.VersionID != "version-1" {
		t.Fatalf("made media not kept on the run: %+v", kept)
	}

	// Again: the version exists, so nothing new is made.
	if rec := post(f.h.InternalMakeMedia, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID}); rec.Code != http.StatusOK || len(f.studio.calls) != 3 {
		t.Fatalf("second make-media: %d, %d calls", rec.Code, len(f.studio.calls))
	}

	// A quote that was never approved, and an unknown run.
	quote := f.approvedRun(t, osrun.ChannelPortal)
	quote.Approved = false
	_ = f.runs.Update(context.Background(), quote)
	if rec := post(f.h.InternalMakeMedia, map[string]any{"tenant_id": f.tenant.String(), "run_id": quote.RunID}); rec.Code != http.StatusConflict {
		t.Fatalf("unapproved run: %d", rec.Code)
	}
	if rec := post(f.h.InternalMakeMedia, map[string]any{"tenant_id": f.tenant.String(), "run_id": "nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", rec.Code)
	}
}

// Studio's refusals come back as Studio sent them, so the console shows its brand images step.
func TestMakeMediaBrandImagesRefusal(t *testing.T) {
	f := newMediaFixture(t)
	f.studio.replies["POST /api/v1/creative/briefs/brief-1/video-ads"] = studioReply{http.StatusConflict,
		`{"error":"Upload at least 3 brand images first.","code":"brand_images_required","details":{"min":3}}`}
	run := f.approvedRun(t, osrun.ChannelPortal)
	rec := post(f.h.InternalMakeMedia, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID})
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusConflict || body["code"] != "brand_images_required" {
		t.Fatalf("refusal: %d %v", rec.Code, body)
	}
	if got := f.studio.paths(); len(got) != 2 {
		t.Fatalf("no storyboard after a refusal: %v", got)
	}
	_, err := f.h.makeMedia(context.Background(), f.tenant, run, -1)
	reply := f.h.mediaFailReply(err, whatsAppAskRequest{PortalURL: "https://mf.example", TenantSlug: "boi"}, "make the video")
	if !strings.Contains(reply, "at least 3 brand images") || !strings.Contains(reply, "https://mf.example/boi/onboarding") {
		t.Fatalf("whatsapp reply: %q", reply)
	}
}

func (f *mediaFixture) whatsApp(t *testing.T, text string) WhatsAppAskResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"tenant_id": f.tenant.String(), "tenant_slug": "boi", "user_id": "user-1",
		"sender": "+254700000001", "text": text, "portal_url": "https://mf.example"})
	rec := httptest.NewRecorder()
	f.h.WhatsAppAsk(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out WhatsAppAskResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

var yesLine = regexp.MustCompile(`(?i)reply YES (\d{4}) to (approve|make the video)`)

// A kept quote the tenant can afford ends with "Reply YES <code> to approve (about N credits)".
func TestOfferApprovalCode(t *testing.T) {
	f := newMediaFixture(t)
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	plan := &registry.CreativePlan{Recommended: registry.Option{Label: "Recommended", Credits: 14}}
	out := askResponse{RunID: run.RunID, Result: &team.Result{Route: team.RouteQuote, Plan: plan}}
	req := whatsAppAskRequest{UserID: "user-1", Sender: "+254700000001", PortalURL: "https://mf.example", TenantSlug: "boi"}
	reply := f.h.offerApprovalCode(context.Background(), f.tenant, req, out, whatsAppReply(out, "https://mf.example/boi/marketing"))
	m := yesLine.FindStringSubmatch(reply)
	if m == nil || !strings.HasSuffix(reply, "Reply YES "+m[1]+" to approve (about 14 credits)") ||
		!strings.Contains(reply, "works for 30 minutes") || strings.Contains(reply, "\n\nApprove it in MarketFlow") {
		t.Fatalf("quote reply: %q", reply)
	}
	// Short of credits: no code, the portal link stays.
	plan.Shortfall = 5
	f.codes.Cancel(context.Background(), f.tenant.String(), "user-1")
	reply = f.h.offerApprovalCode(context.Background(), f.tenant, req, out, whatsAppReply(out, "https://mf.example/boi/marketing"))
	if yesLine.MatchString(reply) || f.codes.Pending(context.Background(), f.tenant.String(), "user-1") {
		t.Fatalf("a shortfall gets no code: %q", reply)
	}
}

// A wrong or expired code spends nothing and says so; any other message cancels the code; the
// render code approves the storyboard and starts the render.
func TestWhatsAppApprovalCodes(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	run := f.approvedRun(t, osrun.ChannelWhatsApp)
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":22,"enough":true,"balance":80}`}

	code, err := f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1", Credits: 22})
	if err != nil {
		t.Fatal(err)
	}
	if out := f.whatsApp(t, "YES "+wrongCode(code)); !out.Pass || !strings.Contains(out.Reply, "does not match") || len(f.studio.calls) != 0 {
		t.Fatalf("wrong code: %+v %v", out, f.studio.paths())
	}
	if out := f.whatsApp(t, "yes"); !strings.Contains(out.Reply, "4-digit code") {
		t.Fatalf("yes without a code: %+v", out)
	}
	out := f.whatsApp(t, "YES "+code)
	if !strings.Contains(out.Reply, "Making your video now") || len(f.studio.calls) != 0 {
		t.Fatalf("the render reply must come before any Studio call: %+v %v", out, f.studio.paths())
	}
	if n := f.runQueued(); n != 1 {
		t.Fatalf("background jobs: %d", n)
	}
	want := []string{"GET /api/v1/creative/versions/version-1/quote", "POST /api/v1/creative/versions/version-1/storyboard/approve",
		"POST /api/v1/creative/versions/version-1/render"}
	if got := f.studio.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("render calls = %v", got)
	}
	// Single use.
	if out := f.whatsApp(t, "YES "+code); !strings.Contains(out.Reply, "expired or was already used") || len(f.queued) != 0 {
		t.Fatalf("second use: %+v", out)
	}

	// Expired.
	code, _ = f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", VersionID: "version-1"})
	f.mr.FastForward(31 * time.Minute)
	if out := f.whatsApp(t, "YES "+code); !strings.Contains(out.Reply, "expired") || len(f.queued) != 0 {
		t.Fatalf("expired code: %+v", out)
	}

	// Any other message cancels the pending code (and goes on to the gate as usual).
	_, _ = f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionApprove, RunID: run.RunID, TenantID: f.tenant.String(), UserID: "user-1"})
	f.whatsApp(t, "Habari, is my order on the way?")
	if f.codes.Pending(ctx, f.tenant.String(), "user-1") {
		t.Fatal("another message must cancel the pending code")
	}

	// Short of credits for the render: nothing started; the requester gets the top-up link and a
	// new code through the media_failed path.
	f.studio.replies["GET /api/v1/creative/versions/version-1/quote"] = studioReply{http.StatusOK, `{"credits_needed":22,"enough":false,"balance":10,"shortfall":12}`}
	code, _ = f.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: f.tenant.String(),
		UserID: "user-1", Sender: "+254700000001", VersionID: "version-1"})
	f.whatsApp(t, "YES "+code)
	f.runQueued()
	ev := f.events()
	if len(ev) != 1 {
		t.Fatalf("media_failed events: %v", f.studio.paths())
	}
	text, _ := ev[0].Body["error"].(string)
	if ev[0].Key != "int-key" || ev[0].Body["event"] != "media_failed" || ev[0].Body["run_id"] != run.RunID ||
		!strings.Contains(text, "12 short") || !strings.Contains(text, "/boi/settings/billing") || !yesLine.MatchString(text) {
		t.Fatalf("short event: %+v", ev[0])
	}
	if n := strings.Count(strings.Join(f.studio.paths(), ","), "/render"); n != 1 {
		t.Fatalf("a short tenant must not start the render: %v", f.studio.paths())
	}
}

func wrongCode(code string) string {
	if code == "1234" {
		return "4321"
	}
	return "1234"
}

// The storyboard message carries the stills and a render code bound to the version; the
// finished ad's message carries its link; a portal run sends nothing.
func TestWhatsAppNotify(t *testing.T) {
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
	if rec.Code != http.StatusOK || msg.To != "+254700000001" || !strings.Contains(msg.Text, "2. https://media.example/s2.jpg") ||
		!strings.Contains(msg.Text, "https://mf.example/boi/studio/brief-1") || !strings.HasSuffix(msg.Text, "(about 22 credits)") {
		t.Fatalf("storyboard message: %d %+v", rec.Code, msg)
	}
	code := yesLine.FindStringSubmatch(msg.Text)[1]
	a, err := f.codes.Redeem(ctx, f.tenant.String(), "user-1", "+254700000001", code)
	if err != nil || a.Action != osrun.ActionRender || a.VersionID != "version-1" {
		t.Fatalf("render code = %+v, %v", a, err)
	}

	rec = post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": run.RunID, "version_id": "version-1",
		"event": "render_done", "links": []string{"https://media.example/ad.mp4"}})
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if msg.To != "+254700000001" || !strings.Contains(msg.Text, "https://media.example/ad.mp4") {
		t.Fatalf("finished message: %+v", msg)
	}

	portalRun := f.approvedRun(t, osrun.ChannelPortal)
	rec = post(f.h.WhatsAppNotify, map[string]any{"tenant_id": f.tenant.String(), "run_id": portalRun.RunID, "version_id": "version-1", "event": "render_done",
		"links": []string{"https://media.example/ad.mp4"}})
	msg = whatsAppNotifyResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if rec.Code != http.StatusOK || msg.To != "" {
		t.Fatalf("a portal run sends nothing: %+v", msg)
	}
}

// An approved turn with a video script is kept and makes the console's media actions available;
// a quote is kept without them.
func TestKeepRunMakesMediaActionsAvailable(t *testing.T) {
	f := newMediaFixture(t)
	ctx := context.Background()
	res := &team.Result{Route: team.RouteDone, Spec: &promptopt.TaskSpec{Playbook: "video-script"},
		Plan:        &registry.CreativePlan{Recommended: registry.Option{Tier: registry.TemplateVideo}},
		Steps:       []team.Step{{Contract: "video_script", Output: turnScript()}},
		NextActions: []team.Action{{Key: "storyboard", Sprint: "15"}, {Key: "render", Sprint: "15"}, {Key: "publish", Sprint: "16"}}}
	f.h.keepRun(ctx, f.tenant, askRequest{Text: "video ad", ApprovePlan: true, userID: "user-1"}, "run-1", res)
	if !res.NextActions[0].Available || !res.NextActions[1].Available || res.NextActions[2].Available {
		t.Fatalf("actions = %+v", res.NextActions)
	}
	kept, err := f.runs.Get(ctx, f.tenant.String(), "run-1")
	if err != nil || !kept.Approved || kept.Channel != osrun.ChannelPortal || kept.UserID != "user-1" || kept.Playbook != "video-script" {
		t.Fatalf("kept = %+v, %v", kept, err)
	}

	quote := &team.Result{Route: team.RouteQuote, Plan: &registry.CreativePlan{}, NextActions: []team.Action{{Key: "approve_plan", Available: true}}}
	f.h.keepRun(ctx, f.tenant, askRequest{Text: "video ad", viaWhatsApp: true, sender: "+254700000001"}, "run-2", quote)
	if kept, err := f.runs.Get(ctx, f.tenant.String(), "run-2"); err != nil || kept.Approved || kept.Channel != osrun.ChannelWhatsApp || kept.Sender != "+254700000001" {
		t.Fatalf("quote kept = %+v, %v", kept, err)
	}

	chat := &team.Result{Route: team.RouteChat}
	f.h.keepRun(ctx, f.tenant, askRequest{Text: "hi"}, "run-3", chat)
	if _, err := f.runs.Get(ctx, f.tenant.String(), "run-3"); err == nil {
		t.Fatal("a turn without a plan is not kept")
	}
}
