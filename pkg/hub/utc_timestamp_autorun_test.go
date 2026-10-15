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

package hub

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the hub's run of utc-timestamp-normalize at start
// (ptone/scion#5194): an upgraded hub rewrites readable non-canonical
// timestamps once, after a snapshot, with no manual step.

// newFileNormServer returns a hub on a migrated SQLite store in a file, so
// that the run at start can snapshot it.
func newFileNormServer(t *testing.T) (*Server, store.Store, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := newTestStore(t, path)
	require.NoError(t, err)
	db := s.(interface{ DB() *sql.DB }).DB()
	return &Server{store: s}, s, db, path
}

// seedNormMsg creates a message and stores text as its created time.
func seedNormMsg(t *testing.T, s store.Store, db *sql.DB, name, text string) string {
	t.Helper()
	ctx := context.Background()
	proj := normProject(t, s, "auto-"+name)
	id := tid("auto-msg-" + name)
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: id, ProjectID: proj.ID, Sender: "user:alice", Recipient: "agent:bot",
		Msg: name, Channel: "web", CreatedAt: normBase,
	}))
	execOne(t, db, "UPDATE messages SET created = ? WHERE id = ?", text, id)
	return id
}

func msgCreated(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	return rawText(t, db, "SELECT CAST(created AS TEXT) FROM messages WHERE id = ?", id)
}

func normOp(t *testing.T, s store.Store) *store.MaintenanceOperation {
	t.Helper()
	op, err := s.GetMaintenanceOperation(context.Background(), entadapter.UTCTimestampNormalizeKey)
	require.NoError(t, err)
	return op
}

func setNormOp(t *testing.T, s store.Store, status, startedBy, metadata string) {
	t.Helper()
	op := normOp(t, s)
	op.Status, op.StartedBy, op.Metadata = status, startedBy, metadata
	require.NoError(t, s.UpdateMaintenanceOperation(context.Background(), op))
}

func normSnapshots(t *testing.T, path string) []string {
	t.Helper()
	m, err := filepath.Glob(path + "." + entadapter.UTCTimestampSnapshotLabel + "-*.bak")
	require.NoError(t, err)
	return m
}

func normTmp(path string) string {
	return path + "." + entadapter.UTCTimestampSnapshotLabel + ".tmp"
}

// setVar sets *p to v for the test.
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

// syncSlog captures slog output in a buffer that background goroutines may
// write to.
func syncSlog(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

func normIncomplete(t *testing.T, s store.Store) bool {
	t.Helper()
	got, err := entadapter.UTCTimestampNormalizeIncomplete(context.Background(), s)
	require.NoError(t, err)
	return got
}

// TestUTCTimestampAutoRun_NormalizesReadableAtStart is the acceptance test:
// readable (and unreadable) non-canonical values are rewritten at start with
// no manual step, after one snapshot, and the log holds no ERROR or WARN.
func TestUTCTimestampAutoRun_NormalizesReadableAtStart(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"named-zone", "2026-10-01 13:00:00 +0900 JST", "2026-10-01 04:00:00 +0000 UTC"},
		{"monotonic", "2026-10-01 05:00:00 -0700 PDT m=+0.5", "2026-10-01 12:00:00 +0000 UTC"},
		{"rfc3339", "2026-10-01T13:00:00+09:00", "2026-10-01 04:00:00 +0000 UTC"},
		{"numeric-zone", "2026-10-01 09:45:00 +0545 +0545", "2026-10-01 04:00:00 +0000 UTC"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, s, db, path := newFileNormServer(t)
			ctx := context.Background()
			id := seedNormMsg(t, s, db, c.name, c.text)

			logs := captureSlog(t)
			srv.checkStoredTimestamps(ctx)
			out := logs.String()

			assert.Equal(t, c.want, msgCreated(t, db, id))
			assert.NotContains(t, out, "level=ERROR")
			assert.NotContains(t, out, "level=WARN")
			assert.Contains(t, out, "stored timestamps normalized at hub start")
			assert.NotContains(t, out, c.text, "a stored value reached the log")
			assert.Len(t, normSnapshots(t, path), 1, "one snapshot before the rewrite")

			op := normOp(t, s)
			assert.Equal(t, store.MaintenanceStatusCompleted, op.Status, op.Result)
			assert.Equal(t, "system:hub-start", op.StartedBy)
			assert.Contains(t, op.Result, "rewrote 1 values")
			assert.False(t, entadapter.AutoRunIncompleteSet(op.Metadata))
			assert.False(t, normIncomplete(t, s))

			chk, err := entadapter.CheckStoredTimestamps(ctx, db, "sqlite3")
			require.NoError(t, err)
			assert.Empty(t, chk.NeedsRun)
		})
	}
}

