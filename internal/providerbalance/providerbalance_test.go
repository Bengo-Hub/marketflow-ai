package providerbalance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	events "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/marketflow-ai/internal/media"
)

func fakeAPI(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestFalReader(t *testing.T) {
	srv := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/account/billing" || r.URL.Query().Get("expand") != "credits" || r.Header.Get("Authorization") != "Key fk" {
			http.Error(w, "bad", 400)
			return
		}
		_, _ = w.Write([]byte(`{"username":"x","credits":{"current_balance":3.25,"currency":"USD"}}`))
	})
	prev := falAPIBase
	falAPIBase = srv.URL
	t.Cleanup(func() { falAPIBase = prev })
	r, ok, err := falReader{}.Read(context.Background(), "fk", nil)
	if err != nil || !ok || r.Remaining != 3.25 || r.Unit != "usd" {
		t.Fatalf("fal = %+v %v %v", r, ok, err)
	}
}

func TestElevenLabsReader(t *testing.T) {
	srv := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/user/subscription" || r.Header.Get("xi-api-key") != "ek" {
			http.Error(w, "bad", 401)
			return
		}
		_, _ = w.Write([]byte(`{"tier":"creator","character_count":103950,"character_limit":121000,"current_overage":{"amount":"1.50","currency":"usd"}}`))
	})
	prev := elevenLabsAPIBase
	elevenLabsAPIBase = srv.URL
	t.Cleanup(func() { elevenLabsAPIBase = prev })
	r, ok, err := elevenLabsReader{}.Read(context.Background(), "ek", nil)
	if err != nil || !ok || r.Remaining != 17050 || r.Unit != "characters" || r.Overage != 1.5 {
		t.Fatalf("elevenlabs = %+v %v %v", r, ok, err)
	}
}

// Higgsfield: funded amount minus usage since the funding date, in 31-day windows, paged.
func TestHiggsfieldEstimate(t *testing.T) {
	var mu sync.Mutex
	var windows []string
	srv := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := time.Parse(time.RFC3339, q.Get("start"))
		end, _ := time.Parse(time.RFC3339, q.Get("end"))
		if r.URL.Path != "/models/usage" || end.Sub(start) > usageWindow || r.Header.Get("Authorization") != "Key hk" {
			http.Error(w, `{"detail":"Time range cannot exceed 31 days"}`, 400)
			return
		}
		page, _ := strconv.Atoi(q.Get("page"))
		mu.Lock()
		windows = append(windows, q.Get("start")+"#"+q.Get("page"))
		mu.Unlock()
		// Two pages of one bucket each in every window: USD 1.00 + 0.50.
		cost := "1.0000"
		if page == 2 {
			cost = "0.5000"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 2, "page": page, "limit": 1,
			"time_series": []any{map[string]any{"bucket": "x", "results": []any{map[string]any{"model": "m", "cost": cost, "currency": "USD"}}}}})
	})
	prevBase, prevNow := higgsfieldAPIBase, now
	higgsfieldAPIBase = srv.URL
	now = func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { higgsfieldAPIBase, now = prevBase, prevNow })

	if _, ok, err := (higgsfieldReader{}).Read(context.Background(), "hk", Settings{}); ok || err != nil {
		t.Fatal("no funded amount means no reading, not an error")
	}
	// Funded 40 days before now: two windows, two pages each, USD 3.00 used.
	r, ok, err := higgsfieldReader{}.Read(context.Background(), "hk", Settings{"funded_amount_usd": "20", "funded_at": "2026-09-24"})
	if err != nil || !ok {
		t.Fatalf("read: %v %v", ok, err)
	}
	if r.Remaining != 17 || !r.Estimate || r.Unit != "usd" || len(windows) != 4 {
		t.Fatalf("higgsfield = %+v windows=%v", r, windows)
	}
}

type fakeReader struct {
	p   string
	r   Reading
	err error
}

func (f fakeReader) Provider() string { return f.p }
func (f fakeReader) Label() string    { return f.p }
func (f fakeReader) TopUpURL() string { return "https://top.up/" + f.p }
func (f fakeReader) Read(context.Context, string, Settings) (Reading, bool, error) {
	return f.r, f.err == nil, f.err
}

func TestWatcherGatesAndAlertsOncePerDay(t *testing.T) {
	at := time.Now().UTC() // fresh, so the router gate counts the readings
	claimed := map[string]bool{}
	var published []*events.Event
	bal := media.NewBalances()
	w := &Watcher{
		Readers: []Reader{
			fakeReader{p: "fal", r: Reading{Remaining: 2, Unit: "usd", At: at}},
			fakeReader{p: "higgsfield", r: Reading{Remaining: 0, Unit: "usd", Estimate: true, At: at}},
			fakeReader{p: "elevenlabs", r: Reading{Remaining: 90000, Unit: "characters", At: at}},
			fakeReader{p: "broken", err: errors.New("down")},
			fakeReader{p: "nokey", r: Reading{Remaining: 0, Unit: "usd", At: at}},
		},
		Keys: func(_ context.Context, p string) string {
			if p == "nokey" {
				return ""
			}
			return "k"
		},
		Settings: func(_ context.Context, p string) Settings {
			if p == "fal" {
				return Settings{"low_water_usd": "3"}
			}
			return nil
		},
		Claim: func(_ context.Context, key string, _ time.Duration) bool {
			if claimed[key] {
				return false
			}
			claimed[key] = true
			return true
		},
		Publish:  func(_ context.Context, e *events.Event) error { published = append(published, e); return nil },
		Balances: bal,
	}
	for i := 0; i < 3; i++ { // three ticks in one day
		if err := w.ReadAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(published) != 2 {
		t.Fatalf("published %d alerts, want fal low and higgsfield empty once each", len(published))
	}
	got := map[string]map[string]any{}
	for _, e := range published {
		if e.Subject() != "billing.balance.low" {
			t.Fatalf("subject = %s", e.Subject())
		}
		got[e.Payload["resource"].(string)] = e.Payload
	}
	if f := got["fal"]; f["level"] != "low" || f["scope"] != "platform" || f["threshold"] != 3.0 || f["period"] != at.Format("2006-01-02") || f["buy_url"] != "https://top.up/fal" {
		t.Fatalf("fal alert = %v", f)
	}
	if h := got["higgsfield"]; h["level"] != "empty" || h["estimate"] != true {
		t.Fatalf("higgsfield alert = %v", h)
	}
	// The router gate now holds the readings: Higgsfield is skipped, ElevenLabs is fine.
	if bal.Allows("higgsfield", 0.1) || !bal.Allows("elevenlabs", 5000) || !bal.Allows("fal", 1.5) || bal.Allows("fal", 2.5) {
		t.Fatal("gate does not reflect the readings")
	}
	if _, ok := bal.Get("broken"); ok {
		t.Fatal("a failed read must not record a reading")
	}
	if l := LowMark("characters", nil); l != DefaultLowWaterCharacters {
		t.Fatalf("character mark = %v", l)
	}
}
