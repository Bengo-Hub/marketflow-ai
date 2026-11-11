package media

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fal push (owner's push-first rule, 2026-11-11): fal jobs are submitted with
// ?fal_webhook=<marketflow-ai's public callback>, and fal posts the finished job there
// (https://fal.ai/docs/model-endpoints/webhooks). A delivery is signed with ED25519 over
// "request-id\nuser-id\ntimestamp\nhex(sha256(body))"; the public keys are fal's JWKS. The callback
// drops the verified delivery in the shared push box; the waiting pod takes it and uses the
// payload directly. Polling stays as the slow fallback for a missed delivery.

// falJWKSURL is where fal publishes its webhook signing keys.
var falJWKSURL = "https://rest.fal.ai/.well-known/jwks.json"

// falSkew is the most a delivery's timestamp may differ from now.
const falSkew = 5 * time.Minute

var falWebhookURL string

// SetFalPush turns the fal push path on (call once at startup). box is shared with Higgsfield.
func SetFalPush(callbackURL string, box PushBox) {
	falWebhookURL = strings.TrimSpace(callbackURL)
	if box != nil && hfPushBox == nil {
		hfPushBox = box
	}
}

// VerifyFalWebhook checks a delivery's signature against fal's published keys.
func VerifyFalWebhook(keys []ed25519.PublicKey, requestID, userID, timestamp, sigHex string, body []byte, now time.Time) error {
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return errors.New("fal webhook: bad timestamp")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > falSkew || d < -falSkew {
		return errors.New("fal webhook: timestamp outside five minutes")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("fal webhook: bad signature encoding")
	}
	sum := sha256.Sum256(body)
	msg := []byte(strings.Join([]string{requestID, userID, strings.TrimSpace(timestamp), hex.EncodeToString(sum[:])}, "\n"))
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, msg, sig) {
			return nil
		}
	}
	return errors.New("fal webhook: signature does not verify")
}

// parseFalDelivery reads a delivery: the request id, whether it succeeded, the model output,
// and the error message when it failed.
func parseFalDelivery(body []byte) (requestID string, ok bool, payload json.RawMessage, errMsg string) {
	var d struct {
		RequestID    string          `json:"request_id"`
		Status       string          `json:"status"`
		Payload      json.RawMessage `json:"payload"`
		Error        string          `json:"error"`
		PayloadError string          `json:"payload_error"`
	}
	if json.Unmarshal(body, &d) != nil {
		return "", false, nil, ""
	}
	if d.Status == "OK" && len(d.Payload) > 0 && string(d.Payload) != "null" {
		return d.RequestID, true, d.Payload, ""
	}
	msg := firstNonEmpty(d.Error, d.PayloadError, "the job failed")
	return d.RequestID, false, nil, msg
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// FalKeys fetches and caches fal's webhook keys (at most 24 hours, as fal advises).
type FalKeys struct {
	HTTP *http.Client
	URL  string // JWKS address; empty means fal's published one
	mu   sync.Mutex
	keys []ed25519.PublicKey
	at   time.Time
}

// Get returns the cached keys, refetching them when stale or when refresh is true (a delivery
// signed by a key not in the cache).
func (f *FalKeys) Get(ctx context.Context, refresh bool) ([]ed25519.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !refresh && len(f.keys) > 0 && time.Since(f.at) < 24*time.Hour {
		return f.keys, nil
	}
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	url := f.URL
	if url == "" {
		url = falJWKSURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return f.keys, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return f.keys, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return f.keys, fmt.Errorf("fal jwks: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			X string `json:"x"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&set); err != nil {
		return f.keys, err
	}
	var keys []ed25519.PublicKey
	for _, k := range set.Keys {
		if raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.X, "=")); err == nil && len(raw) == ed25519.PublicKeySize {
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}
	if len(keys) == 0 {
		return f.keys, errors.New("fal jwks: no ed25519 keys")
	}
	f.keys, f.at = keys, time.Now()
	return keys, nil
}

// FalDeliveryRequestID is the request id a delivery names ("" when the body is not one).
func FalDeliveryRequestID(body []byte) string {
	id, _, _, _ := parseFalDelivery(body)
	return id
}
