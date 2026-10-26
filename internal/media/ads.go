package media

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A template video ad is made from an approved script (the team's video_script
// contract) and the owner's real photos and footage: one voice-over for the
// whole script, each scene as long as its own lines, captions from the voice's
// word timings, and the end card. Nothing is generated but the voice, so the
// product on screen is always the owner's own.

// Script shots this tier renders. A presenter needs an AI presenter ad (a
// licensed avatar library), which is not available yet.
const (
	ShotProduct   = "product_clip"
	ShotBRoll     = "b_roll"
	ShotFootage   = "owner_footage"
	ShotEndCard   = "end_card"
	ShotPresenter = "presenter"
)

// Ad limits. About 15 characters of speech per second keeps a 60 s ad under
// the assembler's limit before any voice is paid for.
const (
	MinAdScenes     = 2
	MaxAdVoiceChars = 900
	endCardTail     = endCardSeconds + 0.3 // the end card starts after the last word
)

// ScriptScene is one scene of an approved script.
type ScriptScene struct {
	ID        string  `json:"id"`
	Shot      string  `json:"shot"`
	Asset     int     `json:"asset,omitempty"` // 1-based photo or footage index; 0 picks in order
	Voiceover string  `json:"voiceover"`
	Caption   string  `json:"caption"`
	DurationS float64 `json:"duration_s"` // used only for a scene without voiceover
}

// AdRequest is one template video ad.
type AdRequest struct {
	TenantID  uuid.UUID
	VersionID string
	Format    string
	Tier      string
	Voice     string // a voice id or name prefix; "" lets the delivery pick the best voice
	Delivery  string // DeliveryAd (default), DeliveryUGC, DeliveryKids, DeliveryStory, DeliveryInfo
	Prefs     VoicePrefs
	Language  string // en or sw
	Scenes    []ScriptScene
	Photos    []string // the owner's product photos, http(s)
	Footage   []string // the owner's clips, http(s)
	EndCard   Overlay
	LogoURL   string
}

// Ads makes template video ads.
type Ads struct {
	voices *Voices
	asm    *Assembler
}

// NewAds builds the ad pipeline.
func NewAds(voices *Voices, asm *Assembler) *Ads { return &Ads{voices: voices, asm: asm} }

// Voices lists the voices an ad can use.
func (a *Ads) Voices() *Voices { return a.voices }

// Ready reports whether ads can be made here.
func (a *Ads) Ready() bool { return a != nil && a.voices != nil && a.asm.Ready() }

// Validate refuses a script this tier cannot render, before anything is paid.
func (a *Ads) Validate(req AdRequest) error {
	if _, ok := Formats[req.Format]; !ok {
		return &UserError{Msg: "format must be 1080x1080, 1080x1350 or 1080x1920"}
	}
	if _, ok := Deliveries[req.Delivery]; req.Delivery != "" && !ok {
		return &UserError{Msg: "delivery must be ad, ugc, kids, story or info"}
	}
	if len(req.Scenes) < MinAdScenes || len(req.Scenes) > MaxAdScenes {
		return &UserError{Msg: fmt.Sprintf("a video ad has %d to %d scenes", MinAdScenes, MaxAdScenes)}
	}
	for _, list := range [][]string{req.Photos, req.Footage} {
		for _, u := range list {
			if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
				return &UserError{Msg: "photos and footage must be http(s) links"}
			}
		}
	}
	chars := 0
	for i, s := range req.Scenes {
		switch s.Shot {
		case ShotProduct, ShotBRoll, ShotEndCard:
			if len(req.Photos) == 0 {
				return &UserError{Msg: fmt.Sprintf("scene %d shows the product: add at least one product photo", i+1)}
			}
			if s.Asset > len(req.Photos) {
				return &UserError{Msg: fmt.Sprintf("scene %d uses photo %d, but there are %d", i+1, s.Asset, len(req.Photos))}
			}
		case ShotFootage:
			if len(req.Footage) == 0 {
				return &UserError{Msg: fmt.Sprintf("scene %d uses your footage: add a video clip", i+1)}
			}
			if s.Asset > len(req.Footage) {
				return &UserError{Msg: fmt.Sprintf("scene %d uses clip %d, but there are %d", i+1, s.Asset, len(req.Footage))}
			}
		case ShotPresenter:
			return &UserError{Msg: fmt.Sprintf("scene %d needs an AI presenter, which this video type does not include", i+1)}
		default:
			return &UserError{Msg: fmt.Sprintf("scene %d has an unknown shot %q", i+1, s.Shot)}
		}
		if s.Asset < 0 {
			return &UserError{Msg: fmt.Sprintf("scene %d has an invalid asset number", i+1)}
		}
		chars += utf8.RuneCountInString(strings.TrimSpace(s.Voiceover))
	}
	if chars == 0 || chars > MaxAdVoiceChars {
		return &UserError{Msg: fmt.Sprintf("the voice-over is 1 to %d characters (about 60 seconds)", MaxAdVoiceChars)}
	}
	return nil
}

