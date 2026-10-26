package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeEleven serves ElevenLabs' subscription and text-to-speech-with-timestamps
// endpoints in the shape read live on 2026-10-24.
type fakeEleven struct {
	srv  *httptest.Server
	tier string
	mu   sync.Mutex
	got  map[string]any
	path string
	key  string
	// collection holds the codevertex-voices collection's voices (nil: no collection).
	collection []map[string]any
	// audio replaces the placeholder bytes (a real MP3 for assembly tests);
	// seconds spreads the character timings over its length.
	audio   []byte
	seconds float64
}

// ownerCollection mirrors part of the owner's codevertex-voices collection
// (labels as read on 2026-10-27).
func ownerCollection() []map[string]any {
	v4 := []string{"eleven_multilingual_v2", "eleven_v4", "eleven_v4_turbo"}
	voice := func(id, name, accent, gender, useCase, descriptive string) map[string]any {
		return map[string]any{"voice_id": id, "name": name, "category": "professional", "high_quality_base_model_ids": v4,
			"labels": map[string]string{"language": "en", "accent": accent, "gender": gender, "use_case": useCase, "descriptive": descriptive}}
	}
	return []map[string]any{
		voice("ZmIZJsdxTxo4PlEu2rjh", "Lola - Calm, Warm and Natural", "nigerian", "female", "social_media", "calm"),
		voice("ijKilL5CnjXKMWDHOJH8", "Yna Agalo - Kenyan Lady", "received pronunciation", "female", "narrative_story", "confident"),
		voice("VUGQSU6BSEjkbudnJbOj", "Emily - Confident and Steady", "american", "female", "informative_educational", "professional"),
		voice("CXoGcuszI2UkuF6sqV8W", "Emily E.-Premium Female Commercial Voice", "british", "female", "advertisement", "calm"),
	}
}

