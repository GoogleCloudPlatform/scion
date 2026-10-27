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

package entc

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// openFKTestDB opens a file-backed SQLite database pinned to one connection
// with foreign-key enforcement off, so tests can seed orphaned rows the way
// a database written without the pragma would hold them.
func openFKTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "fk.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, "PRAGMA foreign_keys = OFF")
	return db
}

func mustExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
}

// captureSlog routes the default slog logger into a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func fkCountRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query).Scan(&n), query)
	return n
}

// seedSyntheticFKSchema creates a parent table and one child table per
// ON DELETE action, each holding one valid row and one orphaned row.
func seedSyntheticFKSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE c_cascade (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE CASCADE)`,
		`CREATE TABLE c_setnull (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE SET NULL)`,
		`CREATE TABLE c_implicit (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent ON DELETE SET NULL)`,
		`INSERT INTO parent VALUES ('p1')`,
		`INSERT INTO c_cascade VALUES ('ok', 'p1'), ('orphan', 'gone'), ('null', NULL)`,
		`INSERT INTO c_setnull VALUES ('ok', 'p1'), ('orphan', 'gone')`,
		`INSERT INTO c_implicit VALUES ('ok', 'p1'), ('orphan', 'gone')`,
	)
}

func TestReconcileSQLiteForeignKeys_AppliesDeclaredActions(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	seedSyntheticFKSchema(t, db)
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Empty(t, res.Unrepaired)
	require.Empty(t, res.diagnostic())

	// CASCADE: the orphan is deleted; the valid row and the NULL-key row stay.
	require.Equal(t, 2, fkCountRows(t, db, `SELECT count(*) FROM c_cascade`))
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM c_cascade WHERE id = 'orphan'`))
	// SET NULL: the orphan's key is cleared, the row and the valid row stay.
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c_setnull WHERE id = 'orphan' AND pid IS NULL`))
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c_setnull WHERE id = 'ok' AND pid = 'p1'`))
	// A REFERENCES clause without a column list resolves to the parent key.
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c_implicit WHERE id = 'orphan' AND pid IS NULL`))
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c_implicit WHERE id = 'ok' AND pid = 'p1'`))
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`))

	// The connection's pragma state is restored.
	require.Equal(t, 1, fkCountRows(t, db, `PRAGMA foreign_keys`))

	// Idempotent.
	res, err = reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Empty(t, res.Unrepaired)
}

func TestReconcileSQLiteForeignKeys_FollowsCascades(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE a (id TEXT PRIMARY KEY)`,
		`CREATE TABLE b (id TEXT PRIMARY KEY, aid TEXT REFERENCES a(id) ON DELETE CASCADE)`,
		`CREATE TABLE c (id TEXT PRIMARY KEY, bid TEXT REFERENCES b(id) ON DELETE SET NULL)`,
		`INSERT INTO b VALUES ('b1', 'gone')`,
		`INSERT INTO c VALUES ('c1', 'b1')`,
	)
	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Empty(t, res.Unrepaired)
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM b`))
	// c1 was orphaned by the cascade, and gets its own declared action.
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c WHERE id = 'c1' AND bid IS NULL`))
}

