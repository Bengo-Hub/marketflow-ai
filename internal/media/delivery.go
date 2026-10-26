package media

import (
	"sort"
	"strings"
)

// A delivery is how a voice-over should sound for one kind of content, the way
// ElevenLabs Studio sets a voice up per project: voice settings for the energy,
// an opening direction tag that v4 and v3 models follow (and never read aloud),
// and the voice traits that suit it. Ads and UGC want a livelier, less steady
// read than an explainer: lower stability lets the model follow the direction,
// some style adds expression, and a slightly faster pace keeps attention.
// The values are tuned starting points (ElevenLabs prompting guide, 2026-10-27:
// lower stability is more expressive, speed runs 0.7 to 1.2); change them here.

// VoiceSettings are ElevenLabs' voice_settings.
type VoiceSettings struct {
	Stability       float64 `json:"stability"`
	SimilarityBoost float64 `json:"similarity_boost"`
	Style           float64 `json:"style"`
	Speed           float64 `json:"speed"`
	SpeakerBoost    bool    `json:"use_speaker_boost"`
}

// Delivery is one content kind's voice direction.
type Delivery struct {
	Key       string        `json:"key"`
	Name      string        `json:"name"`
	Settings  VoiceSettings `json:"settings"`
	Direction string        `json:"direction"` // the opening tag
	// UseCases and Traits are ElevenLabs voice labels (use_case, descriptive)
	// and name words that fit this delivery, best first.
	UseCases []string `json:"use_cases"`
	Traits   []string `json:"traits"`
}

// Delivery keys.
const (
	DeliveryAd    = "ad"
	DeliveryUGC   = "ugc"
	DeliveryKids  = "kids"
	DeliveryStory = "story"
	DeliveryInfo  = "info"
)

// Deliveries are the voice directions the studio offers.
var Deliveries = map[string]Delivery{
	DeliveryAd: {Key: DeliveryAd, Name: "Ad, upbeat and confident",
		Settings:  VoiceSettings{Stability: 0.35, SimilarityBoost: 0.8, Style: 0.35, Speed: 1.05, SpeakerBoost: true},
		Direction: "[Upbeat, confident, warm ad read]",
		UseCases:  []string{"advertisement", "social_media", "conversational"},
		Traits:    []string{"confident", "warm", "upbeat", "energetic", "professional", "casual"}},
	DeliveryUGC: {Key: DeliveryUGC, Name: "UGC, casual and friendly",
		Settings:  VoiceSettings{Stability: 0.3, SimilarityBoost: 0.75, Style: 0.45, Speed: 1.05, SpeakerBoost: true},
		Direction: "[Casual, friendly, conversational tone, like talking to a friend]",
		UseCases:  []string{"social_media", "conversational"},
		Traits:    []string{"casual", "warm", "relatable", "natural", "joyful", "friendly"}},
	DeliveryKids: {Key: DeliveryKids, Name: "Kids and parents, cheerful",
		Settings:  VoiceSettings{Stability: 0.45, SimilarityBoost: 0.75, Style: 0.3, Speed: 1.0, SpeakerBoost: true},
		Direction: "[Warm, cheerful and encouraging tone]",
		UseCases:  []string{"informative_educational", "conversational", "narrative_story"},
		Traits:    []string{"warm", "cheerful", "joyful", "friendly", "calm"}},
	DeliveryStory: {Key: DeliveryStory, Name: "Brand story, warm and proud",
		Settings:  VoiceSettings{Stability: 0.5, SimilarityBoost: 0.8, Style: 0.25, Speed: 0.98, SpeakerBoost: true},
		Direction: "[Warm, proud storytelling tone]",
		UseCases:  []string{"narrative_story", "advertisement"},
		Traits:    []string{"warm", "rich", "deep", "calm", "confident"}},
	DeliveryInfo: {Key: DeliveryInfo, Name: "Clear explainer",
		Settings:  VoiceSettings{Stability: 0.55, SimilarityBoost: 0.8, Style: 0.15, Speed: 1.0, SpeakerBoost: true},
		Direction: "[Clear, friendly, professional tone]",
		UseCases:  []string{"informative_educational", "conversational"},
		Traits:    []string{"clear", "professional", "calm", "confident"}},
}

