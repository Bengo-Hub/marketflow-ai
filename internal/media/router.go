package media

import (
	"sort"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/breaker"
)

// Capability is a kind of media job.
type Capability string

// CapPosterImage is the text-free scene a poster is composed on.
const CapPosterImage Capability = "poster_image"

// Tiers set the quality floor a route must meet.
const (
	TierDraft    = "draft"
	TierStandard = "standard"
	TierPremium  = "premium"
)

var tierFloor = map[string]float64{TierDraft: 2, TierStandard: 3, TierPremium: 4}

// Route is one way to do a capability: a provider endpoint with its price and
// a provisional quality score (1-5) until the golden-brief eval sets it.
type Route struct {
	Key        string
	Provider   string // fal | higgsfield
	Capability Capability
	PriceUSD   float64 // per output, the price the cost engine quotes against
	Quality    float64
	PAYG       bool          // pay as you go (preferred) or subscription
	Timeout    time.Duration // per attempt
	MinTier    string        // the lowest tier that may use it ("" = any)
}

// weights from docs/integrations/video-generation-providers.md: quality, cost,
// measured reliability and the billing model (pay as you go preferred). The
// owner pays for quality on premium and for speed and price on drafts.
type weights struct{ quality, cost, reliability, billing float64 }

var tierWeights = map[string]weights{
	TierDraft:    {0.25, 0.50, 0.15, 0.10},
	TierStandard: {0.40, 0.35, 0.15, 0.10},
	TierPremium:  {0.60, 0.15, 0.15, 0.10},
}

// Router orders a capability's routes by score, keeping routes whose breaker is
// open behind the healthy ones.
type Router struct {
	routes []Route
	br     *breaker.Set

	mu    sync.Mutex
	stats map[string]*outcomes
}

// outcomes is an exponentially weighted success rate (1.0 with no data).
type outcomes struct{ rate float64 }

// NewRouter builds a router over routes.
func NewRouter(routes []Route) *Router {
	return &Router{routes: routes, br: breaker.New(3, time.Minute, 15*time.Minute), stats: map[string]*outcomes{}}
}

// Order returns the routes to try for cap at tier, best first. Routes below the
// tier's quality floor, or restricted to higher tiers, are left out.
func (r *Router) Order(cap Capability, tier string, available func(Route) bool) []Route {
	floor := tierFloor[tier]
	var eligible []Route
	cheapest := 0.0
	for _, rt := range r.routes {
		if rt.Capability != cap || rt.Quality < floor || !tierAllows(tier, rt.MinTier) {
			continue
		}
		if available != nil && !available(rt) {
			continue // no key configured, or the provider's balance is below its mark
		}
		eligible = append(eligible, rt)
		if cheapest == 0 || rt.PriceUSD < cheapest {
			cheapest = rt.PriceUSD
		}
	}
	w, ok := tierWeights[tier]
	if !ok {
		w = tierWeights[TierStandard]
	}
	scores := make(map[string]float64, len(eligible))
	for _, rt := range eligible {
		scores[rt.Key] = r.score(rt, cheapest, w)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return scores[eligible[i].Key] > scores[eligible[j].Key] })

	healthy := make([]Route, 0, len(eligible))
	var open []Route
	for _, rt := range eligible {
		if r.br.Admit(rt.Key) {
			healthy = append(healthy, rt)
		} else {
			open = append(open, rt)
		}
	}
	return append(healthy, open...)
}

func (r *Router) score(rt Route, cheapest float64, w weights) float64 {
	cost := 1.0
	if rt.PriceUSD > 0 {
		cost = cheapest / rt.PriceUSD
	}
	billing := 0.5
	if rt.PAYG {
		billing = 1
	}
	return w.quality*(rt.Quality/5) + w.cost*cost + w.reliability*r.reliability(rt.Key) + w.billing*billing
}

func (r *Router) reliability(key string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if o := r.stats[key]; o != nil {
		return o.rate
	}
	return 1
}

// Succeeded records a good attempt.
func (r *Router) Succeeded(key string) {
	r.br.Succeeded(key)
	r.observe(key, 1)
}

// Failed records a failed attempt; callerGaveUp is true when the job's own
// budget ran out, which says nothing about the route.
func (r *Router) Failed(key string, err error, callerGaveUp bool) {
	r.br.Failed(key, err, callerGaveUp)
	if !callerGaveUp {
		r.observe(key, 0)
	}
}

func (r *Router) observe(key string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.stats[key]
	if o == nil {
		o = &outcomes{rate: 1}
		r.stats[key] = o
	}
	o.rate = 0.8*o.rate + 0.2*v
}

func tierAllows(tier, min string) bool {
	rank := map[string]int{"": 0, TierDraft: 0, TierStandard: 1, TierPremium: 2}
	return rank[tier] >= rank[min]
}
