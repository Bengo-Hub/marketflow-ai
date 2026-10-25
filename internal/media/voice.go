package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CapVoice reads an ad's script aloud.
const CapVoice Capability = "voice"

// VoiceRoutes price per 1,000 characters (docs/integrations/video-generation-providers.md
// section 5). ElevenLabs direct is a subscription, so it is eligible only on a
// paid plan: the free plan's output may not be used commercially.
var VoiceRoutes = []Route{
	{Key: "elevenlabs/eleven-v4-turbo", Provider: "elevenlabs", Capability: CapVoice, PriceUSD: 0.04, Quality: 4, Timeout: time.Minute},
	{Key: "fal/elevenlabs-v3", Provider: "fal", Capability: CapVoice, PriceUSD: 0.10, Quality: 4, PAYG: true, Timeout: 90 * time.Second, Fallback: true},
	{Key: "elevenlabs/eleven-v4", Provider: "elevenlabs", Capability: CapVoice, PriceUSD: 0.08, Quality: 4.8, Timeout: time.Minute, MinTier: TierPremium},
}

// MaxVoiceChars bounds one voice-over (a 60 s ad is about 900 characters).
const MaxVoiceChars = 3000

// VoiceCatalog maps the voices the studio offers to their ElevenLabs ids. fal's
// ElevenLabs route takes the name. All are ElevenLabs premade voices, so both
// routes have them.
var VoiceCatalog = map[string]string{
	"Bella":   "hpp4J3VqNfWAUOO0d1Us", // warm, bright female
	"Sarah":   "EXAVITQu4vr4xnSDxMaL", // mature, reassuring female
	"Matilda": "XrExE9yKIg1WjnnlVkGX", // professional female
	"Alice":   "Xb7hH8MSUJpSbSDYk0k2", // clear British female
	"Brian":   "nPczCjzI2devNBz1zQrb", // deep, comforting male
	"Eric":    "cjVigY5qzO86Huf0OWal", // smooth, trustworthy male
	"Chris":   "iP95p4xoKVk53GoZ742B", // down-to-earth male
	"Daniel":  "onwK4e9ZLuTAKqWW03F9", // steady British male
	"Will":    "bIHbv24MWmeRgasZH58o", // relaxed, optimistic male
}

// DefaultVoice is used when a script names none.
const DefaultVoice = "Bella"

// AudioStore keeps voice-overs with the draft (public: the presenter route
// fetches the audio by URL).
type AudioStore interface {
	PutAudio(ctx context.Context, tenantID uuid.UUID, versionID, name string, data []byte) (key, url string, err error)
}

// VoiceRequest is one voice-over.
type VoiceRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Name      string // names the file within the version, e.g. "voice" or "scene-2"
	Text      string
	Language  string // en (default) or sw
	Voice     string // a VoiceCatalog name
	Tier      string
}

// WordTiming places a word in the audio, for captions.
type WordTiming struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// VoiceResult is the stored voice-over.
type VoiceResult struct {
	URL        string       `json:"url"`
	Key        string       `json:"key"`
	Bytes      int          `json:"bytes"`
	Seconds    float64      `json:"seconds"`
	Words      []WordTiming `json:"words"`
	Exact      bool         `json:"exact_timings"` // false when the timings are estimated
	Characters int          `json:"characters"`
	Route      string       `json:"route"`
	CostUSD    float64      `json:"cost_usd"`
	Attempts   int          `json:"attempts"`
}

// ElevenLabs API base (tests point it at a fake server).
var elevenBase = "https://api.elevenlabs.io"

// Voices makes voice-overs on the best route with fallbacks.
type Voices struct {
	router *Router
	keys   KeySource
	store  AudioStore
	queue  *queueClient
	http   *http.Client
	logger *zap.Logger

	mu    sync.Mutex
	plans map[string]planEntry // ElevenLabs plan per key
}

type planEntry struct {
	paid bool
	at   time.Time
}

// planTTL: a plan changes when the owner upgrades, rarely.
const planTTL = time.Hour

// NewVoices builds the voice pipeline.
func NewVoices(keys KeySource, store AudioStore, logger *zap.Logger) *Voices {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Voices{router: NewRouter(VoiceRoutes), keys: keys, store: store, queue: newQueueClient(),
		http: &http.Client{Timeout: 90 * time.Second}, logger: logger.Named("media.voices"), plans: map[string]planEntry{}}
}

