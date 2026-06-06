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

// Package entc — migration alpha (α).
//
// Migration α upgrades a legacy raw-SQL Hub database (the ~53-migration,
// 30-table schema produced by the now-removed pkg/store/sqlite store) to the
// consolidated Ent-backed SQLite schema. It runs in-process on first boot when
// the hub detects a legacy schema, behind an automatic backup.
//
// Strategy (validated against four real-world production hub.db files):
//
//  1. Detect the legacy schema by the presence of the `schema_migrations`
//     bookkeeping table plus the legacy-only `agents.agent_id` column.
//  2. Back up the original file (checkpoint WAL, then copy to
//     hub.db.bak.<timestamp>).
//  3. AutoMigrate a fresh Ent schema into a temporary file.
//  4. ATTACH the legacy file and copy every table with `INSERT … SELECT`,
//     applying the mechanical schema differences (column renames such as
//     created_at→created / agent_id→slug, the policies→access_policies and
//     group_members→group_memberships table renames, surrogate-id synthesis for
//     the formerly composite-keyed tables, and the polymorphic
//     member_type/principal_type split). SQLite's dynamic typing carries
//     bool-as-int, JSON-as-TEXT and timestamp text across unchanged, and Ent
//     reads them back natively (verified end-to-end).
//  5. Verify per-table row counts match.
//  6. Atomically swap the migrated file into place.
//
// Foreign keys are disabled on the loader connection so the copy is insensitive
// to insertion order and to any dangling references the legacy data already
// contained; the live store re-enables them for all subsequent writes.
package entc

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	entschema "entgo.io/ent/dialect/sql/schema"
	"entgo.io/ent/schema/field"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
	"github.com/google/uuid"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// migrationNamespace is the fixed UUIDv5 namespace used to deterministically
// rewrite legacy non-UUID primary keys (e.g. the internal "hub-…-signing_key"
// secret ids and "plugin-broker-…" runtime-broker ids) into the UUIDs the Ent
// schema requires. Deterministic so that a key and every foreign-key reference
// to it map to the same UUID, and so re-deriving the value is stable.
var migrationNamespace = uuid.MustParse("5c104390-a1d0-5e9a-9b1e-5c104390a1d0")

// remapSource is a legacy table whose primary key may hold a non-UUID string.
type remapSource struct {
	table string
	pk    string
}

// remapSources are the legacy tables observed (and known possible) to carry
// non-UUID primary keys. Their keys are rewritten to deterministic UUIDs.
var remapSources = []remapSource{
	{table: "secrets", pk: "id"},
	{table: "runtime_brokers", pk: "id"},
}

// remapRefColumns maps an Ent table to the columns that reference a remappable
// id and must be rewritten with the same mapping. Includes the primary keys of
// the remapped entities themselves plus every foreign key that points at a
// runtime broker (whether typed UUID or TEXT in the Ent schema).
var remapRefColumns = map[string][]string{
	"secrets":              {"id"},
	"runtime_brokers":      {"id"},
	"agents":               {"runtime_broker_id"},
	"broker_secrets":       {"broker_id"},
	"broker_join_tokens":   {"broker_id"},
	"project_contributors": {"broker_id"},
	"project_sync_state":   {"broker_id"},
	"projects":             {"default_runtime_broker_id"},
}

// uuidGenExpr is a SQLite expression that mints a syntactically valid v4-style
// UUID string (8-4-4-4-12 hex). Used to synthesize primary keys for legacy
// tables that had no `id` column (composite-keyed project_contributors /
// project_sync_state, and the restructured group_memberships / policy_bindings).
const uuidGenExpr = `lower(hex(randomblob(4))||'-'||hex(randomblob(2))||'-'||hex(randomblob(2))||'-'||hex(randomblob(2))||'-'||hex(randomblob(6)))`

// nowExpr is a SQLite expression yielding the current UTC time in the RFC3339
// form Ent stores. Used for required timestamp columns the legacy schema lacked
// (e.g. policy_bindings.created).
const nowExpr = `strftime('%Y-%m-%dT%H:%M:%fZ','now')`

