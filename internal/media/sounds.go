package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Sound cues and music for the audio ladder (audio_ladder.go), on the same router, breaker
// and fal queue as the other media. A cue or a score that fails is skipped by the caller:
// it never fails an ad and is never charged.

// Sound capabilities.
const (
	CapSFX   Capability = "sound_effect"
	CapMusic Capability = "music"
)

// SoundRoutes price per second of output (docs/integrations/video-generation-providers.md,
// audio section): fal's ElevenLabs sound effects at USD 0.002 a second, and music at
// USD 0.60 a minute. ElevenLabs Music direct is a subscription, eligible only on a paid
// plan (as the direct voice route), so its price here is fal's, as an upper bound.
var SoundRoutes = []Route{
	{Key: "fal/elevenlabs-sfx-v3", Provider: "fal", Capability: CapSFX, PriceUSD: 0.002, PerSecond: true, MaxSeconds: 30,
		Quality: 4, PAYG: true, Timeout: time.Minute},
	{Key: "elevenlabs/music", Provider: "elevenlabs", Capability: CapMusic, PriceUSD: 0.01, PerSecond: true, MaxSeconds: 600,
		Quality: 4.5, Timeout: 3 * time.Minute},
	{Key: "fal/elevenlabs-music-v2.5", Provider: "fal", Capability: CapMusic, PriceUSD: 0.01, PerSecond: true, MaxSeconds: 600,
		Quality: 4.5, PAYG: true, Timeout: 4 * time.Minute, Fallback: true},
}

// fal endpoints (input schemas read from fal's OpenAPI on 2026-11-12).
const (
	falSFXEndpoint   = "elevenlabs/sound-effects/v3"
	falMusicEndpoint = "elevenlabs/music/v2.5"
	// minMusicSeconds is the shortest music both providers make (music_length_ms 3000).
	minMusicSeconds = 3.0
)

// SoundRequest is one cue or one piece of music.
type SoundRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Name      string // names the file within the version
	Prompt    string
	Seconds   float64
	Tier      string
}

// SoundResult is the stored cue or music.
type SoundResult struct {
	URL     string  `json:"url"`
	Key     string  `json:"key"`
	Route   string  `json:"route"`
	CostUSD float64 `json:"cost_usd"`
}

// Sounds makes sound cues and music on the best route with fallbacks.
type Sounds struct {
	router     *Router
	keys       KeySource
	store      AudioStore
	queue      *queueClient
	http       *http.Client
	paid       func(ctx context.Context, key string) bool // ElevenLabs paid plan check
	musicModel string
	logger     *zap.Logger
}

// NewSounds builds the sound pipeline. voices supplies the ElevenLabs plan check (nil: the
// direct route is never used).
func NewSounds(keys KeySource, store AudioStore, voices *Voices, logger *zap.Logger) *Sounds {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Sounds{router: NewRouter(SoundRoutes), keys: keys, store: store, queue: newQueueClient(),
		http: &http.Client{Timeout: 3 * time.Minute}, musicModel: defaultMusicModel, logger: logger.Named("media.sounds")}
	if voices != nil {
		s.paid = voices.paidPlan
	}
	return s
}

// WithMusicModel sets the ElevenLabs Music model id; "" keeps the default.
func (s *Sounds) WithMusicModel(model string) *Sounds {
	if model = strings.TrimSpace(model); model != "" {
		s.musicModel = model
	}
	return s
}

func (s *Sounds) withLogger(l *zap.Logger) {
	if l != nil {
		s.logger = l.Named("media.sounds")
	}
}

// key is the provider's key, "" when none is configured.
func (s *Sounds) key(ctx context.Context, provider string) string {
	if s.keys == nil {
		return ""
	}
	return s.keys(ctx, provider)
}

// Quote prices one output of the given length on the first route with a key, without any
// provider call. No route means nothing would be made, so the price is 0.
func (s *Sounds) Quote(ctx context.Context, cap Capability, tier string, seconds float64) (float64, string) {
	if s == nil {
		return 0, ""
	}
	seconds = soundSeconds(cap, seconds)
	routes := s.router.OrderFor(cap, tierOr(tier), seconds, func(rt Route) bool { return s.key(ctx, rt.Provider) != "" })
	if len(routes) == 0 {
		return 0, ""
	}
	return routes[0].Cost(seconds), routes[0].Key
}

// Effect makes one sound cue.
func (s *Sounds) Effect(ctx context.Context, req SoundRequest) (*SoundResult, error) {
	return s.make(ctx, CapSFX, req)
}

// Music makes one instrumental piece of the asked length.
func (s *Sounds) Music(ctx context.Context, req SoundRequest) (*SoundResult, error) {
	return s.make(ctx, CapMusic, req)
}

func tierOr(tier string) string {
	if tier == "" {
		return TierStandard
	}
	return tier
}

