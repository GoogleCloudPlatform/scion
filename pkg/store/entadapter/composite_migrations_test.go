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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oldAccessPoliciesDDL is the frozen v1 schema for access_policies WITHOUT the
// unique index on (name, scope_type, scope_id). This is what already-deployed
// databases look like before the migration from PR #993.
const oldAccessPoliciesDDL = `CREATE TABLE access_policies (
	id integer PRIMARY KEY AUTOINCREMENT,
	name text NOT NULL,
	description text NULL,
	scope_type text NOT NULL DEFAULT '',
	scope_id text NOT NULL DEFAULT '',
	rules text NULL,
	created datetime NOT NULL,
	updated datetime NOT NULL
)`

// newTestCompositeStoreFromDSN opens a SQLite database at the given DSN and
// returns a CompositeStore wrapping it. The caller is responsible for closing
// any raw *sql.DB handles separately; the returned CompositeStore is cleaned
// up via t.Cleanup.
func newTestCompositeStoreFromDSN(t *testing.T, dsn string) *CompositeStore {
	t.Helper()
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// countAccessPolicies returns the number of rows in the access_policies table.
func countAccessPolicies(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM access_policies").Scan(&n)
	require.NoError(t, err)
	return n
}

// TestDeduplicateAccessPolicies_RemovesDuplicates verifies that duplicate
// (name, scope_type, scope_id) rows are collapsed to the single oldest row.
func TestDeduplicateAccessPolicies_RemovesDuplicates(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)

	// Create old schema without the unique index.
	_, err = raw.ExecContext(ctx, oldAccessPoliciesDDL)
	require.NoError(t, err)

	// Insert 3 duplicate rows (same name, scope_type, scope_id) with different
	// timestamps. The oldest (2026-01-01) should survive.
	stmts := []string{
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-a', 'project', 'proj-1', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-a', 'project', 'proj-1', '2026-02-01 00:00:00', '2026-02-01 00:00:00')`,
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-a', 'project', 'proj-1', '2026-03-01 00:00:00', '2026-03-01 00:00:00')`,
		// 1 unique row — different name.
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-b', 'project', 'proj-1', '2026-01-15 00:00:00', '2026-01-15 00:00:00')`,
	}
	for _, stmt := range stmts {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	require.Equal(t, 4, countAccessPolicies(t, raw))
	require.NoError(t, raw.Close())

	cs := newTestCompositeStoreFromDSN(t, dsn)
	require.NoError(t, cs.deduplicateAccessPolicies(ctx))

	db := cs.DB()
	require.NotNil(t, db)

	// 2 rows total: 1 survivor from the dup set + 1 unique row.
	assert.Equal(t, 2, countAccessPolicies(t, db))

	// The survivor from the dup set is the oldest by "created".
	var created string
	err = db.QueryRow(
		"SELECT created FROM access_policies WHERE name = 'policy-a'",
	).Scan(&created)
	require.NoError(t, err)
	assert.Contains(t, created, "2026-01-01")
}

// TestDeduplicateAccessPolicies_NoDuplicates verifies that when all rows have
// distinct (name, scope_type, scope_id), nothing is deleted.
func TestDeduplicateAccessPolicies_NoDuplicates(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)

	_, err = raw.ExecContext(ctx, oldAccessPoliciesDDL)
	require.NoError(t, err)

	stmts := []string{
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-a', 'project', 'proj-1', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-b', 'project', 'proj-1', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-c', 'hub', '', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
	}
	for _, stmt := range stmts {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	require.NoError(t, raw.Close())

	cs := newTestCompositeStoreFromDSN(t, dsn)
	require.NoError(t, cs.deduplicateAccessPolicies(ctx))

	assert.Equal(t, 3, countAccessPolicies(t, cs.DB()))
}

// TestDeduplicateAccessPolicies_FreshDatabase verifies that the function is a
// no-op on a fresh database that has no access_policies table at all.
func TestDeduplicateAccessPolicies_FreshDatabase(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	cs := newTestCompositeStoreFromDSN(t, dsn)

	// No tables exist — dedup should return nil without error.
	require.NoError(t, cs.deduplicateAccessPolicies(ctx))
}

// TestDeduplicateAccessPolicies_TimestampTies verifies that when duplicates
// share the exact same "created" timestamp, exactly one row survives.
func TestDeduplicateAccessPolicies_TimestampTies(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)

	_, err = raw.ExecContext(ctx, oldAccessPoliciesDDL)
	require.NoError(t, err)

	// Two rows with the exact same (name, scope_type, scope_id, created).
	stmts := []string{
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-tie', 'project', 'proj-1', '2026-06-15 12:00:00', '2026-06-15 12:00:00')`,
		`INSERT INTO access_policies (name, scope_type, scope_id, created, updated)
		 VALUES ('policy-tie', 'project', 'proj-1', '2026-06-15 12:00:00', '2026-06-15 12:00:00')`,
	}
	for _, stmt := range stmts {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	require.Equal(t, 2, countAccessPolicies(t, raw))
	require.NoError(t, raw.Close())

	cs := newTestCompositeStoreFromDSN(t, dsn)
	require.NoError(t, cs.deduplicateAccessPolicies(ctx))

	// Exactly 1 row survives (either is acceptable).
	assert.Equal(t, 1, countAccessPolicies(t, cs.DB()))
}
