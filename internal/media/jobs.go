package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// A motion poster or a video ad takes minutes, longer than an HTTP request may
// stay open here (the server's write timeout is 120 s). So it runs as a job:
// Start records it in Redis and does the work in the background, and when it
// settles the notifier pushes the state to the caller (marketflow-api's job
// callback). Get is the caller's fallback for a push that never arrived. The
// state outlives this pod; a job lost with its pod stays "running" until
// staleness, and the caller starts it again under the same version id, whose
// charge key stops a second charge.

// Job states.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// MediaResult is a stored video: a motion poster clip or an assembled ad.
type MediaResult struct {
	URL          string  `json:"url"`
	Key          string  `json:"key"`
	Bytes        int64   `json:"bytes"`
	Seconds      float64 `json:"seconds"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	SHA256       string  `json:"sha256"` // of the stored file: the approval evidence names it
	Route        string  `json:"route,omitempty"`
	CostUSD      float64 `json:"cost_usd"`
	SourceURL    string  `json:"source_url,omitempty"` // a provider's link (expires)
	Attempts     int     `json:"attempts,omitempty"`
	CaptionLines int     `json:"caption_lines,omitempty"`
}

// Job is a job's state as callers see it.
type Job struct {
	Status         string       `json:"status"`
	Result         *MediaResult `json:"result,omitempty"`
	CreditsCharged int          `json:"credits_charged"`
	Error          string       `json:"error,omitempty"`
	StartedAt      time.Time    `json:"started_at"`
	// Attempt numbers the charges for this version. A failed job is refunded,
	// so its retry is a new attempt with a new charge key; a job lost with its
	// pod was not refunded, so its retry reuses the key and is not charged twice.
	Attempt   int    `json:"attempt"`
	Refunded  bool   `json:"refunded"`
	ChargeKey string `json:"-"`
}

type storedJob struct {
	Job
	ChargeKey string `json:"charge_key"`
}

// NextCharge returns the attempt and charge key a new start of this version
// must use, given its previous state (ok=false when there was none).
func NextCharge(versionID string, prev Job, ok bool) (int, string) {
	attempt := 1
	if ok {
		attempt = max(prev.Attempt, 1)
		if prev.Refunded {
			attempt++
		}
	}
	return attempt, fmt.Sprintf("render:%s:%d", versionID, attempt)
}

// ErrJobsBusy means this pod is already running as many media jobs as it may.
var ErrJobsBusy = errors.New("media: too many videos in progress, try again shortly")

const (
	jobTTL       = 48 * time.Hour
	jobBudget    = 15 * time.Minute
	jobStaleness = 20 * time.Minute // a "running" job older than this was lost
	maxJobs      = 3                // per pod: each holds a temp dir and an ffmpeg run
)

// Jobs runs media jobs in the background with their state in Redis.
type Jobs struct {
	redis  *redis.Client
	ns     string
	slots  chan struct{}
	notify func(versionID string, job Job)
	logger *zap.Logger
	nowFn  func() time.Time
}

// NewJobs builds the job runner.
func NewJobs(rdb *redis.Client, namespace string, logger *zap.Logger) *Jobs {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Jobs{redis: rdb, ns: namespace, slots: make(chan struct{}, maxJobs),
		logger: logger.Named("media.jobs"), nowFn: time.Now}
}

// WithNotify sets who is told when a job is done or failed; nil leaves callers
// to poll Get.
func (j *Jobs) WithNotify(fn func(versionID string, job Job)) *Jobs {
	j.notify = fn
	return j
}

// Ready reports whether jobs can run here.
func (j *Jobs) Ready() bool { return j != nil && j.redis != nil }

func (j *Jobs) key(versionID string) string { return j.ns + ":mediajob:" + versionID }

// Get returns a job's state; ok=false when there is none. A running job past
// its staleness is reported as failed (its pod stopped), so the caller retries.
func (j *Jobs) Get(ctx context.Context, versionID string) (Job, bool, error) {
	raw, err := j.redis.Get(ctx, j.key(versionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	var s storedJob
	if err := json.Unmarshal(raw, &s); err != nil {
		return Job{}, false, err
	}
	s.Job.ChargeKey = s.ChargeKey
	if s.Status == JobRunning && j.nowFn().Sub(s.StartedAt) > jobStaleness {
		s.Status, s.Error = JobFailed, "the video job stopped; it will be retried"
	}
	return s.Job, true, nil
}

// Reserve claims a slot before the caller charges, so a busy pod refuses
// without taking credits. Release it with the returned func if Start is not called.
func (j *Jobs) Reserve() (release func(), err error) {
	select {
	case j.slots <- struct{}{}:
		return func() { <-j.slots }, nil
	default:
		return nil, ErrJobsBusy
	}
}

// Start records the job and runs work in the background, in the slot the
// caller reserved. onFail runs when work fails (the caller refunds there).
func (j *Jobs) Start(versionID string, credits, attempt int, chargeKey string, release func(), onFail func(chargeKey string),
	work func(ctx context.Context) (*MediaResult, error)) error {
	state := storedJob{Job: Job{Status: JobRunning, CreditsCharged: credits, StartedAt: j.nowFn().UTC(), Attempt: attempt}, ChargeKey: chargeKey}
	if err := j.put(context.Background(), versionID, state); err != nil {
		release()
		return err
	}
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), jobBudget)
		defer cancel()
		res, err := work(ctx)
		if err != nil {
			j.logger.Warn("media job failed", zap.String("version_id", versionID), zap.Error(err))
			if onFail != nil && chargeKey != "" {
				onFail(chargeKey)
			}
			state.Status, state.Error, state.CreditsCharged, state.Refunded = JobFailed, jobFailure(err), 0, true
		} else {
			state.Status, state.Result = JobDone, res
		}
		if perr := j.put(context.Background(), versionID, state); perr != nil {
			j.logger.Error("media job state not saved", zap.String("version_id", versionID), zap.Error(perr))
		}
		if j.notify != nil {
			// Its own goroutine: the retries must not hold this job's slot.
			job := state.Job
			job.ChargeKey = ""
			go j.notify(versionID, job)
		}
	}()
	return nil
}

// jobFailure is the message the owner sees; the charge was refunded.
func jobFailure(err error) string {
	var ue *UserError
	switch {
	case errors.As(err, &ue):
		return ue.Msg + "; nothing was charged"
	case errors.Is(err, ErrNoFFmpeg):
		return "video is not available on this server yet; nothing was charged"
	}
	return "the video could not be made right now; nothing was charged"
}

// UserError is a failure the owner can act on (a missing photo, a scene that
// needs a presenter); its message is shown as is.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return "media: " + e.Msg }

func (j *Jobs) put(ctx context.Context, versionID string, s storedJob) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return j.redis.Set(ctx, j.key(versionID), b, jobTTL).Err()
}
