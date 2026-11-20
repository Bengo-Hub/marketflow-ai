package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// A video ad is made from an approved script (the team's video_script contract), the
// owner's real photos and footage and, for a presenter ad, the approved storyboard:
//   - every scene with words gets its own voice-over, so a presenter's lips are
//     lip-synced to exactly the words of that scene and the cut lands where its words end;
//   - presenter scenes are the ad's one actor still lip-synced by the presenter routes;
//   - in a presenter ad, product and b-roll scenes are animated by the clip routes, from
//     the owner's photo (product) or the approved storyboard still (b-roll);
//   - in a template ad nothing is generated but the voice: photos get a slow push-in;
//   - captions follow the voice's word timings, and each scene's on-screen caption sits
//     at the top of the safe band.
//
// A scene that did not change since an earlier version is reused from the scene cache
// (its files copied to this version), so a revision pays only for the scenes it changed.

// Script shots.
const (
	ShotProduct   = "product_clip"
	ShotBRoll     = "b_roll"
	ShotFootage   = "owner_footage"
	ShotEndCard   = "end_card"
	ShotPresenter = "presenter"
)

// Ad kinds (the video tiers of the studio).
const (
	AdTemplate         = "template"
	AdPresenter        = "presenter"
	AdPresenterPremium = "presenter_premium"
)

func isPresenterKind(k string) bool { return k == AdPresenter || k == AdPresenterPremium }

// Ad limits. About 15 characters of speech per second keeps a 60 s ad under the
// assembler's limit before any voice is paid for; a presenter scene stays under
// Seedance's 15 s per call.
const (
	MinAdScenes            = 2
	MaxAdVoiceChars        = 900
	MaxPresenterVoiceChars = 200
	endCardTail            = endCardSeconds + 0.3 // the end card starts after the last word
	// sceneGap is the breath after a scene's last word before the cut.
	sceneGap = 0.25
	// charsPerSecond estimates speech length for a quote, before any voice exists.
	charsPerSecond = 14.0
	// adWorkers bounds the provider jobs one ad runs at once.
	adWorkers = 3
)

// ScriptScene is one scene of an approved script.
type ScriptScene struct {
	ID        string  `json:"id"`
	Shot      string  `json:"shot"`
	Asset     int     `json:"asset,omitempty"` // 1-based photo or footage index; 0 picks in order
	Visual    string  `json:"visual,omitempty"`
	Voiceover string  `json:"voiceover"`
	Caption   string  `json:"caption"`
	DurationS float64 `json:"duration_s"` // used only for a scene without voiceover
	// SFX is an optional short sound cue at the scene's start ("soft whoosh"), rendered from
	// the sfx audio level up.
	SFX string `json:"sfx,omitempty"`
	// Speaker is who reads the line in a two-voice ad ("male" or "female"; two_voice.go). It
	// counts only for a delivery allowed two voices; elsewhere the ad has one voice.
	Speaker string `json:"speaker,omitempty"`
}

// SceneCache is one rendered scene kept for later versions of the ad: its voice-over and,
// for a generated scene, its clip, under the hash of everything that shaped them.
type SceneCache struct {
	Hash         string       `json:"hash"`
	SceneID      string       `json:"scene_id"`
	AudioURL     string       `json:"audio_url,omitempty"`
	AudioKey     string       `json:"audio_key,omitempty"`
	AudioSeconds float64      `json:"audio_seconds,omitempty"`
	Words        []WordTiming `json:"words,omitempty"`
	VideoURL     string       `json:"video_url,omitempty"`
	VideoKey     string       `json:"video_key,omitempty"`
	Route        string       `json:"route,omitempty"`
	CostUSD      float64      `json:"cost_usd"`
	Voice        string       `json:"voice,omitempty"`
	// Speaker is the scene's speaker in a two-voice ad ("" in a one-voice ad).
	Speaker string `json:"speaker,omitempty"`
	// VoiceSubstitutedFor names the chosen voice when a premade one read the scene instead.
	VoiceSubstitutedFor string `json:"voice_substituted_for,omitempty"`
	// The scene's sound cue, when one was made, and the cue text it was made from (a cue is
	// reused only for the same text; the cue is not part of the scene's hash, so changing it
	// never re-renders the scene's voice or video).
	SFXText    string  `json:"sfx_text,omitempty"`
	SFXURL     string  `json:"sfx_url,omitempty"`
	SFXKey     string  `json:"sfx_key,omitempty"`
	SFXCostUSD float64 `json:"sfx_cost_usd,omitempty"`
	Reused     bool    `json:"reused,omitempty"`
}

// AdRequest is one video ad.
type AdRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Format    string
	Kind      string // AdTemplate (default), AdPresenter or AdPresenterPremium
	Tier      string
	Voice     string // a voice id or name prefix; "" lets the delivery pick the best voice
	Delivery  string // DeliveryAd (default), DeliveryUGC, DeliveryKids, DeliveryStory, DeliveryInfo
	Prefs     VoicePrefs
	Language  string // en or sw
	Scenes    []ScriptScene
	Photos    []string // the owner's product photos, http(s)
	// PhotoInfo describes Photos by index (the offering's own, or brand-library pictures with
	// their caption, tags and logo flag); frames.go picks each scene's start frame from it.
	PhotoInfo []PhotoInfo
	Footage   []string // the owner's clips, http(s)
	EndCard   Overlay
	LogoURL   string
	// MusicURL is an optional licensed music bed, mixed under the voice.
	MusicURL string
	// Audio is the soundtrack's level (AudioVoice, AudioSFX, AudioTrack, AudioScore), decided
	// from the tenant's plan features (Ads.AudioLevel); "" is voice.
	Audio string
	// Mood describes the ad's feel for generated music ("warm, hopeful"); "" uses the delivery.
	Mood string
	// ActorURL is the approved actor still every presenter scene is lip-synced from, and
	// Seed the seed recorded with it.
	ActorURL string
	Seed     int64
	// Stills are the approved storyboard frames by scene id (b-roll scenes animate them).
	Stills map[string]string
	// Cache holds earlier versions' scenes by hash.
	Cache map[string]SceneCache
	// Music is the generated music of the version this one was revised from: reused when the
	// music prompt is the same and the track still fits the length, so a revision does not pay
	// for the same music twice (WP1).
	Music *MusicCache
	// NoAILabel leaves out the on-screen AI note (aiNote) because the owner turned the AI label
	// off (owner 2026-11-14). The zero value keeps the note. The script's honesty rules stay: it
	// never claims an AI presenter is a real customer or a real person.
	NoAILabel bool
	// AutoPremium starts every paid scene on premium routes (the platform owner's tenant under the
	// routing policy); a scene falls back to standard when no premium route can take it or the
	// all-premium quote is over PremiumBudgetUSD (0: the per-video ceiling).
	AutoPremium      bool
	PremiumBudgetUSD float64
	// MaxVideoUSD is the platform's per-video provider-cost ceiling (max_video_budget_usd); 0
	// reads it from the routing policy in force.
	MaxVideoUSD float64
}

