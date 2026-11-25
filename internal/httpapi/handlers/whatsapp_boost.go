package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/osrun"
)

// Boost from WhatsApp (Sprint 17 step 6, owner 2026-11-19). Under a posted ad the owner taps
// Boost it; that code only fetches a quote. Under the quote, Confirm boost launches it. Both are
// osrun approval codes (single use, bound to the run, the user and the number), so nothing here
// can be started by what a message says. marketflow-api picks the boost (approved video, ad
// account, Page, the tenant's defaults) and runs the portal's own quote and boost checks; the
// launch stays PAUSED until Meta's review and the funding.

// Run ads (marketflow item 64, owner 2026-11-24): the Run ads tap now fetches the tenant's whole
// ad plan (Meta, Google Ads, TikTok with budgets, days, total, funding and links) from
// marketflow-api, and Confirm and run ads runs exactly that plan, bound by its fingerprint in the
// approval code. A Confirm code issued before this change (no fingerprint) still launches the
// Meta-only boost it quoted.

const (
	btnBoost        = "Run ads"
	btnConfirmBoost = "Confirm and run ads"
	// The Run ads routes on marketflow-api.
	runAdsPlanPath = "/api/v1/internal/ads/os-turn/run-ads-plan"
	runAdsPath     = "/api/v1/internal/ads/os-turn/run-ads"
	runAdsOTPPath  = "/api/v1/internal/ads/os-turn/run-ads-otp"
	// boostOTPWait is how long a boost waits for marketflow-api's code (its code lasts 10 minutes).
	boostOTPWait = 10 * time.Minute
)

// boostOffer issues the Boost it code for a posted ad. ok is false when no code could be issued.
func (h *OSHandler) boostOffer(ctx context.Context, a *osrun.Approval) (code string, ok bool) {
	if h.codes == nil || a.VersionID == "" {
		return "", false
	}
	code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionBoost, RunID: a.RunID, TenantID: a.TenantID,
		UserID: a.UserID, Sender: a.Sender, VersionID: a.VersionID})
	if err != nil {
		h.logger.Warn("boost code not issued", zap.String("run_id", a.RunID), zap.Error(err))
		return "", false
	}
	return code, true
}

// boostRefusal is the reply for a refusal both boost steps share; ok is false for anything else.
func boostRefusal(err error, studio string) (string, bool) {
	var se *studioError
	if !errors.As(err, &se) {
		return "", false
	}
	switch {
	case se.Code == "ads_disabled":
		return "Paid boosts are not switched on for MarketFlow yet, so nothing was spent. Your post is still live.", true
	case se.Code == "no_ad_account":
		return "Connect your Meta ad account in MarketFlow first (Settings, Integrations), then tap " + btnBoost + " again. Nothing was spent.", true
	case se.Code == "not_approved":
		return "Approve the video first; ads run only for an approved video. Nothing was spent.", true
	case se.Code == "run_ads_off":
		return "Run ads is not available yet, so nothing was spent. Your post is still live.", true
	case se.Code == "no_page":
		return "Connect the Facebook Page the ad runs as in MarketFlow first (Settings, Integrations). Nothing was spent.", true
	case se.Code == "no_video":
		return "That video is not available any more, so nothing was spent. Boost another one from the Studio: " + studio, true
	case se.Status == http.StatusForbidden:
		return "Only the business owner or a MarketFlow admin can boost ads, so nothing was spent.", true
	case se.Status < 500 && se.Message != "":
		return "The boost could not go ahead: " + trimRunes(se.Message, 300) + ". Nothing was spent. You can boost it from the Studio: " + studio, true
	}
	return "", false
}

// money writes a whole-unit amount with its currency: KES 3,000.
func money(v int64, currency string) string {
	s := kes(float64(v))
	if c := strings.ToUpper(strings.TrimSpace(currency)); c != "" && c != "KES" {
		s = c + strings.TrimPrefix(s, "KES")
	}
	return s
}

// runAdsPlan is marketflow-api's default Run ads plan with its message.
type runAdsPlan struct {
	Text string `json:"text"`
	// Summary is the plan in a few lines, for a message with buttons.
	Summary     string   `json:"summary"`
	Ready       bool     `json:"ready"`
	Fingerprint string   `json:"fingerprint"`
	Platforms   []string `json:"platforms"`
}

