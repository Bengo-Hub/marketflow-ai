// Package providerbalance watches the platform owner's provider balances (fal, Higgsfield,
// ElevenLabs). marketflow-ai holds the provider keys, so it reads them: every 15 minutes one
// replica reads each balance, shares the readings with every replica through Redis for the
// media router, and raises the central billing.balance.low event (shared-events
// balancealert) for the platform owner when a provider is low or empty, once per day.
//
// A new provider needs a Reader and an entry in Readers; the alert path is shared.
package providerbalance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Reading is one provider's balance in its own unit.
type Reading struct {
	Remaining float64 `json:"remaining"`
	Unit      string  `json:"unit"` // usd | characters
	// Overage is spend already past the plan (ElevenLabs current_overage, USD).
	Overage  float64   `json:"overage,omitempty"`
	Estimate bool      `json:"estimate,omitempty"`
	At       time.Time `json:"at"`
}

// Settings are the provider's ProviderSetting values (media/<provider>).
type Settings map[string]string

// Reader reads one provider's balance. ok is false when the provider cannot be read yet
// (for Higgsfield: no funded amount recorded), which is not an error.
type Reader interface {
	Provider() string
	Label() string
	TopUpURL() string
	Read(ctx context.Context, key string, s Settings) (r Reading, ok bool, err error)
}

// API bases (tests point them at fake servers).
var (
	falAPIBase        = "https://api.fal.ai"
	elevenLabsAPIBase = "https://api.elevenlabs.io"
	higgsfieldAPIBase = "https://api.higgsfield.ai"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func getJSON(ctx context.Context, provider, rawURL string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header = header
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		body := string(b)
		if len(body) > 300 {
			body = body[:300]
		}
		return &ai.StatusError{Provider: provider, Code: resp.StatusCode, Body: body}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: decode: %w", provider, err)
	}
	return nil
}

// fal: GET /v1/account/billing?expand=credits (the key needs billing:usage:read).
type falReader struct{}

func (falReader) Provider() string { return "fal" }
func (falReader) Label() string    { return "fal.ai" }
func (falReader) TopUpURL() string { return "https://fal.ai/dashboard/billing" }

func (falReader) Read(ctx context.Context, key string, _ Settings) (Reading, bool, error) {
	var out struct {
		Credits struct {
			CurrentBalance float64 `json:"current_balance"`
			Currency       string  `json:"currency"`
		} `json:"credits"`
	}
	h := http.Header{"Authorization": {"Key " + key}}
	if err := getJSON(ctx, "fal", falAPIBase+"/v1/account/billing?expand=credits", h, &out); err != nil {
		return Reading{}, false, err
	}
	if c := strings.ToUpper(out.Credits.Currency); c != "" && c != "USD" {
		return Reading{}, false, fmt.Errorf("fal: balance in %s, expected USD", c)
	}
	return Reading{Remaining: out.Credits.CurrentBalance, Unit: "usd"}, true, nil
}

// ElevenLabs: GET /v1/user/subscription; remaining characters are the limit minus the count.
type elevenLabsReader struct{}

func (elevenLabsReader) Provider() string { return "elevenlabs" }
func (elevenLabsReader) Label() string    { return "ElevenLabs" }
func (elevenLabsReader) TopUpURL() string { return "https://elevenlabs.io/app/subscription" }

func (elevenLabsReader) Read(ctx context.Context, key string, _ Settings) (Reading, bool, error) {
	var out struct {
		CharacterCount int64 `json:"character_count"`
		CharacterLimit int64 `json:"character_limit"`
		CurrentOverage *struct {
			Amount   any    `json:"amount"` // a string ("0") or a number
			Currency string `json:"currency"`
		} `json:"current_overage"`
	}
	h := http.Header{"xi-api-key": {key}}
	if err := getJSON(ctx, "elevenlabs", elevenLabsAPIBase+"/v1/user/subscription", h, &out); err != nil {
		return Reading{}, false, err
	}
	r := Reading{Remaining: float64(out.CharacterLimit - out.CharacterCount), Unit: "characters"}
	if out.CurrentOverage != nil {
		switch v := out.CurrentOverage.Amount.(type) {
		case float64:
			r.Overage = v
		case string:
			r.Overage, _ = strconv.ParseFloat(v, 64)
		}
	}
	return r, true, nil
}

// Higgsfield has no balance API. The owner records funded_amount_usd and funded_at on
// /platform/providers (media/higgsfield); remaining is that amount minus GET /models/usage
// since funded_at, read in windows of at most 31 days and paged. It is an estimate.
type higgsfieldReader struct{}

func (higgsfieldReader) Provider() string { return "higgsfield" }
func (higgsfieldReader) Label() string    { return "Higgsfield" }
func (higgsfieldReader) TopUpURL() string { return "https://console.higgsfield.ai" }

// usageWindow is the longest range /models/usage accepts.
const usageWindow = 31 * 24 * time.Hour

// maxUsagePages bounds one window's paging.
const maxUsagePages = 50

var now = time.Now

func (higgsfieldReader) Read(ctx context.Context, key string, s Settings) (Reading, bool, error) {
	funded, err := strconv.ParseFloat(strings.TrimSpace(s["funded_amount_usd"]), 64)
	if err != nil || funded <= 0 {
		return Reading{}, false, nil
	}
	since, ok := parseWhen(s["funded_at"])
	if !ok {
		return Reading{}, false, nil
	}
	h := http.Header{"Authorization": {"Key " + key}}
	end := now().UTC()
	var used float64
	for start := since; start.Before(end); start = start.Add(usageWindow) {
		wEnd := start.Add(usageWindow)
		if wEnd.After(end) {
			wEnd = end
		}
		for page := 1; page <= maxUsagePages; page++ {
			var out struct {
				TimeSeries []struct {
					Results []struct {
						Cost     string `json:"cost"`
						Currency string `json:"currency"`
					} `json:"results"`
				} `json:"time_series"`
				Total int `json:"total"`
				Page  int `json:"page"`
				Limit int `json:"limit"`
			}
			q := url.Values{"start": {start.Format(time.RFC3339)}, "end": {wEnd.Format(time.RFC3339)}, "page": {strconv.Itoa(page)}}
			if err := getJSON(ctx, "higgsfield", higgsfieldAPIBase+"/models/usage?"+q.Encode(), h, &out); err != nil {
				return Reading{}, false, err
			}
			for _, b := range out.TimeSeries {
				for _, r := range b.Results {
					if c, err := strconv.ParseFloat(r.Cost, 64); err == nil {
						used += c
					}
				}
			}
			if out.Limit <= 0 || page*out.Limit >= out.Total || len(out.TimeSeries) == 0 {
				break
			}
		}
	}
	return Reading{Remaining: funded - used, Unit: "usd", Estimate: true}, true, nil
}

// parseWhen accepts RFC 3339 or a plain date.
func parseWhen(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// Readers are the watched providers.
var Readers = []Reader{falReader{}, higgsfieldReader{}, elevenLabsReader{}}

// Low-water mark defaults: USD 5, and for ElevenLabs about USD 5 of characters at its
// overage rate. ProviderSetting low_water_usd or low_water_characters overrides them.
const (
	DefaultLowWaterUSD        = 5.0
	DefaultLowWaterCharacters = 15000.0
)

// LowMark is the provider's low-water mark in its unit.
func LowMark(unit string, s Settings) float64 {
	key, def := "low_water_usd", DefaultLowWaterUSD
	if unit == "characters" {
		key, def = "low_water_characters", DefaultLowWaterCharacters
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(s[key]), 64); err == nil && v >= 0 {
		return v
	}
	return def
}
