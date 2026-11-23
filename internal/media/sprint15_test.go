package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func signWebhook(secret, id, ts string, body []byte) string {
	key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "." + string(body)))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Standard Webhooks: whsec_ base64 key, "{id}.{ts}.{body}", any v1 entry, five minutes.
func TestVerifyStandardWebhook(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	old := "whsec_" + base64.StdEncoding.EncodeToString([]byte("an older signing key for rotation"))
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"request_id":"r1","status":"completed","error":null,"payload":{"video":{"url":"https://cdn/v.mp4"}}}`)
	sig := signWebhook(secret, "msg_1", ts, body)
	if err := VerifyStandardWebhook([]string{secret}, "msg_1", ts, sig, body, now); err != nil {
		t.Fatalf("valid delivery refused: %v", err)
	}
	// During a rotation the header carries one signature per secret.
	both := signWebhook(old, "msg_1", ts, body) + " " + sig
	if err := VerifyStandardWebhook([]string{secret}, "msg_1", ts, both, body, now); err != nil {
		t.Fatalf("rotated delivery refused: %v", err)
	}
	for name, c := range map[string]struct {
		secret, id, ts, sig string
		body                []byte
	}{
		"tampered body": {secret, "msg_1", ts, sig, []byte(strings.Replace(string(body), "v.mp4", "x.mp4", 1))},
		"other id":      {secret, "msg_2", ts, sig, body},
		"wrong secret":  {old, "msg_1", ts, sig, body},
		"no signature":  {secret, "msg_1", ts, "", body},
		"v2 scheme":     {secret, "msg_1", ts, strings.Replace(sig, "v1,", "v2,", 1), body},
	} {
		if err := VerifyStandardWebhook([]string{c.secret}, c.id, c.ts, c.sig, c.body, now); !errors.Is(err, ErrWebhookSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	stale := strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)
	if err := VerifyStandardWebhook([]string{secret}, "msg_1", stale, signWebhook(secret, "msg_1", stale, body), body, now); !errors.Is(err, ErrWebhookStale) {
		t.Fatalf("stale delivery: %v", err)
	}
}

// A delivery becomes the status endpoint's shape (outputs beside status).
func TestHiggsfieldDeliveryStatusBody(t *testing.T) {
	d, err := ParseHiggsfieldDelivery([]byte(`{"request_id":"9417a243","status":"completed","error":null,"payload":{"video":{"url":"https://cdn/v.mp4"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := d.StatusBody()
	var out videoOut
	_ = json.Unmarshal(b, &out)
	if out.Video.URL != "https://cdn/v.mp4" || !strings.Contains(string(b), `"status":"completed"`) {
		t.Fatalf("status body %s", b)
	}
	for _, bad := range []string{`{"status":"completed"}`, `{"request_id":"r1","status":"queued"}`, `{"request_id":"a/b","status":"failed"}`, `nope`} {
		if _, err := ParseHiggsfieldDelivery([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

type memBox struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (b *memBox) Put(_ context.Context, id string, s []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[id] = s
	return nil
}

func (b *memBox) Take(_ context.Context, id string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[id]
	return s, ok
}

// With a secret saved, a Higgsfield job asks for the webhook and finishes from the push,
// without waiting for the slow poll.
func TestHiggsfieldPushFinishesTheJob(t *testing.T) {
	f := newFakeProviders(t)
	box := &memBox{m: map[string][]byte{}}
	prevCheck, prevSlow := pushCheck, pushPollSlow
	pushCheck, pushPollSlow = 10*time.Millisecond, time.Hour
	SetHiggsfieldPush("https://marketflowai.example/api/v1/webhooks/higgsfield", box, func(context.Context) bool { return true })
	t.Cleanup(func() {
		pushCheck, pushPollSlow = prevCheck, prevSlow
		SetHiggsfieldPush("", nil, nil)
	})
	var gotQuery string
	orig := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/hf/requests/") {
			t.Error("the provider was polled although a push was expected")
		}
		if strings.HasPrefix(r.URL.Path, "/hf/minimax") {
			gotQuery = r.URL.RawQuery
			d, _ := ParseHiggsfieldDelivery([]byte(`{"request_id":"h1","status":"completed","error":null,"payload":{"video":{"url":"https://cdn/pushed.mp4"}}}`))
			s, _ := d.StatusBody()
			_ = box.Put(r.Context(), "h1", s)
		}
		orig.ServeHTTP(w, r)
	})
	var out videoOut
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := newQueueClient().run(ctx, higgsfieldJob("k", "minimax/hailuo-2.3/standard/image-to-video"), map[string]any{"prompt": "x"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Video.URL != "https://cdn/pushed.mp4" || !strings.Contains(gotQuery, "hf_webhook=https%3A%2F%2Fmarketflowai.example") {
		t.Fatalf("out %+v query %q", out, gotQuery)
	}
}

func TestParseLoudnessAndSilences(t *testing.T) {
	m, err := parseLoudness("[Parsed_loudnorm_0 @ 0x1] \n{\n\t\"input_i\" : \"-22.90\",\n\t\"input_tp\" : \"-3.90\",\n\t\"input_lra\" : \"5.10\",\n\t\"input_thresh\" : \"-33.20\",\n\t\"target_offset\" : \"0.40\"\n}\n")
	if err != nil || !strings.Contains(m.secondPass(), "measured_I=-22.90") || !strings.Contains(m.secondPass(), "linear=true") {
		t.Fatalf("loudness %+v %v", m, err)
	}
	if _, err := parseLoudness(`{"input_i" : "-inf","input_tp" : "-inf","input_lra" : "0.00","input_thresh" : "-70.00","target_offset" : "0.00"}`); err == nil {
		t.Fatal("a silent mix must fall back to one pass")
	}
	logs := "  Duration: 00:00:05.20, start: 0.000000, bitrate: 128 kb/s\n" +
		"[silencedetect @ 0x1] silence_start: 0\n[silencedetect @ 0x1] silence_end: 0.4 | silence_duration: 0.4\n" +
		"[silencedetect @ 0x1] silence_start: 2.1\n[silencedetect @ 0x1] silence_end: 2.9 | silence_duration: 0.8\n" +
		"[silencedetect @ 0x1] silence_start: 4.8\n"
	spans, total := parseSilences(logs)
	if total != 5.2 || len(spans) != 2 || spans[0] != (span{0.4, 2.1}) || spans[1] != (span{2.9, 4.8}) {
		t.Fatalf("spans %v total %v", spans, total)
	}
}

// Estimated words land on speech only: none starts in a pause, and the last ends where the
// speaking ends.
func TestRealignWordsSkipsPauses(t *testing.T) {
	words := estimateWords("Hello parents. The holiday is here.", 5.2)
	spans := []span{{0.4, 2.1}, {2.9, 4.8}}
	got := realignWords(words, spans)
	for _, w := range got {
		if (w.Start > 2.1 && w.Start < 2.9) || w.End > 4.8+1e-9 || w.Start < 0.4 {
			t.Fatalf("word %+v in a pause", w)
		}
	}
	if last := got[len(got)-1]; last.End < 4.79 {
		t.Fatalf("last word ends at %.2f", last.End)
	}
	if got[0].Start < 0.39 || got[0].Start > 0.41 {
		t.Fatalf("first word starts at %.2f, not where speech starts", got[0].Start)
	}
}

// A light logo gets a dark plate, over a video overlay (scene unknown) and over any scene, and
// never a light one (logo_plate.go).
func TestLogoPlateOnLightScenes(t *testing.T) {
	logo := image.NewRGBA(image.Rect(0, 0, 220, 120))
	draw.Draw(logo, image.Rect(10, 10, 210, 110), image.NewUniform(color.White), image.Point{}, draw.Src)
	overlay, err := ComposeOverlay("1080x1920", Overlay{Headline: "Digitika", Logo: logo})
	if err != nil {
		t.Fatal(err)
	}
	// Just outside the logo's top-left corner, inside the plate's padding.
	margin := 1080 * 6 / 100
	if px := overlay.RGBAAt(margin-4, margin+20); px.A == 0 || px.R > 40 {
		t.Fatalf("no plate behind a light logo on an overlay: %+v", px)
	}
	scene := image.NewRGBA(image.Rect(0, 0, 1080, 1920))
	draw.Draw(scene, scene.Bounds(), image.NewUniform(color.RGBA{20, 20, 30, 255}), image.Point{}, draw.Src)
	poster, err := ComposePoster(scene, "1080x1920", Overlay{Headline: "Digitika", Logo: logo})
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := poster.At(margin-4, margin+20).RGBA()
	if r>>8 > 40 || g>>8 > 40 || b>>8 > 45 {
		t.Fatalf("a light plate behind a light logo over a dark scene: %d %d %d", r>>8, g>>8, b>>8)
	}
}

// The storyboard plan: presenter scenes show the actor, product scenes the owner's photo,
// b-roll in a presenter ad a generated still; only generated stills (and a new actor) cost.
func TestStoryboardPlan(t *testing.T) {
	s := &Storyboards{}
	req := StoryboardRequest{Kind: AdPresenter, Photos: []string{"https://x/p1.jpg", "https://x/p2.jpg"}, Scenes: []ScriptScene{
		{ID: "s1", Shot: ShotPresenter}, {ID: "s2", Shot: ShotProduct, Asset: 2}, {ID: "s3", Shot: ShotBRoll}, {ID: "s4", Shot: ShotFootage},
	}}
	stills, generated := s.Plan(req)
	if stills[0].Source != StillActor || stills[1].Source != StillPhoto || stills[1].URL != "https://x/p2.jpg" ||
		stills[2].Source != StillGenerated || stills[3].Source != StillFootage || generated != 2 {
		t.Fatalf("plan %+v generated %d", stills, generated)
	}
	req.Actor = &Actor{URL: "https://x/actor.jpg"}
	if _, generated := s.Plan(req); generated != 1 {
		t.Fatalf("a kept actor is not drawn again: %d", generated)
	}
	// Since 2026-11-19 a template ad's b-roll is a generated still too, never a photo in turn.
	req.Kind = AdTemplate
	if _, generated := s.Plan(req); generated != 1 {
		t.Fatalf("a template storyboard draws only its b-roll still: %d", generated)
	}
}

// The storyboard draws the actor and the b-roll still on Soul v2 with the documented shape.
func TestStoryboardMakeOnSoul(t *testing.T) {
	f := newFakeProviders(t)
	store := &memStore{}
	s := NewStoryboards(keysFor("higgsfield"), store, NewPosters(keysFor("higgsfield"), store, nil), nil)
	res, err := s.Make(context.Background(), StoryboardRequest{TenantID: [16]byte{1}, VersionID: "v1", Format: "1080x1920", Kind: AdPresenter,
		Photos: []string{"https://x/p.jpg"}, Scenes: []ScriptScene{{ID: "s1", Shot: ShotPresenter}, {ID: "s2", Shot: ShotBRoll, Visual: "kids at laptops"}},
		ActorPrompt: "a warm Kenyan mother in her thirties"})
	if err != nil {
		t.Fatal(err)
	}
	in := f.inputs["higgsfield-ai/soul/v2/standard"]
	if in["aspect_ratio"] != "9:16" || in["prompt"] == "" || res.Actor == nil || res.Actor.URL == "" || res.Actor.Seed == 0 {
		t.Fatalf("soul input %v actor %+v", in, res.Actor)
	}
	if res.Stills[0].URL != res.Actor.URL || res.Stills[1].URL == "" || res.Stills[1].Source != StillGenerated {
		t.Fatalf("stills %+v", res.Stills)
	}
}

// The end card preview is the end card's still with our overlay on it, stored for the plan.
func TestEndCardPreview(t *testing.T) {
	f := newFakeProviders(t)
	store := &memStore{}
	s := NewStoryboards(keysFor("higgsfield"), store, NewPosters(keysFor("higgsfield"), store, nil), nil)
	req := StoryboardRequest{TenantID: [16]byte{1}, VersionID: "v9", Format: "1920x1080",
		Scenes:  []ScriptScene{{ID: "s1", Shot: ShotBRoll}, {ID: "s2", Shot: ShotEndCard}},
		EndCard: &Overlay{Headline: "Digitika Kids & Teens", CTA: "Message us on WhatsApp", Contacts: []Contact{{Kind: ContactWhatsApp, Value: "0712 345 678"}}}}
	res := &StoryboardResult{Stills: []Still{{SceneID: "s1", URL: f.srv.URL + "/scene.png"}, {SceneID: "s2", URL: f.srv.URL + "/scene.png"}}}
	p := s.endCardPreview(context.Background(), req, res)
	if p == nil || p.SceneID != "s2" || p.URL == "" {
		t.Fatalf("preview %+v", p)
	}
	if _, ok := store.puts[p.Key]; !ok {
		t.Fatal("the preview was not stored")
	}
	req.EndCard = nil
	if s.endCardPreview(context.Background(), req, res) != nil {
		t.Fatal("no end card words, no preview")
	}
	// The recompose-only path: the same still, new words and plate, a new file, nothing drawn,
	// and a broken logo URL is left out quietly.
	calls := len(f.inputs)
	again, err := s.ComposeEndCardPreview(context.Background(), EndCardPreviewRequest{TenantID: [16]byte{1}, VersionID: "v9",
		Format: "1920x1080", SceneID: "s2", StillURL: f.srv.URL + "/scene.png", LogoURL: f.srv.URL + "/missing-logo.png", Name: "r1",
		EndCard: Overlay{Headline: "Digitika", CTA: "Email us", Plate: PlateStyle{Style: PlateLight}}})
	if err != nil || again.Key == p.Key || again.SceneID != "s2" || len(f.inputs) != calls {
		t.Fatalf("recompose %+v %v (generation calls %d to %d)", again, err, calls, len(f.inputs))
	}
	if _, err := s.ComposeEndCardPreview(context.Background(), EndCardPreviewRequest{Format: "1920x1080", StillURL: f.srv.URL + "/gone"}); err == nil {
		t.Fatal("an unreadable still gave a preview")
	}
}
