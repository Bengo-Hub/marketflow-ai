package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/registry"
)

// Sprint 17 leftovers (2026-11-15): approve and post the finished ad, approve a cheaper option, a
// one-time code above a spend, and the Buy credits button. Every one of them goes through the
// same osrun approval code as the plan's Approve (single use, bound to the run, the user and the
// number); none of them can be triggered by what a message says, only by a tap or a typed code.

// Button titles (Meta allows 20 characters).
const (
	btnApprovePost = "Approve and post"
	btnBuyPrefix   = "Buy "
)

// waOTP asks marketflow-api to send Code to To by the auth_otp template (platform sender).
type waOTP struct {
	To   string `json:"to"`
	Code string `json:"code"`
}

// waTemplate names the utility template marketflow-api sends instead of the text when the
// requester's 24-hour window is closed, if Meta approved it. marketflow-api fills the parameters
// from its own records; Button is the suffix of the template's URL button.
type waTemplate struct {
	Kind   string `json:"kind"`
	Button string `json:"button,omitempty"`
}

// Template kinds marketflow-api maps to template names (MF_WA_TPL_*).
const tplVideoReady = "video_ready"

// afterApproval runs a redeemed approval: first a one-time code when the spend reaches the
// threshold, then the action.
func (h *OSHandler) afterApproval(ctx context.Context, r *http.Request, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	if h.codes.NeedsOTP(*a) {
		code, err := h.codes.IssueOTP(ctx, *a)
		if err != nil {
			h.logger.Warn("one-time code not issued", zap.String("run_id", a.RunID), zap.Error(err))
			out.Reply = "I could not send the confirmation code just now, so nothing was spent. Please try again in a minute."
			return
		}
		out.Route = "otp"
		out.Reply = fmt.Sprintf("This spends about %d credits, so I sent you a 6-digit confirmation code in a separate message. Reply with that code within %s to go ahead. Nothing is spent until then.",
			a.Credits, minutes(h.codes.OTPTTL()))
		out.OTP = &waOTP{To: req.Sender, Code: code}
		return
	}
	h.runApproval(ctx, r, tenantID, req, a, out)
}

// runApproval does what an approval (with its one-time code, when one was needed) allows.
func (h *OSHandler) runApproval(ctx context.Context, r *http.Request, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	switch a.Action {
	case osrun.ActionApprove:
		h.whatsAppApprove(ctx, r, tenantID, req, a, out)
	case osrun.ActionRender:
		h.whatsAppRender(ctx, tenantID, req, a, out)
	case osrun.ActionPublish:
		h.whatsAppPublish(ctx, tenantID, req, a, out)
	case osrun.ActionBoost:
		h.whatsAppBoostQuote(ctx, tenantID, req, a, out)
	case osrun.ActionBoostLaunch:
		h.whatsAppBoostLaunch(ctx, tenantID, req, a, "", "", out)
	default:
		out.Reply = "That code is not for anything I can do. Send your request again."
	}
}

