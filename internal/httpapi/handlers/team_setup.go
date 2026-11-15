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
	if req.ApprovePlan || req.Playbook != "" {
		return false
	}
	setupper, ok := h.source.(team.TeamSetupper)
	if !ok {
		return false
	}
	if !req.TeamSetup {
		learner, _ := h.source.(team.PhraseLearner)
		if isTeam, _ := h.runtime.TeamIntent(ctx, tenantID, req.Text, learner); !isTeam {
			return false
		}
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
	out := h.orgChartReply(ctx, tenantID, runID, setup)
	if setup.First {
		h.refund(r, tenantID, turnKey)
	} else if turnKey != "" {
		out.CreditsCharged = 1
	}
	sink.done(out)
	return true
}

// orgChartReply is the team turn's answer: the org chart, with the names the owner chose.
func (h *OSHandler) orgChartReply(ctx context.Context, tenantID uuid.UUID, runID string, setup team.TeamSetup) askResponse {
	var overrides []team.MemberOverride
	if tc, err := h.source.Load(ctx, tenantID); err == nil {
		overrides = tc.Members
	}
	return askResponse{RunID: runID, Result: &team.Result{Route: team.RouteTeam}, Answer: team.OrgChart(h.lib, setup, overrides)}
}

// firstTeamSetup runs before any credit is checked, so a tenant with no AI credits still gets its
// free first setup from the console (Sprint 21, open since 2026-11-08). It only handles a request
// that is plainly about the team (the console's team_setup button, a fixed pattern or a learned
// phrasing; never a paid decision) and only while the tenant has had no setup: marketflow-api's
// only_first answers 409 otherwise, and the request then goes through the normal charged turn,
// where a review costs 1 credit. Nothing is charged here, so there is nothing to refund.
// handled is false when the request should continue to the charged turn.
func (h *OSHandler) firstTeamSetup(ctx context.Context, r *http.Request, tenantID uuid.UUID, req askRequest, runID string) (out askResponse, status int, msg string, handled bool) {
	if req.ApprovePlan || req.Playbook != "" {
		return out, 0, "", false
	}
	first, ok := h.source.(team.FirstTeamSetupper)
	if !ok {
		return out, 0, "", false
	}
	if !req.TeamSetup {
		learner, _ := h.source.(team.PhraseLearner)
		if !team.KnownTeamRequest(ctx, tenantID, req.Text, learner) {
			return out, 0, "", false
		}
	}
	bearer := ""
	if !req.viaWhatsApp {
		bearer = strings.TrimSpace(r.Header.Get("Authorization"))
	}
	setup, err := first.SetupFirstTeam(ctx, tenantID, bearer)
	switch {
	case errors.Is(err, team.ErrTeamAlreadySetUp):
		return out, 0, "", false
	case errors.Is(err, team.ErrTeamForbidden):
		return out, http.StatusForbidden, err.Error(), true
	case err != nil:
		h.logger.Warn("first team setup failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return out, http.StatusBadGateway, "the marketing team could not be set up just now; please try again shortly", true
	}
	return h.orgChartReply(ctx, tenantID, runID, setup), 0, "", true
}
