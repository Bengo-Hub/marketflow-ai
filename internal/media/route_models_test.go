package media

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeFal is a fal model list held in memory.
type fakeFal struct {
	models map[string]string
	known  bool
}

func (f fakeFal) Lookup(id string) (string, bool, bool) {
	if !f.known {
		return "", false, false
	}
	c, ok := f.models[id]
	return c, ok, true
}

var fixtureFal = fakeFal{known: true, models: map[string]string{
	"fal-ai/nano-banana-pro":                                 "text-to-image",
	"fal-ai/nano-banana-pro/edit":                            "image-to-image",
	"fal-ai/nano-banana-pro-2":                               "text-to-image",
	"bytedance/seedream/v5.5/text-to-image":                  "text-to-image",
	"bytedance/seedream/v5.5/edit":                           "image-to-image",
	"fal-ai/minimax/hailuo-2.4-fast/standard/image-to-video": "image-to-video",
	"bytedance/seedance-2.5/us/text-to-video":                "text-to-video",
	"elevenlabs/tts/eleven-v5":                               "text-to-speech",
}}

// withRouteModels sets the route model source for one test.
func withRouteModels(t *testing.T, m RouteModels, fal FalLookup) {
	t.Helper()
	SetRouteModelSource(func(context.Context) RouteModels { return m }, fal)
	t.Cleanup(func() { SetRouteModelSource(nil, nil) })
}

// A configured id fal lists in the slot's category is used; an unknown id, one in another
// category or one that is not an id at all leaves the default in place.
func TestRouteModelsFromSettings(t *testing.T) {
	m := RouteModelsFromSettings(map[string]string{
		RouteKeyPosterPremium:    "fal-ai/nano-banana-pro-2",
		RouteKeyPosterStandard:   "bytedance/seedream/v9/edit",              // not listed
		RouteKeyVideoPremium:     "bytedance/seedance-2.5/us/text-to-video", // wrong category for a clip
		RouteKeyVideoStandard:    "fal-ai/minimax/hailuo-2.4-fast/standard/image-to-video",
		RouteKeyVoiceFalPremium:  "elevenlabs/tts/eleven-v5",
		RouteKeyVoiceFalStandard: "https://evil.example/tts", // not an endpoint id
		RouteKeyVoicePremium:     "eleven_v5",
		RouteKeyVoiceStandard:    "Eleven V5 Turbo", // not an ElevenLabs id
		RouteKeyTimingsViaScribe: "true",
	}, fixtureFal, nil)
	want := RouteModels{PosterPremium: "fal-ai/nano-banana-pro-2", VideoStandard: "fal-ai/minimax/hailuo-2.4-fast/standard/image-to-video",
		VoiceFalPremium: "elevenlabs/tts/eleven-v5", VoicePremium: "eleven_v5", TimingsViaScribe: true}
	if m != want {
		t.Fatalf("got %+v\nwant %+v", m, want)
	}

	// Before fal's list is read, a well-formed id is trusted (format only), so a slow list never
	// blocks a configured route.
	m = RouteModelsFromSettings(map[string]string{RouteKeyPosterStandard: "bytedance/seedream/v9/edit"}, fakeFal{}, nil)
	if m.PosterStandard != "bytedance/seedream/v9/edit" || m.TimingsViaScribe {
		t.Fatalf("unknown list: %+v", m)
	}
	if m := RouteModelsFromSettings(nil, nil, nil); m != (RouteModels{}) {
		t.Fatalf("no settings keeps every default: %+v", m)
	}
}

// A poster setting names one of a text-to-image and edit pair; the other is derived, and a derived
// sibling fal does not list falls back to the route's own endpoint.
func TestPosterEndpoints(t *testing.T) {
	withRouteModels(t, RouteModels{}, fixtureFal)
	cases := []struct{ configured, text, edit, wantText, wantEdit string }{
		{"", "a/text-to-image", "a/edit", "a/text-to-image", "a/edit"},
		{"bytedance/seedream/v5.5/edit", "bytedance/seedream/v5/lite/text-to-image", "bytedance/seedream/v5/lite/edit",
			"bytedance/seedream/v5.5/text-to-image", "bytedance/seedream/v5.5/edit"},
		{"bytedance/seedream/v5.5/text-to-image", "x/text-to-image", "x/edit", "bytedance/seedream/v5.5/text-to-image", "bytedance/seedream/v5.5/edit"},
		{"fal-ai/nano-banana-pro", "fal-ai/old", "fal-ai/old/edit", "fal-ai/nano-banana-pro", "fal-ai/nano-banana-pro/edit"},
		// nano-banana-pro-2 has no /edit sibling listed: photos keep the route's edit endpoint.
		{"fal-ai/nano-banana-pro-2", "fal-ai/nano-banana-pro", "fal-ai/nano-banana-pro/edit", "fal-ai/nano-banana-pro-2", "fal-ai/nano-banana-pro/edit"},
	}
	for _, c := range cases {
		text, edit := posterEndpoints(c.configured, c.text, c.edit)
		if text != c.wantText || edit != c.wantEdit {
			t.Errorf("%q: got %s, %s; want %s, %s", c.configured, text, edit, c.wantText, c.wantEdit)
		}
	}
}