// DeliveryFor returns the named delivery, the ad delivery for "" or an unknown key.
func DeliveryFor(key string) Delivery {
	if d, ok := Deliveries[key]; ok {
		return d
	}
	return Deliveries[DeliveryAd]
}

// VoicePrefs narrow the choice of voice; empty fields do not count.
type VoicePrefs struct {
	Gender string // female | male
	Accent string // a label or family: kenyan, nigerian, african, british, american
	// Exclude skips voices already used (a different voice per A/B variant).
	Exclude []string
}

// africanAccents are the labels the "african" family covers in ElevenLabs'
// library (Kenyan voices are often labelled received pronunciation, so a name
// mentioning Kenya counts too; see accentMatches).
var africanAccents = []string{"african", "nigerian", "kenyan", "south african", "ghanaian", "east african", "west african"}

func accentMatches(v LibraryVoice, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	acc, name := strings.ToLower(v.Accent), strings.ToLower(v.Name)
	if want == "african" {
		for _, a := range africanAccents {
			if acc == a {
				return true
			}
		}
		return strings.Contains(name, "kenya") || strings.Contains(name, "african")
	}
	return acc == want || strings.Contains(name, want)
}

// voiceScore ranks a candidate for a delivery; higher is better. The weights
// make the configured default win an ordinary ad, the owner's collection beat
// the rest of the account, and a strong fit for the content (use case, traits,
// an asked-for accent) able to overtake both. An asked-for gender filters
// (rankVoices) rather than scores.
func voiceScore(v LibraryVoice, d Delivery, p VoicePrefs, defaultRank int) float64 {
	s := 0.0
	if defaultRank >= 0 {
		s += max(5-float64(defaultRank), 1)
	}
	if v.InCollection {
		s += 3
	}
	for i, uc := range d.UseCases {
		if v.UseCase == uc {
			s += 2 - 0.4*float64(i)
			break
		}
	}
	traits := 0
	text := strings.ToLower(v.Descriptive + " " + v.Name)
	for _, t := range d.Traits {
		if strings.Contains(text, t) {
			traits++
		}
	}
	s += float64(min(traits, 2))
	if accentMatches(v, p.Accent) {
		s += 3
	}
	if v.Premade {
		s -= 0.5 // premade voices are the fallback, not a first choice
	}
	return s
}

// rankVoices orders the candidates that can read lang with model for a
// delivery, best first. defaults names the configured default voices in order
// (an id or a name prefix each).
func rankVoices(all []LibraryVoice, d Delivery, p VoicePrefs, lang, model string, defaults []string) []LibraryVoice {
	type scored struct {
		v LibraryVoice
		s float64
	}
	var list []scored
	for _, v := range all {
		if !v.speaks(lang, model) || contains(p.Exclude, v.ID) {
			continue
		}
		// An asked-for gender is a requirement; a voice without the label stays.
		if p.Gender != "" && v.Gender != "" && !strings.EqualFold(v.Gender, p.Gender) {
			continue
		}
		rank := -1
		for i, def := range defaults {
			if v.ID == def || hasPrefixFold(v.Name, def) {
				rank = i
				break
			}
		}
		list = append(list, scored{v, voiceScore(v, d, p, rank)})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].s > list[j].s })
	out := make([]LibraryVoice, len(list))
	for i, x := range list {
		out[i] = x.v
	}
	return out
}

// stripTags removes bracketed directions ([excited], [short pause]) from text,
// leaving what is spoken: captions and word counts use it.
func stripTags(s string) string {
	if !strings.Contains(s, "[") {
		return s
	}
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
