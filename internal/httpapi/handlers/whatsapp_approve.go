package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/assistgate"
	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// WhatsApp approvals (owner, 2026-11-12): a quote ends "Reply YES <code> to approve"; the code is
// bound to that run, the sender's user and verified number, and works for the configured code
// TTL. marketflow-api's worker forwards only senders with an approving role (marketflow_admin or
// fbo_owner), so the role is checked there on every message. A right code approves the run and
// makes the script, the brief and the storyboard; the stills come back with a second code that
// starts the paid render. A wrong or expired code spends nothing; any other message cancels the
// pending code. All of the code logic is here, in one place.

// brandImagesDefault is how many brand images Studio asks for when its refusal does not say.
const brandImagesDefault = 3

// approvalVerdict marks a YES reply as handled without the gate (it has no marketing words).
var approvalVerdict = assistgate.Verdict{Pass: true, Reason: "approval reply", Source: "rules"}

// approvalReply handles a message that is an approval reply, before the gate. It reports false
// when the message is not one (and then cancels the sender's pending code).
func (h *OSHandler) approvalReply(ctx context.Context, r *http.Request, tenantID uuid.UUID, req whatsAppAskRequest) (WhatsAppAskResponse, bool) {
	if h.codes == nil || req.UserID == "" {
		return WhatsAppAskResponse{}, false
	}
	code, yes := osrun.ParseYes(req.Text)
	if !yes || (code == "" && !h.codes.Pending(ctx, tenantID.String(), req.UserID)) {
		h.codes.Cancel(ctx, tenantID.String(), req.UserID)
		return WhatsAppAskResponse{}, false
	}
	out := WhatsAppAskResponse{Pass: true, Verdict: approvalVerdict, Route: "approval"}
	if code == "" {
		out.Reply = "Reply YES and the 4-digit code from my last message, for example YES 1234."
		return out, true
	}
	a, err := h.codes.Redeem(ctx, tenantID.String(), req.UserID, req.Sender, code)
	switch {
	case errors.Is(err, osrun.ErrWrongCode):
		out.Reply = "That code does not match, so nothing was spent. Check the code in my last message and reply YES with it."
		return out, true
	case errors.Is(err, osrun.ErrNoCode):
		out.Reply = "That code has expired or was already used, so nothing was spent. Send your request again for a new quote."
		return out, true
	case err != nil:
		h.logger.Warn("approval code check failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		out.Reply = "I could not check that code just now, so nothing was spent. Please try again in a minute."
		return out, true
	}
	switch a.Action {
	case osrun.ActionApprove:
		h.whatsAppApprove(ctx, r, tenantID, req, a, &out)
	case osrun.ActionRender:
		h.whatsAppRender(ctx, tenantID, req, a, &out)
	default:
		out.Reply = "That code is not for anything I can do. Send your request again."
	}
	h.recordWhatsApp(ctx, tenantID, req.UserID, req.Text, out.Reply, out.Route)
	return out, true
}

// approvedReply is the immediate answer to a right approval code; the work goes on in the background.
const approvedReply = "Approved. I'm writing the script and storyboard now; I'll send the stills here."

// whatsAppApprove answers at once and, in the background, runs the approved turn (charged like
// the portal's approval), then makes the script into a Studio video ad and starts its storyboard.
// The WhatsApp request never waits on the turn or the Studio calls.
func (h *OSHandler) whatsAppApprove(ctx context.Context, r *http.Request, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	run, err := h.runs.Get(ctx, tenantID.String(), a.RunID)
	if err != nil {
		out.Reply = "That plan has expired, so nothing was spent. Send your request again for a new quote."
		return
	}
	out.Reply = approvedReply
	// A copy of the request made now: the original ends with the reply.
	rc := r.Clone(context.WithoutCancel(ctx))
	h.inBackground(func(bg context.Context) {
		h.approveInBackground(bg, rc.WithContext(bg), tenantID, req, run)
	})
}

// approveInBackground is the approval's work after the reply. Only one runs per quote (a Redis
// lock), and a quote already approved reuses its approved turn, so a second YES or a retry
// neither charges the turn again nor makes a second brief. Every failure reaches the requester
// through marketflow-api (event media_failed).
func (h *OSHandler) approveInBackground(ctx context.Context, r *http.Request, tenantID uuid.UUID, req whatsAppAskRequest, quote *osrun.Run) {
	release, ok, err := h.runs.Lock(ctx, "approve:"+quote.RunID, h.mediaTimeout)
	if err != nil || !ok {
		return // another approval of this quote is already running
	}
	defer release()
	fail := func(runID, text string) { h.tellFailure(ctx, tenantID, runID, "", text) }
	portal := link(req.PortalURL, req.TenantSlug, "/marketing")

	if fresh, err := h.runs.Get(ctx, tenantID.String(), quote.RunID); err == nil {
		quote = fresh
	}
	var approved *osrun.Run
	if quote.ApprovedRunID != "" {
		approved, _ = h.runs.Get(ctx, tenantID.String(), quote.ApprovedRunID)
	}
	if approved == nil {
		var out WhatsAppAskResponse
		ask := askRequest{Text: quote.Text, ApprovePlan: true, Tier: quote.Tier, Platforms: quote.Platforms, viaWhatsApp: true,
			userID: req.UserID, sender: req.Sender, tenantSlug: req.TenantSlug, portalURL: req.PortalURL}
		sink, ok := h.whatsAppTurn(ctx, r, tenantID, req, ask, &out)
		if !ok {
			fail(quote.RunID, out.Reply)
			return
		}
		if res := sink.out.Result; res == nil || res.Route != team.RouteDone {
			// The plan changed on the way (a shortfall, or a question): send it as a normal turn.
			fail(quote.RunID, h.offerApprovalCode(ctx, tenantID, req, sink.out, whatsAppReply(sink.out, portal)))
			return
		}
		if approved, err = h.runs.Get(ctx, tenantID.String(), out.RunID); err != nil {
			fail(quote.RunID, "Approved. The team wrote the drafts; open MarketFlow to make the video: "+portal)
			return
		}
		quote.ApprovedRunID = approved.RunID
		if err := h.runs.Update(ctx, quote); err != nil {
			h.logger.Warn("approved turn not recorded on the quote", zap.String("run_id", quote.RunID), zap.Error(err))
		}
	}
	if _, err := h.makeMedia(ctx, tenantID, approved, -1); err != nil {
		if errors.Is(err, errMediaBusy) {
			return // the other call tells the requester
		}
		fail(approved.RunID, h.mediaFailReply(err, req, "make the video"))
	}
	// On success the storyboard's own message (storyboard_ready) comes from Studio's job callback.
}

// whatsAppRender answers at once and, in the background, approves the storyboard and starts the
// paid render after a fresh quote: a tenant short of credits gets the top-up link and a new code.
func (h *OSHandler) whatsAppRender(_ context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a *osrun.Approval, out *WhatsAppAskResponse) {
	if h.studio == nil || a.VersionID == "" {
		out.Reply = "I cannot start that video from here. Open MarketFlow to render it: " + link(req.PortalURL, req.TenantSlug, "/studio")
		return
	}
	out.Reply = "Making your video now. It takes a few minutes; I'll send the link here when it is ready."
	approval := *a
	h.inBackground(func(bg context.Context) { h.renderInBackground(bg, tenantID, req, approval) })
}

// renderInBackground starts the render once per version (a Redis lock; Studio also refuses a
// version that is no longer waiting). Failures reach the requester as media_failed.
func (h *OSHandler) renderInBackground(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, a osrun.Approval) {
	release, ok, err := h.runs.Lock(ctx, "render:"+a.VersionID, h.mediaTimeout)
	if err != nil || !ok {
		return
	}
	defer release()
	if q, err := h.studio.quote(ctx, tenantID, a.VersionID); err == nil && !q.Enough && q.Shortfall > 0 {
		text := fmt.Sprintf("This video needs about %d credits and you are %d short, so nothing was spent. Top up here: %s",
			q.CreditsNeeded, q.Shortfall, link(req.PortalURL, req.TenantSlug, "/settings/billing"))
		if code, cerr := h.codes.Issue(ctx, a); cerr == nil {
			text += fmt.Sprintf("\n\nThen reply YES %s to make the video (about %d credits).", code, q.CreditsNeeded)
		}
		h.tellFailure(ctx, tenantID, a.RunID, a.VersionID, text)
		return
	}
	if err := h.studio.approveAndRender(ctx, tenantID, a.VersionID); err != nil {
		h.tellFailure(ctx, tenantID, a.RunID, a.VersionID, h.mediaFailReply(err, req, "render it"))
	}
}

// tellFailure sends text to the run's WhatsApp requester through marketflow-api (media_failed).
func (h *OSHandler) tellFailure(ctx context.Context, tenantID uuid.UUID, runID, versionID, text string) {
	if h.studio == nil || strings.TrimSpace(text) == "" {
		return
	}
	if err := h.studio.mediaFailed(ctx, tenantID, runID, versionID, text); err != nil {
		h.logger.Warn("media failure not reported to the requester", zap.String("run_id", runID), zap.Error(err))
	}
}

// mediaFailReply tells a WhatsApp sender why Studio did not go on, and where to go next.
func (h *OSHandler) mediaFailReply(err error, req whatsAppAskRequest, what string) string {
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	var se *studioError
	switch {
	case errors.Is(err, osrun.ErrNoScript):
		return "The team did not write a usable video script this time. Open MarketFlow to try again: " + link(req.PortalURL, req.TenantSlug, "/marketing")
	case errors.As(err, &se) && se.Code == "brand_images_required":
		return fmt.Sprintf("Video ads use pictures of your business. Upload at least %d brand images here: %s\n\nThen open MarketFlow to %s: %s",
			se.minImages(brandImagesDefault), link(req.PortalURL, req.TenantSlug, "/onboarding"), what, link(req.PortalURL, req.TenantSlug, "/marketing"))
	case errors.As(err, &se) && se.Code == "draft_limit":
		return "Too many drafts are waiting in the Studio. Approve or discard some, then " + what + " there: " + studio
	case errors.As(err, &se) && se.Code == "daily_render_limit":
		return "Today's video limit is reached. Nothing was spent; " + what + " from the Studio tomorrow: " + studio
	case errors.As(err, &se) && se.Status < 500 && se.Message != "":
		return "The Studio could not go on: " + se.Message + ". Nothing more was spent. Open it to " + what + ": " + studio
	}
	h.logger.Warn("whatsapp make media failed", zap.Error(err))
	return "I could not start the video just now. Open the Studio to " + what + ": " + studio
}

// offerApprovalCode adds the approval code to a WhatsApp quote that the tenant can afford and
// that was kept: the reply then ends "Reply YES <code> to approve (about N credits)".
func (h *OSHandler) offerApprovalCode(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, out askResponse, reply string) string {
	res := out.Result
	if h.codes == nil || h.runs == nil || req.UserID == "" || res == nil || res.Route != team.RouteQuote ||
		res.Plan == nil || res.Plan.Shortfall > 0 || out.RunID == "" {
		return reply
	}
	if _, err := h.runs.Get(ctx, tenantID.String(), out.RunID); err != nil {
		return reply
	}
	credits := res.Plan.Recommended.Credits
	code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionApprove, RunID: out.RunID, TenantID: tenantID.String(),
		UserID: req.UserID, Sender: req.Sender, Credits: credits})
	if err != nil {
		h.logger.Warn("approval code not issued", zap.String("run_id", out.RunID), zap.Error(err))
		return reply
	}
	portal := link(req.PortalURL, req.TenantSlug, "/marketing")
	reply = strings.Replace(reply, "\n\nApprove it in MarketFlow: "+portal,
		fmt.Sprintf("\n\nThe code below works for %s. You can also approve it in MarketFlow: %s", minutes(h.codes.TTL()), portal), 1)
	return reply + fmt.Sprintf("\n\nReply YES %s to approve (about %d credits)", code, credits)
}

