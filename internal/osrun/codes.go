package osrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// What a WhatsApp approval code allows (owner, 2026-11-12): approving the plan makes the
// script, the brief and the storyboard; a second code, sent with the storyboard stills,
// starts the paid render.
const (
	ActionApprove = "approve"
	ActionRender  = "render"
	// ActionPublish approves the finished ad and posts it to the business's connected accounts. With
	// an ad plan on it, YES (or Post and run ads) also runs that plan; POST (or Post only) never does.
	ActionPublish = "publish"
	// ActionBoost (the Boost it tap under a posted ad) asks marketflow-api for the quote of the
	// default boost; nothing is spent. ActionBoostLaunch (the Confirm boost tap under that quote)
	// launches it through the portal's own boost checks, PAUSED until Meta's review and the
	// funding (owner, 2026-11-19).
	ActionBoost       = "boost"
	ActionBoostLaunch = "boost_launch"
)

// Approval is one pending WhatsApp approval. A user has at most one: a new code replaces the
// old one, and any message that is not "YES <code>" cancels it.
type Approval struct {
	Code     string `json:"code"`
	Action   string `json:"action"`
	RunID    string `json:"run_id"`
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	// Sender is the verified number the code was sent to; only it may use the code.
	Sender string `json:"sender,omitempty"`
	// VersionID is the Studio video ad a render code renders.
	VersionID string `json:"version_id,omitempty"`
	// Credits is the price the message quoted.
	Credits int `json:"credits,omitempty"`
	// PlanFingerprint is the generation plan a render code approves (the plan the message showed);
	// Studio refuses the render when the plan changed since.
	// A publish code carries the ad plan its message showed ("" for a post only message).
	PlanFingerprint string `json:"plan_fingerprint,omitempty"`
	// PostFingerprint is the post a publish code approves and AdPlatforms the platforms its ad plan
	// showed; marketflow-api refuses a post or an ad plan that changed since.
	PostFingerprint string   `json:"post_fingerprint,omitempty"`
	AdPlatforms     []string `json:"ad_platforms,omitempty"`
	// Tier is the media tier an approval of a cheaper option makes instead of the run's own.
	Tier string `json:"tier,omitempty"`
	// Option names the cheaper option a code approves, for the messages ("" for the plan).
	Option string `json:"option,omitempty"`
	// BoostBudget (whole units of the ad account's currency) and BoostDays are the boost a
	// Confirm boost code launches: exactly what the quote showed.
	BoostBudget int64 `json:"boost_budget,omitempty"`
	BoostDays   int   `json:"boost_days,omitempty"`
	// OTPDone marks an approval whose one-time code was already confirmed (or not needed), so a
	// top-up that resumes it does not ask again.
	OTPDone  bool      `json:"otp_done,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	Created  time.Time `json:"created"`
}

// Code check outcomes. Neither spends anything.
var (
	// ErrNoCode is no pending code: it expired, was used, or was cancelled.
	ErrNoCode = errors.New("osrun: no pending approval code")
	// ErrWrongCode is a code that does not match the pending one (or another number sent it).
	ErrWrongCode = errors.New("osrun: wrong approval code")
	// ErrStaleTap is a tapped button from an older message (another run, or a code that was
	// replaced). It never counts as a wrong try (owner, 2026-11-14); the reply shows the current
	// buttons instead.
	ErrStaleTap = errors.New("osrun: button from an older message")
)

// Codes keeps pending approvals under "<namespace>:os:wa-code:<tenant>:<user>".
type Codes struct {
	rdb      *redis.Client
	ns       string
	ttl      time.Duration
	attempts int
	now      func() time.Time
	// gen makes a code; tests replace it.
	gen func() (string, error)
	// otpCredits, otpTTL and genOTP are the one-time code above a spend (otp.go); topupTTL keeps
	// a Buy credits offer and the action waiting for its credits (topup.go).
	otpCredits int
	otpTTL     time.Duration
	genOTP     func() (string, error)
	topupTTL   time.Duration
}

// NewCodes builds the code store from the config.
func NewCodes(rdb *redis.Client, namespace string, cfg Config) *Codes {
	d := DefaultConfig()
	if cfg.CodeTTL <= 0 {
		cfg.CodeTTL = d.CodeTTL
	}
	if cfg.CodeAttempts <= 0 {
		cfg.CodeAttempts = d.CodeAttempts
	}
	if cfg.OTPTTL <= 0 {
		cfg.OTPTTL = d.OTPTTL
	}
	if cfg.OTPCredits < 0 {
		cfg.OTPCredits = 0
	}
	if cfg.TopupTTL <= 0 {
		cfg.TopupTTL = d.TopupTTL
	}
	return &Codes{rdb: rdb, ns: namespace, ttl: cfg.CodeTTL, attempts: cfg.CodeAttempts, now: time.Now, gen: fourDigits,
		otpCredits: cfg.OTPCredits, otpTTL: cfg.OTPTTL, genOTP: sixDigits, topupTTL: cfg.TopupTTL}
}

// TTL is how long a code works, for the message that carries it.
func (c *Codes) TTL() time.Duration { return c.ttl }

func (c *Codes) key(tenantID, userID string) string {
	return c.ns + ":os:wa-code:" + tenantID + ":" + userID
}

// fourDigits is a random code from 1000 to 9999 (no leading zero to lose in a reply).
func fourDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(9000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", 1000+n.Int64()), nil
}

// Issue stores a new pending approval for the user, replacing any other, and returns its code.
func (c *Codes) Issue(ctx context.Context, a Approval) (string, error) {
	if c == nil || c.rdb == nil {
		return "", errors.New("osrun: no redis")
	}
	if a.TenantID == "" || a.UserID == "" || a.RunID == "" || a.Action == "" {
		return "", errors.New("osrun: tenant, user, run and action are required")
	}
	code, err := c.gen()
	if err != nil {
		return "", err
	}
	a.Code, a.Attempts, a.Created = code, 0, c.now().UTC()
	b, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	if err := c.rdb.Set(ctx, c.key(a.TenantID, a.UserID), b, c.ttl).Err(); err != nil {
		return "", err
	}
	return code, nil
}

// Redeem uses the user's pending code. The right code from the right number returns the
// approval and deletes it (single use; of two racing replies only one wins). A wrong code
// keeps it pending until CodeAttempts wrong tries cancel it.
func (c *Codes) Redeem(ctx context.Context, tenantID, userID, sender, code string) (*Approval, error) {
	return c.RedeemRun(ctx, tenantID, userID, sender, code, "")
}

// RedeemRun is Redeem for a tapped button, whose id names the run as well as the code. A tap whose
// run or code is not the pending one is ErrStaleTap and costs no try: buttons stay on screen after
// a newer message replaced them, and tapping one is not a guess. An empty runID is a typed reply,
// where a wrong code counts as before.
func (c *Codes) RedeemRun(ctx context.Context, tenantID, userID, sender, code, runID string) (*Approval, error) {
	if c == nil || c.rdb == nil {
		return nil, ErrNoCode
	}
	key := c.key(tenantID, userID)
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoCode
	}
	if err != nil {
		return nil, err
	}
	var a Approval
	if err := json.Unmarshal(b, &a); err != nil {
		c.rdb.Del(ctx, key)
		return nil, ErrNoCode
	}
	wrongSender := a.Sender != "" && sender != "" && a.Sender != sender
	if runID != "" && !wrongSender && (a.RunID != runID || a.Code != code) {
		return nil, ErrStaleTap
	}
	if a.Code != code || wrongSender {
		a.Attempts++
		if a.Attempts >= c.attempts {
			c.rdb.Del(ctx, key)
		} else if nb, err := json.Marshal(a); err == nil {
			c.rdb.SetArgs(ctx, key, nb, redis.SetArgs{KeepTTL: true, Mode: "XX"})
		}
		return nil, ErrWrongCode
	}
	n, err := c.rdb.Del(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNoCode // another reply used it first
	}
	return &a, nil
}

// Find returns which of the given tenants holds the user's pending code with this value, sent
// to this number, without using it or counting a try. The WhatsApp worker uses it to route a
// bare "YES <code>" from someone with several businesses whose business pick has expired.
func (c *Codes) Find(ctx context.Context, tenantIDs []string, userID, sender, code string) (string, bool) {
	if c == nil || c.rdb == nil || code == "" || userID == "" {
		return "", false
	}
	for _, tid := range tenantIDs {
		b, err := c.rdb.Get(ctx, c.key(tid, userID)).Bytes()
		if err != nil {
			continue
		}
		var a Approval
		if json.Unmarshal(b, &a) == nil && a.Code == code && (a.Sender == "" || sender == "" || a.Sender == sender) {
			return tid, true
		}
	}
	return "", false
}

// Pending reports whether the user has a code waiting.
func (c *Codes) Pending(ctx context.Context, tenantID, userID string) bool {
	if c == nil || c.rdb == nil {
		return false
	}
	return c.rdb.Exists(ctx, c.key(tenantID, userID)).Val() == 1
}

// Cancel drops the user's pending code, if any.
func (c *Codes) Cancel(ctx context.Context, tenantID, userID string) {
	if c == nil || c.rdb == nil {
		return
	}
	c.rdb.Del(ctx, c.key(tenantID, userID))
}

// yesReply is "YES 1234" and the ways people type it: any case, Swahili "ndio" or "ndiyo",
// punctuation between, or the code alone after the word. "POST 1234" (and Swahili "chapisha")
// is the typed form of Approve and post on a finished ad; the code decides what it does.
var yesReply = regexp.MustCompile(`(?i)^\s*(yes|ndio|ndiyo|post|chapisha)[\s,.:;!-]*(\d*)\s*[.!]*\s*$`)

var postReply = regexp.MustCompile(`(?i)^\s*(post|chapisha)\b`)

// PostOnly reports a typed POST (or Swahili "chapisha"): post the ad, never spend on it.
func PostOnly(text string) bool { return postReply.MatchString(text) }

// ParseYes reads an approval reply. yes is true for a reply that is only the yes word and
// maybe a number; code is the number when it has exactly 4 digits.
func ParseYes(text string) (code string, yes bool) {
	m := yesReply.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", false
	}
	if len(m[2]) == 4 {
		return m[2], true
	}
	return "", true
}
