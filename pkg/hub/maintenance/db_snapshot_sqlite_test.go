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

package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openSnapshotTestDB opens a file-backed SQLite database with a few rows.
func openSnapshotTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		"CREATE TABLE agents (id TEXT PRIMARY KEY, name TEXT)",
		"INSERT INTO agents VALUES ('a1', 'one'), ('a2', 'two')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// snapshotTestClock returns a clock that advances one minute per call.
func snapshotTestClock() func() time.Time {
	at := time.Date(2026, 10, 29, 7, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Minute)
		return at
	}
}

func unlimitedFreeSpace(string) (uint64, error) { return 1 << 40, nil }

func TestSQLiteSnapshotter_Snapshot(t *testing.T) {
	db := openSnapshotTestDB(t)
	root := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(settings, []byte("server:\n  maintenance:\n    release_channel: nightly\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, SnapshotsDirName)
	s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: dir, SettingsPath: settings,
		FreeSpace: unlimitedFreeSpace, Now: snapshotTestClock()})
	if st := s.SnapshotStatus(); !st.Supported || st.Dir != dir {
		t.Fatalf("status = %+v", st)
	}

	var log bytes.Buffer
	path, err := s.Snapshot(context.Background(), "nightly-20261023", &log)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if want := filepath.Join(dir, "hub-nightly-20261023-20261029T070100Z.db"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	snap, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	var n int
	if err := snap.QueryRow("SELECT count(*) FROM agents").Scan(&n); err != nil || n != 2 {
		t.Fatalf("snapshot rows = %d, err = %v", n, err)
	}
	copied, err := os.ReadFile(filepath.Join(dir, "settings-20261029T070100Z.yaml"))
	if err != nil || !strings.Contains(string(copied), "release_channel: nightly") {
		t.Fatalf("settings copy = %q, err = %v", copied, err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, filepath.Join(dir, "settings-20261029T070100Z.yaml")} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o600 {
				t.Fatalf("%s: mode %v, want 0600", p, fi.Mode().Perm())
			}
		}
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("snapshot dir mode %v, want 0700", fi.Mode().Perm())
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*"+snapshotPartialSuffix)); len(matches) != 0 {
		t.Fatalf("partial files left: %v", matches)
	}
	if !strings.Contains(log.String(), "Database snapshot written: "+path) || !strings.Contains(log.String(), "Settings copied") {
		t.Fatalf("log:\n%s", log.String())
	}

	t.Run("git build and no settings file", func(t *testing.T) {
		s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: dir, SettingsPath: filepath.Join(root, "missing.yaml"),
			FreeSpace: unlimitedFreeSpace, Now: snapshotTestClock()})
		var log bytes.Buffer
		path, err := s.Snapshot(context.Background(), "", &log)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(path) != "hub-git-20261029T070100Z.db" {
			t.Fatalf("path = %q", path)
		}
		if !strings.Contains(log.String(), "No settings file") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
}

