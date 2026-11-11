// Package fxrate reads exchange rates from treasury-api, the platform's one source of rates
// (fetched daily from the configured forex provider and cached there). MarketFlow's credit
// engine uses KES per USD to price provider costs; it never keeps its own rate.
package fxrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// cacheFor is how long a fetched rate is reused; treasury updates its rates daily.
const cacheFor = 6 * time.Hour

// A KES per USD outside this range is a misconfiguration (for example no USD rate in treasury,
// which returns the amount unchanged), never a real rate.
const minKESPerUSD, maxKESPerUSD = 50, 500

// Source reads rates from treasury-api over S2S.
type Source struct {
	base, key, tenant string
	rdb               *redis.Client
	ns                string
	http              *http.Client
}

func New(treasuryURL, serviceKey, platformTenant string, rdb *redis.Client, namespace string) *Source {
	return &Source{base: strings.TrimRight(treasuryURL, "/"), key: serviceKey, tenant: platformTenant,
		rdb: rdb, ns: namespace, http: &http.Client{Timeout: 5 * time.Second}}
}

// KESPerUSD is the current rate: cached, else fetched from treasury, else the last good rate.
// ok is false when no rate has ever been read, and the caller keeps its own default.
func (s *Source) KESPerUSD(ctx context.Context) (float64, bool) {
	if s == nil || s.base == "" || s.key == "" {
		return 0, false
	}
	fresh, last := s.ns+":fx:USD:KES", s.ns+":fx:USD:KES:last"
	if r, ok := s.cached(ctx, fresh); ok {
		return r, true
	}
	if r, err := s.fetch(ctx); err == nil {
		if s.rdb != nil {
			v := strconv.FormatFloat(r, 'f', -1, 64)
			_ = s.rdb.Set(ctx, fresh, v, cacheFor).Err()
			_ = s.rdb.Set(ctx, last, v, 0).Err()
		}
		return r, true
	}
	return s.cached(ctx, last)
}

func (s *Source) cached(ctx context.Context, key string) (float64, bool) {
	if s.rdb == nil {
		return 0, false
	}
	v, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return 0, false
	}
	r, err := strconv.ParseFloat(v, 64)
	return r, err == nil && r >= minKESPerUSD && r <= maxKESPerUSD
}

func (s *Source) fetch(ctx context.Context) (float64, error) {
	q := url.Values{"from": {"USD"}, "to": {"KES"}, "amount": {"1"}}
	u := fmt.Sprintf("%s/api/v1/s2s/%s/exchange-rates/convert?%s", s.base, url.PathEscape(s.tenant), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", s.key)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("treasury convert: status %d", resp.StatusCode)
	}
	var out struct {
		Converted json.Number `json:"converted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	r, err := out.Converted.Float64()
	if err != nil {
		return 0, err
	}
	if r < minKESPerUSD || r > maxKESPerUSD {
		return 0, fmt.Errorf("treasury convert: implausible KES per USD %v", r)
	}
	return r, nil
}
