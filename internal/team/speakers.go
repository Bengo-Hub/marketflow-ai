package team

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bengobox/marketflow-ai/internal/library"
)

// Two voices in UGC ads (owner, 2026-11-14): when it helps the ad, a UGC script is read by a
// man and a woman taking turns. The writer decides per line who says it (a scene's speaker);
// AssignSpeakers then makes the script consistent, so the renderer can trust it:
//   - one-voice scripts (every other video type, or a UGC script the writer kept to one
//     voice) carry no speaker at all;
//   - a spoken line the writer left without a speaker takes the turn after the line before;
//   - every presenter scene uses the one on-screen presenter's speaker, since all of them
//     are lip-synced from the same actor still;
//   - a script whose lines all ended up with one speaker is a one-voice script.

// Speakers.
const (
	SpeakerMale   = "male"
	SpeakerFemale = "female"
)

// DefaultTwoVoicePlaybooks are the playbooks whose scripts may use two voices.
var DefaultTwoVoicePlaybooks = []string{"ugc-video"}

// TwoVoicePlaybooksFromEnv reads MF_AD_TWO_VOICE_PLAYBOOKS: "" keeps the default (nil),
// "none" or "off" turns two voices off, anything else is a comma-separated list of keys.
func TwoVoicePlaybooksFromEnv(v string) []string {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "":
		return nil
	case "none", "off":
		return []string{}
	}
	var out []string
	for _, k := range strings.Split(v, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// NormalizeSpeaker reads a writer's speaker; anything that is not a man or a woman is "".
func NormalizeSpeaker(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "male", "man", "m":
		return SpeakerMale
	case "female", "woman", "f":
		return SpeakerFemale
	}
	return ""
}

// otherSpeaker is the speaker who takes the next turn.
func otherSpeaker(s string) string {
	if s == SpeakerMale {
		return SpeakerFemale
	}
	return SpeakerMale
}

// spoken reports whether a scene has words to read (delivery tags alone are not words).
func spoken(s Scene) bool { return strings.TrimSpace(stripVoiceTags(s.Voiceover)) != "" }

// AssignSpeakers makes a script's speakers consistent (see the package note above). With
// twoVoice false every speaker is cleared. fixedPresenter ("male", "female" or "") is the
// gender of an actor already saved on the brief: when set, presenter lines take that voice
// whatever the writer chose (owner, 2026-11-14: the script follows the actor).
func AssignSpeakers(v *VideoScript, twoVoice bool, fixedPresenter string) {
	if v == nil {
		return
	}
	clearAll := func() {
		for i := range v.Scenes {
			v.Scenes[i].Speaker = ""
		}
	}
	set := false
	for i := range v.Scenes {
		v.Scenes[i].Speaker = NormalizeSpeaker(v.Scenes[i].Speaker)
		if !spoken(v.Scenes[i]) {
			v.Scenes[i].Speaker = ""
		}
		set = set || v.Scenes[i].Speaker != ""
	}
	if !twoVoice || !set {
		// Not a two-voice ad, or the writer chose one voice for it.
		clearAll()
		return
	}
	// The on-screen presenter has one voice: the saved actor's, else the first presenter line's
	// speaker.
	presenter := NormalizeSpeaker(fixedPresenter)
	for _, s := range v.Scenes {
		if presenter != "" {
			break
		}
		if s.Shot == "presenter" && s.Speaker != "" {
			presenter = s.Speaker
		}
	}
	prev := ""
	for i := range v.Scenes {
		s := &v.Scenes[i]
		if !spoken(*s) {
			continue
		}
		if s.Shot == "presenter" && presenter != "" {
			s.Speaker = presenter
		}
		if s.Speaker == "" {
			if prev != "" {
				s.Speaker = otherSpeaker(prev)
			} else {
				// Lines before the first named speaker lead into that speaker's turn.
				s.Speaker = otherSpeaker(nextSpeaker(v.Scenes[i+1:]))
			}
		}
		if s.Shot == "presenter" && presenter == "" {
			presenter = s.Speaker
		}
		prev = s.Speaker
	}
	if len(Speakers(*v)) < 2 {
		clearAll()
	}
}

// nextSpeaker is the first speaker named in scenes ("" when none).
func nextSpeaker(scenes []Scene) string {
	for _, s := range scenes {
		if s.Speaker != "" {
			return s.Speaker
		}
	}
	return ""
}

// Speakers lists the distinct speakers of a script in the order they first speak.
func Speakers(v VideoScript) []string {
	var out []string
	for _, s := range v.Scenes {
		if s.Speaker != "" && !contains(out, s.Speaker) {
			out = append(out, s.Speaker)
		}
	}
	return out
}

