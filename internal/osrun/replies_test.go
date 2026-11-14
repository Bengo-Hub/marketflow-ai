package osrun

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReplyIDRoundTrip(t *testing.T) {
	cases := []struct {
		id   string
		want Reply
	}{
		{ReplyID(ReplyYes, "run-1", "4821"), Reply{Kind: ReplyYes, RunID: "run-1", Code: "4821"}},
		{ReplyID(ReplyCheaper, "run-1", ""), Reply{Kind: ReplyCheaper, RunID: "run-1"}},
		{ReplyID(ReplyCancel, "run-1", "4821"), Reply{Kind: ReplyCancel, RunID: "run-1"}},
		{ReplyID(ReplyChange, "run-2", ""), Reply{Kind: ReplyChange, RunID: "run-2"}},
		{ReplyID(ReplyDiscard, "run-2", ""), Reply{Kind: ReplyDiscard, RunID: "run-2"}},
		{FieldRowID("objective", 2), Reply{Kind: ReplyField, Field: "objective", Row: 2}},
	}
	for _, tc := range cases {
		got, ok := ParseReply(tc.id)
		if !ok || got != tc.want {
			t.Fatalf("ParseReply(%q) = %+v, %v; want %+v", tc.id, got, ok, tc.want)
		}
	}
	if id := ReplyID(ReplyYes, "6f1c2d3e-0000-4000-8000-000000000001", "1234"); len(id) > 256 {
		t.Fatalf("id too long for Meta: %d", len(id))
	}
}

func TestParseReplyRejects(t *testing.T) {
	for _, id := range []string{
		"", "stop", "approve", "mf:", "mf:yes:run-1", "mf:yes:run-1:12", "mf:yes:run-1:abcd", "mf:yes::1234",
		"mf:cancel:", "mf:cancel:run:extra", "mf:f:objective", "mf:f::1", "mf:f:objective:x", "mf:f:objective:-1",
		"mf:launch:run-1", "xx:yes:run-1:1234",
	} {
		if r, ok := ParseReply(id); ok {
			t.Fatalf("ParseReply(%q) accepted: %+v", id, r)
		}
	}
}

func TestFieldOptionsFitAList(t *testing.T) {
	for field, opts := range fieldOptions {
		if len(opts) == 0 || len(opts) > 10 {
			t.Fatalf("%s: %d rows; a list holds 1 to 10", field, len(opts))
		}
		for i, o := range opts {
			if n := len([]rune(o.Title)); n == 0 || n > 24 {
				t.Fatalf("%s row %d title %q is %d characters", field, i, o.Title, n)
			}
			if len([]rune(o.Description)) > 72 || o.Value == "" {
				t.Fatalf("%s row %d: %+v", field, i, o)
			}
			if v, ok := FieldAnswer(field, i); !ok || v != o.Value {
				t.Fatalf("FieldAnswer(%s, %d) = %q, %v", field, i, v, ok)
			}
		}
		if _, ok := FieldAnswer(field, len(opts)); ok {
			t.Fatalf("%s: a row past the end must not answer", field)
		}
	}
	if FieldOptions("offering") != nil {
		t.Fatal("the offering needs the owner's own words")
	}
}

// A tapped button names its run: a button from an older run is a stale tap and costs no try
// (owner, 2026-11-14), and the right code for its own run approves once.
func TestRedeemRunBindsTheRun(t *testing.T) {
	_, rdb := newRedis(t)
	c := NewCodes(rdb, "mf-ai", Config{CodeTTL: 30 * time.Minute, CodeAttempts: 3})
	c.gen = func() (string, error) { return "4821", nil }
	ctx := context.Background()
	if _, err := c.Issue(ctx, Approval{Action: ActionApprove, RunID: "r1", TenantID: "t1", UserID: "u1", Sender: "+254700000001"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // more than CodeAttempts stale taps never cancel the code
		if _, err := c.RedeemRun(ctx, "t1", "u1", "+254700000001", "4821", "r-old"); !errors.Is(err, ErrStaleTap) {
			t.Fatalf("another run's button: %v", err)
		}
	}
	if _, err := c.RedeemRun(ctx, "t1", "u1", "+254700000001", "1111", "r1"); !errors.Is(err, ErrStaleTap) {
		t.Fatalf("a replaced code's button: %v", err)
	}
	if p, ok := c.PendingApproval(ctx, "t1", "u1"); !ok || p.RunID != "r1" || p.Code != "4821" || p.Attempts != 0 {
		t.Fatalf("pending = %+v, %v", p, ok)
	}
	a, err := c.RedeemRun(ctx, "t1", "u1", "+254700000001", "4821", "r1")
	if err != nil || a.RunID != "r1" {
		t.Fatalf("own run: %+v, %v", a, err)
	}
	if _, err := c.RedeemRun(ctx, "t1", "u1", "+254700000001", "4821", "r1"); !errors.Is(err, ErrNoCode) {
		t.Fatalf("second tap: %v", err)
	}
}

func TestClarifyKeptForOneAnswer(t *testing.T) {
	mr, rdb := newRedis(t)
	c := NewCodes(rdb, "mf-ai", Config{CodeTTL: 30 * time.Minute, CodeAttempts: 3})
	ctx := context.Background()
	if err := c.SaveClarify(ctx, "t1", "u1", Clarify{Text: "make a video ad for my shea butter", Field: "objective", Sender: "+254700000001"}); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("mf-ai:os:wa-clarify:t1:u1"); ttl != 30*time.Minute {
		t.Fatalf("ttl = %v", ttl)
	}
	if tid, ok := c.ClarifyTenant(ctx, []string{"t0", "t1"}, "u1"); !ok || tid != "t1" {
		t.Fatalf("clarify tenant = %q, %v", tid, ok)
	}
	if _, ok := c.TakeClarify(ctx, "t1", "u1", "+254700000009", "objective"); ok {
		t.Fatal("another number must not complete the request")
	}
	// The wrong number's try used it up; save again for the right answer.
	_ = c.SaveClarify(ctx, "t1", "u1", Clarify{Text: "make a video ad for my shea butter", Field: "objective", Sender: "+254700000001"})
	cl, ok := c.TakeClarify(ctx, "t1", "u1", "+254700000001", "objective")
	if !ok || cl.Text != "make a video ad for my shea butter" {
		t.Fatalf("take = %+v, %v", cl, ok)
	}
	if _, ok := c.TakeClarify(ctx, "t1", "u1", "+254700000001", "objective"); ok {
		t.Fatal("a request completes once")
	}
	if err := c.SaveClarify(ctx, "t1", "u1", Clarify{Text: "", Field: "objective"}); err == nil {
		t.Fatal("empty text must be refused")
	}
}
