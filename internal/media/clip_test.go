package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fileStore keeps clips on disk for the test.
type fileStore struct{ dir, got string }

func (s *fileStore) PutClip(_ context.Context, tenantID uuid.UUID, versionID, path string) (string, string, int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", 0, err
	}
	s.got = filepath.Join(s.dir, versionID+".mp4")
	if err := os.WriteFile(s.got, b, 0o600); err != nil {
		return "", "", 0, err
	}
	key := "marketflow/" + tenantID.String() + "/video-drafts/clips/" + versionID + ".mp4"
	return key, "https://media.example/" + key, int64(len(b)), nil
}

// Without ffmpeg nothing is made and no provider is called (nothing is paid).
func TestClipNeedsFFmpeg(t *testing.T) {
	f := newFakeProviders(t)
	c := NewClips(keysFor("higgsfield"), &fileStore{dir: t.TempDir()}, nil, nil)
	c.ffmpeg = ""
	if _, err := c.Make(context.Background(), ClipRequest{TenantID: uuid.New(), VersionID: "v", SceneURL: f.srv.URL + "/scene.png", Format: "1080x1350", Seconds: 5}); err != ErrNoFFmpeg {
		t.Fatalf("err = %v", err)
	}
	if len(f.inputs) != 0 {
		t.Fatalf("a provider was called without ffmpeg: %v", f.inputs)
	}
}

// The default route gets the confirmed Kling 3.0 std shape, and the stored clip
// is the format's size with the text layer burned in. Needs ffmpeg (CI image and
// production have it; skipped elsewhere).
func TestClipKlingShapeAndOverlay(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	f := newFakeProviders(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	// A 6 s, 720x1280 test pattern stands in for the provider's clip.
	if out, err := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=720x1280:rate=24:duration=6",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("make test clip: %v %s", err, out)
	}
	f.clip, _ = os.ReadFile(src)

	store := &fileStore{dir: t.TempDir()}
	c := NewClips(keysFor("higgsfield", "fal"), store, nil, nil)
	res, err := c.Make(context.Background(), ClipRequest{
		TenantID: uuid.New(), VersionID: "v1", SceneURL: f.srv.URL + "/scene.png", Format: "1080x1350", Seconds: 5,
		Overlay: Overlay{Headline: "Soft skin", Price: "KES 850", CTA: "Order on WhatsApp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := f.inputs["kling-video/v3.0/std/image-to-video"]
	if in == nil || in["image_url"] != f.srv.URL+"/scene.png" || in["duration"].(float64) != 5 || in["sound"] != "off" {
		t.Fatalf("kling input = %v (all: %v)", in, f.inputs)
	}
	if !strings.Contains(in["prompt"].(string), "No text") {
		t.Fatalf("prompt must keep the frame free of text: %v", in["prompt"])
	}
	if res.Route != "higgsfield/kling-v3-std" || res.CostUSD < 0.41 || res.CostUSD > 0.43 || res.Bytes == 0 {
		t.Fatalf("result = %+v", res)
	}
	// The stored clip is 1080x1350, 5 s, H.264.
	probe, err := exec.Command(ff, "-hide_banner", "-i", store.got).CombinedOutput()
	if !strings.Contains(string(probe), "1080x1350") || !strings.Contains(string(probe), "h264") || !strings.Contains(string(probe), "Duration: 00:00:05") {
		t.Fatalf("stored clip: %v %s", err, probe)
	}
}
