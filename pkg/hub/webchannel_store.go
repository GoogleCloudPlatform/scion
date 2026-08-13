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

// WebChatStore is the single access point for all webchat_* tables.
// All webchat state goes through this interface — no scattered raw SQL.
//
// Tables are created with CREATE TABLE IF NOT EXISTS at Init time,
// following the same convention as Discord and Telegram integrations
// (see extras/scion-discord/internal/discord/store.go). They are NOT
// Ent entities — this keeps the Ent migration graph clean and preserves
// the option to extract native chat into a plugin binary later.
//
// Two implementations exist — one per dialect:
//   - sqliteWebChatStore  (this file)       — uses ? placeholders, TEXT/INTEGER DDL
//   - pgWebChatStore      (webchannel_store_postgres.go) — uses $N placeholders, TIMESTAMPTZ/BOOLEAN DDL
//
// This mirrors the Discord store split
// (extras/scion-discord/internal/discord/store.go vs store_postgres.go).
type WebChatStore interface {
	// Init creates the webchat_* tables if they do not exist.
	Init() error

	// TouchThread upserts the thread watermark for (user, project, agent).
	// This is called from Publish on every message that passes through
	// the web spoke, so the rail endpoint (Phase 5) can do a single
	// indexed read instead of an aggregate query.
	TouchThread(ctx context.Context, userID, projectID, agentID, messageID string, activityAt time.Time) error

	// RecordChannel upserts reply-affinity context for (user, project, agent).
	// Records the last channel a message was seen on, so the hub can route
	// untagged replies back to the channel the user last spoke from.
	RecordChannel(ctx context.Context, userID, projectID, agentID, channel string, messageAt time.Time) error

	// GetLastChannel returns the last channel recorded for (user, project, agent),
	// or "" if no row exists. Used for reply affinity: when an agent sends an
	// untagged reply, the hub checks here to route to the channel the user last
	// spoke from.
	GetLastChannel(ctx context.Context, userID, projectID, agentID string) (string, error)

	// GetThreadPrefs returns the display preferences for a (user, project, agent) thread.
	// Returns default prefs (visibility_mode = "conversation") if no row exists.
	GetThreadPrefs(ctx context.Context, userID, projectID, agentID string) (ThreadPrefs, error)

	// SetThreadPrefs upserts the display preferences for a (user, project, agent) thread.
	SetThreadPrefs(ctx context.Context, userID, projectID, agentID string, prefs ThreadPrefs) error

	// GetThreads returns thread watermarks for the given user and project,
	// ordered by last_activity_at descending, limited to `limit` rows.
	// This is the backing query for GET /api/v1/chat/threads.
	GetThreads(ctx context.Context, userID, projectID string, limit int) ([]WebChatThread, error)

	// MarkThreadRead advances the last_read_at watermark for the given
	// (user, project, agent) thread to the current time.
	MarkThreadRead(ctx context.Context, userID, projectID, agentID string) error

	// --- Wave-2 Topic methods ---

	// CreateTopic inserts a new topic. Returns an error on name conflict
	// within the same project.
	CreateTopic(ctx context.Context, topic WebChatTopic) error

	// GetTopic returns a topic by ID. Returns nil if not found or soft-deleted.
	GetTopic(ctx context.Context, topicID string) (*WebChatTopic, error)

	// ListTopics returns all non-deleted topics for a project, ordered by
	// last_activity_at DESC. If no #general topic exists, one is lazily
	// created (covers pre-existing projects).
	ListTopics(ctx context.Context, projectID string) ([]WebChatTopic, error)

	// UpdateTopic applies partial updates (rename, set/clear default_agent).
	UpdateTopic(ctx context.Context, topicID string, updates TopicUpdate) error

	// DeleteTopic soft-deletes a topic (sets deleted_at). Returns an error
	// if the topic is #general.
	DeleteTopic(ctx context.Context, topicID string) error

	// TouchTopicActivity updates last_message_id and last_activity_at.
	TouchTopicActivity(ctx context.Context, topicID, messageID string) error

	// EnsureGeneralTopic idempotently creates the #general topic for a project.
	// Returns the topic ID (existing or new) and a boolean indicating
	// whether a new topic was actually created (false when the topic
	// already existed and the INSERT was a no-op).
	EnsureGeneralTopic(ctx context.Context, projectID, createdBy string) (string, bool, error)

	// --- Wave-2 Read-state methods ---

	// GetReadState returns the read state for a user+conversation pair.
	// Returns nil if no row exists.
	GetReadState(ctx context.Context, userID, conversationKey string) (*WebChatReadState, error)

	// SetReadState upserts the read watermark for a user+conversation pair.
	SetReadState(ctx context.Context, userID, conversationKey, messageID string) error

	// GetReadStates returns read states for a user across multiple conversations.
	GetReadStates(ctx context.Context, userID string, conversationKeys []string) ([]WebChatReadState, error)

	// SetPinned updates the pinned flag for a user+conversation pair.
	SetPinned(ctx context.Context, userID, conversationKey string, pinned bool) error

	// SetMuted updates the muted flag for a user+conversation pair.
	SetMuted(ctx context.Context, userID, conversationKey string, muted bool) error

	// IsConversationMuted returns whether the given user has muted the
	// conversation identified by conversationKey. Returns false if no
	// read-state row exists (unmuted by default).
	IsConversationMuted(ctx context.Context, userID, conversationKey string) (bool, error)

	// --- Wave-2 User-prefs methods ---

	// GetUserPrefs returns the user's rail preferences.
	// Returns defaults (activity sort) if no row exists.
	GetUserPrefs(ctx context.Context, userID string) (*WebChatUserPrefs, error)

	// SetUserPrefs upserts the user's rail preferences.
	SetUserPrefs(ctx context.Context, userID string, prefs WebChatUserPrefs) error

	// --- Wave-2 DM methods ---

	// UpsertDM upserts a single participant row for a DM conversation.
	// Callers must invoke this once per participant (typically twice per
	// DM — one row per side).
	UpsertDM(ctx context.Context, dm WebChatDM) error

	// ListDMs returns all DM conversations for a participant.
	ListDMs(ctx context.Context, participantID string) ([]WebChatDM, error)

	// TouchDMActivity updates the last_message_id and last_activity_at
	// watermarks for a DM conversation (both participant rows).
	TouchDMActivity(ctx context.Context, conversationKey, messageID string) error

	// --- Wave-2 Search methods ---

	// SearchChatMessages performs a case-insensitive substring search across
	// web-channel messages. Results are ordered by created_at DESC with
	// keyset pagination via (created_at, id) cursor.
	SearchChatMessages(ctx context.Context, filter ChatSearchFilter) ([]ChatSearchResult, string, error)
}

