package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// CreditDecision is subscriptions-api's answer to a spend of AI credits.
type CreditDecision struct {
	Allowed bool   `json:"allowed"`
	Exempt  bool   `json:"exempt"`
	Reason  string `json:"reason"`
}

// CreditSpender spends AI credits in subscriptions-api, which owns every prepaid balance: the plan's
// monthly allowance first, then bundles the tenant bought. AI credits are prepaid only, so a spend
// that does not fit is refused.
type CreditSpender interface {
	// Spend charges units under key. An error means the answer is unknown and the caller must not
	// run the request.
	Spend(ctx context.Context, tenantID uuid.UUID, units int64, key string) (CreditDecision, error)
	// Refund reverses a spend made under key, for a request that produced nothing.
	Refund(ctx context.Context, tenantID uuid.UUID, key string) error
}

// HTTPCreditSpender talks to subscriptions-api over the internal service key.
type HTTPCreditSpender struct {
	baseURL string
	apiKey  string
	client  *http.Client
	logger  *zap.Logger
}

// NewHTTPCreditSpender creates the spender.
func NewHTTPCreditSpender(baseURL, apiKey string, logger *zap.Logger) *HTTPCreditSpender {
	return &HTTPCreditSpender{baseURL: baseURL, apiKey: apiKey, client: &http.Client{Timeout: 5 * time.Second}, logger: logger.Named("credits")}
}

func (c *HTTPCreditSpender) post(ctx context.Context, path string, body map[string]any) (*http.Response, error) {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	return c.client.Do(req)
}

// Spend implements CreditSpender.
func (c *HTTPCreditSpender) Spend(ctx context.Context, tenantID uuid.UUID, units int64, key string) (CreditDecision, error) {
	resp, err := c.post(ctx, "/api/v1/s2s/credits/ai_credits/consume", map[string]any{
		"tenant_id": tenantID.String(), "units": units, "key": key, "description": "AI chat message",
	})
	if err != nil {
		return CreditDecision{}, fmt.Errorf("ai credit spend: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CreditDecision{}, fmt.Errorf("ai credit spend: status %d", resp.StatusCode)
	}
	var d CreditDecision
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return CreditDecision{}, fmt.Errorf("ai credit spend decode: %w", err)
	}
	return d, nil
}

// Refund implements CreditSpender.
func (c *HTTPCreditSpender) Refund(ctx context.Context, tenantID uuid.UUID, key string) error {
	resp, err := c.post(ctx, "/api/v1/s2s/credits/ai_credits/refund", map[string]any{
		"tenant_id": tenantID.String(), "consume_key": key, "refund_key": key + ":refund",
	})
	if err != nil {
		return fmt.Errorf("ai credit refund: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ai credit refund: status %d", resp.StatusCode)
	}
	return nil
}
