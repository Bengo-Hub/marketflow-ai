package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Voice explorer (owner, 2026-11-12): the platform owner searches ElevenLabs' shared voice
// library (for example Kenyan or East African male voices for ads) with the platform's stored
// ElevenLabs key, so nobody's personal sign-in is needed. A voice the owner likes is added to
// the account and its collection in ElevenLabs, and from then on Voices.List picks it up.

// LibraryQuery narrows a shared library search; empty fields do not filter.
type LibraryQuery struct {
	Search   string // free text, e.g. "kenyan" or "swahili"
	Gender   string // male | female | neutral
	Accent   string // e.g. "kenyan", "african"
	Language string // ISO 639-1, e.g. en, sw
	UseCase  string // e.g. advertisement, narrative_story, conversational
	PageSize int    // 1 to 100, default 30
}

// SharedVoice is one voice in the shared library.
type SharedVoice struct {
	VoiceID       string `json:"voice_id"`
	PublicOwnerID string `json:"public_owner_id"`
	Name          string `json:"name"`
	Gender        string `json:"gender,omitempty"`
	Age           string `json:"age,omitempty"`
	Accent        string `json:"accent,omitempty"`
	Language      string `json:"language,omitempty"`
	UseCase       string `json:"use_case,omitempty"`
	Description   string `json:"description,omitempty"`
	PreviewURL    string `json:"preview_url,omitempty"`
	ClonedByCount int64  `json:"cloned_by_count,omitempty"`
}

// ErrNoVoiceKey is a search with no ElevenLabs key stored for the platform.
var ErrNoVoiceKey = errors.New("media: no ElevenLabs key is stored for the platform")

// sharedVoicesURL builds the shared library request (pure, for tests).
func sharedVoicesURL(q LibraryQuery) string {
	v := url.Values{}
	size := q.PageSize
	if size <= 0 || size > 100 {
		size = 30
	}
	v.Set("page_size", strconv.Itoa(size))
	set := func(key, val string) {
		if s := strings.TrimSpace(val); s != "" {
			v.Set(key, s)
		}
	}
	set("search", q.Search)
	set("gender", strings.ToLower(q.Gender))
	set("accent", strings.ToLower(q.Accent))
	set("language", strings.ToLower(q.Language))
	set("use_cases", q.UseCase)
	return elevenBase + "/v1/shared-voices?" + v.Encode()
}

// SearchLibrary searches ElevenLabs' shared voice library with the platform key.
func (v *Voices) SearchLibrary(ctx context.Context, q LibraryQuery) ([]SharedVoice, error) {
	key := v.keys(ctx, "elevenlabs")
	if key == "" {
		return nil, ErrNoVoiceKey
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sharedVoicesURL(q), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("xi-api-key", key)
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elevenlabs shared voices: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("elevenlabs shared voices: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("elevenlabs shared voices: status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var page struct {
		Voices []SharedVoice `json:"voices"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("elevenlabs shared voices: decode: %w", err)
	}
	if page.Voices == nil {
		page.Voices = []SharedVoice{}
	}
	return page.Voices, nil
}
