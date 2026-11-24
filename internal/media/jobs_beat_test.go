package media

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// A job whose pod stopped (deploy, OOM kill) stops beating and is reported failed, not refunded,
// within jobBeatStale, so marketflow-api re-queues it under the same charge key instead of waiting
// out jobStaleness (2026-11-24: two owner renders sat "rendering" for 40 minutes after a deploy).
func TestLostJobIsKnownByItsHeartbeat(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	now := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	j := NewJobs(rdb, "test", zap.NewNop())
	j.nowFn = func() time.Time { return now }
	ctx := context.Background()

	running := storedJob{Job: Job{Status: JobRunning, StartedAt: now, Attempt: 1}, ChargeKey: "render:v1:1", Beat: now}
	if err := j.put(ctx, "v1", running); err != nil {
		t.Fatal(err)
	}
	now = now.Add(90 * time.Second)
	if job, _, _ := j.Get(ctx, "v1"); job.Status != JobRunning {
		t.Fatalf("a job that beat 90 s ago is %s", job.Status)
	}
	now = now.Add(time.Minute) // 150 s with no heartbeat
	job, ok, err := j.Get(ctx, "v1")
	if err != nil || !ok || job.Status != JobFailed || job.Refunded {
		t.Fatalf("lost job: ok %v err %v %+v", ok, err, job)
	}
	if attempt, key := NextCharge("v1", job, ok); attempt != 1 || key != "render:v1:1" {
		t.Fatalf("the retry of a lost job must reuse its charge: %d %s", attempt, key)
	}

	// A job saved before heartbeats keeps the old 35-minute rule.
	old := storedJob{Job: Job{Status: JobRunning, StartedAt: now.Add(-10 * time.Minute), Attempt: 1}}
	_ = j.put(ctx, "v2", old)
	if job, _, _ := j.Get(ctx, "v2"); job.Status != JobRunning {
		t.Fatalf("a beat-less job 10 min old is %s", job.Status)
	}
}

func TestHeartbeatKeepsALongJobAlive(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	j := NewJobs(rdb, "test", zap.NewNop())
	release, err := j.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	block := make(chan struct{})
	if err := j.Start("v3", 0, 1, "", func() { release(); close(done) }, nil, func(ctx context.Context) (*MediaResult, error) {
		<-block
		return &MediaResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	close(block)
	<-done
	job, ok, _ := j.Get(context.Background(), "v3")
	if !ok || job.Status != JobDone {
		t.Fatalf("finished job %+v", job)
	}
}