// ErrVoiceInput is a request the caller must fix.
var ErrVoiceInput = errors.New("media: invalid voice request")

// Make reads the text aloud and stores the audio.
func (v *Voices) Make(ctx context.Context, req VoiceRequest) (*VoiceResult, error) {
	req.Text = strings.TrimSpace(req.Text)
	n := utf8.RuneCountInString(req.Text)
	if n == 0 || n > MaxVoiceChars {
		return nil, fmt.Errorf("%w: text is 1 to %d characters", ErrVoiceInput, MaxVoiceChars)
	}
	if req.Language == "" {
		req.Language = "en"
	}
	if req.Language != "en" && req.Language != "sw" {
		return nil, fmt.Errorf("%w: language must be en or sw", ErrVoiceInput)
	}
	if req.Voice == "" {
		req.Voice = DefaultVoice
	}
	if _, ok := VoiceCatalog[req.Voice]; !ok {
		return nil, fmt.Errorf("%w: unknown voice %q", ErrVoiceInput, req.Voice)
	}
	if req.Tier == "" {
		req.Tier = TierStandard
	}
	if req.Name == "" {
		req.Name = "voice"
	}
	if v.store == nil {
		return nil, fmt.Errorf("media: object storage is not configured")
	}
	routes := v.router.Order(CapVoice, req.Tier, func(rt Route) bool { return v.eligible(ctx, rt) })
	var lastErr error
	for i, rt := range routes {
		if ctx.Err() != nil {
			break
		}
		actx, cancel := context.WithTimeout(ctx, rt.Timeout)
		audio, words, exact, err := v.speak(actx, rt, req)
		cancel()
		if err != nil {
			lastErr = err
			v.router.Failed(rt.Key, err, ctx.Err() != nil)
			v.logger.Warn("voice route failed, trying the next", zap.String("route", rt.Key), zap.Error(err))
			continue
		}
		v.router.Succeeded(rt.Key)
		seconds := 0.0
		if len(words) > 0 {
			seconds = words[len(words)-1].End
		}
		if !exact {
			seconds = mp3Seconds(audio)
			words = estimateWords(req.Text, seconds)
		}
		key, url, err := v.store.PutAudio(ctx, req.TenantID, req.VersionID, req.Name, audio)
		if err != nil {
			return nil, fmt.Errorf("media: store voice: %w", err)
		}
		return &VoiceResult{URL: url, Key: key, Bytes: len(audio), Seconds: seconds, Words: words, Exact: exact,
			Characters: n, Route: rt.Key, CostUSD: rt.PriceUSD * float64(n) / 1000, Attempts: i + 1}, nil
	}
	if lastErr == nil {
		return nil, ErrNoRoute
	}
	return nil, fmt.Errorf("%w: %v", ErrNoRoute, lastErr)
}

// eligible needs the provider's key, and for ElevenLabs direct a paid plan.
func (v *Voices) eligible(ctx context.Context, rt Route) bool {
	key := v.keys(ctx, rt.Provider)
	if key == "" {
		return false
	}
	if rt.Provider == "elevenlabs" {
		return v.paidPlan(ctx, key)
	}
	return true
}

