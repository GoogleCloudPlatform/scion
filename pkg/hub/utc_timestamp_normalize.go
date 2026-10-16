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

package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// UTCTimestampNormalizeExecutor runs the utc-timestamp-normalize maintenance
// migration: it rewrites stored timestamps to canonical UTC text so that
// SQL ordering and comparison are exact and rows with four-digit numeric
// zone abbreviations become readable. See entadapter.NormalizeUTCTimestamps.
// It honours the dryRun parameter and is safe to run again after it has
// completed: canonical values are skipped.
type UTCTimestampNormalizeExecutor struct {
	DB *sql.DB
	// Dialect is the ent dialect of DB.
	Dialect string
}

// Run implements MaintenanceExecutor.
func (e *UTCTimestampNormalizeExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if e.DB == nil {
		return errors.New("the store has no SQL database handle; cannot normalize timestamps")
	}
	_, err := entadapter.NormalizeUTCTimestamps(ctx, e.DB, e.Dialect, logger, entadapter.TimestampNormalizeOptions{
		DryRun: params["dryRun"] == "true",
	})
	return err
}

// rerunnableMigrations lists migration-category operations that may run
// again after completing. Each one is idempotent: a second run changes
// nothing that the first run already fixed.
//
//   - utc-timestamp-normalize: the startup check keeps reporting rows it has
//     not yet rewritten (rows written by an older binary, restored from a
//     backup, or written by a later migration), so the remedy it names must
//     stay available.
//   - applied-config-tz-cleanup: its description promises a safe re-run that
//     converts 0. A run strips TZ from both env copies, so a second run
//     skips every agent it already handled and writes no agent row.
//   - auto-expose-env-normalize: a run removes every stamp it re-derives, so
//     a second run finds nothing to do; an agent skipped because a lookup
//     failed is retried by the next run.
var rerunnableMigrations = map[string]bool{
	entadapter.UTCTimestampNormalizeKey:  true,
	entadapter.AppliedConfigTZCleanupKey: true,
	entadapter.AutoExposeEnvNormalizeKey: true,
}

// storeDB returns the store's *sql.DB and ent dialect, or nil and "" when
// the store has no SQL handle.
func (s *Server) storeDB() (*sql.DB, string) {
	if p, ok := s.store.(interface {
		DB() *sql.DB
		Dialect() string
	}); ok {
		return p.DB(), p.Dialect()
	}
	return nil, ""
}

// startupTimestampCheckTimeout bounds each stored-timestamp check: the one
// at start and the one after a run. A variable so tests can shorten it.
var startupTimestampCheckTimeout = 5 * time.Minute

// Settings of the hub's run of utc-timestamp-normalize at start. Variables so
// tests can shorten them.
var (
	// timestampAutoRunBudget bounds the snapshot and the rewrite of one run,
	// the same budget as the boot-time repair. A run that exceeds it stops
	// between batches and resumes at the next start.
	timestampAutoRunBudget = 30 * time.Minute
	// timestampStaleTmpAge is the age at which a leftover snapshot temporary
	// file belongs to a killed process: more than every writer's budget.
	timestampStaleTmpAge = 35 * time.Minute
	// timestampAutoRunStopGrace bounds how long shutdown waits for a run.
	timestampAutoRunStopGrace = 5 * time.Second
	// timestampSnapshot takes, or reuses, the snapshot before a run.
	timestampSnapshot = entadapter.SnapshotSQLite
	// timestampNow is the clock of the run.
	timestampNow = time.Now
	// checkStoredTimestampsFn is the check itself.
	checkStoredTimestampsFn = entadapter.CheckStoredTimestamps
)

// timestampAutoRunStartedBy is the StartedBy of the operation row while the
// hub runs utc-timestamp-normalize at start.
const timestampAutoRunStartedBy = "system:hub-start"

// Log messages of the stored-timestamp check and the run at start. The
// release note and docs use the same wording.
const (
	msgTimestampsNeedNormalize = "stored timestamps are not in canonical UTC form and the " +
		entadapter.UTCTimestampNormalizeKey + " run at hub start did not complete; the hub retries it at the next start, " +
		"or run the operation now (Admin -> Maintenance). Until it completes, ordering and paging over the listed tables can be wrong"
	msgTimestampsStillNonCanonical = "stored timestamps are still not in canonical UTC form after the " +
		entadapter.UTCTimestampNormalizeKey + " run at hub start; its run log (Admin -> Maintenance) lists what it did. " +
		"Ordering and paging over the listed tables can be wrong"
	msgTimestampsUnparseable = "stored timestamps that the " + entadapter.UTCTimestampNormalizeKey +
		" operation cannot parse remain; its run log (Admin -> Maintenance) lists them by table, column and rowid; " +
		"correct or clear those values. Every read of a table listed in tables_unreadable fails until then"
)

