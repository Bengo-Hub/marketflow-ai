package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// useHiggsfield points the Higgsfield base at srv for one test.
func useHiggsfield(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev, prevPoll := higgsfieldBase, pollStart
	higgsfieldBase, pollStart = srv.URL, 5*time.Millisecond
	t.Cleanup(func() { higgsfieldBase, pollStart = prev, prevPoll })
}

// nsfw and canceled end the job at once: no further polling, a clear error, and a refusal
// does not count against the route.
func TestHiggsfieldTerminalStatuses(t *testing.T) {
	for _, status := range []string{"nsfw", "canceled", "failed"} {
		var polls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/status") {
				atomic.AddInt32(&polls, 1)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "request_id": "h1"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "queued", "request_id": "h1", "status_url": "http://" + r.Host + "/requests/h1/status"})
		}))
		useHiggsfield(t, srv)
		var out videoOut
		err := newQueueClient().run(context.Background(), higgsfieldJob("k", "kling-video/v3.0/std/image-to-video"), map[string]any{"prompt": "x"}, &out)
		srv.Close()
		if err == nil {
			t.Fatalf("%s: want an error", status)
		}
		if n := atomic.LoadInt32(&polls); n != 1 {
			t.Fatalf("%s: polled %d times, a terminal status is final", status, n)
		}
		switch status {
		case "nsfw":
			if !errors.Is(err, ErrContentRefused) || ai.Retryable(err) {
				t.Fatalf("nsfw must be a refusal the breaker ignores: %v", err)
			}
			if !strings.Contains(jobFailure(err), "safety check") {
				t.Fatalf("owner message = %q", jobFailure(err))
			}
		case "canceled":
			if !errors.Is(err, ErrJobCanceled) || !strings.Contains(jobFailure(err), "canceled") {
				t.Fatalf("canceled: %v / %q", err, jobFailure(err))
			}
		case "failed":
			if errors.Is(err, ErrContentRefused) || errors.Is(err, ErrJobCanceled) || !ai.Retryable(err) {
				t.Fatalf("failed is a plain route failure: %v", err)
			}
		}
	}
	// The sentinel survives the route loop's wrapping.
	wrapped := errors.Join(ErrNoRoute, &errJobFailed{provider: "higgsfield", status: "nsfw"})
	if !errors.Is(wrapped, ErrContentRefused) {
		t.Fatal("ErrContentRefused must survive wrapping")
	}
}

func TestClipInputsForNewRoutes(t *testing.T) {
	req := ClipRequest{SceneURL: "https://cdn.example/scene.png", Seconds: 5, Prompt: "Slow pan"}
	byKey := map[string]Route{}
	for _, rt := range ClipRoutes {
		byKey[rt.Key] = rt
	}
	ep, in, err := clipInput(byKey["higgsfield/hailuo-2.3-std"], req)
	if err != nil || ep != "minimax/hailuo-2.3/standard/image-to-video" || in["duration"] != 6 || in["image_url"] != req.SceneURL || in["prompt_optimizer"] != false {
		t.Fatalf("hailuo = %s %v %v", ep, in, err)
	}
	req.Seconds = 8
	if _, in, _ = clipInput(byKey["higgsfield/hailuo-2.3-std"], req); in["duration"] != 10 {
		t.Fatalf("hailuo 8 s must ask for 10: %v", in)
	}
	req.Seconds = 5
	ep, in, _ = clipInput(byKey["higgsfield/kling-v3-turbo"], req)
	if ep != "kling-video/v3.0-turbo/image-to-video" || in["duration"] != 5 || in["resolution"] != "720p" {
		t.Fatalf("turbo = %s %v", ep, in)
	}
	if _, in, _ = clipInput(byKey["higgsfield/seedance-2.0"], req); in["resolution"] != "720p" {
		t.Fatalf("seedance 2.0 runs at 720p: %v", in)
	}
	// Costs on the tier map: Hailuo per 6 s clip, Kling std flat to 7 s, Seedance 2.0 per second.
	if c := byKey["higgsfield/hailuo-2.3-std"].Cost(5); c != 0.28 {
		t.Fatalf("hailuo 5 s = %v", c)
	}
	if c := byKey["higgsfield/kling-v3-std"].Cost(7); c != 0.476 {
		t.Fatalf("kling 7 s = %v", c)
	}
	if c := byKey["higgsfield/seedance-2.0"].Cost(5); c < 1.51 || c > 1.52 {
		t.Fatalf("seedance 2.0 5 s = %v", c)
	}
}

