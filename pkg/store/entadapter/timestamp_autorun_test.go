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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoRunIncompleteSet(t *testing.T) {
	for _, c := range []struct {
		metadata string
		want     bool
	}{
		{"", false},
		{"{}", false},
		{"not json", false},
		{"[true]", false},
		{`{"autoRunIncomplete":true}`, true},
		{`{"other":1,"autoRunIncomplete":true}`, true},
		{`{"autoRunIncomplete":false}`, false},
		{`{"autoRunIncomplete":"true"}`, false},
		{`{"autoRunIncomplete":1}`, false},
		{`{"autoRunIncomplete":null}`, false},
	} {
		assert.Equal(t, c.want, AutoRunIncompleteSet(c.metadata), c.metadata)
	}
}

func TestWithAutoRunIncomplete(t *testing.T) {
	on := WithAutoRunIncomplete(`{"other":"x"}`, true)
	assert.True(t, AutoRunIncompleteSet(on))
	assert.Contains(t, on, `"other":"x"`, "other keys are kept")
	off := WithAutoRunIncomplete(on, false)
	assert.False(t, AutoRunIncompleteSet(off))
	assert.JSONEq(t, `{"other":"x"}`, off)

	assert.JSONEq(t, `{"autoRunIncomplete":true}`, WithAutoRunIncomplete("", true))
	assert.JSONEq(t, `{"autoRunIncomplete":true}`, WithAutoRunIncomplete("{}", true))
	assert.JSONEq(t, `{"autoRunIncomplete":true}`, WithAutoRunIncomplete("not json", true))
	assert.JSONEq(t, `{}`, WithAutoRunIncomplete("{}", false))
}

// newMaintenanceTestStore returns a migrated SQLite store in a file, with the
// built-in maintenance operations seeded, and its raw handle.
func newMaintenanceTestStore(t *testing.T) (*CompositeStore, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	cs := newTestCompositeStoreFromDSN(t, "file:"+path)
	require.NoError(t, cs.Migrate(context.Background()))
	return cs, cs.DB(), path
}

func setNormalizeRow(t *testing.T, cs *CompositeStore, status, startedBy, metadata string) {
	t.Helper()
	ctx := context.Background()
	op, err := cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	op.Status, op.StartedBy, op.Metadata = status, startedBy, metadata
	op.Result = `{"log":"kept"}`
	require.NoError(t, cs.UpdateMaintenanceOperation(ctx, op))
}

