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
	"testing"
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
