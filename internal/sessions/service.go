package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
}

// NewService creates a new session service.
func NewService(db *sql.DB, logger *zap.Logger) *Service {
	return &Service{
		db:     db,
		logger: logger.Named("sessions"),
	}
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
