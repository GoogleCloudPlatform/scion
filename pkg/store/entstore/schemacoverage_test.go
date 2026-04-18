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
	"fmt"
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

// TestSchemaCoverage_ColumnAttributes compares column type, NOT NULL flag,
// default value, and PK flag between raw SQL and Ent for every shared
// column. Drift entries must be listed in knownAttributeDrift with a reason.
//
// This is the stricter gate that catches subtle DDL differences
// (nullability, types, PK structure) that Phase 2's AutoMigrate would
// otherwise silently "fix" by ALTERing the table.
func TestSchemaCoverage_ColumnAttributes(t *testing.T) {
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
			continue
		}
		rawCols := columnNameSet(tableColumns(t, ctx, rawDB, table))
		entCols := columnNameSet(tableColumns(t, ctx, entDB, table))
		for name, raw := range rawCols {
			ent, ok := entCols[name]
			if !ok {
				continue // reported by TestSchemaCoverage_Columns
			}
			checkAttr(t, table, name, "type", canonType(raw.ctype), canonType(ent.ctype))
			// SQLite quirk: TEXT PRIMARY KEY columns are not enforced as
			// NOT NULL by PRAGMA table_info (only INTEGER PK is, via
			// ROWID). Ent's DDL declares them NOT NULL, which is stricter
			// but behaviorally equivalent because the app never writes
			// NULL ids. Skip the notnull check for PK columns entirely.
			if raw.pk == 0 && ent.pk == 0 {
				checkAttr(t, table, name, "notnull", fmt.Sprintf("%v", raw.notnull), fmt.Sprintf("%v", ent.notnull))
			}
			checkAttr(t, table, name, "default", canonDefault(raw.dflt), canonDefault(ent.dflt))
			checkAttr(t, table, name, "pk", fmt.Sprintf("%d", boolToInt(raw.pk > 0)), fmt.Sprintf("%d", boolToInt(ent.pk > 0)))
		}
	}
}

func checkAttr(t *testing.T, table, col, aspect, raw, ent string) {
	t.Helper()
	if raw == ent {
		return
	}
	key := fmt.Sprintf("%s.%s.%s", table, col, aspect)
	if reason, ok := knownAttributeDrift[key]; ok {
		t.Logf("drift ok @ %s: raw=%q ent=%q (%s)", key, raw, ent, reason)
		return
	}
	t.Errorf("attribute mismatch @ %s: raw=%q ent=%q — add to knownAttributeDrift with a reason, or fix the schema",
		key, raw, ent)
}

// canonType lowercases and collapses SQLite type synonyms so INT==INTEGER
// and TEXT==VARCHAR(n), which SQLite treats as equivalent via affinity.
func canonType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Strip length: VARCHAR(255) -> varchar.
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	switch s {
	case "int", "integer", "bigint", "smallint", "tinyint", "int2", "int8":
		return "integer"
	case "varchar", "text", "clob", "character", "nvarchar", "nchar",
		"uuid", "json", "jsonb":
		// SQLite has no native UUID or JSON types; both get TEXT
		// affinity. Ent emits the literal keyword but storage is TEXT.
		return "text"
	case "real", "double", "float", "numeric", "decimal":
		return "real"
	case "blob", "bytea":
		return "blob"
	case "bool", "boolean":
		return "integer" // SQLite stores bool as integer affinity
	case "timestamp", "datetime", "date", "time":
		return "datetime"
	case "":
		return "text" // SQLite: untyped columns take TEXT affinity
	}
	return s
}