// TestUTCTimestampAutoRun_CanonicalStoreWritesNothing: no snapshot, no op
// row write, no log.
func TestUTCTimestampAutoRun_CanonicalStoreWritesNothing(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	before := normOp(t, s)
	_ = seedNormMsg(t, s, db, "canon", "2026-10-01 04:00:00 +0000 UTC")

	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	assert.Empty(t, logs.String())
	assert.Empty(t, normSnapshots(t, path))
	after := normOp(t, s)
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.Metadata, after.Metadata)
	assert.Empty(t, after.StartedBy)
}

// TestUTCTimestampAutoRun_FailureRetriedNextStart: a failed snapshot writes
// nothing, logs one ERROR, keeps the run incomplete, and the next start
// completes it.
func TestUTCTimestampAutoRun_FailureRetriedNextStart(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	ctx := context.Background()
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	const unreadable = "2026-10-01 09:45:00 +0545 +0545"
	id := seedNormMsg(t, s, db, "fail", legacy)
	idU := seedNormMsg(t, s, db, "fail-unreadable", unreadable)

	orig := timestampSnapshot
	setVar(t, &timestampSnapshot, func(context.Context, *sql.DB, string, time.Time) (entadapter.SQLiteSnapshot, error) {
		return entadapter.SQLiteSnapshot{}, errors.New("no space left on device")
	})
	logs := captureSlog(t)
	srv.checkStoredTimestamps(ctx)
	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "level=ERROR"), out)
	assert.Contains(t, out, "retries it at the next start")
	assert.Contains(t, out, "tables=[messages]")
	assert.Contains(t, out, "tables_unreadable=[messages]")
	assert.Equal(t, legacy, msgCreated(t, db, id), "nothing is written without a snapshot")
	assert.Equal(t, unreadable, msgCreated(t, db, idU))
	op := normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusFailed, op.Status)
	assert.Contains(t, op.Result, "no space left on device")
	assert.True(t, normIncomplete(t, s))

	timestampSnapshot = orig
	logs.Reset()
	(&Server{store: s}).checkStoredTimestamps(ctx)
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.Equal(t, "2026-10-01 04:00:00 +0000 UTC", msgCreated(t, db, id))
	assert.Equal(t, "2026-10-01 04:00:00 +0000 UTC", msgCreated(t, db, idU))
	assert.Equal(t, store.MaintenanceStatusCompleted, normOp(t, s).Status)
	assert.False(t, normIncomplete(t, s))
	assert.Len(t, normSnapshots(t, path), 1)
}

// TestUTCTimestampAutoRun_BudgetStopIsWarning: a run stopped by its time
// budget is a warning, not an error, and stays incomplete.
func TestUTCTimestampAutoRun_BudgetStopIsWarning(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	id := seedNormMsg(t, s, db, "budget-stop", legacy)
	setVar(t, &timestampAutoRunBudget, 100*time.Millisecond)
	setVar(t, &timestampSnapshot, func(ctx context.Context, _ *sql.DB, _ string, _ time.Time) (entadapter.SQLiteSnapshot, error) {
		<-ctx.Done()
		return entadapter.SQLiteSnapshot{}, ctx.Err()
	})
	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	out := logs.String()
	assert.Contains(t, out, "stopped at hub shutdown or at its time budget")
	assert.NotContains(t, out, "level=ERROR")
	assert.Equal(t, legacy, msgCreated(t, db, id))
	assert.Equal(t, store.MaintenanceStatusFailed, normOp(t, s).Status)
	assert.True(t, normIncomplete(t, s))
}

