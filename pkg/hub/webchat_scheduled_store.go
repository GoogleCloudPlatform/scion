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

// Scheduled chat messages (ptone/scion#3666): a message a user wrote in web
// chat and asked the hub to send later. The pending text lives only in
// webchat_scheduled_message, never in messages, so history, search, unread
// state and agents cannot see it before it is sent. At fire time the hub
// sends it through sendChatMessage as an ordinary message from the user.

// Status values of a scheduled chat message.
const (
	ScheduledMessagePending   = "pending"
	ScheduledMessageSending   = "sending"
	ScheduledMessageSent      = "sent"
	ScheduledMessageCancelled = "cancelled"
	ScheduledMessageFailed    = "failed"
)

// Failure reasons recorded on a failed scheduled chat message.
const (
	ScheduledFailureNoAccess         = "no_access"
	ScheduledFailureConversationGone = "conversation_gone"
	ScheduledFailureSenderInactive   = "sender_inactive"
	ScheduledFailureRecipientGone    = "recipient_gone"
	ScheduledFailureMissed           = "missed"
	ScheduledFailureInterrupted      = "interrupted"
	ScheduledFailureDeliveryError    = "delivery_error"
)

// ScheduledChatMessage is one row of webchat_scheduled_message.
type ScheduledChatMessage struct {
	ID              string
	SenderUserID    string
	ConversationKey string
	// ProjectID is the topic's project when the message was scheduled. It
	// is kept for cleanup and filtering only and is never used to decide
	// access: the fire path resolves the project from the topic again.
	ProjectID      string
	Content        string
	ReplyToID      string
	IdempotencyKey string
	FireAt         time.Time
	Status         string
	FailureReason  string
	MessageID      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClaimedAt      *time.Time
}

// ScheduledMessageStore persists scheduled chat messages. Both webchat
// store dialects implement it; obtain it with scheduledMessageStoreFrom.
//
// Every per-message read and mutation that a user can reach takes the
// sender's user ID and matches it, so one user can never see or change
// another user's row. The sweeper methods (ListDue, Claim, Release,
// MarkSent, MarkFailed) are hub-internal.
type ScheduledMessageStore interface {
	// CreateScheduledMessage inserts m, which must be pending. If the
	// sender already has a row with m.IdempotencyKey, that row is returned
	// unchanged with existed=true.
	CreateScheduledMessage(ctx context.Context, m *ScheduledChatMessage) (row *ScheduledChatMessage, existed bool, err error)
	// GetScheduledMessage returns the sender's row with the given ID, or
	// nil when there is none.
	GetScheduledMessage(ctx context.Context, senderUserID, id string) (*ScheduledChatMessage, error)
	// ListScheduledMessages returns the sender's pending, sending and
	// failed rows in the conversation, ordered by fire time.
	ListScheduledMessages(ctx context.Context, senderUserID, conversationKey string) ([]ScheduledChatMessage, error)
	// CancelScheduledMessage moves the sender's row from pending to
	// cancelled. It reports false when the row is not pending (or not
	// the sender's).
	CancelScheduledMessage(ctx context.Context, senderUserID, id string, now time.Time) (bool, error)

	// ListDueScheduledMessages returns up to limit pending rows whose
	// fire time is at or before now, oldest first.
	ListDueScheduledMessages(ctx context.Context, now time.Time, limit int) ([]ScheduledChatMessage, error)
	// ClaimScheduledMessage moves a row from pending to sending. Exactly
	// one concurrent caller gets true; only that caller may deliver it.
	ClaimScheduledMessage(ctx context.Context, id string, now time.Time) (bool, error)
	// ReleaseScheduledMessage moves a claimed row back from sending to
	// pending. Only valid before delivery has started.
	ReleaseScheduledMessage(ctx context.Context, id string, now time.Time) error
	// MarkScheduledMessageSent moves a claimed row to sent, recording the
	// ID of the delivered message.
	MarkScheduledMessageSent(ctx context.Context, id, messageID string, now time.Time) error
	// MarkScheduledMessageFailed moves a claimed row to failed with reason.
	MarkScheduledMessageFailed(ctx context.Context, id, reason string, now time.Time) error
}

// scheduledMessageStoreFrom returns the scheduled-message store behind a
// webchat store, or nil when it has none (test doubles).
func scheduledMessageStoreFrom(wcs WebChatStore) ScheduledMessageStore {
	if sms, ok := wcs.(ScheduledMessageStore); ok {
		return sms
	}
	return nil
}

