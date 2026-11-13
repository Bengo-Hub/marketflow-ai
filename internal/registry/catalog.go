package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The AI credit bundles on sale are owned by subscriptions-api (its credits catalog, billing
// registry `credit_services.go`). The registry's own Bundles are only the fallback for when the
// catalog cannot be read, so a re-priced or retired bundle never needs a change here.

// aiCreditsService is the catalog's service code for AI credits.
const aiCreditsService = "ai_credits"

// CatalogBundles reads the AI credit bundles on sale from subscriptions-api's public credits
// catalog (GET {base}/api/v1/credits/catalog), smallest first.
func CatalogBundles(ctx context.Context, hc *http.Client, base string) ([]Bundle, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, fmt.Errorf("registry: no subscriptions address")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/credits/catalog", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("credits catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("credits catalog: status %d", resp.StatusCode)
	}
	var cat struct {
		Services []struct {
			Service string   `json:"service"`
			Bundles []Bundle `json:"bundles"`
		} `json:"services"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cat); err != nil {
		return nil, fmt.Errorf("credits catalog: decode: %w", err)
	}
	for _, s := range cat.Services {
		if s.Service != aiCreditsService {
			continue
		}
		out := make([]Bundle, 0, len(s.Bundles))
		for _, b := range s.Bundles {
			if b.Code != "" && b.Units > 0 && b.PriceKES > 0 {
				out = append(out, b)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("credits catalog: no AI credit bundles on sale")
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Units < out[j].Units })
		return out, nil
	}
	return nil, fmt.Errorf("credits catalog: no %s service", aiCreditsService)
}
