package media

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The voice explorer searches ElevenLabs' shared library with the platform key and passes the
// filters through (owner, 2026-11-12: East African male voices for the bake-off).
func TestSearchLibrary(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/shared-voices" {
			http.NotFound(w, r)
			return
		}
		gotKey, gotQuery = r.Header.Get("xi-api-key"), r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"voices": []map[string]any{
			{"voice_id": "v1", "public_owner_id": "o1", "name": "Juma", "gender": "male", "accent": "kenyan", "use_case": "advertisement", "cloned_by_count": 12},
		}})
	}))
	defer srv.Close()
	old := elevenBase
	elevenBase = srv.URL
	t.Cleanup(func() { elevenBase = old })

	v := NewVoices(keysFor("elevenlabs"), &memAudio{}, nil)
	got, err := v.SearchLibrary(context.Background(), LibraryQuery{Gender: "Male", Accent: "Kenyan", UseCase: "advertisement", Search: "warm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Juma" || got[0].PublicOwnerID != "o1" {
		t.Fatalf("voices %+v", got)
	}
	if gotKey == "" {
		t.Fatal("the platform key must be sent")
	}
	for _, want := range []string{"gender=male", "accent=kenyan", "use_cases=advertisement", "search=warm", "page_size=30"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}

	none := NewVoices(keysFor(), &memAudio{}, nil)
	if _, err := none.SearchLibrary(context.Background(), LibraryQuery{}); !errors.Is(err, ErrNoVoiceKey) {
		t.Fatalf("no key: %v", err)
	}
}