func TestReconcileSQLiteForeignKeys_LeavesUnrepairableAndReports(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	seedSyntheticFKSchema(t, db)
	mustExec(t, db,
		`CREATE TABLE c_noaction (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id))`,
		`CREATE TABLE c_restrict (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE RESTRICT)`,
		`CREATE TABLE c_notnull (id TEXT PRIMARY KEY, pid TEXT NOT NULL REFERENCES parent(id) ON DELETE SET NULL)`,
		`INSERT INTO c_noaction VALUES ('orphan', 'gone')`,
		`INSERT INTO c_restrict VALUES ('orphan', 'gone')`,
		`INSERT INTO c_notnull VALUES ('orphan', 'gone')`,
	)

	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)

	// The declared actions are still applied to the repairable keys.
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM c_cascade WHERE id = 'orphan'`))
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c_setnull WHERE id = 'orphan' AND pid IS NULL`))
	// The rest are untouched.
	for _, tbl := range []string{"c_noaction", "c_restrict", "c_notnull"} {
		require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM `+tbl+` WHERE id = 'orphan' AND pid = 'gone'`), tbl)
	}

	require.Len(t, res.Unrepaired, 3)
	got := map[string]fkOrphanGroup{}
	for _, g := range res.Unrepaired {
		got[g.info.table] = g
		require.Equal(t, 1, g.count, g.info.table)
	}
	require.Equal(t, "NO ACTION", got["c_noaction"].info.onDelete)
	require.Equal(t, "RESTRICT", got["c_restrict"].info.onDelete)
	require.Equal(t, reasonNotNull, got["c_notnull"].reason)

	d := res.diagnostic()
	for _, want := range []string{
		`c_noaction(pid) -> parent(id) ON DELETE NO ACTION: 1 orphaned row(s)`,
		`c_restrict(pid) -> parent(id) ON DELETE RESTRICT: 1 orphaned row(s)`,
		`c_notnull(pid) -> parent(id) ON DELETE SET NULL: 1 orphaned row(s)`,
		`DELETE FROM "c_noaction" WHERE "c_noaction"."pid" IS NOT NULL AND NOT EXISTS (SELECT 1 FROM "parent" AS p WHERE p."id" = "c_noaction"."pid");`,
		`UPDATE "c_noaction" SET "pid" = NULL WHERE`,
	} {
		require.Contains(t, d, want)
	}
	require.NotContains(t, d, `UPDATE "c_notnull"`, "no SET NULL suggestion for a NOT NULL column")

	// The suggested repair SQL works.
	for _, line := range strings.Split(d, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "delete:") {
			mustExec(t, db, strings.TrimSpace(strings.TrimPrefix(line, "delete:")))
		}
	}
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`))
}