// ThreadPrefs holds per-thread display preferences from webchat_thread_prefs.
type ThreadPrefs struct {
	VisibilityMode string `json:"visibility_mode"`
}

// WebChatThread is a single row from the webchat_thread table,
// returned by GetThreads.
type WebChatThread struct {
	AgentID        string
	LastMessageID  string
	LastActivityAt time.Time
	LastReadAt     *time.Time // nil if never read
}

// ---------------------------------------------------------------------------
// Wave-2 types (new tables: webchat_topic, webchat_read_state,
// webchat_user_prefs, webchat_dm)
// ---------------------------------------------------------------------------

// WebChatTopic represents a shared thread entity within a space (project).
// One row per thread; the #general topic has IsGeneral = true.
type WebChatTopic struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	Name           string     `json:"name"`
	IsGeneral      bool       `json:"isGeneral"`
	DefaultAgent   string     `json:"defaultAgent,omitempty"` // empty = no default
	CreatedBy      string     `json:"createdBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	LastMessageID  string     `json:"lastMessageId,omitempty"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	DeletedAt      *time.Time `json:"deletedAt,omitempty"` // nil = not deleted
}

// TopicUpdate carries optional updates for a topic.
// nil pointer fields mean "no change"; a non-nil pointer to an empty string
// means "clear the value".
type TopicUpdate struct {
	Name         *string // nil = no change
	DefaultAgent *string // nil = no change, pointer to empty = clear
}

// WebChatReadState holds per-user, per-conversation state.
// conversation_key is a thread UUID or dm:... key from the design doc.
type WebChatReadState struct {
	UserID            string
	ConversationKey   string
	LastReadMessageID string
	LastReadAt        time.Time
	Pinned            bool
	Muted             bool
}

// WebChatUserPrefs holds per-user rail preferences.
type WebChatUserPrefs struct {
	UserID         string
	SpaceSortMode  string // "activity", "alpha", "custom"
	SpaceOrder     string // JSON array of project UUIDs
	ThreadSortMode string // "activity", "alpha"
}

// WebChatDM represents one side of a DM conversation.
// Two rows exist per DM — one per participant.
type WebChatDM struct {
	ConversationKey string
	ParticipantID   string
	PeerID          string
	PeerKind        string // "user" or "agent"
	LastMessageID   string
	LastActivityAt  time.Time
}

// ChatSearchFilter defines query parameters for searching chat messages.
type ChatSearchFilter struct {
	Query           string   // search text (required, min 2 chars)
	ProjectID       string   // scope to one project (optional)
	ConversationKey string   // scope to one conversation (optional)
	ProjectIDs      []string // scope to visible projects (for "all" search)
	Limit           int      // max results (default 50)
	Cursor          string   // keyset pagination cursor (base64-encoded "timestamp|id")
}

// ChatSearchResult represents a single search result.
type ChatSearchResult struct {
	MessageID       string    `json:"messageId"`
	ConversationKey string    `json:"conversationKey"`
	ThreadName      string    `json:"threadName"`
	SenderName      string    `json:"senderName"`
	Content         string    `json:"content"`
	Snippet         string    `json:"snippet"`
	Timestamp       time.Time `json:"timestamp"`
	ProjectID       string    `json:"projectId"`
}

// NewWebChatStore creates a new WebChatStore backed by the given database.
// The driverName selects the SQL dialect: "postgres" or "pgx" for Postgres,
// anything else (including "" and "sqlite") for SQLite.
func NewWebChatStore(db *sql.DB, driverName string) WebChatStore {
	switch driverName {
	case "postgres", "pgx":
		return &pgWebChatStore{db: db}
	default:
		return &sqliteWebChatStore{db: db}
	}
}

// ---------------------------------------------------------------------------
// SQLite implementation
// ---------------------------------------------------------------------------

// sqliteWebChatStore implements WebChatStore for SQLite.
// Uses ? placeholders and SQLite-appropriate DDL types (TEXT, INTEGER).
type sqliteWebChatStore struct {
	db *sql.DB
}

// DefaultMigrationBatchSize is the number of rows updated per batch
// during the thread_id backfill migration.
const DefaultMigrationBatchSize = 1000

