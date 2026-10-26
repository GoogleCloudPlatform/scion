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

// The run attribution migration (WP1 attribution design v2.8, section 8;
// tests T17, T17i, T17o): twelve additive agent_lifecycle_cursors columns
// applied by the existing auto-migration, no backfill, no data rewrite.

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// oldAgentLifecycleCursorsDDL is the frozen pre-attribution table (the
// schema at 037defe4c), as the SQLite migration created it.
const oldAgentLifecycleCursorsDDL = "CREATE TABLE `agent_lifecycle_cursors` (`agent_id` uuid NOT NULL, `project_id` uuid NOT NULL, " +
	"`transition_revision` integer NOT NULL DEFAULT (0), `metadata_revision` integer NOT NULL DEFAULT (0), " +
	"`metadata_digest` text NOT NULL DEFAULT (''), `last_fingerprint` text NOT NULL DEFAULT (''), " +
	"`last_deleted` bool NOT NULL DEFAULT (false), `open_run` text NOT NULL DEFAULT (''), " +
	"`open_run_generation` text NOT NULL DEFAULT (''), `open_run_metadata_revision` integer NOT NULL DEFAULT (0), " +
	"`pending_run_change_at` datetime NULL, `reincarnation_base_run_id` text NOT NULL DEFAULT (''), " +
	"`pending_adopted_from` text NOT NULL DEFAULT (''), `pending_adopted_at` datetime NULL, " +
	"`pending_adoption_source` text NOT NULL DEFAULT (''), `pending_adopted_to` text NOT NULL DEFAULT (''), " +
	"`creator_principal_type` text NOT NULL DEFAULT (''), `creator_principal_id` text NOT NULL DEFAULT (''), " +
	"`requested_template` text NOT NULL DEFAULT (''), `updated_at` datetime NOT NULL, PRIMARY KEY (`agent_id`))"

const oldAgentLifecycleCursorsIndexDDL = "CREATE INDEX `agentlifecyclecursor_project_id` ON `agent_lifecycle_cursors` (`project_id`)"

// oldCursorColumns are the pre-attribution columns, in DDL order.
const oldCursorColumns = "agent_id, project_id, transition_revision, metadata_revision, metadata_digest, last_fingerprint, " +
	"last_deleted, open_run, open_run_generation, open_run_metadata_revision, pending_run_change_at, " +
	"reincarnation_base_run_id, pending_adopted_from, pending_adopted_at, pending_adoption_source, pending_adopted_to, " +
	"creator_principal_type, creator_principal_id, requested_template, updated_at"

// oldCursorRows reads every pre-attribution column of every cursor row as
// text, ordered by agent_id.
func oldCursorRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT " + oldCursorColumns + " FROM agent_lifecycle_cursors ORDER BY agent_id")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		parts := make([]string, len(cols))
		for i, v := range vals {
			parts[i] = v.String
		}
		out = append(out, strings.Join(parts, "|"))
	}
	require.NoError(t, rows.Err())
	return out
}

// TestLifecycleAttributionMigration_SQLite: T17 applies the migration on a
// database holding the pre-change table with existing rows: the twelve
// columns are added with their defaults, every existing row reads LEGACY
// (state "", NULL stamp, UNKNOWN history, no witness), every other value
// and the row count are unchanged, and an old row normalises fail-closed
// (held/UNATTRIBUTED, T11). T17i: a second migration is a no-op. T17o: an
// insert listing only the pre-change columns (an older binary) succeeds
// and reads LEGACY.
func TestLifecycleAttributionMigration_SQLite(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "attribution-migration.db")
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	for _, stmt := range []string{oldAgentLifecycleCursorsDDL, oldAgentLifecycleCursorsIndexDDL} {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	idA, idB, project := uuid.NewString(), uuid.NewString(), uuid.NewString()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	insertOld := func(id, openRun, gen string, rev int64) {
		_, err := raw.ExecContext(ctx, "INSERT INTO agent_lifecycle_cursors ("+oldCursorColumns+") VALUES "+
			"(?, ?, 4, 2, ?, ?, false, ?, ?, ?, NULL, '', '', NULL, '', '', 'user', 'u1', 'claude', ?)",
			id, project, strings.Repeat("d", 64), strings.Repeat("f", 64), openRun, gen, rev, base)
		require.NoError(t, err)
	}
	insertOld(idA, "", "", 0)
	insertOld(idB, "run-1", "1", 2)
	before := oldCursorRows(t, raw)
	require.Len(t, before, 2)
	require.NoError(t, raw.Close())

	cs := newTestCompositeStoreFromDSN(t, dsn)
	require.NoError(t, cs.Migrate(ctx))
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ddl := sqliteObjectSQL(t, db, "agent_lifecycle_cursors")
	for _, col := range []string{"attribution_state", "attribution_stamp", "last_run", "last_generation", "attributed_run",
		"attributed_generation", "quarantined_runs", "quarantine_mode", "reincarnation_mint_run", "untrusted_open_run",
		"untrusted_open_run_generation", "untrusted_open_run_rev"} {
		assert.Contains(t, ddl, "`"+col+"`", "column %s added", col)
	}
	assert.Equal(t, before, oldCursorRows(t, db), "existing values and row count unchanged")
	assertLegacy := func(id string) *store.LifecycleCursor {
		t.Helper()
		c, err := cs.client.AgentLifecycleCursor.Get(ctx, uuid.MustParse(id))
		require.NoError(t, err)
		sc := entCursorToStore(c)
		assert.Equal(t, store.LifecycleAttribution{}, sc.Attribution, "LEGACY: every attribution column at its default")
		assert.Nil(t, sc.AttributionStamp, "NULL stamp")
		return sc
	}
	assertLegacy(idA)
	cb := assertLegacy(idB)

	// Old-row behaviour: the LEGACY cursor normalises to held/UNATTRIBUTED
	// with an UNKNOWN history; its valid named segment stays trusted for
	// its own closure (R4).
	f := lifecycleevents.Facts{Op: store.AgentWriteStatus, HasBefore: true, BeforeLive: true, HasAfter: true,
		BeforeRunID: "run-1", AfterRunID: "run-1", BeforeGeneration: 1, AfterGeneration: 1,
		Cursor: lifecycleevents.CursorFacts{Exists: true, OpenRun: cb.OpenRun, Run: cb.RunState(), FingerprintMatches: true}}
	pre := lifecycleevents.PrepareAttribution(&f, cb)
	assert.True(t, pre.Normalized)
	assert.Equal(t, "legacy", pre.StaleReason)
	assert.False(t, pre.SegUntrusted, "a valid named segment on a LEGACY cursor is trusted for its closure")

	// T17i: the second migration changes nothing.
	ddlBefore := ddl
	require.NoError(t, cs.Migrate(ctx))
	assert.Equal(t, ddlBefore, sqliteObjectSQL(t, db, "agent_lifecycle_cursors"))
	assert.Equal(t, before, oldCursorRows(t, db))

	// T17o: an older client inserts only the pre-change columns.
	idC := uuid.NewString()
	_, err = db.ExecContext(ctx, "INSERT INTO agent_lifecycle_cursors ("+oldCursorColumns+") VALUES "+
		"(?, ?, 1, 1, '', '', false, '', '', 0, NULL, '', '', NULL, '', '', '', '', '', ?)", idC, project, base)
	require.NoError(t, err)
	assertLegacy(idC)
}

