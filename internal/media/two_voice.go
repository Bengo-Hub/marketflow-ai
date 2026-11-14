package media

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Two voices in UGC ads (owner, 2026-11-14). A UGC script may give each spoken scene a
// speaker, "male" or "female", taking turns. The ad then reads each scene in its speaker's
// voice:
//   - only deliveries in AudioConfig.TwoVoiceDeliveries (default ugc) may use two voices, and
//     only when the script really has both speakers; every other ad has one voice;
//   - both voices come from the same routing as any voice (ElevenLabs direct, the account's
//     collection, the configured defaults per gender, the fal fallback);
//   - when no second voice of the other gender can be found the ad is read by one voice and
//     the result says why (MediaResult.TwoVoiceNote);
//   - the scene voices are still laid end to end in timeline order, each cleaned by the same
//     chain, levelled to one loudness so neither voice sits above the other, and the music
//     ducks under that joined voice track, so it dips under whichever speaker is talking.

// Speakers of a two-voice ad.
const (
	SpeakerMale   = "male"
	SpeakerFemale = "female"
)

// normalSpeaker reads a script's speaker; anything that is not a man or a woman is "".
func normalSpeaker(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "male", "man", "m":
		return SpeakerMale
	case "female", "woman", "f":
		return SpeakerFemale
	}
	return ""
}

func otherSpeaker(s string) string {
	if s == SpeakerMale {
		return SpeakerFemale
	}
	return SpeakerMale
}

// speakerTurns is each scene's speaker in timeline order, the way the script writer's rules
// set them (team.AssignSpeakers), so an owner's edit cannot break the ad: a scene without
// words has none; a spoken scene left blank takes the turn after the scene before (or leads
// into the first named speaker); every presenter scene uses the first presenter scene's
// speaker, since all of them are lip-synced from one actor. With no speaker named at all every
// turn is "".
func speakerTurns(scenes []ScriptScene) []string {
	turns := make([]string, len(scenes))
	named := false
	for i, s := range scenes {
		if strings.TrimSpace(stripTags(s.Voiceover)) == "" {
			continue
		}
		turns[i] = normalSpeaker(s.Speaker)
		named = named || turns[i] != ""
	}
	if !named {
		return turns
	}
	presenter := ""
	for i, s := range scenes {
		if s.Shot == ShotPresenter && turns[i] != "" {
			presenter = turns[i]
			break
		}
	}
	prev := ""
	for i, s := range scenes {
		if strings.TrimSpace(stripTags(s.Voiceover)) == "" {
			continue
		}
		if s.Shot == ShotPresenter && presenter != "" {
			turns[i] = presenter
		}
		if turns[i] == "" {
			if prev != "" {
				turns[i] = otherSpeaker(prev)
			} else {
				next := ""
				for _, t := range turns[i+1:] {
					if t != "" {
						next = t
						break
					}
				}
				turns[i] = otherSpeaker(next)
			}
		}
		if s.Shot == ShotPresenter && presenter == "" {
			presenter = turns[i]
		}
		prev = turns[i]
	}
	return turns
}