// Init creates the webchat_* tables using SQLite DDL conventions,
// then runs any pending idempotent migrations.
func (s *sqliteWebChatStore) Init() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS webchat_thread (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_message_id TEXT,
    last_activity_at TEXT,
    last_read_at TEXT,
    PRIMARY KEY (user_id, project_id, agent_id)
);

CREATE TABLE IF NOT EXISTS webchat_conversation_context (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    last_channel TEXT,
    last_message_at TEXT,
    PRIMARY KEY (user_id, project_id, agent_id)
);

CREATE TABLE IF NOT EXISTS webchat_thread_prefs (
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    visibility_mode TEXT DEFAULT 'conversation',
    show_state_changes INTEGER DEFAULT 0,
    show_agent_to_agent INTEGER DEFAULT 0,
    muted INTEGER DEFAULT 0,
    PRIMARY KEY (user_id, project_id, agent_id)
);

-- Wave-2 tables

CREATE TABLE IF NOT EXISTS webchat_topic (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    name          TEXT NOT NULL,
    is_general    INTEGER NOT NULL DEFAULT 0,
    default_agent TEXT,
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_message_id TEXT,
    last_activity_at TEXT,
    deleted_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_webchat_topic_project_activity
    ON webchat_topic (project_id, deleted_at, last_activity_at);

CREATE TABLE IF NOT EXISTS webchat_read_state (
    user_id          TEXT NOT NULL,
    conversation_key TEXT NOT NULL,
    last_read_message_id TEXT,
    last_read_at     TEXT,
    pinned           INTEGER NOT NULL DEFAULT 0,
    muted            INTEGER NOT NULL DEFAULT 0,
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
    last_activity_at TEXT,
    PRIMARY KEY (participant_id, conversation_key)
);

CREATE TABLE IF NOT EXISTS webchat_migrations (
    name         TEXT PRIMARY KEY,
    completed_at TEXT
);
`
	_, err := s.db.Exec(ddl)
	if err != nil {
		return fmt.Errorf("webchat store: create tables: %w", err)
	}

	// Create partial unique index for #general (one per project).
	// SQLite supports partial indexes with WHERE.
	const generalIdx = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_one_general
    ON webchat_topic (project_id) WHERE is_general = 1 AND deleted_at IS NULL;
`
	if _, err := s.db.Exec(generalIdx); err != nil {
		return fmt.Errorf("webchat store: create general index: %w", err)
	}

	// Enforce case-insensitive unique topic name per project (excluding
	// soft-deleted topics). COLLATE NOCASE makes the index compare names
	// case-insensitively, matching the Postgres LOWER(name) index.
	const nameIdx = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_project_name
    ON webchat_topic (project_id, name COLLATE NOCASE) WHERE deleted_at IS NULL;
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
func (s *sqliteWebChatStore) TouchThread(ctx context.Context, userID, projectID, agentID, messageID string, activityAt time.Time) error {
	const query = `
INSERT INTO webchat_thread (user_id, project_id, agent_id, last_message_id, last_activity_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id, project_id, agent_id)
DO UPDATE SET
    last_message_id = excluded.last_message_id,
    last_activity_at = excluded.last_activity_at
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID, messageID, activityAt)
	if err != nil {
		return fmt.Errorf("webchat store: touch thread: %w", err)
	}
	return nil
}

// RecordChannel upserts the reply-affinity context for the given (user, project, agent) triple.
func (s *sqliteWebChatStore) RecordChannel(ctx context.Context, userID, projectID, agentID, channel string, messageAt time.Time) error {
	const query = `
INSERT INTO webchat_conversation_context (user_id, project_id, agent_id, last_channel, last_message_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id, project_id, agent_id)
DO UPDATE SET
    last_channel = excluded.last_channel,
    last_message_at = excluded.last_message_at
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID, channel, messageAt)
	if err != nil {
		return fmt.Errorf("webchat store: record channel: %w", err)
	}
	return nil
}

// GetLastChannel returns the last channel for (user, project, agent), or "" if no row exists.
func (s *sqliteWebChatStore) GetLastChannel(ctx context.Context, userID, projectID, agentID string) (string, error) {
	const query = `SELECT last_channel FROM webchat_conversation_context WHERE user_id = ? AND project_id = ? AND agent_id = ?`
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
func (s *sqliteWebChatStore) GetThreadPrefs(ctx context.Context, userID, projectID, agentID string) (ThreadPrefs, error) {
	const query = `SELECT visibility_mode FROM webchat_thread_prefs WHERE user_id = ? AND project_id = ? AND agent_id = ?`
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
func (s *sqliteWebChatStore) SetThreadPrefs(ctx context.Context, userID, projectID, agentID string, prefs ThreadPrefs) error {
	const query = `
INSERT INTO webchat_thread_prefs (user_id, project_id, agent_id, visibility_mode)
VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, project_id, agent_id)
DO UPDATE SET visibility_mode = excluded.visibility_mode
`
	_, err := s.db.ExecContext(ctx, query, userID, projectID, agentID, prefs.VisibilityMode)
	if err != nil {
		return fmt.Errorf("webchat store: set thread prefs: %w", err)
	}
	return nil
}

// GetThreads returns thread watermarks for the given user and project.
func (s *sqliteWebChatStore) GetThreads(ctx context.Context, userID, projectID string, limit int) ([]WebChatThread, error) {
	const query = `
SELECT agent_id, COALESCE(last_message_id, ''), COALESCE(last_activity_at, ''), last_read_at
  FROM webchat_thread
 WHERE user_id = ? AND project_id = ?
 ORDER BY last_activity_at DESC
 LIMIT ?
`
	rows, err := s.db.QueryContext(ctx, query, userID, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get threads: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var threads []WebChatThread
	for rows.Next() {
		var t WebChatThread
		var activityStr string
		var readStr *string
		if err := rows.Scan(&t.AgentID, &t.LastMessageID, &activityStr, &readStr); err != nil {
			return nil, fmt.Errorf("webchat store: scan thread: %w", err)
		}
		if activityStr != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, activityStr); err == nil {
				t.LastActivityAt = parsed
			} else if parsed, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", activityStr); err == nil {
				t.LastActivityAt = parsed
			} else if parsed, err := time.Parse("2006-01-02 15:04:05.999999999", activityStr); err == nil {
				t.LastActivityAt = parsed
			}
		}
		if readStr != nil && *readStr != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, *readStr); err == nil {
				t.LastReadAt = &parsed
			} else if parsed, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", *readStr); err == nil {
				t.LastReadAt = &parsed
			} else if parsed, err := time.Parse("2006-01-02 15:04:05.999999999", *readStr); err == nil {
				t.LastReadAt = &parsed
			}
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}

