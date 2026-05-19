package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/bengobox/marketflow-ai/internal/rag"
	"github.com/bengobox/marketflow-ai/internal/sessions"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// UsageReporter reports usage events to subscriptions-api (fire-and-forget).
type UsageReporter interface {
	Report(tenantID, metric string, value float64)
}

// ChatHandler handles AI chat HTTP requests.
type ChatHandler struct {
	pipeline *rag.Pipeline
	sessions *sessions.Service
	usage    UsageReporter
	logger   *zap.Logger
}

// NewChatHandler creates a new chat handler.
func NewChatHandler(pipeline *rag.Pipeline, sess *sessions.Service, usage UsageReporter, logger *zap.Logger) *ChatHandler {
	return &ChatHandler{
		pipeline: pipeline,
		sessions: sess,
		usage:    usage,
		logger:   logger.Named("chat.handler"),
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

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}

	// Resolve or create the session; load history if existing.
	sessionID, history, err := h.resolveSession(r.Context(), tenantID, req.SessionID)
	if err != nil {
		h.logger.Warn("session resolve failed", zap.Error(err))
		sessionID = uuid.New()
	}

	// Detect if this is a public/widget request (no auth required).
	isPublic := strings.Contains(r.URL.Path, "/public/")

	input := rag.ChatInput{
		TenantID:     tenantID,
		TenantSlug:   r.Header.Get("X-Tenant-ID"), // original slug for tool lookups
		SessionID:    sessionID,
		Question:     req.Question,
		History:      history,
		BusinessType: req.BusinessType,
		PublicMode:   isPublic,
	}

	if req.Stream {
		h.handleStream(w, r, input)
		return
	}

	// Non-streaming response
	result, err := h.pipeline.Chat(r.Context(), input)
	if err != nil {
		h.logger.Error("chat failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "AI processing failed")
		return
	}

	// Persist user + assistant turns using the raw (pre-filter) response so
	// subsequent turns in the same session don't see FLP disclaimer boilerplate.
	savedAssistant := result.RawResponse
	if savedAssistant == "" {
		savedAssistant = result.Response
	}
	if h.sessions != nil {
		_ = h.sessions.AppendMessages(r.Context(), tenantID, sessionID,
			ai.Message{Role: "user", Content: req.Question},
			ai.Message{Role: "assistant", Content: savedAssistant},
		)
	}

	// Report AI chat usage to subscriptions-api (fire-and-forget).
	if h.usage != nil {
		h.usage.Report(tenantID.String(), "ai_chat_messages", 1)
	}

	w.Header().Set("Content-Type", "application/json")
	// Include session_id so the client can continue the conversation.
	respEnvelope := map[string]any{
		"session_id": sessionID.String(),
		"result":     result,
	}
	json.NewEncoder(w).Encode(respEnvelope)
}

// resolveSession loads an existing session's history or creates a new one.
func (h *ChatHandler) resolveSession(ctx context.Context, tenantID uuid.UUID, raw string) (uuid.UUID, []ai.Message, error) {
	if h.sessions == nil {
		return uuid.New(), nil, nil
	}
	if raw != "" {
		if parsed, err := uuid.Parse(raw); err == nil {
			history, err := h.sessions.LoadHistory(ctx, tenantID, parsed)
			if err == nil {
				return parsed, history, nil
			}
			// Session ID provided but not found — fall through and create one with that ID.
		}
	}
	sid, err := h.sessions.CreateSession(ctx, tenantID, "web_widget")
	if err != nil {
		return uuid.New(), nil, err
	}
	return sid, nil, nil
}

// handleStream sends Server-Sent Events for streaming chat responses.
func (h *ChatHandler) handleStream(w http.ResponseWriter, r *http.Request, input rag.ChatInput) {
	flusher, ok := w.(http.Flusher)
	if !ok {
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
		fmt.Fprintf(w, "data: {\"error\": %q}\n\n", err.Error())
		flusher.Flush()
		return
	}

	// Send metadata first
	if meta != nil {
		metaJSON, _ := json.Marshal(map[string]any{
			"intent":     meta.Intent,
			"confidence": meta.Confidence,
			"sources":    meta.Sources,
			"model":      meta.Model,
		})
		fmt.Fprintf(w, "data: {\"type\":\"meta\",\"data\":%s}\n\n", metaJSON)
		flusher.Flush()
	}

	for delta := range ch {
		if delta.Done {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		if delta.Content != "" {
			escaped, _ := json.Marshal(delta.Content)
			fmt.Fprintf(w, "data: {\"type\":\"delta\",\"content\":%s}\n\n", escaped)
			flusher.Flush()
		}
	}
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

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
