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

//go:build !no_sqlite

package hub

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// preExistingSchemaSQL creates the webchat tables as they existed before
// commit eb365a9d3 (#1380, tranche C4) — webchat_topic has ten columns and
// no conversation_id column. This mirrors the actual state on the scion-gteam
// staging VM.
const preExistingSQLiteSchemaSQL = `
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

CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_one_general
    ON webchat_topic (project_id) WHERE is_general = 1 AND deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_project_name
    ON webchat_topic (project_id, name COLLATE NOCASE) WHERE deleted_at IS NULL;

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

CREATE TABLE IF NOT EXISTS webchat_attachment (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    filename    TEXT NOT NULL,
    mime_type   TEXT NOT NULL,
    size        INTEGER NOT NULL,
    uploaded_by TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webchat_attachment_project
    ON webchat_attachment (project_id);

CREATE TABLE IF NOT EXISTS webchat_message_attachment (
    message_id    TEXT NOT NULL,
    attachment_id TEXT NOT NULL,
    PRIMARY KEY (message_id, attachment_id)
);

CREATE INDEX IF NOT EXISTS idx_webchat_message_attachment_message
    ON webchat_message_attachment (message_id);

CREATE TABLE IF NOT EXISTS webchat_message_ext (
    message_id TEXT PRIMARY KEY,
    reply_to_id TEXT,
    edited_at TEXT,
    deleted_at TEXT
);

-- Record the three migrations that existed before #1380.
INSERT INTO webchat_migrations (name, completed_at) VALUES ('thread_id_backfill', '2026-08-17T00:00:00Z');
INSERT INTO webchat_migrations (name, completed_at) VALUES ('wave1_seed', '2026-08-17T00:00:00Z');
INSERT INTO webchat_migrations (name, completed_at) VALUES ('thread_id_index', '2026-08-24T00:00:00Z');
`

// sqliteColumnExists checks whether a column exists in a table.
func sqliteColumnExists(db *sql.DB, table, column string) bool {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt *string
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

// sqliteIndexExists checks whether a named index exists in the database.
func sqliteIndexExists(db *sql.DB, indexName string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?",
		indexName,
	).Scan(&count)
	return err == nil && count > 0
}

// sqliteMigrationRecorded checks whether a migration name is present in webchat_migrations.
func sqliteMigrationRecorded(db *sql.DB, name string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM webchat_migrations WHERE name = ?",
		name,
	).Scan(&count)
	return err == nil && count > 0
}

// --- SQLite tests ---

// TestC4Fix_SQLite_FreshDB verifies that on a brand-new database, Init
// succeeds and the conversation_id column and unique index both exist.
func TestC4Fix_SQLite_FreshDB(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init(), "Init on fresh DB must succeed")

	require.True(t, sqliteColumnExists(db, "webchat_topic", "conversation_id"),
		"conversation_id column must exist after fresh Init")
	require.True(t, sqliteIndexExists(db, "idx_webchat_topic_conversation"),
		"idx_webchat_topic_conversation must exist after fresh Init")
	require.True(t, sqliteMigrationRecorded(db, "topic_conversation_id"),
		"topic_conversation_id migration must be recorded after fresh Init")
}

// TestC4Fix_SQLite_PreExistingDB is the regression test for the bug fixed
// by this change. It creates a database matching the actual schema on the
// scion-gteam staging VM (ten-column webchat_topic without conversation_id,
// three prior migrations recorded). Init must succeed, add the column, create
// the index, and record the migration.
func TestC4Fix_SQLite_PreExistingDB(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Seed the DB with the pre-existing schema (no conversation_id column).
	_, err = db.Exec(preExistingSQLiteSchemaSQL)
	require.NoError(t, err, "seeding pre-existing schema must succeed")

	// Verify the column does NOT exist before Init.
	require.False(t, sqliteColumnExists(db, "webchat_topic", "conversation_id"),
		"precondition: conversation_id must not exist before Init")
	require.False(t, sqliteIndexExists(db, "idx_webchat_topic_conversation"),
		"precondition: idx_webchat_topic_conversation must not exist before Init")

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init(), "Init on pre-existing DB must succeed")

	require.True(t, sqliteColumnExists(db, "webchat_topic", "conversation_id"),
		"conversation_id column must exist after Init on pre-existing DB")
	require.True(t, sqliteIndexExists(db, "idx_webchat_topic_conversation"),
		"idx_webchat_topic_conversation must exist after Init on pre-existing DB")
	require.True(t, sqliteMigrationRecorded(db, "topic_conversation_id"),
		"topic_conversation_id migration must be recorded after Init on pre-existing DB")
}

// TestC4Fix_SQLite_Idempotent verifies Init can be called twice without error.
func TestC4Fix_SQLite_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init(), "first Init must succeed")
	require.NoError(t, store.Init(), "second Init must succeed (idempotent)")

	require.True(t, sqliteColumnExists(db, "webchat_topic", "conversation_id"),
		"conversation_id column must still exist after second Init")
	require.True(t, sqliteIndexExists(db, "idx_webchat_topic_conversation"),
		"idx_webchat_topic_conversation must still exist after second Init")
}

// TestC4Fix_SQLite_PreExistingDB_Idempotent verifies that Init on a
// pre-existing DB is idempotent: the second call succeeds cleanly.
func TestC4Fix_SQLite_PreExistingDB_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.Exec(preExistingSQLiteSchemaSQL)
	require.NoError(t, err)

	store := NewWebChatStore(db, "sqlite3")
	require.NoError(t, store.Init(), "first Init on pre-existing DB must succeed")
	require.NoError(t, store.Init(), "second Init on pre-existing DB must succeed")
}

