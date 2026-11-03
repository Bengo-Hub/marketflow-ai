package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
)

// Higgsfield helpers beside the job queue: reference uploads and cost estimates.

// maxUploadBytes bounds one reference upload (an actor still or a voice-over).
const maxUploadBytes = 50 << 20

// HiggsfieldUploader puts a reference file on Higgsfield's storage, so a job can read it
// without our bucket being public.
type HiggsfieldUploader struct {
	http *http.Client
}

// NewHiggsfieldUploader builds the uploader.
func NewHiggsfieldUploader() *HiggsfieldUploader {
	return &HiggsfieldUploader{http: &http.Client{Timeout: 2 * time.Minute}}
}

// Upload asks POST /files/generate-upload-url for a signed URL, PUTs the bytes there with
// the returned headers and no API key, and returns public_url only after the PUT succeeded.
// The signed URL is never logged or put in an error.
func (u *HiggsfieldUploader) Upload(ctx context.Context, key, contentType string, data []byte) (string, error) {
	if key == "" {
		return "", fmt.Errorf("higgsfield upload: no key configured")
	}
	if len(data) == 0 || len(data) > maxUploadBytes {
		return "", fmt.Errorf("higgsfield upload: file is 1 byte to %d MB", maxUploadBytes>>20)
	}
	body, _ := json.Marshal(map[string]string{"content_type": contentType})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, higgsfieldBase+"/files/generate-upload-url", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Key "+key)
	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("higgsfield upload url: %w", err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &ai.StatusError{Provider: "higgsfield", Code: resp.StatusCode, Body: truncate(string(b), 300)}
	}
	var slot struct {
		UploadURL     string            `json:"upload_url"`
		UploadHeaders map[string]string `json:"upload_headers"`
		PublicURL     string            `json:"public_url"`
	}
	if err := json.Unmarshal(b, &slot); err != nil || slot.UploadURL == "" || slot.PublicURL == "" {
		return "", fmt.Errorf("higgsfield upload url: incomplete reply")
	}

	put, err := http.NewRequestWithContext(ctx, http.MethodPut, slot.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("higgsfield upload: bad upload url")
	}
	for k, v := range slot.UploadHeaders {
		put.Header.Set(k, v)
	}
	if put.Header.Get("Content-Type") == "" {
		put.Header.Set("Content-Type", contentType)
	}
	put.ContentLength = int64(len(data))
	presp, err := u.http.Do(put)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the signed URL from the message
		}
		return "", fmt.Errorf("higgsfield upload put: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(presp.Body, 64<<10))
	presp.Body.Close()
	if presp.StatusCode < 200 || presp.StatusCode > 299 {
		return "", fmt.Errorf("higgsfield upload put: status %d", presp.StatusCode)
	}
	return slot.PublicURL, nil
}

// Estimator quotes a Higgsfield job through POST /estimate/{model}. Answers are cached, the
// call has a hard timeout, and a failure is remembered briefly, so a quote never waits on
// Higgsfield: the caller's registry price is the answer whenever the estimate is not.
type Estimator struct {
	http    *http.Client
	ttl     time.Duration
	failFor time.Duration
	now     func() time.Time

	mu     sync.Mutex
	cache  map[string]estimateEntry
	downAt time.Time
}

type estimateEntry struct {
	usd float64
	at  time.Time
}

// NewEstimator builds an estimator with a 3 s timeout and a 6 h cache.
func NewEstimator() *Estimator {
	return &Estimator{http: &http.Client{Timeout: 3 * time.Second}, ttl: 6 * time.Hour, failFor: time.Minute,
		now: time.Now, cache: map[string]estimateEntry{}}
}

// USD returns the estimate for model with input, or fallback (and false) when Higgsfield
// gives no figure in time. Token-metered models answer with a description instead of a
// price; those also return the fallback.
func (e *Estimator) USD(ctx context.Context, key, model string, input map[string]any, fallback float64) (float64, bool) {
	if e == nil || key == "" {
		return fallback, false
	}
	body, err := json.Marshal(input) // map keys are sorted, so the cache key is stable
	if err != nil {
		return fallback, false
	}
	ck := model + "|" + string(body)
	now := e.now()
	e.mu.Lock()
	if c, ok := e.cache[ck]; ok && now.Sub(c.at) < e.ttl {
		e.mu.Unlock()
		return c.usd, true
	}
	if !e.downAt.IsZero() && now.Sub(e.downAt) < e.failFor {
		e.mu.Unlock()
		return fallback, false
	}
	e.mu.Unlock()

	usd, ok, transient := e.fetch(ctx, key, model, body)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !ok {
		if transient {
			e.downAt = now
		}
		return fallback, false
	}
	e.downAt = time.Time{}
	e.cache[ck] = estimateEntry{usd: usd, at: now}
	return usd, true
}

// fetch reports the price, whether there was one, and whether a miss was Higgsfield being
// slow or down (as opposed to an answer without a price).
func (e *Estimator) fetch(ctx context.Context, key, model string, body []byte) (float64, bool, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, higgsfieldBase+"/estimate/"+strings.TrimPrefix(model, "/"), bytes.NewReader(body))
	if err != nil {
		return 0, false, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Key "+key)
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, false, true
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return 0, false, resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	}
	var out struct {
		Type string `json:"type"`
		USD  string `json:"usd"`
	}
	if json.Unmarshal(b, &out) != nil || out.USD == "" {
		return 0, false, false
	}
	usd, err := strconv.ParseFloat(out.USD, 64)
	if err != nil || usd < 0 {
		return 0, false, false
	}
	return usd, true, false
}
