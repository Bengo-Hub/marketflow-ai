package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.uber.org/zap"
)

// falModelsBase is fal's model list (tests point it at a fake server).
var falModelsBase = "https://api.fal.ai/v1/models"

// FalCatalog is fal's model list (about 1,400 endpoints), read with the platform's fal key and
// kept for falCatalogTTL, so a configured route model can be checked without a call per render.
// Lookups never wait for it: a stale or empty list starts a refresh in the background and the
// lookup answers from what is held (known false until the first read succeeds).
type FalCatalog struct {
	keys   KeySource
	http   *http.Client
	logger *zap.Logger

	mu       sync.Mutex
	models   map[string]string // endpoint id -> category
	readAt   time.Time
	loading  bool
	nextTry  time.Time
	failures int
}

const (
	falCatalogTTL = 6 * time.Hour
	// falCatalogPages bounds one read (100 a page); falCatalogBudget bounds its total time.
	falCatalogPages  = 30
	falCatalogBudget = 2 * time.Minute
	// A failed read is tried again after a backoff from falCatalogRetryMin, doubling to
	// falCatalogRetryMax, with jitter.
	falCatalogRetryMin = 5 * time.Minute
	falCatalogRetryMax = time.Hour
)

// NewFalCatalog builds the catalogue; nothing is read until the first lookup.
func NewFalCatalog(keys KeySource, logger *zap.Logger) *FalCatalog {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &FalCatalog{keys: keys, http: &http.Client{Timeout: 20 * time.Second}, logger: logger.Named("media.fal-catalog")}
}

// Lookup answers from the held list and starts a background refresh when it is stale.
func (c *FalCatalog) Lookup(id string) (category string, listed, known bool) {
	if c == nil {
		return "", false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	if c.models == nil {
		return "", false, false
	}
	cat, ok := c.models[id]
	return cat, ok, true
}

// refreshLocked starts one background read when the list is stale and no read or backoff is
// pending. c.mu is held.
func (c *FalCatalog) refreshLocked() {
	now := time.Now()
	if c.loading || now.Before(c.nextTry) || (c.models != nil && now.Sub(c.readAt) < falCatalogTTL) {
		return
	}
	c.loading = true
	go c.load()
}

func (c *FalCatalog) load() {
	ctx, cancel := context.WithTimeout(context.Background(), falCatalogBudget)
	defer cancel()
	models, err := c.read(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loading = false
	if err != nil {
		c.failures++
		wait := falCatalogRetryMin << min(c.failures-1, 4)
		if wait > falCatalogRetryMax {
			wait = falCatalogRetryMax
		}
		wait += time.Duration(rand.Int64N(int64(wait / 5)))
		c.nextTry = time.Now().Add(wait)
		c.logger.Warn("fal model list unavailable; configured media routes are checked by format only until it is read",
			zap.Error(err), zap.Int("failures", c.failures), zap.Duration("retry_in", wait))
		return
	}
	c.models, c.readAt, c.failures, c.nextTry = models, time.Now(), 0, time.Time{}
	c.logger.Info("fal model list read", zap.Int("models", len(models)))
}

// read pages through fal's list.
func (c *FalCatalog) read(ctx context.Context) (map[string]string, error) {
	key := ""
	if c.keys != nil {
		key = c.keys(ctx, "fal")
	}
	if key == "" {
		return nil, fmt.Errorf("no fal key")
	}
	out := map[string]string{}
	cursor := ""
	for page := 0; page < falCatalogPages; page++ {
		u := falModelsBase + "?limit=100"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Key "+key)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fal models: status %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		var v struct {
			Models []struct {
				EndpointID string `json:"endpoint_id"`
				Metadata   struct {
					Category string `json:"category"`
				} `json:"metadata"`
			} `json:"models"`
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, fmt.Errorf("fal models: unreadable page: %w", err)
		}
		for _, m := range v.Models {
			if m.EndpointID != "" {
				out[m.EndpointID] = m.Metadata.Category
			}
		}
		if !v.HasMore || v.NextCursor == "" {
			return out, nil
		}
		cursor = v.NextCursor
	}
	return out, nil
}

// setForTest fills the list directly.
func (c *FalCatalog) setForTest(models map[string]string) {
	c.mu.Lock()
	c.models, c.readAt = models, time.Now()
	c.mu.Unlock()
}