// timestampAutoRunner tracks the background goroutine of the stored-timestamp
// check and its run at start, so that shutdown can stop and join it.
type timestampAutoRunner struct {
	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	running sync.WaitGroup
}

// startStoredTimestampCheck runs the stored-timestamp check, and on a SQLite
// store that needs it one run of utc-timestamp-normalize, in the background.
// On a canonical store every ent time column is one full scan, so running it
// inline would add a cost that grows with the database to every start, and
// nothing at start depends on its result. Each column is its own query, and
// the rewrite works in short batches, so on SQLite's single connection other
// work waits at most for one query or batch (or, once, for the snapshot
// copy). A panic is logged and does not take the hub down.
// stopStoredTimestampCheck stops and joins it.
func (s *Server) startStoredTimestampCheck(ctx context.Context) {
	rt := &s.timestampAutoRun
	runCtx, cancel := context.WithCancel(ctx)
	rt.mu.Lock()
	if rt.stopped || rt.cancel != nil {
		rt.mu.Unlock()
		cancel()
		return
	}
	rt.cancel = cancel
	rt.running.Add(1)
	rt.mu.Unlock()
	check := storedTimestampCheck // read once, before the goroutine starts
	go func() {
		defer rt.running.Done()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("timestamp check: recovered from panic", "panic", fmt.Sprint(r))
			}
		}()
		check(s, runCtx)
	}()
}

// stopStoredTimestampCheck cancels the background check or run and waits for
// it, for at most timestampAutoRunStopGrace: callers pass contexts without a
// deadline. A cancelled run stops between batches (a snapshot being copied is
// interrupted and its temporary file removed), records itself as failed, and
// resumes at the next start. It is safe to call more than once or without a
// start; a start after it does nothing.
func (s *Server) stopStoredTimestampCheck() {
	rt := &s.timestampAutoRun
	rt.mu.Lock()
	rt.stopped = true
	cancel := rt.cancel
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	done := make(chan struct{})
	go func() {
		rt.running.Wait()
		close(done)
	}()
	grace := timestampAutoRunStopGrace
	select {
	case <-done:
	case <-time.After(grace):
		slog.Warn("timestamp normalize still running at shutdown; it resumes at the next start", "grace", grace)
	}
}

// storedTimestampCheck is the check startStoredTimestampCheck runs. A
// variable so tests can observe that it runs in the background.
var storedTimestampCheck = (*Server).checkStoredTimestamps

// checkStoredTimestamps checks whether a SQLite store holds ent time values
// that are not canonical UTC text. Such rows misorder in SQL comparisons, and
// those with a four-digit numeric zone abbreviation (e.g. "+0545 +0545") make
// every ent read of their table fail. When some can be rewritten it runs
// utc-timestamp-normalize once (see autoNormalizeTimestamps); a young
// snapshot temporary file defers that run once, until the file is stale. It
// logs table names only, never values.
func (s *Server) checkStoredTimestamps(ctx context.Context) {
	if out, youngTmp := s.checkAndNormalizeTimestamps(ctx); out == autoRunYoungTmp {
		delay := rearmDelay(youngTmp, timestampNow(), timestampStaleTmpAge)
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if out, _ := s.checkAndNormalizeTimestamps(ctx); out == autoRunYoungTmp {
			slog.Warn("timestamp normalize: a snapshot is still in progress or was recently interrupted; " +
				"the hub retries at the next start")
		}
	}
}

// rearmDelay returns how long to wait before retrying a run that met a young
// snapshot temporary file modified at mod: until the file is staleAge old,
// clamped to [0, staleAge] so that a modification time in the future (clock
// skew) cannot defer the retry longer.
func rearmDelay(mod, now time.Time, staleAge time.Duration) time.Duration {
	d := mod.Add(staleAge).Sub(now)
	if d < 0 {
		return 0
	}
	if d > staleAge {
		return staleAge
	}
	return d
}

// autoRunOutcome is the result of one checkAndNormalizeTimestamps call.
type autoRunOutcome int

const (
	autoRunNothing  autoRunOutcome = iota // nothing to do, or the check failed
	autoRunDone                           // the run completed
	autoRunSkipped                        // not started (a run in flight, or no row)
	autoRunFailed                         // the run failed
	autoRunStopped                        // cancelled, or out of budget
	autoRunYoungTmp                       // a young snapshot temporary file
)

