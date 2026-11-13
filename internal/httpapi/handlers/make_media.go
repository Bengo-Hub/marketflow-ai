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
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-ai/internal/osrun"
	"github.com/bengobox/marketflow-ai/internal/team"
)

// Make media (Sprint 15 #134/#138, Sprint 17): an approved Marketing Manager turn becomes a
// Studio video ad. marketflow-ai keeps the turn (osrun); marketflow-api owns the brief and the
// version. The steps go through Studio's own routes with the service key, so every Studio gate
// stays: brand images, the draft limit, the storyboard, then the daily cap, the quote, the
// render charge and the sound extras when the owner renders.
//
//  1. POST /api/v1/creative/briefs with metadata {plan, source: os_turn, run_id, requested_by}
//  2. POST /api/v1/creative/briefs/{id}/video-ads with the turn's script (no second script charge)
//  3. POST /api/v1/creative/versions/{id}/storyboard
//
// MarketFlow stops at the storyboard (owner, 2026-11-12); the render needs a second approval.

// WithMakeMedia turns on the kept turns, the WhatsApp approval codes and the make-media step.
// studioURL is marketflow-api's root and studioKey a platform service key it accepts with
// X-Tenant-ID; eventsKey is the internal service key for marketflow-api's internal routes (the
// background failures a WhatsApp requester is told about). An empty studio key keeps the turns
// but leaves make-media off. cfg.MediaTimeout bounds the background work after a WhatsApp YES.
func (h *OSHandler) WithMakeMedia(runs *osrun.Store, codes *osrun.Codes, cfg osrun.Config, studioURL, studioKey, eventsKey string) *OSHandler {
	h.runs, h.codes = runs, codes
	h.mediaTimeout = cfg.MediaTimeout
	if h.mediaTimeout <= 0 {
		h.mediaTimeout = osrun.DefaultConfig().MediaTimeout
	}
	if strings.TrimSpace(studioURL) != "" && strings.TrimSpace(studioKey) != "" {
		h.studio = newStudioClient(studioURL, studioKey)
		h.studio.eventsKey = eventsKey
	}
	return h
}

// inBackground runs fn after the WhatsApp reply has gone back, with a context of its own that
// the request ending does not cancel, bounded by the media timeout.
func (h *OSHandler) inBackground(fn func(ctx context.Context)) {
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), h.mediaTimeout)
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				h.logger.Error("background media work panicked", zap.Any("panic", p))
			}
		}()
		fn(ctx)
	}
	if h.spawn != nil {
		h.spawn(run)
		return
	}
	go run()
}

// errMediaBusy is a run whose video is already being made.
var errMediaBusy = errors.New("this plan's video is already being made")

// Action keys the console makes available once a turn's script can be made into a video.
var mediaActions = map[string]bool{"produce_media": true, "storyboard": true, "render": true}

// keepRun stores a media turn (one with a Creative Plan) so approving it and making its media
// read what the owner saw. An approved turn with a video script makes the console's media
// actions available. A failure is logged; the turn's answer is never held back.
func (h *OSHandler) keepRun(ctx context.Context, tenantID uuid.UUID, req askRequest, runID string, res *team.Result) {
	if h.runs == nil || res == nil || res.Plan == nil {
		return
	}
	run := &osrun.Run{RunID: runID, TenantID: tenantID.String(), TenantSlug: req.tenantSlug, PortalURL: req.portalURL,
		UserID: req.userID, Sender: req.sender, Channel: osrun.ChannelPortal,
		Text: req.Text, Tier: req.Tier, Platforms: req.Platforms, Plan: res.Plan, Steps: res.Steps, Shortlist: res.Shortlist,
		Approved: req.ApprovePlan && res.Route == team.RouteDone}
	if req.viaWhatsApp {
		run.Channel = osrun.ChannelWhatsApp
	}
	if res.Spec != nil {
		run.Playbook, run.Entities = res.Spec.Playbook, res.Spec.Entities
	}
	if err := h.runs.Save(context.WithoutCancel(ctx), run); err != nil {
		h.logger.Warn("media turn not kept", zap.String("run_id", runID), zap.Error(err))
		return
	}
	if run.Approved && h.studio != nil && run.HasVideoScript() {
		for i := range res.NextActions {
			if mediaActions[res.NextActions[i].Key] {
				res.NextActions[i].Available = true
			}
		}
	}
}

// madeMedia is what make-media made in Studio.
type madeMedia struct {
	RunID     string `json:"run_id"`
	BriefID   string `json:"brief_id"`
	VersionID string `json:"version_id"`
	Seconds   int    `json:"seconds"`
	Kind      string `json:"kind"`
	Step      int    `json:"step"`
	// StudioPath is the brief's page in the portal, after the tenant slug.
	StudioPath string `json:"studio_path"`
}

// errMediaOff is make-media without a Studio client.
var errMediaOff = errors.New("making videos from a plan is not configured")

// studio objectives and languages (marketflow-api creative.objectives and languages).
var (
	studioObjectives = map[string]bool{"sales": true, "leads": true, "awareness": true, "traffic": true}
	studioLanguages  = map[string]bool{"en": true, "sw": true}
)