// Make reads the script aloud, times each scene to its lines and assembles the ad.
func (a *Ads) Make(ctx context.Context, req AdRequest) (*MediaResult, error) {
	if !a.Ready() {
		return nil, ErrNoFFmpeg
	}
	if err := a.Validate(req); err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(req.Scenes))
	for _, s := range req.Scenes {
		if v := strings.TrimSpace(s.Voiceover); v != "" {
			lines = append(lines, v)
		}
	}
	voice, err := a.voices.Make(ctx, VoiceRequest{TenantID: req.TenantID, VersionID: req.VersionID, Name: "voice",
		Text: strings.Join(lines, " "), Language: req.Language, Voice: req.Voice, Tier: req.Tier,
		Delivery: req.Delivery, Prefs: req.Prefs})
	if err != nil {
		return nil, fmt.Errorf("media: voice-over: %w", err)
	}
	seconds := sceneTimes(req.Scenes, voice.Words, voice.Seconds)
	scenes := make([]AdScene, len(req.Scenes))
	photo, clip := 0, 0
	for i, s := range req.Scenes {
		// Fit, not cover: the owner's product is never cropped.
		sc := AdScene{Seconds: seconds[i], Fit: true}
		if s.Shot == ShotFootage {
			sc.VideoURL = pick(req.Footage, s.Asset, &clip)
		} else {
			sc.ImageURL = pick(req.Photos, s.Asset, &photo)
		}
		scenes[i] = sc
	}
	card := req.EndCard
	res, err := a.asm.Make(ctx, AssembleRequest{TenantID: req.TenantID, VersionID: req.VersionID, Format: req.Format,
		Scenes: scenes, VoiceURL: voice.URL, Words: voice.Words, Captions: true, EndCard: &card, LogoURL: req.LogoURL})
	if err != nil {
		return nil, err
	}
	res.Route, res.CostUSD = voice.Route, res.CostUSD+voice.CostUSD
	return res, nil
}

// pick returns the asset'th link (1-based), or the next in order for 0.
func pick(list []string, asset int, next *int) string {
	if asset > 0 && asset <= len(list) {
		return list[asset-1]
	}
	u := list[*next%len(list)]
	*next++
	return u
}

// sceneTimes gives each scene the time its own lines take in the voice-over.
// A scene runs from where the previous one ended to the start of the next
// scene's first word, so every cut lands between sentences. A scene without
// lines keeps its scripted length. The last scene holds for the end card.
func sceneTimes(scenes []ScriptScene, words []WordTiming, voiceSeconds float64) []float64 {
	out := make([]float64, len(scenes))
	counts := make([]int, len(scenes))
	total := 0
	for i, s := range scenes {
		// Only spoken words: an inline [tag] is not in the voice's word timings.
		counts[i] = len(strings.Fields(stripTags(s.Voiceover)))
		total += counts[i]
	}
	if total != len(words) {
		return proportionalTimes(scenes, voiceSeconds)
	}
	t, k := 0.0, 0
	for i, s := range scenes {
		if counts[i] == 0 {
			out[i] = clampSeconds(s.DurationS)
			t += out[i]
			continue
		}
		k += counts[i]
		end := words[k-1].End
		if k < len(words) {
			end = words[k].Start
		}
		out[i] = max(end-t, minSceneSeconds)
		t += out[i]
	}
	out[len(out)-1] += endCardTail
	return out
}

// proportionalTimes splits the voice-over by each scene's share of the
// characters, for a voice whose word count does not match the script.
func proportionalTimes(scenes []ScriptScene, voiceSeconds float64) []float64 {
	out := make([]float64, len(scenes))
	chars := 0
	for _, s := range scenes {
		chars += utf8.RuneCountInString(stripTags(s.Voiceover))
	}
	for i, s := range scenes {
		n := utf8.RuneCountInString(stripTags(s.Voiceover))
		if n == 0 || chars == 0 {
			out[i] = clampSeconds(s.DurationS)
			continue
		}
		out[i] = max(voiceSeconds*float64(n)/float64(chars), minSceneSeconds)
	}
	out[len(out)-1] += endCardTail
	return out
}

// clampSeconds bounds a silent scene's scripted length.
func clampSeconds(s float64) float64 {
	return min(max(s, 1.5), 8)
}
