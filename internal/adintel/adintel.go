// Package adintel finds competitor ads that have run for weeks (Sprint 24). Nobody keeps paying
// for an ad that does not make money, so a long-running ad is a winner worth learning from. The
// primary source is the Google Ads Transparency Center through SerpApi; its contract and quota
// rules are in marketflow/docs/integrations/competitor-ad-intel-apis.md.
//
// Owner rule (2026-10-22): SerpApi stays on its free 250 searches a month unless the platform
// owner turns paid use on. The quota is counted in code before every call, each tenant gets a
// fair share, and results are cached for 7 days and shared, since they are public ad data.
package adintel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bengobox/marketflow-ai/internal/llm"
)

// Ad is one competitor creative.
type Ad struct {
	AdvertiserID string    `json:"advertiser_id"`
	Advertiser   string    `json:"advertiser"`
	CreativeID   string    `json:"ad_creative_id"`
	Format       string    `json:"format"`
	FirstShown   time.Time `json:"first_shown"`
	LastShown    time.Time `json:"last_shown"`
	DaysShown    int       `json:"total_days_shown"`
	DetailsLink  string    `json:"details_link"`
	Score        float64   `json:"score"`
	StillRunning bool      `json:"still_running"`
	// Tags are the decide labels for a top winner (tags.go). The ad's words never travel with it.
	Tags *Tags `json:"tags,omitempty"`
}

// ErrQuota means the platform's searches for the month are used, so nobody can search.
var ErrQuota = errors.New("competitor scan quota used; it resumes on the 1st")

// ErrTenantShare means this tenant used its own share for the month. Other tenants still search.
var ErrTenantShare = errors.New("this business's competitor searches for the month are used; they resume on the 1st")

// Limits.
const (
	FreeMonthlySearches = 250
	DefaultTenantShare  = 5
	CacheTTL            = 7 * 24 * time.Hour
	// RegionKenya is the Transparency Center's numeric region for Kenya.
	RegionKenya = "2404"
)

// Settings are the platform provider row adintel/serpapi on Platform, Providers.
type Settings struct {
	Key string
	// PaidCap replaces the free 250 when the platform owner turned paid use on (0 = free only).
	PaidCap int
	// TenantShare is each tenant's searches a month (0 = DefaultTenantShare).
	TenantShare int
	// Details is the separate budget for ad-details reads, the call that returns a winner's
	// words for tagging (details.go).
	Details DetailSettings
	// SummaryClass is the model class of the winners' summary for the Ads Specialist (summary.go),
	// from the row's summary_model_class; empty means nano.
	SummaryClass llm.Class
}

// summaryClass is the configured class, nano when unset or unknown.
func (cfg Settings) summaryClass() llm.Class {
	for _, c := range llm.Classes {
		if c == cfg.SummaryClass {
			return c
		}
	}
	return llm.Nano
}

// SummaryClass is the model class the scan's summary uses now.
func (s *Searcher) SummaryClass(ctx context.Context) llm.Class {
	return s.Settings(ctx).summaryClass()
}

// poolLimit is the month's platform pool: the free 250, or the paid cap the owner set.
func (cfg Settings) poolLimit() int {
	if cfg.PaidCap > 0 {
		return cfg.PaidCap
	}
	return FreeMonthlySearches
}

// Searcher is the SerpApi client.
type Searcher struct {
	Base     string // https://serpapi.com
	HTTP     *http.Client
	Redis    *redis.Client
	NS       string
	Settings func(ctx context.Context) Settings
	now      func() time.Time
}

func NewSearcher(settings func(ctx context.Context) Settings, rdb *redis.Client, namespace string) *Searcher {
	return &Searcher{Base: "https://serpapi.com", HTTP: &http.Client{Timeout: 30 * time.Second}, Redis: rdb, NS: namespace, Settings: settings, now: time.Now}
}

// Search returns the advertiser's or query's ads in a region, winners first. A cached answer
// costs nothing; a fresh one costs one search from the platform pool and the tenant's share.
func (s *Searcher) Search(ctx context.Context, tenantID, query, region string) ([]Ad, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("a competitor name, domain or advertiser id is required")
	}
	if region == "" {
		region = RegionKenya
	}
	cacheKey := fmt.Sprintf("%s:adintel:cache:%s:%s", s.NS, region, strings.ToLower(query))
	if b, err := s.Redis.Get(ctx, cacheKey).Bytes(); err == nil {
		var ads []Ad
		if json.Unmarshal(b, &ads) == nil {
			return ads, nil
		}
	}
	cfg := s.Settings(ctx)
	if cfg.Key == "" {
		return nil, fmt.Errorf("competitor scans are not configured")
	}
	release, err := s.reserve(ctx, tenantID, cfg)
	if err != nil {
		return nil, err
	}
	ads, err := s.fetch(ctx, cfg.Key, query, region)
	if err != nil {
		release() // an errored search is not counted by SerpApi, so it is given back here too
		return nil, err
	}
	if b, err := json.Marshal(ads); err == nil {
		s.Redis.Set(ctx, cacheKey, b, CacheTTL)
	}
	return ads, nil
}

// reserve takes one search from the month's platform pool and the tenant's share; release gives
// both back.
func (s *Searcher) reserve(ctx context.Context, tenantID string, cfg Settings) (func(), error) {
	return reserveQuota(ctx, s.Redis, s.NS, "serpapi", tenantID, s.now(), 1, cfg.poolLimit(), cfg.tenantShare())
}

