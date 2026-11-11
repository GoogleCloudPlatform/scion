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
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SnapshotRetention is how long a snapshot written by SnapshotSQLite is
// kept after its last use before PruneSQLiteSnapshots removes it. The last
// use is the file's modification time: SnapshotSQLite sets it when it
// writes or reuses a snapshot, and TouchSQLiteSnapshots sets it on demand.
const SnapshotRetention = 7 * 24 * time.Hour

// removeSnapshotFile and chtimesSnapshotFile are variables so tests can
// make them fail.
var (
	removeSnapshotFile  = os.Remove
	chtimesSnapshotFile = os.Chtimes
)

// snapshotNameRegexp matches the names SnapshotSQLite gives the snapshots
// of the database file base: "<base>.<label>-<UTC time>.bak", where label
// matches one of labelPatterns. Each label pattern is a regular expression
// fragment; a fixed label must be passed through regexp.QuoteMeta.
func snapshotNameRegexp(base string, labelPatterns []string) (*regexp.Regexp, error) {
	return regexp.Compile(`^` + regexp.QuoteMeta(base) + `\.(?:` + strings.Join(labelPatterns, "|") +
		`)-[0-9]{8}T[0-9]{6}Z\.bak$`)
}

// matchingSnapshots returns the regular files next to file whose names
// match snapshotNameRegexp. Symbolic links, directories and other entries
// are skipped and never followed. An unreadable directory or an invalid
// pattern is logged at WARN and gives no files.
func matchingSnapshots(file string, labelPatterns []string) []os.DirEntry {
	if file == "" || len(labelPatterns) == 0 {
		return nil
	}
	dir, base := filepath.Split(file)
	if dir == "" {
		dir = "."
	}
	re, err := snapshotNameRegexp(base, labelPatterns)
	if err != nil {
		slog.Warn("database snapshot retention: invalid snapshot label pattern; nothing checked", "error", err)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("database snapshot retention: cannot list the database directory; nothing checked",
			"dir", dir, "error", err)
		return nil
	}
	var out []os.DirEntry
	for _, e := range entries {
		if e.Type().IsRegular() && re.MatchString(e.Name()) {
			out = append(out, e)
		}
	}
	return out
}

// PruneSQLiteSnapshots removes the snapshots of the SQLite database file
// (see SnapshotSQLite) whose label matches one of labelPatterns and whose
// last use (modification time) is more than maxAge before now. It never
// removes a snapshot named in keep (compared by base name, all snapshots
// being next to file), one with a modification time in the future, a file
// whose name does not match exactly, or anything that is not a regular
// file. Errors are logged at WARN and the next file is checked; it returns
// the removed paths.
//
// A maintenance command that migrates the same database while a hub
// starts on it is not supported: it could reuse a snapshot between the
// check here and its removal.
func PruneSQLiteSnapshots(file string, labelPatterns, keep []string, maxAge time.Duration, now time.Time) []string {
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		if k != "" {
			kept[filepath.Base(k)] = true
		}
	}
	dir := filepath.Dir(file)
	var removed []string
	for _, e := range matchingSnapshots(file, labelPatterns) {
		path := filepath.Join(dir, e.Name())
		if kept[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("database snapshot retention: cannot read the snapshot; left in place", "path", path, "error", err)
			continue
		}
		age := now.Sub(info.ModTime())
		if age <= maxAge {
			continue
		}
		if err := removeSnapshotFile(path); err != nil {
			slog.Warn("database snapshot retention: cannot remove the snapshot; left in place", "path", path, "error", err)
			continue
		}
		slog.Info("database snapshot retention: removed a database snapshot unused for more than the retention period",
			"path", path, "age", age.Round(time.Second).String(), "bytes", info.Size(), "retention", maxAge.String())
		removed = append(removed, path)
	}
	return removed
}

// TouchSQLiteSnapshots sets the modification time of the snapshots of the
// SQLite database file whose label matches one of labelPatterns to now,
// so that PruneSQLiteSnapshots counts their retention from now. Errors are
// logged at WARN. It returns the paths it updated.
func TouchSQLiteSnapshots(file string, labelPatterns []string, now time.Time) []string {
	dir := filepath.Dir(file)
	var touched []string
	for _, e := range matchingSnapshots(file, labelPatterns) {
		path := filepath.Join(dir, e.Name())
		if err := chtimesSnapshotFile(path, now, now); err != nil {
			slog.Warn("database snapshot retention: cannot update the snapshot's last use; "+
				"it may be removed earlier than the retention period", "path", path, "error", err)
			continue
		}
		touched = append(touched, path)
	}
	return touched
}
