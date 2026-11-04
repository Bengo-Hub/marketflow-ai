package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ChargeCredits handles POST /api/v1/internal/credits/charge (S2S, marketflow-api): spends AI
// credits for work marketflow-api starts, such as a paid ad launch (Sprint 18), through the same
// subscriptions-api spender as every other charge. The key makes a repeat call free, so a retried
// launch is never charged twice.
func (h *OSHandler) ChargeCredits(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TenantID    string `json:"tenant_id"`
		Units       int64  `json:"units"`
		Key         string `json:"key"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tid, err := uuid.Parse(req.TenantID)
	if err != nil || req.Units <= 0 || req.Units > 1000 || strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, "tenant_id, units (1 to 1000) and key are required")
		return
	}
	if h.credits == nil {
		writeJSON(w, http.StatusOK, CreditDecision{Allowed: true, Exempt: true})
		return
	}
	dec, err := h.spend(r.Context(), tid, req.Units, req.Key, req.Description)
	if err != nil {
		h.logger.Warn("internal credit charge failed", zap.String("tenant_id", tid.String()), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "AI credits could not be checked right now")
		return
	}
	writeJSON(w, http.StatusOK, dec)
}