// TestUTCTimestampAutoRun_RecheckHasItsOwnBound: a first check that uses up
// its whole bound does not turn a successful run into a re-check error.
func TestUTCTimestampAutoRun_RecheckHasItsOwnBound(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "slow-check", "2026-10-01 13:00:00 +0900 JST")
	setVar(t, &startupTimestampCheckTimeout, 300*time.Millisecond)
	calls := 0
	orig := checkStoredTimestampsFn
	setVar(t, &checkStoredTimestampsFn, func(ctx context.Context, db *sql.DB, d string) (entadapter.TimestampCheck, error) {
		calls++
		if calls == 1 {
			<-ctx.Done() // the first check takes its whole bound
			return orig(context.Background(), db, d)
		}
		return orig(ctx, db, d)
	})
	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	assert.Equal(t, 2, calls)
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), "stored timestamps normalized at hub start")
	assert.Equal(t, store.MaintenanceStatusCompleted, normOp(t, s).Status)
}

// TestUTCTimestampAutoRun_RunInProgressSkipped: a run already in flight is
// left alone and nothing is written.
func TestUTCTimestampAutoRun_RunInProgressSkipped(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	id := seedNormMsg(t, s, db, "inflight", legacy)
	setNormOp(t, s, store.MaintenanceStatusRunning, "admin@example.com", "{}")

	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	assert.Contains(t, logs.String(), "run of the operation is in progress")
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.Equal(t, legacy, msgCreated(t, db, id))
	assert.Empty(t, normSnapshots(t, path))
	op := normOp(t, s)
	assert.Equal(t, "admin@example.com", op.StartedBy)
	assert.False(t, entadapter.AutoRunIncompleteSet(op.Metadata))
}

// failingRecordStore fails every write of a maintenance operation row.
type failingRecordStore struct {
	store.Store
	db *sql.DB
}

func (f *failingRecordStore) UpdateMaintenanceOperation(context.Context, *store.MaintenanceOperation) error {
	return errors.New("disk I/O error")
}
func (f *failingRecordStore) DB() *sql.DB     { return f.db }
func (f *failingRecordStore) Dialect() string { return "sqlite3" }

// TestUTCTimestampAutoRun_NoRecordNoRun: when the run cannot be recorded
// (the incomplete state the snapshot retention relies on), nothing runs.
func TestUTCTimestampAutoRun_NoRecordNoRun(t *testing.T) {
	_, s, db, path := newFileNormServer(t)
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	id := seedNormMsg(t, s, db, "norecord", legacy)

	logs := captureSlog(t)
	(&Server{store: &failingRecordStore{Store: s, db: db}}).checkStoredTimestamps(context.Background())
	assert.Contains(t, logs.String(), "could not record the run; not started")
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.Equal(t, legacy, msgCreated(t, db, id))
	assert.Empty(t, normSnapshots(t, path))
	_, err := os.Stat(normTmp(path))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestUTCTimestampAutoRun_StopDuringSnapshot: a shutdown during the snapshot
// returns at once, records the run as failed (still incomplete), logs no
// ERROR, and the next start completes the run.
func TestUTCTimestampAutoRun_StopDuringSnapshot(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	ctx := context.Background()
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	id := seedNormMsg(t, s, db, "stop", legacy)
	logs := syncSlog(t)

	entered := make(chan struct{})
	orig := timestampSnapshot
	setVar(t, &timestampSnapshot, func(ctx context.Context, _ *sql.DB, _ string, _ time.Time) (entadapter.SQLiteSnapshot, error) {
		close(entered)
		<-ctx.Done()
		return entadapter.SQLiteSnapshot{}, ctx.Err()
	})
	srv.startStoredTimestampCheck(ctx)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the snapshot")
	}
	start := time.Now()
	srv.stopStoredTimestampCheck()
	assert.Less(t, time.Since(start), 2*time.Second)
	out := logs.String()
	assert.Contains(t, out, "stopped at hub shutdown or at its time budget")
	assert.NotContains(t, out, "level=ERROR")
	assert.NotContains(t, out, "still running at shutdown")
	assert.Equal(t, legacy, msgCreated(t, db, id))
	op := normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusFailed, op.Status)
	assert.True(t, normIncomplete(t, s))
	_, err := os.Stat(normTmp(path))
	assert.ErrorIs(t, err, os.ErrNotExist)

	timestampSnapshot = orig
	(&Server{store: s}).checkStoredTimestamps(ctx)
	assert.Equal(t, "2026-10-01 04:00:00 +0000 UTC", msgCreated(t, db, id))
	assert.Equal(t, store.MaintenanceStatusCompleted, normOp(t, s).Status)
}

