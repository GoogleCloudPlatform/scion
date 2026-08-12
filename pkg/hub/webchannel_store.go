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
	"time"
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

	// GetThreadPrefs returns the display preferences for a (user, project, agent) thread.
	// Returns default prefs (visibility_mode = "conversation") if no row exists.
	GetThreadPrefs(ctx context.Context, userID, projectID, agentID string) (ThreadPrefs, error)

	// SetThreadPrefs upserts the display preferences for a (user, project, agent) thread.
	SetThreadPrefs(ctx context.Context, userID, projectID, agentID string, prefs ThreadPrefs) error
}

// ThreadPrefs holds per-thread display preferences from webchat_thread_prefs.
type ThreadPrefs struct {
	VisibilityMode string `json:"visibility_mode"`
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

// Init creates the webchat_* tables using SQLite DDL conventions.
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
`
	_, err := s.db.Exec(ddl)
	if err != nil {
		return fmt.Errorf("webchat store: create tables: %w", err)
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