// otpReplyMsg handles a reply that is a one-time code while an approval waits for one.
func (h *OSHandler) otpReplyMsg(ctx context.Context, r *http.Request, tenantID uuid.UUID, req whatsAppAskRequest) (WhatsAppAskResponse, bool) {
	if h.codes == nil || req.UserID == "" {
		return WhatsAppAskResponse{}, false
	}
	code, ok := osrun.ParseOTP(req.Text)
	if !ok || !h.codes.OTPPending(ctx, tenantID.String(), req.UserID) {
		return WhatsAppAskResponse{}, false
	}
	out := WhatsAppAskResponse{Pass: true, Verdict: approvalVerdict, Route: "otp"}
	a, err := h.codes.RedeemOTP(ctx, tenantID.String(), req.UserID, req.Sender, code)
	switch {
	case errors.Is(err, osrun.ErrWrongCode):
		out.Reply = "That confirmation code does not match, so nothing was spent. Check the code I sent and reply with it."
	case errors.Is(err, osrun.ErrNoCode):
		out.Reply = "That confirmation code has expired or was already used, so nothing was spent. Send your request again."
	case err != nil:
		h.logger.Warn("one-time code check failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		out.Reply = "I could not check that code just now, so nothing was spent. Please try again in a minute."
	default:
		out.Route = "approval"
		h.runApproval(ctx, r, tenantID, req, a, &out)
	}
	h.recordWhatsApp(ctx, tenantID, req.UserID, "(confirmation code)", out.Reply, out.Route)
	return out, true
}

// ── Approve and post the finished ad ────────────────────────────────────────

// publishOffer is the message about a finished ad: the video, then Approve and post and Change.
// It issues the publish code; ok is false when no code could be issued.
func (h *OSHandler) publishOffer(ctx context.Context, run *osrun.Run, versionID, videoLink, studio string) (text string, ia *waInteractive, ok bool) {
	code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionPublish, RunID: run.RunID, TenantID: run.TenantID,
		UserID: run.UserID, Sender: run.Sender, VersionID: versionID})
	if err != nil {
		h.logger.Warn("publish code not issued", zap.String("run_id", run.RunID), zap.Error(err))
		return "", nil, false
	}
	text = fmt.Sprintf("Your video ad is ready: %s\n\nReply POST %s to approve it and post it to your connected accounts (no paid ads). The code works for %s. To change a scene, open the Studio: %s",
		videoLink, code, minutes(h.codes.TTL()), studio)
	ia = &waInteractive{
		Media: []waMedia{{Type: "video", Link: videoLink, Caption: "Your video ad is ready."}},
		Body: fmt.Sprintf("Tap %s, or reply POST %s, to approve it and post it to your connected accounts (no paid ads). To change a scene, tap %s.",
			btnApprovePost, code, btnChange),
		Buttons: publishButtons(run.RunID, code),
	}
	return text, ia, true
}

// publishButtons are the finished ad's Approve and post and Change.
func publishButtons(runID, code string) []waBtn {
	return []waBtn{
		{ID: osrun.ReplyID(osrun.ReplyYes, runID, code), Title: btnApprovePost},
		{ID: osrun.ReplyID(osrun.ReplyChange, runID, ""), Title: btnChange},
	}
}

// publishResult is marketflow-api's answer to a WhatsApp approve and post.
type publishResult struct {
	Posted []struct {
		Platform    string `json:"platform"`
		DisplayName string `json:"display_name"`
	} `json:"posted"`
	Skipped []struct {
		DisplayName string `json:"display_name"`
		Reason      string `json:"reason"`
	} `json:"skipped"`
}

