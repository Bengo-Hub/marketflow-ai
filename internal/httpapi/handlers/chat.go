package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ChatHandler handles AI chat HTTP requests.
type ChatHandler struct {
	pipeline *rag.Pipeline
	sessions *sessions.Service
	credits  CreditSpender
	logger   *zap.Logger
}

// NewChatHandler creates a new chat handler. credits may be nil (no billing configured, as in local
// development); every message then runs unmetered.
func NewChatHandler(pipeline *rag.Pipeline, sess *sessions.Service, credits CreditSpender, logger *zap.Logger) *ChatHandler {
	return &ChatHandler{
		pipeline: pipeline,
		sessions: sess,
		credits:  credits,
		logger:   logger.Named("chat.handler"),
	}
}

// chargeMessage spends one AI credit for the tenant before a message runs. It returns the charge
// key for a later refund (empty when nothing was charged) and false after writing the refusal.
// Only a tenant that resolved to a real tenant row is billed: a slug the platform could not
// resolve has nobody to charge, and the shared platform tenant answers as exempt.
func (h *ChatHandler) chargeMessage(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, billable bool) (string, bool) {
	if h.credits == nil || !billable {
		return "", true
	}
	key := "chat:" + uuid.NewString()
	dec, err := h.credits.Spend(r.Context(), tenantID, 1, key)
	if err != nil {
		h.logger.Warn("ai credit check failed (fail-closed)", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "AI credits could not be checked right now, try again shortly")
		return "", false
	}
	if !dec.Allowed {
		h.logger.Info("ai message refused", zap.String("tenant_id", tenantID.String()), zap.String("reason", dec.Reason))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]any{
			"error":   dec.Reason,
			"message": "AI credits are used up. Buy a bundle from your billing page to keep chatting.",
		})
		return "", false
	}
	if dec.Exempt {
		return "", true
	}
	return key, true
}

// refund returns the credit when a message produced no answer.
func (h *ChatHandler) refund(r *http.Request, tenantID uuid.UUID, key string) {
	if key == "" || h.credits == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := h.credits.Refund(ctx, tenantID, key); err != nil {
		h.logger.Error("ai credit refund did not go through", zap.String("tenant_id", tenantID.String()), zap.String("key", key), zap.Error(err))
	}
}

type chatRequest struct {
	Question     string `json:"question"`
	SessionID    string `json:"session_id"`
	Stream       bool   `json:"stream"`
	BusinessType string `json:"business_type"` // "flp" | "generic" | "" (pipeline auto-detects)
}