func newFakeEleven(t *testing.T, tier string) *fakeEleven {
	f := &fakeEleven{tier: tier}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/user/subscription":
			_ = json.NewEncoder(w).Encode(map[string]any{"tier": f.tier, "character_limit": 10000})
		case r.URL.Path == "/v1/voices/collections" && f.collection != nil:
			_ = json.NewEncoder(w).Encode(map[string]any{"collections": []map[string]any{
				{"collection_id": "other1", "title": "someone-else"},
				{"collection_id": "vMfi9czGSDnodSoI3y1C", "title": "codevertex-voices"}}})
		case r.URL.Path == "/v2/voices" && r.URL.Query().Get("collection_id") == "vMfi9czGSDnodSoI3y1C":
			_ = json.NewEncoder(w).Encode(map[string]any{"voices": f.collection})
		case r.URL.Path == "/v2/voices" && f.collection != nil:
			// The whole account: the collection plus a voice outside it.
			all := append([]map[string]any{{"voice_id": "outside1", "name": "Juma - Warm Radio Host", "category": "professional",
				"labels": map[string]string{"language": "en", "accent": "kenyan", "gender": "male", "use_case": "advertisement", "descriptive": "warm"}}},
				f.collection...)
			_ = json.NewEncoder(w).Encode(map[string]any{"voices": all})
		case strings.HasSuffix(r.URL.Path, "/with-timestamps"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.got, f.path, f.key = body, r.URL.Path+"?"+r.URL.RawQuery, r.Header.Get("xi-api-key")
			f.mu.Unlock()
			// Spoken characters take time; a bracketed direction is not spoken, so
			// its characters take none (it is aligned at the current moment).
			text := body["text"].(string)
			spoken := []rune(stripTags(text))
			step := 0.1
			if f.seconds > 0 && len(spoken) > 0 {
				step = f.seconds / float64(len(spoken))
			}
			var chars []string
			var starts, ends []float64
			t0, depth := 0.0, 0
			for _, c := range text {
				chars = append(chars, string(c))
				switch {
				case c == '[':
					depth++
				case c == ']' && depth > 0:
					depth--
				}
				if depth > 0 || c == ']' {
					starts, ends = append(starts, t0), append(ends, t0)
					continue
				}
				starts, ends = append(starts, t0), append(ends, t0+step)
				t0 += step
			}
			audio := []byte("ID3fake-mp3")
			if f.audio != nil {
				audio = f.audio
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"audio_base64": base64.StdEncoding.EncodeToString(audio),
				"alignment":    map[string]any{"characters": chars, "character_start_times_seconds": starts, "character_end_times_seconds": ends},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	prev := elevenBase
	elevenBase = f.srv.URL
	t.Cleanup(func() { elevenBase = prev })
	return f
}

type memAudio struct {
	mu   sync.Mutex
	puts map[string][]byte
}

func (s *memAudio) PutAudio(_ context.Context, tenantID uuid.UUID, versionID, name string, data []byte) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.puts == nil {
		s.puts = map[string][]byte{}
	}
	key := "marketflow/" + tenantID.String() + "/audio-drafts/voice/" + versionID + "-" + name + ".mp3"
	s.puts[key] = data
	return key, "https://media.example/" + key, nil
}

// On a paid ElevenLabs plan the direct route reads the text with exact word timings.
func TestVoiceDirectOnPaidPlan(t *testing.T) {
	newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	store := &memAudio{}
	v := NewVoices(keysFor("elevenlabs", "fal"), store, nil)
	res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Soft skin. Order today.", Language: "sw", Voice: "Brian"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "elevenlabs/eleven-v4-turbo" || !res.Exact || res.Characters != 23 || len(store.puts) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if el.got["model_id"] != "eleven_v4_turbo" || el.got["language_code"] != "sw" || el.key != "elevenlabs-id:secret" ||
		!strings.Contains(el.path, "/v1/text-to-speech/"+VoiceCatalog["Brian"]+"/with-timestamps") || !strings.Contains(el.path, "output_format=mp3_44100_128") {
		t.Fatalf("elevenlabs call: %v %s", el.got, el.path)
	}
	// "Soft skin. Order today." is four words after the (unspoken) direction tag;
	// "skin." starts 5 characters after "Soft", and the audio ends with "today.".
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	w := res.Words
	if len(w) != 4 || w[0].Text != "Soft" || w[1].Text != "skin." || !near(w[1].Start-w[0].Start, 0.5) ||
		!near(w[3].End-w[0].Start, 2.3) || !near(res.Seconds, w[3].End) {
		t.Fatalf("words = %+v, seconds %v", res.Words, res.Seconds)
	}
	if res.CostUSD < 0.00091 || res.CostUSD > 0.00093 {
		t.Fatalf("cost = %v", res.CostUSD)
	}
}

// On the free plan ElevenLabs direct is never used (non-commercial output):
// fal's ElevenLabs route reads it, with estimated timings.
func TestVoiceFreePlanUsesFal(t *testing.T) {
	f := newFakeProviders(t)
	el := newFakeEleven(t, "free")
	v := NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Soft skin all week"})
	if err != nil {
		t.Fatal(err)
	}
	if el.got != nil {
		t.Fatal("ElevenLabs direct was called on the free plan")
	}
	in := f.inputs["fal-ai/elevenlabs/tts/eleven-v3"]
	if res.Route != "fal/elevenlabs-v3" || res.Exact || in == nil || in["voice"] != DefaultVoice || in["language_code"] != "en" {
		t.Fatalf("result = %+v, input %v", res, in)
	}
	// 32,000 bytes at 128 kbit/s is 2 s, spread over four words.
	if res.Seconds != 2 || len(res.Words) != 4 || res.Words[3].End < 1.99 {
		t.Fatalf("estimated timings: %v %+v", res.Seconds, res.Words)
	}

	// With the owner's collection the default is Yna Agalo (female, not British),
	// so fal's route, which may not hold library voices, gets Sarah; Emily E.
	// (British, female) named outright gets Alice.
	el.collection = ownerCollection()
	v = NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	res, err = v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v2", Text: "Soft skin"})
	if err != nil || f.inputs["fal-ai/elevenlabs/tts/eleven-v3"]["voice"] != "Sarah" || res.Voice != "Sarah" {
		t.Fatalf("fal voice for Yna Agalo: %v %v %+v", err, f.inputs["fal-ai/elevenlabs/tts/eleven-v3"], res)
	}
	res, err = v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v3", Text: "Soft skin", Voice: "Emily E"})
	if err != nil || f.inputs["fal-ai/elevenlabs/tts/eleven-v3"]["voice"] != "Alice" || res.Voice != "Alice" {
		t.Fatalf("fal voice for Emily E.: %v %v %+v", err, f.inputs["fal-ai/elevenlabs/tts/eleven-v3"], res)
	}
	if in := f.inputs["fal-ai/elevenlabs/tts/eleven-v3"]; in["stability"] != 0.35 || !strings.HasPrefix(in["text"].(string), "[") {
		t.Fatalf("fal gets the delivery's stability and direction: %v", in)
	}
}

// With no voice named, the delivery picks from the whole account with the
// collection and the default list preferred: Yna Agalo (the owner's default)
// reads an ordinary ad, in English or Swahili (a v4 multilingual model), with
// the ad delivery's settings and direction tag, which never reaches the timings.
func TestVoiceDefaultAndDelivery(t *testing.T) {
	newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	el.collection = ownerCollection()
	v := NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	for _, lang := range []string{"en", "sw"} {
		res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Karibu Codevertex", Language: lang})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(el.path, "/v1/text-to-speech/ijKilL5CnjXKMWDHOJH8/") || !strings.HasPrefix(res.Voice, "Yna Agalo") {
			t.Fatalf("%s: voice %q, path %s", lang, res.Voice, el.path)
		}
		settings, _ := el.got["voice_settings"].(map[string]any)
		if !strings.HasPrefix(el.got["text"].(string), Deliveries[DeliveryAd].Direction) || settings["stability"] != 0.35 || settings["speed"] != 1.05 {
			t.Fatalf("ad delivery not sent: %v", el.got)
		}
		if len(res.Words) != 2 || res.Words[0].Text != "Karibu" {
			t.Fatalf("the direction tag reached the timings: %+v", res.Words)
		}
	}
	// A UGC read with a Nigerian accent asked for: content fit beats the default.
	if res, _ := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Hello there",
		Delivery: DeliveryUGC, Prefs: VoicePrefs{Accent: "nigerian"}}); res == nil || !strings.HasPrefix(res.Voice, "Lola") {
		t.Fatalf("ugc, nigerian: %+v", res)
	}
	// A male Kenyan voice asked for: one outside the collection is chosen, since
	// the collection is preferred, never required.
	if res, _ := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Hello there",
		Prefs: VoicePrefs{Gender: "male", Accent: "kenyan"}}); res == nil || !strings.HasPrefix(res.Voice, "Juma") {
		t.Fatalf("male kenyan: %+v", res)
	}
	// A different voice per variant: the default excluded, Emily E. is next.
	if res, _ := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Hello there",
		Prefs: VoicePrefs{Exclude: []string{"ijKilL5CnjXKMWDHOJH8"}}}); res == nil || !strings.HasPrefix(res.Voice, "Emily E.") {
		t.Fatalf("excluded default: %+v", res)
	}
	// A voice named by a prefix of its name, and unknown names and deliveries.
	if res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Hello", Voice: "emily - confident"}); err != nil ||
		!strings.Contains(el.path, "/VUGQSU6BSEjkbudnJbOj/") || !strings.HasPrefix(res.Voice, "Emily - Confident") {
		t.Fatalf("named voice: %v %+v %s", err, res, el.path)
	}
	for _, bad := range []VoiceRequest{{Text: "Hello", Voice: "Nobody"}, {Text: "Hello", Delivery: "opera"}} {
		if _, err := v.Make(context.Background(), bad); !errors.Is(err, ErrVoiceInput) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	// The voices the studio offers: the collection first, then the rest of the
	// account, then the catalogued premade voices.
	list := v.List(context.Background())
	if len(list) != 5+len(VoiceCatalog) || !list[0].InCollection || list[4].ID != "outside1" || list[4].InCollection || !list[len(list)-1].Premade {
		t.Fatalf("list = %+v", list)
	}
}

