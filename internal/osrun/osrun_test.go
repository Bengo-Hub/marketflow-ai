package osrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr, redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

// A quoted turn is kept for the run TTL under a namespaced key, only for its own tenant, and
// an update keeps the time it has left.
func TestStoreSaveGetUpdate(t *testing.T) {
	mr, rdb := newRedis(t)
	s := NewStore(rdb, "mf-ai", 24*time.Hour)
	ctx := context.Background()
	run := &Run{RunID: "r1", TenantID: "t1", UserID: "u1", Channel: ChannelWhatsApp, Sender: "+254700000001",
		Text: "30 second video ad for shea butter", Playbook: "video-script",
		Plan:  &registry.CreativePlan{Recommended: registry.Option{Label: "Recommended", Credits: 14, Tier: registry.TemplateVideo}},
		Steps: []team.Step{{Contract: "video_script", Output: json.RawMessage(`{"scenes":[]}`)}}, Shortlist: []string{"script"}}
	if err := s.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	if !mr.Exists("mf-ai:os:run:r1") {
		t.Fatalf("key not namespaced: %v", mr.Keys())
	}
	if ttl := mr.TTL("mf-ai:os:run:r1"); ttl != 24*time.Hour {
		t.Fatalf("ttl = %v", ttl)
	}
	got, err := s.Get(ctx, "t1", "r1")
	if err != nil || got.Sender != "+254700000001" || got.Plan.Recommended.Credits != 14 || len(got.Steps) != 1 || got.Created.IsZero() {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "t2", "r1"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("another tenant's run: %v", err)
	}
	if _, err := s.Get(ctx, "t1", "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run: %v", err)
	}

	mr.FastForward(time.Hour)
	got.BriefID, got.VersionID = "b1", "v1"
	if err := s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("mf-ai:os:run:r1"); ttl != 23*time.Hour {
		t.Fatalf("update reset the ttl: %v", ttl)
	}
	again, _ := s.Get(ctx, "t1", "r1")
	if again.VersionID != "v1" {
		t.Fatalf("update not kept: %+v", again)
	}

	mr.FastForward(24 * time.Hour)
	if _, err := s.Get(ctx, "t1", "r1"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expired run: %v", err)
	}
	if err := s.Update(ctx, got); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("update of an expired run: %v", err)
	}
}

// A code is 4 digits, bound to the run, the user and the number, works once, expires after
// the code TTL, and a new code or a cancel replaces the pending one.
func TestCodesIssueRedeemExpire(t *testing.T) {
	mr, rdb := newRedis(t)
	c := NewCodes(rdb, "mf-ai", Config{CodeTTL: 30 * time.Minute, CodeAttempts: 3})
	ctx := context.Background()
	a := Approval{Action: ActionApprove, RunID: "r1", TenantID: "t1", UserID: "u1", Sender: "+254700000001", Credits: 14}

	code, err := c.Issue(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 4 || code[0] == '0' {
		t.Fatalf("code = %q", code)
	}
	if ttl := mr.TTL("mf-ai:os:wa-code:t1:u1"); ttl != 30*time.Minute {
		t.Fatalf("code ttl = %v", ttl)
	}
	// Another number holding the same account, or the wrong digits, spend nothing.
	if _, err := c.Redeem(ctx, "t1", "u1", "+254799999999", code); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("other number: %v", err)
	}
	if _, err := c.Redeem(ctx, "t1", "u1", "+254700000001", wrong(code)); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("wrong code: %v", err)
	}
	got, err := c.Redeem(ctx, "t1", "u1", "+254700000001", code)
	if err != nil || got.RunID != "r1" || got.Action != ActionApprove || got.Credits != 14 {
		t.Fatalf("redeem = %+v, %v", got, err)
	}
	// Single use.
	if _, err := c.Redeem(ctx, "t1", "u1", "+254700000001", code); !errors.Is(err, ErrNoCode) {
		t.Fatalf("second use: %v", err)
	}

	// Expiry.
	code, _ = c.Issue(ctx, a)
	mr.FastForward(31 * time.Minute)
	if _, err := c.Redeem(ctx, "t1", "u1", "+254700000001", code); !errors.Is(err, ErrNoCode) {
		t.Fatalf("expired: %v", err)
	}

	// Cancel, and a new code replaces the old one.
	code, _ = c.Issue(ctx, a)
	if !c.Pending(ctx, "t1", "u1") {
		t.Fatal("pending")
	}
	c.Cancel(ctx, "t1", "u1")
	if _, err := c.Redeem(ctx, "t1", "u1", "", code); !errors.Is(err, ErrNoCode) {
		t.Fatalf("cancelled: %v", err)
	}
	seq := []string{"1111", "2222"}
	c.gen = func() (string, error) { s := seq[0]; seq = seq[1:]; return s, nil }
	first, _ := c.Issue(ctx, a)
	second, _ := c.Issue(ctx, Approval{Action: ActionRender, RunID: "r2", TenantID: "t1", UserID: "u1", VersionID: "v1"})
	if _, err := c.Redeem(ctx, "t1", "u1", "", first); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("the replaced code still works: %v", err)
	}
	if got, err := c.Redeem(ctx, "t1", "u1", "", second); err != nil || got.Action != ActionRender || got.VersionID != "v1" {
		t.Fatalf("new code = %+v, %v", got, err)
	}
}

