package media

import (
	"context"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestAdsValidate(t *testing.T) {
	a := NewAds(nil, nil)
	ok := AdRequest{Format: "1080x1920", Photos: []string{"https://x/p.jpg"},
		Scenes: []ScriptScene{{Shot: ShotProduct, Voiceover: "Soft skin"}, {Shot: ShotEndCard, Voiceover: "Order"}}}
	if err := a.Validate(ok); err != nil {
		t.Fatalf("valid script refused: %v", err)
	}
	bad := map[string]func(r *AdRequest){
		"presenter in a template ad": func(r *AdRequest) { r.Scenes[0].Shot = ShotPresenter },
		"presenter not wired":        func(r *AdRequest) { r.Kind, r.ActorURL = AdPresenter, "https://x/a.jpg" },
		"unknown kind":               func(r *AdRequest) { r.Kind = "cinema" },
		"no footage":                 func(r *AdRequest) { r.Scenes[0].Shot = ShotFootage },
		"photo index":                func(r *AdRequest) { r.Scenes[0].Asset = 2 },
		"unknown shot":               func(r *AdRequest) { r.Scenes[0].Shot = "drone" },
		"one scene":                  func(r *AdRequest) { r.Scenes = r.Scenes[:1] },
		"no voice":                   func(r *AdRequest) { r.Scenes[0].Voiceover, r.Scenes[1].Voiceover = "", "" },
		"too much voice":             func(r *AdRequest) { r.Scenes[0].Voiceover = strings.Repeat("a ", MaxAdVoiceChars) },
		"local photo":                func(r *AdRequest) { r.Photos = []string{"file:///etc/passwd"} },
		"format":                     func(r *AdRequest) { r.Format = "800x600" },
	}
	for name, mutate := range bad {
		r := ok
		r.Scenes = append([]ScriptScene(nil), ok.Scenes...)
		mutate(&r)
		var ue *UserError
		if err := a.Validate(r); !errors.As(err, &ue) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A wired presenter ad needs its actor, and each presenter scene has words within the limit.
	p := NewAds(nil, nil).WithPresenters(NewPresenters(keysFor(), nil)).WithClips(&Clips{}, newSceneFiles(t))
	p.asm = &Assembler{ffmpeg: "ffmpeg", store: &fileStore{}}
	p.voices = &Voices{}
	pr := ok
	pr.Kind = AdPresenter
	pr.Scenes = []ScriptScene{{ID: "s1", Shot: ShotPresenter, Voiceover: "Hello parents"}, {ID: "s2", Shot: ShotEndCard, Voiceover: "Order"}}
	if err := p.Validate(pr); err == nil || !strings.Contains(err.Error(), "storyboard") {
		t.Fatalf("presenter ad without its actor: %v", err)
	}
	pr.ActorURL = "https://x/actor.jpg"
	if err := p.Validate(pr); err != nil {
		t.Fatalf("presenter ad refused: %v", err)
	}
	pr.Scenes[0].Voiceover = strings.Repeat("word ", MaxPresenterVoiceChars/4)
	if err := p.Validate(pr); err == nil {
		t.Fatal("a presenter scene over the limit was accepted")
	}
}

// The scene hash changes with what shapes a scene and nothing else.
func TestSceneHashFollowsTheScene(t *testing.T) {
	req := AdRequest{Kind: AdPresenter, ActorURL: "https://x/a.jpg", Voice: "Yna", Language: "en"}
	s := ScriptScene{ID: "s1", Shot: ShotPresenter, Voiceover: "Hello parents"}
	sp := ScenePlan{Work: WorkPresenter, source: req.ActorURL}
	h := sceneHash(req, s, sp)
	if h != sceneHash(req, s, sp) {
		t.Fatal("the hash is not stable")
	}
	req2 := req
	req2.EndCard = Overlay{Headline: "changed"}
	if sceneHash(req2, s, sp) != h {
		t.Fatal("the end card must not change a scene's hash")
	}
	for name, change := range map[string]func(*AdRequest, *ScriptScene){
		"words": func(_ *AdRequest, s *ScriptScene) { s.Voiceover = "Hello again" },
		"voice": func(r *AdRequest, _ *ScriptScene) { r.Voice = "Emily" },
		"actor": func(r *AdRequest, _ *ScriptScene) { r.Seed = 7 },
		"tier":  func(r *AdRequest, _ *ScriptScene) { r.Kind = AdPresenterPremium },
	} {
		r, sc := req, s
		change(&r, &sc)
		if sceneHash(r, sc, sp) == h {
			t.Errorf("%s did not change the hash", name)
		}
	}
}

// sceneFiles is a served store: voices, scene clips and ads land on a local server, so the
// assembler and the cache can fetch, check and copy them.
type sceneFiles struct {
	mu     sync.Mutex
	files  map[string][]byte
	srv    *httptest.Server
	copies int
	gotAd  string
	dir    string
}

func newSceneFiles(t *testing.T) *sceneFiles {
	s := &sceneFiles{files: map[string][]byte{}, dir: t.TempDir()}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, ok := s.files[strings.TrimPrefix(r.URL.Path, "/")]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sceneFiles) put(key string, b []byte) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[key] = b
	return key, s.srv.URL + "/" + key
}

func (s *sceneFiles) PutAudio(_ context.Context, tenantID uuid.UUID, versionID, name string, data []byte) (string, string, error) {
	k, u := s.put("marketflow/"+tenantID.String()+"/audio-drafts/voice/"+versionID+"-"+name+".mp3", data)
	return k, u, nil
}

func (s *sceneFiles) PutClip(_ context.Context, tenantID uuid.UUID, versionID, p string) (string, string, int64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", 0, err
	}
	k, u := s.put("marketflow/"+tenantID.String()+"/video-drafts/clips/"+versionID+".mp4", b)
	return k, u, int64(len(b)), nil
}

