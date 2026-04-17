package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// HTTPUsageReporter reports usage events to subscriptions-api via HTTP.
// Fire-and-forget — a failure never blocks the caller.
type HTTPUsageReporter struct {
	subscriptionsURL string
	apiKey           string
	client           *http.Client
	logger           *zap.Logger
}

// NewHTTPUsageReporter creates a usage reporter.
func NewHTTPUsageReporter(subscriptionsURL, apiKey string, logger *zap.Logger) *HTTPUsageReporter {
	return &HTTPUsageReporter{
		subscriptionsURL: subscriptionsURL,
		apiKey:           apiKey,
		client:           &http.Client{Timeout: 5 * time.Second},
		logger:           logger.Named("usage"),
	}
}

// Report sends a usage increment to subscriptions-api.
func (r *HTTPUsageReporter) Report(tenantID, metric string, value float64) {
	if r.subscriptionsURL == "" || tenantID == "" {
		return
	}
	go func() {
		payload, _ := json.Marshal(map[string]any{
			"metric_type":  metric,
			"service_name": "marketflow",
			"value":        value,
		})
		url := fmt.Sprintf("%s/api/v1/usage/report", r.subscriptionsURL)
		req, _ := http.NewRequest("POST", url, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-ID", tenantID)
		if r.apiKey != "" {
			req.Header.Set("X-API-Key", r.apiKey)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			r.logger.Debug("usage report failed", zap.String("metric", metric), zap.Error(err))
			return
		}
		resp.Body.Close()
	}()
}
