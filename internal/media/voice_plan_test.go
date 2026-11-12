package media

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A plan lookup that fails (a key without user read permission, a slow reply) is not a free
// plan: it is retried shortly, where a known plan is kept for the hour. Before, one failure
// sent every voice to fal's stock voices for an hour without a word in the log.
func TestVoicePlanLookupFailureIsRetried(t *testing.T) {
	newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	el.collection = ownerCollection()
	el.subStatus = 401
	old := planRetry
	planRetry = 0
	t.Cleanup(func() { planRetry = old })

	v := NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Soft skin all week"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "fal/elevenlabs-v3" || el.got != nil {
		t.Fatalf("an unreadable plan must not use the direct route: %+v", res)
	}

	el.mu.Lock()
	el.subStatus = 0
	el.mu.Unlock()
	res, err = v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v2", Text: "Soft skin all week"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "elevenlabs/eleven-v4-turbo" || res.SubstitutedFor != "" {
		t.Fatalf("the plan is read again once the lookup works: %+v", res)
	}
}

// A plan that was read is kept for the hour; a failed read only for planRetry.
func TestVoicePlanCacheWindows(t *testing.T) {
	now := time.Now()
	known := planEntry{paid: false, known: true, at: now.Add(-30 * time.Minute)}
	failed := planEntry{paid: false, known: false, at: now.Add(-2 * time.Minute)}
	if !known.fresh(now) {
		t.Error("a known free plan stays cached for the hour")
	}
	if failed.fresh(now) {
		t.Error("a failed lookup is retried after a minute")
	}
	if !(planEntry{known: false, at: now.Add(-10 * time.Second)}).fresh(now) {
		t.Error("a failed lookup is not retried within the minute")
	}
}

// A clean up chain the local ffmpeg cannot run is switched off at start instead of failing
// every ad; the default chain runs.
func TestVoiceFilterIsTriedAtStart(t *testing.T) {
	ffmpegOrSkip(t)
	asm := NewAssembler(&fileStore{}, nil, nil)
	if !asm.Ready() {
		t.Skip("assembler not ready")
	}
	good := NewAds(nil, asm).WithAudio(DefaultAudioConfig())
	if good.audio.VoiceFilter != DefaultAudioConfig().VoiceFilter {
		t.Fatalf("the default chain must run on this ffmpeg, got %q", good.audio.VoiceFilter)
	}
	cfg := DefaultAudioConfig()
	cfg.VoiceFilter = "highpass=f=80,nosuchfilter=x=1"
	if bad := NewAds(nil, asm).WithAudio(cfg); bad.audio.VoiceFilter != "" {
		t.Fatalf("a chain that does not run is switched off, got %q", bad.audio.VoiceFilter)
	}
}
