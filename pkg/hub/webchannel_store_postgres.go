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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
)

// pgWebChatStore implements WebChatStore for Postgres.
// Uses $N placeholders and Postgres-appropriate DDL types (TIMESTAMPTZ, BOOLEAN).
// This mirrors the Discord store split
// (extras/scion-discord/internal/discord/store_postgres.go).
type pgWebChatStore struct {
	db *sql.DB
}

// Init creates the webchat_* tables using Postgres DDL conventions,
// then runs any pending idempotent migrations.
func (s *pgWebChatStore) Init() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS webchat_thread (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_message_id TEXT,
    last_activity_at TIMESTAMPTZ,
    last_read_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, project_id, agent_id)
);

CREATE TABLE IF NOT EXISTS webchat_conversation_context (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_channel TEXT,
    last_message_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, project_id, agent_id)
);

CREATE TABLE IF NOT EXISTS webchat_thread_prefs (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    visibility_mode TEXT DEFAULT 'conversation',
    show_state_changes BOOLEAN DEFAULT FALSE,
    show_agent_to_agent BOOLEAN DEFAULT FALSE,
    muted BOOLEAN DEFAULT FALSE,
    PRIMARY KEY (user_id, project_id, agent_id)
);

-- Wave-2 tables

CREATE TABLE IF NOT EXISTS webchat_topic (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    name          TEXT NOT NULL,
    is_general    BOOLEAN NOT NULL DEFAULT FALSE,
    default_agent TEXT,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    last_message_id TEXT,
    last_activity_at TIMESTAMPTZ,
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_webchat_topic_project_activity
    ON webchat_topic (project_id, deleted_at, last_activity_at);

CREATE TABLE IF NOT EXISTS webchat_read_state (
    user_id          TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    last_read_message_id TEXT,
    last_read_at     TIMESTAMPTZ,
    pinned           BOOLEAN NOT NULL DEFAULT FALSE,
    muted            BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (user_id, conversation_key)
);

CREATE TABLE IF NOT EXISTS webchat_user_prefs (
    user_id         TEXT PRIMARY KEY,
    space_sort_mode TEXT NOT NULL DEFAULT 'activity',
    space_order     TEXT,
    thread_sort_mode TEXT NOT NULL DEFAULT 'activity'
);

CREATE TABLE IF NOT EXISTS webchat_dm (
    conversation_key TEXT NOT NULL,
    participant_id   TEXT NOT NULL,
    peer_id          TEXT NOT NULL,
    peer_kind        TEXT NOT NULL,
    last_message_id  TEXT,
    last_activity_at TIMESTAMPTZ,
    PRIMARY KEY (participant_id, conversation_key)
);

CREATE TABLE IF NOT EXISTS webchat_migrations (
    name         TEXT PRIMARY KEY,
    completed_at TIMESTAMPTZ
);

-- Wave-2 W7: attachment metadata
CREATE TABLE IF NOT EXISTS webchat_attachment (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    filename    TEXT NOT NULL,
    mime_type   TEXT NOT NULL,
    size        BIGINT NOT NULL,
    uploaded_by TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webchat_attachment_project
    ON webchat_attachment (project_id);

-- Wave-2 W7: message-attachment linkage
CREATE TABLE IF NOT EXISTS webchat_message_attachment (
    message_id    TEXT NOT NULL,
    attachment_id TEXT NOT NULL,
    PRIMARY KEY (message_id, attachment_id)
);

CREATE INDEX IF NOT EXISTS idx_webchat_message_attachment_message
    ON webchat_message_attachment (message_id);

-- Phase-3: message extension data (reply-to, edit, delete)
CREATE TABLE IF NOT EXISTS webchat_message_ext (
    message_id TEXT PRIMARY KEY,
    reply_to_id TEXT,
    edited_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
`
	_, err := s.db.Exec(ddl)
	if err != nil {
		return fmt.Errorf("webchat store: create tables: %w", err)
	}

	// Create partial unique index for #general (one per project).
	const generalIdx = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_one_general
    ON webchat_topic (project_id) WHERE is_general = TRUE AND deleted_at IS NULL;
`
	if _, err := s.db.Exec(generalIdx); err != nil {
		return fmt.Errorf("webchat store: create general index: %w", err)
	}

	// Enforce case-insensitive unique topic name per project (excluding
	// soft-deleted topics). Postgres uses LOWER() for case-insensitive matching.
	const nameIdx = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_project_name
    ON webchat_topic (project_id, LOWER(name)) WHERE deleted_at IS NULL;
`
	if _, err := s.db.Exec(nameIdx); err != nil {
		return fmt.Errorf("webchat store: create name uniqueness index: %w", err)
	}

	// Run idempotent migrations.
	if err := s.runMigrations(); err != nil {
		return fmt.Errorf("webchat store: migrations: %w", err)
	}

	return nil
}

// TouchThread upserts the thread watermark for the given (user, project, agent) triple.
func (s *pgWebChatStore) TouchThread(ctx context.Context, userID, projectID, agentID, messageID string, activityAt time.Time) error {
	const query = `
INSERT INTO webchat_thread (user_id, project_id, agent_id, last_message_id, last_activity_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, project_id, agent_id)
DO UPDATE SET
    last_message_id = EXCLUDED.last_message_id,
    last_activity_at = EXCLUDED.last_activity_at
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID, messageID, activityAt)
	if err != nil {
		return fmt.Errorf("webchat store: touch thread: %w", err)
	}
	return nil
}

// RecordChannel upserts the reply-affinity context for the given (user, project, agent) triple.
func (s *pgWebChatStore) RecordChannel(ctx context.Context, userID, projectID, agentID, channel string, messageAt time.Time) error {
	const query = `
INSERT INTO webchat_conversation_context (user_id, project_id, agent_id, last_channel, last_message_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, project_id, agent_id)
DO UPDATE SET
    last_channel = EXCLUDED.last_channel,
    last_message_at = EXCLUDED.last_message_at
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID, channel, messageAt)
	if err != nil {
		return fmt.Errorf("webchat store: record channel: %w", err)
	}
	return nil
}

// GetLastChannel returns the last channel for (user, project, agent), or "" if no row exists.
func (s *pgWebChatStore) GetLastChannel(ctx context.Context, userID, projectID, agentID string) (string, error) {
	const query = `SELECT last_channel FROM webchat_conversation_context WHERE user_id = $1 AND project_id = $2 AND agent_id = $3`
	var channel sql.NullString
	err := s.db.QueryRowContext(ctx, query, userID, projectID, agentID).Scan(&channel)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("webchat store: get last channel: %w", err)
	}
	return channel.String, nil
}

// GetThreadPrefs returns the display preferences for the given (user, project, agent) triple.
// Returns default prefs (visibility_mode = "conversation") if no row exists.
func (s *pgWebChatStore) GetThreadPrefs(ctx context.Context, userID, projectID, agentID string) (ThreadPrefs, error) {
	const query = `SELECT visibility_mode FROM webchat_thread_prefs WHERE user_id = $1 AND project_id = $2 AND agent_id = $3`
	var mode string
	err := s.db.QueryRowContext(ctx, query, userID, projectID, agentID).Scan(&mode)
	if err != nil {
		if err == sql.ErrNoRows {
			return ThreadPrefs{VisibilityMode: "conversation"}, nil
		}
		return ThreadPrefs{}, fmt.Errorf("webchat store: get thread prefs: %w", err)
	}
	return ThreadPrefs{VisibilityMode: mode}, nil
}

// SetThreadPrefs upserts the display preferences for the given (user, project, agent) triple.
func (s *pgWebChatStore) SetThreadPrefs(ctx context.Context, userID, projectID, agentID string, prefs ThreadPrefs) error {
	const query = `
INSERT INTO webchat_thread_prefs (user_id, project_id, agent_id, visibility_mode)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, project_id, agent_id)
DO UPDATE SET visibility_mode = EXCLUDED.visibility_mode
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID, prefs.VisibilityMode)
	if err != nil {
		return fmt.Errorf("webchat store: set thread prefs: %w", err)
	}
	return nil
}

