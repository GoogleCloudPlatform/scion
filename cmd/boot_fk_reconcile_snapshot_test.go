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

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fkSnapshots(t *testing.T, dbPath string) []string {
	t.Helper()
	m, err := filepath.Glob(dbPath + "." + fkReconcileSnapshotLabelPrefix + "-*.bak")
	require.NoError(t, err)
	return m
}

// openRawFKDB opens the hub database file on one connection with foreign
// key enforcement off, as a writer without the pragma would.
func openRawFKDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = OFF")
	require.NoError(t, err)
	return db
}

// fkInsertRow inserts a row with the given values, filling every other NOT
// NULL column without a default with a unique placeholder.
func fkInsertRow(t *testing.T, db *sql.DB, table string, vals map[string]any) {
	t.Helper()
	rows, err := db.Query(`SELECT name, "notnull", dflt_value FROM pragma_table_info(?)`, table)
	require.NoError(t, err)
	var cols []string
	var args []any
	for rows.Next() {
		var (
			name string
			nn   int
			dflt sql.NullString
		)
		require.NoError(t, rows.Scan(&name, &nn, &dflt))
		if v, ok := vals[name]; ok {
			cols, args = append(cols, `"`+name+`"`), append(args, v)
		} else if nn != 0 && !dflt.Valid {
			cols, args = append(cols, `"`+name+`"`), append(args, uuid.NewString())
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	q := fmt.Sprintf(`INSERT INTO "%s" (%s) VALUES (%s)`, table, strings.Join(cols, ", "),
		strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", "))
	_, err = db.Exec(q, args...)
	require.NoError(t, err, q)
}

// seedOrphanBinding adds a role binding whose role definition does not
// exist (ON DELETE SET NULL, so the reconcile clears the reference).
func seedOrphanBinding(t *testing.T, dbPath string) (id, gone string) {
	t.Helper()
	db := openRawFKDB(t, dbPath)
	id, gone = uuid.NewString(), uuid.NewString()
	fkInsertRow(t, db, "role_bindings", map[string]any{"id": id, "role_definition_id": gone})
	require.NoError(t, db.Close())
	return id, gone
}

func roleDefinitionRef(t *testing.T, db *sql.DB, bindingID string) sql.NullString {
	t.Helper()
	var ref sql.NullString
	require.NoError(t, db.QueryRow(`SELECT role_definition_id FROM role_bindings WHERE id = ?`, bindingID).Scan(&ref))
	return ref
}

func TestFKReconcileSnapshot_BootSnapshotsBeforeRepair(t *testing.T) {
	f := newRepairFixture(t)
	logs, restore := captureSlog(t)
	defer restore()

	// Boot 1: a fresh database has nothing to reconcile.
	s := f.boot(t)
	assert.Empty(t, fkSnapshots(t, f.dbPath))
	assert.NotContains(t, logs.String(), "foreign key reconcile")
	require.NoError(t, s.Close())

	bindingID, gone := seedOrphanBinding(t, f.dbPath)

	// Boot 2 snapshots, then the migration clears the orphaned reference.
	logs.Reset()
	s = f.boot(t)
	snaps := fkSnapshots(t, f.dbPath)
	require.Len(t, snaps, 1, logs.String())
	out := logs.String()
	assert.Contains(t, out, snaps[0], "the snapshot path must be logged")
	assert.Contains(t, out, "table=role_bindings")
	assert.Contains(t, out, "level=WARN")
	assert.False(t, roleDefinitionRef(t, s.(timestampRepairDB).DB(), bindingID).Valid, "the reconcile clears the reference")

	// The snapshot holds the row as it was before the reconcile, and only
	// the owner can read it.
	info, err := os.Stat(snaps[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	snapDB, err := sql.Open("sqlite", "file:"+snaps[0]+"?mode=ro")
	require.NoError(t, err)
	assert.Equal(t, sql.NullString{String: gone, Valid: true}, roleDefinitionRef(t, snapDB, bindingID))
	_ = snapDB.Close()
	require.NoError(t, s.Close())

	// Boot 3: nothing left to reconcile, so no new snapshot.
	logs.Reset()
	s = f.boot(t)
	assert.Equal(t, snaps, fkSnapshots(t, f.dbPath))
	assert.NotContains(t, logs.String(), "foreign key reconcile")
	require.NoError(t, s.Close())
}

// Orphans the reconcile leaves in place change no rows: no snapshot.
func TestFKReconcileSnapshot_UnrepairableOnlyNoSnapshot(t *testing.T) {
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())
	db := openRawFKDB(t, f.dbPath)
	rowID := uuid.NewString()
	fkInsertRow(t, db, "entitlement_bindings", map[string]any{"id": rowID, "limit_definition_id": uuid.NewString()})
	require.NoError(t, db.Close())

	s = f.boot(t)
	assert.Empty(t, fkSnapshots(t, f.dbPath))
	var n int
	require.NoError(t, s.(timestampRepairDB).DB().QueryRow(`SELECT count(*) FROM entitlement_bindings WHERE id = ?`, rowID).Scan(&n))
	assert.Equal(t, 1, n, "the reconcile leaves the row in place")
	require.NoError(t, s.Close())
}

// A snapshot that is needed but fails stops startup before the reconcile
// changes any rows.
func TestFKReconcileSnapshot_SnapshotFailureFailsStartup(t *testing.T) {
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())
	bindingID, gone := seedOrphanBinding(t, f.dbPath)

	orig := snapshotSQLite
	snapshotSQLite = func(context.Context, *sql.DB, string, time.Time) (entadapter.SQLiteSnapshot, error) {
		return entadapter.SQLiteSnapshot{}, errors.New("no space left on device")
	}
	t.Cleanup(func() { snapshotSQLite = orig })

	_, _, err := initStore(context.Background(), f.cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "refusing to continue")
	assert.Contains(t, msg, "no space left on device")
	assert.Contains(t, msg, filepath.Dir(f.dbPath), "the error names where space is needed")
	assert.Contains(t, msg, "bytes", "the error says how much space is needed")

	db := openRawFKDB(t, f.dbPath)
	assert.Equal(t, sql.NullString{String: gone, Valid: true}, roleDefinitionRef(t, db, bindingID), "no row may change")
	assert.Empty(t, fkSnapshots(t, f.dbPath))
}

func TestFKReconcileSnapshot_SnapshotInProgressFailsStartup(t *testing.T) {
	ctx := context.Background()
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())
	seedOrphanBinding(t, f.dbPath)

	db := openRawFKDB(t, f.dbPath)
	groups, err := entc.PendingSQLiteFKRepairs(ctx, db)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	tmp := f.dbPath + "." + fkReconcileSnapshotLabel(groups) + ".tmp"
	require.NoError(t, os.WriteFile(tmp, nil, 0o600))

	err = snapshotBeforeFKReconcile(ctx, db, groups)
	require.Error(t, err)
	assert.True(t, errors.Is(err, entadapter.ErrSnapshotInProgress), err)
	assert.Contains(t, err.Error(), tmp)
	_, err = os.Stat(tmp)
	assert.NoError(t, err, "the other file must be left alone")
}

