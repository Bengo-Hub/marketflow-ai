package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bengobox/marketflow-ai/internal/assistgate"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-ai/internal/agent"
	"github.com/bengobox/marketflow-ai/internal/guardrail"
	"github.com/bengobox/marketflow-ai/internal/library"
	"github.com/bengobox/marketflow-ai/internal/llm"
	"github.com/bengobox/marketflow-ai/internal/media"
	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/promptopt"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/registry"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/bengobox/marketflow-ai/internal/team"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// OSHandler serves the Marketing OS: the Marketing Manager entry point, the
// optimiser preview, Creative Plan quotes, quick-start templates, the team
// roster, custom-agent runs and the daily brief wording.
type OSHandler struct {
	runtime  *team.Runtime
	opt      *promptopt.Optimizer
	lib      *library.Library
	source   team.ContextSource
	pipeline *rag.Pipeline
	agent    *agent.Loop
	credits  CreditSpender
	registry func(ctx context.Context) registry.Registry
	logger   *zap.Logger

	meetingPrep meetingPrepper
	posters     *media.Posters
	jobs        *media.Jobs // motion posters and video ads (video.go)
	clips       *media.Clips
	ads         *media.Ads
	storyboards *media.Storyboards
	gate        *assistgate.Gate  // the WhatsApp assistant gate (whatsapp_ask.go)
	sessions    *sessions.Service // WhatsApp assistant transcripts (whatsapp_ask.go)
	// Quoted and approved media turns, WhatsApp approval codes and the Studio they are made
	// in (make_media.go); nil turns the make-media step off.
	runs   *osrun.Store
	codes  *osrun.Codes
	studio *studioClient
	// mediaTimeout bounds the background work after a WhatsApp YES; spawn starts it (a
	// goroutine; tests replace it to run the work when they choose).
	mediaTimeout time.Duration
	spawn        func(func())
}

// OSConfig configures an OSHandler. Credits may be nil (unmetered, local dev).
type OSConfig struct {
	Runtime  *team.Runtime
	Opt      *promptopt.Optimizer
	Library  *library.Library
	Source   team.ContextSource
	Pipeline *rag.Pipeline
	Agent    *agent.Loop
	Credits  CreditSpender
	Registry func(ctx context.Context) registry.Registry
	Logger   *zap.Logger
}

// NewOSHandler builds the handler.
func NewOSHandler(cfg OSConfig) *OSHandler {
	return &OSHandler{runtime: cfg.Runtime, opt: cfg.Opt, lib: cfg.Library, source: cfg.Source, pipeline: cfg.Pipeline,
		agent: cfg.Agent, credits: cfg.Credits, registry: cfg.Registry, logger: cfg.Logger.Named("os.handler")}
}

const maxAskChars = 2000

// privileged callers may act for a tenant they name: platform owners (auth-api's is_platform_owner
// claim) and service callers. A "superuser" role is tenant-scoped and grants nothing here
// (security audit 2026-11-10).
func privileged(c *authclient.Claims) bool { return c.IsPlatformOwner || c.IsService }

// tenantFromAuth returns the caller's tenant from the token. A user's
// X-Tenant-ID must match their token; only privileged callers may name
// another tenant in the header.
func tenantFromAuth(r *http.Request) (uuid.UUID, int, string) {
	header := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		return uuid.Nil, http.StatusUnauthorized, "authentication required"
	}
	if privileged(claims) && header != "" {
		if h, err := uuid.Parse(header); err == nil {
			return h, 0, ""
		}
		return uuid.Nil, http.StatusBadRequest, "X-Tenant-ID must be a tenant UUID"
	}
	tid, err := claims.TenantUUID()
	if err != nil || tid == nil {
		return uuid.Nil, http.StatusBadRequest, "a tenant is required"
	}
	if header != "" && header != tid.String() {
		return uuid.Nil, http.StatusForbidden, "X-Tenant-ID does not match your account"
	}
	return *tid, 0, ""
}

// headerTenantAllowed reports whether a resolved header tenant may be used by the
// caller: always for privileged callers, otherwise only when it is the token's
// own tenant. Requests without claims are refused: callers skip this check only on the
// public widget path.
func headerTenantAllowed(r *http.Request, resolved uuid.UUID) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		return false
	}
	if privileged(claims) {
		return true
	}
	tid, err := claims.TenantUUID()
	return err == nil && tid != nil && *tid == resolved
}

