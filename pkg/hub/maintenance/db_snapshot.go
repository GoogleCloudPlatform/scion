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

package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SnapshotsDirName is the directory under the install root that holds the
// database snapshots taken before updates (design D1/D4).
const SnapshotsDirName = "snapshots"

// DefaultSnapshotKeep is how many database snapshots (and settings copies)
// are kept; older ones are removed after each new snapshot.
const DefaultSnapshotKeep = 5

// snapshotTimeFormat is the UTC timestamp in snapshot file names. It sorts
// chronologically as text.
const snapshotTimeFormat = "20060102T150405Z"

// Snapshot file name parts: hub-<fromVersion>-<ts>.db and
// settings-<ts>.yaml.
const (
	dbSnapshotPrefix       = "hub-"
	dbSnapshotSuffix       = ".db"
	settingsSnapshotPrefix = "settings-"
	settingsSnapshotSuffix = ".yaml"
	snapshotPartialSuffix  = ".partial"
)

// DBSnapshotStatus says whether the hub can snapshot its database before an
// update, and where the snapshots go.
type DBSnapshotStatus struct {
	// Supported is false when no snapshot can be taken (a PostgreSQL or
	// in-memory database). Downgrades are then refused
	// (CodeDowngradeUnsupportedDriver); upgrades go ahead without one.
	Supported bool
	// Dir is the snapshot directory ("" when unsupported).
	Dir string
	// Reason says why snapshots are unsupported.
	Reason string
}

// DBSnapshotter takes the database snapshot the update executor writes
// before every update (design D4).
type DBSnapshotter interface {
	SnapshotStatus() DBSnapshotStatus
	// Snapshot writes a consistent copy of the database (and of the
	// settings file, when there is one) and returns the database copy's
	// path. fromVersion is the running version ("" for a git build). An
	// error means no usable snapshot was written.
	Snapshot(ctx context.Context, fromVersion string, logger io.Writer) (string, error)
}

// unsupportedDBSnapshotter is a DBSnapshotter for databases that cannot be
// snapshotted.
type unsupportedDBSnapshotter struct{ reason string }

// NewUnsupportedDBSnapshotter returns a DBSnapshotter that takes no
// snapshots, for the given reason.
func NewUnsupportedDBSnapshotter(reason string) DBSnapshotter {
	return unsupportedDBSnapshotter{reason: reason}
}

func (u unsupportedDBSnapshotter) SnapshotStatus() DBSnapshotStatus {
	return DBSnapshotStatus{Reason: u.reason}
}

func (u unsupportedDBSnapshotter) Snapshot(context.Context, string, io.Writer) (string, error) {
	return "", fmt.Errorf("database snapshots are not supported: %s", u.reason)
}

// SQLiteSnapshotOptions configures a SQLiteSnapshotter.
type SQLiteSnapshotOptions struct {
	// DB is the hub's SQLite database.
	DB *sql.DB
	// Dir is the snapshot directory; it is created (0700) when missing.
	Dir string
	// SettingsPath is the settings.yaml copied next to each snapshot. ""
	// or a missing file copies nothing.
	SettingsPath string
	// Keep is how many snapshots to keep; default DefaultSnapshotKeep.
	Keep int
	// FreeSpace returns the bytes available in a directory; default the
	// file system's figure for an unprivileged user. Tests inject it.
	FreeSpace func(dir string) (uint64, error)
	// Now is the clock for snapshot names; default time.Now.
	Now func() time.Time
}

// SQLiteSnapshotter snapshots a SQLite database with VACUUM INTO, which is
// consistent while the hub keeps running and takes no file locks.
type SQLiteSnapshotter struct {
	db        *sql.DB
	dir       string
	settings  string
	keep      int
	freeSpace func(string) (uint64, error)
	now       func() time.Time
}

