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
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const snapshotAgeOld = 8 * 24 * time.Hour

// agedFile creates path with a last use (modification time) age ago.
func agedFile(t *testing.T, path string, age time.Duration) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	mt := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, mt, mt))
	return path
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.ModTime()
}

// stubSnapshotRetention replaces the predicate and the prune with
// counting wrappers for the test.
type stubSnapshotRetention struct {
	predicateCalls int
	pruneCalls     int
	prunePatterns  []string
	pruneKeep      []string
}

func stubRetention(t *testing.T, incomplete bool, predErr error) *stubSnapshotRetention {
	t.Helper()
	st := &stubSnapshotRetention{}
	origPred, origPrune := timestampNormalizeIncomplete, pruneStartupSnapshots
	t.Cleanup(func() { timestampNormalizeIncomplete, pruneStartupSnapshots = origPred, origPrune })
	timestampNormalizeIncomplete = func(context.Context, store.Store) (bool, error) {
		st.predicateCalls++
		return incomplete, predErr
	}
	pruneStartupSnapshots = func(file string, patterns, keep []string, now time.Time) []string {
		st.pruneCalls++
		st.prunePatterns, st.pruneKeep = patterns, keep
		return origPrune(file, patterns, keep, now)
	}
	return st
}

// The label patterns match exactly the labels the snapshot code produces.
func TestStartupSnapshotPatternsMatchLabels(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	stamp := "-" + time.Now().UTC().Format("20060102T150405Z") + ".bak"
	fkLabel := fkReconcileSnapshotLabel([]entc.FKRepairGroup{{Table: "group_memberships", Columns: []string{"user_id"}, Rows: 1}})
	match := []string{
		agedFile(t, db+"."+fkLabel+stamp, snapshotAgeOld),
		agedFile(t, db+"."+utcTimestampSnapshotLabel+stamp, snapshotAgeOld),
	}
	for _, n := range []string{
		fkReconcileSnapshotLabelPrefix + "-0a1b2c3",   // 7 hex digits
		fkReconcileSnapshotLabelPrefix + "-0a1b2c3d4", // 9 hex digits
		fkReconcileSnapshotLabelPrefix,
	} {
		agedFile(t, db+"."+n+stamp, snapshotAgeOld)
	}
	assert.ElementsMatch(t, match, entadapter.TouchSQLiteSnapshots(db, startupSnapshotPatterns(), time.Now()))
}