// canonDefault normalizes SQLite default expressions. Ent doesn't emit SQL
// defaults for Go-side time.Now() values, while raw SQL uses
// CURRENT_TIMESTAMP. These are documented as tolerated drift.
func canonDefault(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	s := strings.TrimSpace(v.String)
	// SQLite wraps some defaults in quotes; canonicalize '0' and "0" to 0.
	s = strings.Trim(s, "'\"")
	s = strings.ToLower(s)
	// Ent emits bool defaults as "false"/"true"; raw SQL uses "0"/"1".
	// Both store the same integer on disk under SQLite's bool-as-integer
	// affinity. Canonicalize to the numeric form.
	switch s {
	case "false":
		return "0"
	case "true":
		return "1"
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// knownAttributeDrift lists (table, column, aspect) tuples where raw SQL and
// Ent intentionally disagree at the column-attribute level. Each entry has
// a short reason comment. Everything not listed must match.
//
// aspect is one of: "type", "notnull", "default", "pk".
var knownAttributeDrift = map[string]string{
	// Ent models template as Optional (NULLABLE) so the CompositeStore can
	// create shadow agent records without a template value. Raw SQL keeps
	// the column NOT NULL. Constraint weakening is safe: existing rows
	// satisfy the weaker constraint.
	"agents.template.notnull": "Ent Optional for shadow records",

	// Ent uses Go-side time.Now() defaults; raw SQL uses
	// DEFAULT CURRENT_TIMESTAMP. Functionally equivalent.
	"agents.created_at.default":                         "Go-side default vs CURRENT_TIMESTAMP",
	"agents.updated_at.default":                         "Go-side default vs CURRENT_TIMESTAMP",
	"groves.created_at.default":                         "Go-side default vs CURRENT_TIMESTAMP",
	"groves.updated_at.default":                         "Go-side default vs CURRENT_TIMESTAMP",
	"users.created_at.default":                          "Go-side default vs CURRENT_TIMESTAMP",
	"runtime_brokers.created_at.default":                "Go-side default vs CURRENT_TIMESTAMP",
	"runtime_brokers.updated_at.default":                "Go-side default vs CURRENT_TIMESTAMP",
	"templates.created_at.default":                      "Go-side default vs CURRENT_TIMESTAMP",
	"templates.updated_at.default":                      "Go-side default vs CURRENT_TIMESTAMP",
	"harness_configs.created_at.default":                "Go-side default vs CURRENT_TIMESTAMP",
	"harness_configs.updated_at.default":                "Go-side default vs CURRENT_TIMESTAMP",
	"env_vars.created_at.default":                       "Go-side default vs CURRENT_TIMESTAMP",
	"env_vars.updated_at.default":                       "Go-side default vs CURRENT_TIMESTAMP",
	"secrets.created_at.default":                        "Go-side default vs CURRENT_TIMESTAMP",
	"secrets.updated_at.default":                        "Go-side default vs CURRENT_TIMESTAMP",
	"broker_secrets.created_at.default":                 "Go-side default vs CURRENT_TIMESTAMP",
	"broker_join_tokens.created_at.default":             "Go-side default vs CURRENT_TIMESTAMP",
	"notification_subscriptions.created_at.default":     "Go-side default vs CURRENT_TIMESTAMP",
	"notifications.created_at.default":                  "Go-side default vs CURRENT_TIMESTAMP",
	"scheduled_events.created_at.default":               "Go-side default vs CURRENT_TIMESTAMP",
	"schedules.created_at.default":                      "Go-side default vs CURRENT_TIMESTAMP",
	"schedules.updated_at.default":                      "Go-side default vs CURRENT_TIMESTAMP",
	"gcp_service_accounts.created_at.default":           "Go-side default vs CURRENT_TIMESTAMP",
	"github_installations.created_at.default":           "Go-side default vs CURRENT_TIMESTAMP",
	"github_installations.updated_at.default":           "Go-side default vs CURRENT_TIMESTAMP",
	"messages.created_at.default":                       "Go-side default vs CURRENT_TIMESTAMP",
	"maintenance_operations.created_at.default":         "Go-side default vs CURRENT_TIMESTAMP",
	"maintenance_operation_runs.started_at.default":     "Go-side default vs CURRENT_TIMESTAMP",
	"grove_contributors.created_at.default":             "Go-side default vs CURRENT_TIMESTAMP (new Ent-only column)",

	// grove_contributors and grove_sync_state use composite PKs in raw SQL
	// but Ent requires a surrogate id. Phase 2 will drop these tables
	// (low-value data, self-heals on next broker heartbeat/sync) and let
	// Ent recreate them.
	"grove_contributors.grove_id.pk":   "composite PK in raw SQL; Phase 2 drops table",
	"grove_contributors.broker_id.pk":  "composite PK in raw SQL; Phase 2 drops table",
	"grove_sync_state.grove_id.pk":     "composite PK in raw SQL; Phase 2 drops table",
	"grove_sync_state.broker_id.pk":    "composite PK in raw SQL; Phase 2 drops table",
	"grove_sync_state.broker_id.notnull": "raw SQL: NOT NULL DEFAULT ''; Ent surrogate doesn't need it",

	// grove_contributors gets an extra created_at column in the Ent schema
	// that raw SQL lacks. Harmless: ADD COLUMN on upgrade.
	"grove_contributors.created_at.notnull": "Ent-only column added by Phase 2 migration",

	// Columns declared in raw SQL with a DEFAULT but without NOT NULL.
	// Ent emits NOT NULL DEFAULT x because .Default(...) without Optional()
	// implies non-nullable. The app never writes NULL to these columns
	// (backfill migrations ensure it), so Phase 2's AutoMigrate tightening
	// is safe for existing data.
	"agents.connection_state.notnull":          "raw DEFAULT without NOT NULL; Ent tightens",
	"agents.tool_name.notnull":                 "raw DEFAULT without NOT NULL; Ent tightens",
	"agents.current_turns.notnull":             "raw DEFAULT without NOT NULL; Ent tightens",
	"agents.current_model_calls.notnull":       "raw DEFAULT without NOT NULL; Ent tightens",
	"agents.activity.notnull":                  "raw DEFAULT without NOT NULL; Ent tightens",
	"agents.stalled_from_activity.notnull":     "raw DEFAULT without NOT NULL; Ent tightens",
	"runtime_brokers.connection_state.notnull": "raw DEFAULT without NOT NULL; Ent tightens",
	"scheduled_events.schedule_id.notnull":     "raw DEFAULT without NOT NULL; Ent tightens",
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