// TestLifecycleAttributionMigration_Postgres: the same on Postgres, through
// MigrateWithSchemaLock: a fresh schema returned to the pre-change table
// shape, existing rows, the migration, old-row values, idempotence and an
// older client's insert. Runs under -tags integration with
// SCION_TEST_POSTGRES_URL set; skips otherwise.
func TestLifecycleAttributionMigration_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres migration case")
	}
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	raw, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	for _, col := range []string{"attribution_state", "attribution_stamp", "last_run", "last_generation", "attributed_run",
		"attributed_generation", "quarantined_runs", "quarantine_mode", "reincarnation_mint_run", "untrusted_open_run",
		"untrusted_open_run_generation", "untrusted_open_run_rev"} {
		_, err := raw.ExecContext(ctx, "ALTER TABLE agent_lifecycle_cursors DROP COLUMN "+col)
		require.NoError(t, err, col)
	}
	id, project := uuid.NewString(), uuid.NewString()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	_, err = raw.ExecContext(ctx, "INSERT INTO agent_lifecycle_cursors ("+oldCursorColumns+") VALUES "+
		"($1, $2, 4, 2, 'd', 'f', false, 'run-1', '1', 2, NULL, '', '', NULL, '', '', 'user', 'u1', 'claude', $3)", id, project, base)
	require.NoError(t, err)
	before := oldCursorRows(t, raw)

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.MigrateWithSchemaLock(ctx))
	// The twelve columns exist (explicit catalog check, implementation
	// review r1 OP-4).
	for _, col := range []string{"attribution_state", "attribution_stamp", "last_run", "last_generation", "attributed_run",
		"attributed_generation", "quarantined_runs", "quarantine_mode", "reincarnation_mint_run", "untrusted_open_run",
		"untrusted_open_run_generation", "untrusted_open_run_rev"} {
		var n int
		require.NoError(t, raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'agent_lifecycle_cursors' AND column_name = $1`, col).Scan(&n))
		assert.Equal(t, 1, n, "column %s added", col)
	}
	assert.Equal(t, before, oldCursorRows(t, raw))
	c, err := cs.client.AgentLifecycleCursor.Get(ctx, uuid.MustParse(id))
	require.NoError(t, err)
	sc := entCursorToStore(c)
	assert.Equal(t, store.LifecycleAttribution{}, sc.Attribution)
	assert.Nil(t, c.AttributionStamp)
	// Old-row behaviour, as in the SQLite case: LEGACY normalises
	// fail-closed and its valid named segment stays trusted (R4).
	f := lifecycleevents.Facts{Op: store.AgentWriteStatus, HasBefore: true, BeforeLive: true, HasAfter: true,
		BeforeRunID: "run-1", AfterRunID: "run-1", BeforeGeneration: 1, AfterGeneration: 1,
		Cursor: lifecycleevents.CursorFacts{Exists: true, OpenRun: sc.OpenRun, Run: sc.RunState(), FingerprintMatches: true}}
	pre := lifecycleevents.PrepareAttribution(&f, sc)
	assert.True(t, pre.Normalized)
	assert.Equal(t, "legacy", pre.StaleReason)
	assert.False(t, pre.SegUntrusted)

	require.NoError(t, cs.MigrateWithSchemaLock(ctx))
	assert.Equal(t, before, oldCursorRows(t, raw))

	idC := uuid.NewString()
	_, err = raw.ExecContext(ctx, "INSERT INTO agent_lifecycle_cursors ("+oldCursorColumns+") VALUES "+
		"($1, $2, 1, 1, '', '', false, '', '', 0, NULL, '', '', NULL, '', '', '', '', '', $3)", idC, project, base)
	require.NoError(t, err)
	c, err = cs.client.AgentLifecycleCursor.Get(ctx, uuid.MustParse(idC))
	require.NoError(t, err)
	assert.Equal(t, store.LifecycleAttribution{}, entCursorToStore(c).Attribution)
	assert.Nil(t, c.AttributionStamp)
}
