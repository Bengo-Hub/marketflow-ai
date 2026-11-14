package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return c.SpendFor(ctx, tenantID, units, key, "AI chat message")
}

// SpendFor spends with the description shown on the tenant's credit history
// ("Marketing team request", "Ad copy batch").
func (c *HTTPCreditSpender) SpendFor(ctx context.Context, tenantID uuid.UUID, units int64, key, description string) (CreditDecision, error) {
	resp, err := c.post(ctx, "/api/v1/s2s/credits/ai_credits/consume", map[string]any{
		"tenant_id": tenantID.String(), "units": units, "key": key, "description": description,
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

// Balance returns the tenant's spendable AI credits: the plan allowance left
// plus purchased bundles. unlimited is true for an unlimited allowance; exempt
// tenants (the platform) report unlimited too. It uses the check endpoint with
// zero units, so nothing is spent.
func (c *HTTPCreditSpender) Balance(ctx context.Context, tenantID uuid.UUID) (credits int64, unlimited bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/s2s/credits/ai_credits/check?tenant_id=%s&units=0", c.baseURL, tenantID), nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("ai credit balance: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("ai credit balance: status %d", resp.StatusCode)
	}
	var d struct {
		Exempt        bool  `json:"exempt"`
		QuotaBalance  int64 `json:"quota_balance"`
		BundleBalance int64 `json:"bundle_balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return 0, false, fmt.Errorf("ai credit balance decode: %w", err)
	}
	if d.Exempt || d.QuotaBalance < 0 {
		return 0, true, nil
	}
	return d.QuotaBalance + d.BundleBalance, false, nil
}

// CreditPurchaser buys an AI credit bundle by an M-Pesa prompt to a phone (Sprint 17, the
// WhatsApp Buy credits button). It is called only after the person tapped the button.
type CreditPurchaser interface {
	PurchaseBundle(ctx context.Context, tenantID uuid.UUID, code, phone, ref string) (intentID string, err error)
}

// purchaseTimeout covers Treasury creating the intent and asking the gateway for the prompt.
const purchaseTimeout = 30 * time.Second

// PurchaseBundle implements CreditPurchaser: subscriptions-api's service route creates the
// platform-billed credit purchase and Treasury pushes the prompt to phone. ref is stored on the
// intent (purchase_source_ref) for tracing.
func (c *HTTPCreditSpender) PurchaseBundle(ctx context.Context, tenantID uuid.UUID, code, phone, ref string) (string, error) {
	payload, _ := json.Marshal(map[string]any{"tenant_id": tenantID.String(), "code": code, "phone": phone,
		"source": "marketflow_whatsapp", "source_ref": ref})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/s2s/credits/ai_credits/purchase", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := (&http.Client{Timeout: purchaseTimeout}).Do(req)
	if err != nil {
		return "", fmt.Errorf("ai credit purchase: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		IntentID string `json:"intent_id"`
		Error    string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai credit purchase: status %d: %s", resp.StatusCode, out.Error)
	}
	if out.IntentID == "" {
		return "", errors.New("ai credit purchase: no intent id in the answer")
	}
	return out.IntentID, nil
}

// CreditBalance reads a tenant's spendable AI credits.
type CreditBalance interface {
	Balance(ctx context.Context, tenantID uuid.UUID) (credits int64, unlimited bool, err error)
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