func TestRankVoicesAndTags(t *testing.T) {
	all := []LibraryVoice{
		{ID: "b", Name: "Emily", Models: []string{"eleven_v3"}, InCollection: true},
		{ID: "c", Name: "Juma"},
	}
	// A model the default voice is not tuned for: the next voice that can read it.
	if r := rankVoices(all, DeliveryFor(""), VoicePrefs{}, "en", "eleven_v4_turbo", []string{"Emily"}); len(r) != 1 || r[0].ID != "c" {
		t.Fatalf("got %+v", r)
	}
	// Nothing at all: the premade default.
	if v, ok := pickVoice(nil, VoiceChoice{Delivery: DeliveryFor("")}, "en", "eleven_v4_turbo"); !ok || !v.Premade {
		t.Fatalf("got %+v", v)
	}
	if falVoiceFor(LibraryVoice{Name: "Emily E.", Accent: "british", Gender: "female"}) != "Alice" ||
		falVoiceFor(LibraryVoice{Name: "Juma", Gender: "male"}) != "Brian" || falVoiceFor(LibraryVoice{Name: "Sarah", Premade: true}) != "Sarah" {
		t.Fatal("fal voice mapping")
	}
	if got := stripTags("[Upbeat, warm] Soft skin [short pause] all week."); got != "Soft skin all week." {
		t.Fatalf("stripTags = %q", got)
	}
	if directed(DeliveryFor(DeliveryKids), "[excited] Hi") != "[excited] Hi" {
		t.Fatal("a script's own opening tag must be kept")
	}
	// A tag in the alignment is skipped when words are grouped.
	chars := strings.Split("[x y] Hi you", "")
	starts, ends := make([]float64, len(chars)), make([]float64, len(chars))
	for i := range chars {
		starts[i], ends[i] = float64(i), float64(i)+1
	}
	if w := wordsFromChars(chars, starts, ends); len(w) != 2 || w[0].Text != "Hi" || w[1].Text != "you" {
		t.Fatalf("words = %+v", w)
	}
}

