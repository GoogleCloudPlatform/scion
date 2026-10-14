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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// pendingKey identifies a repaired foreign key and its row count.
type pendingKey struct {
	table string
	id    int
	rows  int
}

func pendingKeys(groups []FKRepairGroup) []pendingKey {
	out := []pendingKey{}
	for _, g := range groups {
		out = append(out, pendingKey{table: g.Table, id: g.ForeignKeyID, rows: g.Rows})
	}
	return out
}

// firstPassKeys returns the keys the reconcile repaired in its first pass,
// i.e. the orphans that existed before it ran.
func firstPassKeys(res fkReconcileResult) []pendingKey {
	out := []pendingKey{}
	for _, r := range res.Repaired {
		if r.pass == 0 {
			out = append(out, pendingKey{table: r.info.table, id: r.info.id, rows: int(r.rows)})
		}
	}
	return out
}

// writeCounter adds triggers that count every write to the given tables.
func writeCounter(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	mustExec(t, db, `CREATE TABLE writes (n INTEGER)`)
	for _, tbl := range tables {
		mustExec(t, db,
			fmt.Sprintf(`CREATE TRIGGER %s_del AFTER DELETE ON %s BEGIN INSERT INTO writes VALUES (1); END`, tbl, tbl),
			fmt.Sprintf(`CREATE TRIGGER %s_upd AFTER UPDATE ON %s BEGIN INSERT INTO writes VALUES (1); END`, tbl, tbl),
		)
	}
}

// requirePendingMatchesReconcile runs the pre-check, checks it wrote
// nothing, then runs the reconcile and checks that its first pass repaired
// exactly the keys and row counts the pre-check reported.
func requirePendingMatchesReconcile(t *testing.T, db *sql.DB) []FKRepairGroup {
	t.Helper()
	ctx := context.Background()
	before := fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`)
	pending, err := PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Equal(t, before, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`), "the pre-check must not write")

	res, err := reconcileSQLiteForeignKeys(ctx, db)
	require.NoError(t, err)
	require.Equal(t, pendingKeys(pending), firstPassKeys(res), "pre-check and reconcile disagree")
	if len(pending) == 0 {
		require.Empty(t, res.Repaired, "an empty pre-check means the reconcile changes no rows")
	}
	return pending
}

func TestPendingSQLiteFKRepairs_CleanDatabase(t *testing.T) {
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE child (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE CASCADE)`,
		`INSERT INTO parent VALUES ('p1')`,
		`INSERT INTO child VALUES ('c1', 'p1'), ('c2', NULL)`,
	)
	require.Empty(t, requirePendingMatchesReconcile(t, db))
}

func TestPendingSQLiteFKRepairs_RepairableKeys(t *testing.T) {
	db := openFKTestDB(t)
	seedSyntheticFKSchema(t, db)
	writeCounter(t, db, "c_cascade", "c_setnull", "c_implicit")

	pending, err := PendingSQLiteFKRepairs(context.Background(), db)
	require.NoError(t, err)
	require.Zero(t, fkCountRows(t, db, `SELECT count(*) FROM writes`), "the pre-check must not write")
	require.Len(t, pending, 3)
	byTable := map[string]FKRepairGroup{}
	for _, g := range pending {
		byTable[g.Table] = g
		require.Equal(t, []string{"pid"}, g.Columns, g.Table)
		require.Equal(t, "parent", g.Parent, g.Table)
		require.Equal(t, 1, g.Rows, g.Table)
	}
	require.Equal(t, "CASCADE", byTable["c_cascade"].Action)
	require.Equal(t, "SET NULL", byTable["c_setnull"].Action)
	require.Equal(t, "SET NULL", byTable["c_implicit"].Action)

	require.Equal(t, pendingKeys(pending), pendingKeys(requirePendingMatchesReconcile(t, db)))
}

// Only orphans whose declared action is not applied automatically: the
// reconcile changes no rows, so the pre-check reports nothing.
func TestPendingSQLiteFKRepairs_UnrepairableOnlyIsEmpty(t *testing.T) {
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE c_noaction (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id))`,
		`CREATE TABLE c_restrict (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE RESTRICT)`,
		`CREATE TABLE c_default (id TEXT PRIMARY KEY, pid TEXT DEFAULT 'x' REFERENCES parent(id) ON DELETE SET DEFAULT)`,
		`CREATE TABLE c_notnull (id TEXT PRIMARY KEY, pid TEXT NOT NULL REFERENCES parent(id) ON DELETE SET NULL)`,
		`CREATE TABLE c_noparent (id TEXT PRIMARY KEY, pid TEXT REFERENCES nosuch(id) ON DELETE CASCADE)`,
		`INSERT INTO c_noaction VALUES ('orphan', 'gone')`,
		`INSERT INTO c_restrict VALUES ('orphan', 'gone')`,
		`INSERT INTO c_default VALUES ('orphan', 'gone')`,
		`INSERT INTO c_notnull VALUES ('orphan', 'gone')`,
		`INSERT INTO c_noparent VALUES ('orphan', 'gone')`,
	)
	require.Equal(t, 5, fkCountRows(t, db, `SELECT count(*) FROM pragma_foreign_key_check`))
	require.Empty(t, requirePendingMatchesReconcile(t, db))
}