// MarkThreadRead advances the last_read_at watermark to now.
func (s *sqliteWebChatStore) MarkThreadRead(ctx context.Context, userID, projectID, agentID string) error {
	const query = `
UPDATE webchat_thread
   SET last_read_at = ?
 WHERE user_id = ? AND project_id = ? AND agent_id = ?
`
	_, err := s.db.ExecContext(ctx, query, time.Now().UTC().Format(time.RFC3339Nano), userID, projectID, agentID)
	if err != nil {
		return fmt.Errorf("webchat store: mark thread read: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave-2 Topic methods (SQLite)
// ---------------------------------------------------------------------------

// CreateTopic inserts a new topic. Returns an error on name conflict.
func (s *sqliteWebChatStore) CreateTopic(ctx context.Context, topic WebChatTopic) error {
	const query = `
INSERT INTO webchat_topic (id, project_id, name, is_general, default_agent, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
`
	isGeneral := 0
	if topic.IsGeneral {
		isGeneral = 1
	}
	_, err := s.db.ExecContext(ctx, query, topic.ID, topic.ProjectID, topic.Name,
		isGeneral, nullableString(topic.DefaultAgent), topic.CreatedBy,
		topic.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("webchat store: create topic: %w", err)
	}
	return nil
}

// GetTopic returns a topic by ID. Returns nil if not found or soft-deleted.
func (s *sqliteWebChatStore) GetTopic(ctx context.Context, topicID string) (*WebChatTopic, error) {
	const query = `
SELECT id, project_id, name, is_general, COALESCE(default_agent, ''),
       created_by, created_at, COALESCE(last_message_id, ''),
       COALESCE(last_activity_at, ''), deleted_at
  FROM webchat_topic
 WHERE id = ? AND deleted_at IS NULL
`
	var t WebChatTopic
	var isGeneral int
	var createdAtStr, activityStr string
	var deletedAtStr *string
	err := s.db.QueryRowContext(ctx, query, topicID).Scan(
		&t.ID, &t.ProjectID, &t.Name, &isGeneral, &t.DefaultAgent,
		&t.CreatedBy, &createdAtStr, &t.LastMessageID, &activityStr, &deletedAtStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("webchat store: get topic: %w", err)
	}
	t.IsGeneral = isGeneral != 0
	t.CreatedAt = parseSQLiteTime(createdAtStr)
	t.LastActivityAt = parseSQLiteTime(activityStr)
	if deletedAtStr != nil && *deletedAtStr != "" {
		parsed := parseSQLiteTime(*deletedAtStr)
		t.DeletedAt = &parsed
	}
	return &t, nil
}

// ListTopics returns non-deleted topics for a project, ordered by last_activity_at DESC.
// Lazily creates #general if none exists.
func (s *sqliteWebChatStore) ListTopics(ctx context.Context, projectID string) ([]WebChatTopic, error) {
	const query = `
SELECT id, project_id, name, is_general, COALESCE(default_agent, ''),
       created_by, created_at, COALESCE(last_message_id, ''),
       COALESCE(last_activity_at, ''), deleted_at
  FROM webchat_topic
 WHERE project_id = ? AND deleted_at IS NULL
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
		var isGeneral int
		var createdAtStr, activityStr string
		var deletedAtStr *string
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Name, &isGeneral, &t.DefaultAgent,
			&t.CreatedBy, &createdAtStr, &t.LastMessageID, &activityStr, &deletedAtStr); err != nil {
			return nil, fmt.Errorf("webchat store: scan topic: %w", err)
		}
		t.IsGeneral = isGeneral != 0
		t.CreatedAt = parseSQLiteTime(createdAtStr)
		t.LastActivityAt = parseSQLiteTime(activityStr)
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
func (s *sqliteWebChatStore) UpdateTopic(ctx context.Context, topicID string, updates TopicUpdate) error {
	var sets []string
	var args []interface{}

	if updates.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *updates.Name)
	}
	if updates.DefaultAgent != nil {
		sets = append(sets, "default_agent = ?")
		args = append(args, nullableString(*updates.DefaultAgent))
	}
	if len(sets) == 0 {
		return nil
	}

	args = append(args, topicID)
	query := fmt.Sprintf("UPDATE webchat_topic SET %s WHERE id = ? AND deleted_at IS NULL",
		strings.Join(sets, ", "))
	_, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("webchat store: update topic: %w", err)
	}
	return nil
}

// DeleteTopic soft-deletes a topic. Returns an error if it is #general.
func (s *sqliteWebChatStore) DeleteTopic(ctx context.Context, topicID string) error {
	// Check if topic is #general.
	var isGeneral int
	err := s.db.QueryRowContext(ctx, "SELECT is_general FROM webchat_topic WHERE id = ?", topicID).Scan(&isGeneral)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("webchat store: delete topic check: %w", err)
	}
	if isGeneral != 0 {
		return fmt.Errorf("webchat store: delete topic: cannot delete #general topic")
	}

	const query = `UPDATE webchat_topic SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`
	_, err = s.db.ExecContext(ctx, query, time.Now().UTC().Format(time.RFC3339Nano), topicID)
	if err != nil {
		return fmt.Errorf("webchat store: delete topic: %w", err)
	}
	return nil
}

// TouchTopicActivity updates last_activity_at and, when messageID is
// non-empty, also updates last_message_id. An empty messageID is
// accepted gracefully — this happens when the spoke receives a
// StructuredMessage (which has no ID) rather than a store.Message.
func (s *sqliteWebChatStore) TouchTopicActivity(ctx context.Context, topicID, messageID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if messageID != "" {
		const q = `UPDATE webchat_topic SET last_message_id = ?, last_activity_at = ? WHERE id = ?`
		_, err := s.db.ExecContext(ctx, q, messageID, now, topicID)
		if err != nil {
			return fmt.Errorf("webchat store: touch topic activity: %w", err)
		}
		return nil
	}
	const q = `UPDATE webchat_topic SET last_activity_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, q, now, topicID)
	if err != nil {
		return fmt.Errorf("webchat store: touch topic activity: %w", err)
	}
	return nil
}

// EnsureGeneralTopic idempotently creates the #general topic for a project.
// Returns the topic ID (existing or new) and whether a new row was inserted.
func (s *sqliteWebChatStore) EnsureGeneralTopic(ctx context.Context, projectID, createdBy string) (string, bool, error) {
	newID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	const insert = `
INSERT INTO webchat_topic (id, project_id, name, is_general, created_by, created_at)
VALUES (?, ?, 'general', 1, ?, ?)
ON CONFLICT DO NOTHING
`
	_, err := s.db.ExecContext(ctx, insert, newID, projectID, createdBy, now)
	if err != nil {
		return "", false, fmt.Errorf("webchat store: ensure general topic: %w", err)
	}

	// Return the ID of the existing or newly created #general topic.
	// If the returned ID matches the one we tried to insert, we created
	// a new row; otherwise, the topic already existed.
	const lookup = `SELECT id FROM webchat_topic WHERE project_id = ? AND is_general = 1 AND deleted_at IS NULL`
	var id string
	err = s.db.QueryRowContext(ctx, lookup, projectID).Scan(&id)
	if err != nil {
		return "", false, fmt.Errorf("webchat store: ensure general topic lookup: %w", err)
	}
	return id, id == newID, nil
}

// ---------------------------------------------------------------------------
// Wave-2 Read-state methods (SQLite)
// ---------------------------------------------------------------------------

// GetReadState returns the read state for a user+conversation pair.
func (s *sqliteWebChatStore) GetReadState(ctx context.Context, userID, conversationKey string) (*WebChatReadState, error) {
	const query = `
SELECT user_id, conversation_key, COALESCE(last_read_message_id, ''),
       COALESCE(last_read_at, ''), pinned, muted
  FROM webchat_read_state
 WHERE user_id = ? AND conversation_key = ?
`
	var rs WebChatReadState
	var lastReadAtStr string
	var pinned, muted int
	err := s.db.QueryRowContext(ctx, query, userID, conversationKey).Scan(
		&rs.UserID, &rs.ConversationKey, &rs.LastReadMessageID,
		&lastReadAtStr, &pinned, &muted)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("webchat store: get read state: %w", err)
	}
	rs.LastReadAt = parseSQLiteTime(lastReadAtStr)
	rs.Pinned = pinned != 0
	rs.Muted = muted != 0
	return &rs, nil
}

// SetReadState upserts the read watermark.
func (s *sqliteWebChatStore) SetReadState(ctx context.Context, userID, conversationKey, messageID string) error {
	const query = `
INSERT INTO webchat_read_state (user_id, conversation_key, last_read_message_id, last_read_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, conversation_key)
DO UPDATE SET
    last_read_message_id = excluded.last_read_message_id,
    last_read_at = excluded.last_read_at
`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, query, userID, conversationKey, messageID, now)
	if err != nil {
		return fmt.Errorf("webchat store: set read state: %w", err)
	}
	return nil
}

