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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqliteIndexExists checks whether a named index exists in the database.
func sqliteIndexExists(db *sql.DB, indexName string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?",
		indexName,
	).Scan(&count)
	return err == nil && count > 0
}

func requirePostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run Postgres webchat store tests")
	}
	return dsn
}

// pgIndexExists checks whether a named index exists in the current schema
// (Postgres).
func pgIndexExists(db *sql.DB, indexName string) bool {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1",
		indexName,
	).Scan(&count)
	return err == nil && count > 0
}

// pgDropWebchatTables drops all webchat_* tables so each test starts clean.
func pgDropWebchatTables(t *testing.T, db *sql.DB) {
	t.Helper()
	tables := []string{
		"webchat_mention",
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

// newEntWebChatStore opens a webchat store on the ent-migrated test store's
// database, as the hub does in production.
func newEntWebChatStore(t *testing.T) (*sqliteWebChatStore, store.Store, *sql.DB) {
	t.Helper()
	s := createTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	db := dbp.DB()
	wcs := NewWebChatStore(db, "sqlite").(*sqliteWebChatStore)
	require.NoError(t, wcs.Init())
	return wcs, s, db
}

func rawText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow(query, args...).Scan(&s))
	return s
}

func requireUTCInstant(t *testing.T, got, want time.Time) {
	t.Helper()
	assert.True(t, got.Equal(want), "got %v, want %v", got, want)
	assert.Equal(t, time.UTC, got.Location(), "got %v", got)
}
