package osrun

import (
	"context"
	"errors"
	"testing"
	"time"
)

func otpCodes(t *testing.T) (*Codes, func(time.Duration)) {
	t.Helper()
	mr, rdb := newRedis(t)
	c := NewCodes(rdb, "mf-ai", DefaultConfig())
	c.genOTP = func() (string, error) { return "482915", nil }
	return c, mr.FastForward
}

// From the threshold (100 credits by default) an approval needs a one-time code; below it, or once
// confirmed, it does not.
func TestNeedsOTP(t *testing.T) {
	c, _ := otpCodes(t)
	if c.NeedsOTP(Approval{Credits: 99}) || !c.NeedsOTP(Approval{Credits: 100}) || c.NeedsOTP(Approval{Credits: 500, OTPDone: true}) {
		t.Fatal("threshold not applied")
	}
	off := NewCodes(nil, "mf-ai", Config{OTPCredits: 0})
	off.otpCredits = 0
	if off.NeedsOTP(Approval{Credits: 10000}) {
		t.Fatal("a 0 threshold switches the code off")
	}
}

// The one-time code is bound to the user, the number and the action, works once, for 15 minutes,
// and three wrong tries cancel it.
func TestOTPIssueRedeem(t *testing.T) {
	c, ff := otpCodes(t)
	ctx := context.Background()
	a := Approval{Action: ActionRender, RunID: "r1", TenantID: "t1", UserID: "u1", Sender: "+254700000001", VersionID: "v1", Credits: 140}
	code, err := c.IssueOTP(ctx, a)
	if err != nil || code != "482915" || !c.OTPPending(ctx, "t1", "u1") {
		t.Fatalf("issue: %q %v", code, err)
	}
	if _, err := c.RedeemOTP(ctx, "t1", "u1", "+254700000009", "482915"); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("another number: %v", err)
	}
	if _, err := c.RedeemOTP(ctx, "t1", "u2", "+254700000001", "482915"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("another user: %v", err)
	}
	got, err := c.RedeemOTP(ctx, "t1", "u1", "+254700000001", "482915")
	if err != nil || !got.OTPDone || got.VersionID != "v1" || got.Credits != 140 {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	if _, err := c.RedeemOTP(ctx, "t1", "u1", "+254700000001", "482915"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("single use: %v", err)
	}

	if _, err := c.IssueOTP(ctx, a); err != nil {
		t.Fatal(err)
	}
	ff(16 * time.Minute)
	if _, err := c.RedeemOTP(ctx, "t1", "u1", "+254700000001", "482915"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("expired after 15 minutes: %v", err)
	}

	if _, err := c.IssueOTP(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.RedeemOTP(ctx, "t1", "u1", "+254700000001", "111111"); !errors.Is(err, ErrWrongCode) {
			t.Fatalf("wrong try %d: %v", i, err)
		}
	}
	if _, err := c.RedeemOTP(ctx, "t1", "u1", "+254700000001", "482915"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("three wrong tries cancel it: %v", err)
	}
}

// The action hash changes with anything the approval does, so a code confirms one action only.
func TestActionHash(t *testing.T) {
	a := Approval{Action: ActionApprove, TenantID: "t1", UserID: "u1", RunID: "r1", Credits: 120}
	b := a
	b.Credits = 121
	c := a
	c.Tier = "poster"
	if ActionHash(a) == ActionHash(b) || ActionHash(a) == ActionHash(c) || ActionHash(a) != ActionHash(a) {
		t.Fatal("hash does not bind the action")
	}
}

func TestParseOTP(t *testing.T) {
	for text, want := range map[string]string{"482915": "482915", "482 915": "482915", "Code: 482915": "482915",
		"otp 482-915": "482915", " 482915. ": "482915"} {
		if got, ok := ParseOTP(text); !ok || got != want {
			t.Errorf("%q = %q %v", text, got, ok)
		}
	}
	for _, text := range []string{"4829", "4829150", "YES 4821", "make 482915 posters", ""} {
		if got, ok := ParseOTP(text); ok {
			t.Errorf("%q read as %q", text, got)
		}
	}
}