// TestUTCTimestampAutoRun_StopGraceBounded: a run that ignores the cancel
// does not hold shutdown past the grace.
func TestUTCTimestampAutoRun_StopGraceBounded(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "grace", "2026-10-01 13:00:00 +0900 JST")
	logs := syncSlog(t)
	setVar(t, &timestampAutoRunStopGrace, 100*time.Millisecond)

	entered, release := make(chan struct{}), make(chan struct{})
	setVar(t, &timestampSnapshot, func(context.Context, *sql.DB, string, time.Time) (entadapter.SQLiteSnapshot, error) {
		close(entered)
		<-release
		return entadapter.SQLiteSnapshot{}, errors.New("released")
	})
	srv.startStoredTimestampCheck(context.Background())
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached the snapshot")
	}
	start := time.Now()
	srv.stopStoredTimestampCheck()
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Contains(t, logs.String(), "still running at shutdown")
	close(release)
	srv.timestampAutoRun.running.Wait()
	assert.Equal(t, store.MaintenanceStatusFailed, normOp(t, s).Status)
}

// TestUTCTimestampAutoRun_StopBeforeStart: a start after a stop does nothing.
func TestUTCTimestampAutoRun_StopBeforeStart(t *testing.T) {
	called := make(chan struct{}, 1)
	setVar(t, &storedTimestampCheck, func(*Server, context.Context) { called <- struct{}{} })
	srv := &Server{}
	srv.stopStoredTimestampCheck()
	srv.startStoredTimestampCheck(context.Background())
	srv.timestampAutoRun.running.Wait()
	select {
	case <-called:
		t.Fatal("the check ran after stop")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestUTCTimestampAutoRun_PanicFinalizesRow: a panic during the run leaves
// the row failed (not running) with the panic in its result.
func TestUTCTimestampAutoRun_PanicFinalizesRow(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "panic", "2026-10-01 13:00:00 +0900 JST")
	setVar(t, &timestampSnapshot, func(context.Context, *sql.DB, string, time.Time) (entadapter.SQLiteSnapshot, error) {
		panic("boom")
	})
	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	assert.Contains(t, logs.String(), "timestamp normalize: recovered from panic")
	op := normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusFailed, op.Status)
	assert.Contains(t, op.Result, "panic: boom")
	assert.True(t, normIncomplete(t, s))
}

// TestUTCTimestampAutoRun_RearmAfterYoungTmp: a young snapshot temporary file
// defers the run once; when the file has aged it is removed and the run
// completes, with no ERROR. The clock is injected: it stands still at the
// file's modification time until the first attempt has met the file, then
// moves past the stale age, so no decision depends on scheduling.
func TestUTCTimestampAutoRun_RearmAfterYoungTmp(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	id := seedNormMsg(t, s, db, "rearm", "2026-10-01 13:00:00 +0900 JST")
	const stale = time.Hour
	t0 := time.Now().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	setVar(t, &timestampNow, func() time.Time { return time.Unix(0, clock.Load()) })
	setVar(t, &timestampStaleTmpAge, stale)
	require.NoError(t, os.WriteFile(normTmp(path), nil, 0o600))
	require.NoError(t, os.Chtimes(normTmp(path), t0, t0))
	calls := 0
	orig := timestampSnapshot
	setVar(t, &timestampSnapshot, func(ctx context.Context, db *sql.DB, label string, now time.Time) (entadapter.SQLiteSnapshot, error) {
		calls++
		snap, err := orig(ctx, db, label, now)
		if calls == 1 {
			require.ErrorIs(t, err, entadapter.ErrSnapshotInProgress)
			clock.Add(int64(stale)) // the file is now exactly stale; the re-arm fires at once
		}
		return snap, err
	})

	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	out := logs.String()
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, strings.Count(out, "nothing was written, the hub retries later"), out)
	assert.Contains(t, out, "removed the temporary file of an interrupted snapshot")
	assert.NotContains(t, out, "level=ERROR")
	assert.Equal(t, "2026-10-01 04:00:00 +0000 UTC", msgCreated(t, db, id))
	assert.Equal(t, store.MaintenanceStatusCompleted, normOp(t, s).Status)
	_, err := os.Stat(normTmp(path))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Len(t, normSnapshots(t, path), 1)
}