// makeMedia makes an approved run's script into a Studio video ad and starts its storyboard.
// One call at a time per run (a Redis lock), and it is safe to call again: a run that already
// has its brief or version reuses them, so a second YES or a retry makes nothing new.
func (h *OSHandler) makeMedia(ctx context.Context, tenantID uuid.UUID, run *osrun.Run, step int) (*madeMedia, error) {
	if h.studio == nil || h.runs == nil {
		return nil, errMediaOff
	}
	release, ok, err := h.runs.Lock(ctx, "media:"+run.RunID, h.mediaTimeout)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errMediaBusy
	}
	defer release()
	// Read the run again under the lock: another call may have made the brief or version.
	if fresh, err := h.runs.Get(ctx, tenantID.String(), run.RunID); err == nil {
		run = fresh
	}
	script, picked, err := run.PickScript(step)
	if err != nil {
		return nil, err
	}
	seconds, kind := run.VideoSpec()
	out := &madeMedia{RunID: run.RunID, BriefID: run.BriefID, VersionID: run.VersionID, Seconds: seconds, Kind: kind, Step: picked}
	if out.VersionID != "" {
		out.StudioPath = "/studio/" + out.BriefID
		return out, nil
	}
	if out.BriefID == "" {
		id, err := h.studio.createBrief(ctx, tenantID, briefBody(run))
		if err != nil {
			return nil, err
		}
		out.BriefID, run.BriefID = id, id
		h.saveMade(ctx, run)
	}
	osrun.FitScript(script, kind)
	ad := map[string]any{"seconds": seconds, "kind": kind, "script": script}
	if lang := run.Entities.Language; studioLanguages[lang] {
		ad["language"] = lang
	}
	vid, err := h.studio.createVideoAd(ctx, tenantID, out.BriefID, ad)
	if err != nil {
		return nil, err
	}
	out.VersionID, run.VersionID = vid, vid
	h.saveMade(ctx, run)
	if err := h.studio.post(ctx, tenantID, "/api/v1/creative/versions/"+vid+"/storyboard", nil, nil); err != nil {
		return nil, err
	}
	out.StudioPath = "/studio/" + out.BriefID
	return out, nil
}

// saveMade records the brief and version on the run, so a retry does not make them twice.
func (h *OSHandler) saveMade(ctx context.Context, run *osrun.Run) {
	if err := h.runs.Update(context.WithoutCancel(ctx), run); err != nil {
		h.logger.Warn("made media not recorded on the run", zap.String("run_id", run.RunID), zap.Error(err))
	}
}

// briefBody is the Studio brief for a run: the offering and objective the optimiser read, and
// the plan with where it came from.
func briefBody(run *osrun.Run) map[string]any {
	e := run.Entities
	title := strings.TrimSpace(e.Offering)
	if title == "" {
		title = trimRunes(run.Text, 80)
	}
	title = trimRunes("Video ad: "+title, 160)
	body := map[string]any{"title": title, "metadata": map[string]any{
		"plan": run.Plan, "source": "os_turn", "run_id": run.RunID, "requested_by": run.UserID, "channel": run.Channel,
	}}
	if o := strings.TrimSpace(e.Offering); o != "" {
		body["offering"] = map[string]any{"name": trimRunes(o, 300)}
	}
	if obj := strings.ToLower(strings.TrimSpace(e.Objective)); studioObjectives[obj] {
		body["objective"] = obj
	}
	if studioLanguages[e.Language] {
		body["language"] = e.Language
	}
	if a := strings.TrimSpace(e.Audience); a != "" {
		body["audience"] = trimRunes(a, 500)
	}
	return body
}

type makeMediaRequest struct {
	RunID string `json:"run_id"`
	// Step is the owner's pick among the turn's scripts (its index in the steps); nil takes
	// the shortlisted or first one.
	Step *int `json:"step"`
	// TenantID is the internal route's tenant; the portal's comes from the token.
	TenantID string `json:"tenant_id"`
}

// MakeMedia handles POST /api/v1/os/make-media: the portal makes an approved turn's script
// into a Studio video ad and its storyboard. Studio's refusals (brand images missing, too
// many drafts) come back with Studio's status and code.
func (h *OSHandler) MakeMedia(w http.ResponseWriter, r *http.Request) {
	tenantID, status, msg := tenantFromAuth(r)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	h.serveMakeMedia(w, r, tenantID)
}

// InternalMakeMedia handles POST /api/v1/internal/os/make-media (service key): the same step
// for a tenant named in the body.
func (h *OSHandler) InternalMakeMedia(w http.ResponseWriter, r *http.Request) {
	h.serveMakeMedia(w, r, uuid.Nil)
}

