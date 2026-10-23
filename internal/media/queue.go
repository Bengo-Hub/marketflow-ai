// Package media generates ad media (posters first, video next) through paid
// providers, chosen by a weighted router with fallbacks (Sprint 15). Routes,
// prices and the fallback order are in
// docs/integrations/video-generation-providers.md.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Both fal.ai and Higgsfield run jobs on a queue: submit, poll a status URL,
// then read the result. queueJob holds what differs between them.
type queueJob struct {
	provider string
	auth     string // full Authorization header value
	submit   string // URL to POST the input to
	// done reports the job finished (ok) or failed (err); resultURL is where
	// the output is read, empty to use the status body itself.
	done func(status []byte) (finished bool, resultURL string, err error)
}

// Provider API bases (tests point them at fake servers; SetBases overrides them
// from configuration, for a proxy or a regional endpoint).
var (
	falQueueBase   = "https://queue.fal.run"
	higgsfieldBase = "https://api.higgsfield.ai"
)

// SetBases overrides the provider API bases; empty values keep the defaults.
// Call it once at startup.
func SetBases(falQueue, higgsfield string) {
	if v := strings.TrimRight(strings.TrimSpace(falQueue), "/"); v != "" {
		falQueueBase = v
	}
	if v := strings.TrimRight(strings.TrimSpace(higgsfield), "/"); v != "" {
		higgsfieldBase = v
	}
}

// pollStart and pollMax pace status polling: quick at first, slower later.
var (
	pollStart = 500 * time.Millisecond
	pollMax   = 3 * time.Second
)

type queueClient struct {
	http *http.Client
}

func newQueueClient() *queueClient {
	return &queueClient{http: &http.Client{Timeout: 30 * time.Second}}
}

// run submits input, polls until the job ends or ctx ends, and decodes the
// output into out.
func (c *queueClient) run(ctx context.Context, j queueJob, input, out any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.submit, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", j.auth)
	// Higgsfield documents Idempotency-Key, so a retry after an ambiguous
	// timeout never starts (and bills) a second job; fal ignores it.
	req.Header.Set("Idempotency-Key", uuid.NewString())
	sub, err := c.do(req, j.provider)
	if err != nil {
		return err
	}
	var accepted struct {
		RequestID string `json:"request_id"`
		StatusURL string `json:"status_url"`
	}
	if err := json.Unmarshal(sub, &accepted); err != nil || accepted.StatusURL == "" {
		return fmt.Errorf("%s: submit returned no status_url", j.provider)
	}

	wait := pollStart
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: job %s did not finish in time: %w", j.provider, accepted.RequestID, ctx.Err())
		case <-time.After(wait):
		}
		if wait *= 2; wait > pollMax {
			wait = pollMax
		}
		sreq, err := http.NewRequestWithContext(ctx, http.MethodGet, accepted.StatusURL, nil)
		if err != nil {
			return err
		}
		sreq.Header.Set("Authorization", j.auth)
		status, err := c.do(sreq, j.provider)
		if err != nil {
			return err
		}
		finished, resultURL, jerr := j.done(status)
		if jerr != nil {
			return jerr
		}
		if !finished {
			continue
		}
		result := status
		if resultURL != "" {
			rreq, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
			if err != nil {
				return err
			}
			rreq.Header.Set("Authorization", j.auth)
			if result, err = c.do(rreq, j.provider); err != nil {
				return err
			}
		}
		if err := json.Unmarshal(result, out); err != nil {
			return fmt.Errorf("%s: decode result: %w", j.provider, err)
		}
		return nil
	}
}

// do sends a request and returns the body of a 2xx reply; anything else is an
// ai.StatusError so the breaker can tell our bad requests from outages.
func (c *queueClient) do(req *http.Request, provider string) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &ai.StatusError{Provider: provider, Code: resp.StatusCode, Body: truncate(string(b), 400)}
	}
	return b, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// errJobFailed is a provider-side job failure; it counts against the route.
type errJobFailed struct {
	provider, status, detail string
}

func (e *errJobFailed) Error() string {
	return fmt.Sprintf("%s: job %s: %s", e.provider, e.status, e.detail)
}

// falJob is a fal.ai queue job (https://queue.fal.run). Status values are
// IN_QUEUE, IN_PROGRESS and COMPLETED; the output is at response_url.
func falJob(key, endpoint string) queueJob {
	return queueJob{
		provider: "fal",
		auth:     "Key " + key,
		submit:   falQueueBase + "/" + strings.TrimPrefix(endpoint, "/"),
		done: func(b []byte) (bool, string, error) {
			var s struct {
				Status      string `json:"status"`
				ResponseURL string `json:"response_url"`
			}
			if err := json.Unmarshal(b, &s); err != nil {
				return false, "", fmt.Errorf("fal: decode status: %w", err)
			}
			if s.Status != "COMPLETED" {
				return false, "", nil
			}
			if s.ResponseURL == "" {
				return false, "", fmt.Errorf("fal: completed without response_url")
			}
			return true, s.ResponseURL, nil
		},
	}
}

// higgsfieldJob is a Higgsfield job (https://api.higgsfield.ai/<model path>).
// Terminal statuses are completed, failed, nsfw and canceled; the completed
// status body carries the output (images[].url, video.url, audio.url).
func higgsfieldJob(key, path string) queueJob {
	return queueJob{
		provider: "higgsfield",
		auth:     "Key " + key,
		submit:   higgsfieldBase + "/" + strings.TrimPrefix(path, "/"),
		done: func(b []byte) (bool, string, error) {
			var s struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(b, &s); err != nil {
				return false, "", fmt.Errorf("higgsfield: decode status: %w", err)
			}
			switch s.Status {
			case "completed":
				return true, "", nil
			case "failed", "nsfw", "canceled":
				return false, "", &errJobFailed{provider: "higgsfield", status: s.Status, detail: s.Error}
			default: // queued, in_progress
				return false, "", nil
			}
		},
	}
}
