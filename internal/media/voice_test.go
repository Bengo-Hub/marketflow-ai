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

// ownerCollection mirrors the owner's codevertex-voices collection read on 2026-10-26.
func ownerCollection() []map[string]any {
	v4 := []string{"eleven_multilingual_v2", "eleven_v4", "eleven_v4_turbo"}
	return []map[string]any{
		{"voice_id": "VUGQSU6BSEjkbudnJbOj", "name": "Emily - Confident and Steady", "high_quality_base_model_ids": v4,
			"labels": map[string]string{"language": "en", "accent": "american", "gender": "female"}},
		{"voice_id": "CXoGcuszI2UkuF6sqV8W", "name": "Emily E.-Premium Female Commercial Voice", "high_quality_base_model_ids": v4,
			"labels": map[string]string{"language": "en", "accent": "british", "gender": "female"}},
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
		case strings.HasSuffix(r.URL.Path, "/with-timestamps"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.got, f.path, f.key = body, r.URL.Path+"?"+r.URL.RawQuery, r.Header.Get("xi-api-key")
			f.mu.Unlock()
			text := body["text"].(string)
			step := 0.1
			if f.seconds > 0 {
				step = f.seconds / float64(len([]rune(text)))
			}
			var chars []string
			var starts, ends []float64
			i := 0
			for _, c := range text {
				chars = append(chars, string(c))
				starts = append(starts, float64(i)*step)
				ends = append(ends, float64(i)*step+step)
				i++
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
	// "Soft skin. Order today." is four words; "skin." starts at character 5.
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	if len(res.Words) != 4 || res.Words[1].Text != "skin." || !near(res.Words[1].Start, 0.5) || !near(res.Words[3].End, 2.3) || !near(res.Seconds, 2.3) {
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

	// With the owner's collection, Emily E. (British, female) becomes the closest
	// premade voice on fal's route, which may not hold library voices.
	el.collection = ownerCollection()
	res, err = NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil).Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v2", Text: "Soft skin"})
	if err != nil || f.inputs["fal-ai/elevenlabs/tts/eleven-v3"]["voice"] != "Alice" || res.Voice != "Alice" {
		t.Fatalf("fal voice for Emily E.: %v %v %+v", err, f.inputs["fal-ai/elevenlabs/tts/eleven-v3"], res)
	}
}

// With no voice named, the owner's collection decides: Emily E. reads it on
// the direct route, in English or Swahili (a v4 multilingual model); fal's
// route, which may not hold a library voice, gets the closest premade one.
func TestVoiceDefaultsToEmilyEFromTheCollection(t *testing.T) {
	newFakeProviders(t)
	el := newFakeEleven(t, "creator")
	el.collection = ownerCollection()
	v := NewVoices(keysFor("elevenlabs", "fal"), &memAudio{}, nil)
	for _, lang := range []string{"en", "sw"} {
		res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Karibu Codevertex", Language: lang})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(el.path, "/v1/text-to-speech/CXoGcuszI2UkuF6sqV8W/") || !strings.HasPrefix(res.Voice, "Emily E.") {
			t.Fatalf("%s: voice %q, path %s", lang, res.Voice, el.path)
		}
	}
	// A voice named by a prefix of its name, from the collection.
	if res, err := v.Make(context.Background(), VoiceRequest{TenantID: uuid.New(), VersionID: "v1", Text: "Hello", Voice: "emily - confident"}); err != nil ||
		!strings.Contains(el.path, "/VUGQSU6BSEjkbudnJbOj/") || !strings.HasPrefix(res.Voice, "Emily - Confident") {
		t.Fatalf("named voice: %v %+v %s", err, res, el.path)
	}
	if _, err := v.Make(context.Background(), VoiceRequest{Text: "Hello", Voice: "Nobody"}); !errors.Is(err, ErrVoiceInput) {
		t.Fatalf("unknown voice: %v", err)
	}
	// The voices the studio offers: the collection first, then the premade ones.
	list := v.List(context.Background())
	if len(list) != 2+len(VoiceCatalog) || list[0].ID != "VUGQSU6BSEjkbudnJbOj" || !list[len(list)-1].Premade {
		t.Fatalf("list = %+v", list)
	}
}

func TestChooseVoiceOrder(t *testing.T) {
	coll := []LibraryVoice{
		{ID: "a", Name: "Zawadi", Language: "sw", Models: []string{"eleven_v4_turbo"}},
		{ID: "b", Name: "Emily - Confident and Steady", Language: "en", Models: []string{"eleven_v4_turbo"}},
	}
	// No Emily E.: another Emily.
	if v, _ := chooseVoice(coll, "", "en", "eleven_v4_turbo"); v.ID != "b" {
		t.Fatalf("got %+v", v)
	}
	// A model the Emily voice is not tuned for: the next voice that can read it.
	if v, _ := chooseVoice([]LibraryVoice{{ID: "b", Name: "Emily", Models: []string{"eleven_v3"}}, {ID: "c", Name: "Juma"}}, "", "en", "eleven_v4_turbo"); v.ID != "c" {
		t.Fatalf("got %+v", v)
	}
	// An empty collection: the premade default.
	if v, _ := chooseVoice(nil, "", "en", "eleven_v4_turbo"); v.Name != DefaultVoice || !v.Premade {
		t.Fatalf("got %+v", v)
	}
	if falVoiceFor(LibraryVoice{Name: "Emily E.", Accent: "british", Gender: "female"}) != "Alice" ||
		falVoiceFor(LibraryVoice{Name: "Juma", Gender: "male"}) != "Brian" || falVoiceFor(LibraryVoice{Name: "Sarah", Premade: true}) != "Sarah" {
		t.Fatal("fal voice mapping")
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