// GetThreads returns thread watermarks for the given user and project.
func (s *pgWebChatStore) GetThreads(ctx context.Context, userID, projectID string, limit int) ([]WebChatThread, error) {
	const query = `
SELECT agent_id, COALESCE(last_message_id, ''), last_activity_at, last_read_at
  FROM webchat_thread
 WHERE user_id = $1 AND project_id = $2
 ORDER BY last_activity_at DESC
 LIMIT $3
`
	rows, err := s.db.QueryContext(ctx, query, userID, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var threads []WebChatThread
	for rows.Next() {
		var t WebChatThread
		var activityAt *time.Time
		var readAt *time.Time
		if err := rows.Scan(&t.AgentID, &t.LastMessageID, &activityAt, &readAt); err != nil {
			return nil, fmt.Errorf("webchat store: scan thread: %w", err)
		}
		if activityAt != nil {
			t.LastActivityAt = *activityAt
		}
		t.LastReadAt = readAt
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// MarkThreadRead advances the last_read_at watermark to now.
func (s *pgWebChatStore) MarkThreadRead(ctx context.Context, userID, projectID, agentID string) error {
	const query = `
UPDATE webchat_thread
   SET last_read_at = NOW()
 WHERE user_id = $1 AND project_id = $2 AND agent_id = $3
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID)
	if err != nil {
		return fmt.Errorf("webchat store: mark thread read: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave-2 Topic methods (Postgres)
// ---------------------------------------------------------------------------

// CreateTopic inserts a new topic. Returns an error on name conflict.
func (s *pgWebChatStore) CreateTopic(ctx context.Context, topic WebChatTopic) error {
	const query = `
INSERT INTO webchat_topic (id, project_id, name, is_general, default_agent, created_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	var defaultAgent interface{}
	if topic.DefaultAgent != "" {
		defaultAgent = topic.DefaultAgent
	}
	_, err := s.db.ExecContext(ctx, query, topic.ID, topic.ProjectID, topic.Name,
		topic.IsGeneral, defaultAgent, topic.CreatedBy, topic.CreatedAt)
	if err != nil {
		return fmt.Errorf("webchat store: create topic: %w", err)
	}
	return nil
}

// GetTopic returns a topic by ID. Returns nil if not found or soft-deleted.
func (s *pgWebChatStore) GetTopic(ctx context.Context, topicID string) (*WebChatTopic, error) {
	const query = `
SELECT id, project_id, name, is_general, COALESCE(default_agent, ''),
       created_by, created_at, COALESCE(last_message_id, ''),
       last_activity_at, deleted_at
  FROM webchat_topic
 WHERE id = $1 AND deleted_at IS NULL
`
	var t WebChatTopic
	var activityAt *time.Time
	var deletedAt *time.Time
	err := s.db.QueryRowContext(ctx, query, topicID).Scan(
		&t.ID, &t.ProjectID, &t.Name, &t.IsGeneral, &t.DefaultAgent,
		&t.CreatedBy, &t.CreatedAt, &t.LastMessageID, &activityAt, &deletedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("webchat store: get topic: %w", err)
	}
	if activityAt != nil {
		t.LastActivityAt = *activityAt
	}
	t.DeletedAt = deletedAt
	return &t, nil
}

// ListTopics returns non-deleted topics for a project, ordered by last_activity_at DESC.
// Lazily creates #general if none exists.
func (s *pgWebChatStore) ListTopics(ctx context.Context, projectID string) ([]WebChatTopic, error) {
	const query = `
SELECT id, project_id, name, is_general, COALESCE(default_agent, ''),
       created_by, created_at, COALESCE(last_message_id, ''),
       last_activity_at, deleted_at
  FROM webchat_topic
 WHERE project_id = $1 AND deleted_at IS NULL
 ORDER BY last_activity_at DESC
`
	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list topics: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var topics []WebChatTopic
	hasGeneral := false
	for rows.Next() {
		var t WebChatTopic
		var activityAt *time.Time
		var deletedAt *time.Time
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Name, &t.IsGeneral, &t.DefaultAgent,
			&t.CreatedBy, &t.CreatedAt, &t.LastMessageID, &activityAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("webchat store: scan topic: %w", err)
		}
		if activityAt != nil {
			t.LastActivityAt = *activityAt
		}
		t.DeletedAt = deletedAt
		if t.IsGeneral {
			hasGeneral = true
		}
		topics = append(topics, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webchat store: list topics rows: %w", err)
	}

	// Lazy #general creation for pre-existing projects.
	if !hasGeneral {
		generalID, _, err := s.EnsureGeneralTopic(ctx, projectID, "system")
		if err != nil {
			slog.Warn("webchat store: lazy #general creation failed", "project_id", projectID, "error", err)
		} else {
			general, err := s.GetTopic(ctx, generalID)
			if err == nil && general != nil {
				topics = append([]WebChatTopic{*general}, topics...)
			}
		}
	}

	return topics, nil
}

// UpdateTopic applies partial updates to a topic.
func (s *pgWebChatStore) UpdateTopic(ctx context.Context, topicID string, updates TopicUpdate) error {
	var sets []string
	var args []interface{}
	argIdx := 1

	if updates.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, *updates.Name)
		argIdx++
	}
	if updates.DefaultAgent != nil {
		sets = append(sets, fmt.Sprintf("default_agent = $%d", argIdx))
		var val interface{}
		if *updates.DefaultAgent != "" {
			val = *updates.DefaultAgent
		}
		args = append(args, val)
		argIdx++
	}
	if len(sets) == 0 {
		return nil
	}

	args = append(args, topicID)
	query := fmt.Sprintf("UPDATE webchat_topic SET %s WHERE id = $%d AND deleted_at IS NULL",
		strings.Join(sets, ", "), argIdx)
	_, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("webchat store: update topic: %w", err)
	}
	return nil
}

// DeleteTopic soft-deletes a topic. Returns an error if it is #general.
func (s *pgWebChatStore) DeleteTopic(ctx context.Context, topicID string) error {
	var isGeneral bool
	err := s.db.QueryRowContext(ctx, "SELECT is_general FROM webchat_topic WHERE id = $1", topicID).Scan(&isGeneral)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("webchat store: delete topic check: %w", err)
	}
	if isGeneral {
		return fmt.Errorf("webchat store: delete topic: cannot delete #general topic")
	}

	const query = `UPDATE webchat_topic SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`
	_, err = s.db.ExecContext(ctx, query, topicID)
	if err != nil {
		return fmt.Errorf("webchat store: delete topic: %w", err)
	}
	return nil
}

