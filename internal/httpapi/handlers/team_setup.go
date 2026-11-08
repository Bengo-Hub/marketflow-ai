package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/team"
)

// teamTurn answers "set up my marketing team" and "show my team" without a model: marketflow-api
// fits the prebuilt team to the business, and the reply is the org chart. The first setup is free
// (the turn's credit is refunded); a later review costs the normal turn (owner, 2026-11-08).
// It reports whether it handled the request.
func (h *OSHandler) teamTurn(ctx context.Context, r *http.Request, tenantID uuid.UUID, req askRequest, runID, turnKey string, sink askSink) bool {
	if req.ApprovePlan || req.Playbook != "" || !team.IsTeamRequest(req.Text) {
		return false
	}
	setupper, ok := h.source.(team.TeamSetupper)
	if !ok {
		return false
	}
	sink.progress(team.Progress{Stage: "team", Member: "manager", Label: "Setting up your marketing team"})
	bearer := ""
	if !req.viaWhatsApp {
		// The portal route checks the user's own role; WhatsApp senders were verified upstream.
		bearer = strings.TrimSpace(r.Header.Get("Authorization"))
	}
	setup, err := setupper.SetupTeam(ctx, tenantID, bearer)
	if err != nil {
		h.refund(r, tenantID, turnKey)
		if errors.Is(err, team.ErrTeamForbidden) {
			sink.fail(http.StatusForbidden, err.Error())
			return true
		}
		h.logger.Warn("team setup failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		sink.fail(http.StatusBadGateway, "the marketing team could not be set up just now; please try again shortly")
		return true
	}
	var overrides []team.MemberOverride
	if tc, err := h.source.Load(ctx, tenantID); err == nil {
		overrides = tc.Members
	}
	out := askResponse{RunID: runID, Result: &team.Result{Route: team.RouteTeam}, Answer: team.OrgChart(h.lib, setup, overrides)}
	if setup.First {
		h.refund(r, tenantID, turnKey)
	} else if turnKey != "" {
		out.CreditsCharged = 1
	}
	sink.done(out)
	return true
}