// soundSeconds is the length asked for: a cue within the cue bounds, music at least the
// providers' minimum.
func soundSeconds(cap Capability, seconds float64) float64 {
	if cap == CapSFX {
		return min(max(seconds, minSFXSeconds), maxSFXSeconds)
	}
	return max(seconds, minMusicSeconds)
}

func (s *Sounds) make(ctx context.Context, cap Capability, req SoundRequest) (*SoundResult, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("media: sounds are not configured")
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return nil, fmt.Errorf("media: a sound needs a prompt")
	}
	req.Seconds = soundSeconds(cap, req.Seconds)
	routes := s.router.OrderFor(cap, tierOr(req.Tier), req.Seconds, func(rt Route) bool { return s.eligible(ctx, rt, req.Seconds) })
	var lastErr error
	for _, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		audio, err := s.run(actx, rt, req)
		cancel()
		if err != nil {
			lastErr = err
			s.router.Failed(rt.Key, err, ctx.Err() != nil)
			s.logger.Warn("sound route failed, trying the next", zap.String("route", rt.Key), zap.Error(err))
			continue
		}
		s.router.Succeeded(rt.Key)
		key, url, err := s.store.PutAudio(ctx, req.TenantID, req.VersionID, req.Name, audio)
		if err != nil {
			return nil, fmt.Errorf("media: store sound: %w", err)
		}
		return &SoundResult{URL: url, Key: key, Route: rt.Key, CostUSD: rt.Cost(req.Seconds)}, nil
	}
	if lastErr == nil {
		return nil, ErrNoRoute
	}
	return nil, fmt.Errorf("%w: %w", ErrNoRoute, lastErr)
}

// eligible needs the provider's key and its balance; ElevenLabs direct needs a paid plan.
func (s *Sounds) eligible(ctx context.Context, rt Route, seconds float64) bool {
	key := s.key(ctx, rt.Provider)
	if key == "" {
		return false
	}
	if rt.Provider == "elevenlabs" {
		return s.paid != nil && s.paid(ctx, key)
	}
	return providerBalances.Allows(rt.Provider, rt.Cost(seconds))
}

// run makes the audio on one route.
func (s *Sounds) run(ctx context.Context, rt Route, req SoundRequest) ([]byte, error) {
	key := s.key(ctx, rt.Provider)
	ms := int(math.Round(req.Seconds * 1000))
	switch rt.Key {
	case "elevenlabs/music":
		return s.elevenMusic(ctx, key, req.Prompt, ms)
	case "fal/elevenlabs-sfx-v3", "fal/elevenlabs-music-v2.5":
		endpoint := falSFXEndpoint
		input := map[string]any{"text": req.Prompt, "duration_seconds": req.Seconds, "output_format": "mp3_44100_128"}
		if rt.Capability == CapMusic {
			endpoint = falMusicEndpoint
			input = map[string]any{"prompt": req.Prompt, "music_length_ms": ms, "force_instrumental": true, "output_format": "mp3_44100_128"}
		}
		var out struct {
			Audio struct {
				URL string `json:"url"`
			} `json:"audio"`
		}
		if err := s.queue.run(ctx, falJob(key, endpoint), input, &out); err != nil {
			return nil, err
		}
		if out.Audio.URL == "" {
			return nil, &errJobFailed{provider: "fal", status: "completed", detail: "no audio in the result"}
		}
		return fetchAudio(ctx, s.http, out.Audio.URL, string(rt.Capability))
	}
	return nil, fmt.Errorf("media: no builder for route %s", rt.Key)
}

// elevenMusic calls ElevenLabs' compose endpoint (POST /v1/music, read 2026-11-12): a prompt,
// the length in milliseconds (3000 to 600000), the model and force_instrumental; the reply
// is the audio file itself.
func (s *Sounds) elevenMusic(ctx context.Context, key, prompt string, ms int) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{"prompt": prompt, "music_length_ms": ms, "model_id": s.musicModel, "force_instrumental": true})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, elevenBase+"/v1/music?output_format=mp3_44100_128", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("xi-api-key", key)
	hr.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxAudioBytes+1))
	if resp.StatusCode != http.StatusOK {
		return nil, &errJobFailed{provider: "elevenlabs", status: fmt.Sprint(resp.StatusCode), detail: truncate(string(data), 300)}
	}
	// A reply cut off part way (timeout, reset) or larger than the cap is not music to keep:
	// the route fails so the fallback runs, and nothing is charged for it.
	if rerr != nil {
		return nil, fmt.Errorf("elevenlabs: music reply cut off: %w", rerr)
	}
	if len(data) > maxAudioBytes {
		return nil, fmt.Errorf("elevenlabs: music reply is over %d bytes", maxAudioBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("elevenlabs: no audio in the music reply")
	}
	return data, nil
}
