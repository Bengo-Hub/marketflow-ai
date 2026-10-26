package media

import (
	"context"
	"errors"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Each scene lasts from where the previous ended to the next scene's first
// word; a scene without lines keeps its scripted length; the last holds for
// the end card.
func TestSceneTimesFollowTheVoice(t *testing.T) {
	scenes := []ScriptScene{
		{Shot: ShotProduct, Voiceover: "Soft skin all week."},      // 4 words
		{Shot: ShotBRoll, DurationS: 2},                            // silent
		{Shot: ShotProduct, Voiceover: "Order today."},             // 2 words
		{Shot: ShotEndCard, Voiceover: "Shea butter from Kisumu."}, // 4 words
	}
	var words []WordTiming
	for i := 0; i < 10; i++ {
		words = append(words, WordTiming{Text: "w", Start: float64(i) * 0.5, End: float64(i)*0.5 + 0.4})
	}
	got := sceneTimes(scenes, words, 5)
	// Scene 1 ends where word 5 starts (2.0); the silent scene keeps 2 s; scene 3
	// ends where word 7 starts (3.0) but must cover its own words; the last
	// runs to the last word (4.9) plus the end card.
	if !near(got[0], 2.0) || !near(got[1], 2) {
		t.Fatalf("times = %v", got)
	}
	if got[3] < endCardTail {
		t.Fatalf("the last scene must hold for the end card: %v", got)
	}
	total := 0.0
	for _, s := range got {
		if s < minSceneSeconds {
			t.Fatalf("scene shorter than the minimum: %v", got)
		}
		total += s
	}
	// A mismatched word count falls back to shares of the characters.
	if p := sceneTimes(scenes, words[:3], 6); len(p) != 4 || p[1] != 2 || p[0] <= 0 {
		t.Fatalf("proportional = %v", p)
	}
}

func TestAdsValidate(t *testing.T) {
	a := NewAds(nil, nil)
	ok := AdRequest{Format: "1080x1920", Photos: []string{"https://x/p.jpg"},
		Scenes: []ScriptScene{{Shot: ShotProduct, Voiceover: "Soft skin"}, {Shot: ShotEndCard, Voiceover: "Order"}}}
	if err := a.Validate(ok); err != nil {
		t.Fatalf("valid script refused: %v", err)
	}
	bad := map[string]func(r *AdRequest){
		"presenter":      func(r *AdRequest) { r.Scenes[0].Shot = ShotPresenter },
		"no footage":     func(r *AdRequest) { r.Scenes[0].Shot = ShotFootage },
		"photo index":    func(r *AdRequest) { r.Scenes[0].Asset = 2 },
		"unknown shot":   func(r *AdRequest) { r.Scenes[0].Shot = "drone" },
		"one scene":      func(r *AdRequest) { r.Scenes = r.Scenes[:1] },
		"no voice":       func(r *AdRequest) { r.Scenes[0].Voiceover, r.Scenes[1].Voiceover = "", "" },
		"too much voice": func(r *AdRequest) { r.Scenes[0].Voiceover = strings.Repeat("a ", MaxAdVoiceChars) },
		"local photo":    func(r *AdRequest) { r.Photos = []string{"file:///etc/passwd"} },
		"format":         func(r *AdRequest) { r.Format = "800x600" },
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
}

// servedAudio keeps voice-overs on a local server, so the assembler can fetch them.
type servedAudio struct {
	mu    sync.Mutex
	files map[string][]byte
	srv   *httptest.Server
}

func newServedAudio(t *testing.T) *servedAudio {
	s := &servedAudio{files: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		b, ok := s.files[r.URL.Path]
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

func (s *servedAudio) PutAudio(_ context.Context, _ uuid.UUID, versionID, name string, data []byte) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := "/" + versionID + "-" + name + ".mp3"
	s.files[p] = data
	return "audio-drafts/voice" + p, s.srv.URL + p, nil
}

// A script, the owner's photo and clip and the default voice make one ad: as
// long as the voice plus the end card, with the end card in its last seconds.
// Needs ffmpeg.
func TestAdsMakeEndToEnd(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	mk := func(name string, args ...string) string {
		p := filepath.Join(dir, name)
		if out, err := exec.Command(ff, append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), p)...).CombinedOutput(); err != nil {
			t.Fatalf("make %s: %v %s", name, err, out)
		}
		return p
	}
	voice, _ := os.ReadFile(mk("voice.mp3", "-f", "lavfi", "-i", "sine=frequency=330:duration=4", "-c:a", "libmp3lame"))
	assets := map[string]string{
		"/photo.jpg": mk("photo.jpg", "-f", "lavfi", "-i", "testsrc=size=1000x1000:duration=1", "-frames:v", "1"),
		"/clip.mp4":  mk("clip.mp4", "-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30:duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := assets[r.URL.Path]; ok {
			http.ServeFile(w, r, p)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	el.collection, el.audio, el.seconds = ownerCollection(), voice, 4

	store := &fileStore{dir: t.TempDir()}
	ads := NewAds(NewVoices(keysFor("elevenlabs", "fal"), newServedAudio(t), nil), NewAssembler(store, nil, nil))
	res, err := ads.Make(context.Background(), AdRequest{
		TenantID: uuid.New(), VersionID: "ad1", Format: "1080x1920",
		Photos: []string{srv.URL + "/photo.jpg"}, Footage: []string{srv.URL + "/clip.mp4"},
		Scenes: []ScriptScene{
			{Shot: ShotProduct, Asset: 1, Voiceover: "Soft skin all week.", Caption: "Soft skin"},
			{Shot: ShotFootage, Voiceover: "Made in Kisumu."},
			{Shot: ShotEndCard, Voiceover: "Order on WhatsApp."},
		},
		EndCard: Overlay{Headline: "Shea butter", Price: "KES 850", CTA: "Order on WhatsApp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(el.path, "/ijKilL5CnjXKMWDHOJH8/") {
		t.Fatalf("the default voice must be Yna Agalo: %s", el.path)
	}
	// The voice ends at 4 s; the end card adds endCardTail.
	if res.Seconds < 4+endCardTail-0.05 || res.Seconds > 4+endCardTail+0.5 || res.CaptionLines < 2 || res.Route != "elevenlabs/eleven-v4-turbo" {
		t.Fatalf("result = %+v", res)
	}
	p := filepath.Join(dir, "last.png")
	at := res.Seconds - 1
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y", "-i", store.got, "-ss", strconv.FormatFloat(at, 'f', 2, 64),
		"-frames:v", "1", p).CombinedOutput(); err != nil {
		t.Fatalf("frame: %v %s", err, out)
	}
	f, _ := os.Open(p)
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b := rgbAt(img, 100, 1800); b < 150 || g > 110 || b < r+40 {
		t.Fatalf("no end card at %.1f s: %d,%d,%d", at, r, g, b)
	}
}

func rgbAt(img image.Image, x, y int) (int, int, int) {
	r, g, b, _ := img.At(x, y).RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}
