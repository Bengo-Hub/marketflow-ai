package media

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The audio ladder: what an ad's soundtrack holds, by plan. Each level adds to the one below:
//   - voice: the cleaned scene voices only (and the owner's own track, when they supply one);
//   - sfx: a short sound cue at the start of the scenes the writer marked;
//   - track: the owner's supplied backing track at the fixed bed gain, as before;
//   - score: music generated for the ad from its mood and length, ducked under the voice
//     (it dips while someone speaks and rises in the gaps), with a short fade in and out.
//     A supplied track is ducked the same way.
//
// The level comes from the tenant's plan features through AudioConfig.Ladder, never from a
// plan name. Every default lives in DefaultAudioConfig; AudioConfigFromEnv overrides them.

// Audio levels, lowest first.
const (
	AudioVoice = "voice"
	AudioSFX   = "sfx"
	AudioTrack = "track"
	AudioScore = "score"
)

var audioRank = map[string]int{AudioVoice: 0, AudioSFX: 1, AudioTrack: 2, AudioScore: 3}

// audioLevelOf is a request's level; an empty or unknown level is voice, the cheapest.
func audioLevelOf(level string) string {
	if _, ok := audioRank[level]; ok {
		return level
	}
	return AudioVoice
}

// audioAtLeast reports whether level includes min.
func audioAtLeast(level, min string) bool { return audioRank[audioLevelOf(level)] >= audioRank[min] }

// Ducking is how generated (or score-level) music sits under the voice: its base gain, the
// sidechain compressor keyed on the voice, and the fades.
type Ducking struct {
	MusicGain float64 // the bed's gain before ducking
	Threshold float64 // sidechaincompress threshold (linear)
	Ratio     float64
	AttackMS  float64
	ReleaseMS float64
	FadeIn    float64 // seconds
	FadeOut   float64 // seconds
}

// AudioConfig holds every audio setting of the ad pipeline.
type AudioConfig struct {
	// Ladder maps a plan feature to the audio level it grants; the highest wins.
	Ladder map[string]string
	// VoiceFilter is the ffmpeg clean up chain run on each scene voice before the two pass
	// loudnorm ("" turns it off).
	VoiceFilter string
	// SFXGain is a sound cue's gain under the voice; SFXSeconds the cue length asked for (1 to 3).
	SFXGain    float64
	SFXSeconds float64
	// MusicGain is the generated music's base gain; Duck the rest of its mix.
	MusicGain float64
	Duck      Ducking
	// MusicStyle closes every music prompt; MusicModel is the ElevenLabs Music model id.
	MusicStyle string
	MusicModel string
	// Deliveries override the voice settings per delivery (MF_VOICE_DELIVERY_SETTINGS).
	Deliveries map[string]DeliveryOverride
}

// Audio defaults (owner, 2026-11-12).
const (
	defaultVoiceFilter = "highpass=f=80,afftdn=nr=10:nf=-40,deesser=i=0.4,equalizer=f=3500:t=q:w=1.2:g=-2"
	defaultMusicStyle  = "instrumental, no vocals, advert background, builds gently"
	defaultMusicModel  = "music_v2_5"
	minSFXSeconds      = 1.0
	maxSFXSeconds      = 3.0
	maxSFXChars        = 60
)

// DefaultAudioConfig is the one place the audio defaults live.
func DefaultAudioConfig() AudioConfig {
	return AudioConfig{
		Ladder: map[string]string{
			"ai_video_presenter_premium": AudioScore,
			"ai_video_cinematic":         AudioTrack,
			"ai_video_presenter":         AudioSFX,
		},
		VoiceFilter: defaultVoiceFilter,
		SFXGain:     0.35,
		SFXSeconds:  2,
		MusicGain:   0.35,
		Duck:        Ducking{Threshold: 0.02, Ratio: 8, AttackMS: 20, ReleaseMS: 350, FadeIn: 0.5, FadeOut: 1.5},
		MusicStyle:  defaultMusicStyle,
		MusicModel:  defaultMusicModel,
	}
}

// Ducking is the music mix with the configured base gain.
func (c AudioConfig) Ducking() Ducking {
	d := c.Duck
	d.MusicGain = c.MusicGain
	return d
}

