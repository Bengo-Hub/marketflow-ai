package osrun

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// A boost at or above the tenant's threshold needs the 6-digit code marketflow-api sends and
// checks itself (adsmanager boost_otp.go). marketflow-ai only remembers which confirmed boost the
// code is for, so the next 6-digit reply from the same number finishes that boost. The code
// itself is never stored here.

// PendingBoostOTP is a confirmed boost waiting for marketflow-api's code.
type PendingBoostOTP struct {
	Approval Approval  `json:"approval"`
	OTPID    string    `json:"otp_id"`
	Created  time.Time `json:"created"`
}

func (c *Codes) boostOTPKey(tenantID, userID string) string {
	return c.ns + ":os:wa-boost-otp:" + tenantID + ":" + userID
}

// HoldBoostOTP keeps the boost waiting for its code for ttl (marketflow-api's code lifetime).
func (c *Codes) HoldBoostOTP(ctx context.Context, a Approval, otpID string, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return errors.New("osrun: no redis")
	}
	if a.TenantID == "" || a.UserID == "" || otpID == "" {
		return errors.New("osrun: tenant, user and otp id are required")
	}
	b, err := json.Marshal(PendingBoostOTP{Approval: a, OTPID: otpID, Created: c.now().UTC()})
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, c.boostOTPKey(a.TenantID, a.UserID), b, ttl).Err()
}

// BoostOTP returns the boost waiting for a code from this number, without using it up: a wrong
// code keeps it waiting (marketflow-api counts the tries and ends it).
func (c *Codes) BoostOTP(ctx context.Context, tenantID, userID, sender string) (*PendingBoostOTP, bool) {
	if c == nil || c.rdb == nil {
		return nil, false
	}
	b, err := c.rdb.Get(ctx, c.boostOTPKey(tenantID, userID)).Bytes()
	if errors.Is(err, redis.Nil) || err != nil {
		return nil, false
	}
	var p PendingBoostOTP
	if json.Unmarshal(b, &p) != nil {
		return nil, false
	}
	if p.Approval.Sender != "" && sender != "" && p.Approval.Sender != sender {
		return nil, false
	}
	return &p, true
}

// DropBoostOTP ends the wait (the boost went ahead, or the code can no longer work).
func (c *Codes) DropBoostOTP(ctx context.Context, tenantID, userID string) {
	if c == nil || c.rdb == nil {
		return
	}
	c.rdb.Del(ctx, c.boostOTPKey(tenantID, userID))
}
