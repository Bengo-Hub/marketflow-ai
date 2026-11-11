// Package chatallowance prices chat turns (owner decision 2026-11-11): each business gets a free
// daily allowance of chat turns across the website widget, the portal and WhatsApp lead replies;
// after it, one AI credit covers each block of TurnsPerCredit turns. Chat runs on the free
// model tier first, so turns cost the platform almost nothing.
package chatallowance

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Defaults: 50 free turns a day per business, then 1 credit per 5 turns.
const (
	DefaultFreePerDay     = 50
	DefaultTurnsPerCredit = 5
)

// Allowance counts a business's chat turns per UTC day.
type Allowance struct {
	rdb            *redis.Client
	ns             string
	freePerDay     int64
	turnsPerCredit int64
}

func New(rdb *redis.Client, namespace string, freePerDay, turnsPerCredit int) *Allowance {
	if turnsPerCredit < 1 {
		turnsPerCredit = 1
	}
	return &Allowance{rdb: rdb, ns: namespace, freePerDay: int64(freePerDay), turnsPerCredit: int64(turnsPerCredit)}
}

// NeedsCredit counts one turn and reports whether it must be paid for: false inside the daily
// allowance and for the rest of an already paid block. Without Redis, or on a Redis error,
// every turn is charged (the behaviour before the allowance), so a fault never gives chat away.
func (a *Allowance) NeedsCredit(ctx context.Context, tenantID uuid.UUID, now time.Time) bool {
	return a.NeedsCreditFor(ctx, tenantID, now, 0)
}

// NeedsCreditFor is NeedsCredit with the business's own free turns a day (its plan's
// chat_turns_daily); 0 or less keeps the default allowance.
func (a *Allowance) NeedsCreditFor(ctx context.Context, tenantID uuid.UUID, now time.Time, freePerDay int) bool {
	if a == nil || a.rdb == nil {
		return true
	}
	key := fmt.Sprintf("%s:chat-turns:%s:%s", a.ns, tenantID, now.UTC().Format("20060102"))
	n, err := a.rdb.Incr(ctx, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		_ = a.rdb.Expire(ctx, key, 48*time.Hour).Err()
	}
	free := a.freePerDay
	if freePerDay > 0 {
		free = int64(freePerDay)
	}
	if n <= free {
		return false
	}
	return (n-free-1)%a.turnsPerCredit == 0
}
