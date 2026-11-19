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

const (
	btnBoost        = "Boost it"
	btnConfirmBoost = "Confirm boost"
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

// boostQuote is marketflow-api's quote for the default WhatsApp boost.
type boostQuote struct {
	Quote struct {
		Currency      string   `json:"currency"`
		Budget        int64    `json:"budget"`
		Days          int      `json:"days"`
		Shortfall     int64    `json:"shortfall"`
		Funding       string   `json:"funding"`
		LaunchCredits int      `json:"launch_credits"`
		Blockers      []string `json:"blockers"`
		AccountName   string   `json:"account_name"`
		AccountID     string   `json:"ad_account_id"`
	} `json:"quote"`
	Objective string `json:"objective"`
	AdAccount string `json:"ad_account"`
	Page      string `json:"page"`
	Ready     bool   `json:"ready"`
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
		return "Connect your Meta ad account in MarketFlow first (Settings, Integrations), then tap Boost it again. Nothing was spent.", true
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

func boostGoal(objective string) string {
	if objective == "traffic" {
		return "visits to your link"
	}
	return "WhatsApp messages to your business"
}

// whatsAppBoostQuote answers Boost it: the quote, and a Confirm boost code for exactly that boost.
func (h *OSHandler) whatsAppBoostQuote(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	if h.studio == nil || a.VersionID == "" {
		out.Reply = "I cannot boost that video from here. Boost it from the Studio: " + studio
		return
	}
	var q boostQuote
	err := h.studio.internal(ctx, "/api/v1/internal/ads/os-turn/boost-quote", map[string]any{"tenant_id": tenantID.String(),
		"user_id": a.UserID, "version_id": a.VersionID}, &q)
	if err != nil {
		if msg, ok := boostRefusal(err, studio); ok {
			out.Reply = msg
			return
		}
		h.logger.Warn("whatsapp boost quote failed", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not get a boost quote just now. Nothing was spent; try again in a minute or boost it from the Studio: " + studio
		return
	}
	if !q.Ready {
		out.Reply = "This boost cannot start yet: " + trimRunes(strings.Join(q.Quote.Blockers, "; "), 400) + ". Nothing was spent. Fix it in the Studio: " + studio
		return
	}
	code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionBoostLaunch, RunID: a.RunID, TenantID: a.TenantID,
		UserID: a.UserID, Sender: a.Sender, VersionID: a.VersionID, BoostBudget: q.Quote.Budget, BoostDays: q.Quote.Days})
	if err != nil {
		h.logger.Warn("boost launch code not issued", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not prepare the boost just now, so nothing was spent. Please try again in a minute."
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Boost for %d days with a budget of %s, aimed at %s.\nAd account: %s. Runs as: %s.",
		q.Quote.Days, money(q.Quote.Budget, q.Quote.Currency), boostGoal(q.Objective), q.AdAccount, q.Page)
	if q.Quote.Shortfall > 0 {
		fmt.Fprintf(&b, "\nYour ad account needs %s more on Meta before the ad can run. You pay Meta directly (M-Pesa or card).", money(q.Quote.Shortfall, q.Quote.Currency))
	}
	if q.Quote.LaunchCredits > 0 {
		fmt.Fprintf(&b, "\nMarketFlow's launch charge: %d credits.", q.Quote.LaunchCredits)
	}
	b.WriteString("\nThe ad starts paused and goes live once Meta approves it. To change the budget, days or audience, boost it from the Studio instead.")
	text := b.String()
	out.Reply = text + fmt.Sprintf("\n\nReply YES %s within %s to launch it. Studio: %s", code, minutes(h.codes.TTL()), studio)
	out.Interactive = &waInteractive{
		Body: text + "\n\n" + approveLine(btnConfirmBoost, code, "launch it", q.Quote.LaunchCredits),
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

// whatsAppBoostLaunch answers Confirm boost (and the code after it, when one was needed).
func (h *OSHandler) whatsAppBoostLaunch(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, otpID, otpCode string, out *WhatsAppAskResponse) {
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
		h.askBoostOTP(ctx, tenantID, req, a, body, out)
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
	reply := "Boost created. It is paused until Meta approves the ad, usually within a day; I'll tell you here when it goes live. Follow it here: " + boosts
	if res.Quote.Shortfall > 0 && res.Quote.AccountID != "" {
		reply += fmt.Sprintf("\nAdd %s to the ad account on Meta so it can run (M-Pesa or card): %s",
			money(res.Quote.Shortfall, res.Quote.Currency), metaBilling(res.Quote.AccountID))
	}
	out.Reply = reply
}

// askBoostOTP asks marketflow-api to send the code a large boost needs and keeps the boost waiting.
func (h *OSHandler) askBoostOTP(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, body map[string]any, out *WhatsAppAskResponse) {
	var sent struct {
		OTPID    string `json:"otp_id"`
		ToMasked string `json:"to_masked"`
	}
	delete(body, "otp_id")
	delete(body, "otp_code")
	err := h.studio.internal(ctx, "/api/v1/internal/ads/os-turn/boost-otp", body, &sent)
	var se *studioError
	switch {
	case err == nil && sent.OTPID != "":
	case errors.As(err, &se) && se.Code == "otp_rate_limited":
		out.Reply = "A confirmation code was just sent. Reply with it, or wait a moment and tap Confirm boost again. Nothing was spent."
		return
	case errors.As(err, &se) && (se.Code == "no_verified_phone" || se.Status == http.StatusConflict):
		out.Reply = "This boost needs a confirmation code sent to your verified WhatsApp number, and none is on your account. Verify it in MarketFlow, then try again. Nothing was spent."
		return
	default:
		h.logger.Warn("boost confirmation code not sent", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not send the confirmation code just now, so nothing was spent. Please try again in a minute."
		return
	}
	if err := h.codes.HoldBoostOTP(ctx, *a, sent.OTPID, boostOTPWait); err != nil {
		h.logger.Warn("boost waiting for its code not kept", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not keep the boost waiting for its code, so nothing was spent. Please tap Confirm boost again."
		return
	}
	out.Route = "otp"
	out.Reply = fmt.Sprintf("This boost is above your confirmation limit, so I sent a 6-digit code to %s. Reply with it within %s to launch it. Nothing is spent until then.",
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
