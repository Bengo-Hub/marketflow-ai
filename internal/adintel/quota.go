package adintel

import (
	"context"
)

// QuotaUse is one monthly budget: what is used and the limit, for the platform and one tenant.
type QuotaUse struct {
	PlatformUsed  int `json:"platform_used"`
	PlatformLimit int `json:"platform_limit"`
	TenantUsed    int `json:"tenant_used"`
	TenantLimit   int `json:"tenant_limit"`
	// TenantLeft is what this tenant can still use this month: its share, capped by the pool.
	TenantLeft int `json:"tenant_left"`
}

// Quota is this month's competitor scan budget and details budget, read from the same Redis
// counters the reservations use. It makes no outbound call.
type Quota struct {
	Month   string   `json:"month"`
	Scan    QuotaUse `json:"scan"`
	Details QuotaUse `json:"details"`
}

// Quota reads the month's counters for a tenant.
func (s *Searcher) Quota(ctx context.Context, tenantID string) Quota {
	cfg := s.Settings(ctx)
	month := s.now().UTC().Format("2006-01")
	read := func(key string) int {
		n, err := s.Redis.Get(ctx, key).Int()
		if err != nil || n < 0 {
			return 0
		}
		return n
	}
	pool := read(poolKey(s.NS, "serpapi", month))
	scan := use(pool, cfg.poolLimit(), read(tenantKey(s.NS, "serpapi", tenantID, month)), cfg.tenantShare())
	details := use(read(poolKey(s.NS, "serpapi_details", month)), cfg.Details.PlatformMonthly,
		read(tenantKey(s.NS, "serpapi_details", tenantID, month)), cfg.Details.PerTenant)
	// Details also count in the SerpApi pool, so they stop when the pool is used.
	if left := cfg.poolLimit() - pool; details.TenantLeft > left {
		details.TenantLeft = max(left, 0)
	}
	return Quota{Month: month, Scan: scan, Details: details}
}

func use(platformUsed, platformLimit, tenantUsed, tenantLimit int) QuotaUse {
	left := min(tenantLimit-tenantUsed, platformLimit-platformUsed)
	return QuotaUse{PlatformUsed: platformUsed, PlatformLimit: platformLimit, TenantUsed: tenantUsed, TenantLimit: tenantLimit, TenantLeft: max(left, 0)}
}
