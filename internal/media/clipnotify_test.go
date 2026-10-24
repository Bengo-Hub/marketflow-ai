package media

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
)

func TestClipCallbackRetriesThenDelivers(t *testing.T) {
	saved := clipCallbackDelays
	clipCallbackDelays = []time.Duration{0, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { clipCallbackDelays = saved })

	var mu sync.Mutex
	var calls int
	var got ClipJob
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/api/v1/internal/creative/clips/v1" || !httpware.ValidServiceKey(r, "svc") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	notify := NewClipCallback(srv.URL+"/", "svc", nil)
	notify("v1", ClipJob{Status: ClipDone, CreditsCharged: 14, Clip: &ClipResult{URL: "https://x/c.mp4", Key: "k", SHA256: "h"}})
	if calls != 2 || got.Status != ClipDone || got.Clip == nil || got.Clip.SHA256 != "h" || got.CreditsCharged != 14 {
		t.Fatalf("calls %d, got %+v", calls, got)
	}

	// A 404 (the version is gone) is final: no more tries.
	calls = 0
	notify("missing", ClipJob{Status: ClipFailed})
	if calls != 0 {
		t.Fatalf("a 404 must not be retried: %d", calls)
	}
	if NewClipCallback("", "svc", nil) != nil {
		t.Fatal("no URL: no notifier")
	}
}
