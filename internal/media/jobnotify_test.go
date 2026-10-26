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

func TestJobCallbackRetriesThenDelivers(t *testing.T) {
	saved := jobCallbackDelays
	jobCallbackDelays = []time.Duration{0, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { jobCallbackDelays = saved })

	var mu sync.Mutex
	var calls int
	var got Job
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/api/v1/internal/creative/jobs/v1" || !httpware.ValidServiceKey(r, "svc") {
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

	notify := NewJobCallback(srv.URL+"/", "svc", nil)
	notify("v1", Job{Status: JobDone, CreditsCharged: 12, Result: &MediaResult{URL: "https://x/ad.mp4", Key: "k", SHA256: "h", Seconds: 23.5}})
	if calls != 2 || got.Status != JobDone || got.Result == nil || got.Result.SHA256 != "h" || got.Result.Seconds != 23.5 || got.CreditsCharged != 12 {
		t.Fatalf("calls %d, got %+v", calls, got)
	}

	// A 404 (the version is gone) is final: no more tries.
	calls = 0
	notify("missing", Job{Status: JobFailed})
	if calls != 0 {
		t.Fatalf("a 404 must not be retried: %d", calls)
	}
	if NewJobCallback("", "svc", nil) != nil {
		t.Fatal("no URL: no notifier")
	}
}
