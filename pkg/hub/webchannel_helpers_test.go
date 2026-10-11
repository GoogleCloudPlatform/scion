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
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// conversationsTableDDL is the schema for the Ent-managed conversations table,
// reproduced here for testing the dual-write paths that depend on it.
const conversationsTableDDL = `
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

// newTestWebChatStoreWithConversations creates a WebChatStore backed by an
// in-memory SQLite DB that includes the conversations table (Ent-managed in
// production). This enables testing the dual-write paths that require
// hasConversationsTable() to return true.
func newTestWebChatStoreWithConversations(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite3")

	// Create the conversations table BEFORE Init so migrations can see it.
	_, err := db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	return s, db
}

// newPromoteTestStoreWithConversations creates a WebChatStore with in-memory
// SQLite DB that includes both the messages table AND the conversations table.
func newPromoteTestStoreWithConversations(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite3")

	// Create the Ent messages table manually.
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL,
    sender_id TEXT NOT NULL DEFAULT '',
    recipient TEXT NOT NULL,
    recipient_id TEXT NOT NULL DEFAULT '',
    channel TEXT,
    thread_id TEXT,
    conversation_id TEXT NOT NULL DEFAULT '',
    msg TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL DEFAULT 'chat',
    dispatch_state TEXT NOT NULL DEFAULT 'dispatched',
    created TEXT NOT NULL DEFAULT ''
)
`)
	require.NoError(t, err)

	// Create the conversations table.
	_, err = db.Exec(conversationsTableDDL)
	require.NoError(t, err)

	s := NewWebChatStore(db, "sqlite3")
	require.NoError(t, s.Init())

	return s, db
}

// getConversation reads a single conversations row by ID, returning nil if not found.
func getConversation(t *testing.T, db *sql.DB, convID string) *conversationRow {
	t.Helper()
	var c conversationRow
	err := db.QueryRow(
		`SELECT id, COALESCE(project_id,''), kind, surface, COALESCE(display_name,''), COALESCE(drift_state,'')
		   FROM conversations WHERE id = ?`, convID).
		Scan(&c.id, &c.projectID, &c.kind, &c.surface, &c.displayName, &c.driftState)
	if err == sql.ErrNoRows {
		return nil
	}
	require.NoError(t, err)
	return &c
}

// getTopicConvID reads conversation_id directly from the webchat_topic table.
func getTopicConvID(t *testing.T, db *sql.DB, topicID string) string {
	t.Helper()
	var convID string
	err := db.QueryRow("SELECT COALESCE(conversation_id, '') FROM webchat_topic WHERE id = ?", topicID).Scan(&convID)
	require.NoError(t, err)
	return convID
}

// newTestWebChatStoreV2 creates a WebChatStore backed by an in-memory SQLite DB
// for testing wave-2 features. The caller should close the returned *sql.DB.
func newTestWebChatStoreV2(t *testing.T) (WebChatStore, *sql.DB) {
	t.Helper()
	db := openTestMemorySQLite(t, "sqlite3")

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init())

	return store, db
}

// conversationRow holds the relevant columns from a conversations row for test assertions.
type conversationRow struct {
	id, projectID, kind, surface, displayName, driftState string
}