// LevelFor is the audio level a plan's features grant: the highest level of any feature that
// is on, voice when none matches.
func (c AudioConfig) LevelFor(features map[string]bool) string {
	level := AudioVoice
	for feature, on := range features {
		if l, ok := c.Ladder[feature]; on && ok && audioRank[l] > audioRank[level] {
			level = l
		}
	}
	return level
}

// ParseAudioLadder reads "feature=level,feature=level".
func ParseAudioLadder(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		feature, level, ok := strings.Cut(item, "=")
		feature, level = strings.TrimSpace(feature), strings.TrimSpace(level)
		if _, known := audioRank[level]; !ok || feature == "" || !known {
			return nil, fmt.Errorf("audio ladder entry %q is not feature=voice|sfx|track|score", item)
		}
		out[feature] = level
	}
	if len(out) == 0 {
		return nil, errors.New("audio ladder is empty")
	}
	return out, nil
}

// AudioConfigFromEnv reads the overrides: MF_AUDIO_LADDER, MF_VOICE_FILTER ("off" turns the
// clean up off), MF_AUDIO_SFX_GAIN, MF_AUDIO_SFX_SECONDS, MF_AUDIO_MUSIC_GAIN,
// MF_ELEVENLABS_MUSIC_MODEL and MF_VOICE_DELIVERY_SETTINGS (JSON). A bad value keeps its
// default; the error lists every bad value.
func AudioConfigFromEnv(getenv func(string) string) (AudioConfig, error) {
	cfg := DefaultAudioConfig()
	var errs []error
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	if v := get("MF_AUDIO_LADDER"); v != "" {
		if l, err := ParseAudioLadder(v); err != nil {
			errs = append(errs, fmt.Errorf("MF_AUDIO_LADDER: %w", err))
		} else {
			cfg.Ladder = l
		}
	}
	switch v := get("MF_VOICE_FILTER"); {
	case v == "":
	case strings.EqualFold(v, "off"):
		cfg.VoiceFilter = ""
	case strings.ContainsAny(v, ";[]'\""):
		// One plain chain only: labels or a second graph would break the scene filter.
		errs = append(errs, fmt.Errorf("MF_VOICE_FILTER must be one filter chain"))
	default:
		cfg.VoiceFilter = v
	}
	gain := func(key string, dst *float64) {
		if v := get(key); v != "" {
			g, err := strconv.ParseFloat(v, 64)
			if err != nil || !(g > 0 && g <= 1) { // also refuses NaN
				errs = append(errs, fmt.Errorf("%s must be a gain above 0 and at most 1", key))
				return
			}
			*dst = g
		}
	}
	gain("MF_AUDIO_SFX_GAIN", &cfg.SFXGain)
	gain("MF_AUDIO_MUSIC_GAIN", &cfg.MusicGain)
	if v := get("MF_AUDIO_SFX_SECONDS"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err != nil || math.IsNaN(s) {
			errs = append(errs, fmt.Errorf("MF_AUDIO_SFX_SECONDS must be a number"))
		} else {
			cfg.SFXSeconds = min(max(s, minSFXSeconds), maxSFXSeconds)
		}
	}
	if v := get("MF_ELEVENLABS_MUSIC_MODEL"); v != "" {
		cfg.MusicModel = v
	}
	if v := get("MF_VOICE_DELIVERY_SETTINGS"); v != "" {
		if d, err := ParseDeliveryOverrides(v); err != nil {
			errs = append(errs, fmt.Errorf("MF_VOICE_DELIVERY_SETTINGS: %w", err))
		} else {
			cfg.Deliveries = d
		}
	}
	return cfg, errors.Join(errs...)
}

// SFXCue is one sound cue placed in the ad.
type SFXCue struct {
	URL string  `json:"url"`
	At  float64 `json:"at"` // seconds from the ad's start
}

// AudioMix is how the assembler builds the soundtrack. The zero value is the mix as it was
// before the ladder: voice and, when given, the music bed at the fixed gain.
type AudioMix struct {
	VoiceFilter string   // run on each scene voice ("" for none)
	SFX         []SFXCue // cues at their times
	SFXGain     float64
	// Duck, when set, ducks the music under the voice and fades it (the score level).
	Duck *Ducking
}

// stereo48 makes every input of a ducked mix one format, so the sidechain sees the voice as it
// is mixed.
const stereo48 = "aformat=sample_rates=48000:channel_layouts=stereo"