// GetReadStates returns read states for a user across multiple conversations.
func (s *sqliteWebChatStore) GetReadStates(ctx context.Context, userID string, conversationKeys []string) ([]WebChatReadState, error) {
	if len(conversationKeys) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(conversationKeys))
	args := make([]interface{}, 0, len(conversationKeys)+1)
	args = append(args, userID)
	for i, key := range conversationKeys {
		placeholders[i] = "?"
		args = append(args, key)
	}

	query := fmt.Sprintf(`
SELECT user_id, conversation_key, COALESCE(last_read_message_id, ''),
       COALESCE(last_read_at, ''), pinned, muted
  FROM webchat_read_state
 WHERE user_id = ? AND conversation_key IN (%s)
`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("webchat store: get read states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var states []WebChatReadState
	for rows.Next() {
		var rs WebChatReadState
		var lastReadAtStr string
		var pinned, muted int
		if err := rows.Scan(&rs.UserID, &rs.ConversationKey, &rs.LastReadMessageID,
			&lastReadAtStr, &pinned, &muted); err != nil {
			return nil, fmt.Errorf("webchat store: scan read state: %w", err)
		}
		rs.LastReadAt = parseSQLiteTime(lastReadAtStr)
		rs.Pinned = pinned != 0
		rs.Muted = muted != 0
		states = append(states, rs)
	}
	return states, rows.Err()
}

// SetPinned updates the pinned flag.
func (s *sqliteWebChatStore) SetPinned(ctx context.Context, userID, conversationKey string, pinned bool) error {
	pinnedInt := 0
	if pinned {
		pinnedInt = 1
	}
	const query = `
INSERT INTO webchat_read_state (user_id, conversation_key, pinned)
VALUES (?, ?, ?)
ON CONFLICT (user_id, conversation_key)
DO UPDATE SET pinned = excluded.pinned
`
	_, err := s.db.ExecContext(ctx, query, userID, conversationKey, pinnedInt)
	if err != nil {
		return fmt.Errorf("webchat store: set pinned: %w", err)
	}
	return nil
}

// SetMuted updates the muted flag.
func (s *sqliteWebChatStore) SetMuted(ctx context.Context, userID, conversationKey string, muted bool) error {
	mutedInt := 0
	if muted {
		mutedInt = 1
	}
	const query = `
INSERT INTO webchat_read_state (user_id, conversation_key, muted)
VALUES (?, ?, ?)
ON CONFLICT (user_id, conversation_key)
DO UPDATE SET muted = excluded.muted
`
	_, err := s.db.ExecContext(ctx, query, userID, conversationKey, mutedInt)
	if err != nil {
		return fmt.Errorf("webchat store: set muted: %w", err)
	}
	return nil
}

// IsConversationMuted returns whether the user has muted the conversation.
// Returns false (unmuted) when no read-state row exists.
func (s *sqliteWebChatStore) IsConversationMuted(ctx context.Context, userID, conversationKey string) (bool, error) {
	const query = `SELECT muted FROM webchat_read_state WHERE user_id = ? AND conversation_key = ?`
	var muted int
	if err := s.db.QueryRowContext(ctx, query, userID, conversationKey).Scan(&muted); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("webchat store: is conversation muted: %w", err)
	}
	return muted != 0, nil
}