// R1: the timestamp repair reports its snapshot also when the repair does
// not complete, and on success.
func TestUTCTimestampRepair_ReportsSnapshotPath(t *testing.T) {
	ctx := context.Background()

	t.Run("incomplete repair", func(t *testing.T) {
		f := newRepairFixture(t)
		s := f.boot(t)
		f.seed(t, s)
		db := s.(timestampRepairDB).DB()
		f.corrupt(t, db)
		_, err := entadapter.NormalizeUTCTimestamps(ctx, db, "sqlite3", nil, entadapter.TimestampNormalizeOptions{})
		require.NoError(t, err)
		_, err = db.Exec("UPDATE users SET last_seen = ? WHERE id = ?", "2026-02-30 14:45:00 +0545 +0545", f.userID)
		require.NoError(t, err)

		res := repairUnreadableTimestamps(ctx, s.(timestampRepairDB))
		assert.False(t, res.Completed)
		assert.False(t, res.ProbeFailed)
		snaps := f.snapshots(t)
		require.Len(t, snaps, 1)
		assert.Equal(t, snaps[0], res.SnapshotPath)
		_, keep := (&startupSnapshots{tsRepair: res}).pruneInputs(ctx, s)
		assert.Contains(t, keep, res.SnapshotPath)
	})

	t.Run("completed repair", func(t *testing.T) {
		f := newRepairFixture(t)
		s := f.boot(t)
		f.seed(t, s)
		f.corrupt(t, s.(timestampRepairDB).DB())

		res := repairUnreadableTimestamps(ctx, s.(timestampRepairDB))
		assert.True(t, res.Completed)
		snaps := f.snapshots(t)
		require.Len(t, snaps, 1)
		assert.Equal(t, snaps[0], res.SnapshotPath)
	})

	t.Run("panic", func(t *testing.T) {
		f := newRepairFixture(t)
		s := f.boot(t)
		f.seed(t, s)
		f.corrupt(t, s.(timestampRepairDB).DB())
		orig := snapshotSQLite
		t.Cleanup(func() { snapshotSQLite = orig })
		snapshotSQLite = func(context.Context, *sql.DB, string, time.Time) (entadapter.SQLiteSnapshot, error) {
			panic("injected")
		}
		res := repairUnreadableTimestamps(ctx, s.(timestampRepairDB))
		assert.False(t, res.Completed)
		assert.True(t, res.ProbeFailed, "after a panic the repair state is unknown")
	})

	t.Run("panic after the snapshot", func(t *testing.T) {
		f := newRepairFixture(t)
		s := f.boot(t)
		f.seed(t, s)
		f.corrupt(t, s.(timestampRepairDB).DB())
		orig := normalizeUTCTimestamps
		t.Cleanup(func() { normalizeUTCTimestamps = orig })
		normalizeUTCTimestamps = func(context.Context, *sql.DB, string, io.Writer, entadapter.TimestampNormalizeOptions) (entadapter.TimestampNormalizeReport, error) {
			panic("injected")
		}
		res := repairUnreadableTimestamps(ctx, s.(timestampRepairDB))
		assert.False(t, res.Completed)
		assert.True(t, res.ProbeFailed)
		snaps := f.snapshots(t)
		require.Len(t, snaps, 1)
		assert.Equal(t, snaps[0], res.SnapshotPath, "the snapshot taken before the panic is kept")
	})

	t.Run("rewrite error after the snapshot", func(t *testing.T) {
		f := newRepairFixture(t)
		s := f.boot(t)
		f.seed(t, s)
		f.corrupt(t, s.(timestampRepairDB).DB())
		orig := normalizeUTCTimestamps
		t.Cleanup(func() { normalizeUTCTimestamps = orig })
		normalizeUTCTimestamps = func(context.Context, *sql.DB, string, io.Writer, entadapter.TimestampNormalizeOptions) (entadapter.TimestampNormalizeReport, error) {
			return entadapter.TimestampNormalizeReport{}, errors.New("injected rewrite failure")
		}
		res := repairUnreadableTimestamps(ctx, s.(timestampRepairDB))
		assert.False(t, res.Completed)
		assert.False(t, res.ProbeFailed)
		snaps := f.snapshots(t)
		require.Len(t, snaps, 1)
		assert.Equal(t, snaps[0], res.SnapshotPath)
	})

	t.Run("probe failure", func(t *testing.T) {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "hub.db"))
		require.NoError(t, err)
		require.NoError(t, db.Close())
		res := repairUnreadableTimestamps(ctx, fakeRepairDB{db: db, dialect: "sqlite3"})
		assert.True(t, res.ProbeFailed)
		assert.Empty(t, res.SnapshotPath)
	})
}

// snapshotFixture is a migrated store with old snapshots of both kinds and
// an unrelated file next to its database.
type snapshotFixture struct {
	s                    startupStore
	oldFK, oldTS, keptFK string
	keptTS, other, tmpTS string
}

func newSnapshotFixture(t *testing.T) *snapshotFixture {
	t.Helper()
	f := newRepairFixture(t)
	s := f.boot(t).(startupStore)
	p := func(label, stamp string) string { return f.dbPath + "." + label + "-" + stamp + ".bak" }
	return &snapshotFixture{
		s:      s,
		oldFK:  agedFile(t, p(fkReconcileSnapshotLabelPrefix+"-0a1b2c3d", "20260901T000000Z"), snapshotAgeOld),
		oldTS:  agedFile(t, p(utcTimestampSnapshotLabel, "20260901T000000Z"), snapshotAgeOld),
		keptFK: agedFile(t, p(fkReconcileSnapshotLabelPrefix+"-11111111", "20260902T000000Z"), snapshotAgeOld),
		keptTS: agedFile(t, p(utcTimestampSnapshotLabel, "20260902T000000Z"), snapshotAgeOld),
		other:  agedFile(t, f.dbPath+".pre-upgrade-20260901T000000Z.bak", snapshotAgeOld),
		tmpTS:  agedFile(t, f.dbPath+"."+utcTimestampSnapshotLabel+".tmp", snapshotAgeOld),
	}
}

// R2, R3, R5: a failed migration refreshes the last use of every snapshot,
// whatever the filters say, prunes nothing and never reads the
// normalizer's state.
func TestMigrateThenPrune_MigrateFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		incomplete bool
		predErr    error
		probe      bool
	}{
		{name: "probe failed", probe: true},
		{name: "normalize incomplete", incomplete: true},
		{name: "state unreadable", predErr: errors.New("no such table")},
		{name: "no filter", incomplete: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSnapshotFixture(t)
			st := stubRetention(t, tc.incomplete, tc.predErr)
			boom := errors.New("migration failed")
			before := time.Now().Add(-time.Minute)
			in := &startupSnapshots{tsRepair: utcTimestampRepairResult{ProbeFailed: tc.probe}}

			err := migrateThenPrune(context.Background(), fx.s, func(context.Context) error { return boom }, in)
			assert.Same(t, boom, err, "the migration error is returned unchanged")
			assert.Zero(t, st.pruneCalls, "nothing is pruned after a failed migration")
			assert.Zero(t, st.predicateCalls, "the normalizer's state is read only after a successful migration")
			for _, p := range []string{fx.oldFK, fx.oldTS, fx.keptFK, fx.keptTS} {
				assert.True(t, modTime(t, p).After(before), "%s: last use must be refreshed", filepath.Base(p))
			}
			for _, p := range []string{fx.other, fx.tmpTS} {
				assert.True(t, modTime(t, p).Before(before), "%s must not be touched", filepath.Base(p))
			}
		})
	}
}