// --- Postgres integration tests (require SCION_TEST_POSTGRES_DSN) ---

func requirePostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run Postgres webchat store tests")
	}
	return dsn
}

// pgColumnExists checks whether a column exists in a table (Postgres).
func pgColumnExists(db *sql.DB, table, column string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2",
		table, column,
	).Scan(&count)
	return err == nil && count > 0
}

// pgIndexExists checks whether a named index exists (Postgres).
func pgIndexExists(db *sql.DB, indexName string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM pg_indexes WHERE indexname=$1",
		indexName,
	).Scan(&count)
	return err == nil && count > 0
}

// pgMigrationRecorded checks whether a migration name is present in webchat_migrations.
func pgMigrationRecorded(db *sql.DB, name string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM webchat_migrations WHERE name = $1",
		name,
	).Scan(&count)
	return err == nil && count > 0
}

// pgDropWebchatTables drops all webchat_* tables so each test starts clean.
func pgDropWebchatTables(t *testing.T, db *sql.DB) {
	t.Helper()
	tables := []string{
		"webchat_message_ext",
		"webchat_message_attachment",
		"webchat_attachment",
		"webchat_migrations",
		"webchat_dm",
		"webchat_user_prefs",
		"webchat_read_state",
		"webchat_topic",
		"webchat_thread_prefs",
		"webchat_conversation_context",
		"webchat_thread",
	}
	for _, tbl := range tables {
		_, _ = db.Exec("DROP TABLE IF EXISTS " + tbl + " CASCADE")
	}
}

const preExistingPostgresSchemaSQL = `
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

CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_one_general
    ON webchat_topic (project_id) WHERE is_general = TRUE AND deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_webchat_topic_project_name
    ON webchat_topic (project_id, LOWER(name)) WHERE deleted_at IS NULL;

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
    completed_at TEXT
);

CREATE TABLE IF NOT EXISTS webchat_attachment (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    filename    TEXT NOT NULL,
    mime_type   TEXT NOT NULL,
    size        INTEGER NOT NULL,
    uploaded_by TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webchat_attachment_project
    ON webchat_attachment (project_id);

CREATE TABLE IF NOT EXISTS webchat_message_attachment (
    message_id    TEXT NOT NULL,
    attachment_id TEXT NOT NULL,
    PRIMARY KEY (message_id, attachment_id)
);

CREATE INDEX IF NOT EXISTS idx_webchat_message_attachment_message
    ON webchat_message_attachment (message_id);

CREATE TABLE IF NOT EXISTS webchat_message_ext (
    message_id TEXT PRIMARY KEY,
    reply_to_id TEXT,
    edited_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);

INSERT INTO webchat_migrations (name, completed_at) VALUES ('thread_id_backfill', '2026-08-17T00:00:00Z');
INSERT INTO webchat_migrations (name, completed_at) VALUES ('wave1_seed', '2026-08-17T00:00:00Z');
INSERT INTO webchat_migrations (name, completed_at) VALUES ('thread_id_index', '2026-08-24T00:00:00Z');
`

func TestC4Fix_Postgres_FreshDB(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	pgDropWebchatTables(t, db)
	defer pgDropWebchatTables(t, db)

	store := NewWebChatStore(db, "postgres")
	require.NoError(t, store.Init(), "Init on fresh Postgres DB must succeed")

	require.True(t, pgColumnExists(db, "webchat_topic", "conversation_id"),
		"conversation_id column must exist after fresh Init")
	require.True(t, pgIndexExists(db, "idx_webchat_topic_conversation"),
		"idx_webchat_topic_conversation must exist after fresh Init")
	require.True(t, pgMigrationRecorded(db, "topic_conversation_id"),
		"topic_conversation_id migration must be recorded after fresh Init")
}

func TestC4Fix_Postgres_PreExistingDB(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	pgDropWebchatTables(t, db)
	defer pgDropWebchatTables(t, db)

	_, err = db.Exec(preExistingPostgresSchemaSQL)
	require.NoError(t, err, "seeding pre-existing Postgres schema must succeed")

	require.False(t, pgColumnExists(db, "webchat_topic", "conversation_id"),
		"precondition: conversation_id must not exist before Init")

	store := NewWebChatStore(db, "postgres")
	require.NoError(t, store.Init(), "Init on pre-existing Postgres DB must succeed")

	require.True(t, pgColumnExists(db, "webchat_topic", "conversation_id"),
		"conversation_id column must exist after Init on pre-existing DB")
	require.True(t, pgIndexExists(db, "idx_webchat_topic_conversation"),
		"idx_webchat_topic_conversation must exist after Init on pre-existing DB")
	require.True(t, pgMigrationRecorded(db, "topic_conversation_id"),
		"topic_conversation_id migration must be recorded after Init on pre-existing DB")
}

func TestC4Fix_Postgres_Idempotent(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	pgDropWebchatTables(t, db)
	defer pgDropWebchatTables(t, db)

	store := NewWebChatStore(db, "postgres")
	require.NoError(t, store.Init(), "first Init must succeed")
	require.NoError(t, store.Init(), "second Init must succeed (idempotent)")
}

func TestC4Fix_Postgres_PreExistingDB_Idempotent(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	pgDropWebchatTables(t, db)
	defer pgDropWebchatTables(t, db)

	_, err = db.Exec(preExistingPostgresSchemaSQL)
	require.NoError(t, err)

	store := NewWebChatStore(db, "postgres")
	require.NoError(t, store.Init(), "first Init on pre-existing DB must succeed")
	require.NoError(t, store.Init(), "second Init on pre-existing DB must succeed")
}
