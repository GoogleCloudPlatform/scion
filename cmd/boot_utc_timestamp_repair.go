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

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// utcTimestampRepairBudget bounds the boot-time repair of unreadable
// timestamps (probe, snapshot and rewrite) as a runaway guard. A run that
// exceeds it stops between batches; the rewrite is resumable, so the next
// boot continues where it stopped. Exported as a variable so tests can
// override it.
var utcTimestampRepairBudget = 30 * time.Minute

// utcTimestampSnapshotLabel names the snapshot file written before the
// boot-time repair: <db file>.<label>-<UTC time>.bak.
const utcTimestampSnapshotLabel = "pre-" + entadapter.UTCTimestampNormalizeKey

// snapshotSQLite takes the pre-repair snapshot. A variable so tests can make
// it fail.
var snapshotSQLite = entadapter.SnapshotSQLite

// timestampRepairDB is the part of the store the boot-time repair uses: the
// raw SQL handle and its ent dialect.
type timestampRepairDB interface {
	DB() *sql.DB
	Dialect() string
}

// utcTimestampRepairResult is the outcome of repairUnreadableTimestamps.
type utcTimestampRepairResult struct {
	// Completed is true when unreadable tables were found and are now
	// readable. The caller then writes the completion marker.
	Completed bool
	// Unparseable is the number of values the normalizer left unchanged.
	Unparseable int
}

// repairUnreadableTimestamps rewrites, at hub start, the SQLite tables that
// hold a time value the driver cannot scan (a four-digit numeric zone
// abbreviation such as "+0545 +0545"), so that the ent reads that follow
// succeed. It runs the same normalizer as the utc-timestamp-normalize
// maintenance operation, limited to those tables.
//
// It MUST run before migrateStore, not in runBootDataMigrations: Store.Migrate
// reads several tables through ent (agents, hub settings, user access tokens
// and others), any such read fails on these values, and a Migrate error is
// fatal. A repair placed after Migrate would never run on the hubs that need
// it. For the same reason it uses raw SQL only, and it tolerates an older
// schema (ent's schema migration has not run yet).
//
// The probe runs on every boot and decides whether to repair; the marker
// only records that a repair completed. On a healthy store the probe is one
// SELECT EXISTS per ent time column. Before writing it snapshots the
// database next to its file with VACUUM INTO and refuses to write if the
// snapshot fails. It never returns an error and recovers from a panic: on
// failure it logs at ERROR and boot continues, failing at Migrate as it did
// before this repair existed. Postgres stores timestamptz and is skipped.
func repairUnreadableTimestamps(ctx context.Context, s timestampRepairDB) (res utcTimestampRepairResult) {
	const label = "UTC timestamp repair"
	defer func() {
		if r := recover(); r != nil {
			slog.Error(label+": recovered from panic; repair did not complete, will retry next boot", "panic", r)
			res = utcTimestampRepairResult{}
		}
	}()
	db, dbDialect := s.DB(), s.Dialect()
	if db == nil || dbDialect != dialect.SQLite {
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, utcTimestampRepairBudget)
	defer cancel()

	tables, err := entadapter.UnreadableTimestampTables(ctx, db, dbDialect)
	if err != nil {
		slog.Error(label+": probe failed; nothing was written", "error", err)
		return res
	}
	if len(tables) == 0 {
		return res
	}
	slog.Warn(label+": tables hold timestamps that cannot be read; repairing them before the store is used",
		"tables_unreadable", tables)

	path, err := snapshotSQLite(ctx, db, utcTimestampSnapshotLabel, time.Now())
	if err != nil {
		slog.Error(label+": database snapshot failed; refusing to write. Free disk space next to the database "+
			"(the snapshot needs about the database size), then restart the hub",
			"tables_unreadable", tables, "error", err)
		return res
	}
	slog.Warn(label+": database snapshot written before the repair", "path", path)

	logs := &boundedRepairLog{}
	rep, err := entadapter.NormalizeUTCTimestamps(ctx, db, dbDialect, logs,
		entadapter.TimestampNormalizeOptions{Tables: tables})
	logs.flush(label)
	if err != nil {
		slog.Error(label+": rewrite did not complete; will retry next boot", "snapshot", path, "error", err)
		return res
	}
	left, err := entadapter.UnreadableTimestampTables(ctx, db, dbDialect)
	if err != nil {
		slog.Error(label+": re-probe after the rewrite failed; will retry next boot", "error", err)
		return res
	}
	if len(left) > 0 {
		slog.Error(label+": tables are still unreadable after the rewrite; will retry next boot",
			"tables_unreadable", left, "snapshot", path)
		return res
	}
	slog.Info(label+": repair complete", "rewritten", rep.Rewritten, "unparseable", rep.Unparseable, "snapshot", path)
	return utcTimestampRepairResult{Completed: true, Unparseable: rep.Unparseable}
}

// markUTCTimestampRepairComplete writes the completion marker after a repair
// that completed. It runs after migrateStore, when hub settings can be read
// through the store.
func markUTCTimestampRepairComplete(ctx context.Context, s store.Store, res utcTimestampRepairResult) {
	if !res.Completed {
		return
	}
	if err := MarkMigrationComplete(ctx, s, MigrationUTCTimestampRepair, res.Unparseable); err != nil {
		slog.Error("UTC timestamp repair: failed to write completion marker", "error", err)
	}
}

// boundedRepairLog collects the normalizer's run log and logs it with
// per-value lines capped at maxBootLogErrors, so a store with many
// unparseable values cannot flood the boot log. The normalizer never writes
// a stored value to its log, only table, column and rowid.
type boundedRepairLog struct {
	buf bytes.Buffer
}

func (b *boundedRepairLog) Write(p []byte) (int, error) { return b.buf.Write(p) }

func (b *boundedRepairLog) flush(label string) {
	sc := bufio.NewScanner(&b.buf)
	perValue, suppressed := 0, 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "unparseable value") {
			perValue++
			if perValue > maxBootLogErrors {
				suppressed++
				continue
			}
			slog.Warn(label + ": " + line)
			continue
		}
		slog.Info(label + ": " + line)
	}
	if suppressed > 0 {
		slog.Warn(label+": further unparseable values not logged; the "+entadapter.UTCTimestampNormalizeKey+
			" maintenance operation's run log lists them all", "count", suppressed)
	}
	b.buf.Reset()
}
