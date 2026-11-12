package media

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The audio level comes from the plan's features through the configured ladder: the highest
// level a feature grants wins, and no match is voice only.
func TestAudioLadderFromFeatures(t *testing.T) {
	cfg := DefaultAudioConfig()
	for _, c := range []struct {
		features map[string]bool
		want     string
	}{
		{nil, AudioVoice},
		{map[string]bool{"ai_video_template": true}, AudioVoice},
		{map[string]bool{"ai_video_presenter": true}, AudioSFX},
		{map[string]bool{"ai_video_presenter": true, "ai_video_cinematic": true}, AudioTrack},
		{map[string]bool{"ai_video_presenter": true, "ai_video_cinematic": true, "ai_video_presenter_premium": true}, AudioScore},
		// A feature switched off grants nothing.
		{map[string]bool{"ai_video_presenter_premium": false, "ai_video_presenter": true}, AudioSFX},
	} {
		if got := cfg.LevelFor(c.features); got != c.want {
			t.Errorf("%v: level %q, want %q", c.features, got, c.want)
		}
	}

	ladder, err := ParseAudioLadder(" ai_video_template = track , ai_video_presenter=score ")
	if err != nil || len(ladder) != 2 || ladder["ai_video_template"] != AudioTrack || ladder["ai_video_presenter"] != AudioScore {
		t.Fatalf("ladder = %v %v", ladder, err)
	}
	for _, bad := range []string{"ai_video_template", "ai_video_template=loud", "=score"} {
		if _, err := ParseAudioLadder(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// Every setting has a default in code and an environment override; a bad value keeps the
// default and is reported.
func TestAudioConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	def, err := AudioConfigFromEnv(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if def.VoiceFilter != "highpass=f=80,afftdn=nr=10:nf=-40,deesser=i=0.4,equalizer=f=3500:t=q:w=1.2:g=-2" ||
		def.SFXGain != 0.35 || def.MusicGain != 0.35 || def.Duck.Threshold != 0.02 || def.Duck.Ratio != 8 ||
		def.Duck.AttackMS != 20 || def.Duck.ReleaseMS != 350 || def.Duck.FadeIn != 0.5 || def.Duck.FadeOut != 1.5 ||
		def.SFXSeconds < 1 || def.SFXSeconds > 3 || def.MusicModel == "" || def.LevelFor(map[string]bool{"ai_video_presenter": true}) != AudioSFX {
		t.Fatalf("defaults = %+v", def)
	}
	cfg, err := AudioConfigFromEnv(env(map[string]string{
		"MF_AUDIO_LADDER": "ai_video_template=score", "MF_VOICE_FILTER": "off", "MF_AUDIO_SFX_GAIN": "0.2",
		"MF_AUDIO_MUSIC_GAIN": "0.5", "MF_ELEVENLABS_MUSIC_MODEL": "music_v2", "MF_AUDIO_SFX_SECONDS": "9",
		"MF_VOICE_DELIVERY_SETTINGS": `{"ad":{"stability":0.6,"direction":"[calm]"}}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VoiceFilter != "" || cfg.SFXGain != 0.2 || cfg.MusicGain != 0.5 || cfg.MusicModel != "music_v2" || cfg.SFXSeconds != 3 ||
		cfg.LevelFor(map[string]bool{"ai_video_template": true}) != AudioScore || cfg.LevelFor(map[string]bool{"ai_video_presenter": true}) != AudioVoice ||
		cfg.Deliveries[DeliveryAd].Stability == nil || *cfg.Deliveries[DeliveryAd].Stability != 0.6 {
		t.Fatalf("overridden = %+v", cfg)
	}
	bad, err := AudioConfigFromEnv(env(map[string]string{"MF_AUDIO_SFX_GAIN": "3", "MF_VOICE_FILTER": "volume=2;[x]", "MF_AUDIO_LADDER": "x"}))
	if err == nil || bad.SFXGain != def.SFXGain || bad.VoiceFilter != def.VoiceFilter || bad.LevelFor(map[string]bool{"ai_video_presenter": true}) != AudioSFX {
		t.Fatalf("bad values must keep the defaults and be reported: %+v %v", bad, err)
	}
}

// The clean up chain runs on each scene voice before it is padded to the scene; "off" leaves
// the voice as it was.
func TestVoicePieceFilter(t *testing.T) {
	chain := DefaultAudioConfig().VoiceFilter
	got := voicePieceFilter(chain, "2.250")
	if got != "aresample=48000,"+chain+",apad,atrim=0:2.250" {
		t.Fatalf("filter = %q", got)
	}
	for _, part := range []string{"highpass=f=80", "afftdn", "deesser", "equalizer=f=3500"} {
		if !strings.Contains(got, part) {
			t.Fatalf("chain lacks %s: %q", part, got)
		}
	}
	if voicePieceFilter("", "2.250") != "aresample=48000,apad,atrim=0:2.250" {
		t.Fatal("no chain: the voice is only padded")
	}
}

// The mix: the track level keeps today's fixed bed; the score level ducks the music under the
// voice with sidechain compression and fades it; sound cues sit at their scene starts at a low gain.
func TestMixGraph(t *testing.T) {
	cfg := DefaultAudioConfig()
	track := strings.Join(mixGraph(mixInputs{voice: true, music: true, total: 20}), ";")
	if !strings.Contains(track, "volume=0.18") || strings.Contains(track, "sidechaincompress") || strings.Contains(track, "afade") ||
		!strings.Contains(track, "amix=inputs=2") {
		t.Fatalf("track mix = %s", track)
	}
	duck := cfg.Ducking()
	score := strings.Join(mixGraph(mixInputs{voice: true, music: true, sfx: []float64{0, 1.5}, total: 20,
		mix: AudioMix{SFXGain: cfg.SFXGain, Duck: &duck}}), ";")
	for _, want := range []string{
		"asplit=2[vo][key]", "volume=0.35", "afade=t=in:st=0:d=0.500", "afade=t=out:st=18.500:d=1.500",
		"[mu][key]sidechaincompress=threshold=0.02:ratio=8:attack=20:release=350",
		"[2:a]", "[3:a]", "adelay=1500:all=1", "amix=inputs=4",
	} {
		if !strings.Contains(score, want) {
			t.Fatalf("score mix lacks %q:\n%s", want, score)
		}
	}
	// Sound cues without music: no ducking.
	sfx := strings.Join(mixGraph(mixInputs{voice: true, sfx: []float64{2}, total: 10, mix: AudioMix{SFXGain: 0.35}}), ";")
	if strings.Contains(sfx, "sidechaincompress") || !strings.Contains(sfx, "[1:a]") || !strings.Contains(sfx, "adelay=2000:all=1") ||
		!strings.Contains(sfx, "amix=inputs=2") {
		t.Fatalf("sfx mix = %s", sfx)
	}
	// Voice only stays as it was.
	if g := mixGraph(mixInputs{voice: true, total: 10}); len(g) != 1 || g[0] != "[0:a]aresample=48000,apad[a]" {
		t.Fatalf("voice only = %v", g)
	}
}

// Ad delivery settings come from config: defaults in code, any field overridable, values out
// of ElevenLabs' range refused.
func TestDeliveryOverrides(t *testing.T) {
	ad := Deliveries[DeliveryAd]
	if s := ad.Settings; s.Stability != 0.5 || s.SimilarityBoost != 0.75 || s.Style != 0.15 || s.Speed != 1.0 || s.SpeakerBoost ||
		ad.Direction != "[warm, natural, conversational]" {
		t.Fatalf("ad delivery = %+v", ad)
	}
	over, err := ParseDeliveryOverrides(`{"ad":{"style":0.2,"use_speaker_boost":true},"ugc":{"speed":1.1}}`)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVoices(keysFor(), &memAudio{}, nil).WithDeliveryOverrides(over)
	if d := v.Delivery(DeliveryAd); d.Settings.Style != 0.2 || !d.Settings.SpeakerBoost || d.Settings.Stability != 0.5 || d.Direction != ad.Direction {
		t.Fatalf("overridden ad = %+v", d)
	}
	if d := v.Delivery(DeliveryUGC); d.Settings.Speed != 1.1 {
		t.Fatalf("overridden ugc = %+v", d)
	}
	if d := v.Delivery(""); d.Key != DeliveryAd || d.Settings.Style != 0.2 {
		t.Fatalf("the default delivery is the overridden ad: %+v", d)
	}
	if Deliveries[DeliveryAd].Settings.Style != 0.15 {
		t.Fatal("an override must not change the shared defaults")
	}
	for _, bad := range []string{`{"ad":{"speed":2}}`, `{"ad":{"stability":-1}}`, `{"opera":{"speed":1}}`, `not json`} {
		if _, err := ParseDeliveryOverrides(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// When only fal can read a library voice, the premade voice that stands in is recorded on the
// result; a premade voice read as itself records nothing.
func TestVoiceFalSubstitutionIsRecorded(t *testing.T) {
	newFakeProviders(t)
	el := newFakeEleven(t, "free")
	el.collection = ownerCollection()
	v := NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Karibu"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "fal/elevenlabs-v3" || res.Voice != "Sarah" || !strings.HasPrefix(res.SubstitutedFor, "Yna Agalo") {
		t.Fatalf("substitution not recorded: %+v", res)
	}
	res, err = v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v2", Text: "Karibu", Voice: "Brian"})
	if err != nil || res.Voice != "Brian" || res.SubstitutedFor != "" {
		t.Fatalf("a premade voice is not a substitution: %+v %v", res, err)
	}
}

// Sound cues run on fal's ElevenLabs sound effects; music runs on ElevenLabs Music direct on a
// paid plan, with fal's ElevenLabs music as the fallback. Prices follow the seconds made.
func TestSoundsRoutes(t *testing.T) {
	f := newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	store := &memAudio{}
	s := NewSounds(keysFor("elevenlabs", "fal"), store, NewVoices(keysFor("elevenlabs", "fal"), store, nil), nil)

	fx, err := s.Effect(context.Background(), SoundRequest{TenantID: uuid.New(), VersionID: "v1", Name: "sfx-s1", Prompt: "soft whoosh", Seconds: 2})
	if err != nil {
		t.Fatal(err)
	}
	in := f.inputs["elevenlabs/sound-effects/v3"]
	if fx.Route != "fal/elevenlabs-sfx-v3" || in["text"] != "soft whoosh" || in["duration_seconds"] != 2.0 || math.Abs(fx.CostUSD-0.004) > 1e-9 || fx.URL == "" {
		t.Fatalf("effect = %+v, input %v", fx, in)
	}

	el.music = []byte("ID3music")
	mu, err := s.Music(context.Background(), SoundRequest{TenantID: uuid.New(), VersionID: "v1", Name: "music", Prompt: "warm afrobeat", Seconds: 29.2})
	if err != nil {
		t.Fatal(err)
	}
	if mu.Route != "elevenlabs/music" || el.musicBody["prompt"] != "warm afrobeat" || el.musicBody["music_length_ms"] != 29200.0 ||
		el.musicBody["force_instrumental"] != true || el.musicBody["model_id"] != DefaultAudioConfig().MusicModel ||
		!strings.Contains(el.musicQuery, "output_format=mp3_44100_128") || math.Abs(mu.CostUSD-0.30) > 1e-9 {
		t.Fatalf("direct music = %+v, body %v %s", mu, el.musicBody, el.musicQuery)
	}

	// Direct fails: fal's music route makes it.
	el.music = nil
	mu, err = NewSounds(keysFor("elevenlabs", "fal"), store, NewVoices(keysFor("elevenlabs", "fal"), store, nil), nil).
		Music(context.Background(), SoundRequest{TenantID: uuid.New(), VersionID: "v1", Name: "music", Prompt: "warm afrobeat", Seconds: 2})
	fin := f.inputs["elevenlabs/music/v2.5"]
	if err != nil || mu.Route != "fal/elevenlabs-music-v2.5" || fin["prompt"] != "warm afrobeat" || fin["force_instrumental"] != true ||
		fin["music_length_ms"] != 3000.0 {
		t.Fatalf("fal music = %+v %v, input %v", mu, err, fin)
	}

	// A quote without any key is free and names no route: nothing would be made.
	if usd, route := NewSounds(keysFor(), store, nil, nil).Quote(context.Background(), CapMusic, TierStandard, 30); usd != 0 || route != "" {
		t.Fatalf("no key quote = %v %q", usd, route)
	}
	if usd, route := s.Quote(context.Background(), CapSFX, TierStandard, 2); route != "fal/elevenlabs-sfx-v3" || math.Abs(usd-0.004) > 1e-9 {
		t.Fatalf("sfx quote = %v %q", usd, route)
	}
}

// The quote counts sound cues from the sfx level up and generated music only at the score level,
// and the scene hash follows the cue and the level without moving voice-only hashes.
func TestAdPlanAudioParts(t *testing.T) {
	newFakeProviders(t)
	newFakeEleven(t, "free")
	ads := NewAds(NewVoices(keysFor("fal"), &memAudio{}, nil), nil)
	req := AdRequest{Format: "1080x1920", Photos: []string{"https://x/p.jpg"},
		Scenes: []ScriptScene{
			{ID: "s1", Shot: ShotProduct, Voiceover: "Soft skin all week.", SFX: "soft whoosh"},
			{ID: "s2", Shot: ShotEndCard, Voiceover: "Order on WhatsApp.", SFX: "cash register ding"},
			{ID: "s3", Shot: ShotEndCard, Voiceover: "Karibu."},
		}}
	voice, err := ads.Plan(context.Background(), req)
	if err != nil || voice.SFXUSD != 0 || voice.MusicUSD != 0 || voice.Audio != AudioVoice {
		t.Fatalf("voice level plan = %+v %v", voice, err)
	}
	req.Audio = AudioSFX
	sfx, _ := ads.Plan(context.Background(), req)
	if math.Abs(sfx.SFXUSD-2*0.002*DefaultAudioConfig().SFXSeconds) > 1e-9 || sfx.MusicUSD != 0 ||
		math.Abs(sfx.CostUSD-(sfx.VoiceUSD+sfx.SFXUSD)) > 1e-9 {
		t.Fatalf("sfx level plan = %+v", sfx)
	}
	req.Audio = AudioScore
	score, _ := ads.Plan(context.Background(), req)
	if score.MusicUSD <= 0 || score.MusicRoute != "fal/elevenlabs-music-v2.5" ||
		math.Abs(score.CostUSD-(score.VoiceUSD+score.SFXUSD+score.MusicUSD)) > 1e-9 {
		t.Fatalf("score level plan = %+v", score)
	}

	// Hashes: the level and the cue are mixed in at assembly, so neither changes a scene's
	// hash (a plan change or a new cue never re-renders and re-charges the scene's voice or
	// video). Retuned voice settings do change it.
	base := AdRequest{Kind: AdTemplate, Voice: "Yna"}
	s := ScriptScene{ID: "s1", Shot: ShotProduct, Voiceover: "Hello"}
	sp := ScenePlan{Work: WorkStill, source: "https://x/p.jpg"}
	h := sceneHash(base, s, sp, "d1")
	withCue := s
	withCue.SFX = "soft whoosh"
	for _, level := range []string{AudioVoice, AudioSFX, AudioTrack, AudioScore} {
		r := base
		r.Audio = level
		if sceneHash(r, s, sp, "d1") != h || sceneHash(r, withCue, sp, "d1") != h {
			t.Fatalf("level %s or a cue changed the scene hash", level)
		}
	}
	if sceneHash(base, s, sp, "d2") == h {
		t.Fatal("retuned voice settings must change the hash")
	}
}

// A cached cue is reused only when it was made from the same text; changing the cue makes a
// new cue (cheap) and keeps the scene's voice and video.
func TestCachedCueFollowsItsText(t *testing.T) {
	files := &sceneFiles{files: map[string][]byte{"k1": []byte("cue")}}
	a := &Ads{files: files}
	c := SceneCache{SFXKey: "k1", SFXText: "soft whoosh"}
	if !a.cachedCue(context.Background(), c, "soft whoosh") {
		t.Fatal("same text: the cue is reused")
	}
	if a.cachedCue(context.Background(), c, "till ding") {
		t.Fatal("other text: the cue is made again")
	}
}

// A score-level ad: each cue is generated and placed, music is generated and ducked under the
// voice, and the result is at the feeds' loudness. When music fails the ad is still made, with
// no music and no music cost. Needs ffmpeg.
func TestAdsMakeAudioLadder(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	voice := mustRead(t, makeMedia(t, ff, dir, "voice.mp3", "-f", "lavfi", "-i", "sine=frequency=330:duration=2", "-c:a", "libmp3lame"))
	cue := mustRead(t, makeMedia(t, ff, dir, "cue.mp3", "-f", "lavfi", "-i", "anoisesrc=d=1:c=pink:a=0.3", "-c:a", "libmp3lame"))
	music := mustRead(t, makeMedia(t, ff, dir, "music.mp3", "-f", "lavfi", "-i", "sine=frequency=220:duration=12", "-c:a", "libmp3lame"))
	files := newSceneFiles(t)
	files.put("photo.jpg", mustRead(t, makeMedia(t, ff, dir, "photo.jpg", "-f", "lavfi", "-i", "testsrc=size=1000x1000:duration=1", "-frames:v", "1")))

	f := newFakeProviders(t)
	f.audio = cue
	el := newFakeEleven(t, "creator")
	el.collection, el.audio, el.seconds, el.music = ownerCollection(), voice, 2, music

	asm := NewAssembler(files, nil, nil)
	ads := NewAds(NewVoices(keysFor("elevenlabs", "fal"), files, nil), asm).WithClips(nil, files)
	req := AdRequest{TenantID: uuid.New(), VersionID: "ad1", Format: "1080x1920", Audio: AudioScore, Mood: "warm, hopeful",
		Photos: []string{files.srv.URL + "/photo.jpg"},
		Scenes: []ScriptScene{
			{ID: "s1", Shot: ShotProduct, Voiceover: "Soft skin all week.", SFX: "soft whoosh"},
			{ID: "s2", Shot: ShotEndCard, Voiceover: "Order on WhatsApp."},
		},
		EndCard: Overlay{Headline: "Shea butter", CTA: "Order on WhatsApp"}}
	res, err := ads.Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["elevenlabs/sound-effects/v3"] != 1 || el.musicBody == nil || !strings.Contains(el.musicBody["prompt"].(string), "warm, hopeful") ||
		!strings.Contains(el.musicBody["prompt"].(string), "instrumental, no vocals") {
		t.Fatalf("cue calls %v, music body %v", f.calls, el.musicBody)
	}
	if res.AudioLevel != AudioScore || res.MusicRoute != "elevenlabs/music" || res.AudioUSD <= 0 || res.Scenes[0].SFXKey == "" {
		t.Fatalf("result = %+v", res)
	}
	m, err := asm.measureLoudness(context.Background(), files.gotAd)
	if err != nil {
		t.Fatal(err)
	}
	if i := parseFloat(m.InputI); !(i >= -16 && i <= -12) {
		t.Fatalf("loudness %v LUFS, want about -14", i)
	}

	// Music fails on both routes: the ad is made without it, and it costs nothing.
	el.music = nil
	f.failFalPath = "elevenlabs/music"
	req.VersionID = "ad2"
	res2, err := NewAds(NewVoices(keysFor("elevenlabs", "fal"), files, nil), asm).WithClips(nil, files).Make(context.Background(), req)
	if err != nil {
		t.Fatalf("a failed score must not fail the ad: %v", err)
	}
	if res2.MusicRoute != "" || res2.AudioUSD > 0.01 {
		t.Fatalf("failed music charged or recorded: %+v", res2)
	}
	if _, err := os.Stat(files.gotAd); err != nil {
		t.Fatal(err)
	}
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

// Review fixes (2026-11-12): settings that cannot work are refused, the paid-part fallback
// levels, the audio charge key and the stable photo source.
func TestAudioReviewFixes(t *testing.T) {
	if _, err := AudioConfigFromEnv(func(k string) string {
		if k == "MF_AUDIO_MUSIC_GAIN" {
			return "NaN"
		}
		return ""
	}); err == nil {
		t.Error("a NaN gain must be refused")
	}
	if _, err := ParseDeliveryOverrides(`{"ad":{"similarity":0.9}}`); err == nil {
		t.Error("a misspelt delivery field must be refused")
	}
	if _, err := ParseDeliveryOverrides(`{"ad":{"similarity_boost":0.9,"use_speaker_boost":false}}`); err != nil {
		t.Errorf("valid delivery fields: %v", err)
	}
	for in, want := range map[string]string{AudioScore: AudioTrack, AudioSFX: AudioVoice, AudioTrack: AudioTrack, AudioVoice: AudioVoice, "odd": AudioVoice} {
		if got := AudioWithoutPaidParts(in); got != want {
			t.Errorf("AudioWithoutPaidParts(%q) = %q, want %q", in, got, want)
		}
	}
	if AudioChargeKey("render:v1:2") != "render:v1:2:audio" {
		t.Error("the audio key follows the render's charge key")
	}
	if stableSource("https://r2/x/a.jpg?X-Amz-Signature=1") != stableSource("https://r2/x/a.jpg?X-Amz-Signature=2") {
		t.Error("a presigned link must hash the same picture the same way")
	}
}
