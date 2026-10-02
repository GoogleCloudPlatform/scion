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

package entadapter

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const kathmanduStamp = "2026-10-01 14:45:00 +0545 +0545"

// An older schema: users without most of today's columns, agents with a
// canonical value only, and no hub_settings table. This is what the boot
// repair sees before ent's schema migration runs.
func openOldSchema(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		"CREATE TABLE users (id TEXT PRIMARY KEY, created DATETIME)",
		"CREATE TABLE agents (id TEXT PRIMARY KEY, created DATETIME, updated DATETIME)",
		"INSERT INTO users VALUES ('u1', '" + kathmanduStamp + "'), ('u2', '2026-10-01 09:00:00 +0000 UTC')",
		"INSERT INTO agents VALUES ('a1', '2026-10-01 14:45:00 +0545 IST', '2026-10-01 09:00:00 +0000 UTC')",
	} {
		_, err := db.Exec(q)
		require.NoError(t, err, q)
	}
	return db, path
}

func cellText(t *testing.T, db *sql.DB, table, id string) string {
	t.Helper()
	var s string
	require.NoError(t, db.QueryRow("SELECT CAST(created AS TEXT) FROM "+table+" WHERE id = ?", id).Scan(&s))
	return s
}

func TestUnreadableTimestampTables_OlderSchemaAndTablesOption(t *testing.T) {
	ctx := context.Background()
	db, _ := openOldSchema(t)

	tables, err := UnreadableTimestampTables(ctx, db, dialect.SQLite)
	require.NoError(t, err, "missing tables and columns must be skipped")
	assert.Equal(t, []string{"users"}, tables)

	rep, err := NormalizeUTCTimestamps(ctx, db, dialect.SQLite, nil, TimestampNormalizeOptions{Tables: tables})
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Rewritten)
	assert.Equal(t, "2026-10-01 09:00:00 +0000 UTC", cellText(t, db, "users", "u1"))
	assert.Equal(t, "2026-10-01 14:45:00 +0545 IST", cellText(t, db, "agents", "a1"),
		"a table outside Tables must not be rewritten")

	tables, err = UnreadableTimestampTables(ctx, db, dialect.SQLite)
	require.NoError(t, err)
	assert.Empty(t, tables)

	// Without Tables the run covers every table, as the maintenance
	// operation does.
	rep, err = NormalizeUTCTimestamps(ctx, db, dialect.SQLite, nil, TimestampNormalizeOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Rewritten)
	assert.Equal(t, "2026-10-01 09:00:00 +0000 UTC", cellText(t, db, "agents", "a1"))
}

func TestUnreadableTimestampTables_Postgres(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	tables, err := UnreadableTimestampTables(context.Background(), db, dialect.Postgres)
	require.NoError(t, err)
	assert.Empty(t, tables)
}

func TestSnapshotSQLite(t *testing.T) {
	ctx := context.Background()
	db, path := openOldSchema(t)
	now := time.Date(2026, 10, 2, 23, 10, 5, 0, time.FixedZone("x", 3600))

	snap, err := SnapshotSQLite(ctx, db, "pre-test", now)
	require.NoError(t, err)
	assert.Equal(t, path+".pre-test-20261002T221005Z.bak", snap)
	cp, err := sql.Open("sqlite", "file:"+snap+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cp.Close() })
	assert.Equal(t, kathmanduStamp, cellText(t, cp, "users", "u1"))

	_, err = SnapshotSQLite(ctx, db, "pre-test", now)
	assert.ErrorContains(t, err, "already exists", "an existing snapshot must not be overwritten")

	// A failed VACUUM INTO leaves no file behind.
	_, err = SnapshotSQLite(ctx, db, "missing-dir/x", now)
	require.Error(t, err)
	_, statErr := os.Stat(path + ".missing-dir/x-20261002T221005Z.bak")
	assert.True(t, os.IsNotExist(statErr))

	mem, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = mem.Close() })
	_, err = SnapshotSQLite(ctx, mem, "pre-test", now)
	assert.ErrorContains(t, err, "in-memory")
}
