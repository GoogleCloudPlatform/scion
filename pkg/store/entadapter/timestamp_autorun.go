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

package entadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// UTCTimestampSnapshotLabel names the snapshot written before stored
// timestamps are rewritten, by the boot-time repair of unreadable tables and
// by the hub's run of utc-timestamp-normalize at start:
// <db file>.<label>-<UTC time>.bak (see SnapshotSQLite).
const UTCTimestampSnapshotLabel = "pre-" + UTCTimestampNormalizeKey

// UTCTimestampAutoRunIncompleteKey is the key, in the Metadata JSON object of
// the utc-timestamp-normalize maintenance operation, that the hub's run at
// start sets to true before it writes anything and removes once a run
// completes. Manual runs and dry runs never set it.
const UTCTimestampAutoRunIncompleteKey = "autoRunIncomplete"

// UTCTimestampNormalizeIncomplete reports whether a run of
// utc-timestamp-normalize started by the hub at start has begun and not yet
// completed: the operation row exists, its status is not completed, and its
// Metadata holds UTCTimestampAutoRunIncompleteKey set to true. While it is
// true the snapshot taken before that run is the only copy of the values it
// rewrites, so it must be kept. A missing row is false with no error; any
// other read error is returned and callers should treat it as incomplete.
func UTCTimestampNormalizeIncomplete(ctx context.Context, s store.Store) (bool, error) {
	op, err := s.GetMaintenanceOperation(ctx, UTCTimestampNormalizeKey)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return op.Status != store.MaintenanceStatusCompleted && AutoRunIncompleteSet(op.Metadata), nil
}

// AutoRunIncompleteSet reports whether metadata is a JSON object holding
// UTCTimestampAutoRunIncompleteKey set to boolean true. Empty text, "{}",
// text that is not a JSON object and any other value of the key are false.
func AutoRunIncompleteSet(metadata string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(metadata), &obj) != nil {
		return false
	}
	var v bool
	raw, ok := obj[UTCTimestampAutoRunIncompleteKey]
	return ok && json.Unmarshal(raw, &v) == nil && v
}

// WithAutoRunIncomplete returns metadata with UTCTimestampAutoRunIncompleteKey
// set to true (on) or removed (!on). Other keys of a JSON object are kept;
// text that is not a JSON object is replaced.
func WithAutoRunIncomplete(metadata string, on bool) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(metadata), &obj) != nil || obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if on {
		obj[UTCTimestampAutoRunIncompleteKey] = json.RawMessage("true")
	} else {
		delete(obj, UTCTimestampAutoRunIncompleteKey)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		// Not reachable: every value is already valid JSON.
		return metadata
	}
	return string(out)
}

// ClearUTCTimestampAutoRunKey removes UTCTimestampAutoRunIncompleteKey from
// the utc-timestamp-normalize operation's Metadata when no run is in flight.
// It writes Metadata only, as one compare-and-set against the text it read,
// so it never changes the row's status, result or times, and it does nothing
// while the row is running or after a concurrent change to Metadata. It
// reports whether it removed the key. Only SQLite stores are written: the key
// is never set on other dialects.
func ClearUTCTimestampAutoRunKey(ctx context.Context, db *sql.DB, dbDialect string) (bool, error) {
	sqlite, err := isSQLiteDialect(db, dbDialect)
	if err != nil || !sqlite {
		return false, err
	}
	var status, metadata string
	err = db.QueryRowContext(ctx,
		`SELECT status, metadata FROM maintenance_operations WHERE "key" = ?`,
		UTCTimestampNormalizeKey).Scan(&status, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status == store.MaintenanceStatusRunning || !AutoRunIncompleteSet(metadata) {
		return false, nil
	}
	return clearAutoRunKeyCAS(ctx, db, metadata)
}

// clearAutoRunKeyCAS writes metadata without UTCTimestampAutoRunIncompleteKey
// to the utc-timestamp-normalize row, only if the row is not running and its
// Metadata is still exactly old (the text read before). The SQL guards, not
// the caller's read, make it safe against a manual run that starts, or a
// write that lands, between that read and this write. It reports whether a
// row changed.
func clearAutoRunKeyCAS(ctx context.Context, db *sql.DB, old string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE maintenance_operations SET metadata = ? WHERE "key" = ? AND status <> ? AND metadata = ?`,
		WithAutoRunIncomplete(old, false), UTCTimestampNormalizeKey, store.MaintenanceStatusRunning, old)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, err
}

// RemoveStaleSnapshotTemp removes the temporary file of an interrupted
// SnapshotSQLite call with this label ("<file>.<label>.tmp") when its
// modification time is olderThan or more before now. It returns whether it
// removed the file and, when the file exists, its modification time.
//
// olderThan must be at least the time budget of every writer of this label's
// snapshot: a writer whose context has expired has stopped writing and
// removes its own temporary file, so a file older than every budget belongs
// to a process that was killed. Concurrent processes on one SQLite file are
// not supported.
func RemoveStaleSnapshotTemp(ctx context.Context, db *sql.DB, label string, olderThan time.Duration, now time.Time) (bool, time.Time, error) {
	var file string
	if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil {
		return false, time.Time{}, fmt.Errorf("locate database file: %w", err)
	}
	if file == "" {
		return false, time.Time{}, nil
	}
	tmp := file + "." + label + ".tmp"
	fi, err := os.Lstat(tmp)
	if errors.Is(err, os.ErrNotExist) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	mod := fi.ModTime()
	if !fi.Mode().IsRegular() || now.Sub(mod) < olderThan {
		return false, mod, nil
	}
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, mod, err
	}
	return true, mod, nil
}

// SnapshotTimeFromName returns the UTC time in a snapshot file name written
// by SnapshotSQLite for label ("<file>.<label>-<20060102T150405Z>.bak"), and
// false when the name does not hold one (for example a copy named by hand).
func SnapshotTimeFromName(path, label string) (time.Time, bool) {
	const layout = "20060102T150405Z"
	const suffix = ".bak"
	marker := "." + label + "-"
	if len(path) < len(marker)+len(layout)+len(suffix) {
		return time.Time{}, false
	}
	end := len(path) - len(suffix)
	if path[end:] != suffix {
		return time.Time{}, false
	}
	start := end - len(layout)
	if path[start-len(marker):start] != marker {
		return time.Time{}, false
	}
	t, err := time.Parse(layout, path[start:end])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
