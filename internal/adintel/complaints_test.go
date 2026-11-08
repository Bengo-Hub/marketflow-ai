package adintel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/bengobox/marketflow-ai/internal/decide"
)

type fakeDecider struct{ theme map[string]string }

func (f fakeDecider) Decide(_ context.Context, _, state string, qs map[string]decide.Question) (decide.Answers, decide.Usage, error) {
	out := decide.Answers{}
	for id := range qs {
		choice := "other"
		for word, theme := range f.theme {
			if strings.Contains(strings.ToLower(lineOf(state, id)), word) {
				choice = theme
			}
		}
		out[id] = decide.Answer{Type: decide.KindChoice, Choice: choice, Confidence: 0.9}
	}
	return out, decide.Usage{}, nil
}

func lineOf(state, id string) string {
	for _, l := range strings.Split(state, "\n") {
		if strings.HasPrefix(l, "["+id+"]") {
			return l
		}
	}
	return ""
}

func fakeSerper(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-API-KEY") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/places":
			_, _ = w.Write([]byte(`{"places":[{"cid":"1","ratingCount":40},{"cid":"2","ratingCount":0},{"cid":"3","ratingCount":12}]}`))
		case "/reviews":
			_, _ = w.Write([]byte(`{"reviews":[
				{"rating":1,"snippet":"Waited two weeks for delivery","user":{"name":"Jane Doe","link":"https://maps/u/1"},"link":"https://maps/r/1"},
				{"rating":2,"snippet":"Nobody answers on WhatsApp","user":{"name":"John"}},
				{"rating":5,"snippet":"Lovely place"}
			],"nextPageToken":"x"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testComplaints(t *testing.T, base string, cfg ComplaintSettings) *Complaints {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := NewComplaints(func(context.Context) ComplaintSettings { return cfg }, rdb, "test",
		fakeDecider{theme: map[string]string{"delivery": "slow_service", "answers": "poor_response"}})
	c.Base = base
	return c
}

// Low reviews only, sorted into themes; no reviewer identity or review link leaves the package; a
// second ask is served from the cache with no outbound call.
func TestComplaintReport(t *testing.T) {
	srv, calls := fakeSerper(t)
	c := testComplaints(t, srv.URL, ComplaintSettings{Key: "k"})
	r, err := c.Report(context.Background(), "t1", "coding classes for kids Kisumu", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Businesses != 2 || r.Reviewed != 4 {
		t.Fatalf("two places with reviews, two low reviews each: %+v", r)
	}
	got := map[string]int{}
	for _, th := range r.Themes {
		got[th.Key] = th.Count
		if th.Description == "" || th.Example == "" {
			t.Fatalf("each theme says what it is and shows one example: %+v", th)
		}
	}
	if got["slow_service"] != 2 || got["poor_response"] != 2 || len(got) != 2 {
		t.Fatalf("themes: %v", got)
	}
	b, _ := json.Marshal(r)
	for _, leak := range []string{"Jane", "John", "maps/u", "maps/r", "Lovely"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("the report must not carry %q: %s", leak, b)
		}
	}
	before := calls.Load()
	again, err := c.Report(context.Background(), "t2", "Coding classes for kids Kisumu", "KE")
	if err != nil || !again.Cached || calls.Load() != before {
		t.Fatalf("a second ask is cached and free: %+v %v, calls %d to %d", again, err, before, calls.Load())
	}
}

// A report reserves its four searches before any call: past the tenant's share it is refused with
// no outbound call, and another tenant still searches.
func TestComplaintQuota(t *testing.T) {
	srv, calls := fakeSerper(t)
	c := testComplaints(t, srv.URL, ComplaintSettings{Key: "k", TenantShare: 4})
	if _, err := c.Report(context.Background(), "t1", "bakery Kisumu", "ke"); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if _, err := c.Report(context.Background(), "t1", "salon Kisumu", "ke"); !errors.Is(err, ErrTenantShare) {
		t.Fatalf("the share is used: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("a refused report makes no outbound call")
	}
	if _, err := c.Report(context.Background(), "t2", "salon Kisumu", "ke"); err != nil {
		t.Fatalf("another tenant still searches: %v", err)
	}
}

// Without a decider every low review still counts, as "other".
func TestComplaintThemesWithoutDecider(t *testing.T) {
	c := &Complaints{}
	th := c.themes(context.Background(), []lowReview{{Rating: 1, Snippet: "bad"}, {Rating: 2, Snippet: "worse"}})
	if len(th) != 1 || th[0].Key != "other" || th[0].Count != 2 {
		t.Fatalf("%+v", th)
	}
}