// Mixed repairable and unrepairable keys: only the repairable ones are
// reported.
func TestPendingSQLiteFKRepairs_MixedReportsRepairableOnly(t *testing.T) {
	db := openFKTestDB(t)
	seedSyntheticFKSchema(t, db)
	mustExec(t, db,
		`CREATE TABLE c_noaction (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id))`,
		`INSERT INTO c_noaction VALUES ('orphan', 'gone')`,
	)
	pending := requirePendingMatchesReconcile(t, db)
	require.Len(t, pending, 3)
	for _, g := range pending {
		require.NotEqual(t, "c_noaction", g.Table)
	}
}

// A cascade is reported by its starting key; the rows it orphans further
// down are repaired in a later pass.
func TestPendingSQLiteFKRepairs_CascadeReportedByStartingKey(t *testing.T) {
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE a (id TEXT PRIMARY KEY)`,
		`CREATE TABLE b (id TEXT PRIMARY KEY, aid TEXT REFERENCES a(id) ON DELETE CASCADE)`,
		`CREATE TABLE c (id TEXT PRIMARY KEY, bid TEXT REFERENCES b(id) ON DELETE SET NULL)`,
		`INSERT INTO b VALUES ('b1', 'gone'), ('b2', 'gone')`,
		`INSERT INTO c VALUES ('c1', 'b1')`,
	)
	pending := requirePendingMatchesReconcile(t, db)
	require.Equal(t, []pendingKey{{table: "b", id: 0, rows: 2}}, pendingKeys(pending))
	require.Equal(t, 1, fkCountRows(t, db, `SELECT count(*) FROM c WHERE id = 'c1' AND bid IS NULL`))
}

// The pre-check agrees with the reconcile on the real hub schema.
func TestPendingSQLiteFKRepairs_HubSchema(t *testing.T) {
	path := openMigratedFile(t)
	_, db := openHubFile(t, path)
	require.Empty(t, requirePendingMatchesReconcile(t, db), "a freshly migrated hub has nothing to repair")

	mustExec(t, db, "PRAGMA foreign_keys = OFF")
	insertRow(t, db, "role_bindings", map[string]any{"id": uuid.NewString(), "role_definition_id": uuid.NewString()})
	insertRow(t, db, "entitlement_bindings", map[string]any{"id": uuid.NewString(), "limit_definition_id": uuid.NewString()})
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	pending := requirePendingMatchesReconcile(t, db)
	require.Len(t, pending, 1, "the NO ACTION orphan is not reported")
	require.Equal(t, "role_bindings", pending[0].Table)
	require.Equal(t, []string{"role_definition_id"}, pending[0].Columns)
	require.Equal(t, "SET NULL", pending[0].Action)
	require.Equal(t, 1, pending[0].Rows)
}

// The digest tells apart two sets of orphans with the same count.
func TestPendingSQLiteFKRepairs_OrphanDigest(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE child (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE SET NULL)`,
		`INSERT INTO child VALUES ('a', 'gone')`,
	)
	first, err := PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Len(t, first, 1)
	again, err := PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Equal(t, first[0].Orphans, again[0].Orphans, "the digest is stable")

	mustExec(t, db, `UPDATE child SET pid = NULL WHERE id = 'a'`, `INSERT INTO child VALUES ('b', 'gone')`)
	other, err := PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Len(t, other, 1)
	require.Equal(t, first[0].Rows, other[0].Rows)
	require.NotEqual(t, first[0].Orphans, other[0].Orphans, "different rows, same count")

	// A new orphan that reuses the rowid of a deleted one (the highest
	// rowid, as after a CASCADE repair) still gets a different digest.
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM child WHERE id = 'b'`).Scan(&rowid))
	mustExec(t, db, `DELETE FROM child WHERE id = 'b'`, `INSERT INTO child VALUES ('c', 'gone2')`)
	var reused int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM child WHERE id = 'c'`).Scan(&reused))
	require.Equal(t, rowid, reused, "fixture: the rowid is reused")
	third, err := PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Len(t, third, 1)
	require.NotEqual(t, other[0].Orphans, third[0].Orphans, "same rowid and count, different key")
}