// Chat handles POST /api/v1/chat
// @Summary Send a chat message to the AI
// @Description Processes user question through RAG pipeline: classify intent → embed → search KB → generate response → compliance filter
// @Tags Chat
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body chatRequest true "Chat message"
// @Success 200 {object} rag.ChatResult
// @Security bearerAuth
// @Router /chat [post]
func (h *ChatHandler) Chat(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid X-Tenant-ID header")
		return
	}

	// Resolve the raw header to the REAL tenant UUID so chat_sessions rows
	// satisfy their FK to tenants. extractTenantID returns a synthetic SHA1
	// UUID for slug callers (fine for KB scoping / Redis keys, but it never
	// matches a tenants row). Only persist sessions when the tenant is real;
	// otherwise use an ephemeral session to avoid FK violations (23503).
	rawTenant := r.Header.Get("X-Tenant-ID")
	persist := false
	if h.sessions != nil {
		if realID, ok := h.sessions.ResolveTenantID(r.Context(), rawTenant); ok {
			tenantID = realID
			persist = true
		}
	}

	// A signed-in user may only chat as their own tenant (the header used to be
	// trusted on the authenticated route too). The public widget has no token and
	// is unaffected; it only reaches the public tool allowlist.
	if !strings.Contains(r.URL.Path, "/public/") && !headerTenantAllowed(r, tenantID) {
		writeError(w, http.StatusForbidden, "X-Tenant-ID does not match your account")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}
	if len([]rune(req.Question)) > 1000 {
		writeError(w, http.StatusBadRequest, "message too long (max 1000 characters)")
		return
	}

	// Resolve or create the session; load history if existing.
	sessionID, history, err := h.resolveSession(r.Context(), tenantID, req.SessionID, persist)
	if err != nil {
		h.logger.Warn("session resolve failed", zap.Error(err))
		sessionID = uuid.New()
	}

	// Detect if this is a public/widget request (no auth required).
	isPublic := strings.Contains(r.URL.Path, "/public/")

	// X-Widget-Persona is a fallback when the widget does not explicitly send
	// business_type in the request body (e.g. older embed codes or non-MarketFlow widgets).
	businessType := req.BusinessType
	if businessType == "" {
		if persona := r.Header.Get("X-Widget-Persona"); persona != "" {
			businessType = personaToBusinessType(persona)
		}
	}

	input := rag.ChatInput{
		TenantID:     tenantID,
		TenantSlug:   r.Header.Get("X-Tenant-ID"), // original slug for tool lookups
		SessionID:    sessionID,
		Question:     req.Question,
		History:      history,
		BusinessType: businessType,
		PublicMode:   isPublic,
	}

	chargeKey, ok := h.chargeMessage(w, r, tenantID, persist)
	if !ok {
		return
	}

	if req.Stream {
		h.handleStream(w, r, input, persist, tenantID, chargeKey)
		return
	}

	// Non-streaming response
	result, err := h.pipeline.Chat(r.Context(), input)
	if err != nil {
		h.logger.Error("chat failed", zap.Error(err))
		h.refund(r, tenantID, chargeKey)
		writeError(w, http.StatusInternalServerError, "AI processing failed")
		return
	}
	if rag.FreeAnswer(result) {
		h.refund(r, tenantID, chargeKey)
	}

	// Persist user + assistant turns using the raw (pre-filter) response so
	// subsequent turns in the same session don't see FLP disclaimer boilerplate.
	savedAssistant := result.RawResponse
	if savedAssistant == "" {
		savedAssistant = result.Response
	}
	if persist && h.sessions != nil {
		_ = h.sessions.AppendMessages(r.Context(), tenantID, sessionID,
			ai.Message{Role: "user", Content: req.Question},
			ai.Message{Role: "assistant", Content: savedAssistant},
		)
	}

	w.Header().Set("Content-Type", "application/json")
	// Include session_id so the client can continue the conversation.
	respEnvelope := map[string]any{
		"session_id": sessionID.String(),
		"result":     result,
	}
	json.NewEncoder(w).Encode(respEnvelope)
}

// sessionTarget resolves the tenant and session a delete applies to, the same
// way Chat does: the real tenant from X-Tenant-ID (slug or UUID), and on the
// signed-in route only the caller's own tenant. ok=false means the response is
// written. persisted=false means nothing was ever stored (an unknown tenant's
// ephemeral session), so there is nothing to delete.
func (h *ChatHandler) sessionTarget(w http.ResponseWriter, r *http.Request) (tenantID, sessionID uuid.UUID, persisted, ok bool) {
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "session id must be a UUID")
		return
	}
	if h.sessions == nil {
		return uuid.Nil, sessionID, false, true
	}
	tenantID, persisted = h.sessions.ResolveTenantID(r.Context(), r.Header.Get("X-Tenant-ID"))
	if !persisted {
		return uuid.Nil, sessionID, false, true
	}
	if !strings.Contains(r.URL.Path, "/public/") && !headerTenantAllowed(r, tenantID) {
		writeError(w, http.StatusForbidden, "X-Tenant-ID does not match your account")
		return uuid.Nil, uuid.Nil, false, false
	}
	return tenantID, sessionID, true, true
}