// TouchTopicActivity updates last_activity_at and, when messageID is
// non-empty, also updates last_message_id. An empty messageID is accepted
// gracefully — this happens when the spoke receives a StructuredMessage
// (which has no ID) rather than a store.Message.
func (s *pgWebChatStore) TouchTopicActivity(ctx context.Context, topicID, messageID string) error {
	if messageID != "" {
		const q = `UPDATE webchat_topic SET last_message_id = $1, last_activity_at = NOW() WHERE id = $2`
		_, err := s.db.ExecContext(ctx, q, messageID, topicID)
		if err != nil {
			return fmt.Errorf("webchat store: touch topic activity: %w", err)
		}
		return nil
	}
	const q = `UPDATE webchat_topic SET last_activity_at = NOW() WHERE id = $1`
	_, err := s.db.ExecContext(ctx, q, topicID)
	if err != nil {
		return fmt.Errorf("webchat store: touch topic activity: %w", err)
	}
	return nil
}

// EnsureGeneralTopic idempotently creates the #general topic for a project.
// Returns the topic ID (existing or new) and whether a new row was inserted.
func (s *pgWebChatStore) EnsureGeneralTopic(ctx context.Context, projectID, createdBy string) (string, bool, error) {
	newID := uuid.New().String()

	const insert = `
INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at)
VALUES ($1, $2, 'general', TRUE, $3, NOW())
ON CONFLICT DO NOTHING
`
	_, err := s.db.ExecContext(ctx, insert, newID, projectID, createdBy)
	if err != nil {
		return "", false, fmt.Errorf("webchat store: ensure general topic: %w", err)
	}

	const lookup = `SELECT id FROM webchat_topic WHERE project_id = $1 AND is_general = TRUE AND deleted_at IS NULL`
	var id string
	err = s.db.QueryRowContext(ctx, lookup, projectID).Scan(&id)
	if err != nil {
		return "", false, fmt.Errorf("webchat store: ensure general topic lookup: %w", err)
	}
	return id, id == newID, nil
}

