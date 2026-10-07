// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Postgres twin of the scheduled chat message store in
// webchat_scheduled_store.go.

const pgScheduledMessageDDL = `
CREATE TABLE IF NOT EXISTS webchat_scheduled_message (
    id               TEXT PRIMARY KEY,
    sender_user_id   TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    project_id       TEXT,
    content          TEXT NOT NULL,
    reply_to_id      TEXT,
    idempotency_key  TEXT NOT NULL,
    fire_at          TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL,
    failure_reason   TEXT,
    message_id       TEXT,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    claimed_at       TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_webchat_scheduled_message_status_fire
    ON webchat_scheduled_message (status, fire_at);

CREATE INDEX IF NOT EXISTS idx_webchat_scheduled_message_sender_conversation
    ON webchat_scheduled_message (sender_user_id, conversation_key);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_scheduled_message_idempotency
    ON webchat_scheduled_message (sender_user_id, idempotency_key);
`

// initScheduledMessages creates webchat_scheduled_message (idempotent) and
// records it in webchat_migrations. It runs on every replica's Init, not
// under the migration advisory lock: the DDL is IF NOT EXISTS, so a replica
// that skips the locked migrations still has the table.
func (s *pgWebChatStore) initScheduledMessages() error {
	if _, err := s.db.Exec(pgScheduledMessageDDL); err != nil {
		return fmt.Errorf("webchat store: create scheduled message table: %w", err)
	}
	if err := s.markMigrationCompleted(scheduledMessageTableMigration); err != nil {
		return fmt.Errorf("webchat store: record scheduled message migration: %w", err)
	}
	return nil
}

const pgScheduledColumns = `id, sender_user_id, conversation_key, COALESCE(project_id, ''), content,
       COALESCE(reply_to_id, ''), idempotency_key, fire_at, status, COALESCE(failure_reason, ''),
       COALESCE(message_id, ''), created_at, updated_at, claimed_at`

func scanPGScheduled(row interface{ Scan(...any) error }) (*ScheduledChatMessage, error) {
	var m ScheduledChatMessage
	var claimedAt sql.NullTime
	if err := row.Scan(&m.ID, &m.SenderUserID, &m.ConversationKey, &m.ProjectID, &m.Content,
		&m.ReplyToID, &m.IdempotencyKey, &m.FireAt, &m.Status, &m.FailureReason,
		&m.MessageID, &m.CreatedAt, &m.UpdatedAt, &claimedAt); err != nil {
		return nil, err
	}
	m.FireAt = m.FireAt.UTC()
	m.CreatedAt = m.CreatedAt.UTC()
	m.UpdatedAt = m.UpdatedAt.UTC()
	if claimedAt.Valid {
		t := claimedAt.Time.UTC()
		m.ClaimedAt = &t
	}
	return &m, nil
}