func TestUTCTimestampNormalizeIncomplete(t *testing.T) {
	ctx := context.Background()
	cs, db, _ := newMaintenanceTestStore(t)
	key := `{"autoRunIncomplete":true}`

	got, err := UTCTimestampNormalizeIncomplete(ctx, cs)
	require.NoError(t, err)
	assert.False(t, got, "a fresh row")

	for _, c := range []struct {
		name, status, startedBy, metadata string
		want                              bool
	}{
		{"auto-run running", store.MaintenanceStatusRunning, "system:hub-start", key, true},
		{"auto-run failed", store.MaintenanceStatusFailed, "system:hub-start", key, true},
		{"auto-run aborted to pending", store.MaintenanceStatusPending, "system:hub-start", key, true},
		{"failed manual run after an unfinished auto-run", store.MaintenanceStatusFailed, "admin@example.com", key, true},
		{"completed with a stale key", store.MaintenanceStatusCompleted, "system:hub-start", key, false},
		{"manual dry run", store.MaintenanceStatusPending, "admin@example.com", "{}", false},
		{"failed manual run", store.MaintenanceStatusFailed, "admin@example.com", "{}", false},
		{"schema default", store.MaintenanceStatusPending, "", "{}", false},
		{"empty metadata", store.MaintenanceStatusFailed, "system:hub-start", "", false},
		{"non-JSON metadata", store.MaintenanceStatusFailed, "system:hub-start", "oops", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			setNormalizeRow(t, cs, c.status, c.startedBy, c.metadata)
			got, err := UTCTimestampNormalizeIncomplete(ctx, cs)
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}

	// A crash leaves the row running; the reset at the next start keeps the
	// key, so the run still reads as incomplete.
	setNormalizeRow(t, cs, store.MaintenanceStatusRunning, "system:hub-start", key)
	_, _, err = cs.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	op, err := cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	require.Equal(t, store.MaintenanceStatusPending, op.Status)
	got, err = UTCTimestampNormalizeIncomplete(ctx, cs)
	require.NoError(t, err)
	assert.True(t, got, "aborted to pending")

	_, err = db.Exec(`DELETE FROM maintenance_operations WHERE "key" = ?`, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	got, err = UTCTimestampNormalizeIncomplete(ctx, cs)
	require.NoError(t, err)
	assert.False(t, got, "a missing row")
}

// fakeMaintenanceStore returns a fixed maintenance operation or error and
// records the key it was asked for.
type fakeMaintenanceStore struct {
	store.Store
	op  *store.MaintenanceOperation
	err error
	key string
}

func (f *fakeMaintenanceStore) GetMaintenanceOperation(_ context.Context, key string) (*store.MaintenanceOperation, error) {
	f.key = key
	return f.op, f.err
}

// The predicate over each row state and read result, without a database.
func TestUTCTimestampNormalizeIncomplete_Table(t *testing.T) {
	op := func(status, startedBy, metadata string) *store.MaintenanceOperation {
		return &store.MaintenanceOperation{Key: UTCTimestampNormalizeKey, Status: status, StartedBy: startedBy, Metadata: metadata}
	}
	const marked = `{"autoRunIncomplete":true}`
	for _, tc := range []struct {
		name    string
		op      *store.MaintenanceOperation
		err     error
		want    bool
		wantErr bool
	}{
		{name: "no row", err: store.ErrNotFound},
		{name: "read error", err: errors.New("database is locked"), wantErr: true},
		{name: "seeded pending row", op: op(store.MaintenanceStatusPending, "", "")},
		{name: "running auto-run", op: op(store.MaintenanceStatusRunning, "system:hub-start", marked), want: true},
		{name: "failed auto-run", op: op(store.MaintenanceStatusFailed, "system:hub-start", marked), want: true},
		{name: "aborted auto-run back to pending", op: op(store.MaintenanceStatusPending, "system:hub-start", marked), want: true},
		{name: "failed manual run after an unfinished auto-run", op: op(store.MaintenanceStatusFailed, "admin@example.com", marked), want: true},
		{name: "completed", op: op(store.MaintenanceStatusCompleted, "system:hub-start", marked)},
		{name: "dry run leaves pending with StartedBy", op: op(store.MaintenanceStatusPending, "admin@example.com", `{"dryRun":true}`)},
		{name: "failed manual run", op: op(store.MaintenanceStatusFailed, "admin@example.com", "")},
		{name: "key false", op: op(store.MaintenanceStatusFailed, "system:hub-start", `{"autoRunIncomplete":false}`)},
		{name: "key a string", op: op(store.MaintenanceStatusFailed, "system:hub-start", `{"autoRunIncomplete":"true"}`)},
		{name: "not JSON", op: op(store.MaintenanceStatusFailed, "system:hub-start", "autoRunIncomplete")},
		{name: "JSON array", op: op(store.MaintenanceStatusFailed, "system:hub-start", `[true]`)},
		{name: "empty JSON object", op: op(store.MaintenanceStatusFailed, "system:hub-start", `{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeMaintenanceStore{op: tc.op, err: tc.err}
			got, err := UTCTimestampNormalizeIncomplete(context.Background(), f)
			assert.Equal(t, UTCTimestampNormalizeKey, f.key)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClearUTCTimestampAutoRunKey(t *testing.T) {
	ctx := context.Background()
	cs, db, _ := newMaintenanceTestStore(t)
	key := `{"other":1,"autoRunIncomplete":true}`

	// A run in flight: nothing is written.
	setNormalizeRow(t, cs, store.MaintenanceStatusRunning, "admin@example.com", key)
	cleared, err := ClearUTCTimestampAutoRunKey(ctx, db, dialect.SQLite)
	require.NoError(t, err)
	assert.False(t, cleared)
	op, err := cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusRunning, op.Status)
	assert.True(t, AutoRunIncompleteSet(op.Metadata))

	// Another dialect: nothing is written.
	setNormalizeRow(t, cs, store.MaintenanceStatusFailed, "system:hub-start", key)
	cleared, err = ClearUTCTimestampAutoRunKey(ctx, db, dialect.Postgres)
	require.NoError(t, err)
	assert.False(t, cleared)

	// Not running: only Metadata changes, and other keys stay.
	cleared, err = ClearUTCTimestampAutoRunKey(ctx, db, dialect.SQLite)
	require.NoError(t, err)
	assert.True(t, cleared)
	op, err = cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusFailed, op.Status)
	assert.Equal(t, "system:hub-start", op.StartedBy)
	assert.Equal(t, `{"log":"kept"}`, op.Result)
	assert.False(t, AutoRunIncompleteSet(op.Metadata))
	assert.JSONEq(t, `{"other":1}`, op.Metadata)

	// No key: nothing to do.
	cleared, err = ClearUTCTimestampAutoRunKey(ctx, db, dialect.SQLite)
	require.NoError(t, err)
	assert.False(t, cleared)
}

// TestClearAutoRunKeyCAS pins the SQL guards of the compare-and-set itself,
// which the caller's read cannot stand in for: a row that became running, or
// whose Metadata changed, after that read is left alone.
func TestClearAutoRunKeyCAS(t *testing.T) {
	ctx := context.Background()
	cs, db, _ := newMaintenanceTestStore(t)
	key := `{"other":1,"autoRunIncomplete":true}`

	// (a) A manual run started after the read: the status guard holds.
	setNormalizeRow(t, cs, store.MaintenanceStatusRunning, "admin@example.com", key)
	changed, err := clearAutoRunKeyCAS(ctx, db, key)
	require.NoError(t, err)
	assert.False(t, changed)
	op, err := cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusRunning, op.Status)
	assert.Equal(t, key, op.Metadata)

	// (b) Metadata changed after the read: the metadata guard holds.
	setNormalizeRow(t, cs, store.MaintenanceStatusFailed, "system:hub-start", key)
	started := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	op, err = cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	op.StartedAt, op.CompletedAt = &started, &completed
	require.NoError(t, cs.UpdateMaintenanceOperation(ctx, op))
	changed, err = clearAutoRunKeyCAS(ctx, db, `{"autoRunIncomplete":true}`)
	require.NoError(t, err)
	assert.False(t, changed, "a concurrent Metadata change makes the clear a no-op")
	op, err = cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	assert.Equal(t, key, op.Metadata)

	// (c) The current text: one row, only Metadata changes.
	changed, err = clearAutoRunKeyCAS(ctx, db, key)
	require.NoError(t, err)
	assert.True(t, changed)
	op, err = cs.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusFailed, op.Status)
	assert.Equal(t, "system:hub-start", op.StartedBy)
	assert.Equal(t, `{"log":"kept"}`, op.Result)
	assert.JSONEq(t, `{"other":1}`, op.Metadata)
	require.NotNil(t, op.StartedAt)
	require.NotNil(t, op.CompletedAt)
	assert.True(t, op.StartedAt.Equal(started), "started_at must not change")
	assert.True(t, op.CompletedAt.Equal(completed), "completed_at must not change")
}

func TestRemoveStaleSnapshotTemp(t *testing.T) {
	ctx := context.Background()
	db, path := openOldSchema(t)
	tmp := path + ".pre-test.tmp"
	const age = 35 * time.Minute

	removed, mod, err := RemoveStaleSnapshotTemp(ctx, db, "pre-test", age, time.Now())
	require.NoError(t, err)
	assert.False(t, removed)
	assert.True(t, mod.IsZero(), "no file")

	require.NoError(t, os.WriteFile(tmp, nil, 0o600))
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(tmp, base, base))

	removed, mod, err = RemoveStaleSnapshotTemp(ctx, db, "pre-test", age, base.Add(age-time.Second))
	require.NoError(t, err)
	assert.False(t, removed, "younger than the age")
	assert.True(t, mod.Equal(base))
	_, err = os.Stat(tmp)
	require.NoError(t, err)

	removed, _, err = RemoveStaleSnapshotTemp(ctx, db, "pre-test", age, base.Add(age))
	require.NoError(t, err)
	assert.True(t, removed, "exactly the age counts as stale")
	_, err = os.Stat(tmp)
	assert.ErrorIs(t, err, os.ErrNotExist)

	mem, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	mem.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = mem.Close() })
	removed, _, err = RemoveStaleSnapshotTemp(ctx, mem, "pre-test", age, time.Now())
	require.NoError(t, err)
	assert.False(t, removed)
}

func TestSnapshotTimeFromName(t *testing.T) {
	const label = "pre-utc-timestamp-normalize"
	got, ok := SnapshotTimeFromName("/d/hub.db."+label+"-20261002T221005Z.bak", label)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 10, 2, 22, 10, 5, 0, time.UTC), got)
	for _, name := range []string{
		"/d/hub.db." + label + "-manual.bak",
		"/d/hub.db." + label + "-20261302T221005Z.bak",
		"/d/hub.db." + label + "-20261002T221005Z.tmp",
		"/d/hub.db.pre-other-20261002T221005Z.bak",
		"x.bak",
	} {
		_, ok := SnapshotTimeFromName(name, label)
		assert.False(t, ok, name)
	}
}

// TestSnapshotSQLite_CancelDuringCopy cancels while VACUUM INTO is copying:
// the driver interrupts the statement, and the call removes its temporary
// file and leaves no snapshot. The hub's shutdown relies on this.
func TestSnapshotSQLite_CancelDuringCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.db")
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("CREATE TABLE b (v BLOB)")
	require.NoError(t, err)
	// About 160 MB, so that the copy takes long enough to cancel inside it.
	_, err = db.Exec(`INSERT INTO b SELECT randomblob(4096) FROM
		(WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 40000) SELECT x FROM c)`)
	require.NoError(t, err)

	tmp := path + ".pre-test.tmp"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel once the copy has started writing into the temporary file.
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if fi, err := os.Stat(tmp); err == nil && fi.Size() > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err = SnapshotSQLite(ctx, db, "pre-test", time.Now())
	if err == nil {
		t.Skip("the copy finished before the cancel arrived; the interrupt was not exercised")
	}
	assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "interrupt"),
		"got %v", err)
	_, statErr := os.Stat(tmp)
	assert.True(t, os.IsNotExist(statErr), "the temporary file must be removed")
	matches, _ := filepath.Glob(path + ".pre-test-*")
	assert.Empty(t, matches, "no snapshot")
}