// distinctSpeakers lists the speakers in the order they first speak.
func distinctSpeakers(turns []string) []string {
	var out []string
	for _, t := range turns {
		if t != "" && !contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// twoVoice reports whether the ad is read by two voices: its delivery allows it and its
// script has both speakers.
func (a *Ads) twoVoice(req AdRequest) bool {
	return a.audio.TwoVoiceDeliveries[req.Delivery] && len(distinctSpeakers(speakerTurns(req.Scenes))) == 2
}

// voicePicker is the part of Voices casting needs (a fake in tests).
type voicePicker interface {
	Pick(ctx context.Context, req VoiceRequest) (LibraryVoice, bool)
}

// maxCastTries bounds how many unlabelled voices casting skips for one speaker.
const maxCastTries = 6

// castVoices picks one voice per speaker, lead speaker first. A speaker whose reused scenes
// already have a voice keeps it. The owner's chosen voice reads for the speaker of its gender
// (the lead when its gender is not labelled). Any other speaker gets the best voice of its
// gender for the delivery and accent, never the same voice as the other speaker. note is not
// "" when the ad cannot have two voices; the caller then reads it with one.
func (a *Ads) castVoices(ctx context.Context, req AdRequest, speakers []string, seeded map[string]string) (map[string]string, string) {
	if a.voices == nil {
		return nil, "voices are not configured"
	}
	return castVoices(ctx, a.voices, req, speakers, seeded)
}

func castVoices(ctx context.Context, p voicePicker, req AdRequest, speakers []string, seeded map[string]string) (map[string]string, string) {
	if len(speakers) != 2 {
		return nil, "the script does not have two speakers"
	}
	lead, other := speakers[0], speakers[1]
	cast := map[string]LibraryVoice{}
	for _, sp := range speakers {
		if name := seeded[sp]; name != "" {
			if v, ok := p.Pick(ctx, VoiceRequest{Voice: name, Language: req.Language, Delivery: req.Delivery}); ok {
				cast[sp] = v
			}
		}
	}
	if name := strings.TrimSpace(req.Voice); name != "" {
		if v, ok := p.Pick(ctx, VoiceRequest{Voice: name, Language: req.Language, Delivery: req.Delivery}); ok {
			sp := lead
			if normalSpeaker(v.Gender) == other {
				sp = other
			}
			if _, taken := cast[sp]; !taken {
				cast[sp] = v
			}
		}
	}
	for _, sp := range speakers {
		if _, ok := cast[sp]; ok {
			continue
		}
		exclude := append([]string{}, req.Prefs.Exclude...)
		for _, v := range cast {
			exclude = append(exclude, v.ID)
		}
		// The gender filter keeps voices with no gender label, so a pick without the label is
		// skipped and the next best tried, a few times at most.
		found := false
		for try := 0; try < maxCastTries && !found; try++ {
			v, ok := p.Pick(ctx, VoiceRequest{Language: req.Language, Delivery: req.Delivery,
				Prefs: VoicePrefs{Gender: sp, Accent: req.Prefs.Accent, Exclude: exclude}})
			switch {
			case !ok || v.ID == "" || contains(exclude, v.ID):
				try = maxCastTries
			case normalSpeaker(v.Gender) == sp:
				cast[sp], found = v, true
			default:
				exclude = append(exclude, v.ID)
			}
		}
		if !found {
			return nil, fmt.Sprintf("no %s voice is available, so one voice reads the whole ad", sp)
		}
	}
	if cast[lead].ID == cast[other].ID {
		return nil, "both speakers resolved to the same voice, so one voice reads the whole ad"
	}
	out := map[string]string{}
	for sp, v := range cast {
		out[sp] = v.ID
	}
	return out, ""
}

// VoiceSpan is one scene's voice in the ad's timeline: who speaks and when.
type VoiceSpan struct {
	Speaker string  `json:"speaker"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
}

// voiceTimeline lists the scenes that carry a voice, in timeline order, each from its scene's
// start to its end; the scene voices are laid end to end in exactly this order.
func voiceTimeline(scenes []AdScene) []VoiceSpan {
	var out []VoiceSpan
	t := 0.0
	for _, s := range scenes {
		if s.AudioURL != "" {
			out = append(out, VoiceSpan{Speaker: s.Speaker, Start: round3(t), End: round3(t + s.Seconds)})
		}
		t += s.Seconds
	}
	return out
}

// sceneSpeakers counts the distinct speakers among the scenes that carry a voice.
func sceneSpeakers(scenes []AdScene) int {
	var seen []string
	for _, s := range scenes {
		if s.AudioURL != "" && s.Speaker != "" && !contains(seen, s.Speaker) {
			seen = append(seen, s.Speaker)
		}
	}
	return len(seen)
}

// maxMatchGainDB bounds the level change of one scene voice: a larger one means a broken or
// near-silent piece, which is better left as it is.
const maxMatchGainDB = 12.0

// matchGain is the gain in dB that brings a piece measured at inputI (loudnorm's input_i) to
// target LUFS. ok is false when the measurement is unusable or the change is under half a dB.
func matchGain(inputI string, target float64) (float64, bool) {
	in, err := strconv.ParseFloat(strings.TrimSpace(inputI), 64)
	if err != nil || math.IsNaN(in) || math.IsInf(in, 0) || in < -70 || target == 0 {
		return 0, false
	}
	g := math.Max(-maxMatchGainDB, math.Min(maxMatchGainDB, target-in))
	if math.Abs(g) < 0.5 {
		return 0, false
	}
	return math.Round(g*100) / 100, true
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