// MusicCache is one version's generated music, kept for its revisions.
type MusicCache struct {
	Prompt  string  `json:"prompt"`
	URL     string  `json:"url"`
	Key     string  `json:"key"`
	Seconds float64 `json:"seconds"`
	Route   string  `json:"route,omitempty"`
	CostUSD float64 `json:"cost_usd"`
	Reused  bool    `json:"reused,omitempty"`
}

// musicSlack is how much longer than the ad a cached track may be and still be reused: the
// mix ends the music with the ad, so a slightly longer track only loses its tail.
func musicSlack(seconds float64) float64 { return max(2, seconds*0.15) }

// cachedMusic returns the earlier version's music when it was made from the same prompt, is
// at least as long as the ad (and not much longer), and its file still exists.
func (a *Ads) cachedMusic(ctx context.Context, req AdRequest, prompt string, seconds float64) *MusicCache {
	c := req.Music
	if c == nil || c.Key == "" || c.URL == "" || c.Prompt != prompt || a.files == nil {
		return nil
	}
	want := soundSeconds(CapMusic, seconds)
	if c.Seconds < want || c.Seconds > want+musicSlack(want) {
		return nil
	}
	if !a.files.Exists(ctx, c.Key) {
		return nil
	}
	return c
}

// Ads makes video ads.
type Ads struct {
	voices     *Voices
	asm        *Assembler
	presenters *Presenters
	clips      *Clips
	files      SceneFiles
	sounds     *Sounds
	audio      AudioConfig
	logger     *zap.Logger
	// frames fits start frames to the ad's shape before animation (WithFrames).
	frames *frameFitter
}

// WithFrames stores start frames fitted to the ad's format; without it a frame is animated as it is.
func (a *Ads) WithFrames(store Store, posters *Posters) *Ads {
	a.frames = &frameFitter{store: store, posts: posters}
	return a
}

// NewAds builds the ad pipeline with the default audio settings; sound cues and music use the
// voices' keys and store.
func NewAds(voices *Voices, asm *Assembler) *Ads {
	a := &Ads{voices: voices, asm: asm, audio: DefaultAudioConfig(), logger: zap.NewNop()}
	if voices != nil {
		a.sounds = NewSounds(voices.keys, voices.store, voices, nil) // WithLogger names its logger
	}
	return a
}

// WithAudio sets the audio settings (AudioConfigFromEnv): the ladder, the voice clean up, the
// cue and music mix, the music model and the voice delivery overrides.
func (a *Ads) WithAudio(cfg AudioConfig) *Ads {
	// A clean up chain this ffmpeg cannot run (a filter missing from the build, a mistyped
	// setting) would fail every ad, so it is tried once here and switched off if it fails.
	if cfg.VoiceFilter != "" && a.asm.Ready() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := a.asm.TryAudioFilter(ctx, cfg.VoiceFilter)
		cancel()
		if err != nil {
			a.logger.Warn("voice clean up filter does not run on this ffmpeg; it is off until fixed",
				zap.String("filter", cfg.VoiceFilter), zap.Error(err))
			cfg.VoiceFilter = ""
		}
	}
	a.audio = cfg
	if a.sounds != nil {
		a.sounds.WithMusicModel(cfg.MusicModel)
	}
	if a.voices != nil {
		a.voices.WithDeliveryOverrides(cfg.Deliveries).WithSpeakerDefaults(cfg.SpeakerVoices)
	}
	return a
}

// AudioLevel is the soundtrack level a plan's features grant (AudioConfig.LevelFor).
func (a *Ads) AudioLevel(features map[string]bool) string { return a.audio.LevelFor(features) }

// WithPresenters attaches the presenter pipeline.
func (a *Ads) WithPresenters(p *Presenters) *Ads { a.presenters = p; return a }

// WithClips attaches the clip routes and the scene store, for presenter ads' product and
// b-roll scenes and the scene cache.
func (a *Ads) WithClips(c *Clips, files SceneFiles) *Ads { a.clips, a.files = c, files; return a }

// WithLogger names the pipeline's logger.
func (a *Ads) WithLogger(l *zap.Logger) *Ads {
	if l != nil {
		a.logger = l.Named("media.ads")
		if a.sounds != nil {
			a.sounds.withLogger(l)
		}
	}
	return a
}

// Presenters returns the presenter pipeline (nil when not wired).
func (a *Ads) Presenters() *Presenters { return a.presenters }

// Voices lists the voices an ad can use.
func (a *Ads) Voices() *Voices { return a.voices }

// Ready reports whether ads can be made here.
func (a *Ads) Ready() bool { return a != nil && a.voices != nil && a.asm.Ready() }

// PresenterReady reports whether presenter ads can be made here.
func (a *Ads) PresenterReady() bool {
	return a.Ready() && a.presenters != nil && a.clips != nil && a.files != nil
}