// ---------------------------------------------------------------------------
// Wave-2 User-prefs methods (SQLite)
// ---------------------------------------------------------------------------

// GetUserPrefs returns the user's rail preferences. Returns defaults if no row.
func (s *sqliteWebChatStore) GetUserPrefs(ctx context.Context, userID string) (*WebChatUserPrefs, error) {
	const query = `
SELECT user_id, space_sort_mode, COALESCE(space_order, ''), thread_sort_mode
  FROM webchat_user_prefs
 WHERE user_id = ?
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
func (s *sqliteWebChatStore) SetUserPrefs(ctx context.Context, userID string, prefs WebChatUserPrefs) error {
	const query = `
INSERT INTO webchat_user_prefs (user_id, space_sort_mode, space_order, thread_sort_mode)
VALUES (?, ?, ?, ?)
ON CONFLICT (user_id)
DO UPDATE SET
    space_sort_mode = excluded.space_sort_mode,
    space_order = excluded.space_order,
    thread_sort_mode = excluded.thread_sort_mode
`
	_, err := s.db.ExecContext(ctx, query, userID, prefs.SpaceSortMode,
		nullableString(prefs.SpaceOrder), prefs.ThreadSortMode)
	if err != nil {
		return fmt.Errorf("webchat store: set user prefs: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave-2 DM methods (SQLite)
// ---------------------------------------------------------------------------

// UpsertDM upserts a participant row for a DM conversation.
func (s *sqliteWebChatStore) UpsertDM(ctx context.Context, dm WebChatDM) error {
	const query = `
INSERT INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_message_id, last_activity_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (participant_id, conversation_key)
DO UPDATE SET
    peer_id = excluded.peer_id,
    peer_kind = excluded.peer_kind,
    last_message_id = excluded.last_message_id,
    last_activity_at = excluded.last_activity_at
`
	activityAt := ""
	if !dm.LastActivityAt.IsZero() {
		activityAt = dm.LastActivityAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, query, dm.ConversationKey, dm.ParticipantID,
		dm.PeerID, dm.PeerKind, nullableString(dm.LastMessageID), nullableString(activityAt))
	if err != nil {
		return fmt.Errorf("webchat store: upsert dm: %w", err)
	}
	return nil
}

// ListDMs returns all DM conversations for a participant.
func (s *sqliteWebChatStore) ListDMs(ctx context.Context, participantID string) ([]WebChatDM, error) {
	const query = `
SELECT conversation_key, participant_id, peer_id, peer_kind,
       COALESCE(last_message_id, ''), COALESCE(last_activity_at, '')
  FROM webchat_dm
 WHERE participant_id = ?
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
		var activityStr string
		if err := rows.Scan(&dm.ConversationKey, &dm.ParticipantID, &dm.PeerID,
			&dm.PeerKind, &dm.LastMessageID, &activityStr); err != nil {
			return nil, fmt.Errorf("webchat store: scan dm: %w", err)
		}
		dm.LastActivityAt = parseSQLiteTime(activityStr)
		dms = append(dms, dm)
	}
	return dms, rows.Err()
}

