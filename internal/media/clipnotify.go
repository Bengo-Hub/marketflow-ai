package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"
)

// clipCallbackDelays space the callback's tries; past the last one
// marketflow-api's fallback poll picks the job up from Get.
var clipCallbackDelays = []time.Duration{0, 2 * time.Second, 10 * time.Second, 30 * time.Second}

// NewClipCallback returns a ClipJobs notifier that reports a settled job to
// marketflow-api: POST {base}/api/v1/internal/creative/clips/{version_id} with
// the job state, service key auth. A 4xx other than 429 is final (the version
// is gone); a 5xx or a network error is tried again.
func NewClipCallback(baseURL, serviceKey string, logger *zap.Logger) func(versionID string, job ClipJob) {
	if baseURL == "" || serviceKey == "" {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	logger = logger.Named("media.clipcallback")
	base := strings.TrimSuffix(baseURL, "/")
	hc := &http.Client{Timeout: 15 * time.Second}
	return func(versionID string, job ClipJob) {
		body, _ := json.Marshal(job)
		url := base + "/api/v1/internal/creative/clips/" + versionID
		var last error
		for _, d := range clipCallbackDelays {
			time.Sleep(d)
			final, err := postClipCallback(hc, url, serviceKey, body)
			if err == nil {
				return
			}
			last = err
			if final {
				break
			}
		}
		logger.Warn("clip callback not delivered; marketflow-api will poll", zap.String("version_id", versionID), zap.Error(last))
	}
}

func postClipCallback(hc *http.Client, url, key string, body []byte) (final bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return true, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpware.HeaderAPIKey, key)
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return true, fmt.Errorf("marketflow-api %d", resp.StatusCode)
	default:
		return false, fmt.Errorf("marketflow-api %d", resp.StatusCode)
	}
}
