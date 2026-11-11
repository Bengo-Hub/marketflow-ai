package handlers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-ai/internal/media"
)

type memBox struct {
	got map[string][]byte
	err error
}

func (b *memBox) Put(_ context.Context, id string, body []byte) error {
	if b.err != nil {
		return b.err
	}
	b.got[id] = body
	return nil
}

func (b *memBox) Take(_ context.Context, id string) ([]byte, bool) {
	v, ok := b.got[id]
	return v, ok
}

// A fal delivery reaches the push box only with a valid ED25519 signature from fal's JWKS; a
// bad one is refused, and a storage fault is a 503 so fal retries.
func TestFalWebhook(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}]}`))
	}))
	defer jwks.Close()

	body := `{"request_id":"r1","status":"OK","payload":{"video":{"url":"https://x/v.mp4"}}}`
	send := func(box media.PushBox, reqID string, sign bool) int {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		sum := sha256.Sum256([]byte(body))
		sig := hex.EncodeToString(make([]byte, ed25519.SignatureSize))
		if sign {
			sig = hex.EncodeToString(ed25519.Sign(priv, []byte(strings.Join([]string{reqID, "u1", ts, hex.EncodeToString(sum[:])}, "\n"))))
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/fal", strings.NewReader(body))
		req.Header.Set("X-Fal-Webhook-Request-Id", reqID)
		req.Header.Set("X-Fal-Webhook-User-Id", "u1")
		req.Header.Set("X-Fal-Webhook-Timestamp", ts)
		req.Header.Set("X-Fal-Webhook-Signature", sig)
		rec := httptest.NewRecorder()
		FalWebhook(&media.FalKeys{URL: jwks.URL}, box, nil)(rec, req)
		return rec.Code
	}

	box := &memBox{got: map[string][]byte{}}
	if code := send(box, "r1", true); code != http.StatusNoContent {
		t.Fatalf("valid delivery: %d", code)
	}
	if string(box.got["r1"]) != body {
		t.Fatalf("delivery not stored: %q", box.got["r1"])
	}
	if code := send(&memBox{got: map[string][]byte{}}, "r1", false); code != http.StatusBadRequest {
		t.Fatalf("unsigned delivery: %d", code)
	}
	if code := send(&memBox{got: map[string][]byte{}}, "other", true); code != http.StatusBadRequest {
		t.Fatalf("header id differs from body id: %d", code)
	}
	if code := send(&memBox{got: map[string][]byte{}, err: errors.New("redis down")}, "r1", true); code != http.StatusServiceUnavailable {
		t.Fatalf("store fault: %d", code)
	}
}
