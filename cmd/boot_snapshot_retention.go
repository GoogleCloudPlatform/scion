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
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// fkSnapshotLabelPattern matches the labels fkReconcileSnapshotLabel
// produces: the prefix and a fingerprint of 4 bytes in lowercase hex.
func fkSnapshotLabelPattern() string {
	return regexp.QuoteMeta(fkReconcileSnapshotLabelPrefix) + "-[0-9a-f]{8}"
}

// utcSnapshotLabelPattern matches utcTimestampSnapshotLabel.
func utcSnapshotLabelPattern() string {
	return regexp.QuoteMeta(utcTimestampSnapshotLabel)
}

// startupSnapshotPatterns are the label patterns of every snapshot kind a
// hub start can write. A new snapshot label written at start-up must be
// added here so its copies are not kept forever.
func startupSnapshotPatterns() []string {
	return []string{fkSnapshotLabelPattern(), utcSnapshotLabelPattern()}
}

// utcTimestampAutoRunIncompleteKey is the key in the utc-timestamp-normalize
// maintenance operation's metadata that marks an automatic run that started
// and has not completed.
//
// replaced by entadapter.UTCTimestampAutoRunIncompleteKey (ptone/scion#5194)
const utcTimestampAutoRunIncompleteKey = "autoRunIncomplete"

// utcTimestampNormalizeIncomplete reports whether an automatic run of the
// timestamp normalizer started and has not completed, so its snapshot may
// be the only copy of the values it rewrites: the utc-timestamp-normalize
// maintenance operation exists, is not completed, and its metadata is a
// JSON object whose autoRunIncomplete key is true. A missing operation is
// complete; any other read error is returned.
//
// replaced by entadapter.UTCTimestampNormalizeIncomplete (ptone/scion#5194)
func utcTimestampNormalizeIncomplete(ctx context.Context, s store.Store) (bool, error) {
	op, err := s.GetMaintenanceOperation(ctx, entadapter.UTCTimestampNormalizeKey)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if op == nil || op.Status == store.MaintenanceStatusCompleted {
		return false, nil
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(op.Metadata), &meta); err != nil {
		return false, nil
	}
	incomplete, ok := meta[utcTimestampAutoRunIncompleteKey].(bool)
	return ok && incomplete, nil
}

// timestampNormalizeIncomplete and pruneStartupSnapshots are variables so
// tests can observe them.
var (
	timestampNormalizeIncomplete = utcTimestampNormalizeIncomplete
	pruneStartupSnapshots        = func(file string, patterns, keep []string, now time.Time) []string {
		return entadapter.PruneSQLiteSnapshots(file, patterns, keep, entadapter.SnapshotRetention, now)
	}
)

// startupSnapshots collects the snapshots a hub start writes or reuses.
type startupSnapshots struct {
	fkPaths  []string
	tsRepair utcTimestampRepairResult
}

// recordFK is the record function passed to snapshotBeforeFKReconcileRecord.
func (in *startupSnapshots) recordFK(path string) {
	in.fkPaths = append(in.fkPaths, path)
}

// pruneInputs returns the label patterns to prune and the snapshots to
// keep. It is called only after a successful migration: it reads the
// timestamp normalizer's state through the store. The timestamp-repair
// snapshots are left out when this boot's repair could not tell whether
// its snapshot is still needed, or an automatic normalize is incomplete
// (or its state cannot be read).
func (in *startupSnapshots) pruneInputs(ctx context.Context, s store.Store) (patterns, keep []string) {
	keep = append(keep, in.fkPaths...)
	if in.tsRepair.SnapshotPath != "" {
		keep = append(keep, in.tsRepair.SnapshotPath)
	}
	patterns = []string{fkSnapshotLabelPattern()}
	if in.tsRepair.ProbeFailed {
		return patterns, keep
	}
	incomplete, err := timestampNormalizeIncomplete(ctx, s)
	switch {
	case err != nil:
		slog.Warn("database snapshot retention: cannot read the timestamp normalizer's state; "+
			"timestamp repair snapshots left in place", "error", err)
	case !incomplete:
		patterns = append(patterns, utcSnapshotLabelPattern())
	}
	return patterns, keep
}

// startupStore is the part of the store migrateThenPrune uses.
type startupStore interface {
	store.Store
	timestampRepairDB
}

// sqliteDatabaseFile returns the file of a SQLite store, or "" for another
// dialect, an in-memory database, or when it cannot be read.
func sqliteDatabaseFile(ctx context.Context, s timestampRepairDB) string {
	db := s.DB()
	if db == nil || s.Dialect() != dialect.SQLite {
		return ""
	}
	var file string
	if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil {
		slog.Warn("database snapshot retention: cannot locate the database file; snapshots not checked", "error", err)
		return ""
	}
	return file
}

// migrateThenPrune runs the start-up migration, then handles the snapshots
// that the start-up repairs write next to a SQLite database file.
//
// If migrate fails, the error is returned unchanged and every snapshot of
// every kind gets its last use set to now, so its retention counts from
// the last failed start and an outage longer than the retention period
// does not remove it at the first good start. No filter applies there:
// this only keeps files.
//
// If migrate succeeds, the timestamp repair's completion is recorded and
// the snapshots unused for longer than entadapter.SnapshotRetention are
// removed, except the ones this start wrote or reused (see pruneInputs).
// A failed or interrupted repair never reaches this point, so its copy is
// kept.
func migrateThenPrune(ctx context.Context, s startupStore, migrate func(context.Context) error, in *startupSnapshots) error {
	if err := migrate(ctx); err != nil {
		if file := sqliteDatabaseFile(ctx, s); file != "" {
			entadapter.TouchSQLiteSnapshots(file, startupSnapshotPatterns(), time.Now())
		}
		return err
	}
	markUTCTimestampRepairComplete(ctx, s, in.tsRepair)
	if file := sqliteDatabaseFile(ctx, s); file != "" {
		patterns, keep := in.pruneInputs(ctx, s)
		pruneStartupSnapshots(file, patterns, keep, time.Now())
	}
	return nil
}
