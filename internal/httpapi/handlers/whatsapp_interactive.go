package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/assistgate"
	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/registry"
)

// WhatsApp reply buttons, list messages and media (Sprint 17, owner 2026-11-14). marketflow-ai
// writes what to send; marketflow-api sends it from the platform number. Every reply still has
// its plain text (Reply, or Text in whatsapp-notify), which marketflow-api sends instead when the
// requester's 24-hour window is closed, and notifications-api sends when Meta refuses the
// interactive message. That text keeps the typed "Reply YES <code>" way to approve.

// waInteractive is the rich form of one reply. Media go first, one message each; then Body with
// either Buttons (up to 3) or List (up to 10 rows).
type waInteractive struct {
	Body    string    `json:"body,omitempty"`
	Footer  string    `json:"footer,omitempty"`
	Buttons []waBtn   `json:"buttons,omitempty"`
	List    *waList   `json:"list,omitempty"`
	Media   []waMedia `json:"media,omitempty"`
}

type waBtn struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type waList struct {
	Button  string  `json:"button"`
	Section string  `json:"section,omitempty"`
	Rows    []waRow `json:"rows"`
}

type waRow struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// waMedia is an image or video Meta fetches by link, with its caption.
type waMedia struct {
	Type    string `json:"type"`
	Link    string `json:"link"`
	Caption string `json:"caption,omitempty"`
}

// Button titles (Meta allows 20 characters).
const (
	btnApprove   = "Approve"
	btnCheaper   = "Cheaper option"
	btnCancel    = "Cancel"
	btnMakeVideo = "Make the video"
	btnChange    = "Change"
	btnDiscard   = "Discard"
)

// quoteButtons are the plan quote's Approve, Cheaper option and Cancel. Cheaper option is left
// out when the plan has no cheaper way.
func quoteButtons(runID, code string, cheaper bool) []waBtn {
	btns := []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, runID, code), Title: btnApprove}}
	if cheaper {
		btns = append(btns, waBtn{ID: osrun.ReplyID(osrun.ReplyCheaper, runID, ""), Title: btnCheaper})
	}
	return append(btns, waBtn{ID: osrun.ReplyID(osrun.ReplyCancel, runID, ""), Title: btnCancel})
}

// storyboardButtons are the storyboard's Make the video, Change and Discard.
func storyboardButtons(runID, code string) []waBtn {
	return []waBtn{
		{ID: osrun.ReplyID(osrun.ReplyYes, runID, code), Title: btnMakeVideo},
		{ID: osrun.ReplyID(osrun.ReplyChange, runID, ""), Title: btnChange},
		{ID: osrun.ReplyID(osrun.ReplyDiscard, runID, ""), Title: btnDiscard},
	}
}

// cheaperOptions are the plan's alternatives that cost less than the recommended option.
func cheaperOptions(p *registry.CreativePlan) []registry.Option {
	if p == nil {
		return nil
	}
	var out []registry.Option
	for _, o := range p.Alternatives {
		if o.Credits < p.Recommended.Credits {
			out = append(out, o)
		}
	}
	return out
}

// clarifyList is the list message for a missing TaskSpec field, or nil when the field needs the
// owner's own words.
func clarifyList(field string) *waList {
	opts := osrun.FieldOptions(field)
	if len(opts) == 0 {
		return nil
	}
	l := &waList{Button: "Choose", Rows: make([]waRow, 0, len(opts))}
	for i, o := range opts {
		l.Rows = append(l.Rows, waRow{ID: osrun.FieldRowID(field, i), Title: o.Title, Description: o.Description})
	}
	return l
}

// approveLine is how a message with buttons says what to do, keeping the typed way.
func approveLine(button, code, what string, credits int) string {
	if credits > 0 {
		return fmt.Sprintf("Tap %s, or reply YES %s, to %s (about %d credits).", button, code, what, credits)
	}
	return fmt.Sprintf("Tap %s, or reply YES %s, to %s.", button, code, what)
}

// tapVerdict marks a tapped button as handled without the gate (a title has no marketing words).
var tapVerdict = assistgate.Verdict{Pass: true, Reason: "button reply", Source: "rules"}