// AlphaTableResult records the outcome of migrating one legacy table.
type AlphaTableResult struct {
	EntTable    string
	LegacyTable string
	Source      int // rows in the legacy table
	Dest        int // rows in the destination table after the copy
}

// AlphaReport is the aggregate outcome of a migration α run.
type AlphaReport struct {
	BackupPath string
	SourcePath string
	Tables     []AlphaTableResult
	ChildEdges int    // group_child_groups edges copied
	Skipped    bool   // true when the source was not a legacy schema (no-op)
	SkipReason string // populated when Skipped is true
}

// TotalRows returns the total number of destination rows written across all
// tables (excluding M2M child-group edges).
func (r *AlphaReport) TotalRows() int {
	n := 0
	for _, t := range r.Tables {
		n += t.Dest
	}
	return n
}

// AlphaOptions tunes a migration α run.
type AlphaOptions struct {
	// Logf, if non-nil, receives one human-readable progress line per step.
	Logf func(format string, args ...any)
	// BackupSuffix overrides the timestamp suffix appended to the backup file
	// name. Primarily for deterministic tests; defaults to time.Now().
	BackupSuffix string
}

func (o AlphaOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// IsLegacyRawSQLSchema reports whether the SQLite file at path holds a legacy
// raw-SQL Hub schema (as opposed to the consolidated Ent schema, an empty file,
// or a non-existent file). Detection is conservative: it requires both the
// `schema_migrations` bookkeeping table — which the Ent store never creates —
// and the legacy-only `agents.agent_id` column.
func IsLegacyRawSQLSchema(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer db.Close()

	hasMigrations, err := tableExists(db, "schema_migrations")
	if err != nil {
		return false, err
	}
	if !hasMigrations {
		return false, nil
	}
	hasAgents, err := tableExists(db, "agents")
	if err != nil {
		return false, err
	}
	if !hasAgents {
		return false, nil
	}
	cols, err := tableColumns(db, "agents")
	if err != nil {
		return false, err
	}
	return cols["agent_id"], nil
}

// MigrateAlphaSQLite upgrades the legacy raw-SQL Hub database at path in place to
// the consolidated Ent schema. It is a no-op (Skipped=true) when the file is not
// a legacy schema, which makes re-running on an already-migrated database safe.
//
// On success the original file has been replaced by the Ent-schema database and a
// backup of the original remains at <path>.bak.<timestamp>. On any failure the
// original file is left untouched and the temporary working file is removed.
func MigrateAlphaSQLite(ctx context.Context, path string, opts AlphaOptions) (*AlphaReport, error) {
	report := &AlphaReport{SourcePath: path}

	legacy, err := IsLegacyRawSQLSchema(path)
	if err != nil {
		return nil, fmt.Errorf("detecting schema: %w", err)
	}
	if !legacy {
		report.Skipped = true
		report.SkipReason = "not a legacy raw-SQL schema (already Ent, empty, or absent)"
		opts.logf("migration α: %s", report.SkipReason)
		return report, nil
	}

	// 1) Back up the original (fold WAL in first so the copy is complete).
	backupPath, err := backupLegacy(path, opts)
	if err != nil {
		return nil, fmt.Errorf("backing up legacy database: %w", err)
	}
	report.BackupPath = backupPath
	opts.logf("migration α: backed up %s -> %s", path, backupPath)

	// 2) Build the Ent schema into a fresh temporary file.
	tmpPath := path + ".migrating"
	removeSQLiteFiles(tmpPath)
	defer removeSQLiteFiles(tmpPath) // cleaned up unless we successfully swap it in

	if err := buildEntSchema(ctx, tmpPath); err != nil {
		return nil, fmt.Errorf("creating Ent schema: %w", err)
	}
	opts.logf("migration α: created Ent schema in %s", tmpPath)

	// 3) Copy all data from the legacy file into the new Ent file.
	if err := copyLegacyData(ctx, tmpPath, path, report, opts); err != nil {
		return nil, fmt.Errorf("copying data: %w", err)
	}

	// 4) Atomically replace the original with the migrated file. The original is
	// already safely backed up.
	removeSQLiteFiles(path)
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("swapping migrated database into place: %w", err)
	}
	opts.logf("migration α: complete — %d tables, %d rows migrated", len(report.Tables), report.TotalRows())
	return report, nil
}