func TestClipAndPresenterTierOrder(t *testing.T) {
	keys := func(rs []Route) string {
		var out []string
		for _, x := range rs {
			out = append(out, x.Key)
		}
		return strings.Join(out, ",")
	}
	std := keys(NewRouter(ClipRoutes).OrderFor(CapImageToVideo, TierStandard, 5, nil))
	if std != "fal/hailuo-2.3-fast,higgsfield/hailuo-2.3-std,higgsfield/kling-v3-std,higgsfield/kling-v3-turbo,fal/grok-imagine-video,fal/kling-v3-pro" {
		t.Fatalf("standard clips = %s", std)
	}
	if long := keys(NewRouter(ClipRoutes).OrderFor(CapImageToVideo, TierStandard, 12, nil)); strings.Contains(long, "hailuo") {
		t.Fatalf("hailuo makes at most 10 s: %s", long)
	}
	if prem := keys(NewRouter(ClipRoutes).OrderFor(CapImageToVideo, TierPremium, 5, nil)); !strings.HasPrefix(prem, "higgsfield/seedance-2.0,") {
		t.Fatalf("premium clips = %s", prem)
	}
	pstd := keys(NewRouter(PresenterRoutes).OrderFor(CapPresenter, TierStandard, 10, nil))
	if !strings.HasPrefix(pstd, "fal/creatify-boreal,fal/veed-fabric,higgsfield/seedance-2.5-480p") || strings.Contains(pstd, "720p") {
		t.Fatalf("standard presenter = %s", pstd)
	}
	pprem := keys(NewRouter(PresenterRoutes).OrderFor(CapPresenter, TierPremium, 10, nil))
	if !strings.HasPrefix(pprem, "higgsfield/seedance-2.5-720p,") || strings.Contains(pprem, "veed") {
		t.Fatalf("premium presenter = %s", pprem)
	}
}

func TestPresenterSeedanceShape(t *testing.T) {
	f := newFakeProviders(t)
	p := NewPresenters(keysFor("higgsfield"), nil) // no fal key: Seedance 480p is the route
	res, err := p.Make(context.Background(), PresenterRequest{TenantID: uuid.New(), VersionID: "v",
		ImageURL: "https://cdn.example/actor.png", AudioURL: "https://cdn.example/voice.mp3", Seconds: 3.2})
	if err != nil {
		t.Fatal(err)
	}
	in := f.inputs["bytedance/seedance-2.5/reference-to-video"]
	if in == nil {
		t.Fatalf("seedance not called: %v", f.inputs)
	}
	imgs, _ := in["image_urls"].([]any)
	auds, _ := in["audio_urls"].([]any)
	if len(imgs) != 1 || imgs[0] != "https://cdn.example/actor.png" || len(auds) != 1 || auds[0] != "https://cdn.example/voice.mp3" {
		t.Fatalf("references = %v", in)
	}
	if in["duration"].(float64) != 4 || in["resolution"] != "480p" || in["generate_audio"] != false || !strings.Contains(in["prompt"].(string), "No text") {
		t.Fatalf("seedance input = %v", in)
	}
	if res.Route != "higgsfield/seedance-2.5-480p" || res.CostUSD < 0.82 || res.CostUSD > 0.83 { // billed as 4 s
		t.Fatalf("result = %+v", res)
	}

	// With both keys the lowest tier runs Creatify Boreal first and VEED Fabric when it fails.
	f.failFalPath = "creatify/boreal"
	res, err = NewPresenters(keysFor("fal", "higgsfield"), nil).Make(context.Background(), PresenterRequest{TenantID: uuid.New(), VersionID: "v2",
		ImageURL: "https://cdn.example/actor.png", AudioURL: "https://cdn.example/voice.mp3", Seconds: 6})
	if err != nil || res.Route != "fal/veed-fabric" || res.Attempts != 2 {
		t.Fatalf("fallback = %+v %v", res, err)
	}
}

func TestBalanceGateSkipsALowProvider(t *testing.T) {
	b := NewBalances()
	if !b.Allows("fal", 100) {
		t.Fatal("no reading must allow")
	}
	b.Set("higgsfield", BalanceReading{Remaining: 1.00, Unit: "usd", LowMark: 5})
	if b.Allows("higgsfield", 1.51) || !b.Allows("higgsfield", 0.28) {
		t.Fatal("below the mark, only outputs within the remainder may go")
	}
	b.Set("fal", BalanceReading{Remaining: 20, Unit: "usd", LowMark: 5})
	if !b.Allows("fal", 30) {
		t.Fatal("above the mark the gate stays open")
	}
	b.Set("fal", BalanceReading{Remaining: 0, Unit: "usd", LowMark: 5, At: time.Now().Add(-2 * time.Hour)})
	if !b.Allows("fal", 1) {
		t.Fatal("a stale reading must not block")
	}

	// The clip router skips a provider's routes it cannot pay for when its balance is low:
	// fal low skips the fal Hailuo 2.3 Fast default (0.19) for Higgsfield Hailuo (0.28).
	prev := providerBalances
	providerBalances = NewBalances()
	t.Cleanup(func() { providerBalances = prev })
	providerBalances.Set("fal", BalanceReading{Remaining: 0.10, Unit: "usd", LowMark: 5})
	providerBalances.Set("higgsfield", BalanceReading{Remaining: 0.30, Unit: "usd", LowMark: 5})
	c := NewClips(keysFor("higgsfield", "fal"), nil, nil, nil)
	c.estimate = nil // route choice only; no estimate call
	_, route, _ := c.Quote(context.Background(), TierStandard, 5)
	if route != "higgsfield/hailuo-2.3-std" {
		t.Fatalf("with fal low, hailuo (0.28) on Higgsfield fits: %s", route)
	}
	providerBalances.Set("higgsfield", BalanceReading{Remaining: 0.10, Unit: "usd", LowMark: 5})
	if _, route, _ = c.Quote(context.Background(), TierStandard, 5); route != "" {
		t.Fatalf("with both providers at 0.10 no clip route can pay: %s", route)
	}
	providerBalances.Set("fal", BalanceReading{Remaining: 20, Unit: "usd", LowMark: 5})
	if _, route, _ = c.Quote(context.Background(), TierStandard, 5); route != "fal/hailuo-2.3-fast" {
		t.Fatalf("fal topped up: the fal Hailuo 2.3 Fast default again: %s", route)
	}
}

