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
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func (s *fileStore) PutAd(_ context.Context, tenantID uuid.UUID, versionID, path string) (string, string, int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", 0, err
	}
	s.got = filepath.Join(s.dir, versionID+"-ad.mp4")
	if err := os.WriteFile(s.got, b, 0o600); err != nil {
		return "", "", 0, err
	}
	key := "marketflow/" + tenantID.String() + "/video-drafts/ads/" + versionID + ".mp4"
	return key, "https://media.example/" + key, int64(len(b)), nil
}

// Lines break at sentence ends and after four words, hold until the next line,
// and never flash for less than the minimum.
func TestCaptionLines(t *testing.T) {
	var words []WordTiming
	for i, w := range strings.Fields("Soft skin all week long. Order shea butter on WhatsApp today!") {
		words = append(words, WordTiming{Text: w, Start: float64(i) * 0.3, End: float64(i)*0.3 + 0.25})
	}
	lines := CaptionLines(words)
	got := make([]string, len(lines))
	for i, l := range lines {
		got[i] = l.Text
	}
	want := []string{"Soft skin all week", "long.", "Order shea butter on", "WhatsApp today!"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q", got)
	}
	for i := 0; i+1 < len(lines); i++ {
		if lines[i].End != lines[i+1].Start {
			t.Fatalf("line %d does not hold until the next: %+v", i, lines)
		}
	}
	if l := CaptionLines([]WordTiming{{Text: "Hi", Start: 1, End: 1.1}}); l[0].End-l[0].Start < captionMinSeconds {
		t.Fatalf("too short: %+v", l)
	}
	// Fast speech: a line also ends before it runs past captionMaxSeconds.
	fast := CaptionLines(estimateWords("Soft skin all week long", 2.5))
	if len(fast) < 2 {
		t.Fatalf("a 2.5 s run of five words must split: %+v", fast)
	}
}

func TestComposeCaptionDrawsInTheLowerThird(t *testing.T) {
	img, err := ComposeCaption("1080x1920", "Order shea butter today")
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 1080 || b.Dy() != 1920 {
		t.Fatalf("size %v", b)
	}
	if _, _, _, a := img.At(540, 100).RGBA(); a != 0 {
		t.Fatal("the top of the frame must stay clear")
	}
	opaque := 0
	for y := 1920 * 55 / 100; y < 1920*72/100; y++ {
		if _, _, _, a := img.At(540, y).RGBA(); a > 0 {
			opaque++
		}
	}
	if opaque == 0 {
		t.Fatal("no caption box above 72% of the height")
	}
}