// backupLegacy checkpoints the legacy WAL and copies the database to a
// timestamped backup file, returning the backup path.
func backupLegacy(path string, opts AlphaOptions) (string, error) {
	// Fold any WAL frames into the main file so a plain copy is complete.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return "", err
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		// Non-fatal: a DB in rollback-journal mode has no WAL to checkpoint.
		opts.logf("migration α: wal_checkpoint skipped: %v", err)
	}
	db.Close()

	suffix := opts.BackupSuffix
	if suffix == "" {
		suffix = time.Now().UTC().Format("20060102-150405")
	}
	backupPath := path + ".bak." + suffix
	if err := copyFile(path, backupPath); err != nil {
		return "", err
	}
	return backupPath, nil
}

// buildEntSchema creates the destination Ent schema in a new file at tmpPath.
func buildEntSchema(ctx context.Context, tmpPath string) error {
	client, err := OpenSQLite(tmpPath, PoolConfig{})
	if err != nil {
		return err
	}
	defer client.Close()
	return AutoMigrate(ctx, client)
}

// tableMap describes how one Ent table is populated from a legacy table via the
// generic INSERT…SELECT path. The three structurally-restructured tables
// (group_memberships, policy_bindings) are handled by bespoke SQL instead.
type tableMap struct {
	entTable    string
	legacyTable string
	// overrides maps an Ent column name to a raw SQLite select expression,
	// bypassing the automatic same-name / created_at→created mapping. Used for
	// the agents agent_id→slug rename and for synthesizing surrogate ids.
	overrides map[string]string
}

// genericTables lists every table copied by the column-name-driven engine, in
// parent-before-child order (cosmetic only — foreign keys are off during load).
var genericTables = []tableMap{
	{entTable: "users", legacyTable: "users"},
	{entTable: "projects", legacyTable: "projects"},
	{entTable: "runtime_brokers", legacyTable: "runtime_brokers"},
	{entTable: "agents", legacyTable: "agents", overrides: map[string]string{"slug": `"agent_id"`}},
	{entTable: "groups", legacyTable: "groups"},
	{entTable: "access_policies", legacyTable: "policies"},
	{entTable: "templates", legacyTable: "templates"},
	{entTable: "harness_configs", legacyTable: "harness_configs"},
	{entTable: "secrets", legacyTable: "secrets"},
	{entTable: "env_vars", legacyTable: "env_vars"},
	{entTable: "project_contributors", legacyTable: "project_contributors", overrides: map[string]string{"id": uuidGenExpr}},
	{entTable: "project_sync_state", legacyTable: "project_sync_state", overrides: map[string]string{"id": uuidGenExpr}},
	{entTable: "broker_secrets", legacyTable: "broker_secrets"},
	{entTable: "broker_join_tokens", legacyTable: "broker_join_tokens"},
	{entTable: "notification_subscriptions", legacyTable: "notification_subscriptions"},
	{entTable: "notifications", legacyTable: "notifications"},
	{entTable: "subscription_templates", legacyTable: "subscription_templates"},
	{entTable: "scheduled_events", legacyTable: "scheduled_events"},
	{entTable: "schedules", legacyTable: "schedules"},
	{entTable: "messages", legacyTable: "messages"},
	{entTable: "gcp_service_accounts", legacyTable: "gcp_service_accounts"},
	{entTable: "github_installations", legacyTable: "github_installations"},
	{entTable: "maintenance_operations", legacyTable: "maintenance_operations"},
	{entTable: "maintenance_operation_runs", legacyTable: "maintenance_operation_runs"},
	{entTable: "api_keys", legacyTable: "api_keys"},
	{entTable: "user_access_tokens", legacyTable: "user_access_tokens"},
	{entTable: "allow_list", legacyTable: "allow_list"},
	{entTable: "invite_codes", legacyTable: "invite_codes"},
}