// R2: prune runs once, after the migration, with this boot's snapshots in
// the keep set.
func TestMigrateThenPrune_Success(t *testing.T) {
	fx := newSnapshotFixture(t)
	st := stubRetention(t, false, nil)
	in := &startupSnapshots{tsRepair: utcTimestampRepairResult{SnapshotPath: fx.keptTS}}
	in.recordFK(fx.keptFK)
	migrated := false
	err := migrateThenPrune(context.Background(), fx.s, func(context.Context) error {
		assert.Zero(t, st.pruneCalls, "prune must not run before the migration")
		assert.Zero(t, st.predicateCalls)
		migrated = true
		return nil
	}, in)
	require.NoError(t, err)
	assert.True(t, migrated)
	assert.Equal(t, 1, st.pruneCalls)
	assert.ElementsMatch(t, []string{fx.keptFK, fx.keptTS}, st.pruneKeep)
	assert.ElementsMatch(t, startupSnapshotPatterns(), st.prunePatterns)

	assert.NoFileExists(t, fx.oldFK)
	assert.NoFileExists(t, fx.oldTS)
	for _, p := range []string{fx.keptFK, fx.keptTS, fx.other, fx.tmpTS} {
		assert.FileExists(t, p)
	}
}

// R4, N4: while an automatic normalize is incomplete (or its state cannot be
// read, or this boot's probe failed), the timestamp snapshots stay; the
// foreign key snapshots are still pruned.
func TestMigrateThenPrune_TimestampSnapshotsHeld(t *testing.T) {
	for _, tc := range []struct {
		name       string
		incomplete bool
		predErr    error
		probe      bool
	}{
		{name: "normalize incomplete", incomplete: true},
		{name: "state unreadable", predErr: errors.New("database is locked")},
		{name: "probe failed", probe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSnapshotFixture(t)
			stubRetention(t, tc.incomplete, tc.predErr)
			in := &startupSnapshots{tsRepair: utcTimestampRepairResult{ProbeFailed: tc.probe}}
			require.NoError(t, migrateThenPrune(context.Background(), fx.s, func(context.Context) error { return nil }, in))
			assert.NoFileExists(t, fx.oldFK)
			assert.NoFileExists(t, fx.keptFK, "not in the keep set here")
			assert.FileExists(t, fx.oldTS)
			assert.FileExists(t, fx.keptTS)
		})
	}
}

// The real predicate on a migrated store, where nothing set the marker:
// the timestamp snapshots are pruned like the others.
func TestMigrateThenPrune_RealPredicate(t *testing.T) {
	fx := newSnapshotFixture(t)
	require.NoError(t, migrateThenPrune(context.Background(), fx.s, func(context.Context) error { return nil }, &startupSnapshots{}))
	assert.NoFileExists(t, fx.oldTS)
	assert.NoFileExists(t, fx.oldFK)
	assert.FileExists(t, fx.other)
}

// snapshotBeforeFKReconcileRecord passes the snapshot it writes, and the
// one it reuses, to record.
func TestSnapshotBeforeFKReconcileRecord(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("CREATE TABLE t (id TEXT PRIMARY KEY)")
	require.NoError(t, err)
	groups := []entc.FKRepairGroup{{Table: "t", Columns: []string{"c"}, Parent: "p", Action: "CASCADE", Rows: 1}}

	var recorded []string
	record := func(p string) { recorded = append(recorded, p) }
	require.NoError(t, snapshotBeforeFKReconcileRecord(ctx, db, groups, record))
	snaps := fkSnapshots(t, path)
	require.Len(t, snaps, 1)
	assert.Equal(t, snaps, recorded)

	require.NoError(t, snapshotBeforeFKReconcileRecord(ctx, db, groups, record))
	assert.Equal(t, []string{snaps[0], snaps[0]}, recorded, "a reused snapshot is recorded too")

	// A nil record is allowed (the maintenance commands).
	require.NoError(t, snapshotBeforeFKReconcile(ctx, db, groups))
}