// whatsAppBoostQuote answers Run ads: the tenant's ad plan, and a Confirm and run ads code bound
// to exactly that plan.
func (h *OSHandler) whatsAppBoostQuote(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	if h.studio == nil || a.VersionID == "" {
		out.Reply = "I cannot run ads for that video from here. Open Run ads in the Studio: " + studio
		return
	}
	var p runAdsPlan
	err := h.studio.internalFor(ctx, planTimeout, runAdsPlanPath, map[string]any{"tenant_id": tenantID.String(),
		"user_id": a.UserID, "version_id": a.VersionID}, &p)
	if err != nil {
		if msg, ok := boostRefusal(err, studio); ok {
			out.Reply = msg
			return
		}
		h.logger.Warn("whatsapp run ads plan failed", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not prepare the ad plan just now. Nothing was spent; try again in a minute or open Run ads in the Studio: " + studio
		return
	}
	if !p.Ready || p.Fingerprint == "" {
		out.Reply = trimRunes(p.Text, 3000) + "\n\nFix the points above, or change the plan, in the Studio: " + studio
		return
	}
	code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionBoostLaunch, RunID: a.RunID, TenantID: a.TenantID,
		UserID: a.UserID, Sender: a.Sender, VersionID: a.VersionID, PlanFingerprint: p.Fingerprint,
		AdPlatforms: p.Platforms})
	if err != nil {
		h.logger.Warn("run ads code not issued", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not prepare the ad plan just now, so nothing was spent. Please try again in a minute."
		return
	}
	text := p.Text
	if len([]rune(text)) > 900 && p.Summary != "" {
		text = p.Summary // every amount, account and goal in fewer words
	}
	text = trimRunes(text, 900)
	out.Reply = trimRunes(p.Text, 3000) + fmt.Sprintf("\n\nReply YES %s within %s to run it. Studio: %s", code, minutes(h.codes.TTL()), studio)
	out.Interactive = &waInteractive{
		Body: text + "\n\n" + approveLine(btnConfirmBoost, code, "run this plan", 0),
		Buttons: []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, a.RunID, code), Title: btnConfirmBoost},
			{ID: osrun.ReplyID(osrun.ReplyCancel, a.RunID, ""), Title: btnCancel}},
	}
}

// boostResult is marketflow-api's answer to a launched boost (the portal's boost view).
type boostResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Quote  struct {
		Shortfall   int64  `json:"shortfall"`
		Currency    string `json:"currency"`
		AccountID   string `json:"ad_account_id"`
		AccountName string `json:"account_name"`
	} `json:"quote"`
}

// metaBilling is where the business funds its ad account on Meta (the portal's metaBillingURL).
func metaBilling(adAccountID string) string {
	return "https://business.facebook.com/billing_hub/payment_settings/?asset_id=" + strings.TrimPrefix(adAccountID, "act_")
}

// whatsAppBoostLaunch answers Confirm and run ads (and the code after it, when one was needed).
func (h *OSHandler) whatsAppBoostLaunch(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, otpID, otpCode string, out *WhatsAppAskResponse) {
	if a.PlanFingerprint != "" {
		h.whatsAppRunAds(ctx, tenantID, req, a, otpID, otpCode, out)
		return
	}
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	if h.studio == nil || a.VersionID == "" || a.BoostBudget <= 0 || a.BoostDays <= 0 {
		out.Reply = "I cannot launch that boost from here. Boost it from the Studio: " + studio
		return
	}
	body := map[string]any{"tenant_id": tenantID.String(), "user_id": a.UserID, "version_id": a.VersionID,
		"budget": a.BoostBudget, "days": a.BoostDays}
	if otpID != "" {
		body["otp_id"], body["otp_code"] = otpID, otpCode
	}
	var res boostResult
	err := h.studio.internal(ctx, "/api/v1/internal/ads/os-turn/boost", body, &res)
	var se *studioError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.Code == "otp_required":
		h.askBoostOTP(ctx, tenantID, req, a, "/api/v1/internal/ads/os-turn/boost-otp", body, out)
		return
	case errors.As(err, &se) && se.Code == "otp_invalid":
		out.Reply = "That confirmation code does not match, so nothing was spent. Check the code I sent and reply with it."
		return
	case errors.As(err, &se) && se.Code == "no_credits":
		h.codes.DropBoostOTP(ctx, tenantID.String(), a.UserID)
		out.Reply = trimRunes(se.Message, 300) + ". Nothing was spent. Top up here: " + link(req.PortalURL, req.TenantSlug, "/settings/billing")
		return
	default:
		h.codes.DropBoostOTP(ctx, tenantID.String(), a.UserID)
		if msg, ok := boostRefusal(err, studio); ok {
			out.Reply = msg
			return
		}
		h.logger.Warn("whatsapp boost launch failed", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not launch the boost just now. Nothing was spent; try again from the Studio: " + studio
		return
	}
	h.codes.DropBoostOTP(ctx, tenantID.String(), a.UserID)
	boosts := link(req.PortalURL, req.TenantSlug, "/campaigns")
	if res.ID != "" {
		boosts = link(req.PortalURL, req.TenantSlug, "/campaigns/"+res.ID)
	}
	reply := "Boost created. It is paused until Meta approves the ad, usually within a day, then it goes live by itself. Follow it here: " + boosts
	if res.Quote.Shortfall > 0 && res.Quote.AccountID != "" {
		reply += fmt.Sprintf("\nAdd %s to the ad account on Meta so it can run (M-Pesa or card): %s",
			money(res.Quote.Shortfall, res.Quote.Currency), metaBilling(res.Quote.AccountID))
	}
	out.Reply = reply
}

