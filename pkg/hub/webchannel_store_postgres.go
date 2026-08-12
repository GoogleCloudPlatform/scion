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

// pgWebChatStore implements WebChatStore for Postgres.
// Uses $N placeholders and Postgres-appropriate DDL types (TIMESTAMPTZ, BOOLEAN).
// This mirrors the Discord store split
// (extras/scion-discord/internal/discord/store_postgres.go).
type pgWebChatStore struct {
	db *sql.DB
}

// Init creates the webchat_* tables using Postgres DDL conventions.
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
`
	_, err := s.db.Exec(ddl)
	if err != nil {
		return fmt.Errorf("webchat store: create tables: %w", err)
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
