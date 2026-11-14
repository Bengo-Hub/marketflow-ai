package media

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSpeakerTurns(t *testing.T) {
	sc := func(shot, voice, speaker string) ScriptScene {
		return ScriptScene{Shot: shot, Voiceover: voice, Speaker: speaker}
	}
	cases := []struct {
		name   string
		scenes []ScriptScene
		want   string
	}{
		{"named turns", []ScriptScene{sc(ShotBRoll, "Q?", "female"), sc(ShotProduct, "A.", "male"), sc(ShotEndCard, "Order.", "female")}, "female,male,female"},
		{"blanks take turns", []ScriptScene{sc(ShotBRoll, "Q?", "male"), sc(ShotProduct, "A.", ""), sc(ShotEndCard, "Order.", "")}, "male,female,male"},
		{"a leading blank leads into the first name", []ScriptScene{sc(ShotBRoll, "Hi.", ""), sc(ShotProduct, "Yes.", "female")}, "male,female"},
		{"silent scenes have no speaker", []ScriptScene{sc(ShotBRoll, "Q?", "female"), sc(ShotProduct, "[warm]", "male"), sc(ShotEndCard, "Order.", "")}, "female,,male"},
		{"presenter scenes share one speaker", []ScriptScene{sc(ShotPresenter, "Hi.", "female"), sc(ShotBRoll, "Look.", "male"), sc(ShotPresenter, "So.", "male")}, "female,male,female"},
		{"no speaker named is one voice", []ScriptScene{sc(ShotBRoll, "A.", ""), sc(ShotEndCard, "B.", "robot")}, ","},
	}
	for _, tc := range cases {
		if got := strings.Join(speakerTurns(tc.scenes), ","); got != tc.want {
			t.Errorf("%s: turns = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTwoVoiceOnlyForAllowedDeliveries(t *testing.T) {
	ads := NewAds(nil, nil)
	two := []ScriptScene{{Voiceover: "Q?", Speaker: "female"}, {Voiceover: "A.", Speaker: "male"}}
	one := []ScriptScene{{Voiceover: "Q?", Speaker: "female"}, {Voiceover: "A.", Speaker: "female"}}
	if !ads.twoVoice(AdRequest{Delivery: DeliveryUGC, Scenes: two}) {
		t.Fatal("a UGC script with both speakers is a two-voice ad")
	}
	for name, req := range map[string]AdRequest{
		"ad delivery":  {Delivery: DeliveryAd, Scenes: two},
		"no delivery":  {Scenes: two},
		"one speaker":  {Delivery: DeliveryUGC, Scenes: one},
		"story ad":     {Delivery: DeliveryStory, Scenes: two},
	} {
		if ads.twoVoice(req) {
			t.Errorf("%s must keep one voice", name)
		}
	}
	cfg := DefaultAudioConfig()
	cfg.TwoVoiceDeliveries = map[string]bool{}
	if NewAds(nil, nil).WithAudio(cfg).twoVoice(AdRequest{Delivery: DeliveryUGC, Scenes: two}) {
		t.Fatal("turning two voices off must keep UGC to one voice")
	}
}

// fakePicker picks from a fixed list: a named voice by id or name, else the first voice not
// excluded whose gender is unlabelled or the one asked for (as rankVoices filters).
type fakePicker struct{ voices []LibraryVoice }

func (f fakePicker) Pick(_ context.Context, req VoiceRequest) (LibraryVoice, bool) {
	if req.Voice != "" {
		for _, v := range f.voices {
			if v.ID == req.Voice || v.Name == req.Voice {
				return v, true
			}
		}
		return LibraryVoice{}, false
	}
	for _, v := range f.voices {
		if contains(req.Prefs.Exclude, v.ID) {
			continue
		}
		if req.Prefs.Gender != "" && v.Gender != "" && v.Gender != req.Prefs.Gender {
			continue
		}
		return v, true
	}
	return LibraryVoice{}, false
}

func TestCastVoices(t *testing.T) {
	yna := LibraryVoice{ID: "yna", Name: "Yna", Gender: "female"}
	lola := LibraryVoice{ID: "lola", Name: "Lola", Gender: "female"}
	juma := LibraryVoice{ID: "juma", Name: "Juma", Gender: "male"}
	plain := LibraryVoice{ID: "plain", Name: "Unlabelled"}
	ctx := context.Background()
	speakers := []string{"female", "male"}

	cast, note := castVoices(ctx, fakePicker{[]LibraryVoice{plain, yna, juma}}, AdRequest{}, speakers, nil)
	if note != "" || cast["female"] != "yna" || cast["male"] != "juma" {
		t.Fatalf("cast = %v, note %q (an unlabelled voice must be skipped)", cast, note)
	}
	// The owner's chosen voice reads for the speaker of its gender, even when it is not the lead.
	cast, note = castVoices(ctx, fakePicker{[]LibraryVoice{yna, lola, juma}}, AdRequest{Voice: "Juma"}, speakers, nil)
	if note != "" || cast["male"] != "juma" || cast["female"] != "yna" {
		t.Fatalf("owner's voice cast = %v, note %q", cast, note)
	}
	// A speaker whose reused scenes already have a voice keeps it.
	cast, _ = castVoices(ctx, fakePicker{[]LibraryVoice{yna, lola, juma}}, AdRequest{}, speakers, map[string]string{"female": "Lola"})
	if cast["female"] != "lola" || cast["male"] != "juma" {
		t.Fatalf("seeded cast = %v", cast)
	}
	// No male voice at all: one voice reads the ad, and the note says why.
	if cast, note = castVoices(ctx, fakePicker{[]LibraryVoice{yna, lola, plain}}, AdRequest{}, speakers, nil); note == "" || cast != nil {
		t.Fatalf("no male voice: cast %v, note %q", cast, note)
	}
	if _, note = castVoices(ctx, fakePicker{[]LibraryVoice{yna}}, AdRequest{}, []string{"female"}, nil); note == "" {
		t.Fatal("one speaker is not a two-voice cast")
	}
}

// The voices are laid end to end in timeline order: each voiced scene's span starts where the
// scenes before it end, silent scenes keep their time, and the speakers alternate as written.
func TestVoiceTimeline(t *testing.T) {
	scenes := []AdScene{
		{Seconds: 2, AudioURL: "a1", Speaker: "female"},
		{Seconds: 3, AudioURL: "a2", Speaker: "male"},
		{Seconds: 1.5},
		{Seconds: 4.3, AudioURL: "a4", Speaker: "female"},
	}
	got := voiceTimeline(scenes)
	want := []VoiceSpan{{"female", 0, 2}, {"male", 2, 5}, {"female", 6.5, 10.8}}
	if len(got) != len(want) {
		t.Fatalf("timeline = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if sceneSpeakers(scenes) != 2 || sceneSpeakers(scenes[:1]) != 1 || sceneSpeakers([]AdScene{{AudioURL: "x"}}) != 0 {
		t.Fatal("sceneSpeakers miscounts")
	}
}

func TestMatchGain(t *testing.T) {
	cases := []struct {
		in     string
		target float64
		gain   float64
		ok     bool
	}{
		{"-24.5", -18, 6.5, true},
		{"-14.0", -18, -4, true},
		{"-18.2", -18, 0, false}, // under half a dB: left alone
		{"-40", -18, 12, true},   // capped
		{"-inf", -18, 0, false},
		{"-80", -18, 0, false},
		{"x", -18, 0, false},
		{"-24", 0, 0, false}, // matching off
	}
	for _, c := range cases {
		g, ok := matchGain(c.in, c.target)
		if ok != c.ok || g != c.gain {
			t.Errorf("matchGain(%q, %g) = %g, %v; want %g, %v", c.in, c.target, g, ok, c.gain, c.ok)
		}
	}
}

func TestActorWithGender(t *testing.T) {
	cases := []struct{ desc, speaker, want string }{
		{"a friendly Kenyan presenter", "male", "a friendly Kenyan presenter, a man"},
		{"a shop manager in her forties", "male", "a shop manager in her forties"},
		{"a shop manager", "female", "a shop manager, a woman"},
		{"a young woman in a salon", "male", "a young woman in a salon"},
		{"a friendly presenter", "", "a friendly presenter"},
	}
	for _, c := range cases {
		if got := actorWithGender(c.desc, c.speaker); got != c.want {
			t.Errorf("actorWithGender(%q, %q) = %q, want %q", c.desc, c.speaker, got, c.want)
		}
	}
	scenes := []ScriptScene{{Shot: ShotBRoll, Voiceover: "Q?", Speaker: "male"}, {Shot: ShotPresenter, Voiceover: "A.", Speaker: ""}}
	if got := presenterSpeaker(scenes); got != "female" {
		t.Fatalf("presenter speaker = %q", got)
	}
}

// ActorGender reads the saved actor's look, so later scripts give its lines the matching voice.
func TestActorGender(t *testing.T) {
	cases := map[string]string{
		"a friendly Kenyan presenter, a man":   "male",
		"a shop manager in her forties":        "female",
		"mwanamke anayeuza nguo":               "female",
		"a friendly presenter":                 "",
		"a man and a woman behind the counter": "",
	}
	for desc, want := range cases {
		if got := ActorGender(desc); got != want {
			t.Errorf("ActorGender(%q) = %q, want %q", desc, got, want)
		}
	}
}

func TestSceneHashSpeaker(t *testing.T) {
	req := AdRequest{Kind: AdTemplate, Delivery: DeliveryUGC}
	s := ScriptScene{ID: "s1", Shot: ShotProduct, Voiceover: "Hello."}
	sp := ScenePlan{Work: WorkStill}
	h := sceneHash(req, s, sp, "d")
	if sceneHashFor(req, s, sp, "d", "") != h {
		t.Fatal("a one-voice scene must keep its hash")
	}
	if sceneHashFor(req, s, sp, "d", "male") == h || sceneHashFor(req, s, sp, "d", "male") == sceneHashFor(req, s, sp, "d", "female") {
		t.Fatal("the speaker must change a two-voice scene's hash")
	}
}

func TestTwoVoiceAudioConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	def, err := AudioConfigFromEnv(env(nil))
	if err != nil || !def.TwoVoiceDeliveries[DeliveryUGC] || len(def.TwoVoiceDeliveries) != 1 || def.VoiceMatchLUFS != -18 {
		t.Fatalf("defaults = %+v, %v", def, err)
	}
	cfg, err := AudioConfigFromEnv(env(map[string]string{"MF_AD_TWO_VOICE_DELIVERIES": "ugc, story", "MF_VOICE_MATCH_LUFS": "-20",
		"MF_ELEVENLABS_MALE_VOICES": "outside1, Juma", "MF_ELEVENLABS_FEMALE_VOICES": ""}))
	if err != nil || !cfg.TwoVoiceDeliveries[DeliveryStory] || cfg.VoiceMatchLUFS != -20 ||
		strings.Join(cfg.SpeakerVoices["male"], ",") != "outside1,Juma" || len(cfg.SpeakerVoices["female"]) != 0 {
		t.Fatalf("overridden = %+v, %v", cfg, err)
	}
	off, err := AudioConfigFromEnv(env(map[string]string{"MF_AD_TWO_VOICE_DELIVERIES": "off", "MF_VOICE_MATCH_LUFS": "off"}))
	if err != nil || len(off.TwoVoiceDeliveries) != 0 || off.VoiceMatchLUFS != 0 {
		t.Fatalf("off = %+v, %v", off, err)
	}
	bad, err := AudioConfigFromEnv(env(map[string]string{"MF_AD_TWO_VOICE_DELIVERIES": "podcast", "MF_VOICE_MATCH_LUFS": "-5"}))
	if err == nil || !bad.TwoVoiceDeliveries[DeliveryUGC] || bad.VoiceMatchLUFS != -18 {
		t.Fatalf("bad values must keep the defaults and be reported: %+v %v", bad, err)
	}
}

func TestValidateSpeaker(t *testing.T) {
	ads := NewAds(nil, nil)
	req := AdRequest{Format: "1080x1920", Photos: []string{"https://x/p.jpg"}, Scenes: []ScriptScene{
		{ID: "s1", Shot: ShotProduct, Voiceover: "Hello.", Speaker: "female"},
		{ID: "s2", Shot: ShotEndCard, Voiceover: "Order.", Speaker: "Male"}}}
	if err := ads.Validate(req); err != nil {
		t.Fatalf("a valid two-voice script was refused: %v", err)
	}
	req.Scenes[1].Speaker = "robot"
	if err := ads.Validate(req); err == nil {
		t.Fatal("an unknown speaker must be refused")
	}
}

// A UGC ad written for two voices is read by a woman and a man taking turns, in timeline
// order, from the account's voices; the same script on another delivery has one voice.
func TestAdsMakeTwoVoices(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	voice := mustRead(t, makeMedia(t, ff, dir, "voice.mp3", "-f", "lavfi", "-i", "sine=frequency=330:duration=2", "-c:a", "libmp3lame"))
	files := newSceneFiles(t)
	files.put("photo.jpg", mustRead(t, makeMedia(t, ff, dir, "photo.jpg", "-f", "lavfi", "-i", "testsrc=size=1000x1000:duration=1", "-frames:v", "1")))
	el := newFakeEleven(t, "creator")
	el.collection, el.audio, el.seconds = ownerCollection(), voice, 2

	asm := NewAssembler(files, nil, nil)
	ads := NewAds(NewVoices(keysFor("elevenlabs"), files, nil), asm).WithClips(nil, files)
	req := AdRequest{TenantID: uuid.New(), VersionID: "two1", Format: "1080x1920", Delivery: DeliveryUGC,
		Photos: []string{files.srv.URL + "/photo.jpg"},
		Scenes: []ScriptScene{
			{ID: "s1", Shot: ShotProduct, Voiceover: "[curious] Is a till app too dear for a duka?", Speaker: "female"},
			{ID: "s2", Shot: ShotProduct, Voiceover: "[reassuring] Not this one.", Speaker: "male"},
			{ID: "s3", Shot: ShotEndCard, Voiceover: "WhatsApp DEMO to try it."},
		},
		EndCard: Overlay{Headline: "Duka till", CTA: "WhatsApp DEMO"}}
	res, err := ads.Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.TwoVoiceNote != "" || res.Voices["female"] == "" || res.Voices["male"] == "" || res.Voices["female"] == res.Voices["male"] {
		t.Fatalf("voices = %v, note %q", res.Voices, res.TwoVoiceNote)
	}
	if !strings.HasPrefix(res.Voices["male"], "Juma") || !strings.HasPrefix(res.Voices["female"], "Yna") {
		t.Fatalf("voices = %v, want Yna and Juma from the account", res.Voices)
	}
	wantSpeakers := []string{"female", "male", "female"}
	for i, s := range res.Scenes {
		if s.Speaker != wantSpeakers[i] || s.Voice != res.Voices[wantSpeakers[i]] {
			t.Errorf("scene %d read by %q as %q, want %q", i+1, s.Voice, s.Speaker, wantSpeakers[i])
		}
	}
	if len(res.VoiceTimeline) != 3 {
		t.Fatalf("timeline = %+v", res.VoiceTimeline)
	}
	for i, span := range res.VoiceTimeline {
		if span.Speaker != wantSpeakers[i] || (i > 0 && span.Start != res.VoiceTimeline[i-1].End) {
			t.Errorf("span %d = %+v (timeline %+v)", i, span, res.VoiceTimeline)
		}
	}
	if m, err := asm.measureLoudness(context.Background(), files.gotAd); err != nil || parseFloat(m.InputI) < -16 || parseFloat(m.InputI) > -12 {
		t.Fatalf("loudness %+v, %v; want about -14 LUFS", m, err)
	}

	// The same script on the ad delivery is one voice: no speakers, one voice throughout.
	req.VersionID, req.Delivery = "one1", DeliveryAd
	res, err = ads.Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Voices != nil || res.VoiceTimeline != nil {
		t.Fatalf("a one-voice ad reported voices %v", res.Voices)
	}
	for i, s := range res.Scenes {
		if s.Speaker != "" || s.Voice != res.Scenes[0].Voice {
			t.Errorf("scene %d: speaker %q voice %q, want one voice", i+1, s.Speaker, s.Voice)
		}
	}
}