// Three wrong tries cancel the pending code.
func TestCodesWrongTriesCancel(t *testing.T) {
	_, rdb := newRedis(t)
	c := NewCodes(rdb, "mf-ai", Config{CodeTTL: time.Minute, CodeAttempts: 3})
	ctx := context.Background()
	code, _ := c.Issue(ctx, Approval{Action: ActionApprove, RunID: "r1", TenantID: "t1", UserID: "u1"})
	for i := 0; i < 3; i++ {
		if _, err := c.Redeem(ctx, "t1", "u1", "", wrong(code)); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("try %d: %v", i+1, err)
		}
	}
	if _, err := c.Redeem(ctx, "t1", "u1", "", code); !errors.Is(err, ErrNoCode) {
		t.Fatalf("after three wrong tries: %v", err)
	}
}

func wrong(code string) string {
	if code == "1234" {
		return "4321"
	}
	return "1234"
}

func TestParseYes(t *testing.T) {
	for text, want := range map[string]struct {
		code string
		yes  bool
	}{
		"YES 4821":        {"4821", true},
		"yes 4821":        {"4821", true},
		"  Yes, 4821.":    {"4821", true},
		"yes4821":         {"4821", true},
		"Ndio 4821":       {"4821", true},
		"ndiyo - 4821!":   {"4821", true},
		"YES":             {"", true},
		"yes 48":          {"", true},
		"yes 48210":       {"", true},
		"yes please make": {"", false},
		"yesterday 4821":  {"", false},
		"4821":            {"", false},
		"make a video":    {"", false},
	} {
		code, yes := ParseYes(text)
		if code != want.code || yes != want.yes {
			t.Errorf("%q: got (%q, %v) want (%q, %v)", text, code, yes, want.code, want.yes)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg != DefaultConfig() || cfg.RunTTL != 24*time.Hour || cfg.CodeTTL != 30*time.Minute ||
		cfg.MediaTimeout != 5*time.Minute {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	env := map[string]string{"MF_OS_RUN_TTL": "12h", "MF_OS_APPROVAL_CODE_TTL": "bad", "MF_OS_APPROVAL_CODE_ATTEMPTS": "5"}
	cfg, err = ConfigFromEnv(func(k string) string { return env[k] })
	if err == nil || cfg.RunTTL != 12*time.Hour || cfg.CodeTTL != 30*time.Minute || cfg.CodeAttempts != 5 {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
}

func script(scenes ...team.Scene) json.RawMessage {
	b, _ := json.Marshal(team.VideoScript{Format: "demo", Hooks: []string{"Dry skin?"}, Scenes: scenes, CTA: "Order"})
	return b
}

// The script is the owner's pick, or the first usable video_script step; a blocked or
// broken step is skipped.
func TestPickScript(t *testing.T) {
	two := []team.Scene{{ID: "s1", Shot: "product_clip", Asset: 3, Voiceover: "Dry skin?"}, {ID: "s2", Shot: "end_card", Voiceover: "Order today."}}
	run := &Run{Approved: true, Steps: []team.Step{
		{Contract: "ad_copy_batch", Output: json.RawMessage(`{}`)},
		{Contract: "video_script", Error: "blocked by the compliance rules", Output: script(two...)},
		{Contract: "video_script", Output: script(two...)},
		{Contract: "video_script", Output: script(two[0])},
	}}
	if _, i, err := run.PickScript(-1); err != nil || i != 2 {
		t.Fatalf("default pick = %d, %v", i, err)
	}
	if _, _, err := run.PickScript(1); !errors.Is(err, ErrNoScript) {
		t.Fatalf("blocked pick: %v", err)
	}
	if _, _, err := run.PickScript(3); !errors.Is(err, ErrNoScript) {
		t.Fatalf("one-scene pick: %v", err)
	}
	if _, i, err := run.PickScript(2); err != nil || i != 2 {
		t.Fatalf("owner pick = %d, %v", i, err)
	}
	run.Approved = false
	if run.HasVideoScript() {
		t.Fatal("a quote has no script yet")
	}
}

func TestVideoSpec(t *testing.T) {
	cases := []struct {
		run     Run
		seconds int
		kind    string
	}{
		{Run{Text: "make a video ad", Plan: &registry.CreativePlan{Recommended: registry.Option{Tier: registry.TemplateVideo}}}, 30, KindTemplate},
		{Run{Text: "a 45 second video for shea butter"}, 45, KindTemplate},
		{Run{Text: "video ya sekunde 20"}, 20, KindTemplate},
		{Run{Text: "a 90s video"}, 60, KindTemplate},
		{Run{Text: "a 5 sec teaser"}, 15, KindTemplate},
		{Run{Text: "ugc video", Playbook: "ugc-video", Plan: &registry.CreativePlan{Recommended: registry.Option{Tier: registry.Presenter}}}, 30, KindPresenter},
		{Run{Text: "premium presenter", Plan: &registry.CreativePlan{Recommended: registry.Option{Tier: registry.PresenterPremium}}}, 30, KindPresenterPremium},
		{Run{Text: "a 30 second presenter video"}, 30, KindPresenter},
		{Run{Text: "ugc video but use a template", Playbook: "ugc-video"}, 30, KindTemplate},
	}
	for _, c := range cases {
		s, k := c.run.VideoSpec()
		if s != c.seconds || k != c.kind {
			t.Errorf("%q: got %d %s want %d %s", c.run.Text, s, k, c.seconds, c.kind)
		}
	}
}

// A turn's script fits Studio: photo numbers dropped, unknown shots and footage become b_roll,
// presenter scenes stay only for a presenter kind.
func TestFitScript(t *testing.T) {
	v := team.VideoScript{Scenes: []team.Scene{
		{Shot: "presenter", Asset: 0, Voiceover: "Hi"}, {Shot: "owner_footage", Asset: 2}, {Shot: "product_clip", Asset: 5}, {Shot: "end_card"}}}
	tpl := v
	tpl.Scenes = append([]team.Scene(nil), v.Scenes...)
	FitScript(&tpl, KindTemplate)
	want := []string{"b_roll", "b_roll", "product_clip", "end_card"}
	for i, s := range tpl.Scenes {
		if s.Shot != want[i] || s.Asset != 0 {
			t.Fatalf("template scene %d = %+v", i, s)
		}
	}
	pre := v
	pre.Scenes = append([]team.Scene(nil), v.Scenes...)
	FitScript(&pre, KindPresenter)
	if pre.Scenes[0].Shot != "presenter" || pre.Scenes[1].Shot != "b_roll" {
		t.Fatalf("presenter scenes = %+v", pre.Scenes)
	}
}

// One holder at a time; release frees it, and a dead holder's lock ends with its ttl.
func TestStoreLock(t *testing.T) {
	mr, rdb := newRedis(t)
	s := NewStore(rdb, "mf-ai", time.Hour)
	ctx := context.Background()
	release, ok, err := s.Lock(ctx, "media:r1", 5*time.Minute)
	if err != nil || !ok {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	if !mr.Exists("mf-ai:os:lock:media:r1") {
		t.Fatalf("lock key: %v", mr.Keys())
	}
	if _, ok, _ := s.Lock(ctx, "media:r1", 5*time.Minute); ok {
		t.Fatal("a second holder must wait")
	}
	release()
	release2, ok, _ := s.Lock(ctx, "media:r1", 5*time.Minute)
	if !ok {
		t.Fatal("released lock not free")
	}
	// The first holder's late release must not free the second holder's lock.
	release()
	if _, ok, _ := s.Lock(ctx, "media:r1", 5*time.Minute); ok {
		t.Fatal("a stale release freed another holder's lock")
	}
	release2()
	_, _, _ = s.Lock(ctx, "media:r2", time.Minute)
	mr.FastForward(2 * time.Minute)
	if _, ok, _ := s.Lock(ctx, "media:r2", time.Minute); !ok {
		t.Fatal("an expired lock must free itself")
	}
}

// Find routes a bare YES to the business that holds the code, without using it.
func TestCodesFind(t *testing.T) {
	_, rdb := newRedis(t)
	c := NewCodes(rdb, "mf-ai", DefaultConfig())
	ctx := context.Background()
	code, _ := c.Issue(ctx, Approval{Action: ActionApprove, RunID: "r1", TenantID: "t2", UserID: "u1", Sender: "+254700000001"})
	if tid, ok := c.Find(ctx, []string{"t1", "t2"}, "u1", "+254700000001", code); !ok || tid != "t2" {
		t.Fatalf("find = %q %v", tid, ok)
	}
	if _, ok := c.Find(ctx, []string{"t1", "t2"}, "u1", "+254799999999", code); ok {
		t.Fatal("another number must not find the code")
	}
	if _, ok := c.Find(ctx, []string{"t1", "t2"}, "u1", "", wrong(code)); ok {
		t.Fatal("a wrong code finds nothing")
	}
	if got, err := c.Redeem(ctx, "t2", "u1", "+254700000001", code); err != nil || got.RunID != "r1" {
		t.Fatalf("find must not use the code: %+v %v", got, err)
	}
}
