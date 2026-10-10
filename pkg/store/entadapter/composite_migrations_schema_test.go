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
	"testing"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestTableExistsQuery_PostgresUsesCurrentSchema pins the catalog lookup the
// pre-migration dedups rely on. On Postgres it must follow the connection's
// search_path (current_schema()) rather than a hard-coded 'public' schema;
// the SQLite query is unchanged.
func TestTableExistsQuery_PostgresUsesCurrentSchema(t *testing.T) {
	pg := tableExistsQuery(dialect.Postgres, "delegation_edges")
	assert.Contains(t, pg, "table_schema = current_schema()")
	assert.NotContains(t, pg, "'public'")
	assert.Contains(t, pg, "table_name = 'delegation_edges'")

	assert.Equal(t,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='delegation_edges'`,
		tableExistsQuery(dialect.SQLite, "delegation_edges"))

	assert.Empty(t, tableExistsQuery(dialect.MySQL, "delegation_edges"))
}

// TestMigrate_NonPublicSearchPath_DedupsRun_Postgres runs CompositeStore.Migrate
// against tables that live in a non-public schema (selected through the
// connection's search_path) and hold rows that violate the unique indexes the
// migration creates. The pre-migration dedups must find the tables, remove the
// duplicates, and let AutoMigrate recreate the unique indexes.
//
// Runs under -tags integration with SCION_TEST_POSTGRES_URL set; skips
// otherwise.
func TestMigrate_NonPublicSearchPath_DedupsRun_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres migration case")
	}
	ctx := context.Background()

	// A fresh, fully migrated schema; the URL's search_path points at it.
	dsn := enttest.NewSchemaURL(t)
	raw, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	var schema string
	require.NoError(t, raw.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema))
	require.NotEqual(t, "public", schema, "the test needs the tables outside the public schema")

	// Return both tables to their pre-unique-index shape.
	for _, idx := range []string{
		"accesspolicy_name_scope_type_scope_id",
		"delegationedge_delegate_type_delegate_id_scope_type_scope_id",
	} {
		_, err := raw.ExecContext(ctx, "DROP INDEX "+idx)
		require.NoError(t, err, idx)
	}

	// Duplicate rows that the unique indexes would reject. The oldest row of
	// each set must survive.
	stmts := []string{
		`INSERT INTO access_policies (id, name, scope_type, scope_id, resource_type, effect, created, updated) VALUES
			('00000000-0000-0000-0000-0000000000a1', 'policy-a', 'project', 'proj-1', '*', 'allow', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00'),
			('00000000-0000-0000-0000-0000000000a2', 'policy-a', 'project', 'proj-1', '*', 'allow', '2026-02-01 00:00:00+00', '2026-02-01 00:00:00+00'),
			('00000000-0000-0000-0000-0000000000a3', 'policy-b', 'project', 'proj-1', '*', 'allow', '2026-01-15 00:00:00+00', '2026-01-15 00:00:00+00')`,
		`INSERT INTO delegation_edges (id, delegator_type, delegator_id, delegate_type, delegate_id, scope_type, scope_id, role, active, grandfathered, created, updated) VALUES
			('00000000-0000-0000-0000-0000000000e1', 'user', 'user-1', 'agent', 'agent-1', 'project', 'proj-1', 'full', true, false, '2026-01-01 00:00:00+00', '2026-01-01 00:00:00+00'),
			('00000000-0000-0000-0000-0000000000e2', 'user', 'user-1', 'agent', 'agent-1', 'project', 'proj-1', 'full', true, false, '2026-02-01 00:00:00+00', '2026-02-01 00:00:00+00'),
			('00000000-0000-0000-0000-0000000000e3', 'user', 'user-1', 'agent', 'agent-1', 'project', 'proj-1', 'full', false, false, '2026-03-01 00:00:00+00', '2026-03-01 00:00:00+00')`,
	}
	for _, stmt := range stmts {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	exists, err := tableExists(ctx, cs.client, cs.DB(), "delegation_edges")
	require.NoError(t, err)
	require.True(t, exists, "tableExists must see tables in the search_path schema %q", schema)

	require.NoError(t, cs.Migrate(ctx))

	ids := func(query string) []string {
		t.Helper()
		rows, err := raw.QueryContext(ctx, query)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			out = append(out, id)
		}
		require.NoError(t, rows.Err())
		return out
	}
	assert.Equal(t, []string{
		"00000000-0000-0000-0000-0000000000a1",
		"00000000-0000-0000-0000-0000000000a3",
	}, ids(`SELECT id::text FROM access_policies ORDER BY id`))
	// The inactive edge is outside the partial index and is kept.
	assert.Equal(t, []string{
		"00000000-0000-0000-0000-0000000000e1",
		"00000000-0000-0000-0000-0000000000e3",
	}, ids(`SELECT id::text FROM delegation_edges ORDER BY id`))

	for _, idx := range []string{
		"accesspolicy_name_scope_type_scope_id",
		"delegationedge_delegate_type_delegate_id_scope_type_scope_id",
	} {
		var n int
		require.NoError(t, raw.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1`,
			idx).Scan(&n))
		assert.Equal(t, 1, n, "Migrate must recreate unique index %s", idx)
	}
}