// ---------------------------------------------------------------------------
// Wave-2 Read-state methods (Postgres)
// ---------------------------------------------------------------------------

// GetReadState returns the read state for a user+conversation pair.
func (s *pgWebChatStore) GetReadState(ctx context.Context, userID, conversationKey string) (*WebChatReadState, error) {
	const query = `
SELECT user_id, conversation_key, COALESCE(last_read_message_id, ''),
       last_read_at, pinned, muted
  FROM webchat_read_state
 WHERE user_id = $1 AND conversation_key = $2
`
	var rs WebChatReadState
	var lastReadAt *time.Time
	err := s.db.QueryRowContext(ctx, query, userID, conversationKey).Scan(
		&rs.UserID, &rs.ConversationKey, &rs.LastReadMessageID,
		&lastReadAt, &rs.Pinned, &rs.Muted)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("webchat store: get read state: %w", err)
	}
	if lastReadAt != nil {
		rs.LastReadAt = *lastReadAt
	}
	return &rs, nil
}

// SetReadState upserts the read watermark.
func (s *pgWebChatStore) SetReadState(ctx context.Context, userID, conversationKey, messageID string) error {
	const query = `
INSERT INTO webchat_read_state (user_id, conversation_key, last_read_message_id, last_read_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (user_id, conversation_key)
DO UPDATE SET
    last_read_message_id = EXCLUDED.last_read_message_id,
    last_read_at = EXCLUDED.last_read_at
`
	_, err := s.db.ExecContext(ctx, query, userID, conversationKey, messageID)
	if err != nil {
		return fmt.Errorf("webchat store: set read state: %w", err)
	}
	return nil
}

// GetReadStates returns read states for a user across multiple conversations.
func (s *pgWebChatStore) GetReadStates(ctx context.Context, userID string, conversationKeys []string) ([]WebChatReadState, error) {
	if len(conversationKeys) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(conversationKeys))
	args := make([]interface{}, 0, len(conversationKeys)+1)
	args = append(args, userID)
	for i, key := range conversationKeys {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, key)
	}

	query := fmt.Sprintf(`
SELECT user_id, conversation_key, COALESCE(last_read_message_id, ''),
       last_read_at, pinned, muted
  FROM webchat_read_state
 WHERE user_id = $1 AND conversation_key IN (%s)
`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get read states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var states []WebChatReadState
	for rows.Next() {
		var rs WebChatReadState
		var lastReadAt *time.Time
		if err := rows.Scan(&rs.UserID, &rs.ConversationKey, &rs.LastReadMessageID,
			&lastReadAt, &rs.Pinned, &rs.Muted); err != nil {
			return nil, fmt.Errorf("webchat store: scan read state: %w", err)
		}
		if lastReadAt != nil {
			rs.LastReadAt = *lastReadAt
		}
		states = append(states, rs)
	}
	return states, rows.Err()
}

// SetPinned updates the pinned flag.
func (s *pgWebChatStore) SetPinned(ctx context.Context, userID, conversationKey string, pinned bool) error {
	const query = `
INSERT INTO webchat_read_state (user_id, conversation_key, pinned)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, conversation_key)
DO UPDATE SET pinned = EXCLUDED.pinned
`
	_, err := s.db.ExecContext(ctx, query, userID, conversationKey, pinned)
	if err != nil {
		return fmt.Errorf("webchat store: set pinned: %w", err)
	}
	return nil
}

// SetMuted updates the muted flag.
func (s *pgWebChatStore) SetMuted(ctx context.Context, userID, conversationKey string, muted bool) error {
	const query = `
INSERT INTO webchat_read_state (user_id, conversation_key, muted)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, conversation_key)
DO UPDATE SET muted = EXCLUDED.muted
`
	_, err := s.db.ExecContext(ctx, query, userID, conversationKey, muted)
	if err != nil {
		return fmt.Errorf("webchat store: set muted: %w", err)
	}
	return nil
}

// IsConversationMuted returns whether the user has muted the conversation.
// Returns false (unmuted) when no read-state row exists.
func (s *pgWebChatStore) IsConversationMuted(ctx context.Context, userID, conversationKey string) (bool, error) {
	const query = `SELECT muted FROM webchat_read_state WHERE user_id = $1 AND conversation_key = $2`
	var muted bool
	if err := s.db.QueryRowContext(ctx, query, userID, conversationKey).Scan(&muted); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("webchat store: is conversation muted: %w", err)
	}
	return muted, nil
}

// ---------------------------------------------------------------------------
// Wave-2 User-prefs methods (Postgres)
// ---------------------------------------------------------------------------

// GetUserPrefs returns the user's rail preferences. Returns defaults if no row.
func (s *pgWebChatStore) GetUserPrefs(ctx context.Context, userID string) (*WebChatUserPrefs, error) {
	const query = `
SELECT user_id, space_sort_mode, COALESCE(space_order, ''), thread_sort_mode
  FROM webchat_user_prefs
 WHERE user_id = $1
`
	var p WebChatUserPrefs
	err := s.db.QueryRowContext(ctx, query, userID).Scan(
		&p.UserID, &p.SpaceSortMode, &p.SpaceOrder, &p.ThreadSortMode)
	if err != nil {
		if err == sql.ErrNoRows {
			return &WebChatUserPrefs{
				UserID:         userID,
				SpaceSortMode:  "activity",
				ThreadSortMode: "activity",
			}, nil
		}
		return nil, fmt.Errorf("webchat store: get user prefs: %w", err)
	}
	return &p, nil
}