// replyTap handles a tapped reply button or list row (req.ReplyID). handled is false when the id
// is not ours, and for a list row that completed a waiting request: req.Text is then the whole
// request and the normal turn runs.
func (h *OSHandler) replyTap(ctx context.Context, r *http.Request, tenantID uuid.UUID, req *whatsAppAskRequest) (WhatsAppAskResponse, bool) {
	rep, ok := osrun.ParseReply(req.ReplyID)
	if !ok || h.codes == nil || req.UserID == "" {
		return WhatsAppAskResponse{}, false
	}
	tid := tenantID.String()
	out := WhatsAppAskResponse{Pass: true, Verdict: tapVerdict, Route: "approval"}
	switch rep.Kind {
	case osrun.ReplyField:
		cl, ok := h.codes.TakeClarify(ctx, tid, req.UserID, req.Sender, rep.Field)
		answer, aok := osrun.FieldAnswer(rep.Field, rep.Row)
		if !ok || !aok {
			out.Route = "clarify"
			out.Reply = "That question has expired. Send your request again with the details."
			return out, true
		}
		req.Text = cl.Text + "\n" + answer
		req.SessionActive = true
		return WhatsAppAskResponse{}, false
	case osrun.ReplyYes:
		out = h.redeemApproval(ctx, r, tenantID, *req, rep.Code, rep.RunID)
		return out, true
	}

	pending, hasPending := h.codes.PendingApproval(ctx, tid, req.UserID)
	sameRun := hasPending && pending.RunID == rep.RunID && (pending.Sender == "" || req.Sender == "" || pending.Sender == req.Sender)
	run, runErr := h.runs.Get(ctx, tid, rep.RunID)
	portal := link(req.PortalURL, req.TenantSlug, "/marketing")
	studio := link(req.PortalURL, req.TenantSlug, "/studio")
	if runErr == nil && run.BriefID != "" {
		studio = link(req.PortalURL, req.TenantSlug, "/studio/"+run.BriefID)
	}
	switch rep.Kind {
	case osrun.ReplyCancel:
		if sameRun {
			h.codes.Cancel(ctx, tid, req.UserID)
		}
		out.Reply = "Cancelled. Nothing was spent. Send a new request whenever you are ready."
	case osrun.ReplyDiscard:
		if sameRun {
			h.codes.Cancel(ctx, tid, req.UserID)
		}
		// The draft is kept for a while in case the tap was a mistake, then deleted unless the
		// owner opens and changes it (owner, 2026-11-14).
		if runErr == nil && run.VersionID != "" && h.studio != nil {
			if err := h.studio.discarded(ctx, tenantID, run.RunID, run.VersionID); err != nil {
				h.logger.Warn("discard not reported to the studio", zap.String("run_id", run.RunID), zap.Error(err))
			}
		}
		out.Reply = "Discarded. No video will be made and nothing more was spent. The draft stays in the Studio for a few days in case you want it, then it is deleted: " + studio
	case osrun.ReplyCheaper:
		if runErr != nil || run.Plan == nil {
			out.Reply = "That plan has expired, so nothing was spent. Send your request again for a new quote."
			break
		}
		var b strings.Builder
		opts := cheaperOptions(run.Plan)
		if len(opts) == 0 {
			b.WriteString("This plan is already the cheapest way to do it.")
		} else {
			b.WriteString("Cheaper ways to do this:")
			for _, o := range opts {
				fmt.Fprintf(&b, "\n- %s: %d credits.", o.Label, o.Credits)
				if o.Description != "" {
					b.WriteString(" " + o.Description)
				}
			}
			b.WriteString("\n\nTo go with one, send your request again asking for it (for example \"posters instead of video\"), or choose it in MarketFlow: " + portal)
		}
		out.Reply = b.String()
		if sameRun {
			out.Reply += fmt.Sprintf("\n\nThe plan I quoted still stands: reply YES %s to approve it.", pending.Code)
			out.Interactive = &waInteractive{
				Body:    b.String() + "\n\nThe plan I quoted still stands. " + approveLine(btnApprove, pending.Code, "approve it", pending.Credits),
				Buttons: []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, rep.RunID, pending.Code), Title: btnApprove}, {ID: osrun.ReplyID(osrun.ReplyCancel, rep.RunID, ""), Title: btnCancel}},
			}
		}
	case osrun.ReplyChange:
		text := "To change the words or the pictures, open the Studio: " + studio
		out.Reply = text
		if sameRun {
			out.Reply += fmt.Sprintf("\n\nWhen you are happy with it, reply YES %s to make the video.", pending.Code)
			out.Interactive = &waInteractive{
				Body: text + "\n\nWhen you are happy with it: " + approveLine(btnMakeVideo, pending.Code, "make the video", pending.Credits),
				Buttons: []waBtn{{ID: osrun.ReplyID(osrun.ReplyYes, rep.RunID, pending.Code), Title: btnMakeVideo},
					{ID: osrun.ReplyID(osrun.ReplyDiscard, rep.RunID, ""), Title: btnDiscard}},
			}
		}
	default:
		return WhatsAppAskResponse{}, false
	}
	h.recordWhatsApp(ctx, tenantID, req.UserID, req.Text, out.Reply, out.Route)
	return out, true
}

// offerClarifyList turns a missing-field question into a list message and keeps the request, so
// a tapped row completes it. Fields that need the owner's own words stay a typed question.
func (h *OSHandler) offerClarifyList(ctx context.Context, tenantID uuid.UUID, req whatsAppAskRequest, out askResponse, reply string) *waInteractive {
	res := out.Result
	if h.codes == nil || req.UserID == "" || res == nil || res.Spec == nil || len(res.Spec.MissingFields) == 0 || res.Question == "" {
		return nil
	}
	field := res.Spec.MissingFields[0]
	list := clarifyList(field)
	if list == nil {
		return nil
	}
	if err := h.codes.SaveClarify(ctx, tenantID.String(), req.UserID, osrun.Clarify{Text: req.Text, Field: field, Sender: req.Sender}); err != nil {
		return nil
	}
	return &waInteractive{Body: reply + "\n\nChoose one below, or type your answer with your request.", List: list}
}