func (s *sceneFiles) PutAd(_ context.Context, tenantID uuid.UUID, versionID, p string) (string, string, int64, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", 0, err
	}
	s.gotAd = filepath.Join(s.dir, versionID+"-ad.mp4")
	_ = os.WriteFile(s.gotAd, b, 0o600)
	k, u := s.put("marketflow/"+tenantID.String()+"/video-drafts/ads/"+versionID+".mp4", b)
	return k, u, int64(len(b)), nil
}

func (s *sceneFiles) Exists(_ context.Context, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.files[key]
	return ok
}

func (s *sceneFiles) Copy(_ context.Context, tenantID uuid.UUID, key, name string) (string, string, error) {
	if !strings.Contains(key, "/"+tenantID.String()+"/") {
		return "", "", errors.New("not this tenant's")
	}
	s.mu.Lock()
	b, ok := s.files[key]
	s.copies++
	s.mu.Unlock()
	if !ok {
		return "", "", errors.New("gone")
	}
	k, u := s.put(path.Dir(key)+"/"+name+path.Ext(key), b)
	return k, u, nil
}

// ffmpegOrSkip returns ffmpeg's path, or skips the test.
func ffmpegOrSkip(t *testing.T) string {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return ff
}

func makeMedia(t *testing.T, ff, dir, name string, args ...string) string {
	p := filepath.Join(dir, name)
	if out, err := exec.Command(ff, append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), p)...).CombinedOutput(); err != nil {
		t.Fatalf("make %s: %v %s", name, err, out)
	}
	return p
}

