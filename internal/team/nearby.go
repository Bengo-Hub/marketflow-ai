package team

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// The nearby lookup (owner, 2026-11-24): besides the platform's landmark registry, the setting
// brief may read what the web says about the landmarks and street character near the business's
// address. It is one web_search call per tenant and address, cached in Redis so replicas and
// restarts share it, inside a short budget, and its text reaches the Strategist as data only.
// marketflow-api turns it on or off (service config creative.landmark_lookup).

// NearbyCache stores the lookups (Redis in production; nil keeps no cache and skips the lookup,
// so an uncached runtime never spends the web search quota on every script).
type NearbyCache interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, value string, ttl time.Duration)
}

const (
	nearbyTTL      = 7 * 24 * time.Hour // a town's landmarks change slowly
	nearbyEmptyTTL = 24 * time.Hour     // nothing found: try again tomorrow, not every script
	nearbyChars    = 600
)

var nearbyBudget = 8 * time.Second

// nearbyNotes is the cached web lookup of the landmarks and street life near the address, or "".
func (r *Runtime) nearbyNotes(ctx context.Context, tenantID uuid.UUID, p PlaceFacts) string {
	addr := strings.TrimSpace(p.Address)
	tool, ok := r.tools["web_search"]
	if !p.LookupNearby || addr == "" || !ok || r.nearby == nil {
		return ""
	}
	h := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(addr+" "+p.City), " "))))
	key := fmt.Sprintf("creative:nearby:%s:%x", tenantID, h[:8])
	if v, ok := r.nearby.Get(ctx, key); ok {
		return v
	}
	where := addr
	if p.City != "" && !strings.Contains(strings.ToLower(addr), strings.ToLower(p.City)) {
		where += ", " + p.City
	}
	args, _ := json.Marshal(map[string]any{"query": "well-known landmarks, buildings and street life near " + where, "max_results": 4})
	sctx, cancel := context.WithTimeout(ctx, nearbyBudget)
	defer cancel()
	out, err := tool.Invoke(sctx, tenantID, string(args))
	if err != nil {
		r.logger.Info("nearby lookup skipped", zap.Error(err))
		return "" // quota or network: not cached, the daily cap bounds the retries
	}
	notes := nearbyText(out)
	ttl := nearbyTTL
	if notes == "" {
		ttl = nearbyEmptyTTL
	}
	r.nearby.Set(ctx, key, notes, ttl)
	return notes
}

// nearbyText keeps the answer and the snippets of a web_search reply, within nearbyChars.
func nearbyText(raw string) string {
	var res struct {
		Answer  string `json:"answer"`
		Results []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(raw), &res) != nil {
		return ""
	}
	parts := []string{strings.TrimSpace(res.Answer)}
	for _, x := range res.Results {
		parts = append(parts, strings.TrimSpace(firstNonEmptyStr(x.Content, x.Snippet)))
	}
	text := strings.Join(strings.Fields(strings.Join(nonEmptyStrings(parts...), " | ")), " ")
	return trimTo(text, nearbyChars)
}

// RedisNearbyCache keeps the lookups in Redis under the service's namespace; a nil client gives
// nil (no lookup).
func RedisNearbyCache(c *redis.Client, namespace string) NearbyCache {
	if c == nil {
		return nil
	}
	return redisNearby{c: c, ns: namespace}
}

type redisNearby struct {
	c  *redis.Client
	ns string
}

func (r redisNearby) Get(ctx context.Context, key string) (string, bool) {
	v, err := r.c.Get(ctx, r.ns+":"+key).Result()
	return v, err == nil
}

func (r redisNearby) Set(ctx context.Context, key, value string, ttl time.Duration) {
	_ = r.c.Set(ctx, r.ns+":"+key, value, ttl).Err()
}