// scheduledMessageTableMigration names the webchat_migrations row that
// records the creation of webchat_scheduled_message.
const scheduledMessageTableMigration = "scheduled_message_table"

// ---------------------------------------------------------------------------
// SQLite implementation
// ---------------------------------------------------------------------------

// sqliteScheduledTimeLayout is a fixed-width UTC layout, so that fire times
// stored as TEXT compare correctly as strings (RFC3339Nano trims trailing
// zeros and would not).
const sqliteScheduledTimeLayout = "2006-01-02T15:04:05.000000000Z"

func sqliteScheduledTime(t time.Time) string {
	return t.UTC().Format(sqliteScheduledTimeLayout)
}

const sqliteScheduledMessageDDL = `
CREATE TABLE IF NOT EXISTS webchat_scheduled_message (
    id               TEXT PRIMARY KEY,
    sender_user_id   TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    project_id       TEXT,
    content          TEXT NOT NULL,
    reply_to_id      TEXT,
    idempotency_key  TEXT NOT NULL,
    fire_at          TEXT NOT NULL,
    status           TEXT NOT NULL,
    failure_reason   TEXT,
    message_id       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    claimed_at       TEXT
);

CREATE INDEX IF NOT EXISTS idx_webchat_scheduled_message_status_fire
    ON webchat_scheduled_message (status, fire_at);

CREATE INDEX IF NOT EXISTS idx_webchat_scheduled_message_sender_conversation
    ON webchat_scheduled_message (sender_user_id, conversation_key);

CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_scheduled_message_idempotency
    ON webchat_scheduled_message (sender_user_id, idempotency_key);
`

// initScheduledMessages creates webchat_scheduled_message (idempotent) and
// records it in webchat_migrations.
func (s *sqliteWebChatStore) initScheduledMessages() error {
	if _, err := s.db.Exec(sqliteScheduledMessageDDL); err != nil {
		return fmt.Errorf("webchat store: create scheduled message table: %w", err)
	}
	done, err := s.migrationCompleted(scheduledMessageTableMigration)
	if err != nil {
		return fmt.Errorf("webchat store: scheduled message migration: %w", err)
	}
	if done {
		return nil
	}
	const query = `INSERT INTO webchat_migrations (name, completed_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`
	if _, err := s.db.Exec(query, scheduledMessageTableMigration, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("webchat store: record scheduled message migration: %w", err)
	}
	return nil
}

const sqliteScheduledColumns = `id, sender_user_id, conversation_key, COALESCE(project_id, ''), content,
       COALESCE(reply_to_id, ''), idempotency_key, fire_at, status, COALESCE(failure_reason, ''),
       COALESCE(message_id, ''), created_at, updated_at, claimed_at`

func scanSQLiteScheduled(row interface{ Scan(...any) error }) (*ScheduledChatMessage, error) {
	var m ScheduledChatMessage
	var fireAt, createdAt, updatedAt string
	var claimedAt sql.NullString
	if err := row.Scan(&m.ID, &m.SenderUserID, &m.ConversationKey, &m.ProjectID, &m.Content,
		&m.ReplyToID, &m.IdempotencyKey, &fireAt, &m.Status, &m.FailureReason,
		&m.MessageID, &createdAt, &updatedAt, &claimedAt); err != nil {
		return nil, err
	}
	m.FireAt = parseSQLiteTime(fireAt)
	m.CreatedAt = parseSQLiteTime(createdAt)
	m.UpdatedAt = parseSQLiteTime(updatedAt)
	if claimedAt.Valid && claimedAt.String != "" {
		t := parseSQLiteTime(claimedAt.String)
		m.ClaimedAt = &t
	}
	return &m, nil
}