// NewSQLiteSnapshotter returns a SQLiteSnapshotter configured from opts.
func NewSQLiteSnapshotter(opts SQLiteSnapshotOptions) *SQLiteSnapshotter {
	s := &SQLiteSnapshotter{
		db:        opts.DB,
		dir:       opts.Dir,
		settings:  opts.SettingsPath,
		keep:      opts.Keep,
		freeSpace: opts.FreeSpace,
		now:       opts.Now,
	}
	if s.keep <= 0 {
		s.keep = DefaultSnapshotKeep
	}
	if s.freeSpace == nil {
		s.freeSpace = freeDiskSpace
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// SnapshotStatus implements DBSnapshotter.
func (s *SQLiteSnapshotter) SnapshotStatus() DBSnapshotStatus {
	return DBSnapshotStatus{Supported: true, Dir: s.dir}
}

// Snapshot implements DBSnapshotter. It refuses when the free space in the
// snapshot directory is less than twice the database size, writes
// hub-<fromVersion>-<ts>.db through a ".partial" file renamed when
// complete (so a snapshot is never partial), copies the settings file to
// settings-<ts>.yaml the same way, then keeps only the newest Keep of each.
// Files are created 0600: the database and settings hold secrets.
func (s *SQLiteSnapshotter) Snapshot(ctx context.Context, fromVersion string, logger io.Writer) (string, error) {
	if s.db == nil || s.dir == "" {
		return "", errors.New("database snapshot is not configured")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot directory %s: %w", s.dir, err)
	}
	size, err := sqliteDatabaseSize(ctx, s.db)
	if err != nil {
		return "", err
	}
	free, err := s.freeSpace(s.dir)
	if err != nil {
		return "", fmt.Errorf("read the free disk space in %s: %w", s.dir, err)
	}
	if need := 2 * size; free < need {
		return "", fmt.Errorf("not enough free disk space for the database snapshot: %d bytes available in %s, "+
			"at least %d needed (twice the database size of %d bytes); free some space (older snapshots in %s can be removed) and retry",
			free, s.dir, need, size, s.dir)
	}

	ts := s.now().UTC().Format(snapshotTimeFormat)
	dbPath := filepath.Join(s.dir, dbSnapshotPrefix+snapshotVersionLabel(fromVersion)+"-"+ts+dbSnapshotSuffix)
	start := time.Now()
	if err := writeAtomically(dbPath, func(tmp string) error {
		_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp)
		return err
	}); err != nil {
		return "", fmt.Errorf("database snapshot %s: %w", dbPath, err)
	}
	_, _ = fmt.Fprintf(logger, "Database snapshot written: %s (%d bytes in the database, %s)\n",
		dbPath, size, time.Since(start).Round(time.Millisecond))

	if s.settings != "" {
		settingsPath := filepath.Join(s.dir, settingsSnapshotPrefix+ts+settingsSnapshotSuffix)
		copied, err := copySettingsFile(s.settings, settingsPath)
		switch {
		case err != nil:
			return "", fmt.Errorf("copy the settings file next to the database snapshot: %w", err)
		case copied:
			_, _ = fmt.Fprintf(logger, "Settings copied: %s\n", settingsPath)
		default:
			_, _ = fmt.Fprintf(logger, "No settings file at %s; nothing copied.\n", s.settings)
		}
	}

	for _, kind := range [][2]string{{dbSnapshotPrefix, dbSnapshotSuffix}, {settingsSnapshotPrefix, settingsSnapshotSuffix}} {
		removed, err := pruneSnapshots(s.dir, kind[0], kind[1], s.keep)
		for _, name := range removed {
			_, _ = fmt.Fprintf(logger, "Removed old snapshot %s\n", name)
		}
		if err != nil {
			// The new snapshot is in place; an old one left behind only
			// costs disk space.
			_, _ = fmt.Fprintf(logger, "Warning: removing old snapshots failed: %v\n", err)
		}
	}
	return dbPath, nil
}

// sqliteDatabaseSize is the database's size in bytes (pages × page size).
func sqliteDatabaseSize(ctx context.Context, db *sql.DB) (uint64, error) {
	var pages, pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return 0, fmt.Errorf("read the database size: %w", err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("read the database page size: %w", err)
	}
	if pages < 0 || pageSize < 0 {
		return 0, fmt.Errorf("read the database size: invalid page count %d or size %d", pages, pageSize)
	}
	return uint64(pages) * uint64(pageSize), nil
}

// snapshotVersionLabel is the version as it appears in a snapshot name:
// "git" for a git build, any character outside [A-Za-z0-9._-] replaced.
func snapshotVersionLabel(v string) string {
	if v == "" {
		return "git"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, v)
}

// writeAtomically creates path+".partial" exclusively (0600), lets fill
// write it, then renames it to path. On failure the partial file is
// removed. An existing partial file (another snapshot in progress, or a
// killed one) fails the call.
func writeAtomically(path string, fill func(tmp string) error) error {
	tmp := path + snapshotPartialSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = f.Close()
	if err == nil {
		err = fill(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// copySettingsFile copies src to dst (0600, atomically). copied is false
// when src does not exist.
func copySettingsFile(src, dst string) (copied bool, err error) {
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = in.Close() }()
	err = writeAtomically(dst, func(tmp string) error {
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
	return err == nil, err
}

// pruneSnapshots removes all but the newest keep files named
// <prefix>...-<ts><suffix> in dir (ordered by their timestamp) and returns
// the names removed.
func pruneSnapshots(dir, prefix, suffix string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type snap struct{ name, ts string }
	var snaps []snap
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		stem := strings.TrimSuffix(name, suffix)
		ts := stem[strings.LastIndex(stem, "-")+1:]
		if _, err := time.Parse(snapshotTimeFormat, ts); err != nil {
			continue // not a snapshot this code wrote
		}
		snaps = append(snaps, snap{name: name, ts: ts})
	}
	if len(snaps) <= keep {
		return nil, nil
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].ts != snaps[j].ts {
			return snaps[i].ts > snaps[j].ts
		}
		return snaps[i].name > snaps[j].name
	})
	var removed []string
	var errs []error
	for _, sn := range snaps[keep:] {
		if err := os.Remove(filepath.Join(dir, sn.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, sn.name)
	}
	return removed, errors.Join(errs...)
}
