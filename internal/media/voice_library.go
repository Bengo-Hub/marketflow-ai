package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// The studio's voices are the platform owner's ElevenLabs collection (named by
// VoiceCollection, "codevertex-voices" by default): voices added there are
// offered within libraryTTL, with no code change. The default is Emily E., then
// any other Emily that speaks the language, then any voice in the collection,
// then the premade voices in VoiceCatalog. Library voices work only on a paid
// ElevenLabs plan ("Free users cannot use library voices via the API",
// 2026-10-26), which the direct route already requires.

// DefaultVoiceCollection is the owner's ElevenLabs voice collection.
const DefaultVoiceCollection = "codevertex-voices"

// preferredVoices are tried in order for a request that names no voice: a name
// prefix, case-insensitive.
var preferredVoices = []string{"Emily E", "Emily"}

// libraryTTL: the owner adds voices now and then, not by the minute.
const libraryTTL = time.Hour

// LibraryVoice is one voice of the collection.
type LibraryVoice struct {
	ID       string   `json:"voice_id"`
	Name     string   `json:"name"`
	Language string   `json:"language"` // the voice's own language label, e.g. en
	Accent   string   `json:"accent,omitempty"`
	Gender   string   `json:"gender,omitempty"`
	Models   []string `json:"models,omitempty"` // models it is tuned for
	Premade  bool     `json:"premade,omitempty"`
}

// speaks reports whether the voice can read lang with model: its own language,
// or any language through a multilingual model it supports.
func (v LibraryVoice) speaks(lang, model string) bool {
	if model != "" && len(v.Models) > 0 && !contains(v.Models, model) {
		return false
	}
	return v.Language == "" || v.Language == lang || multilingual(model)
}

// multilingual models read Swahili as well as English (eleven_v4 lists 94 languages).
func multilingual(model string) bool {
	return strings.HasPrefix(model, "eleven_v4") || strings.HasPrefix(model, "eleven_multilingual")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// voiceLibrary lists the collection, cached per ElevenLabs key.
type voiceLibrary struct {
	collection string
	http       *http.Client

	mu    sync.Mutex
	cache map[string]libraryEntry
}

type libraryEntry struct {
	voices []LibraryVoice
	at     time.Time
}

func newVoiceLibrary(collection string) *voiceLibrary {
	if collection == "" {
		collection = DefaultVoiceCollection
	}
	return &voiceLibrary{collection: collection, http: &http.Client{Timeout: 10 * time.Second}, cache: map[string]libraryEntry{}}
}

// voices returns the collection's voices; a failed read keeps the last good
// list, and with none the premade voices still serve.
func (l *voiceLibrary) voices(ctx context.Context, key string) []LibraryVoice {
	if key == "" {
		return nil
	}
	l.mu.Lock()
	e, ok := l.cache[key]
	l.mu.Unlock()
	if ok && time.Since(e.at) < libraryTTL {
		return e.voices
	}
	list, err := l.fetch(ctx, key)
	if err != nil {
		list = e.voices // keep the last good list (nil on the first failure)
	}
	l.mu.Lock()
	l.cache[key] = libraryEntry{voices: list, at: time.Now()}
	l.mu.Unlock()
	return list
}

func (l *voiceLibrary) fetch(ctx context.Context, key string) ([]LibraryVoice, error) {
	var colls struct {
		Collections []struct {
			ID    string `json:"collection_id"`
			Title string `json:"title"`
		} `json:"collections"`
	}
	if err := l.get(ctx, key, "/v1/voices/collections", &colls); err != nil {
		return nil, err
	}
	id := ""
	for _, c := range colls.Collections {
		if strings.EqualFold(c.Title, l.collection) || c.ID == l.collection {
			id = c.ID
		}
	}
	if id == "" {
		return nil, nil // no such collection: the premade voices serve
	}
	var page struct {
		Voices []struct {
			ID     string            `json:"voice_id"`
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
			Models []string          `json:"high_quality_base_model_ids"`
		} `json:"voices"`
	}
	if err := l.get(ctx, key, "/v2/voices?page_size=100&collection_id="+url.QueryEscape(id), &page); err != nil {
		return nil, err
	}
	out := make([]LibraryVoice, 0, len(page.Voices))
	for _, v := range page.Voices {
		out = append(out, LibraryVoice{ID: v.ID, Name: v.Name, Language: v.Labels["language"], Accent: v.Labels["accent"],
			Gender: v.Labels["gender"], Models: v.Models})
	}
	return out, nil
}

func (l *voiceLibrary) get(ctx context.Context, key, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, elevenBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("xi-api-key", key)
	resp, err := l.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("elevenlabs %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// premadeVoices are the built-in fallback, every one usable on fal's route too,
// in name order.
func premadeVoices() []LibraryVoice {
	out := make([]LibraryVoice, 0, len(VoiceCatalog))
	for name, id := range VoiceCatalog {
		out = append(out, LibraryVoice{ID: id, Name: name, Premade: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// chooseVoice picks the voice for a request: the named one (by id or name,
// case-insensitive, a name prefix such as "Emily E" is enough) from the
// collection or the premade voices, or else the default order.
func chooseVoice(collection []LibraryVoice, name, lang, model string) (LibraryVoice, bool) {
	all := append(append([]LibraryVoice{}, collection...), premadeVoices()...)
	if name = strings.TrimSpace(name); name != "" {
		for _, v := range all {
			if v.ID == name || strings.EqualFold(v.Name, name) || hasPrefixFold(v.Name, name) {
				return v, true
			}
		}
		return LibraryVoice{}, false
	}
	for _, p := range preferredVoices {
		for _, v := range collection {
			if hasPrefixFold(v.Name, p) && v.speaks(lang, model) {
				return v, true
			}
		}
	}
	for _, v := range collection {
		if v.speaks(lang, model) {
			return v, true
		}
	}
	return LibraryVoice{ID: VoiceCatalog[DefaultVoice], Name: DefaultVoice, Premade: true}, true
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// falVoiceFor is the voice name fal's ElevenLabs route gets: a premade voice as
// is; for a library voice (which fal's account may not hold) the closest
// premade one by gender and accent.
func falVoiceFor(v LibraryVoice) string {
	if v.Premade {
		return v.Name
	}
	switch {
	case v.Gender == "male" && v.Accent == "british":
		return "Daniel"
	case v.Gender == "male":
		return "Brian"
	case v.Accent == "british":
		return "Alice"
	default:
		return "Sarah"
	}
}