// whatsAppPublish approves the finished ad and posts it to the business's connected accounts
// through marketflow-api, which applies the Studio's own checks: the approving roles, active
// accounts, each platform's switch, the preflight, and the compliance and restricted-category
// holds. A paid boost is never started here; the reply only offers the Boost it step.
func (h *OSHandler) whatsAppPublish(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	calendar := link(req.PortalURL, req.TenantSlug, "/publishing")
	if h.studio == nil || a.VersionID == "" {
		out.Reply = "I cannot post that video from here. Open the Studio to approve and post it: " + studio
		return
	}
	res, err := h.studio.publishFromWhatsApp(ctx, tenantID, a.VersionID, a.UserID, a.RunID, a.Sender)
	var se *studioError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.Code == "no_accounts":
		out.Reply = "Approved. You have no connected account that can take the post yet, so nothing was posted. Connect Facebook, Instagram, TikTok or YouTube in MarketFlow, then post it from the Studio: " + studio
		return
	case errors.As(err, &se) && se.Code == "needs_review":
		out.Reply = "This ad needs a look before it is posted (" + trimRunes(se.Message, 300) + "), so nothing was posted. Open it in the Studio to check and post it: " + studio
		return
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		out.Reply = "Only the business owner or a MarketFlow admin can post ads, so nothing was posted."
		return
	case errors.As(err, &se) && se.Status < 500 && se.Message != "":
		out.Reply = "The Studio could not post it: " + trimRunes(se.Message, 300) + ". Nothing was posted. Open it to post it: " + studio
		return
	default:
		h.logger.Warn("whatsapp publish failed", zap.String("run_id", a.RunID), zap.Error(err))
		out.Reply = "I could not post it just now. Nothing was posted; try again from the Studio: " + studio
		return
	}
	var b strings.Builder
	names := make([]string, 0, len(res.Posted))
	for _, p := range res.Posted {
		names = append(names, p.DisplayName+" ("+p.Platform+")")
	}
	fmt.Fprintf(&b, "Approved and sent for posting to %s. I'll tell you here when each post is out.", strings.Join(names, ", "))
	for _, s := range res.Skipped {
		fmt.Fprintf(&b, "\nNot posted to %s: %s.", s.DisplayName, trimRunes(s.Reason, 160))
	}
	posted := b.String()
	out.Reply = posted + " Your posting calendar: " + calendar
	// The paid boost is offered as its own step: the tap only fetches a quote (whatsapp_boost.go).
	if code, ok := h.boostOffer(ctx, a); ok {
		out.Reply += fmt.Sprintf("\n\nTo see what a paid boost of this ad costs, reply YES %s. Nothing is spent until you confirm the quote.", code)
		out.Interactive = &waInteractive{
			Body:    posted + "\n\n" + approveLine(btnBoost, code, "see what a paid boost costs (nothing is spent until you confirm)", 0),
			Buttons: []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, a.RunID, code), Title: btnBoost}},
		}
	}
}

