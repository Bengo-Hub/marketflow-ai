package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/team"
)

type fieldSuggestRequest struct {
	Field    string            `json:"field"`
	Form     map[string]string `json:"form"`
	Language string            `json:"language"`
}

// FieldSuggest handles POST /api/v1/os/field-suggest: "Generate with AI" for one field of the
// public profile or brand context (owner, 2026-11-11). It runs on the free model tier and the
// OS route's per-user rate limit, so it spends no AI credits; nothing is saved.
func (h *OSHandler) FieldSuggest(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	if h.runtime == nil {
		writeError(w, http.StatusServiceUnavailable, "the assistant is not configured")
		return
	}
	var req fieldSuggestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Form) > 30 {
		writeError(w, http.StatusBadRequest, "too many form fields")
		return
	}
	out, err := h.runtime.SuggestField(r.Context(), team.FieldSuggestRequest{
		TenantID: tenantID, Field: strings.TrimSpace(req.Field), Form: req.Form, Language: req.Language,
	})
	switch {
	case errors.Is(err, team.ErrUnknownField):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, team.ErrSuggestionBlocked):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		h.logger.Warn("field suggestion failed", zap.String("field", req.Field), zap.Error(err))
		writeError(w, http.StatusBadGateway, "the assistant could not write this field just now; try again")
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
