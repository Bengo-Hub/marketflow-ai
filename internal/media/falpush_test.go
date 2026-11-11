package media

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fal webhooks (owner's push-first rule, 2026-11-11): a delivery is trusted only when its ED25519
// signature over request id, user id, timestamp and the body's SHA-256 verifies with one of fal's
// published keys, within five minutes.
func TestVerifyFalWebhook(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	body := []byte(`{"request_id":"r1","status":"OK","payload":{"video":{"url":"https://x/v.mp4"}}}`)
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	sum := sha256.Sum256(body)
	msg := strings.Join([]string{"r1", "u1", ts, hex.EncodeToString(sum[:])}, "\n")
	sig := hex.EncodeToString(ed25519.Sign(priv, []byte(msg)))

	keys := []ed25519.PublicKey{other, pub} // any published key may have signed it
	if err := VerifyFalWebhook(keys, "r1", "u1", ts, sig, body, now); err != nil {
		t.Fatalf("valid delivery: %v", err)
	}
	if err := VerifyFalWebhook(keys, "r1", "u1", ts, sig, []byte(`{"tampered":true}`), now); err == nil {
		t.Fatal("a changed body must fail")
	}
	if err := VerifyFalWebhook(keys, "r2", "u1", ts, sig, body, now); err == nil {
		t.Fatal("another request id must fail")
	}
	if err := VerifyFalWebhook(keys, "r1", "u1", ts, sig, body, now.Add(6*time.Minute)); err == nil {
		t.Fatal("an old delivery must fail")
	}
	if err := VerifyFalWebhook([]ed25519.PublicKey{other}, "r1", "u1", ts, sig, body, now); err == nil {
		t.Fatal("an unknown key must fail")
	}
	if err := VerifyFalWebhook(keys, "r1", "u1", ts, "zz", body, now); err == nil {
		t.Fatal("a malformed signature must fail")
	}
}

func TestFalDelivery(t *testing.T) {
	id, ok, payload, msg := parseFalDelivery([]byte(`{"request_id":"r1","status":"OK","payload":{"a":1}}`))
	if id != "r1" || !ok || string(payload) != `{"a":1}` || msg != "" {
		t.Fatalf("ok delivery: %s %v %s %q", id, ok, payload, msg)
	}
	_, ok, _, msg = parseFalDelivery([]byte(`{"request_id":"r2","status":"ERROR","error":"Invalid status code: 422","payload":{"detail":"x"}}`))
	if ok || msg == "" {
		t.Fatalf("error delivery: %v %q", ok, msg)
	}
	if id, _, _, _ := parseFalDelivery([]byte(`nope`)); id != "" {
		t.Fatal("a broken body has no request id")
	}
}

func TestWithWebhookPerProvider(t *testing.T) {
	falWebhookURL, hfWebhookURL = "https://ai.example/api/v1/webhooks/fal", "https://ai.example/api/v1/webhooks/higgsfield"
	t.Cleanup(func() { falWebhookURL, hfWebhookURL = "", "" })
	if got := withWebhook("https://queue.fal.run/creatify/boreal", "fal"); !strings.Contains(got, "?fal_webhook=https%3A%2F%2Fai.example") {
		t.Fatalf("fal: %s", got)
	}
	if got := withWebhook("https://hf.example/x?a=1", "higgsfield"); !strings.Contains(got, "&hf_webhook=") {
		t.Fatalf("higgsfield: %s", got)
	}
}