// paidPlan reads the ElevenLabs subscription tier, cached per key. An unknown
// plan counts as free, so a lookup failure never produces non-commercial audio.
func (v *Voices) paidPlan(ctx context.Context, key string) bool {
	v.mu.Lock()
	e, ok := v.plans[key]
	v.mu.Unlock()
	if ok && time.Since(e.at) < planTTL {
		return e.paid
	}
	paid := false
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, elevenBase+"/v1/user/subscription", nil)
	if err == nil {
		req.Header.Set("xi-api-key", key)
		if resp, err := v.http.Do(req); err == nil {
			var sub struct {
				Tier string `json:"tier"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&sub) == nil {
				paid = sub.Tier != "" && sub.Tier != "free"
			}
			resp.Body.Close()
		}
	}
	v.mu.Lock()
	v.plans[key] = planEntry{paid: paid, at: time.Now()}
	v.mu.Unlock()
	return paid
}

// speak runs one route; exact reports whether words came from the provider.
func (v *Voices) speak(ctx context.Context, rt Route, req VoiceRequest) ([]byte, []WordTiming, bool, error) {
	key := v.keys(ctx, rt.Provider)
	switch rt.Key {
	case "elevenlabs/eleven-v4-turbo", "elevenlabs/eleven-v4":
		model := "eleven_v4_turbo"
		if rt.Key == "elevenlabs/eleven-v4" {
			model = "eleven_v4"
		}
		audio, words, err := v.eleven(ctx, key, model, req)
		return audio, words, err == nil, err
	case "fal/elevenlabs-v3":
		var out struct {
			Audio struct {
				URL string `json:"url"`
			} `json:"audio"`
		}
		input := map[string]any{"text": req.Text, "voice": req.Voice, "language_code": req.Language}
		if err := v.queue.run(ctx, falJob(key, "fal-ai/elevenlabs/tts/eleven-v3"), input, &out); err != nil {
			return nil, nil, false, err
		}
		if out.Audio.URL == "" {
			return nil, nil, false, &errJobFailed{provider: "fal", status: "completed", detail: "no audio in the result"}
		}
		audio, err := v.download(ctx, out.Audio.URL)
		return audio, nil, false, err
	}
	return nil, nil, false, fmt.Errorf("media: no builder for route %s", rt.Key)
}

// eleven calls ElevenLabs text to speech with timestamps: the audio and the
// time of every character, grouped here into words.
func (v *Voices) eleven(ctx context.Context, key, model string, req VoiceRequest) ([]byte, []WordTiming, error) {
	body, _ := json.Marshal(map[string]any{"text": req.Text, "model_id": model, "language_code": req.Language})
	url := elevenBase + "/v1/text-to-speech/" + VoiceCatalog[req.Voice] + "/with-timestamps?output_format=mp3_44100_128"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	hr.Header.Set("xi-api-key", key)
	hr.Header.Set("Content-Type", "application/json")
	resp, err := v.http.Do(hr)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &errJobFailed{provider: "elevenlabs", status: fmt.Sprint(resp.StatusCode), detail: truncate(string(data), 300)}
	}
	var out struct {
		Audio     string `json:"audio_base64"`
		Alignment struct {
			Chars  []string  `json:"characters"`
			Starts []float64 `json:"character_start_times_seconds"`
			Ends   []float64 `json:"character_end_times_seconds"`
		} `json:"alignment"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, nil, fmt.Errorf("elevenlabs: unreadable reply: %w", err)
	}
	audio, err := base64.StdEncoding.DecodeString(out.Audio)
	if err != nil || len(audio) == 0 {
		return nil, nil, fmt.Errorf("elevenlabs: no audio in the reply")
	}
	a := out.Alignment
	if len(a.Chars) == 0 || len(a.Starts) != len(a.Chars) || len(a.Ends) != len(a.Chars) {
		return nil, nil, fmt.Errorf("elevenlabs: no timings in the reply")
	}
	return audio, wordsFromChars(a.Chars, a.Starts, a.Ends), nil
}

// wordsFromChars groups character timings into words (runs without spaces).
func wordsFromChars(chars []string, starts, ends []float64) []WordTiming {
	var words []WordTiming
	var cur strings.Builder
	start, end := 0.0, 0.0
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, WordTiming{Text: cur.String(), Start: start, End: end})
			cur.Reset()
		}
	}
	for i, c := range chars {
		if strings.TrimSpace(c) == "" {
			flush()
			continue
		}
		if cur.Len() == 0 {
			start = starts[i]
		}
		cur.WriteString(c)
		end = ends[i]
	}
	flush()
	return words
}

// estimateWords spreads seconds over the words by their length, for a route
// that returns no timings. Captions then drift a little within each line.
func estimateWords(text string, seconds float64) []WordTiming {
	fields := strings.Fields(text)
	total := 0
	for _, f := range fields {
		total += utf8.RuneCountInString(f) + 1
	}
	if total == 0 || seconds <= 0 {
		return nil
	}
	words := make([]WordTiming, 0, len(fields))
	t := 0.0
	for _, f := range fields {
		d := seconds * float64(utf8.RuneCountInString(f)+1) / float64(total)
		words = append(words, WordTiming{Text: f, Start: t, End: t + d})
		t += d
	}
	return words
}

// mp3Seconds estimates an MP3's length from its size. fal's ElevenLabs route
// returns 128 kbit/s MP3; the assembler measures the exact length with ffmpeg.
func mp3Seconds(audio []byte) float64 {
	return float64(len(audio)) * 8 / 128000
}

func (v *Voices) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download voice: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download voice: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("download voice: %v", err)
	}
	return data, nil
}
