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
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSnapshotPatterns = []string{
	regexp.QuoteMeta("pre-fk-reconcile") + "-[0-9a-f]{8}",
	regexp.QuoteMeta("pre-utc-timestamp-normalize"),
}

var retentionNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// writeAged creates name in dir with the given last use (modification
// time) relative to retentionNow, and returns its path.
func writeAged(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	mt := retentionNow.Add(-age)
	require.NoError(t, os.Chtimes(p, mt, mt))
	return p
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestPruneSQLiteSnapshots(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	writeAged(t, dir, "hub.db", 30*24*time.Hour)
	old := 8 * 24 * time.Hour
	fresh := 6 * 24 * time.Hour

	oldFK := writeAged(t, dir, "hub.db.pre-fk-reconcile-0a1b2c3d-20260901T000000Z.bak", old)
	oldTS := writeAged(t, dir, "hub.db.pre-utc-timestamp-normalize-20260901T000000Z.bak", old)
	// The name time is old, but the last use is recent: kept.
	writeAged(t, dir, "hub.db.pre-fk-reconcile-11111111-20250101T000000Z.bak", fresh)
	// In use on this boot, older than the retention period: kept.
	writeAged(t, dir, "hub.db.pre-fk-reconcile-22222222-20260901T000000Z.bak", old)
	// Modification time in the future: kept.
	writeAged(t, dir, "hub.db.pre-utc-timestamp-normalize-20261001T000000Z.bak", -24*time.Hour)

	untouched := []string{
		"other.db.pre-fk-reconcile-0a1b2c3d-20260901T000000Z.bak", // another database
		"hub.db.old.pre-fk-reconcile-0a1b2c3d-20260901T000000Z.bak",
		"hub.db.pre-fk-reconcile-0a1b2c3d.tmp",                   // in progress or interrupted
		"hub.db.pre-utc-timestamp-normalize.tmp",                 // in progress or interrupted
		"hub.db.pre-fk-reconcile-0a1b2c3-20260901T000000Z.bak",   // 7 hex digits
		"hub.db.pre-fk-reconcile-0a1b2c3d4-20260901T000000Z.bak", // 9 hex digits
		"hub.db.pre-fk-reconcile-0A1B2C3D-20260901T000000Z.bak",  // not lowercase
		"hub.db.pre-fk-reconcile-0a1b2c3d-2026-09-01.bak",        // other time format
		"hub.db.pre-utc-timestamp-normalize-20260901T000000Z.bak.1",
		"hub.db.pre-upgrade-20260901T000000Z.bak",                 // operator copy
		"hub.db.pre-utc-timestampXnormalize-20260901T000000Z.bak", // label is literal
		"hub.db.bak",
	}
	for _, n := range untouched {
		writeAged(t, dir, n, old)
	}
	// A directory with a matching name.
	sub := filepath.Join(dir, "hub.db.pre-fk-reconcile-33333333-20260901T000000Z.bak")
	require.NoError(t, os.Mkdir(sub, 0o700))
	require.NoError(t, os.Chtimes(sub, retentionNow.Add(-old), retentionNow.Add(-old)))
	// A symlink with a matching name, pointing at an old snapshot of
	// another database: neither the link nor its target is touched.
	target := writeAged(t, dir, "target.bak", old)
	link := filepath.Join(dir, "hub.db.pre-fk-reconcile-44444444-20260901T000000Z.bak")
	if err := os.Symlink(target, link); err != nil {
		t.Logf("symlinks unsupported: %v", err)
		link = ""
	}

	before := listDir(t, dir)
	// The keep path is not canonical: it is matched by base name.
	keep := []string{filepath.Join(dir, ".", "hub.db.pre-fk-reconcile-22222222-20260901T000000Z.bak")}
	removed := PruneSQLiteSnapshots(db, testSnapshotPatterns, keep, SnapshotRetention, retentionNow)
	assert.ElementsMatch(t, []string{oldFK, oldTS}, removed)

	var want []string
	for _, n := range before {
		if n != filepath.Base(oldFK) && n != filepath.Base(oldTS) {
			want = append(want, n)
		}
	}
	assert.Equal(t, want, listDir(t, dir))
	if link != "" {
		_, err := os.Stat(target)
		assert.NoError(t, err, "a symlink's target must never be removed")
	}

	// Without the timestamp pattern, an old timestamp snapshot is kept.
	oldTS = writeAged(t, dir, "hub.db.pre-utc-timestamp-normalize-20260902T000000Z.bak", old)
	assert.Empty(t, PruneSQLiteSnapshots(db, testSnapshotPatterns[:1], keep, SnapshotRetention, retentionNow))
	assert.FileExists(t, oldTS)
}

func TestPruneSQLiteSnapshots_RemoveErrorIsTolerated(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	a := writeAged(t, dir, "hub.db.pre-fk-reconcile-0a1b2c3d-20260901T000000Z.bak", 9*24*time.Hour)
	b := writeAged(t, dir, "hub.db.pre-utc-timestamp-normalize-20260901T000000Z.bak", 9*24*time.Hour)

	orig := removeSnapshotFile
	t.Cleanup(func() { removeSnapshotFile = orig })
	removeSnapshotFile = func(p string) error {
		if p == a {
			return errors.New("injected remove failure")
		}
		return orig(p)
	}
	removed := PruneSQLiteSnapshots(db, testSnapshotPatterns, nil, SnapshotRetention, retentionNow)
	assert.Equal(t, []string{b}, removed, "the remaining snapshots are still pruned")
	assert.FileExists(t, a)
}

func TestPruneSQLiteSnapshots_UnreadableOrMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "hub.db")
	assert.Empty(t, PruneSQLiteSnapshots(missing, testSnapshotPatterns, nil, SnapshotRetention, retentionNow))
	assert.Empty(t, TouchSQLiteSnapshots(missing, testSnapshotPatterns, retentionNow))
	assert.Empty(t, PruneSQLiteSnapshots("", testSnapshotPatterns, nil, SnapshotRetention, retentionNow))
	assert.Empty(t, PruneSQLiteSnapshots(filepath.Join(t.TempDir(), "hub.db"), []string{"("}, nil, SnapshotRetention, retentionNow),
		"an invalid pattern checks nothing")
}

