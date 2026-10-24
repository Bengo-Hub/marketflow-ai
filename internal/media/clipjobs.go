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

// A clip takes minutes, longer than an HTTP request may stay open here (the
// server's write timeout is 120 s). So a clip runs as a job: Start records it
// in Redis and works in the background; the caller (marketflow-api's renderer)
// polls Get. The state outlives this pod; a job lost with its pod stays
// "running" until staleness, and the caller starts it again under the same
// version id, whose charge key stops a second charge.

// Clip job states.
const (
	ClipRunning = "running"
	ClipDone    = "done"
	ClipFailed  = "failed"
)

// ClipJob is a job's state as callers see it.
type ClipJob struct {
	Status         string      `json:"status"`
	Clip           *ClipResult `json:"clip,omitempty"`
	CreditsCharged int         `json:"credits_charged"`
	Error          string      `json:"error,omitempty"`
	StartedAt      time.Time   `json:"started_at"`
	// Attempt numbers the charges for this version. A failed job is refunded,
	// so its retry is a new attempt with a new charge key; a job lost with its
	// pod was not refunded, so its retry reuses the key and is not charged twice.
	Attempt   int    `json:"attempt"`
	Refunded  bool   `json:"refunded"`
	ChargeKey string `json:"-"`
}

type storedJob struct {
	ClipJob
	ChargeKey string `json:"charge_key"`
}

// NextCharge returns the attempt and charge key a new start of this version
// must use, given its previous state (ok=false when there was none).
func NextCharge(versionID string, prev ClipJob, ok bool) (int, string) {
	attempt := 1
	if ok {
		attempt = max(prev.Attempt, 1)
		if prev.Refunded {
			attempt++
		}
	}
	return attempt, fmt.Sprintf("render:%s:%d", versionID, attempt)
}

// ErrClipBusy means this pod is already making as many clips as it may.
var ErrClipBusy = errors.New("media: too many clips in progress, try again shortly")

const (
	clipJobTTL    = 48 * time.Hour
	clipJobBudget = 12 * time.Minute
	clipStaleness = 20 * time.Minute // a "running" job older than this was lost
	maxClipJobs   = 3                // per pod: each holds a temp dir and an ffmpeg run
)

// ClipJobs runs clips in the background with their state in Redis.
type ClipJobs struct {
	clips  *Clips
	redis  *redis.Client
	ns     string
	slots  chan struct{}
	logger *zap.Logger
	nowFn  func() time.Time
}

// NewClipJobs builds the job runner.
func NewClipJobs(clips *Clips, rdb *redis.Client, namespace string, logger *zap.Logger) *ClipJobs {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ClipJobs{clips: clips, redis: rdb, ns: namespace, slots: make(chan struct{}, maxClipJobs),
		logger: logger.Named("media.clipjobs"), nowFn: time.Now}
}

// Ready reports whether clips can be made here.
func (j *ClipJobs) Ready() bool { return j != nil && j.redis != nil && j.clips.Ready() }

func (j *ClipJobs) key(versionID string) string { return j.ns + ":clipjob:" + versionID }

// Get returns a job's state; ok=false when there is none. A running job past
// its staleness is reported as failed (its pod stopped), so the caller retries.
func (j *ClipJobs) Get(ctx context.Context, versionID string) (ClipJob, bool, error) {
	raw, err := j.redis.Get(ctx, j.key(versionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ClipJob{}, false, nil
	}
	if err != nil {
		return ClipJob{}, false, err
	}
	var s storedJob
	if err := json.Unmarshal(raw, &s); err != nil {
		return ClipJob{}, false, err
	}
	s.ClipJob.ChargeKey = s.ChargeKey
	if s.Status == ClipRunning && j.nowFn().Sub(s.StartedAt) > clipStaleness {
		s.Status, s.Error = ClipFailed, "the clip job stopped; it will be retried"
	}
	return s.ClipJob, true, nil
}

// Reserve claims a slot before the caller charges, so a busy pod refuses
// without taking credits. Release it with the returned func if Start is not called.
func (j *ClipJobs) Reserve() (release func(), err error) {
	select {
	case j.slots <- struct{}{}:
		return func() { <-j.slots }, nil
	default:
		return nil, ErrClipBusy
	}
}

// Start records the job and makes the clip in the background, in the slot the
// caller reserved. onFail runs when the clip fails (the caller refunds there).
func (j *ClipJobs) Start(versionID string, req ClipRequest, credits, attempt int, chargeKey string, release func(), onFail func(chargeKey string)) error {
	state := storedJob{ClipJob: ClipJob{Status: ClipRunning, CreditsCharged: credits, StartedAt: j.nowFn().UTC(), Attempt: attempt}, ChargeKey: chargeKey}
	if err := j.put(context.Background(), versionID, state); err != nil {
		release()
		return err
	}
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), clipJobBudget)
		defer cancel()
		res, err := j.clips.Make(ctx, req)
		if err != nil {
			j.logger.Warn("clip failed", zap.String("version_id", versionID), zap.Error(err))
			if onFail != nil && chargeKey != "" {
				onFail(chargeKey)
			}
			msg := "the video could not be made right now; nothing was charged"
			if errors.Is(err, ErrNoFFmpeg) {
				msg = "video is not available on this server yet; nothing was charged"
			}
			state.Status, state.Error, state.CreditsCharged, state.Refunded = ClipFailed, msg, 0, true
		} else {
			state.Status, state.Clip = ClipDone, res
		}
		if perr := j.put(context.Background(), versionID, state); perr != nil {
			j.logger.Error("clip state not saved", zap.String("version_id", versionID), zap.Error(perr))
		}
	}()
	return nil
}

func (j *ClipJobs) put(ctx context.Context, versionID string, s storedJob) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return j.redis.Set(ctx, j.key(versionID), b, clipJobTTL).Err()
}
