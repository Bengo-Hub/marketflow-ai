package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/bengobox/marketflow-ai/internal/adintel"
	"go.uber.org/zap"
)

// AdIntelHandler serves the competitor scan (Sprint 24).
type AdIntelHandler struct {
	searcher *adintel.Searcher
	logger   *zap.Logger
}

func NewAdIntelHandler(s *adintel.Searcher, logger *zap.Logger) *AdIntelHandler {
	return &AdIntelHandler{searcher: s, logger: logger.Named("adintel")}
}

// maxScanQueries bounds one scan; a tenant's monthly share is far smaller anyway.
const maxScanQueries = 10

// Scan handles POST /api/v1/internal/adintel/scan {tenant_id, queries, region} (S2S, the weekly
// job): each competitor's winners, top 10. A used quota stops the scan and says so; cached
// answers are free.
func (h *AdIntelHandler) Scan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID string   `json:"tenant_id"`
		Queries  []string `json:"queries"`
		Region   string   `json:"region"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.TenantID == "" || len(req.Queries) == 0 {
		writeError(w, http.StatusBadRequest, "tenant_id and queries are required")
		return
	}
	if len(req.Queries) > maxScanQueries {
		req.Queries = req.Queries[:maxScanQueries]
	}
	type result struct {
		Query string       `json:"query"`
		Ads   []adintel.Ad `json:"ads,omitempty"`
		Error string       `json:"error,omitempty"`
	}
	out := struct {
		Results   []result `json:"results"`
		QuotaUsed bool     `json:"quota_used"`
	}{}
	for _, q := range req.Queries {
		ads, err := h.searcher.Search(r.Context(), req.TenantID, q, req.Region)
		if errors.Is(err, adintel.ErrQuota) {
			out.QuotaUsed = true
			break
		}
		res := result{Query: q}
		if err != nil {
			h.logger.Warn("competitor scan failed", zap.String("tenant", req.TenantID), zap.Error(err))
			res.Error = err.Error()
		} else {
			if len(ads) > 10 {
				ads = ads[:10]
			}
			res.Ads = ads
		}
		out.Results = append(out.Results, res)
	}
	writeJSON(w, http.StatusOK, out)
}
