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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSnapshotVersionLabel(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "git",
		"nightly-20261023":   "nightly-20261023",
		"v0.3.0-preview.4":   "v0.3.0-preview.4",
		"0.1.0+dirty/../x y": "0.1.0_dirty_.._x_y",
	} {
		if got := snapshotVersionLabel(in); got != want {
			t.Errorf("snapshotVersionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsupportedDBSnapshotter(t *testing.T) {
	s := NewUnsupportedDBSnapshotter("PostgreSQL database")
	if st := s.SnapshotStatus(); st.Supported || st.Dir != "" || st.Reason != "PostgreSQL database" {
		t.Fatalf("status = %+v", st)
	}
	if _, err := s.Snapshot(context.Background(), "nightly-20261023", &bytes.Buffer{}); err == nil {
		t.Fatal("an unsupported snapshotter took a snapshot")
	}
}

func TestPruneSnapshots(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"hub-nightly-20261020-20261029T070100Z.db",
		"hub-v0.3.0-preview.4-20261029T070300Z.db",
		"hub-nightly-20261021-20261029T070200Z.db",
		"hub-nightly-20261021-notatime.db", // not ours
		"settings-20261029T070100Z.yaml",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "hub-dir-20261029T060000Z.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := pruneSnapshots(dir, dbSnapshotPrefix, dbSnapshotSuffix, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"hub-nightly-20261020-20261029T070100Z.db"}) {
		t.Fatalf("removed = %v", removed)
	}
	for _, n := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was removed: %v", n, err)
		}
	}
	if removed, err := pruneSnapshots(dir, dbSnapshotPrefix, dbSnapshotSuffix, 5); err != nil || len(removed) != 0 {
		t.Fatalf("under the limit: removed %v, err %v", removed, err)
	}
}

func TestPruneStalePartials(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	touch := func(name string, mod time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	touch("hub-nightly-20261020-20261029T060000Z.db.partial", now.Add(-2*time.Hour))    // old: stale
	touch("hub-nightly-20261021-20261029T070000Z.db.partial", now.Add(-20*time.Minute)) // older than the newest snapshot
	touch("hub-nightly-20261022-20261029T071000Z.db", now.Add(-10*time.Minute))
	touch("settings-20261029T071500Z.yaml.partial", now.Add(-time.Minute)) // newer than every snapshot: maybe in use
	touch("unrelated.txt", now.Add(-48*time.Hour))

	removed, err := pruneStalePartials(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	want := []string{"hub-nightly-20261020-20261029T060000Z.db.partial", "hub-nightly-20261021-20261029T070000Z.db.partial"}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	for _, keep := range []string{"hub-nightly-20261022-20261029T071000Z.db", "settings-20261029T071500Z.yaml.partial", "unrelated.txt"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
}

func TestSQLiteSnapshotter_StatusWithoutFreeSpace(t *testing.T) {
	s := NewSQLiteSnapshotter(SQLiteSnapshotOptions{Dir: "/x"})
	if got := s.SnapshotStatus().Supported; got != freeDiskSpaceSupported {
		t.Fatalf("default Supported = %v, want freeDiskSpaceSupported %v", got, freeDiskSpaceSupported)
	}
	s.freeSpaceOK = false // a platform where free space cannot be read
	if st := s.SnapshotStatus(); st.Supported || st.Dir != "" || !strings.Contains(st.Reason, "free disk space") {
		t.Fatalf("status = %+v", st)
	}
	injected := NewSQLiteSnapshotter(SQLiteSnapshotOptions{Dir: "/x", FreeSpace: func(string) (uint64, error) { return 1, nil }})
	if !injected.SnapshotStatus().Supported {
		t.Fatal("an injected free-space reader must count as supported")
	}
}