// tenantShare is the tenant's searches a month, DefaultTenantShare when unset.
func (cfg Settings) tenantShare() int {
	if cfg.TenantShare <= 0 {
		return DefaultTenantShare
	}
	return cfg.TenantShare
}

// quotaTTL keeps a month's counters a little past the month's end.
const quotaTTL = 40 * 24 * time.Hour

// poolKey and tenantKey are the Redis counters of a provider's month: the platform pool and one
// tenant's share. Quota (quota.go) reads the same keys.
func poolKey(ns, provider, month string) string {
	return fmt.Sprintf("%s:adintel:%s:month:%s", ns, provider, month)
}

func tenantKey(ns, provider, tenantID, month string) string {
	return fmt.Sprintf("%s:adintel:%s:tenant:%s:%s", ns, provider, tenantID, month)
}

// counter is one monthly limit a reservation must fit under, and the error for going over it.
type counter struct {
	key   string
	limit int
	over  error
}

// reserveCounters takes n units from every counter in order, counted in Redis before any outbound
// call. Going over one gives back what was taken and returns that counter's error. release gives
// all the units back, for a call that failed or was not made.
func reserveCounters(ctx context.Context, rdb *redis.Client, n int, counters ...counter) (func(), error) {
	units := int64(n)
	taken := make([]string, 0, len(counters))
	undo := func(c context.Context) {
		for _, k := range taken {
			rdb.DecrBy(c, k, units)
		}
	}
	for _, c := range counters {
		used, err := rdb.IncrBy(ctx, c.key, units).Result()
		if err != nil {
			undo(ctx)
			return nil, err
		}
		taken = append(taken, c.key)
		rdb.Expire(ctx, c.key, quotaTTL)
		if used > int64(c.limit) {
			undo(ctx)
			return nil, c.over
		}
	}
	return func() { undo(context.WithoutCancel(ctx)) }, nil
}

// reserveQuota takes n units from a provider's monthly platform pool and the tenant's monthly
// share. Over the pool is ErrQuota (nobody can search); over the share is ErrTenantShare (only
// this tenant stops).
func reserveQuota(ctx context.Context, rdb *redis.Client, ns, provider, tenantID string, now time.Time, n, limit, share int) (func(), error) {
	month := now.UTC().Format("2006-01")
	return reserveCounters(ctx, rdb, n,
		counter{key: poolKey(ns, provider, month), limit: limit, over: ErrQuota},
		counter{key: tenantKey(ns, provider, tenantID, month), limit: share, over: ErrTenantShare})
}

// fetch reads the first page only: one search, up to 40 ads, is enough to find the winners.
func (s *Searcher) fetch(ctx context.Context, key, query, region string) ([]Ad, error) {
	q := url.Values{"engine": {"google_ads_transparency_center"}, "region": {region}, "api_key": {key}}
	if strings.HasPrefix(query, "AR") && !strings.ContainsAny(query, " .") {
		q.Set("advertiser_id", query)
	} else {
		q.Set("text", query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/search.json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out struct {
		Error       string `json:"error"`
		AdCreatives []struct {
			AdvertiserID string `json:"advertiser_id"`
			Advertiser   string `json:"advertiser"`
			CreativeID   string `json:"ad_creative_id"`
			Format       string `json:"format"`
			FirstShown   int64  `json:"first_shown"`
			LastShown    int64  `json:"last_shown"`
			DaysShown    int    `json:"total_days_shown"`
			DetailsLink  string `json:"details_link"`
		} `json:"ad_creatives"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return nil, fmt.Errorf("serpapi %d: %s", resp.StatusCode, out.Error)
		}
		return nil, fmt.Errorf("serpapi status %d", resp.StatusCode)
	}
	ads := make([]Ad, 0, len(out.AdCreatives))
	for _, c := range out.AdCreatives {
		ads = append(ads, Ad{AdvertiserID: c.AdvertiserID, Advertiser: c.Advertiser, CreativeID: c.CreativeID, Format: c.Format,
			FirstShown: time.Unix(c.FirstShown, 0).UTC(), LastShown: time.Unix(c.LastShown, 0).UTC(), DaysShown: c.DaysShown, DetailsLink: c.DetailsLink})
	}
	Rank(ads, s.now())
	return ads, nil
}

// Rank scores and sorts winners in code (Jev never computes dates): days shown counts most, an
// ad still running counts double, and an advertiser running many variants of a format is
// testing, so each variant adds a little.
func Rank(ads []Ad, now time.Time) {
	variants := map[string]int{}
	for _, a := range ads {
		variants[a.AdvertiserID+"|"+a.Format]++
	}
	for i := range ads {
		a := &ads[i]
		days := a.DaysShown
		if days == 0 && !a.FirstShown.IsZero() && a.LastShown.After(a.FirstShown) {
			days = int(a.LastShown.Sub(a.FirstShown).Hours()/24) + 1
		}
		a.StillRunning = !a.LastShown.IsZero() && now.Sub(a.LastShown) < 3*24*time.Hour
		score := float64(days)
		if a.StillRunning {
			score *= 2
		}
		score += float64(variants[a.AdvertiserID+"|"+a.Format]-1) * 0.5
		a.Score = score
	}
	sort.SliceStable(ads, func(i, j int) bool { return ads[i].Score > ads[j].Score })
}