// The hook runs inside AutoMigrate, after anything the caller did before
// it, and sees the database as the reconcile will: an orphan created after
// an earlier pre-check (as PreMigrate's deletes can) is reported to it.
func TestAutoMigrate_BeforeFKReconcileHookSeesLateOrphans(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)

	early, err := PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Empty(t, early)

	// After the early pre-check, a delete on a connection without
	// enforcement leaves a SET NULL orphan.
	mustExec(t, db, "PRAGMA foreign_keys = OFF")
	roleID, bindingID := uuid.NewString(), uuid.NewString()
	insertRow(t, db, "role_definitions", map[string]any{"id": roleID})
	insertRow(t, db, "role_bindings", map[string]any{"id": bindingID, "role_definition_id": roleID})
	mustExec(t, db, fmt.Sprintf(`DELETE FROM role_definitions WHERE id = '%s'`, roleID))
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	var calls int
	var seen []FKRepairGroup
	hook := func(ctx context.Context, hdb *sql.DB, groups []FKRepairGroup) error {
		calls++
		seen = groups
		// The reconcile has not run yet.
		require.Equal(t, 1, fkCountRows(t, hdb, fmt.Sprintf(
			`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id = '%s'`, bindingID, roleID)))
		// The hook holds AutoMigrate's process lock.
		require.Len(t, autoMigrateSem, 1)
		return nil
	}
	require.NoError(t, AutoMigrate(ctx, client, WithBeforeFKReconcile(hook)))
	require.Equal(t, 1, calls)
	require.Len(t, seen, 1)
	require.Equal(t, "role_bindings", seen[0].Table)
	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id IS NULL`, bindingID)))

	// Nothing left to reconcile: the hook is not called.
	require.NoError(t, AutoMigrate(ctx, client, WithBeforeFKReconcile(hook)))
	require.Equal(t, 1, calls)
}

// A hook error aborts AutoMigrate before the reconcile or the schema
// migration writes anything.
func TestAutoMigrate_BeforeFKReconcileHookErrorLeavesDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)
	// The next migration must rebuild this table; it stays nullable if
	// the migration does not run.
	makeColumnNullable(t, db, "role_bindings", "principal_id")
	bindingID, gone := uuid.NewString(), uuid.NewString()
	insertRow(t, db, "role_bindings", map[string]any{"id": bindingID, "principal_id": "p", "role_definition_id": gone})
	mustExec(t, db, "PRAGMA foreign_keys = ON")
	var schemaBefore string
	require.NoError(t, db.QueryRow(`SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_master ORDER BY name)`).Scan(&schemaBefore))

	errHook := errors.New("snapshot failed")
	err := AutoMigrate(ctx, client, WithBeforeFKReconcile(func(context.Context, *sql.DB, []FKRepairGroup) error { return errHook }))
	require.ErrorIs(t, err, errHook)

	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id = '%s'`, bindingID, gone)), "the orphan is untouched")
	require.False(t, columnNotNull(t, db, "role_bindings", "principal_id"), "the schema migration did not run")
	var schemaAfter string
	require.NoError(t, db.QueryRow(`SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_master ORDER BY name)`).Scan(&schemaAfter))
	require.Equal(t, schemaBefore, schemaAfter)
	require.Len(t, autoMigrateSem, 0, "the lock is released")
}

