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

package hubtestutil

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ConversationsTableDDL is the schema for the Ent-managed conversations table,
// reproduced here for testing the dual-write paths that depend on it.
const ConversationsTableDDL = `
CREATE TABLE IF NOT EXISTS conversations (
    id              TEXT PRIMARY KEY,
    project_id      TEXT,
    kind            TEXT NOT NULL,
    surface         TEXT NOT NULL,
    external_ref    TEXT NOT NULL DEFAULT '',
    parent_ref      TEXT NOT NULL DEFAULT '',
    display_name    TEXT NOT NULL DEFAULT '',
    drift_state     TEXT NOT NULL DEFAULT 'active',
    default_agent_id TEXT,
    last_activity_at TEXT,
    created_at      TEXT,
    archived_at     TEXT,
    deleted_at      TEXT
)
`

// GetTopicConvID reads conversation_id directly from the webchat_topic table.
func GetTopicConvID(t *testing.T, db *sql.DB, topicID string) string {
	t.Helper()
	var convID string
	err := db.QueryRow("SELECT COALESCE(conversation_id, '') FROM webchat_topic WHERE id = ?", topicID).Scan(&convID)
	require.NoError(t, err)
	return convID
}

// RawText runs query, which must return one row with one column, and
// returns that column as a string.
func RawText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(query, args...).Scan(&s))
	return s
}

// RequireUTCInstant asserts that got is the same instant as want and is in
// UTC.
func RequireUTCInstant(t *testing.T, got, want time.Time) {
	t.Helper()
	assert.True(t, got.Equal(want), "got %v, want %v", got, want)
	assert.Equal(t, time.UTC, got.Location(), "got %v", got)
}

// RequirePostgresDSN returns SCION_TEST_POSTGRES_DSN, and skips the test
// when it is not set.
func RequirePostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run Postgres webchat store tests")
	}
	return dsn
}