// minutes writes a code's lifetime for a message: "30 minutes", "2 hours".
func minutes(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	n := int(d.Round(time.Minute) / time.Minute)
	if n <= 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", n)
}

// Events marketflow-api reports for a video made from a WhatsApp turn. media_failed is
// marketflow-ai's own background work failing, reported back through marketflow-api so it
// reaches the requester by the same path; its error is the text to send.
const (
	notifyStoryboardReady = "storyboard_ready"
	notifyRenderDone      = "render_done"
	notifyRenderFailed    = "render_failed"
	notifyMediaFailed     = "media_failed"
)

type whatsAppNotifyRequest struct {
	TenantID  string   `json:"tenant_id"`
	RunID     string   `json:"run_id"`
	VersionID string   `json:"version_id"`
	Event     string   `json:"event"`
	Links     []string `json:"links"`
	Error     string   `json:"error"`
}

// whatsAppNotifyResponse is the message for marketflow-api to send from the platform number;
// an empty To means there is nothing to send.
type whatsAppNotifyResponse struct {
	To   string `json:"to,omitempty"`
	Text string `json:"text,omitempty"`
}

// maxStills caps the storyboard links in one message.
const maxStills = 12

// WhatsAppNotify handles POST /api/v1/internal/os/whatsapp-notify (service key): marketflow-api
// reports that a video made from a WhatsApp turn has its storyboard, or finished rendering. The
// reply is the text for the requester's number: the stills with a second YES code, or the
// finished ad's link. marketflow-api sends it through notifications-api, as it sends the
// assistant's replies.
func (h *OSHandler) WhatsAppNotify(w http.ResponseWriter, r *http.Request) {
	var req whatsAppNotifyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil || req.RunID == "" || (req.VersionID == "" && req.Event != notifyMediaFailed) {
		writeError(w, http.StatusBadRequest, "tenant_id, run_id and version_id are required")
		return
	}
	if h.runs == nil {
		writeJSON(w, http.StatusOK, whatsAppNotifyResponse{})
		return
	}
	ctx := r.Context()
	run, err := h.runs.Get(ctx, tenantID.String(), req.RunID)
	if err != nil || run.Channel != osrun.ChannelWhatsApp || run.Sender == "" || run.UserID == "" {
		writeJSON(w, http.StatusOK, whatsAppNotifyResponse{})
		return
	}
	studio := link(run.PortalURL, run.TenantSlug, "/studio")
	if run.BriefID != "" {
		studio = link(run.PortalURL, run.TenantSlug, "/studio/"+run.BriefID)
	}
	var b strings.Builder
	switch req.Event {
	case notifyStoryboardReady:
		credits := 0
		if h.studio != nil {
			if q, err := h.studio.quote(ctx, tenantID, req.VersionID); err == nil {
				credits = q.CreditsNeeded
			}
		}
		code, err := h.codes.Issue(ctx, osrun.Approval{Action: osrun.ActionRender, RunID: run.RunID, TenantID: run.TenantID,
			UserID: run.UserID, Sender: run.Sender, VersionID: req.VersionID, Credits: credits})
		if err != nil {
			h.logger.Warn("render code not issued", zap.String("run_id", run.RunID), zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "the render code could not be made")
			return
		}
		b.WriteString("Your storyboard is ready. One picture per scene:")
		for i, u := range req.Links {
			if i == maxStills {
				break
			}
			fmt.Fprintf(&b, "\n%d. %s", i+1, u)
		}
		fmt.Fprintf(&b, "\n\nTo change words or pictures, open the Studio: %s\nThe code below works for %s.", studio, minutes(h.codes.TTL()))
		if credits > 0 {
			fmt.Fprintf(&b, "\n\nReply YES %s to make the video (about %d credits)", code, credits)
		} else {
			fmt.Fprintf(&b, "\n\nReply YES %s to make the video", code)
		}
	case notifyRenderDone:
		if len(req.Links) == 0 {
			writeJSON(w, http.StatusOK, whatsAppNotifyResponse{})
			return
		}
		fmt.Fprintf(&b, "Your video ad is ready: %s\n\nApprove it, post it or change a scene in the Studio: %s", req.Links[0], studio)
	case notifyRenderFailed:
		reason := strings.TrimSpace(req.Error)
		if reason == "" {
			reason = "the video could not be made right now"
		}
		fmt.Fprintf(&b, "Your video was not made: %s. Open the Studio to try again: %s", trimRunes(reason, 300), studio)
	case notifyMediaFailed:
		text := strings.TrimSpace(req.Error)
		if text == "" {
			writeJSON(w, http.StatusOK, whatsAppNotifyResponse{})
			return
		}
		b.WriteString(trimRunes(text, maxReply))
	default:
		writeError(w, http.StatusBadRequest, "event must be storyboard_ready, render_done, render_failed or media_failed")
		return
	}
	writeJSON(w, http.StatusOK, whatsAppNotifyResponse{To: run.Sender, Text: b.String()})
}