// stripVoiceTags removes bracketed delivery tags, leaving what is spoken.
func stripVoiceTags(s string) string {
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

// CleanVoiceTags keeps at most one delivery tag, at the start of the line, and only one the
// library allows; every other bracketed text is removed. The voice model would otherwise act
// on an unknown tag or, worse, read a malformed one aloud.
func CleanVoiceTags(line string, allowed func(string) bool) string {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "[") {
		return line
	}
	if !strings.Contains(line, "]") {
		// A bracket that never closes is a typo, not a tag: keep the words.
		return strings.Join(strings.Fields(strings.ReplaceAll(line, "[", " ")), " ")
	}
	lead := ""
	if strings.HasPrefix(line, "[") {
		if end := strings.Index(line, "]"); end > 0 {
			if tag := strings.TrimSpace(line[1:end]); allowed != nil && allowed(tag) {
				lead = "[" + strings.ToLower(tag) + "] "
			}
		}
	}
	rest := stripVoiceTags(line)
	if rest == "" {
		return ""
	}
	return lead + rest
}

// QualityFindings checks a script against the library's quality limits: a hook that lands in
// about 2 seconds, a short first scene, a changing picture, short captions and a specific call
// to action. They are advice for the owner (severity low), never a block.
func QualityFindings(v VideoScript, q library.ScriptQuality) []Finding {
	var out []Finding
	if len(v.Scenes) == 0 {
		return out
	}
	first := v.Scenes[0]
	line := stripVoiceTags(first.Voiceover)
	switch {
	case line == "" && strings.TrimSpace(first.Caption) == "":
		out = append(out, Finding{Quote: first.ID, Severity: "low",
			Issue: "the opening has no words, so nothing hooks the viewer in the first 2 seconds",
			Fix:   "Give the first scene a short spoken hook or a caption that names the buyer's situation."})
	case q.HookMaxChars > 0 && utf8.RuneCountInString(firstSentence(line)) > q.HookMaxChars:
		out = append(out, Finding{Quote: firstSentence(line), Severity: "low",
			Issue: "the hook takes longer than about 2 seconds to say",
			Fix:   fmt.Sprintf("Cut the first sentence to under %d characters (about 8 words).", q.HookMaxChars)})
	}
	if q.FirstSceneMaxS > 0 && first.DurationS > q.FirstSceneMaxS {
		out = append(out, Finding{Quote: first.ID, Severity: "low",
			Issue: fmt.Sprintf("the first scene runs %.1f seconds, so the product shows late", first.DurationS),
			Fix:   fmt.Sprintf("Keep the first scene to %.1f seconds or less and show the product by second 3.", q.FirstSceneMaxS)})
	}
	var long []string
	for _, s := range v.Scenes {
		if q.MaxSceneS > 0 && s.DurationS > q.MaxSceneS && s.Shot != "presenter" && s.Shot != "end_card" {
			long = append(long, s.ID)
		}
		if c := strings.TrimSpace(s.Caption); q.MaxCaptionChars > 0 && utf8.RuneCountInString(c) > q.MaxCaptionChars {
			out = append(out, Finding{Quote: c, Severity: "low",
				Issue: "the caption is longer than two short lines on a phone",
				Fix:   fmt.Sprintf("Keep captions under %d characters; let the voice carry the rest.", q.MaxCaptionChars)})
		}
	}
	if len(long) > 0 {
		out = append(out, Finding{Quote: strings.Join(long, ", "), Severity: "low",
			Issue: "these scenes hold one picture for too long",
			Fix:   fmt.Sprintf("Split scenes longer than %.0f seconds so the picture changes.", q.MaxSceneS)})
	}
	cta := strings.TrimSpace(v.CTA)
	if cta == "" {
		cta = strings.TrimSpace(v.Scenes[len(v.Scenes)-1].Caption)
	}
	switch {
	case cta == "":
		out = append(out, Finding{Quote: "", Severity: "low", Issue: "the ad has no call to action",
			Fix: "End on one specific action, what they get and the channel."})
	case weakCTA(cta, q.WeakCTAs):
		out = append(out, Finding{Quote: cta, Severity: "low", Issue: "the call to action is generic",
			Fix: "Name the action, what they get and the channel, for example the WhatsApp word to send."})
	default:
		if f, ok := multiCTAFinding(cta, q.CTAChannels); ok {
			out = append(out, f)
		}
	}
	return out
}

// firstSentence is the text up to the first sentence end.
func firstSentence(s string) string {
	if i := strings.IndexAny(s, ".?!"); i >= 0 {
		return strings.TrimSpace(s[:i+1])
	}
	return strings.TrimSpace(s)
}

// weakCTA reports a call to action that is only a stock phrase: it matches one exactly, or is
// a stock phrase with at most one other word.
func weakCTA(cta string, weak []string) bool {
	norm := strings.ToLower(strings.Trim(strings.TrimSpace(cta), ".!? "))
	words := len(strings.Fields(norm))
	for _, w := range weak {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if norm == w || (strings.Contains(norm, w) && words <= len(strings.Fields(w))+1) {
			return true
		}
	}
	return false
}