// checkAndNormalizeTimestamps runs the check and, when some tables hold
// values the operation rewrites, one run. On a young snapshot temporary file
// it also returns that file's modification time.
func (s *Server) checkAndNormalizeTimestamps(ctx context.Context) (autoRunOutcome, time.Time) {
	db, dbDialect := s.storeDB()
	if db == nil {
		return autoRunNothing, time.Time{}
	}
	chk, err := s.timestampCheck(ctx, db, dbDialect)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("timestamp check: could not inspect stored timestamps", "error", err)
		}
		return autoRunNothing, time.Time{}
	}
	if len(chk.NeedsRun) == 0 {
		// Nothing left to rewrite: a key left by an earlier run that did not
		// complete no longer protects anything.
		if _, err := entadapter.ClearUTCTimestampAutoRunKey(ctx, db, dbDialect); err != nil && ctx.Err() == nil {
			slog.Warn("timestamp check: could not clear the incomplete-run key; the hub retries at the next start", "error", err)
		}
		logUnparseableTimestamps(ctx, chk)
		return autoRunNothing, time.Time{}
	}
	return s.autoNormalizeTimestamps(ctx, db, dbDialect, chk)
}

// timestampCheck runs CheckStoredTimestamps under its own bound.
func (s *Server) timestampCheck(ctx context.Context, db *sql.DB, dbDialect string) (entadapter.TimestampCheck, error) {
	cctx, cancel := context.WithTimeout(ctx, startupTimestampCheckTimeout)
	defer cancel()
	return checkStoredTimestampsFn(cctx, db, dbDialect)
}

// logUnparseableTimestamps logs the tables whose only leftovers are values
// the operation cannot parse: a warning, or an error when such a table is
// also unreadable (then no read of it succeeds).
func logUnparseableTimestamps(ctx context.Context, chk entadapter.TimestampCheck) {
	if len(chk.UnparseableOnly) == 0 {
		return
	}
	unreadable := intersectTables(chk.Unreadable, chk.UnparseableOnly)
	level := slog.LevelWarn
	if len(unreadable) > 0 {
		level = slog.LevelError
	}
	slog.Log(ctx, level, msgTimestampsUnparseable, "tables", chk.UnparseableOnly, "tables_unreadable", unreadable)
}