// A key whose parent table does not exist is left alone, even with a
// CASCADE action, and the diagnostic only suggests inspecting the rows.
func TestReconcileSQLiteForeignKeys_MissingParentTableLeftAlone(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE child (id TEXT PRIMARY KEY, pid TEXT REFERENCES nosuch(id) ON DELETE CASCADE)`,
		`INSERT INTO child VALUES ('c1', 'x'), ('c2', NULL)`,
	)
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`))

	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Len(t, res.Unrepaired, 1)
	require.Equal(t, reasonNoParent, res.Unrepaired[0].reason)
	require.Equal(t, 2, fkCountRows(t, db, `SELECT count(*) FROM child`))

	d := res.diagnostic()
	require.Contains(t, d, `child(pid) -> nosuch ON DELETE CASCADE: 1 row(s) reference a table that does not exist`)
	const inspect = `SELECT * FROM "child" WHERE "pid" IS NOT NULL;`
	require.Contains(t, d, "inspect: "+inspect)
	require.NotContains(t, d, "DELETE FROM")
	require.NotContains(t, d, "UPDATE")
	// The suggested statement is valid SQL.
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM (`+strings.TrimSuffix(inspect, ";")+`)`))
}

func TestReconcileSQLiteForeignKeys_RollsBackWhenRepairWouldOrphanRows(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE a (id TEXT PRIMARY KEY)`,
		`CREATE TABLE b (id TEXT PRIMARY KEY, aid TEXT REFERENCES a(id) ON DELETE CASCADE)`,
		`CREATE TABLE s (id TEXT PRIMARY KEY, aid TEXT REFERENCES a(id) ON DELETE SET NULL)`,
		// d references b with NO ACTION: deleting the orphan b1 would orphan d1.
		`CREATE TABLE d (id TEXT PRIMARY KEY, bid TEXT REFERENCES b(id))`,
		`INSERT INTO b VALUES ('b1', 'gone')`,
		`INSERT INTO s VALUES ('s1', 'gone')`,
		`INSERT INTO d VALUES ('d1', 'b1')`,
	)
	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.Error(t, err)
	require.True(t, errors.Is(err, errFKRepairIntroducedOrphans), err)
	require.Contains(t, err.Error(), `d(bid) -> b(id) ON DELETE NO ACTION: 1 orphaned row(s) (would be orphaned by the automatic repair)`)
	require.Contains(t, err.Error(), "no changes were made")
	require.Len(t, res.Unrepaired, 1)

	// Nothing changed, not even the independent SET NULL repair.
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM b WHERE id = 'b1' AND aid = 'gone'`))
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM s WHERE id = 's1' AND aid = 'gone'`))
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM d WHERE id = 'd1' AND bid = 'b1'`))
}

func TestReconcileSQLiteForeignKeys_CleanDatabaseUnchanged(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE child (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE CASCADE)`,
		`INSERT INTO parent VALUES ('p1')`,
		`INSERT INTO child VALUES ('c1', 'p1'), ('c2', NULL)`,
		// Any write to child would show up here.
		`CREATE TABLE writes (n INTEGER)`,
		`CREATE TRIGGER child_del AFTER DELETE ON child BEGIN INSERT INTO writes VALUES (1); END`,
		`CREATE TRIGGER child_upd AFTER UPDATE ON child BEGIN INSERT INTO writes VALUES (1); END`,
	)
	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Empty(t, res.Unrepaired)
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM writes`))
	require.Equal(t, 2, fkCountRows(t, db, `SELECT count(*) FROM child`))
}

// --- AutoMigrate against the real schema ---

// openMigratedFile creates a fully migrated hub database at a file path and
// returns the path.
func openMigratedFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	client, err := OpenSQLite(path, PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, AutoMigrate(context.Background(), client))
	require.NoError(t, client.Close())
	return path
}

func openHubFile(t *testing.T, path string) (*ent.Client, *sql.DB) {
	t.Helper()
	client, err := OpenSQLite(path, PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client, SQLDB(client.Driver())
}

// insertRow inserts a row with the given values, filling every other
// NOT NULL column that has no default with a unique placeholder.
func insertRow(t *testing.T, db *sql.DB, table string, vals map[string]any) {
	t.Helper()
	rows, err := db.Query(`SELECT name, "notnull", dflt_value FROM pragma_table_info(?)`, table)
	require.NoError(t, err)
	cols := []string{}
	args := []any{}
	for rows.Next() {
		var (
			name string
			nn   int
			dflt sql.NullString
		)
		require.NoError(t, rows.Scan(&name, &nn, &dflt))
		if v, ok := vals[name]; ok {
			cols, args = append(cols, quoteIdent(name)), append(args, v)
		} else if nn != 0 && !dflt.Valid {
			cols, args = append(cols, quoteIdent(name)), append(args, uuid.NewString())
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteIdent(table), strings.Join(cols, ", "),
		strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", "))
	_, err = db.Exec(q, args...)
	require.NoError(t, err, q)
}

// makeColumnNullable recreates an (empty) table with one NOT NULL column
// relaxed, as an older schema might have declared it. The next AutoMigrate
// must then rebuild the table, which is what trips the check on orphans.
func makeColumnNullable(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	var createSQL string
	require.NoError(t, db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&createSQL))
	re := regexp.MustCompile("([`\"]" + regexp.QuoteMeta(column) + "[`\"] [a-z]+) NOT NULL")
	require.Regexp(t, re, createSQL)
	relaxed := re.ReplaceAllString(createSQL, "$1 NULL")
	tmp := table + "_fk_test_tmp"
	relaxed = strings.Replace(relaxed, "`"+table+"`", "`"+tmp+"`", 1)
	require.NotEqual(t, createSQL, relaxed)
	mustExec(t, db,
		"PRAGMA foreign_keys = OFF",
		relaxed,
		fmt.Sprintf("DROP TABLE %s", quoteIdent(table)),
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s", quoteIdent(tmp), quoteIdent(table)),
	)
}

func columnNotNull(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	return fkCountRows(t, db, fmt.Sprintf(`SELECT "notnull" FROM pragma_table_info('%s') WHERE name = '%s'`, table, column)) == 1
}