type askRequest struct {
	Text        string   `json:"text"`
	ApprovePlan bool     `json:"approve_plan"`
	Tier        string   `json:"tier"`
	Platforms   []string `json:"platforms"`
	// Playbook pins a guided flow's route (for example "brand-context").
	Playbook string `json:"playbook"`
	// TeamSetup is the console's "Set up my team" button: the turn is the team setup, with no
	// intent detection (team_setup.go).
	TeamSetup bool `json:"team_setup"`
	// Stream answers as server-sent events: one progress event per stage, then
	// the same response as the JSON reply in a final "result" event.
	Stream bool `json:"stream"`
	// viaWhatsApp is set by the WhatsApp assistant, whose sender identity and role were
	// verified upstream; it is never read from a request body.
	viaWhatsApp bool
	// userID, sender, tenantSlug and portalURL are kept with a media turn (osrun.Run): who
	// asked, their verified WhatsApp number, and the portal links. Never read from a body.
	userID, sender, tenantSlug, portalURL string
}

// askSink is where Ask writes its outcome: plain JSON, or a stream of events.
type askSink interface {
	progress(p team.Progress)
	fail(status int, msg string)
	done(out askResponse)
}

type jsonSink struct{ w http.ResponseWriter }

func (jsonSink) progress(team.Progress)        {}
func (s jsonSink) fail(status int, msg string) { writeError(s.w, status, msg) }
func (s jsonSink) done(out askResponse)        { writeJSON(s.w, http.StatusOK, out) }

// sseSink streams progress so the console can say who is working on what.
// Headers go out when it is created, so errors after that are events, not
// statuses. The client must send Accept: text/event-stream, which also keeps
// the router's request timeout off the stream (httpware.BypassForStreaming);
// the team's own 95s budget still bounds it.
type sseSink struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSESink(w http.ResponseWriter) (*sseSink, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	httpware.StreamHeaders(w)
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseSink{w: w, f: f}, true
}

func (s *sseSink) event(v any) {
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.f.Flush()
}

func (s *sseSink) progress(p team.Progress) {
	s.event(map[string]any{"type": "progress", "data": p})
}

func (s *sseSink) fail(status int, msg string) {
	s.event(map[string]any{"type": "error", "status": status, "error": msg})
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.f.Flush()
}

func (s *sseSink) done(out askResponse) {
	s.event(map[string]any{"type": "result", "data": out})
	_, _ = fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.f.Flush()
}

type askResponse struct {
	RunID          string       `json:"run_id"`
	Result         *team.Result `json:"result"`
	Answer         string       `json:"answer,omitempty"`
	// AnswerReviewNotes are the marketing-law notes on Answer (advice only, rag.ChatResult).
	AnswerReviewNotes []string `json:"answer_review_notes,omitempty"`
	CreditsCharged    int      `json:"credits_charged"`
}

// Ask handles POST /api/v1/os/ask: one request to the Marketing Manager.
// It costs one credit (the existing chat price); an approved copy batch adds the
// registry's copy batch price. Charges are refunded when nothing was produced.
func (h *OSHandler) Ask(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var req askRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || len([]rune(req.Text)) > maxAskChars {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("text is required (max %d characters)", maxAskChars))
		return
	}
	if blocked, reply := guardrail.ScreenInput(req.Text); blocked {
		writeJSON(w, http.StatusOK, askResponse{Answer: reply, Result: &team.Result{Route: "blocked"}})
		return
	}

	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil {
		req.userID = c.Subject
	}
	r = r.WithContext(guardrail.WithActor(r.Context(), actorFor(r, guardrail.OriginChat)))

	runID := uuid.NewString()
	// The free first team setup comes before the credit check, so zero credits never block it.
	if out, status, msg, handled := h.firstTeamSetup(r.Context(), r, tenantID, req, runID); handled {
		sink, _ := h.askSinkFor(w, r, req)
		if status != 0 {
			sink.fail(status, msg)
			return
		}
		sink.done(out)
		return
	}
	turnKey, ok := h.charge(w, r, tenantID, 1, "os:"+runID+":turn", "Marketing team request")
	if !ok {
		return
	}
	sink, ctx := h.askSinkFor(w, r, req)
	h.runTurn(ctx, r, tenantID, req, runID, turnKey, sink)
}