// ClearSession handles DELETE /api/v1/chat/sessions/{id} and its public twin:
// "Clear chat" erases the stored conversation. 204 also for an unknown session.
// On the public route the session id is the credential, as it is for history.
// @Summary Clear a chat conversation
// @Tags Chat
// @Param X-Tenant-ID header string true "Tenant slug or UUID"
// @Param id path string true "Session UUID"
// @Success 204
// @Router /chat/sessions/{id} [delete]
func (h *ChatHandler) ClearSession(w http.ResponseWriter, r *http.Request) {
	tenantID, sessionID, persisted, ok := h.sessionTarget(w, r)
	if !ok {
		return
	}
	if persisted {
		if err := h.sessions.ClearSession(r.Context(), tenantID, sessionID); err != nil {
			h.logger.Error("clear chat failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "the conversation could not be cleared, try again")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteTurn handles DELETE /api/v1/chat/sessions/{id}/turns/{n} and its public
// twin: removes the n-th question and answer (0-based). 404 when there is no
// such turn.
// @Summary Delete one question and answer from a conversation
// @Tags Chat
// @Param X-Tenant-ID header string true "Tenant slug or UUID"
// @Param id path string true "Session UUID"
// @Param n path int true "Turn index, 0-based"
// @Success 204
// @Failure 404 {object} map[string]string
// @Router /chat/sessions/{id}/turns/{n} [delete]
func (h *ChatHandler) DeleteTurn(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "turn must be a number from 0")
		return
	}
	tenantID, sessionID, persisted, ok := h.sessionTarget(w, r)
	if !ok {
		return
	}
	if !persisted {
		writeError(w, http.StatusNotFound, "no such turn")
		return
	}
	if err := h.sessions.DeleteTurn(r.Context(), tenantID, sessionID, n); err != nil {
		if errors.Is(err, sessions.ErrNoTurn) {
			writeError(w, http.StatusNotFound, "no such turn")
			return
		}
		h.logger.Error("delete turn failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "the message could not be deleted, try again")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveSession loads an existing session's history or creates a new one.
// When persist is false (unknown/public tenant with no tenants row) it returns
// an ephemeral in-memory session id and skips all DB writes, so we never trip
// the chat_sessions → tenants foreign key.
func (h *ChatHandler) resolveSession(ctx context.Context, tenantID uuid.UUID, raw string, persist bool) (uuid.UUID, []ai.Message, error) {
	if h.sessions == nil || !persist {
		return uuid.New(), nil, nil
	}
	if raw != "" {
		if parsed, err := uuid.Parse(raw); err == nil && parsed != uuid.Nil {
			history, err := h.sessions.LoadHistory(ctx, tenantID, parsed)
			if err == nil {
				return parsed, history, nil
			}
			// Not found for this tenant: start the session under the client's id, unless
			// another tenant already owns it, then fall through to a fresh one.
			if created, cerr := h.sessions.CreateSessionWithID(ctx, tenantID, parsed, "web_widget"); cerr == nil && created {
				return parsed, nil, nil
			}
		}
	}
	sid, err := h.sessions.CreateSession(ctx, tenantID, "web_widget")
	if err != nil {
		return uuid.New(), nil, err
	}
	return sid, nil, nil
}

// handleStream sends Server-Sent Events for streaming chat responses.
// When persist is true the assembled answer is written back to the session
// history once the stream completes, so the dashboard chat (which always
// streams) keeps conversation memory across turns.
func (h *ChatHandler) handleStream(w http.ResponseWriter, r *http.Request, input rag.ChatInput, persist bool, tenantID uuid.UUID, chargeKey string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.refund(r, tenantID, chargeKey)
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, meta, err := h.pipeline.ChatStream(r.Context(), input)
	if err != nil {
		h.refund(r, tenantID, chargeKey)
		fmt.Fprintf(w, "data: {\"error\": %q}\n\n", err.Error())
		flusher.Flush()
		return
	}

	// An answer no model produced (cache, template, knowledge base) costs no credit.
	refunded := rag.FreeAnswer(meta)
	if refunded {
		h.refund(r, tenantID, chargeKey)
	}

	// Send metadata first
	if meta != nil {
		metaJSON, _ := json.Marshal(map[string]any{
			// The client sends it back as session_id to keep the conversation.
			"session_id": input.SessionID.String(),
			"intent":     meta.Intent,
			"confidence": meta.Confidence,
			"sources":    meta.Sources,
			"model":      meta.Model,
		})
		fmt.Fprintf(w, "data: {\"type\":\"meta\",\"data\":%s}\n\n", metaJSON)
		flusher.Flush()
	}

	var assembled strings.Builder
	for delta := range ch {
		if delta.Done {
			// A model that streamed nothing was replaced by the knowledge base.
			if !refunded && rag.FreeAnswer(meta) {
				h.refund(r, tenantID, chargeKey)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			h.persistStreamTurn(r.Context(), input, assembled.String(), persist)
			return
		}
		if delta.Content != "" {
			escaped, _ := json.Marshal(delta.Content)
			evtType := "delta"
			if delta.Type == "replace" {
				evtType = "replace"
				assembled.Reset()
			}
			assembled.WriteString(delta.Content)
			fmt.Fprintf(w, "data: {\"type\":%q,\"content\":%s}\n\n", evtType, escaped)
			flusher.Flush()
		}
	}
	// Channel closed without an explicit Done sentinel — still persist what we have.
	h.persistStreamTurn(r.Context(), input, assembled.String(), persist)
}

// persistStreamTurn appends the user question and the assembled streamed answer
// to the session history. No-op for ephemeral (unknown-tenant) sessions.
func (h *ChatHandler) persistStreamTurn(ctx context.Context, input rag.ChatInput, answer string, persist bool) {
	if !persist || h.sessions == nil || answer == "" {
		return
	}
	_ = h.sessions.AppendMessages(ctx, input.TenantID, input.SessionID,
		ai.Message{Role: "user", Content: input.Question},
		ai.Message{Role: "assistant", Content: answer},
	)
}

// Health responds with service status.
func Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"service": "marketflow-ai",
	})
}

func extractTenantID(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("X-Tenant-ID")
	if header == "" {
		return uuid.UUID{}, fmt.Errorf("X-Tenant-ID header is required")
	}
	// Accept UUID directly.
	if id, err := uuid.Parse(header); err == nil {
		return id, nil
	}
	// Accept tenant slug — resolve to UUID via a deterministic namespace UUID.
	// This produces a stable UUID from the slug so the same slug always maps
	// to the same "tenant ID" for caching / session purposes. The pipeline
	// uses this UUID for Redis keys and pgvector tenant scoping.
	//
	// NOTE: this is a BEST-EFFORT mapping. It works because the tenant slug
	// is unique (enforced by the Tenant.slug unique index). The real UUID
	// lives in the Tenant table but we avoid an extra API call here for speed.
	// The KB search uses OR(tenant_id=X, scope='platform') so the platform
	// rows (which power most public chat) are always included regardless.
	slugUUID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("marketflow:tenant:"+header))
	return slugUUID, nil
}

// personaToBusinessType maps the X-Widget-Persona header value to a businessType
// recognised by the RAG pipeline. Used when the widget embed does not send business_type.
func personaToBusinessType(persona string) string {
	p := strings.ToLower(persona)
	switch {
	case strings.Contains(p, "cafe") || strings.Contains(p, "loft") || strings.Contains(p, "restaurant") || strings.Contains(p, "bistro"):
		return "cafe"
	case strings.Contains(p, "flp") || strings.Contains(p, "forever"):
		return "flp"
	case strings.Contains(p, "codevertex"):
		return "codevertex"
	default:
		return "generic"
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