// TouchDMActivity updates watermarks for a DM conversation (all participant
// rows). When messageID is empty, only last_activity_at is updated.
func (s *sqliteWebChatStore) TouchDMActivity(ctx context.Context, conversationKey, messageID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if messageID != "" {
		const q = `UPDATE webchat_dm SET last_message_id = ?, last_activity_at = ? WHERE conversation_key = ?`
		_, err := s.db.ExecContext(ctx, q, messageID, now, conversationKey)
		if err != nil {
			return fmt.Errorf("webchat store: touch dm activity: %w", err)
		}
		return nil
	}
	const q = `UPDATE webchat_dm SET last_activity_at = ? WHERE conversation_key = ?`
	_, err := s.db.ExecContext(ctx, q, now, conversationKey)
	if err != nil {
		return fmt.Errorf("webchat store: touch dm activity: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wave-2 Search methods (SQLite)
// ---------------------------------------------------------------------------

// SearchChatMessages performs a case-insensitive substring search over the
// messages table, scoped to channel='web'. SQLite LIKE is case-insensitive
// for ASCII which covers most search queries.
func (s *sqliteWebChatStore) SearchChatMessages(ctx context.Context, filter ChatSearchFilter) ([]ChatSearchResult, string, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 200 {
		filter.Limit = 200
	}

	// Check if the messages table exists.
	var tableExists int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages'").Scan(&tableExists); err != nil || tableExists == 0 {
		return nil, "", nil
	}

	// Build the query dynamically based on filter.
	var conditions []string
	var args []interface{}

	conditions = append(conditions, "channel = 'web'")
	conditions = append(conditions, "msg LIKE '%' || ? || '%'")
	args = append(args, filter.Query)

	if filter.ConversationKey != "" {
		conditions = append(conditions, "thread_id = ?")
		args = append(args, filter.ConversationKey)
	}

	if filter.ProjectID != "" {
		conditions = append(conditions, "project_id = ?")
		args = append(args, filter.ProjectID)
	} else if len(filter.ProjectIDs) > 0 {
		placeholders := make([]string, len(filter.ProjectIDs))
		for i, pid := range filter.ProjectIDs {
			placeholders[i] = "?"
			args = append(args, pid)
		}
		conditions = append(conditions, fmt.Sprintf("project_id IN (%s)", strings.Join(placeholders, ",")))
	}

	// Keyset pagination cursor: "timestamp|id"
	if filter.Cursor != "" {
		cursorParts := strings.SplitN(filter.Cursor, "|", 2)
		if len(cursorParts) == 2 {
			conditions = append(conditions, "(created < ? OR (created = ? AND id < ?))")
			args = append(args, cursorParts[0], cursorParts[0], cursorParts[1])
		}
	}

	query := fmt.Sprintf(`
SELECT id, project_id, COALESCE(thread_id, ''), sender, msg, created
  FROM messages
 WHERE %s
 ORDER BY created DESC, id DESC
 LIMIT ?
`, strings.Join(conditions, " AND "))
	args = append(args, filter.Limit+1) // fetch one extra to detect next page

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("webchat store: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []ChatSearchResult
	for rows.Next() {
		var r ChatSearchResult
		var createdStr string
		if err := rows.Scan(&r.MessageID, &r.ProjectID, &r.ConversationKey, &r.SenderName, &r.Content, &createdStr); err != nil {
			return nil, "", fmt.Errorf("webchat store: scan search result: %w", err)
		}
		r.Timestamp = parseSQLiteTime(createdStr)
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
// Wave-2 Migrations (SQLite)
// ---------------------------------------------------------------------------

// runMigrations executes idempotent data migrations.
func (s *sqliteWebChatStore) runMigrations() error {
	if err := s.migrateThreadIDs(DefaultMigrationBatchSize); err != nil {
		return fmt.Errorf("thread_id backfill: %w", err)
	}
	if err := s.seedFromWave1(); err != nil {
		return fmt.Errorf("wave-1 seed: %w", err)
	}
	return nil
}

// migrationCompleted checks whether a named migration has already run.
func (s *sqliteWebChatStore) migrationCompleted(name string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM webchat_migrations WHERE name = ?", name).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// markMigrationCompleted records a migration as done.
func (s *sqliteWebChatStore) markMigrationCompleted(name string) error {
	const query = `INSERT INTO webchat_migrations (name, completed_at) VALUES (?, ?)`
	_, err := s.db.Exec(query, name, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// migrateThreadIDs backfills thread_id from "agent:<id>" to "dm:agent:<aid>:user:<uid>"
// for web channel messages. Processes in batches to avoid locking.
func (s *sqliteWebChatStore) migrateThreadIDs(batchSize int) error {
	done, err := s.migrationCompleted("thread_id_backfill")
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// Check if the messages table exists (it's Ent-managed and may not exist in tests).
	var tableExists int
	err = s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages'").Scan(&tableExists)
	if err != nil || tableExists == 0 {
		// No messages table — nothing to backfill; mark as complete.
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
            CASE WHEN recipient_id != '' THEN recipient_id ELSE REPLACE(recipient, 'user:', '') END
        ELSE
            CASE WHEN sender_id != '' THEN sender_id ELSE REPLACE(sender, 'user:', '') END
    END
WHERE channel = 'web'
  AND thread_id LIKE 'agent:%'
  AND rowid IN (
      SELECT rowid FROM messages
       WHERE channel = 'web' AND thread_id LIKE 'agent:%'
       LIMIT ?
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
func (s *sqliteWebChatStore) seedFromWave1() error {
	done, err := s.migrationCompleted("wave1_seed")
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	// Seed webchat_dm entries from webchat_thread rows.
	// Each wave-1 thread represents a user↔agent DM.
	// SQLite requires INSERT OR IGNORE for INSERT...SELECT with conflict handling.
	const seedDM = `
INSERT OR IGNORE INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_message_id, last_activity_at)
SELECT
    'dm:agent:' || t.agent_id || ':user:' || t.user_id,
    t.user_id,
    t.agent_id,
    'agent',
    t.last_message_id,
    t.last_activity_at
FROM webchat_thread t
`
	if _, err := s.db.Exec(seedDM); err != nil {
		return fmt.Errorf("seed dm (user side): %w", err)
	}

	// Agent side of the DM.
	const seedDMAgent = `
INSERT OR IGNORE INTO webchat_dm (conversation_key, participant_id, peer_id, peer_kind, last_message_id, last_activity_at)
SELECT
    'dm:agent:' || t.agent_id || ':user:' || t.user_id,
    t.agent_id,
    t.user_id,
    'user',
    t.last_message_id,
    t.last_activity_at
FROM webchat_thread t
`
	if _, err := s.db.Exec(seedDMAgent); err != nil {
		return fmt.Errorf("seed dm (agent side): %w", err)
	}

	// Seed webchat_read_state from webchat_thread watermarks.
	const seedReadState = `
INSERT OR IGNORE INTO webchat_read_state (user_id, conversation_key, last_read_at)
SELECT
    t.user_id,
    'dm:agent:' || t.agent_id || ':user:' || t.user_id,
    t.last_read_at
FROM webchat_thread t
WHERE t.last_read_at IS NOT NULL
`
	if _, err := s.db.Exec(seedReadState); err != nil {
		return fmt.Errorf("seed read state: %w", err)
	}

	// Carry muted flag from webchat_thread_prefs into webchat_read_state.
	// For muted, we need to upsert: create the read_state row if it doesn't
	// exist, or update the muted flag if it does. SQLite INSERT OR REPLACE
	// would lose other columns, so we do a two-step approach.
	const seedMutedInsert = `
INSERT OR IGNORE INTO webchat_read_state (user_id, conversation_key, muted)
SELECT
    p.user_id,
    'dm:agent:' || p.agent_id || ':user:' || p.user_id,
    p.muted
FROM webchat_thread_prefs p
WHERE p.muted = 1
`
	if _, err := s.db.Exec(seedMutedInsert); err != nil {
		return fmt.Errorf("seed muted insert: %w", err)
	}

	// Update existing rows to set muted = 1.
	const seedMutedUpdate = `
UPDATE webchat_read_state SET muted = 1
WHERE (user_id, conversation_key) IN (
    SELECT p.user_id, 'dm:agent:' || p.agent_id || ':user:' || p.user_id
    FROM webchat_thread_prefs p
    WHERE p.muted = 1
)
`
	if _, err := s.db.Exec(seedMutedUpdate); err != nil {
		return fmt.Errorf("seed muted update: %w", err)
	}

	return s.markMigrationCompleted("wave1_seed")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseSQLiteTime parses a timestamp string stored by SQLite in multiple
// possible formats (mirrors the wave-1 parsing in GetThreads).
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return parsed
	}
	if parsed, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err == nil {
		return parsed
	}
	if parsed, err := time.Parse("2006-01-02 15:04:05.999999999", s); err == nil {
		return parsed
	}
	return time.Time{}
}

// nullableString returns nil for empty strings, suitable for nullable TEXT columns.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// generateSnippet extracts a snippet around the first case-insensitive match
// of query in content, with approximately contextLen characters of context.
// The match is wrapped in <mark> tags for client-side highlighting.
func generateSnippet(content, query string, contextLen int) string {
	if query == "" || content == "" {
		return truncateSnippet(content, contextLen)
	}

	contentRunes := []rune(content)
	queryRunes := []rune(strings.ToLower(query))
	lowerRunes := []rune(strings.ToLower(content))

	// Find the first match position (rune-based).
	matchStart := -1
	for i := 0; i <= len(lowerRunes)-len(queryRunes); i++ {
		found := true
		for j := 0; j < len(queryRunes); j++ {
			if lowerRunes[i+j] != queryRunes[j] {
				found = false
				break
			}
		}
		if found {
			matchStart = i
			break
		}
	}

	if matchStart < 0 {
		return truncateSnippet(content, contextLen)
	}

	matchEnd := matchStart + len(queryRunes)

	// Calculate window around the match.
	halfCtx := contextLen / 2
	start := matchStart - halfCtx
	if start < 0 {
		start = 0
	}
	end := matchEnd + halfCtx
	if end > len(contentRunes) {
		end = len(contentRunes)
	}

	// Build snippet with <mark> tags.
	var sb strings.Builder
	if start > 0 {
		sb.WriteString("...")
	}
	sb.WriteString(string(contentRunes[start:matchStart]))
	sb.WriteString("<mark>")
	sb.WriteString(string(contentRunes[matchStart:matchEnd]))
	sb.WriteString("</mark>")
	sb.WriteString(string(contentRunes[matchEnd:end]))
	if end < len(contentRunes) {
		sb.WriteString("...")
	}

	return sb.String()
}

// truncateSnippet truncates content to maxLen runes for display.
func truncateSnippet(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
