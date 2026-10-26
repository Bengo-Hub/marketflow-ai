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

// The studio picks from every voice the ElevenLabs account holds: the owner's
// collection (VoiceCollection, "codevertex-voices" by default) is preferred,
// but never the only choice, so the best voice for the content wins (see
// rankVoices). Voices added to the account or the collection are picked up
// within libraryTTL, with no code change. Library voices work only on a paid
// ElevenLabs plan ("Free users cannot use library voices via the API",
// 2026-10-26), which the direct route already requires.

// DefaultVoiceCollection is the owner's ElevenLabs voice collection.
const DefaultVoiceCollection = "codevertex-voices"

// DefaultVoices are the configured defaults, best first: an id or a name
// prefix each (owner, 2026-10-27). Yna Agalo, a Kenyan voice, leads; then
// the multilingual Emily the owner uses in ElevenLabs Studio
// (yYHmXZDCllTpvyYiLRDT, counted once the account holds it), then Emily E.
// MF_ELEVENLABS_DEFAULT_VOICES overrides the list.
var DefaultVoices = []string{"ijKilL5CnjXKMWDHOJH8", "yYHmXZDCllTpvyYiLRDT", "Emily E", "Emily"}

// libraryTTL: the owner adds voices now and then, not by the minute.
const libraryTTL = time.Hour

// LibraryVoice is one voice the studio can use.
type LibraryVoice struct {
	ID           string   `json:"voice_id"`
	Name         string   `json:"name"`
	Language     string   `json:"language"` // the voice's own language label, e.g. en
	Accent       string   `json:"accent,omitempty"`
	Gender       string   `json:"gender,omitempty"`
	UseCase      string   `json:"use_case,omitempty"`    // e.g. advertisement, social_media
	Descriptive  string   `json:"descriptive,omitempty"` // e.g. warm, confident
	Models       []string `json:"models,omitempty"`      // models it is tuned for
	InCollection bool     `json:"in_collection,omitempty"`
	Premade      bool     `json:"premade,omitempty"`
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
	return strings.HasPrefix(model, "eleven_v4") || strings.HasPrefix(model, "eleven_multilingual") || model == ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// voiceLibrary lists the account's voices, cached per ElevenLabs key.
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

// voices returns the account's voices, collection first; a failed read keeps
// the last good list, and with none the premade voices still serve.
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

type apiVoice struct {
	ID       string            `json:"voice_id"`
	Name     string            `json:"name"`
	Category string            `json:"category"`
	Labels   map[string]string `json:"labels"`
	Models   []string          `json:"high_quality_base_model_ids"`
}

func (v apiVoice) library(inCollection bool) LibraryVoice {
	return LibraryVoice{ID: v.ID, Name: v.Name, Language: v.Labels["language"], Accent: v.Labels["accent"],
		Gender: v.Labels["gender"], UseCase: v.Labels["use_case"], Descriptive: v.Labels["descriptive"],
		Models: v.Models, InCollection: inCollection, Premade: v.Category == "premade"}
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
	collID := ""
	for _, c := range colls.Collections {
		if strings.EqualFold(c.Title, l.collection) || c.ID == l.collection {
			collID = c.ID
		}
	}
	var out []LibraryVoice
	inColl := map[string]bool{}
	if collID != "" {
		var page struct {
			Voices []apiVoice `json:"voices"`
		}
		if err := l.get(ctx, key, "/v2/voices?page_size=100&collection_id="+url.QueryEscape(collID), &page); err != nil {
			return nil, err
		}
		for _, v := range page.Voices {
			out = append(out, v.library(true))
			inColl[v.ID] = true
		}
	}
	// The rest of the account: the owner's other voices and the premade ones.
	var all struct {
		Voices []apiVoice `json:"voices"`
	}
	if err := l.get(ctx, key, "/v2/voices?page_size=100", &all); err != nil {
		return out, nil // the collection alone still serves
	}
	for _, v := range all.Voices {
		if !inColl[v.ID] {
			out = append(out, v.library(false))
		}
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
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
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

// withPremade adds the catalogued premade voices the account list lacks.
func withPremade(account []LibraryVoice) []LibraryVoice {
	out := append([]LibraryVoice{}, account...)
	for _, p := range premadeVoices() {
		known := false
		for _, v := range account {
			if v.ID == p.ID {
				known = true
				break
			}
		}
		if !known {
			out = append(out, p)
		}
	}
	return out
}

// VoiceChoice is what a voice-over asks for.
type VoiceChoice struct {
	Name     string // a voice id or name (a prefix will do); "" lets the delivery decide
	Delivery Delivery
	Prefs    VoicePrefs
	Defaults []string
}

// pickVoice returns the named voice, or the best voice for the delivery that
// can read lang with model; ok is false only for a named voice that does not exist.
func pickVoice(account []LibraryVoice, c VoiceChoice, lang, model string) (LibraryVoice, bool) {
	all := withPremade(account)
	if name := strings.TrimSpace(c.Name); name != "" {
		for _, v := range all {
			if v.ID == name || strings.EqualFold(v.Name, name) || hasPrefixFold(v.Name, name) {
				return v, true
			}
		}
		return LibraryVoice{}, false
	}
	// The premade DefaultVoice closes the default list, so with no account
	// voices at all the fallback is that one, not the first by name.
	defaults := append(append([]string{}, c.Defaults...), DefaultVoice)
	if ranked := rankVoices(all, c.Delivery, c.Prefs, lang, model, defaults); len(ranked) > 0 {
		return ranked[0], true
	}
	return LibraryVoice{ID: VoiceCatalog[DefaultVoice], Name: DefaultVoice, Premade: true}, true
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// falVoiceFor is the voice name fal's ElevenLabs route gets: a catalogued
// premade voice as is; for any other voice (which fal's account may not hold)
// the closest premade one by gender and accent.
func falVoiceFor(v LibraryVoice) string {
	if _, ok := VoiceCatalog[v.Name]; ok {
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
