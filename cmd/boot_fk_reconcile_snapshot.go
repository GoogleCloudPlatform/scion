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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// fkReconcileSnapshotLabelPrefix starts the label of the snapshot written
// before the foreign key reconcile: <db file>.<prefix>-<fingerprint>-<UTC time>.bak.
const fkReconcileSnapshotLabelPrefix = "pre-fk-reconcile"

// pendingFKRepairs is entc.PendingSQLiteFKRepairs; a variable so tests can
// make the pre-check fail.
var pendingFKRepairs = entc.PendingSQLiteFKRepairs

// fkReconcileSnapshotLabel names the snapshot after a fingerprint of the
// repairs. SnapshotSQLite reuses an existing snapshot with the same label,
// so a boot that retries the same reconcile (after it failed, or the hub
// stopped before the migration) reuses the snapshot instead of writing
// another copy, while a later, different set of orphans gets its own
// snapshot instead of an older one.
func fkReconcileSnapshotLabel(groups []entc.FKRepairGroup) string {
	h := sha256.New()
	for _, g := range groups {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00%d\n", g.Table, g.ForeignKeyID, strings.Join(g.Columns, ","), g.Rows)
	}
	return fkReconcileSnapshotLabelPrefix + "-" + hex.EncodeToString(h.Sum(nil)[:4])
}

// snapshotBeforeFKReconcile snapshots a SQLite database before the schema
// migration applies the declared ON DELETE action to orphaned rows (see
// entc.AutoMigrate), so an operator can recover the rows the reconcile
// deletes or clears. It runs in initStore just before migrateStore.
//
// The read-only pre-check, entc.PendingSQLiteFKRepairs, lists the keys the
// reconcile will change; when there are none (a healthy database, or only
// orphans the reconcile leaves in place) nothing is written or logged. The
// snapshot is written next to the database file by the same helper as the
// UTC timestamp repair (VACUUM INTO, mode 0600, about the database size). If
// both repairs run on one boot, each writes its own copy.
//
// A snapshot failure is returned, and fails startup: the reconcile changes
// rows permanently, so it does not run without a copy to recover from. A
// pre-check failure is logged and boot continues; the migration then runs
// the reconcile as it would without this step. An in-memory database has
// no file to copy and is skipped. Postgres is skipped.
func snapshotBeforeFKReconcile(ctx context.Context, s timestampRepairDB) error {
	const label = "foreign key reconcile"
	db, dbDialect := s.DB(), s.Dialect()
	if db == nil || dbDialect != dialect.SQLite {
		return nil
	}
	groups, err := pendingFKRepairs(ctx, db)
	if err != nil {
		slog.Warn(label+": pre-check failed; no snapshot was taken before the schema migration", "error", err)
		return nil
	}
	if len(groups) == 0 {
		return nil
	}
	for _, g := range groups {
		slog.Warn(label+": orphaned rows will get their key's declared ON DELETE action before the schema migration",
			"table", g.Table, "column", strings.Join(g.Columns, ","), "parent", g.Parent, "action", g.Action, "rows", g.Rows)
	}

	var file string
	if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil {
		return fmt.Errorf("%s: locating the database file for the snapshot: %w", label, err)
	}
	if file == "" {
		slog.Info(label + ": in-memory database; no snapshot taken")
		return nil
	}

	snap, err := snapshotSQLite(ctx, db, fkReconcileSnapshotLabel(groups), time.Now())
	switch {
	case errors.Is(err, entadapter.ErrSnapshotInProgress):
		return fmt.Errorf("%s: refusing to start without a database snapshot: %w", label, err)
	case err != nil:
		return fmt.Errorf("%s: refusing to start: the database snapshot taken before repairing orphaned rows failed, "+
			"and the repair changes rows permanently. %s, then restart the hub: %w", label, snapshotSpaceHint(file), err)
	case snap.Reused:
		slog.Warn(label+": reusing the database snapshot from an earlier attempt; no new snapshot written", "path", snap.Path)
	default:
		slog.Warn(label+": database snapshot written before repairing orphaned rows; it is a full copy of the database, "+
			"delete it once the repair is verified", "path", snap.Path)
	}
	return nil
}

// snapshotSpaceHint says how much space the snapshot needs and where, and
// how much is free there when that can be read.
func snapshotSpaceHint(file string) string {
	dir := filepath.Dir(file)
	hint := "Make sure the directory " + dir + " is writable and has free space for a copy of the database"
	if st, err := os.Stat(file); err == nil {
		hint += fmt.Sprintf(" (about %d bytes, the size of %s", st.Size(), filepath.Base(file))
		if free, ok := freeDiskBytes(dir); ok {
			hint += fmt.Sprintf("; %d bytes are available", free)
		}
		hint += ")"
	}
	return hint
}