// The clip and presenter routes call the configured endpoint, and their own without one.
func TestRouteEndpoint(t *testing.T) {
	if got := routeEndpoint(context.Background(), "fal/hailuo-2.3-fast", "default"); got != "default" {
		t.Fatalf("no source: %s", got)
	}
	withRouteModels(t, RouteModels{VideoStandard: "v/std", VideoPremium: "v/pro", Presenter: "p/boreal", PresenterFabric: "p/fabric"}, nil)
	for key, want := range map[string]string{"fal/hailuo-2.3-fast": "v/std", "fal/kling-v3-pro": "v/pro",
		"fal/creatify-boreal": "p/boreal", "fal/veed-fabric": "p/fabric", "fal/grok-imagine-video": "default"} {
		if got := routeEndpoint(context.Background(), key, "default"); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
}

// ElevenLabs direct reads every voice when it can, whatever fal model is configured; the
// configured direct model is the one sent.
func TestVoiceDirectAlwaysFirst(t *testing.T) {
	f := newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	withRouteModels(t, RouteModels{VoiceStandard: "eleven_v5_turbo", VoiceFalStandard: "elevenlabs/tts/eleven-v5-turbo"}, nil)
	v := NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Soft skin all week", Voice: "Brian"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "elevenlabs/eleven-v4-turbo" || !res.Exact || res.Timings != "provider" || el.got["model_id"] != "eleven_v5_turbo" {
		t.Fatalf("direct route expected with the configured model: %+v %v", res, el.got)
	}
	if len(f.inputs) != 0 {
		t.Fatalf("fal must not be called while ElevenLabs direct works: %v", f.inputs)
	}
}

// Without ElevenLabs direct (no key here; down, out of credit or a free plan work the same) fal
// reads the voice on the endpoint of the request's tier.
func TestVoiceFalFallbackMatchesTier(t *testing.T) {
	f := newFakeProviders(t)
	v := NewVoices(keysFor("fal"), &memAudio{}, nil)
	res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Soft skin", Tier: TierPremium})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != falVoiceRoute || f.inputs[defaultFalVoicePremium] == nil || math.Abs(res.CostUSD-0.08*9/1000) > 1e-9 {
		t.Fatalf("premium falls back to fal eleven-v4: %+v %v", res, f.inputs)
	}
	res, err = v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v2", Text: "Soft skin", Tier: TierStandard})
	if err != nil || res.Route != falVoiceTurboRoute || f.inputs[defaultFalVoiceStandard] == nil {
		t.Fatalf("standard falls back to fal eleven-v4-turbo: %+v %v", res, err)
	}

	// A configured fal voice model is the endpoint called.
	withRouteModels(t, RouteModels{VoiceFalStandard: "elevenlabs/tts/eleven-v5-turbo"}, nil)
	if _, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v3", Text: "Soft skin"}); err != nil ||
		f.inputs["elevenlabs/tts/eleven-v5-turbo"] == nil {
		t.Fatalf("configured fal voice endpoint not used: %v %v", err, f.inputs)
	}
}