func collectPGScheduled(rows *sql.Rows) ([]ScheduledChatMessage, error) {
	defer func() { _ = rows.Close() }()
	out := []ScheduledChatMessage{}
	for rows.Next() {
		m, err := scanPGScheduled(rows)
		if err != nil {
			return nil, fmt.Errorf("webchat store: scan scheduled message: %w", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webchat store: scan scheduled messages: %w", err)
	}
	return out, nil
}

func (s *pgWebChatStore) CreateScheduledMessage(ctx context.Context, m *ScheduledChatMessage) (*ScheduledChatMessage, bool, error) {
	const query = `
INSERT INTO webchat_scheduled_message
    (id, sender_user_id, conversation_key, project_id, content, reply_to_id, idempotency_key,
     fire_at, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (sender_user_id, idempotency_key) DO NOTHING
`
	res, err := s.db.ExecContext(ctx, query, m.ID, m.SenderUserID, m.ConversationKey, nullableString(m.ProjectID),
		m.Content, nullableString(m.ReplyToID), m.IdempotencyKey, m.FireAt.UTC(), m.Status,
		m.CreatedAt.UTC(), m.UpdatedAt.UTC())
	if err != nil {
		return nil, false, fmt.Errorf("webchat store: create scheduled message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("webchat store: create scheduled message: %w", err)
	}
	existed := n == 0
	row, err := scanPGScheduled(s.db.QueryRowContext(ctx,
		`SELECT `+pgScheduledColumns+` FROM webchat_scheduled_message WHERE sender_user_id = $1 AND idempotency_key = $2`,
		m.SenderUserID, m.IdempotencyKey))
	if err != nil {
		return nil, false, fmt.Errorf("webchat store: read scheduled message: %w", err)
	}
	return row, existed, nil
}

func (s *pgWebChatStore) GetScheduledMessage(ctx context.Context, senderUserID, id string) (*ScheduledChatMessage, error) {
	row, err := scanPGScheduled(s.db.QueryRowContext(ctx,
		`SELECT `+pgScheduledColumns+` FROM webchat_scheduled_message WHERE id = $1 AND sender_user_id = $2`,
		id, senderUserID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("webchat store: get scheduled message: %w", err)
	}
	return row, nil
}

func (s *pgWebChatStore) ListScheduledMessages(ctx context.Context, senderUserID, conversationKey string) ([]ScheduledChatMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pgScheduledColumns+` FROM webchat_scheduled_message
		  WHERE sender_user_id = $1 AND conversation_key = $2 AND status IN ($3, $4, $5)
		  ORDER BY fire_at, created_at, id`,
		senderUserID, conversationKey, ScheduledMessagePending, ScheduledMessageSending, ScheduledMessageFailed)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list scheduled messages: %w", err)
	}
	return collectPGScheduled(rows)
}

func (s *pgWebChatStore) CancelScheduledMessage(ctx context.Context, senderUserID, id string, now time.Time) (bool, error) {
	return execOneRow(s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = $1, updated_at = $2
		  WHERE id = $3 AND sender_user_id = $4 AND status = $5`,
		ScheduledMessageCancelled, now.UTC(), id, senderUserID, ScheduledMessagePending))
}

func (s *pgWebChatStore) ListDueScheduledMessages(ctx context.Context, now time.Time, limit int) ([]ScheduledChatMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pgScheduledColumns+` FROM webchat_scheduled_message
		  WHERE status = $1 AND fire_at <= $2
		  ORDER BY fire_at, id LIMIT $3`,
		ScheduledMessagePending, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list due scheduled messages: %w", err)
	}
	return collectPGScheduled(rows)
}

func (s *pgWebChatStore) ClaimScheduledMessage(ctx context.Context, id string, now time.Time) (bool, error) {
	return execOneRow(s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = $1, claimed_at = $2, updated_at = $2
		  WHERE id = $3 AND status = $4`,
		ScheduledMessageSending, now.UTC(), id, ScheduledMessagePending))
}

func (s *pgWebChatStore) ReleaseScheduledMessage(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = $1, claimed_at = NULL, updated_at = $2
		  WHERE id = $3 AND status = $4`,
		ScheduledMessagePending, now.UTC(), id, ScheduledMessageSending)
	if err != nil {
		return fmt.Errorf("webchat store: release scheduled message: %w", err)
	}
	return nil
}

func (s *pgWebChatStore) MarkScheduledMessageSent(ctx context.Context, id, messageID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = $1, message_id = $2, updated_at = $3
		  WHERE id = $4 AND status = $5`,
		ScheduledMessageSent, messageID, now.UTC(), id, ScheduledMessageSending)
	if err != nil {
		return fmt.Errorf("webchat store: mark scheduled message sent: %w", err)
	}
	return nil
}

func (s *pgWebChatStore) MarkScheduledMessageFailed(ctx context.Context, id, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = $1, failure_reason = $2, updated_at = $3
		  WHERE id = $4 AND status = $5`,
		ScheduledMessageFailed, reason, now.UTC(), id, ScheduledMessageSending)
	if err != nil {
		return fmt.Errorf("webchat store: mark scheduled message failed: %w", err)
	}
	return nil
}