// publishFromWhatsApp asks marketflow-api to approve the version and post it organically to every
// connected account that can take it, as the user who approved it on WhatsApp.
func (c *studioClient) publishFromWhatsApp(ctx context.Context, tenantID uuid.UUID, versionID, userID, runID, sender string) (*publishResult, error) {
	var out publishResult
	err := c.internal(ctx, "/api/v1/internal/creative/os-turn/publish", map[string]string{"tenant_id": tenantID.String(),
		"version_id": versionID, "user_id": userID, "run_id": runID, "sender": sender}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// internal posts to a marketflow-api /internal route with the internal service key and reads a
// refusal like a Studio one.
func (c *studioClient) internal(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.eventsKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("studio %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error   string         `json:"error"`
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(data, &e)
		return &studioError{Status: resp.StatusCode, Code: e.Code, Message: e.Error, Details: e.Details}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("studio %s: unreadable answer: %w", path, err)
		}
	}
	return nil
}

// ── Cheaper options ─────────────────────────────────────────────────────────

// isVideoTier reports whether a media tier makes a video (it needs a storyboard): only those can
// be made from WhatsApp today.
func (h *OSHandler) isVideoTier(ctx context.Context, tier string) bool {
	if h.registry == nil || tier == "" {
		return false
	}
	out, ok := h.registry(ctx).Outputs[tier]
	return ok && out.Kind == "media" && out.NeedsStoryboard
}

// cheaperChoices are the cheaper options as buttons (when every label fits a button and there are
// at most 3) or as a list.
func cheaperChoices(runID string, opts []registry.Option) (btns []waBtn, list *waList) {
	fits := len(opts) <= 3
	for _, o := range opts {
		if len([]rune(o.Label)) > 20 {
			fits = false
		}
	}
	if fits {
		for i, o := range opts {
			btns = append(btns, waBtn{ID: osrun.AltID(runID, i), Title: o.Label})
		}
		return btns, nil
	}
	list = &waList{Button: "See options", Rows: make([]waRow, 0, len(opts))}
	for i, o := range opts {
		if i == 10 {
			break
		}
		list.Rows = append(list.Rows, waRow{ID: osrun.AltID(runID, i), Title: trimRunes(o.Label, 24),
			Description: trimRunes(fmt.Sprintf("%d credits. %s", o.Credits, o.Description), 72)})
	}
	return nil, list
}

// chooseCheaper answers a tapped cheaper option: a video option gets its own price and approval
// code (Approve and Cancel); posters and copy are made in the Studio, so they end with its link.
func (h *OSHandler) chooseCheaper(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, rep osrun.Reply, out *WhatsAppAskResponse) {
	run, err := h.runs.Get(ctx, tenantID.String(), rep.RunID)
	if err != nil || run.Plan == nil || run.UserID != req.UserID {
		out.Reply = "That plan has expired, so nothing was spent. Send your request again for a new quote."
		return
	}
	opts := cheaperOptions(run.Plan)
	if rep.Row >= len(opts) {
		out.Reply = "That option is no longer on offer. Send your request again for a new quote."
		return
	}
	o := opts[rep.Row]
	marketing := link(req.PortalURL, req.TenantSlug, "/marketing")
	if !h.isVideoTier(ctx, o.Tier) {
		out.Reply = fmt.Sprintf("%s (%d credits) is made in MarketFlow, not on WhatsApp yet. Nothing was spent. Choose it there: %s", o.Label, o.Credits, marketing)
		return
	}
	if bal := h.balance(ctx, tenantID); bal >= 0 && bal < o.Credits {
		waiting := &osrun.Approval{Action: osrun.ActionApprove, RunID: run.RunID, TenantID: run.TenantID, UserID: req.UserID,
			Sender: req.Sender, Credits: o.Credits, Tier: o.Tier, Option: o.Label}
		out.Reply, out.Interactive = h.offerBuy(ctx, tenantID, req, run.RunID, o.Credits, o.Credits-bal, waiting,
			fmt.Sprintf("%s needs about %d credits and you have %d, so nothing was spent.", o.Label, o.Credits, bal))
		return
	}
	code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionApprove, RunID: run.RunID, TenantID: run.TenantID,
		UserID: req.UserID, Sender: req.Sender, Credits: o.Credits, Tier: o.Tier, Option: o.Label})
	if err != nil {
		h.logger.Warn("option code not issued", zap.String("run_id", run.RunID), zap.Error(err))
		out.Reply = "I could not prepare that option just now, so nothing was spent. Please try again in a minute."
		return
	}
	text := fmt.Sprintf("%s: about %d credits.", o.Label, o.Credits)
	if o.Description != "" {
		text += "\n" + o.Description
	}
	out.Reply = text + fmt.Sprintf("\n\nThe code below works for %s.\n\nReply YES %s to approve it (about %d credits)", minutes(h.codes.TTL()), code, o.Credits)
	out.Interactive = &waInteractive{
		Body: text + "\n\n" + approveLine(btnApprove, code, "approve it", o.Credits),
		Buttons: []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, run.RunID, code), Title: btnApprove},
			{ID: osrun.ReplyID(osrun.ReplyCancel, run.RunID, ""), Title: btnCancel}},
	}
}

// ── Buy credits ──────────────────────────────────────────────────────────────

// buyTitle is the button: "Buy 60 credits" (Meta allows 20 characters, so the price is in the text).
func buyTitle(units int) string {
	return trimRunes(fmt.Sprintf("%s%d credits", btnBuyPrefix, units), 20)
}

// kes writes a price for a message: KES 1,750.
func kes(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "KES " + s
}

