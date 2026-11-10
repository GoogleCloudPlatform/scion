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

//go:build unix

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Leftover update staging after a successful update (ptone/scion#7177).

// scionUpdateTemps returns the scion-update-* entries in dir.
func scionUpdateTemps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "scion-update-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// appliedStagingDirs returns the plugin staging directories in dir, failing
// unless each is marked applied, has no new/ and keeps old/.
func appliedStagingDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), pluginStagingPrefix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if !isAppliedPluginStaging(p) {
			t.Errorf("%s is not marked applied", p)
		}
		if _, err := os.Stat(filepath.Join(p, "new")); !os.IsNotExist(err) {
			t.Errorf("%s still has new/ (%v)", p, err)
		}
		if _, err := os.Stat(filepath.Join(p, "old")); err != nil {
			t.Errorf("%s lost old/ before the restart: %v", p, err)
		}
		out = append(out, p)
	}
	return out
}

// TestBinaryUpdate_DownloadsAndStagingAtRestart: when the restart is
// requested (the hook runs just before it), the download temp directory is
// already gone and the plugin staging holds only old/ and the applied
// marker, because the restart may end the process before any later
// cleanup runs.
func TestBinaryUpdate_DownloadsAndStagingAtRestart(t *testing.T) {
	requireLinux(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	gh, _, dir, signals, e := symlinkLockstepSetup(t, map[string]string{"telegram": "telegram 22"})
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z",
		lockstepPlugin{name: "telegram", body: "telegram 23"})

	hooked := false
	var log bytes.Buffer
	err := e.RunWithBeforeRestart(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error {
			hooked = true
			if left := scionUpdateTemps(t, tmp); len(left) != 0 {
				t.Errorf("download temp left at the restart: %v", left)
			}
			staging := appliedStagingDirs(t, dir)
			if len(staging) != 1 {
				t.Fatalf("staging dirs at the restart = %v, want 1", staging)
			}
			wantFile(t, filepath.Join(staging[0], "old", "scion-plugin-telegram"), "telegram 22")
			wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 23")
			return nil
		})
	if err != nil || !hooked || *signals != 1 {
		t.Fatalf("Run = %v, hooked %v, signals %d\n%s", err, hooked, *signals, log.String())
	}
	if left := scionUpdateTemps(t, tmp); len(left) != 0 {
		t.Fatalf("download temp left after the run: %v", left)
	}
}

// TestBinaryUpdate_HookFailureStillRestoresAfterMark: the applied marker
// does not stop a rollback: a failing hook restores the old plugin builds
// from old/ and removes the staging.
func TestBinaryUpdate_HookFailureStillRestoresAfterMark(t *testing.T) {
	requireLinux(t)
	gh, _, dir, signals, e := symlinkLockstepSetup(t, map[string]string{"telegram": "telegram 22"})
	gh.addLockstepRelease("nightly-20261023", "2026-10-23T04:00:00Z",
		lockstepPlugin{name: "telegram", body: "telegram 23"})
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error {
			if n := len(appliedStagingDirs(t, dir)); n != 1 {
				t.Fatalf("staging dirs = %d, want 1 marked", n)
			}
			return os.ErrPermission
		})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("Run = %v, want a rollback", err)
	}
	if *signals != 0 {
		t.Fatal("restart requested")
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-telegram"), "telegram 22")
	noStaging(t, dir)
}