func TestTouchSQLiteSnapshots(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	old := 20 * 24 * time.Hour
	fk := writeAged(t, dir, "hub.db.pre-fk-reconcile-0a1b2c3d-20260901T000000Z.bak", old)
	ts := writeAged(t, dir, "hub.db.pre-utc-timestamp-normalize-20260901T000000Z.bak", old)
	other := writeAged(t, dir, "hub.db.pre-upgrade-20260901T000000Z.bak", old)
	tmp := writeAged(t, dir, "hub.db.pre-fk-reconcile-0a1b2c3d.tmp", old)

	touched := TouchSQLiteSnapshots(db, testSnapshotPatterns, retentionNow)
	assert.ElementsMatch(t, []string{fk, ts}, touched)
	for p, want := range map[string]time.Time{
		fk: retentionNow, ts: retentionNow,
		other: retentionNow.Add(-old), tmp: retentionNow.Add(-old),
	} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.True(t, info.ModTime().Equal(want), "%s: mtime %v, want %v", filepath.Base(p), info.ModTime(), want)
	}
	assert.Empty(t, PruneSQLiteSnapshots(db, testSnapshotPatterns, nil, SnapshotRetention, retentionNow),
		"touched snapshots are in their retention period")

	// A failure is logged and the rest are still updated.
	orig := chtimesSnapshotFile
	t.Cleanup(func() { chtimesSnapshotFile = orig })
	chtimesSnapshotFile = func(p string, a, m time.Time) error {
		if p == fk {
			return errors.New("injected chtimes failure")
		}
		return orig(p, a, m)
	}
	assert.Equal(t, []string{ts}, TouchSQLiteSnapshots(db, testSnapshotPatterns, retentionNow.Add(time.Hour)))
}