// Validate refuses a script this ad cannot render, before anything is paid.
func (a *Ads) Validate(req AdRequest) error {
	if _, ok := Formats[req.Format]; !ok {
		return &UserError{Msg: "format must be " + FormatsText}
	}
	if _, ok := Deliveries[req.Delivery]; req.Delivery != "" && !ok {
		return &UserError{Msg: "delivery must be ad, ugc, kids, story or info"}
	}
	if req.MusicURL != "" && !strings.HasPrefix(req.MusicURL, "https://") {
		return &UserError{Msg: "the music link must be https"}
	}
	switch req.Kind {
	case "", AdTemplate:
	case AdPresenter, AdPresenterPremium:
		if !a.PresenterReady() {
			return &UserError{Msg: "presenter videos are not available on this server yet"}
		}
		if !httpLink(req.ActorURL) {
			return &UserError{Msg: "a presenter ad needs its actor: make and approve the storyboard first"}
		}
	default:
		return &UserError{Msg: "the video type must be template, presenter or presenter_premium"}
	}
	if len(req.Scenes) < MinAdScenes || len(req.Scenes) > MaxAdScenes {
		return &UserError{Msg: fmt.Sprintf("a video ad has %d to %d scenes", MinAdScenes, MaxAdScenes)}
	}
	for _, list := range [][]string{req.Photos, req.Footage} {
		for _, u := range list {
			if !httpLink(u) {
				return &UserError{Msg: "photos and footage must be http(s) links"}
			}
		}
	}
	for _, u := range req.Stills {
		if u != "" && !httpLink(u) {
			return &UserError{Msg: "storyboard stills must be http(s) links"}
		}
	}
	chars := 0
	frames := newFrameChooser(req.Photos, req.Footage, req.PhotoInfo)
	for i, s := range req.Scenes {
		words := utf8.RuneCountInString(strings.TrimSpace(s.Voiceover))
		switch s.Shot {
		case ShotProduct, ShotEndCard, ShotBRoll:
			if s.Asset > len(req.Photos) {
				return &UserError{Msg: fmt.Sprintf("scene %d uses photo %d, but there are %d", i+1, s.Asset, len(req.Photos))}
			}
			if f := frames.choose(s); f.Source == FrameGenerated && req.Stills[s.ID] == "" {
				return &UserError{Msg: fmt.Sprintf("scene %d needs a storyboard still: make the storyboard first", i+1)}
			}
		case ShotFootage:
			if len(req.Footage) == 0 {
				return &UserError{Msg: fmt.Sprintf("scene %d uses your footage: add a video clip", i+1)}
			}
			if s.Asset > len(req.Footage) {
				return &UserError{Msg: fmt.Sprintf("scene %d uses clip %d, but there are %d", i+1, s.Asset, len(req.Footage))}
			}
		case ShotPresenter:
			if !isPresenterKind(req.Kind) {
				return &UserError{Msg: fmt.Sprintf("scene %d needs an AI presenter: choose the presenter video type", i+1)}
			}
			if words == 0 {
				return &UserError{Msg: fmt.Sprintf("scene %d: a presenter scene needs words to say", i+1)}
			}
			if words > MaxPresenterVoiceChars {
				return &UserError{Msg: fmt.Sprintf("scene %d: a presenter scene says at most %d characters (about 14 seconds)", i+1, MaxPresenterVoiceChars)}
			}
		default:
			return &UserError{Msg: fmt.Sprintf("scene %d has an unknown shot %q", i+1, s.Shot)}
		}
		if s.Asset < 0 {
			return &UserError{Msg: fmt.Sprintf("scene %d has an invalid asset number", i+1)}
		}
		if utf8.RuneCountInString(strings.TrimSpace(s.SFX)) > maxSFXChars {
			return &UserError{Msg: fmt.Sprintf("scene %d: a sound cue is at most %d characters", i+1, maxSFXChars)}
		}
		if sp := strings.TrimSpace(s.Speaker); sp != "" && normalSpeaker(sp) == "" {
			return &UserError{Msg: fmt.Sprintf("scene %d: the speaker is male, female or empty", i+1)}
		}
		chars += words
	}
	if chars == 0 || chars > MaxAdVoiceChars {
		return &UserError{Msg: fmt.Sprintf("the voice-over is 1 to %d characters (about 60 seconds)", MaxAdVoiceChars)}
	}
	return nil
}