// TestUTCTimestampAutoRun_RearmAtMostOnce: a file still young at the re-arm
// logs a warning, leaves the row failed, and schedules no third attempt.
func TestUTCTimestampAutoRun_RearmAtMostOnce(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	id := seedNormMsg(t, s, db, "once", legacy)
	t0 := time.Now()
	setVar(t, &timestampNow, func() time.Time { return t0 })
	setVar(t, &timestampStaleTmpAge, 50*time.Millisecond)
	require.NoError(t, os.WriteFile(normTmp(path), nil, 0o600))
	require.NoError(t, os.Chtimes(normTmp(path), t0, t0))

	logs := captureSlog(t)
	done := make(chan struct{})
	go func() {
		srv.checkStoredTimestamps(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the check did not return after its one re-arm")
	}
	out := logs.String()
	assert.Equal(t, 2, strings.Count(out, "nothing was written, the hub retries later"), out)
	assert.Contains(t, out, "the hub retries at the next start")
	assert.NotContains(t, out, "level=ERROR")
	assert.Equal(t, legacy, msgCreated(t, db, id))
	assert.Equal(t, store.MaintenanceStatusFailed, normOp(t, s).Status)
	assert.True(t, normIncomplete(t, s))
}

// TestUTCTimestampAutoRun_StopDuringRearmWait: a stop during the wait
// returns at once and leaves the row failed, not running.
func TestUTCTimestampAutoRun_StopDuringRearmWait(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "wait", "2026-10-01 13:00:00 +0900 JST")
	setVar(t, &timestampStaleTmpAge, time.Hour)
	require.NoError(t, os.WriteFile(normTmp(path), nil, 0o600))
	logs := syncSlog(t)

	srv.startStoredTimestampCheck(context.Background())
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "nothing was written, the hub retries later")
	}, 10*time.Second, 10*time.Millisecond)
	start := time.Now()
	srv.stopStoredTimestampCheck()
	assert.Less(t, time.Since(start), time.Second)
	assert.NotContains(t, logs.String(), "still running at shutdown")
	assert.Equal(t, store.MaintenanceStatusFailed, normOp(t, s).Status)
}

// TestUTCTimestampAutoRun_ManualCompletionDuringWait: if a manual run
// completes during the wait, the re-armed attempt finds nothing to do and
// writes no snapshot; it only clears the incomplete-run key.
func TestUTCTimestampAutoRun_ManualCompletionDuringWait(t *testing.T) {
	srv, s, db, path := newFileNormServer(t)
	ctx := context.Background()
	id := seedNormMsg(t, s, db, "manual", "2026-10-01 13:00:00 +0900 JST")
	// The clock stands still at the file's modification time, so the
	// re-armed attempt can never count the file as stale: it can only find
	// nothing left to do. The wait (the stale age) leaves time for the
	// manual run below.
	t0 := time.Now().Truncate(time.Second)
	setVar(t, &timestampNow, func() time.Time { return t0 })
	setVar(t, &timestampStaleTmpAge, 5*time.Second)
	require.NoError(t, os.WriteFile(normTmp(path), nil, 0o600))
	require.NoError(t, os.Chtimes(normTmp(path), t0, t0))
	logs := syncSlog(t)

	srv.startStoredTimestampCheck(ctx)
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "nothing was written, the hub retries later")
	}, 10*time.Second, 10*time.Millisecond)
	// A manual run, as executeMigration records it: it writes back the
	// Metadata it read.
	runNormalize(t, db, entadapter.TimestampNormalizeOptions{})
	op := normOp(t, s)
	op.Status, op.StartedBy = store.MaintenanceStatusCompleted, "admin@example.com"
	require.NoError(t, s.UpdateMaintenanceOperation(ctx, op))

	require.Eventually(t, func() bool {
		return !entadapter.AutoRunIncompleteSet(normOp(t, s).Metadata)
	}, 20*time.Second, 20*time.Millisecond)
	srv.stopStoredTimestampCheck()
	assert.Empty(t, normSnapshots(t, path))
	op = normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusCompleted, op.Status)
	assert.Equal(t, "admin@example.com", op.StartedBy)
	assert.Equal(t, "2026-10-01 04:00:00 +0000 UTC", msgCreated(t, db, id))
}

