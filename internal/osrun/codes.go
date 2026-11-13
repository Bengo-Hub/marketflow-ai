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
	Credits  int       `json:"credits,omitempty"`
	Attempts int       `json:"attempts,omitempty"`
	Created  time.Time `json:"created"`
}

// Code check outcomes. Neither spends anything.
var (
	// ErrNoCode is no pending code: it expired, was used, or was cancelled.
	ErrNoCode = errors.New("osrun: no pending approval code")
	// ErrWrongCode is a code that does not match the pending one (or another number sent it).
	ErrWrongCode = errors.New("osrun: wrong approval code")
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
	return &Codes{rdb: rdb, ns: namespace, ttl: cfg.CodeTTL, attempts: cfg.CodeAttempts, now: time.Now, gen: fourDigits}
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
	if a.Code != code || (a.Sender != "" && sender != "" && a.Sender != sender) {
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
// punctuation between, or the code alone after the word.
var yesReply = regexp.MustCompile(`(?i)^\s*(yes|ndio|ndiyo)[\s,.:;!-]*(\d*)\s*[.!]*\s*$`)

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