// A retried reconcile with the same orphans reuses its snapshot; different
// orphans get a new one.
func TestFKReconcileSnapshot_RetryReusesSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())
	seedOrphanBinding(t, f.dbPath)
	db := openRawFKDB(t, f.dbPath)
	pending := func() []entc.FKRepairGroup {
		g, err := entc.PendingSQLiteFKRepairs(ctx, db)
		require.NoError(t, err)
		require.NotEmpty(t, g)
		return g
	}

	logs, restore := captureSlog(t)
	defer restore()
	require.NoError(t, snapshotBeforeFKReconcile(ctx, db, pending()))
	first := fkSnapshots(t, f.dbPath)
	require.Len(t, first, 1)

	logs.Reset()
	require.NoError(t, snapshotBeforeFKReconcile(ctx, db, pending()))
	assert.Equal(t, first, fkSnapshots(t, f.dbPath))
	assert.Contains(t, logs.String(), "reusing the database snapshot")

	fkInsertRow(t, db, "role_bindings", map[string]any{"id": uuid.NewString(), "role_definition_id": uuid.NewString()})
	require.NoError(t, snapshotBeforeFKReconcile(ctx, db, pending()))
	assert.Len(t, fkSnapshots(t, f.dbPath), 2, "a different set of orphans gets its own snapshot")
}

// Orphans that come back after a completed reconcile, with the same key
// and the same count but different rows, get a new snapshot that holds
// them, not the earlier one.
func TestFKReconcileSnapshot_SameCountDifferentRowsNewSnapshot(t *testing.T) {
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())

	seedOrphanBinding(t, f.dbPath)
	s = f.boot(t)
	require.NoError(t, s.Close())
	first := fkSnapshots(t, f.dbPath)
	require.Len(t, first, 1)

	secondID, gone := seedOrphanBinding(t, f.dbPath)
	s = f.boot(t)
	snaps := fkSnapshots(t, f.dbPath)
	require.Len(t, snaps, 2, "a later reconcile must not reuse the earlier snapshot")
	require.NoError(t, s.Close())
	var latest string
	for _, p := range snaps {
		if p != first[0] {
			latest = p
		}
	}
	snapDB, err := sql.Open("sqlite", "file:"+latest+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = snapDB.Close() }()
	assert.Equal(t, sql.NullString{String: gone, Valid: true}, roleDefinitionRef(t, snapDB, secondID), "the new snapshot holds the new orphan")
}