// SetUserPrefs upserts the user's rail preferences.
func (s *pgWebChatStore) SetUserPrefs(ctx context.Context, userID string, prefs WebChatUserPrefs) error {
	const query = `
INSERT INTO webchat_user_prefs (user_id, space_sort_mode, space_order, thread_sort_mode)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id)
DO UPDATE SET
    space_sort_mode = EXCLUDED.space_sort_mode,
    space_order = EXCLUDED.space_order,
    thread_sort_mode = EXCLUDED.thread_sort_mode
`
	var spaceOrder interface{}
	if prefs.SpaceOrder != "" {
		spaceOrder = prefs.SpaceOrder
	}
	_, err := s.db.ExecContext(ctx, query, userID, prefs.SpaceSortMode,
		spaceOrder, prefs.ThreadSortMode)
	if err != nil {
		return fmt.Errorf("webchat store: set user prefs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave-2 DM methods (Postgres)
// ---------------------------------------------------------------------------

// UpsertDM upserts a participant row for a DM conversation.
//
// An empty LastMessageID means "unknown", not "clear it": registration
// callers (ensureDMRegistered) never carry a message ID, and overwriting the
// stored watermark with NULL would break the unread indicator, which compares
// last_message_id against the reader's watermark.
func (s *pgWebChatStore) UpsertDM(ctx context.Context, dm WebChatDM) error {
	const query = `
INSERT INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_message_id, last_activity_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (participant_id, conversation_key)
DO UPDATE SET
    peer_id = EXCLUDED.peer_id,
    peer_kind = EXCLUDED.peer_kind,
    last_message_id = COALESCE(EXCLUDED.last_message_id, webchat_dm.last_message_id),
    last_activity_at = COALESCE(EXCLUDED.last_activity_at, webchat_dm.last_activity_at)
`
	var lastMsgID interface{}
	if dm.LastMessageID != "" {
		lastMsgID = dm.LastMessageID
	}
	var activityAt interface{}
	if !dm.LastActivityAt.IsZero() {
		activityAt = dm.LastActivityAt
	}
	_, err := s.db.ExecContext(ctx, query, dm.ConversationKey, dm.ParticipantID,
		dm.PeerID, dm.PeerKind, lastMsgID, activityAt)
	if err != nil {
		return fmt.Errorf("webchat store: upsert dm: %w", err)
	}
	return nil
}

// ListDMs returns all DM conversations for a participant.
func (s *pgWebChatStore) ListDMs(ctx context.Context, participantID string) ([]WebChatDM, error) {
	const query = `
SELECT conversation_key, participant_id, peer_id, peer_kind,
       COALESCE(last_message_id, ''), last_activity_at
  FROM webchat_dm
 WHERE participant_id = $1
 ORDER BY last_activity_at DESC
`
	rows, err := s.db.QueryContext(ctx, query, participantID)
	if err != nil {
		return nil, fmt.Errorf("webchat store: list dms: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var dms []WebChatDM
	for rows.Next() {
		var dm WebChatDM
		var activityAt *time.Time
		if err := rows.Scan(&dm.ConversationKey, &dm.ParticipantID, &dm.PeerID,
			&dm.PeerKind, &dm.LastMessageID, &activityAt); err != nil {
			return nil, fmt.Errorf("webchat store: scan dm: %w", err)
		}
		if activityAt != nil {
			dm.LastActivityAt = *activityAt
		}
		dms = append(dms, dm)
	}
	return dms, rows.Err()
}

// TouchDMActivity updates watermarks for a DM conversation (all participant
// rows). When messageID is empty, only last_activity_at is updated.
func (s *pgWebChatStore) TouchDMActivity(ctx context.Context, conversationKey, messageID string) error {
	if messageID != "" {
		const q = `UPDATE webchat_dm SET last_message_id = $1, last_activity_at = NOW() WHERE conversation_key = $2`
		_, err := s.db.ExecContext(ctx, q, messageID, conversationKey)
		if err != nil {
			return fmt.Errorf("webchat store: touch dm activity: %w", err)
		}
		return nil
	}
	const q = `UPDATE webchat_dm SET last_activity_at = NOW() WHERE conversation_key = $1`
	_, err := s.db.ExecContext(ctx, q, conversationKey)
	if err != nil {
		return fmt.Errorf("webchat store: touch dm activity: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave-2 Search methods (Postgres)
// ---------------------------------------------------------------------------

// SearchChatMessages performs a case-insensitive substring search over the
// messages table, scoped to channel='web'. Uses ILIKE for case-insensitive
// matching.
func (s *pgWebChatStore) SearchChatMessages(ctx context.Context, filter ChatSearchFilter) ([]ChatSearchResult, string, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}

	// Check if the messages table exists.
	var tableExists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT FROM information_schema.tables WHERE table_name = 'messages'
	)`).Scan(&tableExists); err != nil || !tableExists {
		return nil, "", nil
	}

	// Build the query dynamically based on filter.
	var conditions []string
	var args []interface{}
	argIdx := 1

	conditions = append(conditions, "channel = 'web'")
	conditions = append(conditions, fmt.Sprintf("msg ILIKE '%%' || $%d || '%%'", argIdx))
	args = append(args, filter.Query)
	argIdx++

	if filter.ConversationKey != "" {
		conditions = append(conditions, fmt.Sprintf("thread_id = $%d", argIdx))
		args = append(args, filter.ConversationKey)
		argIdx++
	}

	if filter.ProjectID != "" {
		conditions = append(conditions, fmt.Sprintf("project_id = $%d", argIdx))
		args = append(args, filter.ProjectID)
		argIdx++
	} else if len(filter.ProjectIDs) > 0 {
		placeholders := make([]string, len(filter.ProjectIDs))
		for i, pid := range filter.ProjectIDs {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, pid)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("project_id IN (%s)", strings.Join(placeholders, ",")))
	}

	// Keyset pagination cursor: "timestamp|id"
	if filter.Cursor != "" {
		cursorParts := strings.SplitN(filter.Cursor, "|", 2)
		if len(cursorParts) == 2 {
			conditions = append(conditions, fmt.Sprintf("(created < $%d OR (created = $%d AND id < $%d))", argIdx, argIdx+1, argIdx+2))
			args = append(args, cursorParts[0], cursorParts[0], cursorParts[1])
			argIdx += 3
		}
	}

	query := fmt.Sprintf(`
SELECT id, project_id, COALESCE(thread_id, ''), sender, msg, created
  FROM messages
 WHERE %s
 ORDER BY created DESC, id DESC
 LIMIT $%d
`, strings.Join(conditions, " AND "), argIdx)
	args = append(args, filter.Limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("webchat store: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []ChatSearchResult
	for rows.Next() {
		var r ChatSearchResult
		var createdAt *time.Time
		if err := rows.Scan(&r.MessageID, &r.ProjectID, &r.ConversationKey, &r.SenderName, &r.Content, &createdAt); err != nil {
			return nil, "", fmt.Errorf("webchat store: scan search result: %w", err)
		}
		if createdAt != nil {
			r.Timestamp = *createdAt
		}
		r.Snippet = generateSnippet(r.Content, filter.Query, 80)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("webchat store: search rows: %w", err)
	}

	// Determine next cursor.
	var nextCursor string
	if len(results) > filter.Limit {
		results = results[:filter.Limit]
		last := results[filter.Limit-1]
		nextCursor = last.Timestamp.UTC().Format(time.RFC3339Nano) + "|" + last.MessageID
	}

	return results, nextCursor, nil
}

// ---------------------------------------------------------------------------
// Wave-2 Migrations (Postgres)
// ---------------------------------------------------------------------------

// runMigrations executes idempotent data migrations.
func (s *pgWebChatStore) runMigrations() error {
	if err := s.migrateThreadIDs(DefaultMigrationBatchSize); err != nil {
		return fmt.Errorf("thread_id backfill: %w", err)
	}
	if err := s.seedFromWave1(); err != nil {
		return fmt.Errorf("wave-1 seed: %w", err)
	}
	return nil
}

// migrationCompleted checks whether a named migration has already run.
func (s *pgWebChatStore) migrationCompleted(name string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM webchat_migrations WHERE name = $1", name).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// markMigrationCompleted records a migration as done.
func (s *pgWebChatStore) markMigrationCompleted(name string) error {
	const query = `INSERT INTO webchat_migrations (name, completed_at) VALUES ($1, NOW())`
	_, err := s.db.Exec(query, name)
	return err
}

// migrateThreadIDs backfills thread_id from "agent:<id>" to "dm:agent:<aid>:user:<uid>"
// for web channel messages. Processes in batches to avoid locking.
func (s *pgWebChatStore) migrateThreadIDs(batchSize int) error {
	done, err := s.migrationCompleted("thread_id_backfill")
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// Check if the messages table exists (it's Ent-managed and may not exist in tests).
	var tableExists bool
	err = s.db.QueryRow(`SELECT EXISTS (
		SELECT FROM information_schema.tables WHERE table_name = 'messages'
	)`).Scan(&tableExists)
	if err != nil || !tableExists {
		return s.markMigrationCompleted("thread_id_backfill")
	}

	for {
		res, err := s.db.Exec(`
UPDATE messages SET thread_id = 'dm:agent:' ||
    CASE
        WHEN sender LIKE 'agent:%' THEN sender_id
        ELSE recipient_id
    END || ':user:' ||
    CASE
        WHEN sender LIKE 'agent:%' THEN
            CASE WHEN recipient_id IS NOT NULL AND recipient_id != '' THEN recipient_id ELSE REPLACE(recipient, 'user:', '') END
        ELSE
            CASE WHEN sender_id IS NOT NULL AND sender_id != '' THEN sender_id ELSE REPLACE(sender, 'user:', '') END
    END
WHERE id IN (
    SELECT id FROM messages
     WHERE channel = 'web' AND thread_id LIKE 'agent:%'
     LIMIT $1
)
`, batchSize)
		if err != nil {
			return fmt.Errorf("batch update: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			break
		}
	}

	return s.markMigrationCompleted("thread_id_backfill")
}

// seedFromWave1 populates webchat_dm and webchat_read_state from the wave-1
// webchat_thread and webchat_thread_prefs tables.
func (s *pgWebChatStore) seedFromWave1() error {
	done, err := s.migrationCompleted("wave1_seed")
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// Seed webchat_dm entries from webchat_thread rows (user side).
	const seedDM = `
INSERT INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_message_id, last_activity_at)
SELECT
    'dm:agent:' || t.agent_id || ':user:' || t.user_id,
    t.user_id,
    t.agent_id,
    'agent',
    t.last_message_id,
    t.last_activity_at
FROM webchat_thread t
ON CONFLICT (participant_id, conversation_key) DO NOTHING
`
	if _, err := s.db.Exec(seedDM); err != nil {
		return fmt.Errorf("seed dm (user side): %w", err)
	}

	// Agent side of the DM.
	const seedDMAgent = `
INSERT INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_message_id, last_activity_at)
SELECT
    'dm:agent:' || t.agent_id || ':user:' || t.user_id,
    t.agent_id,
    t.user_id,
    'user',
    t.last_message_id,
    t.last_activity_at
FROM webchat_thread t
ON CONFLICT (participant_id, conversation_key) DO NOTHING
`
	if _, err := s.db.Exec(seedDMAgent); err != nil {
		return fmt.Errorf("seed dm (agent side): %w", err)
	}

	// Seed webchat_read_state from webchat_thread watermarks.
	const seedReadState = `
INSERT INTO webchat_read_state (user_id, conversation_key, last_read_at)
SELECT
    t.user_id,
    'dm:agent:' || t.agent_id || ':user:' || t.user_id,
    t.last_read_at
FROM webchat_thread t
WHERE t.last_read_at IS NOT NULL
ON CONFLICT (user_id, conversation_key) DO NOTHING
`
	if _, err := s.db.Exec(seedReadState); err != nil {
		return fmt.Errorf("seed read state: %w", err)
	}

	// Carry muted flag from webchat_thread_prefs into webchat_read_state.
	const seedMuted = `
INSERT INTO webchat_read_state (user_id, conversation_key, muted)
SELECT
    p.user_id,
    'dm:agent:' || p.agent_id || ':user:' || p.user_id,
    p.muted
FROM webchat_thread_prefs p
WHERE p.muted = TRUE
ON CONFLICT (user_id, conversation_key) DO UPDATE SET muted = EXCLUDED.muted
`
	if _, err := s.db.Exec(seedMuted); err != nil {
		return fmt.Errorf("seed muted: %w", err)
	}

	return s.markMigrationCompleted("wave1_seed")
}

// ---------------------------------------------------------------------------
// W7: Attachment metadata CRUD (Postgres)
// ---------------------------------------------------------------------------

// CreateAttachment inserts a new attachment metadata row.
func (s *pgWebChatStore) CreateAttachment(ctx context.Context, meta AttachmentMeta) error {
	const query = `
INSERT INTO webchat_attachment (id, project_id, filename, mime_type, size, uploaded_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	_, err := s.db.ExecContext(ctx, query, meta.ID, meta.ProjectID, meta.Filename, meta.MimeType, meta.Size, meta.UploadedBy, meta.CreatedAt)
	if err != nil {
		return fmt.Errorf("webchat store: create attachment: %w", err)
	}
	return nil
}

// GetAttachment returns attachment metadata by ID.
func (s *pgWebChatStore) GetAttachment(ctx context.Context, id string) (*AttachmentMeta, error) {
	const query = `
SELECT id, project_id, filename, mime_type, size, uploaded_by, created_at
FROM webchat_attachment WHERE id = $1
`
	var meta AttachmentMeta
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&meta.ID, &meta.ProjectID, &meta.Filename, &meta.MimeType,
		&meta.Size, &meta.UploadedBy, &meta.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get attachment: %w", err)
	}
	return &meta, nil
}

// DeleteAttachment removes attachment metadata by ID.
func (s *pgWebChatStore) DeleteAttachment(ctx context.Context, id string) error {
	const query = `DELETE FROM webchat_attachment WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("webchat store: delete attachment: %w", err)
	}
	return nil
}

// LinkAttachmentToMessage associates an attachment with a message.
func (s *pgWebChatStore) LinkAttachmentToMessage(ctx context.Context, messageID, attachmentID string) error {
	const query = `
INSERT INTO webchat_message_attachment (message_id, attachment_id)
VALUES ($1, $2)
ON CONFLICT (message_id, attachment_id) DO NOTHING
`
	_, err := s.db.ExecContext(ctx, query, messageID, attachmentID)
	if err != nil {
		return fmt.Errorf("webchat store: link attachment: %w", err)
	}
	return nil
}

// GetAttachmentsByMessage returns all attachments linked to a message.
func (s *pgWebChatStore) GetAttachmentsByMessage(ctx context.Context, messageID string) ([]AttachmentMeta, error) {
	const query = `
SELECT a.id, a.project_id, a.filename, a.mime_type, a.size, a.uploaded_by, a.created_at
FROM webchat_attachment a
JOIN webchat_message_attachment ma ON ma.attachment_id = a.id
WHERE ma.message_id = $1
`
	rows, err := s.db.QueryContext(ctx, query, messageID)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get message attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []AttachmentMeta
	for rows.Next() {
		var meta AttachmentMeta
		if err := rows.Scan(&meta.ID, &meta.ProjectID, &meta.Filename, &meta.MimeType,
			&meta.Size, &meta.UploadedBy, &meta.CreatedAt); err != nil {
			return nil, fmt.Errorf("webchat store: scan attachment: %w", err)
		}
		result = append(result, meta)
	}
	return result, rows.Err()
}

// GetAttachmentsByMessages returns attachments for multiple messages in a
// single IN (...) query, keyed by message ID. This replaces per-message
// loops in the history endpoint to avoid N+1 queries.
func (s *pgWebChatStore) GetAttachmentsByMessages(ctx context.Context, messageIDs []string) (map[string][]AttachmentMeta, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(messageIDs))
	args := make([]interface{}, len(messageIDs))
	for i, id := range messageIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}

	query := fmt.Sprintf(`
SELECT ma.message_id, a.id, a.project_id, a.filename, a.mime_type, a.size, a.uploaded_by, a.created_at
FROM webchat_attachment a
JOIN webchat_message_attachment ma ON ma.attachment_id = a.id
WHERE ma.message_id IN (%s)
`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get messages attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string][]AttachmentMeta)
	for rows.Next() {
		var messageID string
		var meta AttachmentMeta
		if err := rows.Scan(&messageID, &meta.ID, &meta.ProjectID, &meta.Filename, &meta.MimeType,
			&meta.Size, &meta.UploadedBy, &meta.CreatedAt); err != nil {
			return nil, fmt.Errorf("webchat store: scan attachment: %w", err)
		}
		result[messageID] = append(result[messageID], meta)
	}
	return result, rows.Err()
}

// ---------------------------------------------------------------------------
// Phase-3: message extension methods (Postgres)
// ---------------------------------------------------------------------------

// SetMessageReplyTo upserts the reply_to_id for a message.
func (s *pgWebChatStore) SetMessageReplyTo(ctx context.Context, messageID, replyToID string) error {
	const query = `
INSERT INTO webchat_message_ext (message_id, reply_to_id)
VALUES ($1, $2)
ON CONFLICT (message_id)
DO UPDATE SET reply_to_id = EXCLUDED.reply_to_id
`
	_, err := s.db.ExecContext(ctx, query, messageID, replyToID)
	if err != nil {
		return fmt.Errorf("webchat store: set message reply_to: %w", err)
	}
	return nil
}

// GetMessageExt returns the extension row for a single message.
func (s *pgWebChatStore) GetMessageExt(ctx context.Context, messageID string) (*WebChatMessageExt, error) {
	const query = `SELECT message_id, reply_to_id, edited_at, deleted_at FROM webchat_message_ext WHERE message_id = $1`
	var ext WebChatMessageExt
	var replyToID sql.NullString
	var editedAt, deletedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, query, messageID).Scan(&ext.MessageID, &replyToID, &editedAt, &deletedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("webchat store: get message ext: %w", err)
	}
	ext.ReplyToID = replyToID.String
	if editedAt.Valid {
		ext.EditedAt = &editedAt.Time
	}
	if deletedAt.Valid {
		ext.DeletedAt = &deletedAt.Time
	}
	return &ext, nil
}

// GetMessageExts returns extension rows for multiple messages in a single query.
func (s *pgWebChatStore) GetMessageExts(ctx context.Context, messageIDs []string) (map[string]*WebChatMessageExt, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(messageIDs))
	args := make([]interface{}, len(messageIDs))
	for i, id := range messageIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := `SELECT message_id, reply_to_id, edited_at, deleted_at FROM webchat_message_ext WHERE message_id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get message exts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]*WebChatMessageExt, len(messageIDs))
	for rows.Next() {
		var ext WebChatMessageExt
		var replyToID sql.NullString
		var editedAt, deletedAt sql.NullTime
		if err := rows.Scan(&ext.MessageID, &replyToID, &editedAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("webchat store: scan message ext: %w", err)
		}
		ext.ReplyToID = replyToID.String
		if editedAt.Valid {
			ext.EditedAt = &editedAt.Time
		}
		if deletedAt.Valid {
			ext.DeletedAt = &deletedAt.Time
		}
		result[ext.MessageID] = &ext
	}
	return result, rows.Err()
}

// SetMessageEdited marks a message as edited at the given time.
func (s *pgWebChatStore) SetMessageEdited(ctx context.Context, messageID string, editedAt time.Time) error {
	const query = `
INSERT INTO webchat_message_ext (message_id, edited_at)
VALUES ($1, $2)
ON CONFLICT (message_id)
DO UPDATE SET edited_at = EXCLUDED.edited_at
`
	_, err := s.db.ExecContext(ctx, query, messageID, editedAt)
	if err != nil {
		return fmt.Errorf("webchat store: set message edited: %w", err)
	}
	return nil
}

// SetMessageDeleted marks a message as soft-deleted at the given time.
func (s *pgWebChatStore) SetMessageDeleted(ctx context.Context, messageID string, deletedAt time.Time) error {
	const query = `
INSERT INTO webchat_message_ext (message_id, deleted_at)
VALUES ($1, $2)
ON CONFLICT (message_id)
DO UPDATE SET deleted_at = EXCLUDED.deleted_at
`
	_, err := s.db.ExecContext(ctx, query, messageID, deletedAt)
	if err != nil {
		return fmt.Errorf("webchat store: set message deleted: %w", err)
	}
	return nil
}

// UpdateMessageContent updates the content (msg column) of a message in the
// Ent messages table using raw SQL. This bypasses the Ent ORM intentionally
// because the webchat layer must not import the ent package.
func (s *pgWebChatStore) UpdateMessageContent(ctx context.Context, messageID, content string) error {
	const query = `UPDATE messages SET msg = $1 WHERE id = $2`
	res, err := s.db.ExecContext(ctx, query, content, messageID)
	if err != nil {
		return fmt.Errorf("webchat store: update message content: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("webchat store: message %s not found", messageID)
	}
	return nil
}
