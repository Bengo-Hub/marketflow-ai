package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bengobox/marketflow-ai/internal/ai"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Service manages chat session persistence.
// Uses the shared marketflow database (same as marketflow-api) via raw SQL
// since marketflow-ai doesn't have its own Ent client for chat_sessions.
type Service struct {
	db     *sql.DB
	logger *zap.Logger

	// tenantCache memoises slug/uuid → real tenant UUID lookups so chat_sessions
	// rows satisfy the FK to tenants. Keyed by the raw X-Tenant-ID header value.
	tenantCache sync.Map // map[string]uuid.UUID
}

// NewService creates a new session service.
func NewService(db *sql.DB, logger *zap.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger.Named("sessions"),
	}
}

// ResolveTenantID maps a raw X-Tenant-ID header value (either a tenant slug or a
// UUID) to the REAL tenant UUID stored in the shared tenants table. The chat
// handler used to synthesise a deterministic SHA1 UUID from the slug, but that
// value never matched a tenants row — so every chat_sessions insert failed its
// foreign key (SQLSTATE 23503) and conversation history silently stopped
// persisting. Resolving against the actual table keeps the FK satisfied.
//
// Returns (uuid, true) when a real tenant exists; (Nil, false) for unknown
// tenants (e.g. arbitrary public-widget embeds) so the caller can fall back to
// an ephemeral, non-persisted session instead of triggering an FK error.
func (s *Service) ResolveTenantID(ctx context.Context, raw string) (uuid.UUID, bool) {
	if s == nil || s.db == nil || raw == "" {
		return uuid.Nil, false
	}
	if cached, ok := s.tenantCache.Load(raw); ok {
		return cached.(uuid.UUID), true
	}

	var id uuid.UUID
	var err error
	if parsed, perr := uuid.Parse(raw); perr == nil {
		// Looks like a UUID — confirm it actually exists before trusting it.
		err = s.db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE id = $1`, parsed).Scan(&id)
	} else {
		// Treat as a slug.
		err = s.db.QueryRowContext(ctx, `SELECT id FROM tenants WHERE slug = $1`, raw).Scan(&id)
	}
	if err != nil {
		if err != sql.ErrNoRows {
			s.logger.Warn("resolve tenant failed", zap.String("raw", raw), zap.Error(err))
		}
		return uuid.Nil, false
	}

	s.tenantCache.Store(raw, id)
	return id, true
}

// CreateSession creates a new chat session.
func (s *Service) CreateSession(ctx context.Context, tenantID uuid.UUID, channel string) (uuid.UUID, error) {
	sessionID := uuid.New()
	now := time.Now()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_sessions (id, tenant_id, status, channel, messages, message_count, ai_responses, escalations, started_at)
		 VALUES ($1, $2, 'active', $3, '[]'::jsonb, 0, 0, 0, $4)`,
		sessionID, tenantID, channel, now)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create session: %w", err)
	}

	return sessionID, nil
}

// LoadHistory loads message history from a chat session.
func (s *Service) LoadHistory(ctx context.Context, tenantID, sessionID uuid.UUID) ([]ai.Message, error) {
	var messagesJSON []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT messages FROM chat_sessions WHERE id = $1 AND tenant_id = $2`,
		sessionID, tenantID).Scan(&messagesJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("load history: %w", err)
	}

	var rawMessages []map[string]any
	if err := json.Unmarshal(messagesJSON, &rawMessages); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}

	messages := make([]ai.Message, 0, len(rawMessages))
	for _, m := range rawMessages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role != "" && content != "" {
			messages = append(messages, ai.Message{Role: role, Content: content})
		}
	}

	return messages, nil
}

// AppendMessages adds user question and AI response to the session history.
func (s *Service) AppendMessages(ctx context.Context, tenantID, sessionID uuid.UUID, userMsg, assistantMsg ai.Message) error {
	now := time.Now()

	newMessages := []map[string]any{
		{"role": userMsg.Role, "content": userMsg.Content, "timestamp": now.Format(time.RFC3339)},
		{"role": assistantMsg.Role, "content": assistantMsg.Content, "timestamp": now.Format(time.RFC3339)},
	}
	newJSON, err := json.Marshal(newMessages)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE chat_sessions
		 SET messages = messages || $1::jsonb,
		     message_count = message_count + 2,
		     ai_responses = ai_responses + 1
		 WHERE id = $2 AND tenant_id = $3`,
		string(newJSON), sessionID, tenantID)
	if err != nil {
		return fmt.Errorf("append messages: %w", err)
	}

	return nil
}

// CloseSession marks a session as closed.
func (s *Service) CloseSession(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_sessions SET status = 'closed', ended_at = $1 WHERE id = $2 AND tenant_id = $3`,
		now, sessionID, tenantID)
	return err
}