// whatsAppRunAds runs the plan the message showed (its fingerprint). marketflow-api refuses a plan
// that changed since, and returns the first result when the plan already ran.
func (h *OSHandler) whatsAppRunAds(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, otpID, otpCode string, out *WhatsAppAskResponse) {
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	if h.studio == nil || a.VersionID == "" {
		out.Reply = "I cannot run that plan from here. Open Run ads in the Studio: " + studio
		return
	}
	body := map[string]any{"tenant_id": tenantID.String(), "user_id": a.UserID, "version_id": a.VersionID,
		"fingerprint": a.PlanFingerprint}
	if len(a.AdPlatforms) > 0 {
		body["platforms"] = a.AdPlatforms
	}
	if otpID != "" {
		body["otp_id"], body["otp_code"] = otpID, otpCode
	}
	var res struct {
		Text string `json:"text"`
	}
	err := h.studio.internalFor(ctx, runTimeout, runAdsPath, body, &res)
	var se *studioError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.Code == "otp_required":
		h.askBoostOTP(ctx, tenantID, req, a, runAdsOTPPath, body, out)
		return
	case errors.As(err, &se) && se.Code == "otp_invalid":
		out.Reply = "That confirmation code does not match, so nothing was spent. Check the code I sent and reply with it."
		return
	case errors.As(err, &se) && se.Code == "plan_changed":
		h.codes.DropBoostOTP(ctx, tenantID.String(), a.UserID)
		out.Reply = "The ad plan changed since I sent it (a budget, an account or the funding), so nothing was spent. Tap Run ads again for the new plan, or open it in the Studio: " + studio
		return
	default:
		h.codes.DropBoostOTP(ctx, tenantID.String(), a.UserID)
		if msg, ok := boostRefusal(err, studio); ok {
			out.Reply = msg
			return
		}
		h.logger.Warn("whatsapp run ads failed", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not run the ad plan just now. Nothing was spent; try again from the Studio: " + studio
		return
	}
	h.codes.DropBoostOTP(ctx, tenantID.String(), a.UserID)
	out.Reply = trimRunes(res.Text, 3000) + "\n\nFollow your campaigns here: " + link(req.PortalURL, req.TenantSlug, "/campaigns")
}

// askBoostOTP asks marketflow-api (path) to send the code a large plan needs and keeps it waiting.
func (h *OSHandler) askBoostOTP(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, path string, body map[string]any, out *WhatsAppAskResponse) {
	var sent struct {
		OTPID    string `json:"otp_id"`
		ToMasked string `json:"to_masked"`
	}
	delete(body, "otp_id")
	delete(body, "otp_code")
	err := h.studio.internalFor(ctx, planTimeout, path, body, &sent)
	var se *studioError
	switch {
	case err == nil && sent.OTPID != "":
	case errors.As(err, &se) && se.Code == "otp_rate_limited":
		out.Reply = "A confirmation code was just sent. Reply with it, or wait a moment and tap " + btnConfirmBoost + " again. Nothing was spent."
		return
	case errors.As(err, &se) && (se.Code == "no_verified_phone" || se.Status == http.StatusConflict):
		out.Reply = "This needs a confirmation code sent to your verified WhatsApp number, and none is on your account. Verify it in MarketFlow, then try again. Nothing was spent."
		return
	default:
		h.logger.Warn("boost confirmation code not sent", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not send the confirmation code just now, so nothing was spent. Please try again in a minute."
		return
	}
	if err := h.codes.HoldBoostOTP(ctx, *a, sent.OTPID, boostOTPWait); err != nil {
		h.logger.Warn("boost waiting for its code not kept", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not keep the ads waiting for their code, so nothing was spent. Please tap " + btnConfirmBoost + " again."
		return
	}
	out.Route = "otp"
	out.Reply = fmt.Sprintf("This is above your confirmation limit, so I sent a 6-digit code to %s. Reply with it within %s to go ahead. Nothing is spent until then.",
		sent.ToMasked, minutes(boostOTPWait))
}

// boostOTPReplyMsg handles a 6-digit reply while a boost waits for its code.
func (h *OSHandler) boostOTPReplyMsg(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest) (WhatsAppAskResponse, bool) {
	if h.codes == nil || req.UserID == "" {
		return WhatsAppAskResponse{}, false
	}
	code, ok := osrun.ParseOTP(req.Text)
	if !ok {
		return WhatsAppAskResponse{}, false
	}
	p, ok := h.codes.BoostOTP(ctx, tenantID.String(), req.UserID, req.Sender)
	if !ok {
		return WhatsAppAskResponse{}, false
	}
	out := WhatsAppAskResponse{Pass: true, Verdict: approvalVerdict, Route: "approval"}
	h.whatsAppBoostLaunch(ctx, tenantID, req, &p.Approval, p.OTPID, code, &out)
	h.recordWhatsApp(ctx, tenantID, req.UserID, "(boost confirmation code)", out.Reply, out.Route)
	return out, true
}