// restoreMarkerSetup stages, applies and marks a one-plugin swap in a fresh
// plugin dir, and makes the marker removal fail failures times.
func restoreMarkerSetup(t *testing.T, failures int) (*pluginSwap, string) {
	t.Helper()
	dir := pluginDirWith(t, map[string]string{"a": "a old"})
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(bin, []byte("a new"), 0o755); err != nil {
		t.Fatal(err)
	}
	sw, err := stagePluginSwap(io.Discard, &pluginPlan{dir: dir, plugins: []pluginUpdate{
		{installedPlugin: installedPlugin{file: "scion-plugin-a", name: "a", path: filepath.Join(dir, "scion-plugin-a")}, bin: bin},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.apply(io.Discard); err != nil {
		t.Fatal(err)
	}
	sw.markApplied(io.Discard)
	if !isAppliedPluginStaging(sw.staging) {
		t.Fatal("not marked")
	}
	orig := removePluginStagingMarker
	t.Cleanup(func() { removePluginStagingMarker = orig })
	removePluginStagingMarker = func(marker string) error {
		if failures > 0 {
			failures--
			return errors.New("injected marker removal failure")
		}
		return orig(marker)
	}
	return sw, dir
}

// blockRestore makes the plugin path something a file cannot be renamed
// over, so restoring it fails.
func blockRestore(t *testing.T, dir string) {
	t.Helper()
	p := filepath.Join(dir, "scion-plugin-a")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestPluginSwap_RestoreMarkerStuckMovesAside (#7177 review r2): when the
// restore fails and the applied marker cannot be removed even on retry, the
// staging (the only old builds) is moved off the staging prefix, so neither
// the startup cleanup nor the next update removes it, and the error says
// where it is.
func TestPluginSwap_RestoreMarkerStuckMovesAside(t *testing.T) {
	sw, dir := restoreMarkerSetup(t, 2)
	orig := sw.staging
	blockRestore(t, dir)
	err := sw.restore()
	if err == nil || !strings.Contains(err.Error(), "so the backups were moved to "+sw.staging) {
		t.Fatalf("restore = %v, want the moved-aside note", err)
	}
	if !strings.HasPrefix(filepath.Base(sw.staging), pluginKeptPrefix) || sw.staging == orig {
		t.Fatalf("staging = %s, want it moved under %s", sw.staging, pluginKeptPrefix)
	}
	if _, err := os.Stat(orig); !os.IsNotExist(err) {
		t.Fatalf("original staging still there: %v", err)
	}
	if !sw.keep {
		t.Fatal("staging not kept")
	}
	sw.cleanup()
	wantFile(t, filepath.Join(sw.staging, "old", "scion-plugin-a"), "a old")
	removeStalePluginStaging(io.Discard, dir)
	if removed, err := RemoveAppliedPluginStaging(dir); err != nil || len(removed) != 0 {
		t.Fatalf("RemoveAppliedPluginStaging removed %v (%v)", removed, err)
	}
	wantFile(t, filepath.Join(sw.staging, "old", "scion-plugin-a"), "a old")
}

// TestPluginSwap_RestoreMarkerStuckNotMovable: if the move aside fails too
// (the plugin dir is read-only), the staging stays and the error tells the
// operator to move old/ aside before the next start.
func TestPluginSwap_RestoreMarkerStuckNotMovable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a 0555 directory")
	}
	sw, dir := restoreMarkerSetup(t, 2)
	blockRestore(t, dir)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := sw.restore()
	if err == nil || !strings.Contains(err.Error(), "could not be moved aside") || !strings.Contains(err.Error(), "move "+filepath.Join(sw.staging, "old")+" aside") {
		t.Fatalf("restore = %v, want the move-it-by-hand warning", err)
	}
	if !sw.keep || !strings.HasPrefix(filepath.Base(sw.staging), pluginStagingPrefix) {
		t.Fatalf("keep %v, staging %s", sw.keep, sw.staging)
	}
	wantFile(t, filepath.Join(sw.staging, "old", "scion-plugin-a"), "a old")
}

// TestPluginSwap_RestoreMarkerRetried: a marker removal that fails once is
// retried when the restore fails, so the kept staging is unmarked and the
// startup cleanup leaves it.
func TestPluginSwap_RestoreMarkerRetried(t *testing.T) {
	sw, dir := restoreMarkerSetup(t, 1)
	blockRestore(t, dir)
	err := sw.restore()
	if err == nil || strings.Contains(err.Error(), "still marked applied") {
		t.Fatalf("restore = %v, want a plain restore failure", err)
	}
	if isAppliedPluginStaging(sw.staging) {
		t.Fatal("kept staging still marked")
	}
	if removed, err := RemoveAppliedPluginStaging(dir); err != nil || len(removed) != 0 {
		t.Fatalf("RemoveAppliedPluginStaging removed %v (%v)", removed, err)
	}
}

// TestPluginSwap_RestoreSucceedsDespiteMarkerFailure: when every plugin is
// put back the backups are no longer needed, so a marker that cannot be
// removed does not fail the restore.
func TestPluginSwap_RestoreSucceedsDespiteMarkerFailure(t *testing.T) {
	sw, dir := restoreMarkerSetup(t, 5)
	if err := sw.restore(); err != nil {
		t.Fatalf("restore = %v", err)
	}
	wantFile(t, filepath.Join(dir, "scion-plugin-a"), "a old")
}