func (h *OSHandler) serveMakeMedia(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	var req makeMediaRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.RunID == "" {
		writeError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if tenantID == uuid.Nil {
		id, err := uuid.Parse(req.TenantID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "tenant_id must be a UUID")
			return
		}
		tenantID = id
	}
	if h.studio == nil || h.runs == nil {
		writeError(w, http.StatusServiceUnavailable, errMediaOff.Error())
		return
	}
	run, err := h.runs.Get(r.Context(), tenantID.String(), req.RunID)
	if errors.Is(err, osrun.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "this plan has expired; ask the team again")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "the plan could not be read right now")
		return
	}
	if !run.Approved {
		writeError(w, http.StatusConflict, "approve the plan first")
		return
	}
	step := -1
	if req.Step != nil {
		step = *req.Step
	}
	made, err := h.makeMedia(r.Context(), tenantID, run, step)
	var se *studioError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, made)
	case errors.Is(err, osrun.ErrNoScript):
		writeError(w, http.StatusConflict, "this plan has no video script to make; ask the team for a video script")
	case errors.Is(err, errMediaBusy):
		writeError(w, http.StatusConflict, errMediaBusy.Error()+"; open the Studio in a minute")
	case errors.As(err, &se) && se.Status < 500:
		writeJSON(w, se.Status, map[string]any{"error": se.Message, "code": se.Code, "details": se.Details})
	default:
		h.logger.Warn("make media failed", zap.String("run_id", req.RunID), zap.Error(err))
		writeError(w, http.StatusBadGateway, "Studio could not start the video just now; try again shortly")
	}
}

// studioClient calls marketflow-api's Creative Studio for a tenant with the service key.
type studioClient struct {
	base string
	key  string
	http *http.Client
	// eventsKey is the internal service key for marketflow-api's /internal routes.
	eventsKey string
}

// mediaFailed tells marketflow-api that background work for a WhatsApp turn failed; it asks back
// for the message (whatsapp-notify, event media_failed) and sends text to the requester, the
// same path as the storyboard message.
func (c *studioClient) mediaFailed(ctx context.Context, tenantID uuid.UUID, runID, versionID, text string) error {
	b, _ := json.Marshal(map[string]string{"tenant_id": tenantID.String(), "run_id": runID, "version_id": versionID,
		"event": notifyMediaFailed, "error": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/internal/creative/os-turn/events", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.eventsKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("studio: media_failed event: status %d", resp.StatusCode)
	}
	return nil
}

func newStudioClient(base, key string) *studioClient {
	return &studioClient{base: strings.TrimSuffix(strings.TrimSpace(base), "/"), key: key, http: &http.Client{Timeout: 20 * time.Second}}
}

// studioError is a Studio refusal: its status, code, message and details as Studio sent them.
type studioError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *studioError) Error() string {
	return fmt.Sprintf("studio: %d %s: %s", e.Status, e.Code, e.Message)
}

// minImages is a refusal's details.min (the brand images needed), or def.
func (e *studioError) minImages(def int) int {
	if f, ok := e.Details["min"].(float64); ok && f > 0 {
		return int(f)
	}
	return def
}

func (c *studioClient) do(ctx context.Context, method, path string, tenantID uuid.UUID, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("X-Tenant-ID", tenantID.String())
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
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("studio %s: unreadable answer: %w", path, err)
		}
	}
	return nil
}

func (c *studioClient) post(ctx context.Context, tenantID uuid.UUID, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, tenantID, body, out)
}

func (c *studioClient) createBrief(ctx context.Context, tenantID uuid.UUID, body map[string]any) (string, error) {
	var b struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, tenantID, "/api/v1/creative/briefs", body, &b); err != nil {
		return "", err
	}
	if b.ID == "" {
		return "", errors.New("studio: the brief came back without an id")
	}
	return b.ID, nil
}

func (c *studioClient) createVideoAd(ctx context.Context, tenantID uuid.UUID, briefID string, body map[string]any) (string, error) {
	var v struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, tenantID, "/api/v1/creative/briefs/"+briefID+"/video-ads", body, &v); err != nil {
		return "", err
	}
	if v.ID == "" {
		return "", errors.New("studio: the video ad came back without an id")
	}
	return v.ID, nil
}

// studioQuote is the part of Studio's render quote the WhatsApp messages use.
type studioQuote struct {
	CreditsNeeded int  `json:"credits_needed"`
	Balance       int  `json:"balance"`
	Enough        bool `json:"enough"`
	Shortfall     int  `json:"shortfall"`
}

func (c *studioClient) quote(ctx context.Context, tenantID uuid.UUID, versionID string) (studioQuote, error) {
	var q studioQuote
	err := c.do(ctx, http.MethodGet, "/api/v1/creative/versions/"+versionID+"/quote", tenantID, nil, &q)
	return q, err
}

// approveAndRender approves the storyboard and queues the paid render.
func (c *studioClient) approveAndRender(ctx context.Context, tenantID uuid.UUID, versionID string) error {
	if err := c.post(ctx, tenantID, "/api/v1/creative/versions/"+versionID+"/storyboard/approve", nil, nil); err != nil {
		return err
	}
	return c.post(ctx, tenantID, "/api/v1/creative/versions/"+versionID+"/render", nil, nil)
}