// voicePieceFilter is one scene voice's filter: resampled, cleaned, then padded or cut to the
// scene's length.
func voicePieceFilter(chain, dur string) string {
	if chain = strings.TrimSpace(chain); chain == "" {
		return "aresample=48000,apad,atrim=0:" + dur
	}
	return "aresample=48000," + chain + ",apad,atrim=0:" + dur
}

// mixInputs are the inputs of the mix, in input order: the voice, the music, then each cue.
type mixInputs struct {
	voice, music bool
	sfx          []float64 // each cue's start, in seconds
	mix          AudioMix
	total        float64
}

// mixGraph builds the mix's filter graph, ending in [a]. Without cues or ducking it is the
// graph the assembler always used.
func mixGraph(in mixInputs) []string {
	musicIdx := 0
	if in.voice {
		musicIdx = 1
	}
	fade := func(d Ducking) string {
		return fmt.Sprintf("[%d:a]aresample=48000,%s,apad,atrim=0:%.3f,volume=%g,afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f",
			musicIdx, stereo48, in.total, d.MusicGain, d.FadeIn, math.Max(0, in.total-d.FadeOut), d.FadeOut)
	}
	if len(in.sfx) == 0 {
		switch {
		case in.voice && !in.music:
			return []string{"[0:a]aresample=48000,apad[a]"}
		case !in.voice && in.music && in.mix.Duck != nil:
			return []string{fade(*in.mix.Duck) + "[a]"}
		case !in.voice && in.music:
			return []string{fmt.Sprintf("[0:a]aresample=48000,volume=%.2f,apad[a]", musicGain*3)}
		}
	}
	var graph, mixIn []string
	duck := in.voice && in.music && in.mix.Duck != nil
	next := 0
	if in.voice {
		if duck {
			graph = append(graph, "[0:a]aresample=48000,"+stereo48+",apad,asplit=2[vo][key]")
		} else {
			graph = append(graph, "[0:a]aresample=48000,apad[vo]")
		}
		mixIn = append(mixIn, "[vo]")
		next++
	}
	if in.music {
		next++
		switch {
		case duck:
			d := *in.mix.Duck
			graph = append(graph, fade(d)+"[mu]",
				fmt.Sprintf("[mu][key]sidechaincompress=threshold=%g:ratio=%g:attack=%g:release=%g[md]", d.Threshold, d.Ratio, d.AttackMS, d.ReleaseMS))
			mixIn = append(mixIn, "[md]")
		case in.mix.Duck != nil:
			graph = append(graph, fade(*in.mix.Duck)+"[md]")
			mixIn = append(mixIn, "[md]")
		default:
			graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,volume=%.2f[m]", musicIdx, musicGain))
			mixIn = append(mixIn, "[m]")
		}
	}
	for k, at := range in.sfx {
		graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,%s,volume=%g,adelay=%d:all=1[s%d]",
			next, stereo48, in.mix.SFXGain, int(math.Round(math.Max(0, at)*1000)), k))
		mixIn = append(mixIn, fmt.Sprintf("[s%d]", k))
		next++
	}
	graph = append(graph, fmt.Sprintf("%samix=inputs=%d:duration=first:dropout_transition=0:normalize=0[a]", strings.Join(mixIn, ""), len(mixIn)))
	return graph
}

// musicPrompt is the music asked for: the ad's mood, then the configured style.
func musicPrompt(mood, delivery, style string) string {
	mood = strings.TrimSpace(mood)
	if mood == "" {
		mood = strings.ToLower(DeliveryFor(delivery).Name)
	}
	if r := []rune(mood); len(r) > 200 {
		mood = string(r[:200])
	}
	return strings.TrimSpace(strings.Trim(mood, " ,.") + ", " + style)
}

// AudioWithoutPaidParts is the nearest level below this one with no generated parts: score
// keeps a supplied backing track (track), sfx drops to the voice. Used when the balance cannot
// cover the cues and music.
func AudioWithoutPaidParts(level string) string {
	switch audioLevelOf(level) {
	case AudioScore:
		return AudioTrack
	case AudioSFX:
		return AudioVoice
	}
	return audioLevelOf(level)
}

// AudioChargeKey is the charge key of a render's cues and music: the render's own key plus
// ":audio", so a retry of the same charge never pays twice and a lost job's refund finds it.
func AudioChargeKey(renderKey string) string { return renderKey + ":audio" }