// After a completed reconcile, an orphan with the same rowid and the same
// key but different content in another column gets a new snapshot that
// holds it; the earlier snapshot, still on disk, is not reused.
func TestFKReconcileSnapshot_ChangedRowContentNewSnapshot(t *testing.T) {
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())

	db := openRawFKDB(t, f.dbPath)
	bindingID, gone := uuid.NewString(), uuid.NewString()
	fkInsertRow(t, db, "role_bindings", map[string]any{"id": bindingID, "principal_id": "p1", "role_definition_id": gone})
	var rowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM role_bindings WHERE id = ?`, bindingID).Scan(&rowid))
	require.NoError(t, db.Close())

	s = f.boot(t) // snapshot 1, then the reconcile clears the reference
	require.NoError(t, s.Close())
	first := fkSnapshots(t, f.dbPath)
	require.Len(t, first, 1)

	// The same row, by rowid and key, is orphaned again with a different
	// principal.
	db = openRawFKDB(t, f.dbPath)
	_, err := db.Exec(`UPDATE role_bindings SET role_definition_id = ?, principal_id = 'p2' WHERE id = ?`, gone, bindingID)
	require.NoError(t, err)
	var again int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM role_bindings WHERE id = ?`, bindingID).Scan(&again))
	require.Equal(t, rowid, again, "fixture: same rowid")
	require.NoError(t, db.Close())

	logs, restore := captureSlog(t)
	defer restore()
	s = f.boot(t)
	require.NoError(t, s.Close())
	snaps := fkSnapshots(t, f.dbPath)
	require.Len(t, snaps, 2, "the earlier snapshot must not be reused: %s", logs.String())
	assert.NotContains(t, logs.String(), "reusing the database snapshot")
	var latest string
	for _, p := range snaps {
		if p != first[0] {
			latest = p
		}
	}
	snapDB, err := sql.Open("sqlite", "file:"+latest+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = snapDB.Close() }()
	var principal string
	var ref sql.NullString
	require.NoError(t, snapDB.QueryRow(`SELECT principal_id, role_definition_id FROM role_bindings WHERE id = ?`, bindingID).Scan(&principal, &ref))
	assert.Equal(t, "p2", principal, "the new snapshot holds the row as it is now")
	assert.Equal(t, sql.NullString{String: gone, Valid: true}, ref)
}

// The maintenance migrate helper snapshots too.
func TestFKReconcileSnapshot_MaintenanceMigrate(t *testing.T) {
	ctx := context.Background()
	f := newRepairFixture(t)
	s := f.boot(t)
	require.NoError(t, s.Close())
	bindingID, gone := seedOrphanBinding(t, f.dbPath)

	client, err := entc.OpenSQLite("file:"+f.dbPath, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, migrateHubSchema(ctx, client, false, true))

	snaps := fkSnapshots(t, f.dbPath)
	require.Len(t, snaps, 1)
	snapDB, err := sql.Open("sqlite", "file:"+snaps[0]+"?mode=ro")
	require.NoError(t, err)
	defer func() { _ = snapDB.Close() }()
	assert.Equal(t, sql.NullString{String: gone, Valid: true}, roleDefinitionRef(t, snapDB, bindingID))
}

// An in-memory database has no file to snapshot: skipped, no error.
func TestFKReconcileSnapshot_InMemorySkipped(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	orig := snapshotSQLite
	snapshotSQLite = func(context.Context, *sql.DB, string, time.Time) (entadapter.SQLiteSnapshot, error) {
		t.Fatal("no snapshot for an in-memory database")
		return entadapter.SQLiteSnapshot{}, nil
	}
	t.Cleanup(func() { snapshotSQLite = orig })

	logs, restore := captureSlog(t)
	defer restore()
	groups := []entc.FKRepairGroup{{Table: "t", Columns: []string{"c"}, Parent: "p", Action: "CASCADE", Rows: 1}}
	require.NoError(t, snapshotBeforeFKReconcile(context.Background(), db, groups))
	assert.Contains(t, logs.String(), "in-memory database; no snapshot taken")
}

func TestFKReconcileSnapshotLabel(t *testing.T) {
	a := []entc.FKRepairGroup{{Table: "t", ForeignKeyID: 0, Columns: []string{"c"}, Rows: 1, Orphans: "x"}}
	b := []entc.FKRepairGroup{{Table: "t", ForeignKeyID: 0, Columns: []string{"c"}, Rows: 2, Orphans: "x"}}
	c := []entc.FKRepairGroup{{Table: "t", ForeignKeyID: 0, Columns: []string{"c"}, Rows: 1, Orphans: "y"}}
	assert.Equal(t, fkReconcileSnapshotLabel(a), fkReconcileSnapshotLabel(a))
	assert.NotEqual(t, fkReconcileSnapshotLabel(a), fkReconcileSnapshotLabel(b))
	assert.NotEqual(t, fkReconcileSnapshotLabel(a), fkReconcileSnapshotLabel(c), "same count, different rows")
	assert.Regexp(t, `^`+fkReconcileSnapshotLabelPrefix+`-[0-9a-f]{8}$`, fkReconcileSnapshotLabel(a))
}