// copyLegacyData opens the new Ent database, attaches the legacy database, and
// copies every table with foreign keys disabled.
func copyLegacyData(ctx context.Context, dstPath, legacyPath string, report *AlphaReport, opts AlphaOptions) error {
	db, err := sql.Open("sqlite", "file:"+dstPath)
	if err != nil {
		return err
	}
	defer db.Close()
	// Pin to one connection so PRAGMAs and the ATTACH apply to every statement.
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disabling foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ATTACH DATABASE ? AS legacy`, "file:"+legacyPath+"?mode=ro"); err != nil {
		return fmt.Errorf("attaching legacy database: %w", err)
	}
	defer db.ExecContext(ctx, "DETACH DATABASE legacy") //nolint:errcheck

	// Build the id-remap table (legacy non-UUID primary keys -> deterministic
	// UUIDs) before copying, so every reference resolves consistently. The table
	// is TEMP, so it never lands in the migrated file.
	if err := buildIDRemap(ctx, db, opts); err != nil {
		return fmt.Errorf("building id remap: %w", err)
	}

	entCols := entColumnsByTable()

	for _, tm := range genericTables {
		cols, ok := entCols[tm.entTable]
		if !ok {
			return fmt.Errorf("unknown Ent table %q (generated schema drift?)", tm.entTable)
		}
		res, err := copyGenericTable(ctx, db, tm, cols)
		if err != nil {
			return fmt.Errorf("copying %s: %w", tm.entTable, err)
		}
		report.Tables = append(report.Tables, res)
		opts.logf("migration α: %-26s source=%d dest=%d", tm.entTable, res.Source, res.Dest)
	}

	// Restructured tables: polymorphic membership and policy-binding splits.
	memberships, err := copyGroupMemberships(ctx, db)
	if err != nil {
		return fmt.Errorf("copying group_memberships: %w", err)
	}
	report.Tables = append(report.Tables, memberships)
	opts.logf("migration α: %-26s source=%d dest=%d", "group_memberships", memberships.Source, memberships.Dest)

	bindings, err := copyPolicyBindings(ctx, db)
	if err != nil {
		return fmt.Errorf("copying policy_bindings: %w", err)
	}
	report.Tables = append(report.Tables, bindings)
	opts.logf("migration α: %-26s source=%d dest=%d", "policy_bindings", bindings.Source, bindings.Dest)

	// The Group.child_groups M2M edge, derived from legacy groups.parent_id.
	edges, err := copyGroupChildEdgesSQL(ctx, db)
	if err != nil {
		return fmt.Errorf("copying group child edges: %w", err)
	}
	report.ChildEdges = edges

	return nil
}

// copyGenericTable performs the column-name-driven INSERT…SELECT for one table
// and verifies the row counts match.
func copyGenericTable(ctx context.Context, db *sql.DB, tm tableMap, entCols []*entschema.Column) (AlphaTableResult, error) {
	res := AlphaTableResult{EntTable: tm.entTable, LegacyTable: tm.legacyTable}

	legacyCols, err := attachedTableColumns(ctx, db, tm.legacyTable)
	if err != nil {
		return res, err
	}
	if len(legacyCols) == 0 {
		// The legacy database does not have this table (e.g. an older schema
		// version). Nothing to copy.
		return res, nil
	}

	var destNames, selectExprs []string
	for _, c := range entCols {
		if ov, ok := tm.overrides[c.Name]; ok {
			destNames = append(destNames, quoteIdent(c.Name))
			selectExprs = append(selectExprs, ov)
			continue
		}
		src := legacySourceColumn(c.Name, legacyCols)
		if src == "" {
			continue // no legacy source; rely on the Ent column default
		}
		expr := coerceExpr(src, c)
		if isRemapColumn(tm.entTable, c.Name) {
			expr = remapWrap(expr)
		}
		destNames = append(destNames, quoteIdent(c.Name))
		selectExprs = append(selectExprs, expr)
	}
	if len(destNames) == 0 {
		return res, fmt.Errorf("no column mapping for %s", tm.entTable)
	}

	stmt := fmt.Sprintf("INSERT INTO main.%s (%s) SELECT %s FROM legacy.%s",
		quoteIdent(tm.entTable), strings.Join(destNames, ", "),
		strings.Join(selectExprs, ", "), quoteIdent(tm.legacyTable))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return res, fmt.Errorf("insert: %w", err)
	}

	res.Source, err = countRows(ctx, db, "legacy."+quoteIdent(tm.legacyTable))
	if err != nil {
		return res, err
	}
	res.Dest, err = countRows(ctx, db, "main."+quoteIdent(tm.entTable))
	if err != nil {
		return res, err
	}
	if res.Source != res.Dest {
		return res, fmt.Errorf("row count mismatch: legacy=%d dest=%d", res.Source, res.Dest)
	}
	return res, nil
}

// legacySourceColumn resolves the legacy column that feeds an Ent column,
// applying the systematic created_at→created / updated_at→updated renames.
// Returns "" when the legacy table has no corresponding column.
func legacySourceColumn(entCol string, legacyCols map[string]bool) string {
	if legacyCols[entCol] {
		return entCol
	}
	switch entCol {
	case "created":
		if legacyCols["created_at"] {
			return "created_at"
		}
	case "updated":
		if legacyCols["updated_at"] {
			return "updated_at"
		}
	}
	return ""
}

// coerceExpr wraps a legacy column reference with any conversion needed for the
// destination Ent column type. SQLite's dynamic typing handles bool-as-int and
// timestamp text transparently, so only two cases need care:
//   - nullable JSON columns: an empty string is not valid JSON, so map ”→NULL.
//   - nullable UUID columns: legacy free-text values (or ”) that are not a
//     36-char UUID are mapped to NULL so Ent's uuid.Scan never fails on read.
func coerceExpr(legacyCol string, c *entschema.Column) string {
	ref := quoteIdent(legacyCol)
	switch {
	case c.Type == field.TypeJSON && c.Nullable:
		return "NULLIF(" + ref + ", '')"
	case c.Type == field.TypeUUID && c.Nullable:
		return fmt.Sprintf("CASE WHEN %s LIKE '________-____-____-____-____________' THEN %s ELSE NULL END", ref, ref)
	default:
		return ref
	}
}

// isRemapColumn reports whether the given Ent table/column holds a reference to
// a remappable (formerly non-UUID) id and must be rewritten via _id_remap.
func isRemapColumn(entTable, entCol string) bool {
	for _, c := range remapRefColumns[entTable] {
		if c == entCol {
			return true
		}
	}
	return false
}

// remapWrap rewrites a column reference through the _id_remap table: a legacy id
// present in the map yields its deterministic UUID; anything else (already a
// UUID, or NULL) passes through unchanged.
func remapWrap(expr string) string {
	return "COALESCE((SELECT new FROM _id_remap WHERE old = " + expr + "), " + expr + ")"
}

// buildIDRemap creates and populates the TEMP _id_remap table mapping each
// legacy non-UUID primary key to a deterministic UUID. Already-UUID keys are not
// added (they pass through remapWrap unchanged).
func buildIDRemap(ctx context.Context, db *sql.DB, opts AlphaOptions) error {
	if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE _id_remap (old TEXT PRIMARY KEY, new TEXT NOT NULL)`); err != nil {
		return err
	}
	total := 0
	for _, rs := range remapSources {
		rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT DISTINCT %s FROM legacy.%s", quoteIdent(rs.pk), quoteIdent(rs.table)))
		if err != nil {
			return err
		}
		var legacyIDs []string
		for rows.Next() {
			var id sql.NullString
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !id.Valid || id.String == "" {
				continue
			}
			if _, err := uuid.Parse(id.String); err == nil {
				continue // already a valid UUID
			}
			legacyIDs = append(legacyIDs, id.String)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, old := range legacyIDs {
			newID := uuid.NewSHA1(migrationNamespace, []byte(old)).String()
			if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO _id_remap (old, new) VALUES (?, ?)`, old, newID); err != nil {
				return err
			}
			total++
			opts.logf("migration α: remap id %-40s -> %s (%s)", old, newID, rs.table)
		}
	}
	if total > 0 {
		opts.logf("migration α: rewrote %d non-UUID id(s) to deterministic UUIDs", total)
	}
	return nil
}

// copyGroupMemberships migrates the legacy `group_members` table (composite key
// group_id/member_type/member_id) into Ent `group_memberships`, splitting the
// polymorphic member into user_id / agent_id and minting a surrogate id.
func copyGroupMemberships(ctx context.Context, db *sql.DB) (AlphaTableResult, error) {
	res := AlphaTableResult{EntTable: "group_memberships", LegacyTable: "group_members"}
	if ok, err := attachedTableExists(ctx, db, "group_members"); err != nil || !ok {
		return res, err
	}
	stmt := fmt.Sprintf(`INSERT INTO main.group_memberships (id, group_id, user_id, agent_id, role, added_at, added_by)
		SELECT %s, group_id,
		       CASE WHEN member_type='user'  THEN member_id END,
		       CASE WHEN member_type='agent' THEN member_id END,
		       role, added_at, added_by
		FROM legacy.group_members`, uuidGenExpr)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return res, err
	}
	return countSourceDest(ctx, db, res, "legacy.group_members", "main.group_memberships")
}

// copyPolicyBindings migrates the legacy composite-keyed `policy_bindings` into
// the Ent table, splitting the polymorphic principal into user_id / group_id /
// agent_id, minting a surrogate id, and stamping a `created` time the legacy
// schema did not record.
func copyPolicyBindings(ctx context.Context, db *sql.DB) (AlphaTableResult, error) {
	res := AlphaTableResult{EntTable: "policy_bindings", LegacyTable: "policy_bindings"}
	if ok, err := attachedTableExists(ctx, db, "policy_bindings"); err != nil || !ok {
		return res, err
	}
	stmt := fmt.Sprintf(`INSERT INTO main.policy_bindings (id, policy_id, principal_type, user_id, group_id, agent_id, created)
		SELECT %s, policy_id, principal_type,
		       CASE WHEN principal_type='user'  THEN principal_id END,
		       CASE WHEN principal_type='group' THEN principal_id END,
		       CASE WHEN principal_type='agent' THEN principal_id END,
		       %s
		FROM legacy.policy_bindings`, uuidGenExpr, nowExpr)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return res, err
	}
	return countSourceDest(ctx, db, res, "legacy.policy_bindings", "main.policy_bindings")
}

// copyGroupChildEdgesSQL populates the group_child_groups M2M join table from
// legacy groups that carried a parent_id. Idempotent within a fresh run.
func copyGroupChildEdgesSQL(ctx context.Context, db *sql.DB) (int, error) {
	cols, err := attachedTableColumns(ctx, db, "groups")
	if err != nil {
		return 0, err
	}
	if !cols["parent_id"] {
		// No legacy groups table, or a schema without parent hierarchy.
		return 0, nil
	}
	// Only legacy groups with a non-empty parent_id contribute an edge.
	stmt := `INSERT INTO main.group_child_groups (group_id, parent_group_id)
		SELECT parent_id, id FROM legacy.groups
		WHERE parent_id IS NOT NULL AND parent_id <> ''`
	r, err := db.ExecContext(ctx, stmt)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// --- small helpers ---

func countSourceDest(ctx context.Context, db *sql.DB, res AlphaTableResult, srcQ, dstQ string) (AlphaTableResult, error) {
	var err error
	if res.Source, err = countRows(ctx, db, srcQ); err != nil {
		return res, err
	}
	if res.Dest, err = countRows(ctx, db, dstQ); err != nil {
		return res, err
	}
	if res.Source != res.Dest {
		return res, fmt.Errorf("row count mismatch: source=%d dest=%d", res.Source, res.Dest)
	}
	return res, nil
}

func countRows(ctx context.Context, db *sql.DB, qualifiedTable string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+qualifiedTable).Scan(&n)
	return n, err
}

// entColumnsByTable indexes the generated Ent schema columns by table name.
func entColumnsByTable() map[string][]*entschema.Column {
	out := make(map[string][]*entschema.Column, len(migrate.Tables))
	for _, t := range migrate.Tables {
		out[t.Name] = t.Columns
	}
	return out
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0, err
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// attachedTableExists reports whether a table exists in the ATTACHed legacy DB.
func attachedTableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM legacy.sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0, err
}

// attachedTableColumns reads the columns of a table in the ATTACHed legacy DB.
func attachedTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA legacy.table_info(%s)", quoteIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// quoteIdent double-quotes a SQLite identifier.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeSQLiteFiles removes a SQLite database file and its WAL/SHM sidecars.
func removeSQLiteFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}
