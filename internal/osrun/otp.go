package osrun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// One-time codes above a spend (Sprint 17): an approval that quotes at least Config.OTPCredits
// credits needs, after the tap or the typed YES, a 6-digit code that notifications-api sends to
// the same verified number by the auth_otp template. The code is bound to the tenant, the user,
// the number and the action hash (what exactly is being approved), and works for Config.OTPTTL.
// Only a hash of the code is stored.

// PendingOTP is an approval waiting for its one-time code.
type PendingOTP struct {
	Approval   Approval  `json:"approval"`
	CodeHash   string    `json:"code_hash"`
	ActionHash string    `json:"action_hash"`
	Attempts   int       `json:"attempts,omitempty"`
	Created    time.Time `json:"created"`
}

// ActionHash identifies exactly what an approval does: the action, the tenant, the user, the
// run, the version, the tier and the credits. A one-time code confirms this hash and nothing else.
func ActionHash(a Approval) string {
	parts := []string{a.Action, a.TenantID, a.UserID, a.RunID, a.VersionID, a.Tier, strconv.Itoa(a.Credits)}
	if a.BoostBudget != 0 || a.BoostDays != 0 {
		// Only boosts carry these, so the hash of every other approval stays as it was.
		parts = append(parts, strconv.FormatInt(a.BoostBudget, 10), strconv.Itoa(a.BoostDays))
	}
	s := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func otpHash(code, actionHash string) string {
	sum := sha256.Sum256([]byte(code + "|" + actionHash))
	return hex.EncodeToString(sum[:])
}

// sixDigits is a random code from 100000 to 999999.
func sixDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", 100000+n.Int64()), nil
}

func (c *Codes) otpKey(tenantID, userID string) string {
	return c.ns + ":os:wa-otp:" + tenantID + ":" + userID
}

// OTPTTL is how long a one-time code works, for the message that announces it.
func (c *Codes) OTPTTL() time.Duration { return c.otpTTL }

// NeedsOTP reports whether an approval also needs a one-time code: its spend reaches the
// threshold and no code was confirmed for it yet.
func (c *Codes) NeedsOTP(a Approval) bool {
	return c != nil && c.otpCredits > 0 && a.Credits >= c.otpCredits && !a.OTPDone
}

// IssueOTP keeps the approval waiting for a one-time code and returns the code for
// notifications-api to send. A newer one replaces it.
func (c *Codes) IssueOTP(ctx context.Context, a Approval) (string, error) {
	if c == nil || c.rdb == nil {
		return "", errors.New("osrun: no redis")
	}
	if a.TenantID == "" || a.UserID == "" || a.Action == "" {
		return "", errors.New("osrun: tenant, user and action are required")
	}
	code, err := c.genOTP()
	if err != nil {
		return "", err
	}
	ah := ActionHash(a)
	p := PendingOTP{Approval: a, CodeHash: otpHash(code, ah), ActionHash: ah, Created: c.now().UTC()}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if err := c.rdb.Set(ctx, c.otpKey(a.TenantID, a.UserID), b, c.otpTTL).Err(); err != nil {
		return "", err
	}
	return code, nil
}

// OTPPending reports whether the user has an approval waiting for its one-time code.
func (c *Codes) OTPPending(ctx context.Context, tenantID, userID string) bool {
	if c == nil || c.rdb == nil {
		return false
	}
	return c.rdb.Exists(ctx, c.otpKey(tenantID, userID)).Val() == 1
}

// CancelOTP drops a waiting one-time code.
func (c *Codes) CancelOTP(ctx context.Context, tenantID, userID string) {
	if c == nil || c.rdb == nil {
		return
	}
	c.rdb.Del(ctx, c.otpKey(tenantID, userID))
}

// RedeemOTP confirms the one-time code. The right code from the right number returns the
// approval, marked OTPDone, and deletes the record (single use). A wrong code counts a try;
// CodeAttempts wrong tries cancel it. An approval whose action hash no longer matches is refused.
func (c *Codes) RedeemOTP(ctx context.Context, tenantID, userID, sender, code string) (*Approval, error) {
	if c == nil || c.rdb == nil {
		return nil, ErrNoCode
	}
	key := c.otpKey(tenantID, userID)
	b, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoCode
	}
	if err != nil {
		return nil, err
	}
	var p PendingOTP
	if json.Unmarshal(b, &p) != nil || p.ActionHash != ActionHash(p.Approval) {
		c.rdb.Del(ctx, key)
		return nil, ErrNoCode
	}
	wrongSender := p.Approval.Sender != "" && sender != "" && p.Approval.Sender != sender
	if wrongSender || subtle.ConstantTimeCompare([]byte(otpHash(code, p.ActionHash)), []byte(p.CodeHash)) != 1 {
		p.Attempts++
		if p.Attempts >= c.attempts {
			c.rdb.Del(ctx, key)
		} else if nb, err := json.Marshal(p); err == nil {
			c.rdb.SetArgs(ctx, key, nb, redis.SetArgs{KeepTTL: true, Mode: "XX"})
		}
		return nil, ErrWrongCode
	}
	n, err := c.rdb.Del(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNoCode
	}
	a := p.Approval
	a.OTPDone = true
	return &a, nil
}

// otpReply is a 6-digit one-time code as people type it: alone, spaced in two groups, or after
// "code" or "otp".
var otpReply = regexp.MustCompile(`(?i)^\s*(?:otp|code|nambari)?[\s:.-]*(\d{3})\s?-?(\d{3})\s*[.!]?\s*$`)

// ParseOTP reads a one-time code reply; ok is false for anything else.
func ParseOTP(text string) (string, bool) {
	m := otpReply.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", false
	}
	return m[1] + m[2], true
}