// A pre-check failure is logged; the hook is not called and the reconcile
// runs as it would without one.
func TestAutoMigrate_BeforeFKReconcilePreCheckFailureContinues(t *testing.T) {
	ctx := context.Background()
	path := openMigratedFile(t)
	client, db := openHubFile(t, path)
	mustExec(t, db, "PRAGMA foreign_keys = OFF")
	bindingID := uuid.NewString()
	insertRow(t, db, "role_bindings", map[string]any{"id": bindingID, "role_definition_id": uuid.NewString()})
	mustExec(t, db, "PRAGMA foreign_keys = ON")

	orig := pendingSQLiteFKRepairs
	pendingSQLiteFKRepairs = func(context.Context, *sql.DB) ([]FKRepairGroup, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { pendingSQLiteFKRepairs = orig })

	logs := captureSlog(t)
	called := false
	require.NoError(t, AutoMigrate(ctx, client, WithBeforeFKReconcile(func(context.Context, *sql.DB, []FKRepairGroup) error {
		called = true
		return nil
	})))
	require.False(t, called)
	require.Contains(t, logs.String(), "pre-check failed")
	require.Equal(t, 1, fkCountRows(t, db, fmt.Sprintf(
		`SELECT count(*) FROM role_bindings WHERE id = '%s' AND role_definition_id IS NULL`, bindingID)))
}

// The digest covers the orphaned rows' full content: the same rowid and the
// same key with a different value in another column give a new digest,
// while byte-identical rows give the same one.
func TestPendingSQLiteFKRepairs_OrphanDigestCoversRowContent(t *testing.T) {
	ctx := context.Background()
	db := openFKTestDB(t)
	mustExec(t, db,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE child (id TEXT PRIMARY KEY, pid TEXT REFERENCES parent(id) ON DELETE CASCADE, note TEXT)`,
		`INSERT INTO child VALUES ('a', 'gone', 'first')`,
	)
	digest := func() string {
		t.Helper()
		g, err := PendingSQLiteFKRepairs(ctx, db)
		require.NoError(t, err)
		require.Len(t, g, 1)
		require.Equal(t, 1, g[0].Rows)
		return g[0].Orphans
	}
	rowid := func() int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM child WHERE id = 'a'`).Scan(&id))
		return id
	}
	first, firstRowid := digest(), rowid()

	// The row is deleted (as a CASCADE repair would) and the same row comes
	// back with the same rowid and key but a different note.
	mustExec(t, db, `DELETE FROM child WHERE id = 'a'`, `INSERT INTO child VALUES ('a', 'gone', 'second')`)
	require.Equal(t, firstRowid, rowid(), "fixture: the rowid is reused")
	second := digest()
	require.NotEqual(t, first, second, "same rowid and key, different non-key column")

	// Byte-identical rows give the same digest.
	mustExec(t, db, `DELETE FROM child WHERE id = 'a'`, `INSERT INTO child VALUES ('a', 'gone', 'first')`)
	require.Equal(t, firstRowid, rowid())
	require.Equal(t, first, digest())
}