// offerBuy turns a short balance into a Buy credits offer: the smallest bundle that covers the
// shortfall, one button, and the plain text with the billing link. The offer is kept for the
// person to tap; nothing is bought without the tap. lead opens the message.
func (h *OSHandler) offerBuy(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, runID string, need, shortfall int, waiting *osrun.Approval, lead string) (string, *waInteractive) {
	billing := link(req.PortalURL, req.TenantSlug, "/settings/billing")
	text := lead + " Top up here: " + billing
	if h.codes == nil || h.registry == nil || req.UserID == "" || req.Sender == "" {
		return text, nil
	}
	if _, ok := h.credits.(CreditPurchaser); !ok {
		return text, nil
	}
	if shortfall < 1 {
		shortfall = 1
	}
	b, ok := h.registry(ctx).SmallestBundle(shortfall)
	if !ok || b.Code == "" || b.PriceKES <= 0 {
		return text, nil
	}
	ref, err := h.codes.OfferBuy(ctx, osrun.BuyOffer{TenantID: tenantID.String(), UserID: req.UserID, Sender: req.Sender,
		TenantSlug: req.TenantSlug, PortalURL: req.PortalURL, RunID: runID, BundleCode: b.Code, Units: b.Units,
		PriceKES: b.PriceKES, Need: need, Waiting: waiting})
	if err != nil {
		h.logger.Warn("buy offer not kept", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return text, nil
	}
	price := kes(b.PriceKES) + " (plus VAT where it applies)"
	text = fmt.Sprintf("%s\n\nBuy %d credits for %s by M-Pesa on this number: reply BUY, or top up here: %s", lead, b.Units, price, billing)
	ia := &waInteractive{
		Body: fmt.Sprintf("%s\n\nTap %s to get an M-Pesa prompt for %s on this number. Nothing is charged until you enter your PIN.%s",
			lead, buyTitle(b.Units), price, resumeLine(waiting)),
		Buttons: []waBtn{{ID: osrun.BuyID(ref), Title: buyTitle(b.Units)}},
	}
	return text, ia
}

// resumeLine says what happens when the credits land.
func resumeLine(waiting *osrun.Approval) string {
	switch {
	case waiting == nil:
		return " When the credits arrive, send your request again."
	case waiting.Action == osrun.ActionRender:
		return " When the credits arrive I start the video you approved."
	default:
		return " When the credits arrive I send the plan back for you to approve."
	}
}

// buyTap starts the purchase a Buy credits tap (or a typed BUY) confirms: subscriptions-api makes
// the platform-billed credit purchase and Treasury sends the M-Pesa prompt to this number.
func (h *OSHandler) buyTap(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, ref string, out *WhatsAppAskResponse) {
	billing := link(req.PortalURL, req.TenantSlug, "/settings/billing")
	p, ok := h.credits.(CreditPurchaser)
	if !ok {
		out.Reply = "Buying credits from WhatsApp is not available right now. Top up here: " + billing
		return
	}
	if ref == "" {
		pending, ok := h.codes.PendingBuy(ctx, tenantID.String(), req.UserID)
		if !ok {
			out.Reply = "There is no credit purchase waiting. Top up here: " + billing
			return
		}
		ref = pending.Ref
	}
	o, err := h.codes.TakeBuy(ctx, tenantID.String(), req.UserID, req.Sender, ref)
	switch {
	case errors.Is(err, osrun.ErrStaleTap):
		out.Reply = "That button is from an older message and has expired, so nothing was charged."
		if cur, ok := h.codes.PendingBuy(ctx, tenantID.String(), req.UserID); ok {
			out.Interactive = &waInteractive{Body: fmt.Sprintf("That button has expired. Tap %s to get an M-Pesa prompt for %s on this number.",
				buyTitle(cur.Units), kes(cur.PriceKES)), Buttons: []waBtn{{ID: osrun.BuyID(cur.Ref), Title: buyTitle(cur.Units)}}}
		}
		return
	case err != nil:
		out.Reply = "That offer has expired, so nothing was charged. Top up here: " + billing
		return
	}
	phone := strings.TrimPrefix(req.Sender, "+")
	intentID, err := p.PurchaseBundle(ctx, tenantID, o.BundleCode, phone, o.Ref)
	if err != nil {
		h.logger.Warn("whatsapp credit purchase failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		out.Reply = "I could not start the M-Pesa payment just now, so nothing was charged. Top up here: " + billing
		return
	}
	o.IntentID = intentID
	if err := h.codes.SaveTopup(ctx, *o); err != nil {
		h.logger.Warn("credit purchase not kept for resuming", zap.String("intent_id", intentID), zap.Error(err))
	}
	out.Reply = fmt.Sprintf("I sent an M-Pesa prompt for %s to this number. Enter your PIN to pay for %d credits.%s",
		kes(o.PriceKES)+" (plus VAT where it applies)", o.Units, resumeLine(o.Waiting))
}

// buyReply is a typed BUY (or "nunua") answering a pending Buy credits offer.
func isBuyReply(text string) bool {
	t := strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!"))
	return t == "buy" || t == "nunua"
}

type topupPaidRequest struct {
	TenantID string `json:"tenant_id"`
	IntentID string `json:"intent_id"`
}

// WhatsAppTopupPaid handles POST /api/v1/internal/os/whatsapp-topup-paid (service key):
// marketflow-api saw Treasury report a credit purchase paid. When the purchase was started from a
// WhatsApp Buy credits tap, the action waiting for the credits resumes and the answer is the
// message for the buyer's number. 409 means the credits have not landed yet (the caller retries);
// an empty answer means the payment is not ours or was already handled.
func (h *OSHandler) WhatsAppTopupPaid(w http.ResponseWriter, r *http.Request) {
	var req topupPaidRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil || req.IntentID == "" {
		writeError(w, http.StatusBadRequest, "tenant_id and intent_id are required")
		return
	}
	ctx := r.Context()
	o, ok := h.codes.Topup(ctx, req.IntentID)
	if !ok || o.TenantID != tenantID.String() {
		writeJSON(w, http.StatusOK, whatsAppNotifyResponse{})
		return
	}
	if o.Need > 0 {
		if bal := h.balance(ctx, tenantID); bal >= 0 && bal < o.Need {
			writeError(w, http.StatusConflict, "the credits have not landed yet")
			return
		}
	}
	if !h.codes.FinishTopup(ctx, req.IntentID) {
		writeJSON(w, http.StatusOK, whatsAppNotifyResponse{})
		return
	}
	writeJSON(w, http.StatusOK, h.resumeTopup(ctx, tenantID, o))
}

// resumeTopup is what happens once the credits landed: a video the person already approved starts
// at once; a plan comes back with a fresh code to approve; anything else asks for the request again.
func (h *OSHandler) resumeTopup(ctx context.Context, tenantID uuid.UUID, o *osrun.BuyOffer) whatsAppNotifyResponse {
	paid := fmt.Sprintf("Payment received: %d credits added.", o.Units)
	req := whatsAppAskRequest{TenantID: o.TenantID, TenantSlug: o.TenantSlug, UserID: o.UserID, Sender: o.Sender, PortalURL: o.PortalURL}
	resp := whatsAppNotifyResponse{To: o.Sender, Text: paid + " Send your request again whenever you are ready."}
	a := o.Waiting
	if a == nil {
		return resp
	}
	switch a.Action {
	case osrun.ActionRender:
		approval := *a
		approval.OTPDone = true // the code (when one was needed) was confirmed before the shortfall
		h.inBackground(func(bg context.Context) { h.renderInBackground(bg, tenantID, req, approval) })
		resp.Text = paid + " Making your video now. It takes a few minutes; I'll send the link here when it is ready."
	case osrun.ActionApprove:
		fresh := *a
		code, err := h.codes.Issue(ctx, fresh)
		if err != nil {
			resp.Text = paid + " Send your request again for a new quote."
			return resp
		}
		what := "the plan"
		if a.Option != "" {
			what = a.Option
		}
		resp.Text = fmt.Sprintf("%s Reply YES %s to approve %s (about %d credits). The code works for %s.", paid, code, what, a.Credits, minutes(h.codes.TTL()))
		resp.Interactive = &waInteractive{
			Body: paid + " " + approveLine(btnApprove, code, "approve "+what, a.Credits),
			Buttons: []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, a.RunID, code), Title: btnApprove},
				{ID: osrun.ReplyID(osrun.ReplyCancel, a.RunID, ""), Title: btnCancel}},
		}
	}
	return resp
}