func TestSQLiteSnapshotter_Retention(t *testing.T) {
	db := openSnapshotTestDB(t)
	dir := t.TempDir()
	settings := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(settings, []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Files that are not snapshots of this code are never removed.
	for _, name := range []string{"hub.db", "hub-notes.db", "README"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: dir, SettingsPath: settings,
		FreeSpace: unlimitedFreeSpace, Now: snapshotTestClock()})
	var paths []string
	for i := 0; i < DefaultSnapshotKeep+2; i++ {
		// Alternate versions: retention orders by time, not by name.
		from := []string{"v0.3.0-preview.4", "nightly-20261023"}[i%2]
		p, err := s.Snapshot(context.Background(), from, &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(p))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var dbs, sets, others []string
	for _, e := range entries {
		switch n := e.Name(); {
		case strings.HasPrefix(n, "hub-") && strings.Contains(n, "T07"):
			dbs = append(dbs, n)
		case strings.HasPrefix(n, "settings-"):
			sets = append(sets, n)
		default:
			others = append(others, n)
		}
	}
	slices.Sort(dbs)
	want := slices.Clone(paths[2:])
	slices.Sort(want)
	if !slices.Equal(dbs, want) {
		t.Fatalf("kept db snapshots %v, want the newest %d: %v", dbs, DefaultSnapshotKeep, want)
	}
	if len(sets) != DefaultSnapshotKeep || slices.Contains(sets, "settings-20261029T070100Z.yaml") {
		t.Fatalf("kept settings copies %v", sets)
	}
	slices.Sort(others)
	if !slices.Equal(others, []string{"README", "hub-notes.db", "hub.db"}) {
		t.Fatalf("other files = %v", others)
	}
}

func TestSQLiteSnapshotter_DiskCheck(t *testing.T) {
	db := openSnapshotTestDB(t)
	size, err := sqliteDatabaseSize(context.Background(), db)
	if err != nil || size == 0 {
		t.Fatalf("size = %d, err = %v", size, err)
	}

	t.Run("less than twice the database size is refused", func(t *testing.T) {
		dir := t.TempDir()
		var asked string
		s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: dir, Now: snapshotTestClock(),
			FreeSpace: func(d string) (uint64, error) { asked = d; return 2*size - 1, nil }})
		_, err := s.Snapshot(context.Background(), "nightly-20261023", &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "not enough free disk space") {
			t.Fatalf("err = %v", err)
		}
		if asked != dir {
			t.Fatalf("free space read for %q, want %q", asked, dir)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("files written despite the refusal: %v", entries)
		}
	})

	t.Run("exactly twice is enough", func(t *testing.T) {
		s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: t.TempDir(), Now: snapshotTestClock(),
			FreeSpace: func(string) (uint64, error) { return 2 * size, nil }})
		if _, err := s.Snapshot(context.Background(), "nightly-20261023", &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unreadable free space fails closed", func(t *testing.T) {
		s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: t.TempDir(), Now: snapshotTestClock(),
			FreeSpace: func(string) (uint64, error) { return 0, errors.ErrUnsupported }})
		if _, err := s.Snapshot(context.Background(), "nightly-20261023", &bytes.Buffer{}); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the default reads the real file system", func(t *testing.T) {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Skip("free space is read on linux and darwin only")
		}
		s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: t.TempDir()})
		if _, err := s.Snapshot(context.Background(), "nightly-20261023", &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSQLiteSnapshotter_ExistingPartialFails(t *testing.T) {
	db := openSnapshotTestDB(t)
	dir := t.TempDir()
	at := snapshotTestClock()() // the clock's first reading
	s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: dir, FreeSpace: unlimitedFreeSpace,
		Now: func() time.Time { return at }})
	partial := filepath.Join(dir, "hub-nightly-20261023-"+at.UTC().Format(snapshotTimeFormat)+".db"+snapshotPartialSuffix)
	if err := os.WriteFile(partial, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// In use: written "now" by the injected clock, so stale-partial pruning
	// keeps it and the exclusive create is what fails.
	if err := os.Chtimes(partial, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(context.Background(), "nightly-20261023", &bytes.Buffer{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(partial); string(b) != "x" {
		t.Fatal("another writer's partial file was touched")
	}
}

func TestSQLiteSnapshotter_StalePartialsFreedBeforeDiskCheck(t *testing.T) {
	db := openSnapshotTestDB(t)
	dir := t.TempDir()
	// snapshotTestClock's first reading, which Snapshot takes once for the
	// stale-partial cutoff and the snapshot name. File times are set
	// relative to it, not to the wall clock, so the test depends on
	// Snapshot using the injected clock (Gemini review on #4371).
	clockNow := snapshotTestClock()()
	stale := filepath.Join(dir, "hub-nightly-20261020-20261028T060000Z.db"+snapshotPartialSuffix)
	if err := os.WriteFile(stale, bytes.Repeat([]byte("x"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	old := clockNow.Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// Ten minutes old by the injected clock (about a day old by the wall
	// clock): possibly in use, so it must be kept. A snapshotter reading
	// the wall clock would remove it.
	fresh := filepath.Join(dir, "settings-20261029T065100Z.yaml"+snapshotPartialSuffix)
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	recent := clockNow.Add(-10 * time.Minute)
	if err := os.Chtimes(fresh, recent, recent); err != nil {
		t.Fatal(err)
	}
	var staleAtCheck bool
	s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{DB: db, Dir: dir, Now: snapshotTestClock(),
		FreeSpace: func(string) (uint64, error) {
			_, err := os.Stat(stale)
			staleAtCheck = err == nil
			return 1 << 40, nil
		}})
	var log bytes.Buffer
	if _, err := s.Snapshot(context.Background(), "nightly-20261023", &log); err != nil {
		t.Fatal(err)
	}
	if staleAtCheck {
		t.Fatal("the stale partial still existed at the free-space check")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a partial that is fresh by the injected clock was removed: %v", err)
	}
	for _, want := range []string{"Removed a leftover partial snapshot", "Writing a database snapshot of", "busy until it completes"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
}