// TestAutoMigrate_RebuildWithSetNullOrphans covers the upgrade failure: a
// table the migration rebuilds holds rows whose ON DELETE SET NULL parent is
// gone. The orphan's rowid differs after the rebuild (an earlier row was
// deleted), so without reconciling first, the migrator reports it as a new
// foreign key violation and refuses to commit.
func TestAutoMigrate_RebuildWithSetNullOrphans(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)
	makeColumnNullable(t, db, "role_bindings", "principal_id")

	roleID, orphanID, keptID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	insertRow(t, db, "role_definitions", map[string]any{"id": roleID})
	insertRow(t, db, "role_bindings", map[string]any{"id": uuid.NewString(), "principal_id": "first", "role_definition_id": uuid.NewString()})
	insertRow(t, db, "role_bindings", map[string]any{"id": orphanID, "principal_id": "orphan", "role_definition_id": uuid.NewString()})
	mustExec(t, db, fmt.Sprintf(`DELETE FROM role_bindings WHERE id != '%s'`, orphanID))
	insertRow(t, db, "role_bindings", map[string]any{"id": keptID, "principal_id": "kept", "role_definition_id": roleID})
	mustExec(t, db, "PRAGMA foreign_keys = ON")
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`))

	require.NoError(t, AutoMigrate(ctx, client))

	require.True(t, columnNotNull(t, db, "role_bindings", "principal_id"), "the table should have been rebuilt")
	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id IS NULL AND principal_id = 'orphan'`, orphanID)))
	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id = '%s'`, keptID, roleID)))
	require.Equal(t, 2, fkCountRows(t, db, `SELECT count(*) FROM role_bindings`))
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`))
}

// A binding row whose role reference is gone keeps its row, with the
// reference cleared as declared, and nothing else about it changes. (That
// an empty role reference grants nothing is pinned in the hub's kernel
// tests.)
func TestAutoMigrate_NullsOrphanedBindingReference(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)
	mustExec(t, db, "PRAGMA foreign_keys = OFF")
	bindingID := uuid.NewString()
	insertRow(t, db, "role_bindings", map[string]any{
		"id": bindingID, "principal_type": "user", "principal_id": "p1", "scope_type": "project", "scope_id": "s1",
		"role_definition_id": uuid.NewString(),
	})
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	require.NoError(t, AutoMigrate(ctx, client))

	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id IS NULL
		 AND principal_type = 'user' AND principal_id = 'p1' AND scope_type = 'project' AND scope_id = 's1'`,
		bindingID)))
}

// An orphan on a NO ACTION key in a table the migration does not rebuild
// does not block startup: it is reported at ERROR, and left alone.
func TestAutoMigrate_NoActionOrphanWithoutRebuildStillMigrates(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)
	mustExec(t, db, "PRAGMA foreign_keys = OFF")
	rowID, gone := uuid.NewString(), uuid.NewString()
	insertRow(t, db, "entitlement_bindings", map[string]any{"id": rowID, "limit_definition_id": gone})
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	logs := captureSlog(t)
	require.NoError(t, AutoMigrate(ctx, client))
	require.Contains(t, logs.String(), "level=ERROR")
	require.Contains(t, logs.String(), `entitlement_bindings(limit_definition_id) -> limit_definitions(id) ON DELETE NO ACTION: 1 orphaned row(s)`)
	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM entitlement_bindings WHERE id = '%s' AND limit_definition_id = '%s'`, rowID, gone)))
}

// An orphan on a NO ACTION key in a table the migration rebuilds fails the
// migration, and the error names the key, the count and the repair SQL.
func TestAutoMigrate_NoActionOrphanWithRebuildFailsWithDiagnostic(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)
	makeColumnNullable(t, db, "entitlement_bindings", "value")
	insertRow(t, db, "entitlement_bindings", map[string]any{"id": uuid.NewString(), "value": 1, "limit_definition_id": uuid.NewString()})
	rowID := uuid.NewString()
	insertRow(t, db, "entitlement_bindings", map[string]any{"id": rowID, "value": 2, "limit_definition_id": uuid.NewString()})
	mustExec(t, db, fmt.Sprintf(`DELETE FROM entitlement_bindings WHERE id != '%s'`, rowID))
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	err := AutoMigrate(ctx, client)
	require.Error(t, err)
	msg := err.Error()
	require.Contains(t, msg, `entitlement_bindings(limit_definition_id) -> limit_definitions(id) ON DELETE NO ACTION: 1 orphaned row(s)`)
	require.Contains(t, msg, `DELETE FROM "entitlement_bindings" WHERE "entitlement_bindings"."limit_definition_id" IS NOT NULL AND NOT EXISTS (SELECT 1 FROM "limit_definitions" AS p WHERE p."id" = "entitlement_bindings"."limit_definition_id");`)
	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(`SELECT count(*) FROM entitlement_bindings WHERE id = '%s'`, rowID)))
}

func TestAutoMigrate_CleanDatabaseHasNoFKDiagnostic(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	_, db := openHubFile(t, path)
	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Empty(t, res.Unrepaired)
}
