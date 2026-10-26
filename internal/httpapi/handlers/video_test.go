package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/registry"
)

type adStore struct{}

func (adStore) PutAd(context.Context, uuid.UUID, string, string) (string, string, int64, error) {
	return "", "", 0, nil
}

// lockedSpender is a fakeSpender safe for the job goroutine's refund.
type lockedSpender struct {
	mu sync.Mutex
	fakeSpender
}

func (l *lockedSpender) Spend(ctx context.Context, t uuid.UUID, units int64, key string) (CreditDecision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeSpender.Spend(ctx, t, units, key)
}

func (l *lockedSpender) Refund(ctx context.Context, t uuid.UUID, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fakeSpender.Refund(ctx, t, key)
}

func (l *lockedSpender) state() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spent, append([]string(nil), l.refunded...)
}

func adBody(version, voiceover string) string {
	b, _ := json.Marshal(map[string]any{
		"tenant_id": uuid.NewString(), "version_id": version, "format": "1080x1920", "charge": true,
		"photo_urls": []string{"https://cdn.example/p.jpg"}, "headline": "Shea butter", "cta": "Order on WhatsApp",
		"scenes": []map[string]any{
			{"shot": "product_clip", "voiceover": voiceover},
			{"shot": "end_card", "voiceover": "Order on WhatsApp."},
		},
	})
	return string(b)
}

func adCall(h *OSHandler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Ad(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/creative/ad", strings.NewReader(body)))
	return rec
}

// The ad endpoint refuses what it cannot make, and what the compliance filter
// blocks, before any credit is taken; it charges once per attempt, never twice
// for a running job, and the failed job is refunded. Needs ffmpeg; the job part
// needs a Redis at MF_TEST_REDIS.
func TestAdEndpointChargesOnlyWhatItCanMake(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	addr := os.Getenv("MF_TEST_REDIS")
	if addr == "" {
		addr = "127.0.0.1:1" // nothing listens: the job part answers 503 without charging
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 500 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	noKeys := func(context.Context, string) string { return "" }
	spender := &lockedSpender{fakeSpender: fakeSpender{decision: CreditDecision{Allowed: true}}}
	h := &OSHandler{logger: zap.NewNop(), registry: func(context.Context) registry.Registry { return registry.Defaults() }, credits: spender}
	h.SetVideo(media.NewJobs(rdb, "mftest-"+uuid.NewString()[:8], nil), nil,
		media.NewAds(media.NewVoices(noKeys, nil, nil), media.NewAssembler(adStore{}, nil, nil)))

	presenter := strings.Replace(adBody(uuid.NewString(), "Soft skin"), `"product_clip"`, `"presenter"`, 1)
	if rec := adCall(h, presenter); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "presenter") {
		t.Fatalf("presenter scene = %d %s", rec.Code, rec.Body.String())
	}
	if rec := adCall(h, adBody(uuid.NewString(), "This tea cures headaches.")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blocked claim = %d %s", rec.Code, rec.Body.String())
	}
	if spent, _ := spender.state(); spent != 0 {
		t.Fatalf("charged %d for scripts that were refused", spent)
	}
	if os.Getenv("MF_TEST_REDIS") == "" {
		if rec := adCall(h, adBody(uuid.NewString(), "Soft skin")); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("no Redis = %d", rec.Code)
		}
		if spent, _ := spender.state(); spent != 0 {
			t.Fatal("charged without a job store")
		}
		t.Skip("MF_TEST_REDIS not set: job part skipped")
	}

	// A long voice-over (over adBlockChars) costs two template-video blocks.
	version := uuid.NewString()
	long := strings.Repeat("Soft skin all week long. ", 20)
	price, _ := registry.Defaults().Credits(registry.TemplateVideo)
	if rec := adCall(h, adBody(version, long)); rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	if spent, _ := spender.state(); spent != 2*price {
		t.Fatalf("charged %d, want %d", spent, 2*price)
	}
	// The job fails (no voice keys) and is refunded; the same version may then
	// start again as attempt 2.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, refunded := spender.state(); len(refunded) == 1 {
			if refunded[0] != "render:"+version+":1" {
				t.Fatalf("refund key %q", refunded[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed job was not refunded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	job, ok, err := h.jobs.Get(context.Background(), version)
	if err != nil || !ok || job.Status != media.JobFailed || !job.Refunded || !strings.Contains(job.Error, "nothing was charged") {
		t.Fatalf("job = %+v %v %v", job, ok, err)
	}
}
