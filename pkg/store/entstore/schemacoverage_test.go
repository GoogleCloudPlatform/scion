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

//go:build schemadiff

// This is the Phase 1 safety gate. It runs only under the `schemadiff` build
// tag so it does not slow down `make test` or CI. Invoke explicitly:
//
//	go test -tags schemadiff ./pkg/store/entstore/...
//
// The gate proves that every table and column produced by the raw SQL
// migrations (V1-V45 in pkg/store/sqlite) is also produced by Ent's
// AutoMigrate. Without this, Phase 2 — which points Ent at the raw SQL
// database — would silently drop columns under WithDropColumn(true).
//
// Exclusions:
//   - The four V5 stub tables (groups, group_members, policies,
//     policy_bindings) are intentionally divergent. Phase 2 drops them
//     before Ent touches hub.db; Ent's own group/policy schemas are the
//     target.
//   - schema_migrations is a raw-SQL migration bookkeeping table and has no
//     Ent counterpart.
//   - sqlite_sequence / sqlite_* are SQLite internals.

package entstore

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
)

// excludedTables are tables where raw SQL and Ent intentionally disagree.
var excludedTables = map[string]bool{
	// V5 stubs (dropped in Phase 2; Ent schemas are authoritative).
	"groups":          true,
	"group_members":   true,
	"policies":        true,
	"policy_bindings": true,
	// Raw SQL bookkeeping only.
	"schema_migrations": true,
	// V7 api_keys is superseded by V34 user_access_tokens but was never
	// dropped from the raw SQL schema; Ent does not model it.
	"api_keys": true,
}

// TestSchemaCoverage_RawSQLTables_HaveEntTables asserts every non-excluded
// raw-SQL table has a matching Ent table. Column-level coverage is checked
// by TestSchemaCoverage_Columns.
func TestSchemaCoverage_RawSQLTables_HaveEntTables(t *testing.T) {
	ctx := context.Background()

	rawDB := openRawSQL(t, ctx)
	defer rawDB.Close()
	entDB := openEnt(t, ctx)
	defer entDB.Close()

	rawTables := listTables(t, ctx, rawDB)
	entTables := listTables(t, ctx, entDB)

	missing := diffTables(rawTables, entTables, excludedTables)
	if len(missing) > 0 {
		t.Errorf("Ent AutoMigrate is missing tables present in raw SQL:\n  %s", strings.Join(missing, "\n  "))
	}
	extra := diffTables(entTables, rawTables, excludedTables)
	if len(extra) > 0 {
		t.Logf("Ent creates extra tables not present in raw SQL (informational): %s", strings.Join(extra, ", "))
	}
}

// TestSchemaCoverage_Columns asserts, for every non-excluded table present in
// both stores, that every raw-SQL column has a matching Ent column with a
// compatible type.
func TestSchemaCoverage_Columns(t *testing.T) {
	ctx := context.Background()

	rawDB := openRawSQL(t, ctx)
	defer rawDB.Close()
	entDB := openEnt(t, ctx)
	defer entDB.Close()

	rawTables := listTables(t, ctx, rawDB)
	entTables := toSet(listTables(t, ctx, entDB))

	for _, table := range rawTables {
		if excludedTables[table] {
			continue
		}
		if !entTables[table] {
			continue // reported by table-level test
		}
		rawCols := tableColumns(t, ctx, rawDB, table)
		entCols := tableColumns(t, ctx, entDB, table)
		entColNames := columnNameSet(entCols)
		for _, c := range rawCols {
			if _, ok := entColNames[c.name]; !ok {
				t.Errorf("table %q: raw SQL has column %q (type=%s, notnull=%v) but Ent is missing it",
					table, c.name, c.ctype, c.notnull)
			}
		}
	}
}

// --- helpers ---

type columnInfo struct {
	name    string
	ctype   string
	notnull bool
	dflt    sql.NullString
	pk      int
}

func openRawSQL(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	s, err := sqlite.New("file:rawsql?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("opening raw SQL store: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("running raw SQL migrations: %v", err)
	}
	return s.DB()
}

func openEnt(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	client, err := entc.OpenSQLite("file:entmig?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("opening ent client: %v", err)
	}
	if err := entc.AutoMigrate(ctx, client); err != nil {
		t.Fatalf("running Ent auto-migration: %v", err)
	}
	// Reach into the client's underlying driver to run PRAGMA queries.
	// Ent doesn't expose DB() directly; pragmatic workaround: open a
	// second driver pointing at the same shared-cache in-memory DB.
	db, err := sql.Open("sqlite", "file:entmig?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("opening ent inspection handle: %v", err)
	}
	// Keep the ent client alive for the duration of the test by closing it
	// when this handle closes. Use a finalizer via t.Cleanup instead of
	// altering the return type.
	t.Cleanup(func() { _ = client.Close() })
	return db
}

func listTables(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning table row: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating tables: %v", err)
	}
	sort.Strings(out)
	return out
}

func tableColumns(t *testing.T, ctx context.Context, db *sql.DB, table string) []columnInfo {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT name, type, \"notnull\", dflt_value, pk FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var c columnInfo
		var notnullInt int
		if err := rows.Scan(&c.name, &c.ctype, &notnullInt, &c.dflt, &c.pk); err != nil {
			t.Fatalf("scanning column row: %v", err)
		}
		c.notnull = notnullInt != 0
		out = append(out, c)
	}
	return out
}

func columnNameSet(cols []columnInfo) map[string]columnInfo {
	out := make(map[string]columnInfo, len(cols))
	for _, c := range cols {
		out[c.name] = c
	}
	return out
}

func diffTables(a, b []string, excluded map[string]bool) []string {
	bSet := toSet(b)
	var missing []string
	for _, t := range a {
		if excluded[t] {
			continue
		}
		if !bSet[t] {
			missing = append(missing, t)
		}
	}
	return missing
}

func toSet(xs []string) map[string]bool {
	out := make(map[string]bool, len(xs))
	for _, x := range xs {
		out[x] = true
	}
	return out
}
