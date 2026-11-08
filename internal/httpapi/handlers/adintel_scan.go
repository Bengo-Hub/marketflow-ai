package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-ai/internal/adintel"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AdIntelHandler serves the competitor scan (Sprint 24) and the complaint report (Fix step).
type AdIntelHandler struct {
	searcher   *adintel.Searcher
	complaints *adintel.Complaints
	credits    CreditSpender
	logger     *zap.Logger
}

// WithCredits charges for fresh complaint reports; nil leaves them free (local development).
func (h *AdIntelHandler) WithCredits(c CreditSpender) *AdIntelHandler {
	h.credits = c
	return h
}

func NewAdIntelHandler(s *adintel.Searcher, logger *zap.Logger) *AdIntelHandler {
	return &AdIntelHandler{searcher: s, logger: logger.Named("adintel")}
}

// WithComplaints wires the Serper complaint report.
func (h *AdIntelHandler) WithComplaints(c *adintel.Complaints) *AdIntelHandler {
	h.complaints = c
	return h
}

// Complaints handles POST /api/v1/internal/adintel/complaints {tenant_id, query, country} (S2S,
// marketflow-api for the owner's request): complaint themes in the niche, for the owner to confirm
// before any theme shapes an ad.
func (h *AdIntelHandler) Complaints(w http.ResponseWriter, r *http.Request) {
	if h.complaints == nil {
		writeError(w, http.StatusServiceUnavailable, "complaint search is not configured")
		return
	}
	var req struct {
		TenantID string `json:"tenant_id"`
		Query    string `json:"query"`
		Country  string `json:"country"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.TenantID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id and query are required")
		return
	}
	tid, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	// A fresh report costs real searches, so it costs one credit; a cached one is free. The key
	// is per tenant, query and month, so a retry within the month is never charged twice.
	key := ""
	if h.credits != nil && !h.complaints.Cached(r.Context(), req.Query, req.Country) {
		key = fmt.Sprintf("complaints:%s:%s:%s", tid, time.Now().UTC().Format("2006-01"), complaintKey(req.Query, req.Country))
		dec, err := h.credits.Spend(r.Context(), tid, complaintReportCredits, key)
		if err != nil {
			h.logger.Warn("complaint report credit check failed", zap.String("tenant", req.TenantID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "AI credits could not be checked right now, try again shortly")
			return
		}
		if !dec.Allowed {
			writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": "AI credits are used up. Buy a bundle from your billing page to keep going."})
			return
		}
	}
	report, err := h.complaints.Report(r.Context(), req.TenantID, req.Query, req.Country)
	if key != "" && (err != nil || len(report.Themes) == 0) {
		// Nothing was produced, so nothing is owed.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		if rerr := h.credits.Refund(rctx, tid, key); rerr != nil {
			h.logger.Warn("complaint report refund failed", zap.String("tenant", req.TenantID), zap.Error(rerr))
		}
		cancel()
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, report)
	case errors.Is(err, adintel.ErrQuota), errors.Is(err, adintel.ErrTenantShare):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		h.logger.Warn("complaint report failed", zap.String("tenant", req.TenantID), zap.Error(err))
		writeError(w, http.StatusBadGateway, "the complaint search failed; try again later")
	}
}

// complaintReportCredits is the price of one fresh complaint report.
const complaintReportCredits = 1

func complaintKey(query, country string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(query)) + "|" + strings.ToLower(country)))
	return hex.EncodeToString(sum[:8])
}

// maxScanQueries bounds one scan; a tenant's monthly share is far smaller anyway.
const maxScanQueries = 10

// Scan handles POST /api/v1/internal/adintel/scan {tenant_id, queries, region} (S2S, the weekly
// job): each competitor's winners, top 10. Cached answers are free. A used platform pool stops the
// scan with quota_used, which stops the job for every tenant. A used tenant share affects only this
// tenant, with share_used: cached queries are still answered, and the others are left out of
// results, so the job keeps their earlier winners.
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
		ShareUsed bool     `json:"share_used,omitempty"`
	}{Results: []result{}}
	for _, q := range req.Queries {
		ads, err := h.searcher.Search(r.Context(), req.TenantID, q, req.Region)
		if errors.Is(err, adintel.ErrQuota) {
			out.QuotaUsed = true
			break
		}
		if errors.Is(err, adintel.ErrTenantShare) {
			// Later queries may still be cached, which is free; uncached ones stop here too.
			out.ShareUsed = true
			continue
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