func (s *sqliteWebChatStore) CreateScheduledMessage(ctx context.Context, m *ScheduledChatMessage) (*ScheduledChatMessage, bool, error) {
	const query = `
INSERT INTO webchat_scheduled_message
    (id, sender_user_id, conversation_key, project_id, content, reply_to_id, idempotency_key,
     fire_at, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (sender_user_id, idempotency_key) DO NOTHING
`
	res, err := s.db.ExecContext(ctx, query, m.ID, m.SenderUserID, m.ConversationKey, nullableString(m.ProjectID),
		m.Content, nullableString(m.ReplyToID), m.IdempotencyKey, sqliteScheduledTime(m.FireAt), m.Status,
		sqliteScheduledTime(m.CreatedAt), sqliteScheduledTime(m.UpdatedAt))
	if err != nil {
		return nil, false, fmt.Errorf("webchat store: create scheduled message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("webchat store: create scheduled message: %w", err)
	}
	existed := n == 0
	row, err := scanSQLiteScheduled(s.db.QueryRowContext(ctx,
		`SELECT `+sqliteScheduledColumns+` FROM webchat_scheduled_message WHERE sender_user_id = ? AND idempotency_key = ?`,
		m.SenderUserID, m.IdempotencyKey))
	if err != nil {
		return nil, false, fmt.Errorf("webchat store: read scheduled message: %w", err)
	}
	return row, existed, nil
}

func (s *sqliteWebChatStore) GetScheduledMessage(ctx context.Context, senderUserID, id string) (*ScheduledChatMessage, error) {
	row, err := scanSQLiteScheduled(s.db.QueryRowContext(ctx,
		`SELECT `+sqliteScheduledColumns+` FROM webchat_scheduled_message WHERE id = ? AND sender_user_id = ?`,
		id, senderUserID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("webchat store: get scheduled message: %w", err)
	}
	return row, nil
}

func (s *sqliteWebChatStore) ListScheduledMessages(ctx context.Context, senderUserID, conversationKey string) ([]ScheduledChatMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqliteScheduledColumns+` FROM webchat_scheduled_message
		  WHERE sender_user_id = ? AND conversation_key = ? AND status IN (?, ?, ?)
		  ORDER BY fire_at, created_at, id`,
		senderUserID, conversationKey, ScheduledMessagePending, ScheduledMessageSending, ScheduledMessageFailed)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list scheduled messages: %w", err)
	}
	return collectSQLiteScheduled(rows)
}

func collectSQLiteScheduled(rows *sql.Rows) ([]ScheduledChatMessage, error) {
	defer func() { _ = rows.Close() }()
	out := []ScheduledChatMessage{}
	for rows.Next() {
		m, err := scanSQLiteScheduled(rows)
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

func (s *sqliteWebChatStore) CancelScheduledMessage(ctx context.Context, senderUserID, id string, now time.Time) (bool, error) {
	return execOneRow(s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = ?, updated_at = ?
		  WHERE id = ? AND sender_user_id = ? AND status = ?`,
		ScheduledMessageCancelled, sqliteScheduledTime(now), id, senderUserID, ScheduledMessagePending))
}

func (s *sqliteWebChatStore) ListDueScheduledMessages(ctx context.Context, now time.Time, limit int) ([]ScheduledChatMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sqliteScheduledColumns+` FROM webchat_scheduled_message
		  WHERE status = ? AND fire_at <= ?
		  ORDER BY fire_at, id LIMIT ?`,
		ScheduledMessagePending, sqliteScheduledTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list due scheduled messages: %w", err)
	}
	return collectSQLiteScheduled(rows)
}

func (s *sqliteWebChatStore) ClaimScheduledMessage(ctx context.Context, id string, now time.Time) (bool, error) {
	ts := sqliteScheduledTime(now)
	return execOneRow(s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = ?, claimed_at = ?, updated_at = ?
		  WHERE id = ? AND status = ?`,
		ScheduledMessageSending, ts, ts, id, ScheduledMessagePending))
}

func (s *sqliteWebChatStore) ReleaseScheduledMessage(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = ?, claimed_at = NULL, updated_at = ?
		  WHERE id = ? AND status = ?`,
		ScheduledMessagePending, sqliteScheduledTime(now), id, ScheduledMessageSending)
	if err != nil {
		return fmt.Errorf("webchat store: release scheduled message: %w", err)
	}
	return nil
}

func (s *sqliteWebChatStore) MarkScheduledMessageSent(ctx context.Context, id, messageID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = ?, message_id = ?, updated_at = ?
		  WHERE id = ? AND status = ?`,
		ScheduledMessageSent, messageID, sqliteScheduledTime(now), id, ScheduledMessageSending)
	if err != nil {
		return fmt.Errorf("webchat store: mark scheduled message sent: %w", err)
	}
	return nil
}

func (s *sqliteWebChatStore) MarkScheduledMessageFailed(ctx context.Context, id, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webchat_scheduled_message SET status = ?, failure_reason = ?, updated_at = ?
		  WHERE id = ? AND status = ?`,
		ScheduledMessageFailed, reason, sqliteScheduledTime(now), id, ScheduledMessageSending)
	if err != nil {
		return fmt.Errorf("webchat store: mark scheduled message failed: %w", err)
	}
	return nil
}

// execOneRow reports whether an UPDATE affected exactly one row.
func execOneRow(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, fmt.Errorf("webchat store: update scheduled message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("webchat store: update scheduled message: %w", err)
	}
	return n == 1, nil
}
