package rag

import (
	"context"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Retrieval counts (Sprint 25): how often each route's answers were rated helpful, per business
// and question kind. Only the direct knowledge base route acts on them: it skips the model, so it
// is the one a bad record can switch off. Every other route keeps its routing.
const (
	RouteKBDirect   = "kb_direct"
	RouteCache      = "cache"
	RouteKBFallback = "kb_fallback"
	RouteModel      = "model"

	// minRatings is how many ratings a route needs before its record counts.
	minRatings = 10
	// trustedShare is the helpful share below which the direct route is skipped.
	trustedShare = 0.5
	// exploreShare of questions still try a distrusted route, so it can earn trust back.
	exploreShare = 0.1
	// statsTTL lets a business's counts lapse after three months without ratings.
	statsTTL = 90 * 24 * time.Hour
)

// RouteOf names the route that produced an answer, from ChatResult.Model.
func RouteOf(model string) string {
	switch model {
	case ModelKnowledgeBase:
		return RouteKBDirect
	case ModelKBFallback:
		return RouteKBFallback
	case "cache", "semantic_cache":
		return RouteCache
	default:
		return RouteModel
	}
}

func (p *Pipeline) statsKey(tenantID uuid.UUID) string {
	return p.redisNamespace + ":retrieval:" + tenantID.String()
}

// RecordRating counts a rating against the route and question kind that produced the answer.
// prev is the earlier rating ("up", "down" or ""), so changing a rating moves one count.
func (p *Pipeline) RecordRating(ctx context.Context, tenantID uuid.UUID, intent, route, prev string, helpful bool) {
	if p.redis == nil || intent == "" || route == "" {
		return
	}
	now := "down"
	if helpful {
		now = "up"
	}
	if prev == now {
		return
	}
	key := p.statsKey(tenantID)
	field := intent + "|" + route + "|"
	pipe := p.redis.TxPipeline()
	pipe.HIncrBy(ctx, key, field+now, 1)
	if prev == "up" || prev == "down" {
		pipe.HIncrBy(ctx, key, field+prev, -1)
	}
	pipe.Expire(ctx, key, statsTTL)
	_, _ = pipe.Exec(ctx)
}

// routeTrusted reports whether a route may answer this question kind for this business. A route
// without enough ratings is trusted; a distrusted one is still tried on a share of questions. Any
// Redis error leaves the route trusted, so the counts can only ever narrow the direct route.
func (p *Pipeline) routeTrusted(ctx context.Context, tenantID uuid.UUID, intent, route string) bool {
	if p.redis == nil || intent == "" {
		return true
	}
	rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	field := intent + "|" + route + "|"
	vals, err := p.redis.HMGet(rctx, p.statsKey(tenantID), field+"up", field+"down").Result()
	if err != nil || len(vals) != 2 {
		return true
	}
	up, down := count(vals[0]), count(vals[1])
	return trusted(up, down, rand.Float64())
}

// trusted is the rule, separate so it can be tested: few ratings or a helpful record trust the
// route; otherwise it answers only the exploring share.
func trusted(up, down int64, roll float64) bool {
	if up+down < minRatings || float64(up)/float64(up+down) >= trustedShare {
		return true
	}
	return roll < exploreShare
}

func count(v any) int64 {
	s, _ := v.(string)
	n, _ := strconv.ParseInt(s, 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
