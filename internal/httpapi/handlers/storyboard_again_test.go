package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
)

// A storyboard asked for again after it was made (a new script, a redrawn actor) is made again as
// a new attempt; any other finished job is reported, not repeated.
func TestStoryboardIsMadeAgain(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	jobs := media.NewJobs(rdb, "mftest-"+uuid.NewString()[:8], nil)
	h := &OSHandler{logger: zap.NewNop(), registry: func(context.Context) registry.Registry { return registry.Defaults() }}
	h.jobs = jobs
	id := media.StoryboardJobID(uuid.NewString())
	made := 0
	work := func(context.Context) (*media.MediaResult, error) {
		made++
		return &media.MediaResult{CostUSD: 0.01}, nil
	}
	free := func(registry.Registry) (int, error) { return 0, nil }
	start := func(again bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.startJob(rec, httptest.NewRequest(http.MethodPost, "/", nil), uuid.New(), id, false, free, "Storyboard", work, again)
		return rec
	}
	waitDone := func() media.Job {
		for i := 0; i < 100; i++ {
			if j, ok, _ := jobs.Get(context.Background(), id); ok && j.Status == media.JobDone {
				return j
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("job never finished")
		return media.Job{}
	}
	if rec := start(true); rec.Code != http.StatusAccepted {
		t.Fatalf("first = %d", rec.Code)
	}
	waitDone()
	if rec := start(false); rec.Code != http.StatusOK || made != 1 {
		t.Fatalf("a finished job is reported, got %d after %d runs", rec.Code, made)
	}
	rec := start(true)
	var j media.Job
	_ = json.Unmarshal(rec.Body.Bytes(), &j)
	if rec.Code != http.StatusAccepted || j.Attempt != 2 {
		t.Fatalf("made again = %d attempt %d", rec.Code, j.Attempt)
	}
	waitDone()
	if made != 2 {
		t.Fatalf("made %d times", made)
	}
}
