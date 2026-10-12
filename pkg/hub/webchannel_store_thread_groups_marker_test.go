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
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Store tests for webchat_user_prefs.thread_groups_migrated_at
// (ptone/scion#4424, design section 5a). The same assertions run on SQLite
// and, with SCION_TEST_POSTGRES_DSN set, on Postgres
// (make test-webchat-postgres).

func pendingUserIDs(t *testing.T, wcs WebChatStore) []string {
	t.Helper()
	rows, err := wcs.ListUserPrefsPendingThreadGroupMigration(context.Background())
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.UserID)
	}
	return ids
}

func getPrefs(t *testing.T, wcs WebChatStore, userID string) *WebChatUserPrefs {
	t.Helper()
	p, err := wcs.GetUserPrefs(context.Background(), userID)
	require.NoError(t, err)
	return p
}

func assertUserPrefsThreadGroupsMarker(t *testing.T, wcs WebChatStore) {
	t.Helper()
	ctx := context.Background()
	const (
		groups  = `{"p1":[{"id":"g1","name":"Release","threadIds":["t1"]}]}`
		groups2 = `{"p1":[{"id":"g1","name":"Release","threadIds":["t1","t2"]}]}`
		order   = `{"p1":["g1","t3"]}`
		order2  = `{"p1":["g1","pg1","t3"]}`
	)
	set := func(userID, threadGroups, threadOrder string) {
		t.Helper()
		require.NoError(t, wcs.SetUserPrefs(ctx, userID, WebChatUserPrefs{
			SpaceSortMode: "activity", ThreadSortMode: "activity",
			ThreadGroups: threadGroups, ThreadOrder: threadOrder,
		}))
	}

	set("u-b", groups, order)
	set("u-a", groups, "")
	set("u-none", "", order)
	assert.Equal(t, []string{"u-a", "u-b"}, pendingUserIDs(t, wcs), "rows with groups, in user ID order")
	assert.False(t, getPrefs(t, wcs, "u-b").ThreadGroupsMigrated)
	rows, err := wcs.ListUserPrefsPendingThreadGroupMigration(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, groups, rows[1].ThreadGroups)
	assert.Equal(t, order, rows[1].ThreadOrder)

	// Compare-and-set: a stale thread_groups or thread_order writes nothing.
	ok, err := wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-b", groups2, order, order2)
	require.NoError(t, err)
	assert.False(t, ok, "stale thread_groups")
	ok, err = wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-b", groups, order2, order2)
	require.NoError(t, err)
	assert.False(t, ok, "stale thread_order")
	assert.False(t, getPrefs(t, wcs, "u-b").ThreadGroupsMigrated)

	ok, err = wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-b", groups, order, order2)
	require.NoError(t, err)
	assert.True(t, ok)
	p := getPrefs(t, wcs, "u-b")
	assert.True(t, p.ThreadGroupsMigrated)
	assert.Equal(t, order2, p.ThreadOrder)
	assert.Equal(t, groups, p.ThreadGroups, "personal groups are kept")
	assert.Equal(t, []string{"u-a"}, pendingUserIDs(t, wcs))

	// Already marked: a second mark writes nothing.
	ok, err = wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-b", groups, order2, order)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, order2, getPrefs(t, wcs, "u-b").ThreadOrder)

	// An empty old order matches a NULL thread_order, and an empty new
	// order is stored as NULL.
	ok, err = wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-a", groups, "", "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "", getPrefs(t, wcs, "u-a").ThreadOrder)
	assert.Empty(t, pendingUserIDs(t, wcs))

	// A write that keeps thread_groups keeps the marker.
	set("u-b", groups, order)
	assert.True(t, getPrefs(t, wcs, "u-b").ThreadGroupsMigrated, "unchanged thread_groups keeps the marker")
	// A write that changes thread_groups clears it.
	set("u-b", groups2, order)
	assert.False(t, getPrefs(t, wcs, "u-b").ThreadGroupsMigrated, "changed thread_groups clears the marker")
	assert.Equal(t, []string{"u-b"}, pendingUserIDs(t, wcs))
	// Clearing thread_groups clears it too, and the row is not pending.
	ok, err = wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-b", groups2, order, order)
	require.NoError(t, err)
	require.True(t, ok)
	set("u-b", "", order)
	assert.False(t, getPrefs(t, wcs, "u-b").ThreadGroupsMigrated)
	assert.Empty(t, pendingUserIDs(t, wcs))

	// A missing row is not an error and writes nothing.
	ok, err = wcs.MarkUserPrefsThreadGroupsMigrated(ctx, "u-missing", "", "", "")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestUserPrefsThreadGroupsMarker_SQLite(t *testing.T) {
	db := openTestMemorySQLite(t, "sqlite3")
	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	assertUserPrefsThreadGroupsMarker(t, wcs)
}

func TestUserPrefsThreadGroupsMarker_Postgres(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	pgDropWebchatTables(t, db)
	t.Cleanup(func() { pgDropWebchatTables(t, db) })

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	assertUserPrefsThreadGroupsMarker(t, wcs)
}

// The column is added to a database created before it existed, and a row
// written before the upgrade is pending (NULL marker).
func TestUserPrefsThreadGroupsMigratedAt_SQLite_PreExistingDB(t *testing.T) {
	db := openTestMemorySQLite(t, "sqlite3")
	_, err := db.Exec(preExistingSQLiteSchemaSQL)
	require.NoError(t, err)
	require.False(t, sqliteColumnExists(db, "webchat_user_prefs", "thread_groups_migrated_at"))

	wcs := NewWebChatStore(db, "sqlite3")
	require.NoError(t, wcs.Init())
	require.True(t, sqliteColumnExists(db, "webchat_user_prefs", "thread_groups_migrated_at"))
	require.True(t, sqliteMigrationRecorded(db, "add_user_prefs_thread_groups_migrated_at"))
	require.NoError(t, wcs.Init(), "second Init is idempotent")

	require.NoError(t, wcs.SetUserPrefs(context.Background(), "u1", WebChatUserPrefs{
		SpaceSortMode: "activity", ThreadSortMode: "activity",
		ThreadGroups: `{"p":[{"id":"g","name":"n","threadIds":[]}]}`,
	}))
	assert.Equal(t, []string{"u1"}, pendingUserIDs(t, wcs))
}

func TestUserPrefsThreadGroupsMigratedAt_Postgres_PreExistingDB(t *testing.T) {
	dsn := requirePostgresDSN(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	pgDropWebchatTables(t, db)
	t.Cleanup(func() { pgDropWebchatTables(t, db) })

	_, err = db.Exec(preExistingPostgresSchemaSQL)
	require.NoError(t, err)
	require.False(t, pgColumnExists(db, "webchat_user_prefs", "thread_groups_migrated_at"))

	wcs := NewWebChatStore(db, "postgres")
	require.NoError(t, wcs.Init())
	require.True(t, pgColumnExists(db, "webchat_user_prefs", "thread_groups_migrated_at"))
	require.True(t, pgMigrationRecorded(db, "user_prefs_thread_groups_migrated_at"))
	require.NoError(t, wcs.Init(), "second Init is idempotent")

	require.NoError(t, wcs.SetUserPrefs(context.Background(), "u1", WebChatUserPrefs{
		SpaceSortMode: "activity", ThreadSortMode: "activity",
		ThreadGroups: `{"p":[{"id":"g","name":"n","threadIds":[]}]}`,
	}))
	assert.Equal(t, []string{"u1"}, pendingUserIDs(t, wcs))
}
