package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 255), 120, uint8(y % 255), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeProviders serves fal.ai's queue and Higgsfield's API the way their docs
// describe them, records every submitted input, and can be told to fail.
type fakeProviders struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	inputs  map[string]map[string]any // path -> decoded submit body
	auth    map[string]string
	failFal bool
	scene   []byte
	clip    []byte // served at /clip.mp4 (made with ffmpeg by the clip test)
}

func newFakeProviders(t *testing.T) *fakeProviders {
	f := &fakeProviders{t: t, inputs: map[string]map[string]any{}, auth: map[string]string{}, scene: pngBytes(t, 2160, 2700)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	prevFal, prevHF, prevPoll := falQueueBase, higgsfieldBase, pollStart
	falQueueBase, higgsfieldBase, pollStart = f.srv.URL+"/fal", f.srv.URL+"/hf", 10*time.Millisecond
	t.Cleanup(func() { falQueueBase, higgsfieldBase, pollStart = prevFal, prevHF, prevPoll })
	return f
}

func (f *fakeProviders) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/scene.png":
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(f.scene)
	case strings.HasPrefix(p, "/fal/requests/"): // status, then result
		if strings.HasSuffix(p, "/status") {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "COMPLETED", "request_id": "r1", "response_url": f.srv.URL + "/fal/requests/r1"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{{"url": f.srv.URL + "/scene.png", "width": 2160, "height": 2700}}, "seed": 1})
	case strings.HasPrefix(p, "/fal/"):
		f.record(r, strings.TrimPrefix(p, "/fal/"))
		if f.failFal {
			http.Error(w, `{"detail":"upstream error"}`, http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "IN_QUEUE", "request_id": "r1",
			"status_url": f.srv.URL + "/fal/requests/r1/status", "response_url": f.srv.URL + "/fal/requests/r1"})
	case p == "/clip.mp4":
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(f.clip)
	case strings.HasPrefix(p, "/hf/requests/"):
		// A completed Higgsfield job carries images or a video, by model.
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "request_id": "h1",
			"images": []map[string]any{{"url": f.srv.URL + "/scene.png"}},
			"video":  map[string]any{"url": f.srv.URL + "/clip.mp4"}})
	case strings.HasPrefix(p, "/hf/"):
		f.record(r, strings.TrimPrefix(p, "/hf/"))
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "queued", "request_id": "h1",
			"status_url": f.srv.URL + "/hf/requests/h1/status", "cancel_url": f.srv.URL + "/hf/requests/h1/cancel"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeProviders) record(r *http.Request, path string) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		f.t.Errorf("submit body for %s is not JSON: %s", path, b)
	}
	f.mu.Lock()
	f.inputs[path], f.auth[path] = m, r.Header.Get("Authorization")
	f.mu.Unlock()
}

type memStore struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func (s *memStore) PutPoster(_ context.Context, tenantID uuid.UUID, versionID string, data []byte) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.puts == nil {
		s.puts = map[string][]byte{}
	}
	key := "marketflow/" + tenantID.String() + "/images/creative/" + versionID + ".jpg"
	s.puts[key] = data
	return key, "https://media.example/" + key, nil
}

func keysFor(providers ...string) KeySource {
	return func(_ context.Context, p string) string {
		for _, x := range providers {
			if x == p {
				return p + "-id:secret"
			}
		}
		return ""
	}
}

func posterReq() PosterRequest {
	return PosterRequest{TenantID: uuid.New(), VersionID: "v1", Prompt: "A jar of shea butter lotion on a wooden table, morning light",
		PhotoURLs: []string{"https://cdn.example/photo.jpg"}, Format: "1080x1350",
		Overlay: Overlay{Headline: "Soft skin all day", Subline: "Shea butter from Kisumu", Price: "KES 850", CTA: "Order on WhatsApp"}}
}

