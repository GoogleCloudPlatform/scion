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

package enttest

import (
	"context"
	"database/sql"
	"testing"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// PreMigrateUniqueIndexes maps each table that the pre-migration step
// de-duplicates to the unique index the schema migration creates on it.
var PreMigrateUniqueIndexes = map[string]string{
	"access_policies":       "accesspolicy_name_scope_type_scope_id",
	"delegation_edges":      "delegationedge_delegate_type_delegate_id_scope_type_scope_id",
	"agent_session_metrics": "agentsessionmetrics_agent_id_session_id_started_at",
}

// preMigrateWantRows is the row count each seeded table must have once the
// duplicates are removed.
var preMigrateWantRows = map[string]int{
	"access_policies":       2, // one kept of the duplicate pair, plus a distinct policy
	"delegation_edges":      3, // one kept of the active pair, plus an inactive edge and a distinct edge
	"agent_session_metrics": 2, // one kept of the duplicate pair, plus a later segment
}

// PreMigrateTables lists the tables SeedPreMigrateDuplicates can seed.
var PreMigrateTables = []string{"access_policies", "delegation_edges", "agent_session_metrics"}

// preMigrateSeedRows are inserted once the unique indexes are dropped. Each
// table gets a duplicate pair in the shape the pre-migration step removes,
// plus rows that are not duplicates and must survive.
var preMigrateSeedRows = map[string]string{
	"access_policies": `INSERT INTO access_policies (id, name, scope_type, scope_id, resource_type, effect, created, updated) VALUES
		('00000000-0000-0000-0000-00000000a001', 'policy-a', 'project', 'p1', '*', 'allow', '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
		('00000000-0000-0000-0000-00000000a002', 'policy-a', 'project', 'p1', '*', 'allow', '2026-02-01 00:00:00', '2026-02-01 00:00:00'),
		('00000000-0000-0000-0000-00000000a003', 'policy-b', 'project', 'p1', '*', 'allow', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
	"delegation_edges": `INSERT INTO delegation_edges (id, delegator_type, delegator_id, delegate_type, delegate_id, scope_type, scope_id, role, active, created, updated) VALUES
		('00000000-0000-0000-0000-00000000d001', 'user', 'u1', 'agent', 'a1', 'project', 'p1', 'member', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00'),
		('00000000-0000-0000-0000-00000000d002', 'user', 'u1', 'agent', 'a1', 'project', 'p1', 'member', 1, '2026-02-01 00:00:00', '2026-02-01 00:00:00'),
		('00000000-0000-0000-0000-00000000d003', 'user', 'u1', 'agent', 'a1', 'project', 'p1', 'member', 0, '2026-03-01 00:00:00', '2026-03-01 00:00:00'),
		('00000000-0000-0000-0000-00000000d004', 'user', 'u1', 'agent', 'a2', 'project', 'p1', 'member', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
	"agent_session_metrics": `INSERT INTO agent_session_metrics (id, agent_id, grove_id, session_id, started_at, created_at) VALUES
		('00000000-0000-0000-0000-00000000e001', 'agent-a', 'p1', 's1', '2026-01-01 10:00:00', '2026-01-01 11:00:00'),
		('00000000-0000-0000-0000-00000000e002', 'agent-a', 'p1', 's1', '2026-01-01 10:00:00', '2026-01-01 12:00:00'),
		('00000000-0000-0000-0000-00000000e003', 'agent-a', 'p1', 's1', '2026-01-01 13:00:00', '2026-01-01 14:00:00')`,
}

// SeedPreMigrateDuplicates turns the SQLite database file at dbPath into an
// older hub database: the full schema without the unique indexes listed in
// PreMigrateUniqueIndexes, holding duplicate rows those indexes reject in
// each of tables (all of PreMigrateTables when none are given). Running the
// schema migration on it fails unless the pre-migration step
// (entadapter.PreMigrate) runs first.
func SeedPreMigrateDuplicates(t testing.TB, dbPath string, tables ...string) {
	t.Helper()
	withSQLiteDB(t, dbPath, func(db *sql.DB) {
		ctx := context.Background()
		for _, idx := range PreMigrateUniqueIndexes {
			if _, err := db.ExecContext(ctx, "DROP INDEX "+idx); err != nil {
				t.Fatalf("drop index %s: %v", idx, err)
			}
		}
		if len(tables) == 0 {
			tables = PreMigrateTables
		}
		for _, table := range tables {
			stmt, ok := preMigrateSeedRows[table]
			if !ok {
				t.Fatalf("no duplicate-row seed for table %q", table)
			}
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("seed duplicate rows: %v\n%s", err, stmt)
			}
		}
	}, true)
}

// AssertPreMigrateDeduplicated checks that the database file at dbPath,
// seeded by SeedPreMigrateDuplicates with the same tables, has every unique
// index back and, in each seeded table, only the rows that are not
// duplicates.
func AssertPreMigrateDeduplicated(t testing.TB, dbPath string, tables ...string) {
	t.Helper()
	withSQLiteDB(t, dbPath, func(db *sql.DB) {
		for table, idx := range PreMigrateUniqueIndexes {
			var name string
			err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&name)
			if err != nil {
				t.Errorf("unique index %s on %s missing after migration: %v", idx, table, err)
			}
		}
		if len(tables) == 0 {
			tables = PreMigrateTables
		}
		for _, table := range tables {
			var n int
			if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			if want := preMigrateWantRows[table]; n != want {
				t.Errorf("%s has %d rows after migration, want %d", table, n, want)
			}
		}
	}, false)
}

// withSQLiteDB opens the SQLite file at dbPath, optionally runs the schema
// migration, and passes the raw database handle to fn.
func withSQLiteDB(t testing.TB, dbPath string, fn func(*sql.DB), migrate bool) {
	t.Helper()
	client, err := entc.OpenSQLite("file:"+dbPath, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	defer func() { _ = client.Close() }()
	if migrate {
		if err := entc.AutoMigrate(context.Background(), client); err != nil {
			t.Fatalf("migrate sqlite %s: %v", dbPath, err)
		}
	}
	drv, ok := client.Driver().(*entsql.Driver)
	if !ok {
		t.Fatalf("sqlite client has no database/sql driver")
	}
	fn(drv.DB())
}
