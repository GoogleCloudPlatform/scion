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
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// fkReconcileSnapshotLabelPrefix starts the label of the snapshot written
// before the foreign key reconcile: <db file>.<prefix>-<fingerprint>-<UTC time>.bak.
const fkReconcileSnapshotLabelPrefix = "pre-fk-reconcile"

// fkReconcileSnapshotLabel names the snapshot after a fingerprint of the
// repairs, including the orphaned rows' full content
// (entc.FKRepairGroup.Orphans). SnapshotSQLite reuses an existing snapshot
// with the same label. A snapshot is therefore reused only when the
// orphaned rows the reconcile starts from are byte-identical to the ones it
// was taken for: a boot that retries the same reconcile (after it failed,
// or the hub stopped before the migration) reuses it instead of writing
// another copy, and that copy holds exactly those rows, also when an
// earlier reconcile completed and its snapshot is still on disk. Any
// difference in the orphaned rows, even in a column outside the key, gives
// a new snapshot. Rows that a CASCADE repair reaches further down are not
// compared.
func fkReconcileSnapshotLabel(groups []entc.FKRepairGroup) string {
	h := sha256.New()
	for _, g := range groups {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\x00%d\x00%s\n", g.Table, g.ForeignKeyID, strings.Join(g.Columns, ","), g.Rows, g.Orphans)
	}
	return fkReconcileSnapshotLabelPrefix + "-" + hex.EncodeToString(h.Sum(nil)[:4])
}

// snapshotBeforeFKReconcile is the entc.BeforeFKReconcileFunc that hub
// startup (initStore) and the maintenance commands that migrate the hub
// database (migrateHubStore, migrateHubSchema) pass to the schema
// migration. entc.AutoMigrate calls it on SQLite, after PreMigrate and just
// before the reconcile applies the declared ON DELETE action to orphaned
// rows, with the keys whose rows the reconcile is about to change; it is
// not called when there are none (a healthy database, or only orphans the
// reconcile leaves in place). It snapshots the database so an operator can
// recover the rows the reconcile deletes or clears.
//
// The snapshot is written next to the database file by the same helper as
// the UTC timestamp repair (VACUUM INTO, mode 0600, about the database
// size). If both repairs run on one boot, each writes its own copy.
//
// A snapshot failure is returned, which aborts the migration (and so
// startup) before the reconcile changes any rows: the reconcile changes
// rows permanently, so it does not run without a copy to recover from. An
// in-memory database has no file to copy and is skipped.
func snapshotBeforeFKReconcile(ctx context.Context, db *sql.DB, groups []entc.FKRepairGroup) error {
	const label = "foreign key reconcile"
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
		return fmt.Errorf("%s: refusing to continue without a database snapshot: %w", label, err)
	case err != nil:
		return fmt.Errorf("%s: refusing to continue: the database snapshot taken before repairing orphaned rows failed, "+
			"and the repair changes rows permanently. %s, then retry: %w", label, snapshotSpaceHint(file), err)
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