// A Buy credits offer is used once, only by its own tap from its number; the purchase it starts is
// kept under the Treasury intent until the payment resumes the waiting action once.
func TestBuyOfferAndTopup(t *testing.T) {
	c, ff := otpCodes(t)
	ctx := context.Background()
	waiting := &Approval{Action: ActionRender, RunID: "r1", TenantID: "t1", UserID: "u1", VersionID: "v1", Credits: 80}
	o := BuyOffer{TenantID: "t1", UserID: "u1", Sender: "+254700000001", BundleCode: "bundle_60", Units: 60, PriceKES: 750, Need: 80, Waiting: waiting}
	if _, err := c.OfferBuy(ctx, BuyOffer{TenantID: "t1"}); err == nil {
		t.Fatal("an offer without a bundle must be refused")
	}
	ref, err := c.OfferBuy(ctx, o)
	if err != nil || len(ref) != 16 {
		t.Fatalf("offer: %q %v", ref, err)
	}
	if _, err := c.TakeBuy(ctx, "t1", "u1", "+254700000001", "0000000000000000"); !errors.Is(err, ErrStaleTap) {
		t.Fatalf("older button: %v", err)
	}
	if _, err := c.TakeBuy(ctx, "t1", "u1", "+254700000009", ref); !errors.Is(err, ErrStaleTap) {
		t.Fatalf("another number: %v", err)
	}
	got, err := c.TakeBuy(ctx, "t1", "u1", "+254700000001", ref)
	if err != nil || got.BundleCode != "bundle_60" || got.Waiting == nil || got.Waiting.VersionID != "v1" {
		t.Fatalf("take: %+v %v", got, err)
	}
	if _, err := c.TakeBuy(ctx, "t1", "u1", "+254700000001", ref); !errors.Is(err, ErrNoCode) {
		t.Fatalf("single use: %v", err)
	}

	if err := c.SaveTopup(ctx, BuyOffer{TenantID: "t1"}); err == nil {
		t.Fatal("a purchase without an intent must be refused")
	}
	got.IntentID = "intent-1"
	if err := c.SaveTopup(ctx, *got); err != nil {
		t.Fatal(err)
	}
	if tp, ok := c.Topup(ctx, "intent-1"); !ok || tp.Waiting.RunID != "r1" {
		t.Fatalf("topup: %+v %v", tp, ok)
	}
	if !c.FinishTopup(ctx, "intent-1") || c.FinishTopup(ctx, "intent-1") {
		t.Fatal("only one resume per payment")
	}
	if _, ok := c.Topup(ctx, "intent-2"); ok {
		t.Fatal("an unknown intent is not ours")
	}

	if err := c.SaveTopup(ctx, BuyOffer{TenantID: "t1", IntentID: "intent-3"}); err != nil {
		t.Fatal(err)
	}
	ff(3 * time.Hour)
	if _, ok := c.Topup(ctx, "intent-3"); ok {
		t.Fatal("a purchase is kept for the top-up TTL only")
	}
}

func TestAltAndBuyIDs(t *testing.T) {
	r, ok := ParseReply(AltID("run-1", 2))
	if !ok || r.Kind != ReplyAlt || r.RunID != "run-1" || r.Row != 2 {
		t.Fatalf("alt: %+v %v", r, ok)
	}
	r, ok = ParseReply(BuyID("0123456789abcdef"))
	if !ok || r.Kind != ReplyBuy || r.RunID != "0123456789abcdef" {
		t.Fatalf("buy: %+v %v", r, ok)
	}
	for _, bad := range []string{"mf:alt:run-1", "mf:alt:run-1:x", "mf:alt:run-1:12", "mf:buy:short", "mf:buy"} {
		if _, ok := ParseReply(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if code, yes := ParseYes("POST 4821"); !yes || code != "4821" {
		t.Fatalf("POST <code>: %q %v", code, yes)
	}
	if code, yes := ParseYes("chapisha 4821"); !yes || code != "4821" {
		t.Fatalf("chapisha <code>: %q %v", code, yes)
	}
	if _, yes := ParseYes("post this on facebook"); yes {
		t.Fatal("a request is not a POST reply")
	}
}

func TestConfigFromEnvOTPAndTopup(t *testing.T) {
	cfg, err := ConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg.OTPCredits != 100 || cfg.OTPTTL != 15*time.Minute || cfg.TopupTTL != 2*time.Hour {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	env := map[string]string{"MF_OS_OTP_CREDIT_THRESHOLD": "250", "MF_OS_OTP_TTL": "10m", "MF_OS_TOPUP_TTL": "nope"}
	cfg, err = ConfigFromEnv(func(k string) string { return env[k] })
	if err == nil || cfg.OTPCredits != 250 || cfg.OTPTTL != 10*time.Minute || cfg.TopupTTL != 2*time.Hour {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
	env = map[string]string{"MF_OS_OTP_CREDIT_THRESHOLD": "-1"}
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Fatal("a negative threshold must be refused")
	}
}