func TestUTCTimestampAutoRun_RearmDelay(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	const age = 35 * time.Minute
	for _, c := range []struct {
		name string
		mod  time.Time
		want time.Duration
	}{
		{"just written", now, age},
		{"half way", now.Add(-10 * time.Minute), 25 * time.Minute},
		{"already stale", now.Add(-time.Hour), 0},
		{"future mtime is clamped", now.Add(24 * time.Hour), age},
	} {
		assert.Equal(t, c.want, rearmDelay(c.mod, now, age), c.name)
	}
}

// TestUTCTimestampAutoRun_BudgetFromOuterContext: the run's budget does not
// come from the check's bound.
func TestUTCTimestampAutoRun_BudgetFromOuterContext(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "budget", "2026-10-01 13:00:00 +0900 JST")
	setVar(t, &startupTimestampCheckTimeout, 5*time.Second)
	orig := timestampSnapshot
	setVar(t, &timestampSnapshot, func(ctx context.Context, db *sql.DB, label string, now time.Time) (entadapter.SQLiteSnapshot, error) {
		dl, ok := ctx.Deadline()
		assert.True(t, ok)
		assert.Greater(t, time.Until(dl), 10*time.Minute, "the run must have its own budget")
		return orig(ctx, db, label, now)
	})
	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.Equal(t, store.MaintenanceStatusCompleted, normOp(t, s).Status)
}

// TestUTCTimestampAutoRun_UnparseableLeftover: after a run only values that
// do not parse remain: one warning, no error, row completed.
func TestUTCTimestampAutoRun_UnparseableLeftover(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "good", "2026-10-01 13:00:00 +0900 JST")
	_ = seedNormMsg(t, s, db, "bad", "garbage-auto-7f3a")

	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	out := logs.String()
	assert.NotContains(t, out, "level=ERROR")
	assert.Equal(t, 1, strings.Count(out, "level=WARN"), out)
	assert.Contains(t, out, "cannot parse remain")
	assert.NotContains(t, out, "garbage-auto-7f3a")
	op := normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusCompleted, op.Status)
	assert.Contains(t, op.Result, "1 unparseable")
}

// TestUTCTimestampAutoRun_ReusesEarlierSnapshot: a snapshot left by an
// earlier run or boot repair is reused, logged with its path and its age
// from the file name; a name without a time logs age=unknown.
func TestUTCTimestampAutoRun_ReusesEarlierSnapshot(t *testing.T) {
	for _, c := range []struct {
		suffix  string
		unknown bool
	}{
		{"-20260101T000000Z.bak", false},
		{"-manual.bak", true},
	} {
		t.Run(c.suffix, func(t *testing.T) {
			srv, s, db, path := newFileNormServer(t)
			_ = seedNormMsg(t, s, db, "reuse", "2026-10-01 13:00:00 +0900 JST")
			earlier := path + "." + entadapter.UTCTimestampSnapshotLabel + c.suffix
			require.NoError(t, os.WriteFile(earlier, []byte("earlier copy"), 0o600))

			logs := captureSlog(t)
			srv.checkStoredTimestamps(context.Background())
			out := logs.String()
			assert.Contains(t, out, "reusing the database snapshot")
			assert.Contains(t, out, earlier)
			if c.unknown {
				assert.Contains(t, out, "age=unknown")
			} else {
				assert.NotContains(t, out, "age=unknown")
				assert.Contains(t, out, " age=")
			}
			m, err := filepath.Glob(path + "." + entadapter.UTCTimestampSnapshotLabel + "-*.bak")
			require.NoError(t, err)
			assert.Equal(t, []string{earlier}, m, "no new snapshot")
			assert.Equal(t, store.MaintenanceStatusCompleted, normOp(t, s).Status)
		})
	}
}