func httpLink(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// Scene work kinds.
const (
	WorkPresenter = "presenter" // lip-synced actor (paid)
	WorkClip      = "clip"      // animated still (paid)
	WorkStill     = "still"     // a photo with a push-in (free)
	WorkFootage   = "footage"   // the owner's clip (free)
)

// ScenePlan is what one scene will cost and why.
type ScenePlan struct {
	SceneID   string  `json:"scene_id"`
	Shot      string  `json:"shot"`
	Work      string  `json:"work"`
	Hash      string  `json:"hash"`
	Cached    bool    `json:"cached"`
	Seconds   float64 `json:"seconds"` // estimated from the words until the voice exists
	Chars     int     `json:"chars"`
	Route     string  `json:"route,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	Estimated bool    `json:"estimated"`         // the price came from the provider's own estimate
	SFXUSD    float64 `json:"sfx_usd,omitempty"` // the scene's sound cue still to make
	// The generation plan the owner approves before the render (owner, 2026-11-19): where the
	// scene's first frame comes from (Frame: actor, photo, generated or footage; FrameURL the
	// photo or storyboard still; Photo and Origin which photo), the exact prompt and negative
	// prompt the route is sent, and the prompt a generated still was drawn from.
	Frame          string `json:"frame"`
	FrameURL       string `json:"frame_url,omitempty"`
	Photo          int    `json:"photo,omitempty"`
	Origin         string `json:"origin,omitempty"`
	Prompt         string `json:"prompt,omitempty"`
	NegativePrompt string `json:"negative_prompt,omitempty"`
	StillPrompt    string `json:"still_prompt,omitempty"`
	// AvoidText marks a scene showing screens or signs: its clip goes to a route that takes a
	// negative prompt when the registry has one.
	AvoidText bool `json:"avoid_text,omitempty"`
	// Tier is the scene's own route tier when it differs by scene (the platform owner's premium
	// default, with per-scene fallback); "" is the request's tier.
	Tier    string `json:"tier,omitempty"`
	source  string
	speaker string
}

// AdPlan prices an ad scene by scene before anything is paid.
type AdPlan struct {
	Kind       string      `json:"kind"`
	Audio      string      `json:"audio"` // the soundtrack level
	Scenes     []ScenePlan `json:"scenes"`
	VoiceChars int         `json:"voice_chars"` // characters still to be voiced
	VoiceUSD   float64     `json:"voice_usd"`
	SFXUSD     float64     `json:"sfx_usd"`   // sound cues still to make
	MusicUSD   float64     `json:"music_usd"` // generated music (score level)
	MusicRoute string      `json:"music_route,omitempty"`
	CostUSD    float64     `json:"cost_usd"` // provider cost of everything still to make
	Cached     int         `json:"cached_scenes"`
}

// WithoutAudioExtras is the plan without its sound cues and music: what is charged before the
// render, since cues and music that fail are skipped and never charged.
func (p AdPlan) WithoutAudioExtras() AdPlan {
	p.CostUSD -= p.SFXUSD + p.MusicUSD
	p.SFXUSD, p.MusicUSD = 0, 0
	return p
}

// voiceUSDPer1000 is the dearest voice route's price, for the quote.
const voiceUSDPer1000 = 0.10

// Plan resolves each scene's source, hashes it, checks the cache and prices the rest:
// presenter scenes through Presenters.Quote and clip scenes through Clips.Quote (each
// reading the provider's cached estimate, with the route price as the fallback). It
// fails only when a paid scene has no route at all.
func (a *Ads) Plan(ctx context.Context, req AdRequest) (AdPlan, error) {
	plan := AdPlan{Kind: kindOf(req.Kind), Audio: audioLevelOf(req.Audio)}
	two, turns := a.twoVoice(req), speakerTurns(req.Scenes)
	frames := newFrameChooser(req.Photos, req.Footage, req.PhotoInfo)
	adSeconds := endCardTail
	for i, s := range req.Scenes {
		chars := utf8.RuneCountInString(strings.TrimSpace(stripTags(s.Voiceover)))
		sp := ScenePlan{SceneID: s.ID, Shot: s.Shot, Chars: chars, Seconds: estimateSeconds(chars, s.DurationS)}
		f := frames.choose(s)
		sp.Frame, sp.Photo, sp.Origin = f.Source, f.Photo, f.Origin
		switch f.Source {
		case FrameActor:
			sp.Work, sp.source = WorkPresenter, req.ActorURL
		case FrameFootage:
			sp.Work, sp.source = WorkFootage, f.URL
		case FrameGenerated:
			sp.Work, sp.source = WorkStill, req.Stills[s.ID]
			sp.StillPrompt = StillPrompt(req.Format, req.Delivery, sceneVisual(s))
		default:
			sp.Work, sp.source = WorkStill, f.URL
		}
		sp.FrameURL = sp.source
		if isPresenterKind(req.Kind) && (s.Shot == ShotProduct || s.Shot == ShotBRoll) {
			sp.Work = WorkClip
			sp.AvoidText = showsWriting(s.Visual)
		}
		speaker := ""
		if two {
			speaker = turns[i]
		}
		paid := sp.Work == WorkPresenter || sp.Work == WorkClip
		if req.AutoPremium && paid {
			sp.Tier = TierPremium // the platform owner's tenant starts every paid scene on premium
		}
		if err := a.priceScene(ctx, req, s, &sp, speaker); err != nil && sp.Tier == TierPremium && req.AutoPremium {
			// No premium route can take the scene (offline, unpaid or low): it falls back to standard.
			sp.Tier = TierStandard
			if err := a.priceScene(ctx, req, s, &sp, speaker); err != nil {
				return plan, err
			}
		} else if err != nil {
			return plan, err
		}
		if sp.Cached {
			plan.Cached++
		} else {
			plan.VoiceChars += chars
			plan.CostUSD += sp.CostUSD
		}
		// A cue is made for a new scene, or for a reused one whose cue is missing.
		if cue := sceneSFX(req, s); cue != "" && (!sp.Cached || !a.cachedCue(ctx, req.Cache[sp.Hash], cue)) {
			sp.SFXUSD, _ = a.sounds.Quote(ctx, CapSFX, tierOf(req), a.audio.SFXSeconds)
			plan.SFXUSD += sp.SFXUSD
		}
		adSeconds += sp.Seconds
		plan.Scenes = append(plan.Scenes, sp)
	}
	// Music is priced before the budget fit so the fit counts it (it was left out before
	// 2026-11-20, so an "in budget" plan could still run over by the music bed).
	if plan.Audio == AudioScore {
		if c := a.cachedMusic(ctx, req, musicPrompt(req.Mood, req.Delivery, a.audio.MusicStyle), adSeconds); c != nil {
			plan.MusicRoute = c.Route // the earlier version's music is reused at no cost
		} else {
			plan.MusicUSD, plan.MusicRoute = a.sounds.Quote(ctx, CapMusic, tierOf(req), adSeconds)
		}
	}
	ceiling := req.MaxVideoUSD
	if ceiling <= 0 {
		ceiling = Policy(ctx).MaxVideoUSD
	}
	if req.AutoPremium {
		if req.PremiumBudgetUSD <= 0 || req.PremiumBudgetUSD > ceiling {
			req.PremiumBudgetUSD = ceiling
		}
		a.fitPremiumBudget(ctx, req, &plan)
	}
	plan.VoiceUSD = float64(plan.VoiceChars) * voiceUSDPer1000 / 1000
	plan.CostUSD += plan.VoiceUSD + plan.SFXUSD + plan.MusicUSD
	// No video ad costs more than the platform's max_video_budget_usd at provider prices, on any
	// tier or tenant. A plan still over it is refused with what to change, before any spend.
	if ceiling > 0 && plan.CostUSD > ceiling+0.005 {
		return plan, &UserError{Msg: fmt.Sprintf("this video would cost about USD %.2f to make, over the USD %.2f limit per video; make it shorter, use fewer scenes or a standard quality", plan.CostUSD, ceiling)}
	}
	return plan, nil
}

// priceScene hashes a scene at its tier, checks the cache and, for a scene still to make, quotes
// it on the route a render would use first and fills in the exact prompts that route is sent.
func (a *Ads) priceScene(ctx context.Context, req AdRequest, s ScriptScene, sp *ScenePlan, speaker string) error {
	sp.speaker = speaker
	sp.Hash = sceneHashFor(req, s, *sp, a.deliveryPrint(req.Delivery), speaker)
	sp.Cached, sp.Route, sp.CostUSD, sp.Estimated, sp.Prompt, sp.NegativePrompt = false, "", 0, false, "", ""
	if c, ok := req.Cache[sp.Hash]; ok && a.cacheUsable(ctx, req, c, *sp) {
		sp.Cached = true // a cached scene is not sent again, so it shows no prompt
		return nil
	}
	tier := sceneTier(req, *sp)
	switch sp.Work {
	case WorkPresenter:
		usd, route, est := a.presenters.Quote(ctx, tier, sp.Seconds)
		if route == "" {
			return fmt.Errorf("%w: no presenter route is available", ErrNoRoute)
		}
		sp.Route, sp.CostUSD, sp.Estimated = route, usd, est
		sp.Prompt, sp.NegativePrompt = presenterPromptFor(sp.Route, s.Visual)
	case WorkClip:
		usd, route, est := a.clips.QuoteFor(ctx, tier, clipSeconds(sp.Seconds), sp.AvoidText)
		if route == "" {
			return fmt.Errorf("%w: no clip route is available", ErrNoRoute)
		}
		sp.Route, sp.CostUSD, sp.Estimated = route, usd, est
		sp.Prompt = clipPrompt(s.Visual)
		if rt, ok := clipRoute(sp.Route); ok && rt.NegativePrompt {
			sp.NegativePrompt = clipNegativePrompt
		}
	}
	return nil
}

// fitPremiumBudget moves an all-premium ad's dearest premium scenes to standard, one at a time,
// until its paid scenes, voice and cues fit the budget (or none is left to move).
func (a *Ads) fitPremiumBudget(ctx context.Context, req AdRequest, plan *AdPlan) {
	total := func() float64 {
		return plan.CostUSD + float64(plan.VoiceChars)*voiceUSDPer1000/1000 + plan.SFXUSD + plan.MusicUSD
	}
	tried := map[int]bool{}
	for total() > req.PremiumBudgetUSD {
		pick := -1
		for i, sp := range plan.Scenes {
			if !sp.Cached && sp.Tier == TierPremium && !tried[i] && (pick < 0 || sp.CostUSD > plan.Scenes[pick].CostUSD) {
				pick = i
			}
		}
		if pick < 0 {
			return
		}
		tried[pick] = true
		sp := plan.Scenes[pick]
		was := sp
		sp.Tier = TierStandard
		if err := a.priceScene(ctx, req, req.Scenes[pick], &sp, was.speaker); err != nil {
			continue
		}
		plan.CostUSD += sp.CostUSD - was.CostUSD
		if sp.Cached {
			plan.Cached++
			plan.VoiceChars -= sp.Chars
		}
		plan.Scenes[pick] = sp
	}
}

// sceneTier is the route tier of one scene: its own (an exempt tenant's per-scene choice), else
// the request's.
func sceneTier(req AdRequest, sp ScenePlan) string {
	if sp.Tier != "" {
		return sp.Tier
	}
	return tierOf(req)
}

// sceneSFX is the scene's sound cue when the request's level renders cues, else "".
func sceneSFX(req AdRequest, s ScriptScene) string {
	if !audioAtLeast(req.Audio, AudioSFX) {
		return ""
	}
	return strings.TrimSpace(s.SFX)
}

// stableSource is a scene source without its query: brand images arrive as presigned links
// whose signature changes on every call, and the same picture must keep the same hash.
func stableSource(src string) string {
	if i := strings.IndexByte(src, '?'); i >= 0 {
		return src[:i]
	}
	return src
}

// sceneCue is the cue the request renders for the scene with this id ("" when none).
func sceneCue(req AdRequest, sceneID string) string {
	for _, s := range req.Scenes {
		if s.ID == sceneID {
			return sceneSFX(req, s)
		}
	}
	return ""
}

// cachedCue reports whether a cached scene holds a cue made from this text whose file still
// exists.
func (a *Ads) cachedCue(ctx context.Context, c SceneCache, cue string) bool {
	return c.SFXKey != "" && c.SFXText == cue && a.files != nil && a.files.Exists(ctx, c.SFXKey)
}

// deliveryPrint fingerprints the voice settings a delivery resolves to (with overrides), so
// a settings change makes new voices instead of reusing ones read the old way.
func (a *Ads) deliveryPrint(key string) string {
	if a.voices == nil {
		return ""
	}
	b, _ := json.Marshal(a.voices.Delivery(key))
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func kindOf(k string) string {
	if k == "" {
		return AdTemplate
	}
	return k
}

// tierOf is the route tier: the premium presenter ad pays for premium routes.
func tierOf(req AdRequest) string {
	if req.Kind == AdPresenterPremium || req.Tier == TierPremium {
		return TierPremium
	}
	return TierStandard
}

// estimateSeconds is a scene's length before its voice exists.
func estimateSeconds(chars int, scripted float64) float64 {
	if chars == 0 {
		return clampSeconds(scripted)
	}
	return max(float64(chars)/charsPerSecond+sceneGap, minSceneSeconds)
}

// clipSeconds is the clip length asked for a scene: whole seconds, 4 to 10 (a longer
// scene holds the clip's last frame).
func clipSeconds(seconds float64) int {
	return min(max(int(math.Ceil(seconds-1e-9)), MinClipSeconds), MaxClipSeconds)
}

// sceneHash names everything that shapes a scene's files: change any of it and the scene
// is made again. The end card and other scenes are not part of it. The audio level and the
// cue count only from the sfx level up, so voice-only scenes keep the hashes they had.
// The audio level and the sound cue are left out on purpose: they are mixed in at assembly,
// so a plan change or a new cue never re-renders (and re-charges) the scene's voice or video.
// deliveryPrint is the resolved voice settings' fingerprint, so retuned settings make new
// voices.
func sceneHash(req AdRequest, s ScriptScene, sp ScenePlan, deliveryPrint string) string {
	return sceneHashFor(req, s, sp, deliveryPrint, "")
}

// sceneHashFor is sceneHash with the scene's speaker in a two-voice ad; the speaker is left
// out of one-voice scenes, so their hashes stay as they were.
func sceneHashFor(req AdRequest, s ScriptScene, sp ScenePlan, deliveryPrint, speaker string) string {
	parts := map[string]any{
		"kind": kindOf(req.Kind), "tier": sceneTier(req, sp), "shot": s.Shot, "work": sp.Work, "source": stableSource(sp.source),
		"visual": strings.TrimSpace(s.Visual), "voiceover": strings.TrimSpace(s.Voiceover), "duration": s.DurationS,
		"voice": req.Voice, "delivery": req.Delivery, "language": req.Language,
		"gender": req.Prefs.Gender, "accent": req.Prefs.Accent, "seed": req.Seed,
	}
	if deliveryPrint != "" {
		parts["delivery_settings"] = deliveryPrint
	}
	if speaker != "" {
		parts["speaker"] = speaker
	}
	b, _ := json.Marshal(parts) // map keys are sorted, so the hash is stable
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// cacheUsable checks that a cached scene has every file this scene needs and that the
// files still exist (a purged draft takes its files with it).
func (a *Ads) cacheUsable(ctx context.Context, req AdRequest, c SceneCache, sp ScenePlan) bool {
	if a.files == nil {
		return false
	}
	if sp.Chars > 0 && (c.AudioKey == "" || !a.files.Exists(ctx, c.AudioKey)) {
		return false
	}
	if (sp.Work == WorkPresenter || sp.Work == WorkClip) && (c.VideoKey == "" || !a.files.Exists(ctx, c.VideoKey)) {
		return false
	}
	return true
}

// sceneOut is one scene's files once made or reused.
type sceneOut struct {
	cache   SceneCache
	seconds float64
	scene   AdScene
}

// Make voices each scene, makes or reuses its visual and assembles the ad.
func (a *Ads) Make(ctx context.Context, req AdRequest) (*MediaResult, error) {
	if !a.Ready() {
		return nil, ErrNoFFmpeg
	}
	if err := a.Validate(req); err != nil {
		return nil, err
	}
	plan, err := a.Plan(ctx, req)
	if err != nil {
		return nil, err
	}
	outs := make([]sceneOut, len(req.Scenes))
	// A two-voice ad (UGC with a man and a woman taking turns) reads each scene in its
	// speaker's voice; every other ad has one voice.
	two, turns := a.twoVoice(req), speakerTurns(req.Scenes)

	// 1. Reused scenes: their files are copied to this version, so purging the old
	// version leaves this one whole.
	voice := req.Voice
	seeded := map[string]string{} // a two-voice ad's voices from reused scenes, by speaker
	for i, sp := range plan.Scenes {
		if !sp.Cached {
			continue
		}
		c, err := a.reuse(ctx, req, req.Cache[sp.Hash], sp)
		if err != nil {
			a.logger.Info("cached scene not reused; making it again", zap.String("scene", sp.SceneID), zap.Error(err))
			plan.Scenes[i].Cached = false
			continue
		}
		outs[i].cache = c
		switch {
		case two && turns[i] != "":
			if seeded[turns[i]] == "" && c.Voice != "" {
				seeded[turns[i]] = c.Voice // the same voice as this speaker's reused scenes
			}
		case voice == "" && c.Voice != "":
			voice = c.Voice // the same voice as the reused scenes
		}
	}

	// 2. Voices for the rest. A two-voice ad casts one voice per speaker first; when a second
	// voice cannot be found the ad falls back to one voice and says why. In a one-voice ad the
	// first scene picks the voice and every other scene uses it.
	cast, twoNote := map[string]string{}, ""
	if two {
		cast, twoNote = a.castVoices(ctx, req, distinctSpeakers(turns), seeded)
		if twoNote != "" {
			two = false
			if voice == "" {
				voice = seeded[distinctSpeakers(turns)[0]]
			}
			a.logger.Warn("two-voice ad read by one voice", zap.String("version_id", req.VersionID), zap.String("reason", twoNote))
		}
	}
	todo := []int{}
	for i, sp := range plan.Scenes {
		if !sp.Cached && sp.Chars > 0 {
			todo = append(todo, i)
		}
	}
	if two {
		if err := a.parallel(ctx, todo, func(i int) error {
			return a.voiceScene(ctx, req, plan.Scenes[i], req.Scenes[i], cast[turns[i]], &outs[i])
		}); err != nil {
			return nil, err
		}
	} else {
		if len(todo) > 0 && voice == "" {
			first := todo[0]
			if err := a.voiceScene(ctx, req, plan.Scenes[first], req.Scenes[first], "", &outs[first]); err != nil {
				return nil, err
			}
			voice = outs[first].cache.Voice
			todo = todo[1:]
		}
		if err := a.parallel(ctx, todo, func(i int) error {
			return a.voiceScene(ctx, req, plan.Scenes[i], req.Scenes[i], voice, &outs[i])
		}); err != nil {
			return nil, err
		}
	}
	for i := range outs {
		outs[i].cache.Speaker = ""
		if two {
			outs[i].cache.Speaker = turns[i]
		}
	}

	// 3. Scene lengths from the voices, then the paid visuals.
	for i, s := range req.Scenes {
		sec := clampSeconds(s.DurationS)
		if c := outs[i].cache; c.AudioSeconds > 0 {
			sec = max(c.AudioSeconds+sceneGap, minSceneSeconds)
		}
		outs[i].seconds = sec
	}
	outs[len(outs)-1].seconds += endCardTail
	// The voices set the length; an ad over the limit stops here, before any paid scene.
	total := 0.0
	for _, o := range outs {
		total += o.seconds
	}
	if total > MaxAdSeconds {
		return nil, &UserError{Msg: fmt.Sprintf("the voice-over runs %.0f seconds with the end card; shorten the script to fit %d", total, MaxAdSeconds)}
	}
	paid := []int{}
	for i, sp := range plan.Scenes {
		if !sp.Cached && (sp.Work == WorkPresenter || sp.Work == WorkClip) {
			paid = append(paid, i)
		}
	}
	dir, err := os.MkdirTemp("", "adscenes-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// The sound cues and the music are made while the paid scenes render; they never fail
	// the ad.
	snd := a.startSounds(ctx, req, plan, outs, total)
	if err := a.parallel(ctx, paid, func(i int) error {
		return a.visualScene(ctx, req, plan.Scenes[i], req.Scenes[i], dir, &outs[i])
	}); err != nil {
		snd.wait()
		return nil, err
	}
	snd.wait()
	extra := 0.0 // what the cues and the music made here cost
	for i, r := range snd.cues {
		if r != nil {
			outs[i].cache.SFXURL, outs[i].cache.SFXKey, outs[i].cache.SFXCostUSD = r.URL, r.Key, r.CostUSD
			outs[i].cache.SFXText = sceneSFX(req, req.Scenes[i])
			extra += r.CostUSD
		}
	}
	level := audioLevelOf(req.Audio)
	musicURL, musicRoute := req.MusicURL, ""
	if snd.music != nil {
		musicURL, musicRoute = snd.music.URL, snd.music.Route
		extra += snd.music.CostUSD
	}
	mix := AudioMix{VoiceFilter: a.audio.VoiceFilter, SFXGain: a.audio.SFXGain, VoiceMatchLUFS: a.audio.VoiceMatchLUFS}
	if musicURL != "" {
		// Any music, generated or the owner's track, is ducked under the voice (2026-11-19 audit:
		// a flat bed under the speech read as an echo).
		d := a.audio.Ducking()
		mix.Duck = &d
	}

	// 4. Assemble: each scene's own audio and the words shifted to its start.
	scenes := make([]AdScene, len(req.Scenes))
	var words []WordTiming
	var sceneCaps []CaptionLine
	t := 0.0
	cost := 0.0
	for i, s := range req.Scenes {
		o := outs[i]
		sp := plan.Scenes[i]
		// Every scene is fitted, never cropped (owner, 2026-11-19): clips made from frames
		// already in the ad's shape fill it exactly, and anything else keeps all its content.
		sc := AdScene{Seconds: o.seconds, AudioURL: o.cache.AudioURL, Speaker: o.cache.Speaker, Fit: true}
		switch {
		case o.cache.VideoURL != "":
			sc.VideoURL = o.cache.VideoURL
		case sp.Work == WorkFootage:
			sc.VideoURL = sp.source
		default:
			sc.ImageURL = sp.source
		}
		scenes[i] = sc
		for _, w := range o.cache.Words {
			words = append(words, WordTiming{Text: w.Text, Start: t + w.Start, End: t + w.End})
		}
		if c := strings.TrimSpace(s.Caption); c != "" {
			sceneCaps = append(sceneCaps, CaptionLine{Text: c, Start: t, End: t + o.seconds})
		}
		if o.cache.SFXURL != "" && sceneSFX(req, s) != "" {
			mix.SFX = append(mix.SFX, SFXCue{URL: o.cache.SFXURL, At: t})
		}
		if !o.cache.Reused {
			cost += o.cache.CostUSD
		}
		t += o.seconds
	}
	card := req.EndCard
	res, err := a.asm.Make(ctx, AssembleRequest{TenantID: req.TenantID, VersionID: req.VersionID, Format: req.Format,
		Scenes: scenes, Words: words, Captions: true, SceneCaptions: sceneCaps, EndCard: &card, LogoURL: req.LogoURL,
		MusicURL: musicURL, Mix: mix, AINote: adAINote(req)})
	if err != nil {
		return nil, err
	}
	res.CostUSD += cost + extra
	res.Voice = voice
	res.Route = routeSummary(outs)
	res.AudioLevel, res.AudioUSD, res.MusicRoute = level, extra, musicRoute
	res.Music = snd.musicCache
	for _, o := range outs {
		res.Scenes = append(res.Scenes, o.cache)
		if res.VoiceSubstitution == "" && o.cache.VoiceSubstitutedFor != "" {
			res.VoiceSubstitution = o.cache.VoiceSubstitutedFor
		}
	}
	res.TwoVoiceNote = twoNote
	if two {
		// The voices by speaker as they read the ad, and the order they speak in.
		res.Voices = map[string]string{}
		for _, o := range outs {
			if o.cache.Speaker != "" && o.cache.Voice != "" && res.Voices[o.cache.Speaker] == "" {
				res.Voices[o.cache.Speaker] = o.cache.Voice
			}
		}
		res.Voice = res.Voices[distinctSpeakers(turns)[0]]
		res.VoiceTimeline = voiceTimeline(scenes)
	}
	return res, nil
}

// adSounds are the cues and the music being made for one ad.
type adSounds struct {
	wg    sync.WaitGroup
	cues  []*SoundResult // by scene, nil where none was made
	music *SoundResult
	// musicCache is the music as kept for later versions (made here or reused).
	musicCache *MusicCache
}

func (s *adSounds) wait() { s.wg.Wait() }

// startSounds makes, in the background, each scene's missing cue (sfx level up) and the
// music (score level). A failure is logged and skipped: the ad goes on without that part,
// and it is not charged.
func (a *Ads) startSounds(ctx context.Context, req AdRequest, plan AdPlan, outs []sceneOut, total float64) *adSounds {
	snd := &adSounds{cues: make([]*SoundResult, len(outs))}
	if a.sounds == nil {
		return snd
	}
	var todo []int
	lengths := make([]float64, len(outs)) // read here: the paid scenes write outs meanwhile
	for i, s := range req.Scenes {
		lengths[i] = outs[i].seconds
		if sceneSFX(req, s) != "" && outs[i].cache.SFXURL == "" {
			todo = append(todo, i)
		}
	}
	if len(todo) > 0 {
		snd.wg.Add(1)
		go func() {
			defer snd.wg.Done()
			_ = a.parallel(ctx, todo, func(i int) error {
				sp := plan.Scenes[i]
				r, err := a.sounds.Effect(ctx, SoundRequest{TenantID: req.TenantID, VersionID: req.VersionID, Name: sceneName(sp) + "-sfx",
					Prompt: sceneSFX(req, req.Scenes[i]), Seconds: min(a.audio.SFXSeconds, lengths[i]), Tier: tierOf(req)})
				if err != nil {
					a.logger.Warn("sound cue skipped", zap.String("scene", sp.SceneID), zap.Error(err))
					return nil
				}
				snd.cues[i] = r
				return nil
			})
		}()
	}
	if audioLevelOf(req.Audio) == AudioScore {
		prompt := musicPrompt(req.Mood, req.Delivery, a.audio.MusicStyle)
		if c := a.cachedMusic(ctx, req, prompt, total); c != nil {
			// The same music as the version this one was revised from: no new track, no cost.
			snd.music = &SoundResult{URL: c.URL, Key: c.Key, Route: c.Route}
			kept := *c
			kept.Reused, kept.CostUSD = true, 0
			snd.musicCache = &kept
			return snd
		}
		snd.wg.Add(1)
		go func() {
			defer snd.wg.Done()
			r, err := a.sounds.Music(ctx, SoundRequest{TenantID: req.TenantID, VersionID: req.VersionID, Name: "music",
				Prompt: prompt, Seconds: total, Tier: tierOf(req)})
			if err != nil {
				a.logger.Warn("generated music failed; the ad uses the supplied track or none", zap.String("version_id", req.VersionID), zap.Error(err))
				return
			}
			snd.music = r
			snd.musicCache = &MusicCache{Prompt: prompt, URL: r.URL, Key: r.Key, Seconds: soundSeconds(CapMusic, total),
				Route: r.Route, CostUSD: r.CostUSD}
		}()
	}
	return snd
}

// aiNote is the on-screen label every ad with a synthetic person carries (ugc-video-ads.md, honesty
// rule 3). Presenter ads use a generated actor and generated storyboard stills, so a synthetic
// person can appear in any of their scenes; template ads show only the owner's own photos.
func aiNote(kind, language string) string {
	if !isPresenterKind(kind) {
		return ""
	}
	if language == "sw" {
		return "Imetengenezwa kwa AI"
	}
	return "AI-generated"
}

// adAINote is the request's on-screen AI note: aiNote, unless the owner turned the AI label off
// (owner 2026-11-14: the visual label is the owner's choice).
func adAINote(req AdRequest) string {
	if req.NoAILabel {
		return ""
	}
	return aiNote(req.Kind, req.Language)
}

// routeSummary names the routes the ad used, for the version's record.
func routeSummary(outs []sceneOut) string {
	seen := map[string]bool{}
	var list []string
	for _, o := range outs {
		if r := o.cache.Route; r != "" && !seen[r] {
			seen[r] = true
			list = append(list, r)
		}
	}
	return strings.Join(list, ",")
}

// parallel runs fn for each index, at most adWorkers at once, and returns the first error.
func (a *Ads) parallel(ctx context.Context, idx []int, fn func(i int) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sem   = make(chan struct{}, adWorkers)
	)
	for _, i := range idx {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if err := fn(i); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return first
}

// voiceScene reads one scene's words aloud. Estimated timings (the fal fallback gives
// none) are re-aligned onto the speech the audio actually holds.
func (a *Ads) voiceScene(ctx context.Context, req AdRequest, sp ScenePlan, s ScriptScene, voice string, out *sceneOut) error {
	if voice == "" {
		voice = req.Voice
	}
	v, err := a.voices.Make(ctx, VoiceRequest{TenantID: req.TenantID, VersionID: req.VersionID, Name: sceneName(sp),
		Text: s.Voiceover, Language: req.Language, Voice: voice, Tier: req.Tier, Delivery: req.Delivery, Prefs: req.Prefs})
	if err != nil {
		return fmt.Errorf("media: voice-over for scene %s: %w", sp.SceneID, err)
	}
	words, seconds := v.Words, v.Seconds
	if !v.Exact && len(v.Audio) > 0 {
		if spans, total, err := a.asm.SpeechSpans(ctx, v.Audio); err == nil {
			words = realignWords(words, spans)
			seconds = total
			if len(spans) > 0 {
				seconds = spans[len(spans)-1].end
			}
		}
	}
	out.cache = SceneCache{Hash: sp.Hash, SceneID: sp.SceneID, AudioURL: v.URL, AudioKey: v.Key, AudioSeconds: seconds,
		Words: words, Voice: v.Voice, VoiceSubstitutedFor: v.SubstitutedFor, CostUSD: v.CostUSD, Route: v.Route}
	return nil
}

// visualScene makes a presenter or clip scene and keeps its clip for the scene cache.
func (a *Ads) visualScene(ctx context.Context, req AdRequest, sp ScenePlan, s ScriptScene, dir string, out *sceneOut) error {
	path := filepath.Join(dir, sp.SceneID+"-"+sp.Hash[:8]+".mp4")
	var route string
	var cost float64
	switch sp.Work {
	case WorkPresenter:
		if out.cache.AudioURL == "" {
			return fmt.Errorf("media: presenter scene %s has no voice", sp.SceneID)
		}
		p, err := a.presenters.Make(ctx, PresenterRequest{TenantID: req.TenantID, VersionID: req.VersionID, ImageURL: req.ActorURL,
			AudioURL: out.cache.AudioURL, Seconds: out.cache.AudioSeconds, Tier: sceneTier(req, sp), Prompt: s.Visual, Format: req.Format})
		if err != nil {
			return fmt.Errorf("media: presenter scene %s: %w", sp.SceneID, err)
		}
		if err := downloadTo(ctx, a.asm.fetch, p.URL, path, maxSceneBytes, "presenter scene"); err != nil {
			return err
		}
		route, cost = p.Route, p.CostUSD
	case WorkClip:
		secs := clipSeconds(out.seconds)
		// The start frame is fitted to the ad's shape first, so every clip comes out that shape.
		frame, err := a.frames.fit(ctx, req.TenantID, req.VersionID, sceneName(sp)+"-frame", sp.source, req.Format)
		if err != nil {
			return fmt.Errorf("media: clip scene %s: %w", sp.SceneID, err)
		}
		_, rt, _, err := a.clips.Animate(ctx, ClipRequest{TenantID: req.TenantID, VersionID: req.VersionID, SceneURL: frame,
			Prompt: s.Visual, Seconds: secs, Tier: sceneTier(req, sp), Format: req.Format, AvoidText: sp.AvoidText, Scenario: ScenarioAdClip}, path)
		if err != nil {
			return fmt.Errorf("media: clip scene %s: %w", sp.SceneID, err)
		}
		route, cost = rt.Key, rt.Cost(float64(secs))
	}
	key, url, _, err := a.files.PutClip(ctx, req.TenantID, req.VersionID+"-"+sceneName(sp), path)
	if err != nil {
		return fmt.Errorf("media: store scene %s: %w", sp.SceneID, err)
	}
	out.cache.VideoURL, out.cache.VideoKey = url, key
	out.cache.Route = route
	out.cache.CostUSD += cost
	return nil
}

// reuse copies a cached scene's files under this version's names.
func (a *Ads) reuse(ctx context.Context, req AdRequest, c SceneCache, sp ScenePlan) (SceneCache, error) {
	name := req.VersionID + "-" + sceneName(sp)
	if c.AudioKey != "" {
		k, u, err := a.files.Copy(ctx, req.TenantID, c.AudioKey, name)
		if err != nil {
			return c, err
		}
		c.AudioKey, c.AudioURL = k, u
	}
	if c.VideoKey != "" {
		k, u, err := a.files.Copy(ctx, req.TenantID, c.VideoKey, name)
		if err != nil {
			return c, err
		}
		c.VideoKey, c.VideoURL = k, u
	}
	// A cue made from other text, or one that cannot be copied, is made again (it is cheap);
	// the scene is still reused.
	if want := sceneCue(req, sp.SceneID); c.SFXKey != "" && c.SFXText != want {
		c.SFXKey, c.SFXURL, c.SFXCostUSD, c.SFXText = "", "", 0, ""
	}
	if c.SFXKey != "" {
		if k, u, err := a.files.Copy(ctx, req.TenantID, c.SFXKey, name+"-sfx"); err == nil {
			c.SFXKey, c.SFXURL = k, u
		} else {
			c.SFXKey, c.SFXURL, c.SFXCostUSD = "", "", 0
		}
	}
	c.SceneID, c.Reused = sp.SceneID, true
	return c, nil
}

// sceneName names a scene's files within its version.
func sceneName(sp ScenePlan) string { return "scene-" + safeID(sp.SceneID) + "-" + sp.Hash[:8] }

// safeID keeps a scene id usable in an object key.
func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 24 {
			break
		}
	}
	if b.Len() == 0 {
		return "s"
	}
	return b.String()
}

// pick returns the asset'th link (1-based), or the next in order for 0.
func pick(list []string, asset int, next *int) string {
	if len(list) == 0 {
		return ""
	}
	if asset > 0 && asset <= len(list) {
		return list[asset-1]
	}
	u := list[*next%len(list)]
	*next++
	return u
}

// clampSeconds bounds a silent scene's scripted length.
func clampSeconds(s float64) float64 {
	return min(max(s, 1.5), 8)
}
