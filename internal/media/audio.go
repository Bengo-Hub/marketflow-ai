package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Audio steps of the assembler: the per-scene voice track, the mix, the two-pass
// loudness and the speech stretches used to re-align estimated word timings.

// loudTarget is the social feeds' loudness: -14 LUFS integrated, -1.5 dBTP peak.
const loudTarget = "loudnorm=I=-14:TP=-1.5:LRA=11"

// sceneAudio reports whether any scene carries its own voice-over.
func sceneAudio(scenes []AdScene) bool {
	for _, s := range scenes {
		if s.AudioURL != "" {
			return true
		}
	}
	return false
}

// sceneVoice lays each scene's voice-over at the start of its scene, cleaned by the filter
// chain (AudioConfig.VoiceFilter; "" for none) and padded or cut to the scene's length
// (silence for a scene without one), into one WAV.
func (a *Assembler) sceneVoice(ctx context.Context, dir string, scenes []AdScene, filter string) (string, error) {
	var list strings.Builder
	for i, s := range scenes {
		piece := filepath.Join(dir, fmt.Sprintf("a%d.wav", i))
		dur := strconv.FormatFloat(s.Seconds, 'f', 3, 64)
		var err error
		if s.AudioURL == "" {
			err = a.run(ctx, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", dur, "-c:a", "pcm_s16le", piece)
		} else {
			raw := filepath.Join(dir, fmt.Sprintf("a%d.audio", i))
			if err = downloadTo(ctx, a.fetch, s.AudioURL, raw, maxAudioBytes, fmt.Sprintf("scene %d voice", i+1)); err != nil {
				return "", err
			}
			err = a.run(ctx, "-i", raw, "-af", voicePieceFilter(filter, dur), "-ac", "2", "-c:a", "pcm_s16le", piece)
		}
		if err != nil {
			return "", fmt.Errorf("scene %d: %w", i+1, err)
		}
		fmt.Fprintf(&list, "file '%s'\n", filepath.ToSlash(piece))
	}
	listPath := filepath.Join(dir, "voices.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o600); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "voice.wav")
	if err := a.run(ctx, "-f", "concat", "-safe", "0", "-i", listPath, "-c", "copy", out); err != nil {
		return "", err
	}
	return out, nil
}

// placedCue is a downloaded sound cue and its start in the ad.
type placedCue struct {
	path string
	at   float64
}

// mix joins the voice, the music bed and the sound cues into one stereo WAV of the ad's
// length, before loudness is measured (mixGraph builds the graph).
func (a *Assembler) mix(ctx context.Context, voice, music string, cues []placedCue, m AudioMix, out string, total float64) error {
	in := mixInputs{voice: voice != "", music: music != "", mix: m, total: total}
	var args []string
	for _, p := range []string{voice, music} {
		if p != "" {
			args = append(args, "-i", p)
		}
	}
	for _, c := range cues {
		args = append(args, "-i", c.path)
		in.sfx = append(in.sfx, c.at)
	}
	graph := mixGraph(in)
	args = append(args, "-filter_complex", strings.Join(graph, ";"), "-map", "[a]", "-ac", "2",
		"-t", strconv.FormatFloat(total, 'f', 3, 64), "-c:a", "pcm_s16le", out)
	return a.run(ctx, args...)
}

// loudness is loudnorm's first-pass measurement.
type loudness struct {
	InputI      string `json:"input_i"`
	InputTP     string `json:"input_tp"`
	InputLRA    string `json:"input_lra"`
	InputThresh string `json:"input_thresh"`
	Offset      string `json:"target_offset"`
}

// secondPass is the loudnorm filter that applies the measurement linearly.
func (m loudness) secondPass() string {
	return fmt.Sprintf("%s:measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true",
		loudTarget, m.InputI, m.InputTP, m.InputLRA, m.InputThresh, m.Offset)
}

// measureLoudness runs loudnorm's first pass over the mix and reads its JSON report.
func (a *Assembler) measureLoudness(ctx context.Context, path string) (loudness, error) {
	logs, err := a.output(ctx, "-i", path, "-af", loudTarget+":print_format=json", "-f", "null", "-")
	if err != nil {
		return loudness{}, err
	}
	return parseLoudness(logs)
}

// parseLoudness reads the last JSON object loudnorm prints. A silent track measures -inf,
// which cannot be applied, so it is refused.
func parseLoudness(logs string) (loudness, error) {
	i, j := strings.LastIndex(logs, "{"), strings.LastIndex(logs, "}")
	if i < 0 || j < i {
		return loudness{}, fmt.Errorf("no loudnorm report")
	}
	var m loudness
	if err := json.Unmarshal([]byte(logs[i:j+1]), &m); err != nil {
		return loudness{}, fmt.Errorf("loudnorm report: %w", err)
	}
	for _, v := range []string{m.InputI, m.InputTP, m.InputLRA, m.InputThresh, m.Offset} {
		if _, err := strconv.ParseFloat(v, 64); err != nil || strings.Contains(v, "inf") {
			return loudness{}, fmt.Errorf("loudnorm measured %q", v)
		}
	}
	return m, nil
}

// span is a stretch of time in an audio file.
type span struct{ start, end float64 }

// SpeechSpans finds where speech is in a voice-over (the stretches between silences of
// 0.2 s or more under -35 dB) and its exact length.
func (a *Assembler) SpeechSpans(ctx context.Context, audio []byte) ([]span, float64, error) {
	if a == nil || a.ffmpeg == "" {
		return nil, 0, ErrNoFFmpeg
	}
	f, err := os.CreateTemp("", "voice-*.audio")
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(audio); err != nil {
		f.Close()
		return nil, 0, err
	}
	f.Close()
	logs, err := a.output(ctx, "-i", f.Name(), "-af", "silencedetect=noise=-35dB:d=0.2", "-f", "null", "-")
	if err != nil {
		return nil, 0, err
	}
	spans, total := parseSilences(logs)
	if total <= 0 {
		return nil, 0, fmt.Errorf("no duration in the voice-over")
	}
	return spans, total, nil
}

// parseSilences turns ffmpeg's Duration line and silencedetect's silence_start and
// silence_end lines into the speech stretches between them.
func parseSilences(logs string) ([]span, float64) {
	total := 0.0
	if i := strings.Index(logs, "Duration: "); i >= 0 {
		var h, m int
		var sec float64
		if _, err := fmt.Sscanf(logs[i+len("Duration: "):], "%d:%d:%f", &h, &m, &sec); err == nil {
			total = float64(h)*3600 + float64(m)*60 + sec
		}
	}
	var spans []span
	cur := 0.0
	for _, line := range strings.Split(logs, "\n") {
		if i := strings.Index(line, "silence_start: "); i >= 0 {
			if f := strings.Fields(line[i+len("silence_start: "):]); len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					if cur >= 0 && v > cur+0.05 {
						spans = append(spans, span{cur, v})
					}
					cur = -1
				}
			}
		}
		if i := strings.Index(line, "silence_end: "); i >= 0 {
			if f := strings.Fields(line[i+len("silence_end: "):]); len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					cur = v
				}
			}
		}
	}
	if cur >= 0 && total > cur+0.05 {
		spans = append(spans, span{cur, total})
	}
	return spans, total
}