func TestHiggsfieldUploadNeverSendsTheKeyToStorage(t *testing.T) {
	var putAuth, putHeader, putBody string
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/files/generate-upload-url", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.Header.Get("Authorization") != "Key hf" || in["content_type"] != "image/png" {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"upload_url": srv.URL + "/signed/abc?sig=secret",
			"upload_headers": map[string]string{"x-amz-acl": "public-read"}, "public_url": "https://cdn.higgsfield.example/abc.png"})
	})
	mux.HandleFunc("/signed/abc", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		putAuth, putHeader, putBody = r.Header.Get("Authorization"), r.Header.Get("x-amz-acl"), string(b)
		if r.Method != http.MethodPut {
			http.Error(w, "method", 405)
		}
	})
	useHiggsfield(t, srv)
	u, err := NewHiggsfieldUploader().Upload(context.Background(), "hf", "image/png", []byte("png-bytes"))
	if err != nil || u != "https://cdn.higgsfield.example/abc.png" {
		t.Fatalf("upload = %q %v", u, err)
	}
	if putAuth != "" || putHeader != "public-read" || putBody != "png-bytes" {
		t.Fatalf("put auth=%q header=%q body=%q", putAuth, putHeader, putBody)
	}

	// A failed PUT returns no public URL and the error never carries the signed URL.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/files/generate-upload-url" {
			_ = json.NewEncoder(w).Encode(map[string]any{"upload_url": "http://127.0.0.1:1/signed?sig=secret", "public_url": "https://cdn/x"})
			return
		}
	}))
	defer srv2.Close()
	useHiggsfield(t, srv2)
	u, err = NewHiggsfieldUploader().Upload(context.Background(), "hf", "image/png", []byte("x"))
	if err == nil || u != "" || strings.Contains(err.Error(), "sig=secret") {
		t.Fatalf("failed put = %q %v", u, err)
	}
}

func TestEstimatorCachesAndFallsBack(t *testing.T) {
	var calls int32
	var mode atomic.Value
	mode.Store("price")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch mode.Load() {
		case "price":
			_, _ = w.Write([]byte(`{"type":"estimate","credits":"8.960","usd":"0.560","discount":null}`))
		case "description":
			_, _ = w.Write([]byte(`{"type":"description","pricing_description":"token metered"}`))
		case "slow":
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"usd":"9"}`))
		case "down":
			http.Error(w, "down", 503)
		}
	}))
	defer srv.Close()
	useHiggsfield(t, srv)
	e := NewEstimator()
	in := map[string]any{"image_url": quoteImage, "duration": 5}
	for i := 0; i < 3; i++ {
		if usd, ok := e.USD(context.Background(), "hf", "kling-video/v3.0-turbo/image-to-video", in, 0.7); !ok || usd != 0.56 {
			t.Fatalf("estimate = %v %v", usd, ok)
		}
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("estimate must be cached: %d calls", calls)
	}
	mode.Store("description")
	if usd, ok := e.USD(context.Background(), "hf", "bytedance/seedance-2.5/reference-to-video", in, 1.03); ok || usd != 1.03 {
		t.Fatalf("a description answer uses the fallback: %v %v", usd, ok)
	}
	mode.Store("slow")
	e.http.Timeout = 50 * time.Millisecond
	start := time.Now()
	if usd, ok := e.USD(context.Background(), "hf", "m/slow", in, 2); ok || usd != 2 || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("a slow estimate must fall back fast: %v %v %v", usd, ok, time.Since(start))
	}
	// Right after a timeout the estimator does not call again for a while.
	before := atomic.LoadInt32(&calls)
	if usd, _ := e.USD(context.Background(), "hf", "m/other", in, 3); usd != 3 || atomic.LoadInt32(&calls) != before {
		t.Fatal("a recent outage must short-circuit to the fallback")
	}
}