// A template ad: every scene voiced on its own, as long as its words, with the end card in
// its last seconds and the scene's on-screen caption drawn. Needs ffmpeg.
func TestAdsMakeEndToEnd(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	voice, _ := os.ReadFile(makeMedia(t, ff, dir, "voice.mp3", "-f", "lavfi", "-i", "sine=frequency=330:duration=2", "-c:a", "libmp3lame"))
	files := newSceneFiles(t)
	files.put("photo.jpg", mustRead(t, makeMedia(t, ff, dir, "photo.jpg", "-f", "lavfi", "-i", "testsrc=size=1000x1000:duration=1", "-frames:v", "1")))
	files.put("clip.mp4", mustRead(t, makeMedia(t, ff, dir, "clip.mp4", "-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30:duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p")))

	newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	el.collection, el.audio, el.seconds = ownerCollection(), voice, 2

	ads := NewAds(NewVoices(keysFor("elevenlabs", "fal"), files, nil), NewAssembler(files, nil, nil)).WithClips(nil, files)
	res, err := ads.Make(context.Background(), AdRequest{
		TenantID: uuid.New(), VersionID: "ad1", Format: "1080x1920",
		Photos: []string{files.srv.URL + "/photo.jpg"}, Footage: []string{files.srv.URL + "/clip.mp4"},
		Scenes: []ScriptScene{
			{ID: "s1", Shot: ShotProduct, Asset: 1, Voiceover: "Soft skin all week.", Caption: "Soft skin"},
			{ID: "s2", Shot: ShotFootage, Voiceover: "Made in Kisumu."},
			{ID: "s3", Shot: ShotEndCard, Voiceover: "Order on WhatsApp."},
		},
		EndCard: Overlay{Headline: "Shea butter", Price: "KES 850", CTA: "Order on WhatsApp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(el.path, "/ijKilL5CnjXKMWDHOJH8/") || el.calls != 3 {
		t.Fatalf("one call per scene, all in the default voice Yna Agalo: %d %s", el.calls, el.path)
	}
	want := 3*(2+sceneGap) + endCardTail
	if res.Seconds < want-0.1 || res.Seconds > want+0.3 || res.CaptionLines < 3 || len(res.Scenes) != 3 || res.Voice == "" {
		t.Fatalf("result = %+v (want about %.2f s)", res, want)
	}
	img := frameAt(t, ff, dir, files.gotAd, res.Seconds-1)
	if r, g, b := rgbAt(img, 100, 1800); b < 150 || g > 110 || b < r+40 {
		t.Fatalf("no end card: %d,%d,%d", r, g, b)
	}
	// The scene caption "Soft skin" sits at the top of the safe band in scene 1: white
	// text with a black outline somewhere in that band.
	top := frameAt(t, ff, dir, files.gotAd, 1)
	if !hasWhiteAndBlack(top, 1920*20/100, 1920*30/100) {
		t.Fatal("no outlined scene caption at the top of the safe band")
	}
}

// A presenter ad: the presenter scene lip-synced from the actor and its own voice, the
// product scene animated, and a revision that changes one scene pays for that scene only.
// Needs ffmpeg.
func TestPresenterAdAndSceneCache(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	voice, _ := os.ReadFile(makeMedia(t, ff, dir, "voice.mp3", "-f", "lavfi", "-i", "sine=frequency=330:duration=2", "-c:a", "libmp3lame"))
	files := newSceneFiles(t)
	files.put("photo.jpg", mustRead(t, makeMedia(t, ff, dir, "photo.jpg", "-f", "lavfi", "-i", "testsrc=size=1000x1000:duration=1", "-frames:v", "1")))
	f := newFakeProviders(t)
	f.clip = mustRead(t, makeMedia(t, ff, dir, "clip.mp4", "-f", "lavfi", "-i", "testsrc=size=720x1280:rate=30:duration=4", "-c:v", "libx264", "-pix_fmt", "yuv420p"))
	el := newFakeEleven(t, "creator")
	el.collection, el.audio, el.seconds = ownerCollection(), voice, 2

	keys := keysFor("elevenlabs", "higgsfield")
	posters := NewPosters(keys, &memStore{}, nil)
	clips := NewClips(keys, files, posters, nil)
	ads := NewAds(NewVoices(keys, files, nil), NewAssembler(files, nil, nil)).
		WithPresenters(NewPresenters(keys, nil)).WithClips(clips, files)
	tenant := uuid.New()
	req := AdRequest{TenantID: tenant, VersionID: "v1", Format: "1080x1920", Kind: AdPresenter,
		ActorURL: files.srv.URL + "/photo.jpg", Photos: []string{files.srv.URL + "/photo.jpg"},
		Scenes: []ScriptScene{
			{ID: "s1", Shot: ShotPresenter, Voiceover: "Parents, the holiday is here."},
			{ID: "s2", Shot: ShotProduct, Asset: 1, Voiceover: "Kids build their own games.", Visual: "a child coding"},
			{ID: "s3", Shot: ShotEndCard, Voiceover: "WhatsApp us today."},
		},
		EndCard: Overlay{Headline: "Digitika Kids", CTA: "WhatsApp us"}}
	plan, err := ads.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scenes[0].Work != WorkPresenter || plan.Scenes[0].Route != "higgsfield/seedance-2.5-480p" || plan.Scenes[1].Work != WorkClip ||
		plan.Scenes[1].Route != "higgsfield/hailuo-2.3-std" || plan.Scenes[2].Work != WorkStill || plan.CostUSD <= 0 {
		t.Fatalf("plan = %+v", plan)
	}
	res, err := ads.Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	seedance := f.inputs["bytedance/seedance-2.5/reference-to-video"]
	if f.calls["bytedance/seedance-2.5/reference-to-video"] != 1 || f.calls["minimax/hailuo-2.3/standard/image-to-video"] != 1 || el.calls != 3 {
		t.Fatalf("calls = %v, voices %d", f.calls, el.calls)
	}
	audio, _ := seedance["audio_urls"].([]any)
	if len(audio) != 1 || !strings.Contains(audio[0].(string), "scene-s1-") {
		t.Fatalf("the presenter must lip-sync to its own scene's voice: %v", seedance)
	}

	// Revision: only the end card's words change.
	cache := map[string]SceneCache{}
	for _, c := range res.Scenes {
		cache[c.Hash] = c
	}
	rev := req
	rev.VersionID, rev.Cache = "v2", cache
	rev.Scenes = append([]ScriptScene(nil), req.Scenes...)
	rev.Scenes[2].Voiceover = "Send us a WhatsApp today."
	plan2, err := ads.Plan(context.Background(), rev)
	if err != nil || plan2.Cached != 2 || plan2.CostUSD >= plan.CostUSD/2 {
		t.Fatalf("revision plan = %+v %v", plan2, err)
	}
	if _, err := ads.Make(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
	if f.calls["bytedance/seedance-2.5/reference-to-video"] != 1 || f.calls["minimax/hailuo-2.3/standard/image-to-video"] != 1 || el.calls != 4 {
		t.Fatalf("the revision re-made unchanged scenes: %v, voices %d", f.calls, el.calls)
	}
	if files.copies < 3 {
		t.Fatalf("reused scenes must be copied to the new version: %d copies", files.copies)
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func frameAt(t *testing.T, ff, dir, video string, at float64) image.Image {
	p := filepath.Join(dir, "frame-"+strconv.FormatFloat(at, 'f', 2, 64)+".png")
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y", "-i", video, "-ss", strconv.FormatFloat(at, 'f', 2, 64),
		"-frames:v", "1", p).CombinedOutput(); err != nil {
		t.Fatalf("frame: %v %s", err, out)
	}
	fh, _ := os.Open(p)
	defer fh.Close()
	img, err := png.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// hasWhiteAndBlack reports near-white and near-black pixels between rows y0 and y1.
func hasWhiteAndBlack(img image.Image, y0, y1 int) bool {
	white, black := false, false
	b := img.Bounds()
	for y := y0; y < y1; y += 2 {
		for x := b.Min.X; x < b.Max.X; x += 2 {
			r, g, bl := rgbAt(img, x, y)
			if r > 235 && g > 235 && bl > 235 {
				white = true
			}
			if r < 20 && g < 20 && bl < 20 {
				black = true
			}
		}
	}
	return white && black
}

func rgbAt(img image.Image, x, y int) (int, int, int) {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}
