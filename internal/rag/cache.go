package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// cacheLayer is the multi-tier Redis cache that sits in front of every
// expensive operation (classification, embedding, KB search, response).
//
// Key hierarchy:
//   {ns}:chat:exact:{hash16}      → response (24h)       ← L1 exact match
//   {ns}:chat:sem:{tenant}:{hash16} → response (48h)     ← L2 semantic (embedding-based)
//   {ns}:cls:{hash16}             → ClassificationJSON (7d) ← L3 classification
//   {ns}:emb:{hash16}             → []float32 JSON (24h) ← L4 question embedding
//   {ns}:kb:{tenant}:{hash16}     → []SearchResult JSON (1h) ← L5 KB results
type cacheLayer struct {
	redis     *redis.Client
	namespace string
	logger    *zap.Logger
}

func newCacheLayer(r *redis.Client, ns string, logger *zap.Logger) *cacheLayer {
	return &cacheLayer{redis: r, namespace: ns, logger: logger.Named("cache")}
}

// ── L1: exact response cache ────────────────────────────────────────────

func (c *cacheLayer) exactKey(tenantID uuid.UUID, question string) string {
	h := sha256.Sum256([]byte(tenantID.String() + ":" + normalizeQ(question)))
	return c.namespace + ":chat:exact:" + hex.EncodeToString(h[:16])
}

func (c *cacheLayer) getExact(ctx context.Context, tenantID uuid.UUID, q string) (string, bool) {
	v, err := c.redis.Get(ctx, c.exactKey(tenantID, q)).Result()
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

func (c *cacheLayer) setExact(ctx context.Context, tenantID uuid.UUID, q, response string) {
	_ = c.redis.Set(ctx, c.exactKey(tenantID, q), response, 24*time.Hour).Err()
}

// ── L2: semantic response cache (embedding-based, threshold 0.92) ───────

type semCacheEntry struct {
	Response  string    `json:"r"`
	Embedding []float32 `json:"e"`
}

func (c *cacheLayer) semPrefix(tenantID uuid.UUID) string {
	return c.namespace + ":chat:sem:" + tenantID.String() + ":"
}

func (c *cacheLayer) getSemantic(ctx context.Context, tenantID uuid.UUID, embedding []float32) (string, bool) {
	if len(embedding) == 0 {
		return "", false
	}
	pattern := c.semPrefix(tenantID) + "*"
	keys, err := c.redis.Keys(ctx, pattern).Result()
	if err != nil || len(keys) == 0 {
		return "", false
	}
	// Scan up to 50 recent entries; if any has cosine sim > 0.92, return it.
	limit := 50
	if len(keys) < limit {
		limit = len(keys)
	}
	for _, k := range keys[:limit] {
		raw, err := c.redis.Get(ctx, k).Result()
		if err != nil || raw == "" {
			continue
		}
		var entry semCacheEntry
		if json.Unmarshal([]byte(raw), &entry) != nil || len(entry.Embedding) == 0 {
			continue
		}
		sim := cosineSim(embedding, entry.Embedding)
		if sim >= 0.92 {
			c.logger.Debug("semantic cache hit", zap.Float64("sim", sim), zap.String("key", k))
			return entry.Response, true
		}
	}
	return "", false
}

func (c *cacheLayer) setSemantic(ctx context.Context, tenantID uuid.UUID, q string, embedding []float32, response string) {
	if len(embedding) == 0 {
		return
	}
	h := sha256.Sum256([]byte(tenantID.String() + ":" + normalizeQ(q)))
	key := c.semPrefix(tenantID) + hex.EncodeToString(h[:16])
	entry := semCacheEntry{Response: response, Embedding: embedding}
	data, _ := json.Marshal(entry)
	_ = c.redis.Set(ctx, key, string(data), 48*time.Hour).Err()
}

// ── L3: classification cache ────────────────────────────────────────────

func (c *cacheLayer) clsKey(q string) string {
	h := sha256.Sum256([]byte(normalizeQ(q)))
	return c.namespace + ":cls:" + hex.EncodeToString(h[:16])
}

type clsCache struct {
	Intent        string  `json:"i"`
	Confidence    float64 `json:"c"`
	NeedsLiveInfo bool    `json:"n"`
}

func (c *cacheLayer) getClassification(ctx context.Context, q string) (clsCache, bool) {
	raw, err := c.redis.Get(ctx, c.clsKey(q)).Result()
	if err != nil || raw == "" {
		return clsCache{}, false
	}
	var cc clsCache
	if json.Unmarshal([]byte(raw), &cc) != nil {
		return clsCache{}, false
	}
	return cc, true
}

func (c *cacheLayer) setClassification(ctx context.Context, q string, cc clsCache) {
	data, _ := json.Marshal(cc)
	_ = c.redis.Set(ctx, c.clsKey(q), string(data), 7*24*time.Hour).Err()
}

// ── L4: embedding cache ─────────────────────────────────────────────────

func (c *cacheLayer) embKey(q string) string {
	h := sha256.Sum256([]byte(normalizeQ(q)))
	return c.namespace + ":emb:" + hex.EncodeToString(h[:16])
}

func (c *cacheLayer) getEmbedding(ctx context.Context, q string) ([]float32, bool) {
	raw, err := c.redis.Get(ctx, c.embKey(q)).Result()
	if err != nil || raw == "" {
		return nil, false
	}
	var emb []float32
	if json.Unmarshal([]byte(raw), &emb) != nil {
		return nil, false
	}
	return emb, true
}

func (c *cacheLayer) setEmbedding(ctx context.Context, q string, emb []float32) {
	data, _ := json.Marshal(emb)
	_ = c.redis.Set(ctx, c.embKey(q), string(data), 24*time.Hour).Err()
}

// ── L5: KB search result cache ──────────────────────────────────────────

func (c *cacheLayer) kbKey(tenantID uuid.UUID, embHash string) string {
	return c.namespace + ":kb:" + tenantID.String() + ":" + embHash
}

func (c *cacheLayer) getKBResults(ctx context.Context, tenantID uuid.UUID, emb []float32) ([]SearchResult, bool) {
	h := sha256.Sum256([]byte(fmt.Sprintf("%v", emb[:min(8, len(emb))])))
	key := c.kbKey(tenantID, hex.EncodeToString(h[:8]))
	raw, err := c.redis.Get(ctx, key).Result()
	if err != nil || raw == "" {
		return nil, false
	}
	var results []SearchResult
	if json.Unmarshal([]byte(raw), &results) != nil {
		return nil, false
	}
	return results, true
}

func (c *cacheLayer) setKBResults(ctx context.Context, tenantID uuid.UUID, emb []float32, results []SearchResult) {
	h := sha256.Sum256([]byte(fmt.Sprintf("%v", emb[:min(8, len(emb))])))
	key := c.kbKey(tenantID, hex.EncodeToString(h[:8]))
	data, _ := json.Marshal(results)
	_ = c.redis.Set(ctx, key, string(data), 1*time.Hour).Err()
}

// ── helpers ─────────────────────────────────────────────────────────────

func normalizeQ(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (sqrt(normA) * sqrt(normB))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