// fal's timings are used when it returns them; otherwise Scribe times the audio when the setting
// is on, and with it off (or Scribe failing) the timings are estimated.
func TestVoiceFalTimings(t *testing.T) {
	f := newFakeProviders(t)
	v := NewVoices(keysFor("fal"), &memAudio{}, nil)
	req := VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Soft skin"}

	f.extra = map[string]any{"timestamps": []map[string]any{{"text": "Soft", "start": 0.0, "end": 0.4}, {"text": " ", "start": 0.4, "end": 0.5},
		{"text": "skin", "start": 0.5, "end": 0.9}}}
	res, err := v.Make(context.Background(), req)
	if err != nil || !res.Exact || res.Timings != "provider" || len(res.Words) != 2 || res.Words[1].Start != 0.5 {
		t.Fatalf("fal timestamps: %v %+v", err, res)
	}

	f.extra = map[string]any{"words": []map[string]any{{"text": "Soft", "start": 0.0, "end": 0.4, "type": "word"},
		{"text": " ", "start": 0.4, "end": 0.5, "type": "spacing"}, {"text": "skin", "start": 0.5, "end": 0.9, "type": "word"}}}
	res, err = v.Make(context.Background(), req)
	if err != nil || res.Exact || res.Timings != "estimated" || f.inputs[scribeEndpoint] != nil {
		t.Fatalf("scribe is off by default: %v %+v", err, res)
	}
	withRouteModels(t, RouteModels{TimingsViaScribe: true}, nil)
	res, err = v.Make(context.Background(), req)
	if err != nil || !res.Exact || res.Timings != "scribe" || len(res.Words) != 2 || f.inputs[scribeEndpoint]["audio_url"] == nil || res.CostUSD <= 0.04*9/1000 {
		t.Fatalf("scribe timings: %v %+v", err, res)
	}

	// Scribe failing leaves estimated timings, never a failed voice-over.
	f.failFalPath = "speech-to-text"
	res, err = v.Make(context.Background(), req)
	if err != nil || res.Exact || res.Timings != "estimated" {
		t.Fatalf("scribe failure: %v %+v", err, res)
	}
}

func TestFalTimingsShapes(t *testing.T) {
	chars := json.RawMessage(`{"characters":["H","i"," ","y","o"],"character_start_times_seconds":[0,0.1,0.2,0.3,0.4],"character_end_times_seconds":[0.1,0.2,0.3,0.4,0.5]}`)
	if w := falTimings(chars); len(w) != 2 || w[0].Text != "Hi" || w[1].Start != 0.3 {
		t.Fatalf("alignment object: %+v", w)
	}
	perChar := json.RawMessage(`[{"character":"H","start":0,"end":0.1},{"character":"i","start":0.1,"end":0.2},{"character":" ","start":0.2,"end":0.3},{"character":"y","start":0.3,"end":0.4}]`)
	if w := falTimings(perChar); len(w) != 2 || w[1].Text != "y" {
		t.Fatalf("per character: %+v", w)
	}
	words := json.RawMessage(`[{"word":"[warm]","start":0,"end":0},{"word":"Karibu","start":0,"end":0.5}]`)
	if w := falTimings(words); len(w) != 1 || w[0].Text != "Karibu" {
		t.Fatalf("words: %+v", w)
	}
	for _, bad := range []string{``, `null`, `[]`, `[{"text":"a"}]`, `[{"text":"b","start":1,"end":0.5}]`, `"x"`} {
		if w := falTimings(json.RawMessage(bad)); w != nil {
			t.Errorf("%s gave %+v", bad, w)
		}
	}
}

// The catalogue never makes a lookup wait: the first lookup answers "not known" at once and
// loads in the background; a failed read backs off instead of retrying on every lookup.
func TestFalCatalog(t *testing.T) {
	var hits atomic.Int32
	fail := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.Header.Get("Authorization") != "Key fal-id:secret" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"endpoint_id": "a/one", "metadata": map[string]any{"category": "text-to-image"}}},
				"has_more": true, "next_cursor": "p2"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"endpoint_id": "b/two", "metadata": map[string]any{"category": "image-to-video"}}}})
	}))
	t.Cleanup(srv.Close)
	prev := falModelsBase
	falModelsBase = srv.URL
	t.Cleanup(func() { falModelsBase = prev })

	c := NewFalCatalog(keysFor("fal"), nil)
	start := time.Now()
	if _, _, known := c.Lookup("a/one"); known {
		t.Fatal("the first lookup cannot know the list yet")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("a lookup waited for the list")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, known := c.Lookup("a/one"); known || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cat, ok, known := c.Lookup("b/two"); !known || !ok || cat != "image-to-video" {
		t.Fatalf("second page: %q %v %v", cat, ok, known)
	}
	if _, ok, _ := c.Lookup("c/three"); ok {
		t.Fatal("an unlisted id is listed")
	}

	fail.Store(true)
	bad := NewFalCatalog(keysFor("fal"), nil)
	bad.Lookup("a/one")
	deadline = time.Now().Add(3 * time.Second)
	for {
		bad.mu.Lock()
		backingOff := !bad.loading && !bad.nextTry.IsZero()
		bad.mu.Unlock()
		if backingOff || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := hits.Load()
	for i := 0; i < 20; i++ {
		bad.Lookup("a/one")
	}
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != before {
		t.Fatal("a failed read must back off, not retry on every lookup")
	}
	bad.mu.Lock()
	wait := time.Until(bad.nextTry)
	bad.mu.Unlock()
	if wait < falCatalogRetryMin-time.Second {
		t.Fatalf("backoff too short: %v", wait)
	}
}