// realignWords places estimated words onto the speech stretches: each word keeps its
// share of the characters, but the time it covers is speech time only, so no caption
// runs through a pause and the last word ends where the speaking ends.
func realignWords(words []WordTiming, spans []span) []WordTiming {
	if len(words) == 0 || len(spans) == 0 {
		return words
	}
	speech := 0.0
	for _, s := range spans {
		speech += s.end - s.start
	}
	est := words[len(words)-1].End
	if speech <= 0 || est <= 0 {
		return words
	}
	// at maps a point of speech time to clock time and names the stretch it falls in.
	at := func(t float64) (float64, int) {
		for k, s := range spans {
			d := s.end - s.start
			if t <= d {
				return s.start + t, k
			}
			t -= d
		}
		return spans[len(spans)-1].end, len(spans) - 1
	}
	out := make([]WordTiming, len(words))
	for i, w := range words {
		start, ks := at(w.Start / est * speech)
		end, ke := at(w.End / est * speech)
		// A word that would straddle a pause starts after it.
		if ke > ks {
			start = spans[ke].start
		}
		out[i] = WordTiming{Text: w.Text, Start: start, End: end}
	}
	return out
}

// output runs ffmpeg at the info log level and returns what it printed (loudnorm's and
// silencedetect's reports go to the log).
func (a *Assembler) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, a.ffmpeg, append([]string{"-hide_banner", "-nostats", "-loglevel", "info"}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, truncate(string(b), 400))
	}
	return string(b), nil
}