// askSinkFor answers as server-sent events when the request asked for a stream and the writer
// can flush, otherwise as one JSON reply. The context carries the progress callback.
func (h *OSHandler) askSinkFor(w http.ResponseWriter, r *http.Request, req askRequest) (askSink, context.Context) {
	ctx := r.Context()
	if req.Stream {
		if s, ok := newSSESink(w); ok {
			return s, team.WithProgress(ctx, s.progress)
		}
	}
	return jsonSink{w}, ctx
}

// runTurn runs one charged Marketing Manager turn and writes the outcome to sink. The portal
// (Ask) and the WhatsApp assistant (WhatsAppAsk) share it, so a turn behaves the same on both.
// turnKey is the credit spend to refund when nothing was produced ("" when nothing was charged).
func (h *OSHandler) runTurn(ctx context.Context, r *http.Request, tenantID uuid.UUID, req askRequest, runID, turnKey string, sink askSink) {
	if h.teamTurn(ctx, r, tenantID, req, runID, turnKey, sink) {
		return
	}
	available := h.balance(ctx, tenantID)
	meter := &llm.Meter{}
	ctx = llm.WithMeter(ctx, meter)
	res, err := h.runtime.Ask(ctx, team.Request{
		TenantID: tenantID, Text: req.Text, ApprovePlan: req.ApprovePlan,
		Tier: req.Tier, Platforms: req.Platforms, CreditsAvailable: available, Playbook: req.Playbook,
	})
	if errors.Is(err, team.ErrBadPlaybook) {
		h.refundUnlessPaid(r, tenantID, turnKey, meter)
		sink.fail(http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		h.logger.Error("os ask failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		h.refundUnlessPaid(r, tenantID, turnKey, meter)
		sink.fail(http.StatusInternalServerError, "the marketing team could not finish this request")
		return
	}
	if res.Route == team.RouteClarify {
		// A clarifying question produced nothing; the owner pays when the answered
		// request runs (unless a paid model already worked on it).
		if h.refundUnlessPaid(r, tenantID, turnKey, meter) {
			turnKey = ""
		}
	}
	out := askResponse{RunID: runID, Result: res}
	if res.Route == team.RouteChat && h.pipeline != nil {
		sink.progress(team.Progress{Stage: "answering", Member: "manager", Label: "Looking up the answer"})
		chat, err := h.pipeline.Chat(ctx, rag.ChatInput{TenantID: tenantID, SessionID: uuid.New(), Question: req.Text, BusinessType: h.chatBusinessType(ctx, tenantID)})
		if err != nil {
			h.refundUnlessPaid(r, tenantID, turnKey, meter)
			sink.fail(http.StatusInternalServerError, "AI processing failed")
			return
		}
		out.Answer = chat.Response
		out.AnswerReviewNotes = chat.ReviewNotes
	}
	if turnKey != "" {
		out.CreditsCharged = 1
	}
	if res.CopyBatchRun {
		price, _ := h.registry(ctx).Credits(registry.CopyBatch)
		if price > 0 {
			if key, ok := h.chargeQuiet(ctx, tenantID, int64(price), "os:"+runID+":copy", "Ad copy batch"); ok && key != "" {
				out.CreditsCharged += price
			}
		}
	}
	h.keepRun(ctx, tenantID, req, runID, res)
	sink.done(out)
}

// Optimize handles POST /api/v1/os/optimize: the TaskSpec preview ("We
// understood this as..."). It is free.
func (h *OSHandler) Optimize(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	var tc promptopt.Context
	if h.source != nil {
		if loaded, err := h.source.Load(r.Context(), tenantID); err == nil {
			tc.Offerings = loaded.Offerings
		}
	}
	spec, err := h.opt.Optimize(r.Context(), tenantID.String(), req.Text, tc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

// Quote handles POST /api/v1/os/plan/quote: a Creative Plan from explicit inputs.
func (h *OSHandler) Quote(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var in registry.PlanInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.AdBudgetKES < 0 || in.Days < 0 || in.Days > 366 {
		writeError(w, http.StatusBadRequest, "budget and days must be positive (days at most 366)")
		return
	}
	in.CreditsAvailable = h.balance(r.Context(), tenantID)
	if h.source != nil {
		if tc, err := h.source.Load(r.Context(), tenantID); err == nil {
			in.HasPlan, in.Features = tc.HasPlan, tc.Features
		}
	}
	plan, err := h.runtime.Quote(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// Templates handles GET /api/v1/os/library/templates: the templates with their facets, a media
// template's credit price from the registry, and the facet labels the gallery's filters show.
func (h *OSHandler) Templates(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list := h.lib.Templates(r.URL.Query().Get("q"), r.URL.Query().Get("category"), limit)
	if h.registry != nil {
		reg := h.registry(r.Context())
		for i := range list {
			if list[i].Tier == "" {
				continue
			}
			if c, err := reg.Credits(list[i].Tier); err == nil {
				list[i].Credits = &c
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list, "facets": h.lib.Facets()})
}

type teamMember struct {
	library.Member
	Enabled bool `json:"enabled"`
}

// Team handles GET /api/v1/os/team: the default team with the tenant's overrides.
func (h *OSHandler) Team(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	var overrides []team.MemberOverride
	if h.source != nil {
		if tc, err := h.source.Load(r.Context(), tenantID); err == nil {
			overrides = tc.Members
		}
	}
	members := make([]teamMember, 0, 10)
	for _, m := range h.lib.Members() {
		tm := teamMember{Member: m, Enabled: true}
		for _, o := range overrides {
			if o.Key != m.Key {
				continue
			}
			if o.Name != "" {
				tm.Name = o.Name
			}
			if o.ModelClass != "" {
				tm.ModelClass = o.ModelClass
			}
			tm.Enabled = !o.Disabled || m.Key == "manager"
		}
		members = append(members, tm)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members, "playbooks": h.lib.Playbooks()})
}

type agentRunRequest struct {
	RunID         string         `json:"run_id"`
	TenantID      string         `json:"tenant_id"`
	SystemPrompt  string         `json:"system_prompt"`
	AllowedTools  []string       `json:"allowed_tools"`
	// NoTools asks for a plain completion with no tools (ticketing's summaries and reply drafts).
	// An empty allowed_tools still means the read-only knowledge tools.
	NoTools       bool           `json:"no_tools"`
	MaxIterations int            `json:"max_iterations"`
	Context       map[string]any `json:"context"`
	// Billable is set by the worker for automatic (event and schedule) runs,
	// which cost the tenant one AI credit; Run now is free.
	Billable bool `json:"billable"`
}

// agentRunCredits is the price of one automatic custom-agent run, the same as
// a Marketing team request.
const agentRunCredits = 1

const customAgentSystem = `You are a custom MarketFlow agent working for one business. Follow the owner's instructions below. Use only the tools you are given. Never invent data; if a tool fails, say what you could not do.`

// spendTools are never run by an automatic agent, whatever its allowed_tools say: they create ad
// campaigns and published pages, which only a person launches (owner decision G5, 2026-11-16).
var spendTools = map[string]bool{"create_campaign": true, "create_funnel": true}

// agentRunTools is the tool list an automatic run gets: the agent's allowed_tools minus the
// spend tools, or the read-only knowledge tools when none are listed.
func agentRunTools(requested []string) []string {
	out := make([]string, 0, len(requested))
	for _, t := range requested {
		if !spendTools[t] {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return []string{"kb_search", "get_product", "get_faq"}
	}
	return out
}

// AgentRun handles POST /api/v1/agent/run (S2S): one custom-agent run from the
// worker, restricted to the agent's allowed_tools. The worker records the trace.
func (h *OSHandler) AgentRun(w http.ResponseWriter, r *http.Request) {
	if h.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "the agent loop is disabled")
		return
	}
	var req agentRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	iters := req.MaxIterations
	if iters <= 0 || iters > 20 {
		iters = 5
	}
	trigger, _ := json.Marshal(req.Context)
	// The trigger is event data (a lead's message, a form, a comment): fenced as data, and a run
	// whose trigger carries an injection phrase starts with its writing tools withdrawn.
	triggerText := guardrail.MaskPII(string(trigger))
	tainted := guardrail.LooksInjected(string(trigger))
	system := customAgentSystem + "\n" + guardrail.UntrustedRule
	if s := strings.TrimSpace(req.SystemPrompt); s != "" {
		system += "\n\nOWNER INSTRUCTIONS:\n" + s
	}
	// Automatic runs are paid before they start (fail closed: no credit check,
	// no run), keyed on the run so a retried delivery is charged once, and
	// refunded when the run fails.
	chargeKey := ""
	if req.Billable && h.credits != nil {
		dec, cerr := h.spend(r.Context(), tenantID, agentRunCredits, "agent:"+req.RunID, "Automatic agent run")
		switch {
		case cerr != nil:
			h.logger.Warn("agent run credit check failed", zap.String("run_id", req.RunID), zap.Error(cerr))
			writeJSON(w, http.StatusOK, map[string]any{"run_id": req.RunID, "status": "failed",
				"error": "Could not check AI credits, so this automatic run did not start. It will run on the next trigger."})
			return
		case !dec.Allowed:
			writeJSON(w, http.StatusOK, map[string]any{"run_id": req.RunID, "status": "failed",
				"error": "Not enough AI credits for an automatic run. Top up credits; Run now still works."})
			return
		case !dec.Exempt:
			chargeKey = "agent:" + req.RunID
		}
	}

	meter := &llm.Meter{}
	ctx, cancel := context.WithTimeout(llm.WithMeter(r.Context(), meter), 100*time.Second)
	defer cancel()
	allowed := agentRunTools(req.AllowedTools)
	ctx = guardrail.WithActor(ctx, guardrail.Actor{Origin: guardrail.OriginAgentRun})
	res, err := h.agent.Run(ctx, agent.Input{
		TenantID:     tenantID,
		SystemPrompt: system,
		Question:     "Run now. Trigger context:\n" + guardrail.Quote("trigger", triggerText),
		MaxIters:     iters,
		AllowedTools: allowed,
		NoTools:      req.NoTools,
		Tainted:      tainted,
	})
	if err != nil {
		h.logger.Warn("custom agent run failed", zap.String("run_id", req.RunID), zap.Error(err))
		h.refundUnlessPaid(r, tenantID, chargeKey, meter)
		writeJSON(w, http.StatusOK, map[string]any{"run_id": req.RunID, "status": "failed", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": req.RunID, "status": "completed", "answer": res.Answer, "trace": res.Trace,
		"total_tokens": res.TotalTokens, "tool_calls": res.ToolCallsN, "iterations": res.Iterations,
		"stopped_early": res.StoppedEarly, "stop_reason": res.StopReason,
		"review_notes": agentRunReviewNotes(res, system, string(trigger)),
		"tainted":      res.Tainted,
	})
}

// agentRunReviewNotes runs the outward figure and commitment checks on a run's answer. A run's
// answer is a draft (a person edits a ticket reply before it is sent; a custom agent acts only
// through its tools), so the notes are returned for the caller to show, never used to block or
// rewrite. Figures may come from the owner's instructions, the trigger data and the tool results.
func agentRunReviewNotes(res *agent.Result, system, trigger string) []string {
	if res == nil {
		return []string{}
	}
	allowed := guardrail.AllowedNumbers(append([]string{system, trigger}, res.ToolResults()...)...)
	notes := guardrail.OutputViolations(res.Answer, allowed)
	if notes == nil {
		notes = []string{}
	}
	return notes
}

// meetingPrepper is the meeting_prep agent tool: it reads the contact's 360
// view from marketflow-api and writes a briefing with talking points.
type meetingPrepper interface {
	Invoke(ctx context.Context, tenantID uuid.UUID, argsJSON string) (string, error)
}

// meetingPrepBudget sits under the worker's 30s client, so a slow model costs
// the talking points, never the reminder.
const meetingPrepBudget = 20 * time.Second

// SetMeetingPrep wires the meeting_prep tool for the worker's reminders.
func (h *OSHandler) SetMeetingPrep(t meetingPrepper) { h.meetingPrep = t }

// MeetingPrep handles POST /api/v1/internal/os/meeting-prep (S2S): the worker
// asks for a briefing and talking points before a meeting. Any failure is an
// error status; the worker then sends its reminder without them.
func (h *OSHandler) MeetingPrep(w http.ResponseWriter, r *http.Request) {
	if h.meetingPrep == nil {
		writeError(w, http.StatusServiceUnavailable, "meeting prep is not configured")
		return
	}
	var req struct {
		TenantID  string `json:"tenant_id"`
		ContactID string `json:"contact_id"`
		Purpose   string `json:"purpose"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
		return
	}
	if _, err := uuid.Parse(req.ContactID); err != nil {
		writeError(w, http.StatusBadRequest, "contact_id must be a UUID")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), meetingPrepBudget)
	defer cancel()
	args, _ := json.Marshal(map[string]string{"contact_id": req.ContactID, "meeting_purpose": trimRunes(req.Purpose, 300)})
	out, err := h.meetingPrep.Invoke(ctx, tenantID, string(args))
	if err != nil {
		h.logger.Warn("meeting prep failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		writeError(w, http.StatusBadGateway, "meeting prep failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(out))
}

func trimRunes(s string, n int) string {
	if r := []rune(strings.TrimSpace(s)); len(r) > n {
		return string(r[:n])
	}
	return strings.TrimSpace(s)
}

// DailyBrief handles POST /api/v1/internal/os/daily-brief (S2S): marketflow-api
// sends the computed metrics and gets the highlights and wording back.
func (h *OSHandler) DailyBrief(w http.ResponseWriter, r *http.Request) {
	var in team.BriefInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || len(in.Metrics) == 0 {
		writeError(w, http.StatusBadRequest, "metrics are required")
		return
	}
	out, err := h.runtime.Brief(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// charge spends credits fail-closed, writing the refusal; key is empty when
// nothing was charged (no billing configured, or an exempt tenant).
func (h *OSHandler) charge(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, units int64, key, desc string) (string, bool) {
	if h.credits == nil {
		return "", true
	}
	dec, err := h.spend(r.Context(), tenantID, units, key, desc)
	if err != nil {
		h.logger.Warn("ai credit check failed (fail-closed)", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "AI credits could not be checked right now, try again shortly")
		return "", false
	}
	if !dec.Allowed {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error": dec.Reason, "message": "AI credits are used up. Buy a bundle from your billing page to keep going.",
		})
		return "", false
	}
	if dec.Exempt {
		return "", true
	}
	return key, true
}

// chargeQuiet spends after the work is done; a refusal is logged, not returned,
// because the result was already produced inside the approved quote.
func (h *OSHandler) chargeQuiet(ctx context.Context, tenantID uuid.UUID, units int64, key, desc string) (string, bool) {
	if h.credits == nil {
		return "", false
	}
	dec, err := h.spend(ctx, tenantID, units, key, desc)
	if err != nil || !dec.Allowed {
		h.logger.Warn("post-run credit spend not taken", zap.String("tenant_id", tenantID.String()), zap.String("key", key), zap.Error(err))
		return "", false
	}
	if dec.Exempt {
		return "", false
	}
	return key, true
}

func (h *OSHandler) spend(ctx context.Context, tenantID uuid.UUID, units int64, key, desc string) (CreditDecision, error) {
	if s, ok := h.credits.(interface {
		SpendFor(context.Context, uuid.UUID, int64, string, string) (CreditDecision, error)
	}); ok {
		return s.SpendFor(ctx, tenantID, units, key, desc)
	}
	return h.credits.Spend(ctx, tenantID, units, key)
}

// refundUnlessPaid gives a credit flagged for refund back only when no paid
// model was used for it (owner rule, 2026-10-24): paid tokens already spent are
// not returned. It reports whether the refund was made.
func (h *OSHandler) refundUnlessPaid(r *http.Request, tenantID uuid.UUID, key string, m *llm.Meter) bool {
	if key == "" || m.Paid() {
		return false
	}
	h.refund(r, tenantID, key)
	return true
}

func (h *OSHandler) refund(r *http.Request, tenantID uuid.UUID, key string) {
	if key == "" || h.credits == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := h.credits.Refund(ctx, tenantID, key); err != nil {
		h.logger.Error("ai credit refund did not go through", zap.String("key", key), zap.Error(err))
	}
}

// balance returns spendable credits, or -1 when unknown or unlimited.
func (h *OSHandler) balance(ctx context.Context, tenantID uuid.UUID) int {
	b, ok := h.credits.(CreditBalance)
	if !ok {
		return -1
	}
	n, unlimited, err := b.Balance(ctx, tenantID)
	if err != nil || unlimited {
		return -1
	}
	return int(n)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