func TestAssembleValidates(t *testing.T) {
	a := NewAssembler(&fileStore{dir: t.TempDir()}, nil, nil)
	a.ffmpeg = "ffmpeg" // validation runs before ffmpeg is used
	for _, req := range []AssembleRequest{
		{Format: "800x600", Scenes: []AdScene{{ImageURL: "https://x/a.png", Seconds: 2}}},
		{Format: "1080x1350"},
		{Format: "1080x1350", Scenes: []AdScene{{ImageURL: "https://x/a.png", VideoURL: "https://x/a.mp4", Seconds: 2}}},
		{Format: "1080x1350", Scenes: []AdScene{{ImageURL: "file:///etc/passwd", Seconds: 2}}},
		{Format: "1080x1350", Scenes: []AdScene{{ImageURL: "https://x/a.png", Seconds: 0.5}}},
		{Format: "1080x1350", Scenes: []AdScene{{ImageURL: "https://x/a.png", Seconds: 61}}},
		{Format: "1080x1350", Scenes: []AdScene{{ImageURL: "https://x/a.png", Seconds: 2, StartAt: 1}}},
	} {
		if _, err := a.Make(context.Background(), req); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
	a.ffmpeg = ""
	if _, err := a.Make(context.Background(), AssembleRequest{}); !errors.Is(err, ErrNoFFmpeg) {
		t.Fatalf("no ffmpeg: %v", err)
	}
}

// A clip scene and a still scene, a voice-over with music, captions and an end
// card make one 1080x1920 ad of the scenes' length, H.264 with AAC. Needs ffmpeg.
func TestAssembleMakesTheAd(t *testing.T) {
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
	// The clip is shorter than its scene, so its last frame must be held.
	files := map[string]string{
		"/clip.mp4":  mk("clip.mp4", "-f", "lavfi", "-i", "testsrc=size=720x1280:rate=24:duration=2", "-c:v", "libx264", "-pix_fmt", "yuv420p"),
		// WebP, as on the website: its decoder's timing once broke the overlays.
		"/still.webp": mk("still.webp", "-f", "lavfi", "-i", "testsrc=size=1200x1200:duration=1", "-frames:v", "1", "-c:v", "libwebp"),
		"/voice.mp3": mk("voice.mp3", "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-c:a", "libmp3lame"),
		"/music.mp3": mk("music.mp3", "-f", "lavfi", "-i", "sine=frequency=220:duration=10", "-c:a", "libmp3lame"),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, p)
	}))
	defer srv.Close()

	store := &fileStore{dir: t.TempDir()}
	a := NewAssembler(store, nil, nil)
	res, err := a.Make(context.Background(), AssembleRequest{
		TenantID: uuid.New(), VersionID: "ad1", Format: "1080x1920",
		// The landscape-style clip is fitted over a blurred fill, from 0.5 s in. The
		// voice runs into the still, so captions must show over the last scene too.
		Scenes:   []AdScene{{VideoURL: srv.URL + "/clip.mp4", Seconds: 3, StartAt: 0.5, Fit: true}, {ImageURL: srv.URL + "/still.webp", Seconds: 6}},
		VoiceURL: srv.URL + "/voice.mp3", MusicURL: srv.URL + "/music.mp3",
		Words: estimateWords("Soft skin all week. Order shea butter today.", 5), Captions: true,
		EndCard: &Overlay{Headline: "Shea butter", Price: "KES 850", CTA: "Order on WhatsApp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Seconds != 9 || res.Width != 1080 || res.Height != 1920 || res.SHA256 == "" || res.Lines < 2 ||
		!strings.HasSuffix(res.Key, "/video-drafts/ads/ad1.mp4") {
		t.Fatalf("result = %+v", res)
	}
	probe, _ := exec.Command(ff, "-hide_banner", "-i", store.got).CombinedOutput()
	s := string(probe)
	for _, want := range []string{"1080x1920", "h264", "aac", "30 fps", "Duration: 00:00:09.0"} {
		if !strings.Contains(s, want) {
			t.Fatalf("stored ad lacks %q:\n%s", want, s)
		}
	}

	// Real frames, not just the container: probing size and duration let a
	// broken overlay clock and a frozen push-in pass before.
	frame := func(at string) image.Image {
		t.Helper()
		p := filepath.Join(dir, "f"+at+".png")
		if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y", "-i", store.got, "-ss", at, "-frames:v", "1", p).CombinedOutput(); err != nil {
			t.Fatalf("frame at %s: %v %s", at, err, out)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		img, err := png.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	rgb := func(img image.Image, x, y int) (int, int, int) {
		r, g, b, _ := img.At(x, y).RGBA()
		return int(r >> 8), int(g >> 8), int(b >> 8)
	}
	// A caption box over the still scene: dark just inside its bottom edge.
	if r, g, b := rgb(frame("4.5"), 540, 1920*72/100-6); r+g+b > 3*90 {
		t.Fatalf("no caption over the last scene at 4.5 s: %d,%d,%d", r, g, b)
	}
	// The still moves: the same spot differs between two moments of the push-in.
	a1, a2 := frame("3.3"), frame("5.7")
	diff := 0
	for y := 200; y < 400; y += 10 {
		for x := 200; x < 880; x += 10 {
			r1, g1, b1 := rgb(a1, x, y)
			r2, g2, b2 := rgb(a2, x, y)
			diff += abs(r1-r2) + abs(g1-g2) + abs(b1-b2)
		}
	}
	if diff < 2000 {
		t.Fatalf("the still does not move (difference %d)", diff)
	}
	// The end card's button (accent purple) in the last seconds.
	if r, g, b := rgb(frame("8.4"), 100, 1800); b < 150 || g > 110 || b < r+40 {
		t.Fatalf("no end card button at 8.4 s: %d,%d,%d", r, g, b)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