type approvalTenantRequest struct {
	UserID    string   `json:"user_id"`
	Sender    string   `json:"sender"`
	Text      string   `json:"text"`
	TenantIDs []string `json:"tenant_ids"`
}

// approvalTenantResponse says whether the text is an approval reply and, when its code is
// pending for one of the businesses, which.
type approvalTenantResponse struct {
	Yes      bool   `json:"yes"`
	TenantID string `json:"tenant_id,omitempty"`
}

// ApprovalTenant handles POST /api/v1/internal/os/approval-tenant (service key): the WhatsApp
// worker asks which of a sender's businesses a bare "YES <code>" is for when their business pick
// has expired. The code is looked up for that user and number only and is not used here.
func (h *OSHandler) ApprovalTenant(w http.ResponseWriter, r *http.Request) {
	var req approvalTenantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil || req.UserID == "" || len(req.TenantIDs) > 50 {
		writeError(w, http.StatusBadRequest, "user_id and at most 50 tenant_ids are required")
		return
	}
	code, yes := osrun.ParseYes(req.Text)
	out := approvalTenantResponse{Yes: yes}
	if yes && code != "" {
		out.TenantID, _ = h.codes.Find(r.Context(), req.TenantIDs, req.UserID, req.Sender, code)
	}
	writeJSON(w, http.StatusOK, out)
}
