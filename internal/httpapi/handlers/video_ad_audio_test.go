package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/team"
)

type fakeContextSource struct {
	tc  team.TenantContext
	err error
}

func (f fakeContextSource) Load(context.Context, uuid.UUID) (team.TenantContext, error) {
	return f.tc, f.err
}

// The soundtrack level follows the plan's features (owner, 2026-11-12). A plan that cannot be
// read gets the voice-only level, the cheapest, so nobody is overcharged.
func TestAudioLevelFollowsPlanFeatures(t *testing.T) {
	ads := media.NewAds(nil, nil)
	level := func(src team.ContextSource) string {
		h := &OSHandler{ads: ads, source: src}
		return h.audioLevel(context.Background(), uuid.New())
	}
	cases := []struct {
		features map[string]bool
		want     string
	}{
		{map[string]bool{"ai_video_template": true}, media.AudioVoice},
		{map[string]bool{"ai_video_template": true, "ai_video_presenter": true}, media.AudioSFX},
		{map[string]bool{"ai_video_presenter": true, "ai_video_cinematic": true}, media.AudioTrack},
		{map[string]bool{"ai_video_presenter": true, "ai_video_cinematic": true, "ai_video_presenter_premium": true}, media.AudioScore},
	}
	for _, c := range cases {
		if got := level(fakeContextSource{tc: team.TenantContext{Features: c.features}}); got != c.want {
			t.Errorf("features %v: level %q, want %q", c.features, got, c.want)
		}
	}
	if got := level(fakeContextSource{err: errors.New("marketflow-api down")}); got != media.AudioVoice {
		t.Errorf("unreadable plan: level %q, want voice", got)
	}
	if got := (&OSHandler{ads: ads}).audioLevel(context.Background(), uuid.New()); got != media.AudioVoice {
		t.Errorf("no source: level %q, want voice", got)
	}
}

// Cues and music are charged for what was made: nothing made costs nothing, part made costs
// its share (rounded up), and never more than quoted.
func TestMadeExtraCredits(t *testing.T) {
	cases := []struct {
		quoted          int
		quotedUSD, made float64
		want            int
	}{
		{4, 0.40, 0.40, 4},
		{4, 0.40, 0.10, 1},
		{4, 0.40, 0.11, 2},
		{4, 0.40, 0, 0},
		{4, 0.40, 0.90, 4},
		{0, 0.40, 0.40, 0},
		{4, 0, 0.40, 0},
	}
	for _, c := range cases {
		if got := madeExtraCredits(c.quoted, c.quotedUSD, c.made); got != c.want {
			t.Errorf("madeExtraCredits(%d, %.2f, %.2f) = %d, want %d", c.quoted, c.quotedUSD, c.made, got, c.want)
		}
	}
}

// Review fix (2026-11-12): paid cues and music are made only when the balance covers the
// whole quote; otherwise the ad drops to the nearest level without paid parts.
func TestAffordableAudioLowersTheLevel(t *testing.T) {
	reg := registry.Defaults()
	plan := media.AdPlan{Kind: media.AdTemplate, Audio: media.AudioScore, VoiceChars: 300, MusicUSD: 0.30}
	full, err := adCredits(reg, plan)
	if err != nil {
		t.Fatal(err)
	}
	h := &OSHandler{credits: &balanceCredits{n: int64(full)}}
	if level, lowered := h.affordableAudio(context.Background(), uuid.New(), reg, plan); lowered || level != media.AudioScore {
		t.Fatalf("a balance covering the quote keeps the level: %s %v", level, lowered)
	}
	h = &OSHandler{credits: &balanceCredits{n: int64(full - 1)}}
	if level, lowered := h.affordableAudio(context.Background(), uuid.New(), reg, plan); !lowered || level != media.AudioTrack {
		t.Fatalf("a short balance drops score to track: %s %v", level, lowered)
	}
}

// Review fix: a template ad's cues and music add their cost only, not a second render
// surcharge.
func TestTemplateExtrasAddNoSecondSurcharge(t *testing.T) {
	reg := registry.Defaults()
	reg.Settings.FixedSurcharge = 5
	base := media.AdPlan{Kind: media.AdTemplate, VoiceChars: 300}
	withCue := base
	withCue.SFXUSD = 0.004
	b, _ := adCredits(reg, base)
	c, _ := adCredits(reg, withCue)
	if c-b != reg.CostCredits(0.004) || c-b >= 5 {
		t.Fatalf("one cue added %d credits (cost alone is %d)", c-b, reg.CostCredits(0.004))
	}
}