func TestVoiceRejectsBadInput(t *testing.T) {
	v := NewVoices(keysFor("fal"), &memAudio{}, nil)
	for _, req := range []VoiceRequest{
		{Text: " "},
		{Text: strings.Repeat("a", MaxVoiceChars+1)},
		{Text: "hi", Language: "fr"},
		{Text: "hi", Voice: "Nobody"},
	} {
		if _, err := v.Make(context.Background(), req); !errors.Is(err, ErrVoiceInput) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

// VEED Fabric gets the confirmed shape; when it fails, Creatify Aurora at 480p
// takes over, and the price is per whole second.
func TestPresenterShapesAndFallback(t *testing.T) {
	f := newFakeProviders(t)
	p := NewPresenters(keysFor("fal"), nil)
	req := PresenterRequest{TenantID: uuid.New(), VersionID: "v1", ImageURL: "https://cdn.example/avatar.png", AudioURL: "https://cdn.example/voice.mp3", Seconds: 7.2}
	res, err := p.Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	in := f.inputs["veed/fabric-1.0"]
	if res.Route != "fal/veed-fabric" || in["image_url"] != req.ImageURL || in["audio_url"] != req.AudioURL || in["resolution"] != "720p" {
		t.Fatalf("fabric: %+v %v", res, in)
	}
	if res.CostUSD < 0.639 || res.CostUSD > 0.641 || !strings.HasSuffix(res.URL, "/clip.mp4") {
		t.Fatalf("fabric result: %+v", res)
	}

	f.failFalPath = "veed/"
	res, err = NewPresenters(keysFor("fal"), nil).Make(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != "fal/creatify-aurora-480p" || f.inputs["fal-ai/creatify/aurora"]["resolution"] != "480p" || res.Attempts != 2 {
		t.Fatalf("fallback: %+v %v", res, f.inputs["fal-ai/creatify/aurora"])
	}
	if _, err := p.Make(context.Background(), PresenterRequest{ImageURL: "file:///x", AudioURL: req.AudioURL, Seconds: 3}); err == nil {
		t.Fatal("a non-http image must be refused")
	}
}
