package media

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Higgsfield push (owner's callback-first rule): every Higgsfield job is submitted with
// ?hf_webhook=<marketflow-ai's public callback>, and Higgsfield posts the finished job there,
// signed with the Standard Webhooks scheme (https://docs.higgsfield.ai/docs/how-to/webhooks,
// /docs/enterprise/webhook-security). The callback checks the signature and drops the
// result in Redis under the request id; whichever pod runs the job reads it from there.
// Polling stays as the fallback, at a slow pace while a push is expected.

// PushBox carries pushed job results between pods.
type PushBox interface {
	Put(ctx context.Context, requestID string, status []byte) error
	Take(ctx context.Context, requestID string) ([]byte, bool)
}

// Push settings (SetHiggsfieldPush). enabled reports whether a signing secret is saved:
// without one a delivery cannot be trusted, so no webhook is asked for.
var (
	hfWebhookURL string
	hfPushBox    PushBox
	hfPushOn     func(ctx context.Context) bool
)

// Pacing while a push is expected: the box is read often (it is local Redis), the
// provider rarely.
var (
	pushCheck    = 2 * time.Second
	pushPollSlow = 30 * time.Second
)

// SetHiggsfieldPush turns the push path on. Call it once at startup.
func SetHiggsfieldPush(callbackURL string, box PushBox, enabled func(ctx context.Context) bool) {
	hfWebhookURL, hfPushBox, hfPushOn = strings.TrimSpace(callbackURL), box, enabled
}

// pushing reports whether a Higgsfield job submitted now should ask for a webhook.
func pushing(ctx context.Context, provider string) bool {
	switch provider {
	case "higgsfield":
		return hfWebhookURL != "" && hfPushBox != nil && hfPushOn != nil && hfPushOn(ctx)
	case "fal":
		return falWebhookURL != "" && hfPushBox != nil
	}
	return false
}

// withWebhook adds the callback to a submit URL.
func withWebhook(submit, provider string) string {
	sep := "?"
	if strings.Contains(submit, "?") {
		sep = "&"
	}
	if provider == "fal" {
		return submit + sep + "fal_webhook=" + url.QueryEscape(falWebhookURL)
	}
	return submit + sep + "hf_webhook=" + url.QueryEscape(hfWebhookURL)
}

// RedisPushBox keeps pushed results for two hours (Higgsfield retries for two hours).
type RedisPushBox struct {
	Redis *redis.Client
	NS    string
}

func (b RedisPushBox) key(id string) string { return b.NS + ":hfpush:" + id }

// Put implements PushBox.
func (b RedisPushBox) Put(ctx context.Context, requestID string, status []byte) error {
	return b.Redis.Set(ctx, b.key(requestID), status, 2*time.Hour).Err()
}

// Take implements PushBox; the result stays for a duplicate reader.
func (b RedisPushBox) Take(ctx context.Context, requestID string) ([]byte, bool) {
	v, err := b.Redis.Get(ctx, b.key(requestID)).Bytes()
	return v, err == nil && len(v) > 0
}

// HiggsfieldDelivery is a webhook body: the request, its terminal status and the outputs.
type HiggsfieldDelivery struct {
	RequestID string          `json:"request_id"`
	Status    string          `json:"status"`
	Error     *string         `json:"error"`
	Payload   json.RawMessage `json:"payload"`
}

// StatusBody turns a delivery into the shape the status endpoint answers with (the
// outputs beside status and error), so the job reads a push and a poll the same way.
func (d HiggsfieldDelivery) StatusBody() ([]byte, error) {
	out := map[string]any{}
	if len(d.Payload) > 0 && string(d.Payload) != "null" {
		if err := json.Unmarshal(d.Payload, &out); err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
	}
	out["status"], out["request_id"] = d.Status, d.RequestID
	if d.Error != nil {
		out["error"] = *d.Error
	}
	return json.Marshal(out)
}

// ParseHiggsfieldDelivery reads and checks a verified webhook body against the documented
// envelope.
func ParseHiggsfieldDelivery(body []byte) (HiggsfieldDelivery, error) {
	var d HiggsfieldDelivery
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("webhook body: %w", err)
	}
	if strings.TrimSpace(d.RequestID) == "" || len(d.RequestID) > 100 || strings.ContainsAny(d.RequestID, " /:") {
		return d, errors.New("webhook body: bad request_id")
	}
	switch d.Status {
	case "completed", "failed", "nsfw", "canceled":
	default:
		return d, fmt.Errorf("webhook body: status %q is not terminal", d.Status)
	}
	return d, nil
}

// Webhook verification errors.
var (
	ErrWebhookSignature = errors.New("webhook signature does not match")
	ErrWebhookStale     = errors.New("webhook timestamp is outside five minutes")
)

// webhookTolerance is the Standard Webhooks timestamp window.
const webhookTolerance = 5 * time.Minute

// VerifyStandardWebhook checks a Standard Webhooks delivery: each secret is "whsec_" plus
// the base64 HMAC key, the signed content is "{id}.{timestamp}.{raw body}", and the
// signature header lists space-separated "v1,<base64 HMAC-SHA256>" entries, any of which
// may match (two during a secret rotation). The timestamp must be within five minutes.
func VerifyStandardWebhook(secrets []string, id, timestamp, signatures string, body []byte, now time.Time) error {
	if id == "" || timestamp == "" || signatures == "" {
		return ErrWebhookSignature
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrWebhookSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > webhookTolerance || d < -webhookTolerance {
		return ErrWebhookStale
	}
	signed := []byte(id + "." + timestamp + "." + string(body))
	for _, secret := range secrets {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(secret), "whsec_"))
		if err != nil || len(key) == 0 {
			continue
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(signed)
		want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		for _, entry := range strings.Fields(signatures) {
			version, sig, ok := strings.Cut(entry, ",")
			if ok && version == "v1" && subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1 {
				return nil
			}
		}
	}
	return ErrWebhookSignature
}