// dialectStore reports another dialect for a SQLite store.
type dialectStore struct {
	store.Store
	db      *sql.DB
	dialect string
}

func (d *dialectStore) DB() *sql.DB     { return d.db }
func (d *dialectStore) Dialect() string { return d.dialect }

// TestUTCTimestampAutoRun_NonSQLiteDialect: nothing runs on other dialects.
func TestUTCTimestampAutoRun_NonSQLiteDialect(t *testing.T) {
	_, s, db, path := newFileNormServer(t)
	const legacy = "2026-10-01 13:00:00 +0900 JST"
	id := seedNormMsg(t, s, db, "pg", legacy)
	logs := captureSlog(t)
	(&Server{store: &dialectStore{Store: s, db: db, dialect: "postgres"}}).checkStoredTimestamps(context.Background())
	assert.Empty(t, logs.String())
	assert.Equal(t, legacy, msgCreated(t, db, id))
	assert.Empty(t, normSnapshots(t, path))
	assert.Equal(t, store.MaintenanceStatusPending, normOp(t, s).Status)
}

// TestUTCTimestampAutoRun_StillNonCanonicalCompletes: when the run completes
// but the re-check still reports tables, the row is completed with the key
// removed in the same write, and one ERROR names the tables.
func TestUTCTimestampAutoRun_StillNonCanonicalCompletes(t *testing.T) {
	srv, s, db, _ := newFileNormServer(t)
	_ = seedNormMsg(t, s, db, "o1", "2026-10-01 13:00:00 +0900 JST")
	calls := 0
	orig := checkStoredTimestampsFn
	setVar(t, &checkStoredTimestampsFn, func(ctx context.Context, db *sql.DB, d string) (entadapter.TimestampCheck, error) {
		calls++
		chk, err := orig(ctx, db, d)
		if calls == 2 {
			chk.NeedsRun = []string{"messages"}
		}
		return chk, err
	})
	logs := captureSlog(t)
	srv.checkStoredTimestamps(context.Background())
	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "level=ERROR"), out)
	assert.Contains(t, out, "still not in canonical UTC form after the")
	op := normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusCompleted, op.Status)
	assert.False(t, entadapter.AutoRunIncompleteSet(op.Metadata))
}

// TestUTCTimestampAutoRun_CanonicalCheckKeyClear: on a canonical store the
// check clears a key left by an earlier run, but never touches a row with a
// run in flight.
func TestUTCTimestampAutoRun_CanonicalCheckKeyClear(t *testing.T) {
	srv, s, _, path := newFileNormServer(t)
	key := `{"autoRunIncomplete":true}`

	setNormOp(t, s, store.MaintenanceStatusRunning, "admin@example.com", key)
	srv.checkStoredTimestamps(context.Background())
	op := normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusRunning, op.Status)
	assert.True(t, entadapter.AutoRunIncompleteSet(op.Metadata), "a row with a run in flight is left alone")

	setNormOp(t, s, store.MaintenanceStatusFailed, "system:hub-start", key)
	srv.checkStoredTimestamps(context.Background())
	op = normOp(t, s)
	assert.Equal(t, store.MaintenanceStatusFailed, op.Status)
	assert.Equal(t, "system:hub-start", op.StartedBy)
	assert.False(t, entadapter.AutoRunIncompleteSet(op.Metadata))
	assert.Empty(t, normSnapshots(t, path))
}