func TestPosterDefaultRouteSendsTheDocumentedShape(t *testing.T) {
	f := newFakeProviders(t)
	store := &memStore{}
	res, err := NewPosters(keysFor("fal", "higgsfield"), store, nil).Make(context.Background(), posterReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "fal/seedream-v5-lite" || res.Attempts != 1 || res.CostUSD != 0.035 {
		t.Fatalf("result = %+v", res)
	}
	// The poster and its clean scene (for free text edits) are both stored.
	if res.Width != 1080 || res.Height != 1350 || res.Bytes == 0 || len(store.puts) != 2 {
		t.Fatalf("stored poster = %+v", res)
	}
	if !strings.HasSuffix(res.SceneKey, "-scene.jpg") || res.SceneURL == "" {
		t.Fatalf("scene copy = %q %q", res.SceneKey, res.SceneURL)
	}
	in := f.inputs["fal-ai/bytedance/seedream/v5/lite/edit"]
	if in == nil {
		t.Fatalf("with product photos the edit endpoint must be used: %v", f.inputs)
	}
	if !strings.Contains(in["prompt"].(string), "no text") || len(in["image_urls"].([]any)) != 1 {
		t.Fatalf("seedream input = %v", in)
	}
	size := in["image_size"].(map[string]any)
	if w, h := size["width"].(float64), size["height"].(float64); w*h < 2560*1440 || w*h > 4096*4096 {
		t.Fatalf("seedream needs 2560x1440 to 4096x4096 pixels, sent %vx%v", w, h)
	}
	if f.auth["fal-ai/bytedance/seedream/v5/lite/edit"] != "Key fal-id:secret" {
		t.Fatalf("fal auth header = %q", f.auth["fal-ai/bytedance/seedream/v5/lite/edit"])
	}
}

// A text-only revision reads the stored scene and calls no provider.
func TestRecomposeUsesTheSceneOnly(t *testing.T) {
	f := newFakeProviders(t)
	store := &memStore{}
	p := NewPosters(keysFor("fal", "higgsfield"), store, nil)
	req := posterReq()
	req.Overlay.Headline = "New price this week"
	res, err := p.Recompose(context.Background(), req, f.srv.URL+"/scene.png")
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != RouteRecompose || res.CostUSD != 0 || res.Width != 1080 || len(store.puts) != 1 {
		t.Fatalf("recompose = %+v (%d stored)", res, len(store.puts))
	}
	if len(f.inputs) != 0 {
		t.Fatalf("a provider was called: %v", f.inputs)
	}
	if _, err := p.Recompose(context.Background(), req, f.srv.URL+"/gone.png"); !errors.Is(err, ErrSceneGone) {
		t.Fatalf("missing scene: %v", err)
	}
}

func TestPosterFallsBackAcrossProviders(t *testing.T) {
	f := newFakeProviders(t)
	f.failFal = true
	req := posterReq()
	req.Palette = [][3]int{{109, 40, 217}, {300, -5, 10}}
	res, err := NewPosters(keysFor("fal", "higgsfield"), &memStore{}, nil).Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "higgsfield/recraft-v4.1" {
		t.Fatalf("want the Higgsfield fallback, got %s", res.Route)
	}
	in := f.inputs["recraft/v4.1/text-to-image"]
	if in["aspect_ratio"] != "4:5" || in["output_format"] != "jpg" || in["image_urls"] != nil {
		t.Fatalf("recraft input = %v (it takes no photos)", in)
	}
	colors := in["colors"].([]any)
	second := colors[1].(map[string]any)["rgb"].([]any)
	if second[0].(float64) != 255 || second[1].(float64) != 0 {
		t.Fatalf("colours must be clamped to 0-255: %v", colors)
	}
	if f.auth["recraft/v4.1/text-to-image"] != "Key higgsfield-id:secret" {
		t.Fatal("higgsfield auth header wrong")
	}
}

func TestPosterWithNoKeysFailsCleanly(t *testing.T) {
	newFakeProviders(t)
	_, err := NewPosters(keysFor(), &memStore{}, nil).Make(context.Background(), posterReq())
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
}

func TestRouterOrderByTier(t *testing.T) {
	r := NewRouter(PosterRoutes)
	keys := func(rs []Route) []string {
		var out []string
		for _, x := range rs {
			out = append(out, x.Key)
		}
		return out
	}
	std := keys(r.Order(CapPosterImage, TierStandard, nil))
	if strings.Join(std, ",") != "fal/seedream-v5-lite,higgsfield/recraft-v4.1,fal/nano-banana-2" {
		t.Fatalf("standard order = %v (premium-only route must be out)", std)
	}
	prem := keys(r.Order(CapPosterImage, TierPremium, nil))
	if prem[0] != "fal/nano-banana-pro" || strings.Contains(strings.Join(prem, ","), "recraft") {
		t.Fatalf("premium order = %v (quality first, recraft below the floor)", prem)
	}
	// A failing route drops behind the healthy ones once its breaker opens.
	for i := 0; i < 3; i++ {
		r.Failed("fal/seedream-v5-lite", errors.New("503"), false)
	}
	if got := keys(r.Order(CapPosterImage, TierStandard, nil)); got[0] == "fal/seedream-v5-lite" || got[len(got)-1] != "fal/seedream-v5-lite" {
		t.Fatalf("an open route must go last: %v", got)
	}
}

func TestComposePosterSizesAndWraps(t *testing.T) {
	scene := image.NewRGBA(image.Rect(0, 0, 3000, 2000)) // landscape scene into portrait formats
	long := strings.Repeat("Soft skin all day with real shea butter ", 6)
	for format, size := range Formats {
		img, err := ComposePoster(scene, format, Overlay{Headline: long, Price: "KES 850", CTA: "Order on WhatsApp now and get free delivery in Kisumu town"})
		if err != nil {
			t.Fatal(err)
		}
		if b := img.Bounds(); b.Dx() != size.X || b.Dy() != size.Y {
			t.Fatalf("%s composed at %v", format, b)
		}
		if _, err := EncodeJPEG(img); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ComposePoster(scene, "800x600", Overlay{}); err == nil {
		t.Fatal("unknown format must be refused")
	}
}

func TestObjectStorePutsUnderTheTenantAndVersion(t *testing.T) {
	st, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost:4300/media"})
	if err != nil {
		t.Fatal(err)
	}
	tid := uuid.New()
	key, url, err := ObjectStore{S: st}.PutPoster(context.Background(), tid, "3f6c", []byte("jpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "marketflow/"+tid.String()+"/creative-drafts/posters/3f6c.jpg") || !strings.Contains(url, key) {
		t.Fatalf("key=%s url=%s", key, url)
	}
	if ok, _ := st.Exists(context.Background(), key); !ok {
		t.Fatal("poster not stored")
	}
	if _, _, err := (ObjectStore{}).PutPoster(context.Background(), tid, "v", nil); err == nil {
		t.Fatal("a missing store must fail clearly")
	}
}
