package adintel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Ad details (Sprint 24 decide tags). The Transparency Center search returns no ad copy, so the
// words of a top winner come from the separate ad-details engine, one search per creative. They
// are read only to tag the winner (tags.go): the words are cached here for 30 days, shared across
// tenants because they are public ad data, and never leave this package or reach a tenant's record.
//
// Budget (decided 2026-11-18, sized to cost nothing): details have their own monthly limits on the
// adintel/serpapi provider row, details_platform_monthly and details_per_tenant, and every details
// read also counts in the SerpApi pool, so the account never passes its free 250 and the scans
// always keep the rest of the pool. A used budget skips quietly: those winners stay untagged and
// the scan never fails.

// Detail defaults, used when the provider row leaves a field unset.
const (
	DefaultDetailsPerTenant       = 3
	DefaultDetailsPlatformMonthly = 50
	DefaultDetailsK               = 3
	detailsCacheTTL               = 30 * 24 * time.Hour
	maxDetailRunes                = 400
)

// DetailSettings is the details budget. A zero PerTenant, PlatformMonthly or K turns details off.
type DetailSettings struct {
	PerTenant       int
	PlatformMonthly int
	// K is how many top winners per tenant scan are read and tagged.
	K int
}

// DefaultDetailSettings is the budget when the provider row sets none of it.
func DefaultDetailSettings() DetailSettings {
	return DetailSettings{PerTenant: DefaultDetailsPerTenant, PlatformMonthly: DefaultDetailsPlatformMonthly, K: DefaultDetailsK}
}

func (d DetailSettings) enabled() bool { return d.PerTenant > 0 && d.PlatformMonthly > 0 && d.K > 0 }

// adText is a winner's words: third-party text, kept only in the cache and quoted before any model
// sees it.
type adText struct {
	Headline string `json:"headline,omitempty"`
	Snippet  string `json:"snippet,omitempty"`
	CTA      string `json:"cta,omitempty"`
}

func (t adText) empty() bool { return t.Headline == "" && t.Snippet == "" && t.CTA == "" }

func (t adText) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []struct{ label, v string }{{"headline", t.Headline}, {"text", t.Snippet}, {"call to action", t.CTA}} {
		if p.v != "" {
			parts = append(parts, p.label+": "+p.v)
		}
	}
	return clip(strings.Join(parts, " | "), maxDetailRunes)
}

func detailsCacheKey(ns, creativeID string) string {
	return fmt.Sprintf("%s:adintel:details:%s", ns, creativeID)
}

// reserveDetails takes one details read: from the SerpApi pool, the details pool and the tenant's
// details share.
func (s *Searcher) reserveDetails(ctx context.Context, tenantID string, cfg Settings) (func(), error) {
	month := s.now().UTC().Format("2006-01")
	return reserveCounters(ctx, s.Redis, 1,
		counter{key: poolKey(s.NS, "serpapi", month), limit: cfg.poolLimit(), over: ErrQuota},
		counter{key: poolKey(s.NS, "serpapi_details", month), limit: cfg.Details.PlatformMonthly, over: ErrQuota},
		counter{key: tenantKey(s.NS, "serpapi_details", tenantID, month), limit: cfg.Details.PerTenant, over: ErrTenantShare})
}

// topWinners picks the K best-scored ads across the scan, the ones worth reading.
func topWinners(ads []*Ad, k int) []*Ad {
	sorted := make([]*Ad, 0, len(ads))
	for _, a := range ads {
		if a != nil && a.CreativeID != "" && a.AdvertiserID != "" {
			sorted = append(sorted, a)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	if len(sorted) > k {
		sorted = sorted[:k]
	}
	return sorted
}

// readText returns a winner's words: cached for free, or one details read when the budget allows.
// ok is false when they cannot be had now (budget used, no key, a failed call); never an error.
func (s *Searcher) readText(ctx context.Context, cfg Settings, tenantID, region string, ad *Ad) (adText, bool) {
	key := detailsCacheKey(s.NS, ad.CreativeID)
	if b, err := s.Redis.Get(ctx, key).Bytes(); err == nil {
		var t adText
		if json.Unmarshal(b, &t) == nil {
			return t, !t.empty()
		}
	}
	if cfg.Key == "" {
		return adText{}, false
	}
	release, err := s.reserveDetails(ctx, tenantID, cfg)
	if err != nil {
		return adText{}, false
	}
	t, err := s.fetchDetails(ctx, cfg.Key, ad.AdvertiserID, ad.CreativeID, region)
	if err != nil {
		release()
		return adText{}, false
	}
	// An ad with no words (an image or video without text) is cached too, so it is not read again.
	if b, err := json.Marshal(t); err == nil {
		s.Redis.Set(ctx, key, b, detailsCacheTTL)
	}
	return t, !t.empty()
}

// fetchDetails calls the ad-details engine. SerpApi's own tutorial reads ad_creatives[] with
// headline (falling back to title), snippet and call_to_action; missing fields are tolerated.
func (s *Searcher) fetchDetails(ctx context.Context, key, advertiserID, creativeID, region string) (adText, error) {
	q := url.Values{"engine": {"google_ads_transparency_center_ad_details"}, "advertiser_id": {advertiserID},
		"creative_id": {creativeID}, "api_key": {key}}
	if region != "" {
		q.Set("region", region)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/search.json?"+q.Encode(), nil)
	if err != nil {
		return adText{}, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return adText{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return adText{}, fmt.Errorf("serpapi details status %d", resp.StatusCode)
	}
	var out struct {
		AdCreatives []struct {
			Headline     string `json:"headline"`
			Title        string `json:"title"`
			Snippet      string `json:"snippet"`
			CallToAction string `json:"call_to_action"`
		} `json:"ad_creatives"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return adText{}, err
	}
	var t adText
	for _, c := range out.AdCreatives {
		if t.Headline == "" {
			t.Headline = clip(firstNonEmpty(c.Headline, c.Title), maxDetailRunes)
		}
		if t.Snippet == "" {
			t.Snippet = clip(c.Snippet, maxDetailRunes)
		}
		if t.CTA == "" {
			t.CTA = clip(c.CallToAction, 60)
		}
	}
	return t, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// TagWinners reads the words of the scan's top K winners, within the details budget, and tags
// them with one decide call (tags.go). It sets Tags on the ads it could tag and returns how many.
// Budget used, no decider or a failed decision all leave ads untagged: never an error.
func (s *Searcher) TagWinners(ctx context.Context, d Decider, tenantID, region, brand string, ads []*Ad) int {
	if d == nil {
		return 0
	}
	cfg := s.Settings(ctx)
	if !cfg.Details.enabled() {
		return 0
	}
	if region == "" {
		region = RegionKenya
	}
	var items []tagItem
	for _, a := range topWinners(ads, cfg.Details.K) {
		if t, ok := s.readText(ctx, cfg, tenantID, region, a); ok {
			items = append(items, tagItem{ad: a, text: t})
		}
	}
	return tagAds(ctx, d, brand, items)
}