// autoNormalizeTimestamps runs utc-timestamp-normalize once at hub start, on
// a SQLite store whose check found values it rewrites, so that an upgraded
// hub needs no manual step. The run is recorded on the operation's row, as a
// manual run is, so Admin -> Maintenance shows it: running with StartedBy
// system:hub-start and UTCTimestampAutoRunIncompleteKey set before anything
// is written, then completed (key removed) or failed (key kept). If that
// first write fails, or a run is already in flight, nothing runs. Before the
// first rewrite it takes, or reuses, the database snapshot (the boot repair's
// label) and refuses to write without one. The rewrite is idempotent and
// compare-and-set per value, so a failed or stopped run resumes at the next
// start. A stop by shutdown or by the budget is a warning, not an error.
func (s *Server) autoNormalizeTimestamps(ctx context.Context, db *sql.DB, dbDialect string, chk entadapter.TimestampCheck) (out autoRunOutcome, youngTmp time.Time) {
	key := entadapter.UTCTimestampNormalizeKey
	tables := chk.NeedsRun
	op, err := s.store.GetMaintenanceOperation(ctx, key)
	if err != nil {
		slog.Warn("timestamp normalize: could not read the operation record; not started, the hub retries at the next start",
			"tables", tables, "error", err)
		return autoRunSkipped, time.Time{}
	}
	if op.Status == store.MaintenanceStatusRunning {
		slog.Info("timestamp normalize: a run of the operation is in progress; not started at hub start", "tables", tables)
		return autoRunSkipped, time.Time{}
	}
	started := timestampNow()
	op.Status = store.MaintenanceStatusRunning
	op.StartedAt = &started
	op.CompletedAt = nil
	op.StartedBy = timestampAutoRunStartedBy
	op.Metadata = entadapter.WithAutoRunIncomplete(op.Metadata, true)
	if err := s.store.UpdateMaintenanceOperation(ctx, op); err != nil {
		slog.Warn("timestamp normalize: could not record the run; not started, the hub retries at the next start",
			"tables", tables, "error", err)
		return autoRunSkipped, time.Time{}
	}

	var logs bytes.Buffer
	finalize := func(status string, errText string) {
		finished := timestampNow()
		op.Status = status
		op.CompletedAt = &finished
		result := map[string]interface{}{"log": logs.String()}
		if errText != "" {
			result["error"] = errText
		}
		resultJSON, _ := json.Marshal(result)
		op.Result = string(resultJSON)
		if status == store.MaintenanceStatusCompleted {
			op.Metadata = entadapter.WithAutoRunIncomplete(op.Metadata, false)
		}
		if err := s.store.UpdateMaintenanceOperation(context.WithoutCancel(ctx), op); err != nil {
			slog.Warn("timestamp normalize: could not record the outcome of the run", "status", status, "error", err)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			finalize(store.MaintenanceStatusFailed, fmt.Sprintf("panic: %v", r))
			slog.Error("timestamp normalize: recovered from panic; the hub retries at the next start", "panic", fmt.Sprint(r))
			out, youngTmp = autoRunFailed, time.Time{}
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, timestampAutoRunBudget)
	defer cancel()
	stopped := func(stage string, err error) (autoRunOutcome, time.Time) {
		finalize(store.MaintenanceStatusFailed, "stopped ("+stage+"): "+err.Error())
		slog.Warn("timestamp normalize: stopped at hub shutdown or at its time budget; it resumes at the next start",
			"stage", stage, "tables", tables)
		return autoRunStopped, time.Time{}
	}
	failed := func(stage string, err error, attrs ...any) (autoRunOutcome, time.Time) {
		finalize(store.MaintenanceStatusFailed, stage+": "+err.Error())
		slog.Error(msgTimestampsNeedNormalize, append([]any{"stage", stage, "tables", tables,
			"tables_unreadable", intersectTables(chk.Unreadable, tables), "error", err}, attrs...)...)
		return autoRunFailed, time.Time{}
	}

	label := entadapter.UTCTimestampSnapshotLabel
	slog.Info("timestamp normalize: rewriting non-canonical stored timestamps at hub start; taking (or reusing) a database snapshot first",
		"tables", tables)
	removed, tmpMod, err := entadapter.RemoveStaleSnapshotTemp(runCtx, db, label, timestampStaleTmpAge, timestampNow())
	switch {
	case err != nil && runCtx.Err() != nil:
		return stopped("snapshot", err)
	case err != nil:
		return failed("snapshot", err)
	case removed:
		slog.Warn("timestamp normalize: removed the temporary file of an interrupted snapshot", "older_than", timestampStaleTmpAge)
	}
	snap, err := timestampSnapshot(runCtx, db, label, timestampNow())
	switch {
	case errors.Is(err, entadapter.ErrSnapshotInProgress):
		finalize(store.MaintenanceStatusFailed, "snapshot: "+err.Error())
		slog.Warn("timestamp normalize: a snapshot is in progress or was recently interrupted; nothing was written, "+
			"the hub retries later", "tables", tables)
		if tmpMod.IsZero() {
			tmpMod = timestampNow()
		}
		return autoRunYoungTmp, tmpMod
	case err != nil && runCtx.Err() != nil:
		return stopped("snapshot", err)
	case err != nil:
		return failed("snapshot", err)
	case snap.Reused:
		age := "unknown"
		if t, ok := entadapter.SnapshotTimeFromName(snap.Path, label); ok {
			age = timestampNow().Sub(t).Round(time.Second).String()
		}
		slog.Info("timestamp normalize: reusing the database snapshot from an earlier run; no new snapshot written",
			"path", snap.Path, "age", age)
	default:
		slog.Info("timestamp normalize: database snapshot written; it is a full copy of the database, "+
			"store it like the database and delete it once the run is verified", "path", snap.Path)
	}

	rep, err := entadapter.NormalizeUTCTimestamps(runCtx, db, dbDialect, &logs, entadapter.TimestampNormalizeOptions{})
	switch {
	case err != nil && runCtx.Err() != nil:
		return stopped("rewrite", err)
	case err != nil:
		return failed("rewrite", err, "snapshot", snap.Path)
	}

	after, err := s.timestampCheck(ctx, db, dbDialect)
	switch {
	case err != nil && ctx.Err() != nil:
		return stopped("re-check", err)
	case err != nil:
		return failed("re-check", err, "snapshot", snap.Path)
	}
	finalize(store.MaintenanceStatusCompleted, "")
	if len(after.NeedsRun) > 0 {
		slog.Error(msgTimestampsStillNonCanonical, "tables", after.NeedsRun,
			"tables_unreadable", intersectTables(after.Unreadable, after.NeedsRun))
	} else {
		slog.Info("timestamp normalize: stored timestamps normalized at hub start", "tables", tables,
			"rewritten", rep.Rewritten, "unparseable", rep.Unparseable, "snapshot", snap.Path)
	}
	logUnparseableTimestamps(ctx, after)
	return autoRunDone, time.Time{}
}

// intersectTables returns the names in a that are also in b, in a's order.
func intersectTables(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}
