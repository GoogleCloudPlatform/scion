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

package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeReporter records every Reporter call for assertions.
type fakeReporter struct {
	envIgnored  []struct{ name, replacement string }
	migrated    int
	conflicts   int
	skipped     int
	lastManual  string
	lastReason  string
	lastDetail  string
	lastTracked bool
}

func (f *fakeReporter) Migrated(old, new string, tracked bool) {
	f.migrated++
	f.lastTracked = tracked
}
func (f *fakeReporter) Conflict(old, new, detail string) {
	f.conflicts++
	f.lastDetail = detail
}
func (f *fakeReporter) Skipped(old, reason, manual string) {
	f.skipped++
	f.lastReason = reason
	f.lastManual = manual
}
func (f *fakeReporter) EnvIgnored(name, replacement string) {
	f.envIgnored = append(f.envIgnored, struct{ name, replacement string }{name, replacement})
}

func assertFileGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be gone, stat err = %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("%s content = %q, want %q", path, got, want)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// linkErrorFunc returns a linkFile replacement that runs pre against newname
// (if set) before returning a LinkError with the given errno. pre lets a
// test simulate a concurrent writer acting on newname between this call's
// own checks and its Link attempt, without needing a real second process.
func linkErrorFunc(errno syscall.Errno, pre func(newname string)) func(string, string) error {
	return func(oldname, newname string) error {
		if pre != nil {
			pre(newname)
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
	}
}

func TestWarnRemovedLegacyEnv(t *testing.T) {
	t.Run("reports SCION_HUB_GROVE_ID when set", func(t *testing.T) {
		env := map[string]string{"SCION_HUB_GROVE_ID": "some-uuid"}
		r := &fakeReporter{}

		WarnRemovedLegacyEnv(func(k string) string { return env[k] }, r)

		if len(r.envIgnored) != 1 {
			t.Fatalf("EnvIgnored called %d times, want 1", len(r.envIgnored))
		}
		if got := r.envIgnored[0]; got.name != "SCION_HUB_GROVE_ID" || got.replacement != "SCION_HUB_PROJECT_ID" {
			t.Fatalf("EnvIgnored(%q, %q), want (SCION_HUB_GROVE_ID, SCION_HUB_PROJECT_ID)", got.name, got.replacement)
		}
		if r.migrated != 0 || r.conflicts != 0 || r.skipped != 0 {
			t.Fatalf("unexpected non-env reports: migrated=%d conflicts=%d skipped=%d", r.migrated, r.conflicts, r.skipped)
		}
	})

	t.Run("silent when unset", func(t *testing.T) {
		r := &fakeReporter{}

		WarnRemovedLegacyEnv(func(string) string { return "" }, r)

		if len(r.envIgnored) != 0 {
			t.Fatalf("EnvIgnored called %d times, want 0", len(r.envIgnored))
		}
	})

	t.Run("nil getenv or report is a no-op", func(t *testing.T) {
		WarnRemovedLegacyEnv(nil, &fakeReporter{})
		WarnRemovedLegacyEnv(func(string) string { return "x" }, nil)
	})
}

// TestWarnRemovedLegacyEnvOnce guards the shared boot-hook Once: hub and
// broker both call WarnRemovedLegacyEnvOnce in the same process
// (`scion server start --enable-hub --enable-runtime-broker`), and it must
// report exactly once total, not once per caller.
func TestWarnRemovedLegacyEnvOnce(t *testing.T) {
	warnRemovedLegacyEnvOnce = sync.Once{}
	t.Cleanup(func() { warnRemovedLegacyEnvOnce = sync.Once{} })

	env := map[string]string{"SCION_HUB_GROVE_ID": "some-uuid"}
	getenv := func(k string) string { return env[k] }
	r := &fakeReporter{}

	WarnRemovedLegacyEnvOnce(getenv, r) // hub's call
	WarnRemovedLegacyEnvOnce(getenv, r) // broker's call, same process

	if len(r.envIgnored) != 1 {
		t.Fatalf("EnvIgnored called %d times across two WarnRemovedLegacyEnvOnce calls, want 1", len(r.envIgnored))
	}
}

// TestSlogReporter captures records through a real slog.JSONHandler (not
// just method calls) so it can assert the right level per event and no
// duplicate keys: the reporter must not add a second "component" key on top
// of the one hub/broker already set on slog.Default().
func TestSlogReporter(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	r := NewSlogReporter()
	r.Migrated("old", "new", true)
	r.Conflict("old", "new", "detail")
	r.Skipped("old", "reason", "manual")
	r.EnvIgnored("SCION_HUB_GROVE_ID", "SCION_HUB_PROJECT_ID")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d log records, want 4:\n%s", len(lines), buf.String())
	}

	wantLevels := []string{"INFO", "WARN", "WARN", "WARN"}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record %d: invalid JSON: %v\n%s", i, err, line)
		}
		if got, _ := rec["level"].(string); got != wantLevels[i] {
			t.Errorf("record %d: level = %q, want %q", i, got, wantLevels[i])
		}
		if got, _ := rec["subsystem"].(string); got != "layout-migration" {
			t.Errorf("record %d: subsystem = %q, want %q", i, got, "layout-migration")
		}
		// Checked against the raw JSON text, not the decoded map: a
		// duplicate key collapses silently under json.Unmarshal (last
		// value wins), which is exactly how the earlier bug stayed
		// invisible to a naive test.
		if n := strings.Count(line, `"subsystem"`); n != 1 {
			t.Errorf("record %d: %q key appears %d times, want 1:\n%s", i, "subsystem", n, line)
		}
		if n := strings.Count(line, `"component"`); n > 1 {
			t.Errorf("record %d: %q key appears %d times, want at most 1:\n%s", i, "component", n, line)
		}
	}
}

// TestMigrateLegacyProjectFile exercises the file-rename algorithm
// (migrateLegacyProjectFile) directly, one on-disk outcome at a time. It
// bypasses the per-directory mutex and report deduplication in
// MigrateLegacyProject so each case can call the algorithm exactly once and
// inspect its result; TestMigrateLegacyProject_Idempotent,
// TestMigrateLegacyProject_ConcurrentRace,
// TestMigrateLegacyProject_StaleAfterDelete, and
// TestMigrateLegacyProject_LateAppearingGroveID cover that wrapper
// separately.
func TestMigrateLegacyProjectFile(t *testing.T) {
	t.Run("only legacy present", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "legacy-id\n")

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		// "" per ProjectOverrides: the canonical file now holds the right
		// content, so the caller's own read of it (immediately following,
		// in ReadProjectID) already returns the right value.
		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		assertFileGone(t, filepath.Join(dir, "grove-id"))
		assertFileContent(t, filepath.Join(dir, "project-id"), "legacy-id")
		if r.migrated != 1 || r.conflicts != 0 || r.skipped != 0 {
			t.Errorf("reports: migrated=%d conflicts=%d skipped=%d, want 1/0/0", r.migrated, r.conflicts, r.skipped)
		}
	})

	t.Run("both present same value", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "same-id")
		writeFile(t, filepath.Join(dir, "project-id"), "same-id")

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		assertFileGone(t, filepath.Join(dir, "grove-id"))
		assertFileContent(t, filepath.Join(dir, "project-id"), "same-id")
		if r.migrated != 1 || r.conflicts != 0 {
			t.Errorf("reports: migrated=%d conflicts=%d, want 1/0", r.migrated, r.conflicts)
		}
	})

	t.Run("both present different values", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "legacy-id")
		writeFile(t, newPath, "canonical-id")

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty (canonical wins, caller reads project-id normally)", got)
		}
		// Both files survive; canonical is untouched, legacy is left for
		// the user to resolve.
		assertFileContent(t, old, "legacy-id")
		assertFileContent(t, newPath, "canonical-id")
		if r.conflicts != 1 || r.migrated != 0 {
			t.Errorf("reports: conflicts=%d migrated=%d, want 1/0", r.conflicts, r.migrated)
		}
		wantDetail := "both " + old + " and " + newPath + " exist with different values; using project-id. " +
			"Remove " + old + " (after checking its value) to silence this warning."
		if r.lastDetail != wantDetail {
			t.Errorf("conflict detail = %q, want %q", r.lastDetail, wantDetail)
		}

		// Conflicts are not idempotent: repeated invocations warn every
		// time until the user resolves it by hand (this direct call to the
		// algorithm has no report-deduplication layer; MigrateLegacyProject
		// is what adds that, and only for the same event kind recurring
		// unnecessarily within one process).
		r2 := &fakeReporter{}
		if got2 := migrateLegacyProjectFile(dir, r2); got2 != "" {
			t.Errorf("second run returned id = %q, want empty", got2)
		}
		if r2.conflicts != 1 {
			t.Errorf("second run conflicts = %d, want 1 (conflicts repeat every run)", r2.conflicts)
		}
	})

	t.Run("read-only directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission checks")
		}
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "ro-id")
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "ro-id" {
			t.Errorf("returned id = %q, want %q (in-memory override)", got, "ro-id")
		}
		if r.skipped != 1 {
			t.Errorf("skipped reports = %d, want 1", r.skipped)
		}
		wantManual := "mv " + filepath.Join(dir, "grove-id") + " " + filepath.Join(dir, "project-id")
		if r.lastManual != wantManual {
			t.Errorf("manual command = %q, want %q", r.lastManual, wantManual)
		}
		// The unwrapped errno text, not the full "link a b: ..." error.
		if r.lastReason != "permission denied" {
			t.Errorf("reason = %q, want %q", r.lastReason, "permission denied")
		}
		assertFileContent(t, filepath.Join(dir, "grove-id"), "ro-id")
	})

	t.Run("not owner", func(t *testing.T) {
		// Simulate ownership mismatch via the geteuid seam (the same
		// technique pkg/shareddirs/walk_unix.go uses) instead of chowning a
		// fixture to another uid, which would require running as root.
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "owned-by-other")

		origEuid := geteuid
		geteuid = func() int { return origEuid() + 1 }
		t.Cleanup(func() { geteuid = origEuid })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "owned-by-other" {
			t.Errorf("returned id = %q, want %q (in-memory override)", got, "owned-by-other")
		}
		if r.skipped != 1 {
			t.Errorf("skipped reports = %d, want 1", r.skipped)
		}
		assertFileContent(t, filepath.Join(dir, "grove-id"), "owned-by-other")
	})

	t.Run("no-hard-link fallback", func(t *testing.T) {
		// Pin the umask so the mode assertion below doesn't depend on the
		// environment's own umask.
		oldUmask := syscall.Umask(0o022)
		t.Cleanup(func() { syscall.Umask(oldUmask) })

		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		if err := os.WriteFile(old, []byte("cross-device-id"), 0644); err != nil {
			t.Fatal(err)
		}
		// 0664, set explicitly after the (umask-filtered) WriteFile above:
		// a mode with a group- or other-write bit the 0o022 umask would
		// otherwise clear. A 0600 (or any umask-only-narrows-it) fixture
		// cannot tell a dropped Chmod fix-up apart from OpenFile's own
		// umask filtering, since both produce the same result.
		if err := os.Chmod(old, 0664); err != nil {
			t.Fatal(err)
		}

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
		}
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		assertFileGone(t, old)
		newPath := filepath.Join(dir, "project-id")
		assertFileContent(t, newPath, "cross-device-id")
		if r.migrated != 1 || r.skipped != 0 {
			t.Errorf("reports: migrated=%d skipped=%d, want 1/0", r.migrated, r.skipped)
		}
		info, err := os.Stat(newPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0664 {
			t.Errorf("project-id mode = %v, want 0664 (OpenFile's perm is umask-filtered; must be fixed up)", got)
		}
	})

	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP} {
		t.Run("no-hard-link fallback on "+errno.Error(), func(t *testing.T) {
			// linkUnsupported must treat EPERM and ENOTSUP the same as EXDEV:
			// some FUSE and SMB-style mounts return EPERM for link(2), and
			// missing either case here would report Skipped on every
			// process start instead of completing the copy fallback.
			dir := t.TempDir()
			old := filepath.Join(dir, "grove-id")
			writeFile(t, old, "fallback-id")

			origLink := linkFile
			linkFile = linkErrorFunc(errno, nil)
			t.Cleanup(func() { linkFile = origLink })

			r := &fakeReporter{}
			got := migrateLegacyProjectFile(dir, r)

			if got != "" {
				t.Errorf("returned id = %q, want empty", got)
			}
			if r.migrated != 1 || r.skipped != 0 {
				t.Errorf("reports: migrated=%d skipped=%d, want 1/0", r.migrated, r.skipped)
			}
			assertFileGone(t, old)
			assertFileContent(t, filepath.Join(dir, "project-id"), "fallback-id")
		})
	}

	t.Run("an unrecognized link error is skipped, not copied", func(t *testing.T) {
		// Only EXDEV/EPERM/ENOTSUP may take the copy fallback; any other
		// link error (e.g. EIO on a writable directory) must be reported
		// Skipped, not silently copied on an error the algorithm doesn't
		// otherwise understand.
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		writeFile(t, old, "io-error-id")

		origLink := linkFile
		linkFile = linkErrorFunc(syscall.EIO, nil)
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "io-error-id" {
			t.Errorf("returned id = %q, want %q (in-memory override)", got, "io-error-id")
		}
		if r.skipped != 1 || r.migrated != 0 {
			t.Errorf("reports: skipped=%d migrated=%d, want 1/0", r.skipped, r.migrated)
		}
		assertFileGone(t, filepath.Join(dir, "project-id"))
		assertFileContent(t, old, "io-error-id")
	})

	t.Run("no-hard-link fallback cleans up a partial file on chmod failure", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		writeFile(t, old, "cross-device-id")
		newPath := filepath.Join(dir, "project-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
		}
		t.Cleanup(func() { linkFile = origLink })

		origChmodFile := chmodFile
		chmodFile = func(f *os.File, mode os.FileMode) error {
			return errors.New("injected chmod failure")
		}
		t.Cleanup(func() { chmodFile = origChmodFile })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "cross-device-id" {
			t.Errorf("returned id = %q, want %q (in-memory override)", got, "cross-device-id")
		}
		if r.skipped != 1 || r.migrated != 0 {
			t.Errorf("reports: skipped=%d migrated=%d, want 1/0", r.skipped, r.migrated)
		}
		// The O_EXCL create succeeded before the injected failure; it must
		// not leave a partial (here, empty) project-id behind to win every
		// future "both exist" comparison against the intact legacy file.
		assertFileGone(t, newPath)
		assertFileContent(t, old, "cross-device-id")
	})

	t.Run("no-hard-link fallback cleans up a partial file on write failure", func(t *testing.T) {
		// Pins that "complete" becomes true only after Write succeeds, not
		// any earlier: moving it above Write would leave an empty or
		// partial project-id behind on a real Write failure (e.g. ENOSPC),
		// which then wins every later "both exist" comparison against the
		// intact legacy file. No new seam is needed — closing f inside the
		// chmodFile hook (which still succeeds) makes the real f.Write that
		// follows fail with "file already closed".
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		writeFile(t, old, "cross-device-id")
		newPath := filepath.Join(dir, "project-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
		}
		t.Cleanup(func() { linkFile = origLink })

		origChmodFile := chmodFile
		chmodFile = func(f *os.File, mode os.FileMode) error {
			_ = f.Close()
			return nil
		}
		t.Cleanup(func() { chmodFile = origChmodFile })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "cross-device-id" {
			t.Errorf("returned id = %q, want %q (in-memory override)", got, "cross-device-id")
		}
		if r.skipped != 1 || r.migrated != 0 {
			t.Errorf("reports: skipped=%d migrated=%d, want 1/0", r.skipped, r.migrated)
		}
		assertFileGone(t, newPath)
		assertFileContent(t, old, "cross-device-id")
	})

	t.Run("no-hard-link fallback: old vanishes mid-copy (ENOENT)", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		writeFile(t, old, "legacy-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			// Simulate a concurrent process finishing the migration by hard
			// link right as this call decides hard links aren't supported:
			// remove old, then report EXDEV so the copy fallback runs and
			// finds old already gone.
			if err := os.Remove(oldname); err != nil {
				return err
			}
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
		}
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.skipped != 0 || r.migrated != 0 || r.conflicts != 0 {
			t.Errorf("reports: skipped=%d migrated=%d conflicts=%d, want 0/0/0 (a concurrent finish is silent)",
				r.skipped, r.migrated, r.conflicts)
		}
		assertFileGone(t, old)
	})

	t.Run("no-hard-link fallback: a Sync failure after a complete write must not delete the survivor", func(t *testing.T) {
		// Regression: once Write has fully placed the content in project-id,
		// a later Sync or Close failure must not remove it. A concurrent
		// process can already see that complete file and rely on it to
		// finish its own migration — including removing grove-id — via the
		// "both exist, same value" branch. If this call then deleted
		// project-id too (because ITS OWN Sync failed), neither file would
		// survive and the id would be lost. Simulated here with a nested
		// migrateLegacyProjectFile call run from inside the injected Sync,
		// standing in for a second process racing on the same directory.
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "concurrent-finish-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EXDEV}
		}
		t.Cleanup(func() { linkFile = origLink })

		origSyncFile := syncFile
		syncFile = func(f *os.File) error {
			// project-id is now fully written (Write has already
			// returned). Run the concurrent finisher before this call
			// decides what to do about the Sync failure below.
			migrateLegacyProjectFile(dir, &fakeReporter{})
			return errors.New("injected sync failure (e.g. EIO)")
		}
		t.Cleanup(func() { syncFile = origSyncFile })

		r := &fakeReporter{}
		migrateLegacyProjectFile(dir, r)

		if r.skipped != 1 {
			t.Errorf("skipped reports = %d, want 1", r.skipped)
		}
		// The concurrent finisher already removed grove-id; project-id
		// must still be here, with the id, or the id is lost entirely.
		assertFileGone(t, old)
		assertFileContent(t, newPath, "concurrent-finish-id")
	})

	t.Run("no-hard-link fallback: a Close failure after a complete write must not delete the survivor", func(t *testing.T) {
		// The Close-failure twin of the Sync-failure case above: an fsync
		// that succeeds followed by a Close that fails (e.g. a delayed
		// write-back error on NFS) must be treated the same way — Skipped,
		// with the now-complete project-id left in place rather than
		// removed, since a concurrent process may already be relying on it.
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "close-failure-id")

		origLink := linkFile
		linkFile = linkErrorFunc(syscall.EXDEV, nil)
		t.Cleanup(func() { linkFile = origLink })

		origSyncFile := syncFile
		syncFile = func(f *os.File) error {
			// Sync itself succeeds, but leaves the file already closed so
			// the real Close that follows fails.
			_ = f.Close()
			return nil
		}
		t.Cleanup(func() { syncFile = origSyncFile })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "close-failure-id" {
			t.Errorf("returned id = %q, want %q (in-memory override)", got, "close-failure-id")
		}
		if r.skipped != 1 || r.migrated != 0 {
			t.Errorf("reports: skipped=%d migrated=%d, want 1/0", r.skipped, r.migrated)
		}
		assertFileContent(t, newPath, "close-failure-id")
		assertFileContent(t, old, "close-failure-id")
	})

	t.Run("concurrent winner finishes with a different value (EEXIST)", func(t *testing.T) {
		// Closes a mutation gap: dropping the EEXIST re-entry check, or
		// swapping the hard link for something that clobbers (e.g.
		// os.Rename), both let a concurrent writer's value be lost. The
		// injected linker writes project-id itself and then calls the real
		// default linker, so this exercises a genuine EEXIST from the OS,
		// not a stand-in error.
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "legacy-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			if err := os.WriteFile(newname, []byte("concurrent-id"), 0644); err != nil {
				return err
			}
			return origLink(oldname, newname)
		}
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.conflicts != 1 || r.migrated != 0 {
			t.Errorf("reports: conflicts=%d migrated=%d, want 1/0", r.conflicts, r.migrated)
		}
		// The concurrent winner's value must survive untouched: a
		// clobbering link (e.g. os.Rename) would silently replace it with
		// "legacy-id" here.
		assertFileContent(t, newPath, "concurrent-id")
		assertFileContent(t, old, "legacy-id")
	})

	t.Run("concurrent winner finishes with the same value (EEXIST)", func(t *testing.T) {
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "legacy-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			if err := os.WriteFile(newname, []byte("legacy-id"), 0644); err != nil {
				return err
			}
			return origLink(oldname, newname)
		}
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.migrated != 1 || r.conflicts != 0 {
			t.Errorf("reports: migrated=%d conflicts=%d, want 1/0", r.migrated, r.conflicts)
		}
		assertFileGone(t, old)
		assertFileContent(t, newPath, "legacy-id")
	})

	t.Run("copy fallback: concurrent winner with a different value (O_EXCL EEXIST)", func(t *testing.T) {
		// The copy-path twin of the two EEXIST cases above: O_EXCL must
		// still guard the create, and the copy's own fs.ErrExist must
		// re-enter resolveExistingProjectIDFiles the same way the link
		// path's EEXIST does. Losing either guard loses the concurrent
		// writer's value, just in different ways: a create that overwrites
		// instead of failing (e.g. O_TRUNC) would silently replace it and
		// report Migrated; a create that fails without the re-entry would
		// instead report Skipped, with a manual mv command that would
		// clobber the canonical file if a person actually ran it.
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "legacy-id")

		origLink := linkFile
		linkFile = linkErrorFunc(syscall.EXDEV, func(n string) {
			if err := os.WriteFile(n, []byte("concurrent-id"), 0644); err != nil {
				t.Fatal(err)
			}
		})
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.conflicts != 1 || r.migrated != 0 || r.skipped != 0 {
			t.Errorf("reports: conflicts=%d migrated=%d skipped=%d, want 1/0/0", r.conflicts, r.migrated, r.skipped)
		}
		// The concurrent winner's value must survive untouched, and the
		// event must be a Conflict, not a Skipped: an O_TRUNC create would
		// silently overwrite it with "legacy-id" here and report Migrated,
		// while a missing ErrExist re-entry would instead report Skipped
		// with a manual mv command that would clobber it if followed.
		assertFileContent(t, newPath, "concurrent-id")
		assertFileContent(t, old, "legacy-id")
	})

	t.Run("concurrent process fully finished (ENOENT on Link)", func(t *testing.T) {
		// oldpath is resolved before newpath, so a concurrent process that
		// completed the whole migration (Link then Remove(old)) between
		// this call's Lstat(old) and its own Link call surfaces as ENOENT,
		// not EEXIST. This must be treated the same as EEXIST — someone
		// else finished — not reported as Skipped.
		dir := t.TempDir()
		old := filepath.Join(dir, "grove-id")
		newPath := filepath.Join(dir, "project-id")
		writeFile(t, old, "legacy-id")

		origLink := linkFile
		linkFile = func(oldname, newname string) error {
			if err := os.WriteFile(newname, []byte("legacy-id"), 0644); err != nil {
				return err
			}
			if err := os.Remove(oldname); err != nil {
				return err
			}
			// oldname is now gone, so the real linker — not a fabricated
			// error — produces the genuine kernel ENOENT this test pins.
			return origLink(oldname, newname)
		}
		t.Cleanup(func() { linkFile = origLink })

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		// By the time resolveExistingProjectIDFiles re-reads old, the
		// concurrent process has already removed it: nothing left for this
		// caller to do or report, per resolveExistingProjectIDFiles's
		// existing "old is gone" handling.
		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.skipped != 0 || r.migrated != 0 || r.conflicts != 0 {
			t.Errorf("reports: skipped=%d migrated=%d conflicts=%d, want 0/0/0 (a concurrent finish is silent)",
				r.skipped, r.migrated, r.conflicts)
		}
		assertFileGone(t, old)
		assertFileContent(t, newPath, "legacy-id")
	})

	t.Run("git-tracked file gives the commit hint", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		repo := t.TempDir()
		runGit(t, repo, "init", "-q")
		runGit(t, repo, "config", "user.email", "test@example.com")
		runGit(t, repo, "config", "user.name", "test")

		scionDir := filepath.Join(repo, ".scion")
		writeFile(t, filepath.Join(scionDir, "grove-id"), "tracked-id")
		runGit(t, repo, "add", ".scion/grove-id")
		runGit(t, repo, "commit", "-q", "-m", "add grove-id")

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(scionDir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.migrated != 1 {
			t.Fatalf("migrated reports = %d, want 1", r.migrated)
		}
		if !r.lastTracked {
			t.Error("Migrated was called with tracked=false, want true")
		}
	})

	t.Run("untracked file reports tracked=false", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "untracked-id")

		r := &fakeReporter{}
		migrateLegacyProjectFile(dir, r)

		if r.lastTracked {
			t.Error("Migrated was called with tracked=true for an untracked file")
		}
	})

	t.Run("nothing to migrate", func(t *testing.T) {
		dir := t.TempDir()

		r := &fakeReporter{}
		got := migrateLegacyProjectFile(dir, r)

		if got != "" {
			t.Errorf("returned id = %q, want empty", got)
		}
		if r.migrated != 0 || r.conflicts != 0 || r.skipped != 0 {
			t.Errorf("unexpected reports: migrated=%d conflicts=%d skipped=%d", r.migrated, r.conflicts, r.skipped)
		}
	})
}

// TestCheckGitTracked_OnlyCalledOnMigration puts checkGitTracked behind a
// counting stub and asserts it is never invoked on a path that did not
// migrate anything: the no-op, conflict, and skipped cases.
func TestCheckGitTracked_OnlyCalledOnMigration(t *testing.T) {
	var calls int
	orig := checkGitTracked
	checkGitTracked = func(path string) bool {
		calls++
		return false
	}
	t.Cleanup(func() { checkGitTracked = orig })

	t.Run("nothing to migrate", func(t *testing.T) {
		calls = 0
		dir := t.TempDir()
		migrateLegacyProjectFile(dir, &fakeReporter{})
		if calls != 0 {
			t.Errorf("checkGitTracked called %d times, want 0", calls)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		calls = 0
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "legacy-id")
		writeFile(t, filepath.Join(dir, "project-id"), "canonical-id")
		migrateLegacyProjectFile(dir, &fakeReporter{})
		if calls != 0 {
			t.Errorf("checkGitTracked called %d times, want 0", calls)
		}
	})

	t.Run("skipped (read-only)", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission checks")
		}
		calls = 0
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "ro-id")
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0755) })
		migrateLegacyProjectFile(dir, &fakeReporter{})
		if calls != 0 {
			t.Errorf("checkGitTracked called %d times, want 0", calls)
		}
	})

	t.Run("migrated", func(t *testing.T) {
		calls = 0
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), "legacy-id")
		migrateLegacyProjectFile(dir, &fakeReporter{})
		if calls != 1 {
			t.Errorf("checkGitTracked called %d times, want 1", calls)
		}
	})
}

// TestGitTracked_TimesOutOnHungGit proves that gitTracked returns false
// promptly when the git process hangs, instead of blocking on it: a fake
// `git` on PATH sleeps well past a shrunk gitTrackedTimeout, and the call
// must still return within a small multiple of that timeout, not the
// sleep duration.
func TestGitTracked_TimesOutOnHungGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "git")
	// exec sleep replaces the shell process with sleep instead of forking
	// it, so killing this script's direct child (what exec.CommandContext
	// does on timeout) is enough to stop it.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	origTimeout := gitTrackedTimeout
	gitTrackedTimeout = 100 * time.Millisecond
	t.Cleanup(func() { gitTrackedTimeout = origTimeout })

	dir := t.TempDir()
	target := filepath.Join(dir, "file")
	writeFile(t, target, "content")

	resultCh := make(chan bool, 1)
	start := time.Now()
	go func() { resultCh <- gitTracked(target) }()

	select {
	case got := <-resultCh:
		if got {
			t.Errorf("gitTracked = true, want false when git hangs")
		}
		if elapsed := time.Since(start); elapsed >= 2*time.Second {
			t.Errorf("gitTracked took %v, want well under the 10s sleep", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gitTracked did not return within 2s of a 100ms timeout")
	}
}

// TestReadProjectID_ReadOnlyDirUsesInMemoryOverrideAndWarnsOnce is the
// read-only-filesystem case exercised at the ReadProjectID integration
// point, not just migrateLegacyProjectFile's return value: the caller must
// still resolve the legacy id, and the warning must fire only once even
// across repeated calls.
func TestReadProjectID_ReadOnlyDirUsesInMemoryOverrideAndWarnsOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "grove-id"), "ro-project-id")
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

	r := &fakeReporter{}
	orig := currentProjectMigrationReporter()
	SetProjectMigrationReporter(r)
	t.Cleanup(func() { SetProjectMigrationReporter(orig) })

	id, err := ReadProjectID(dir)
	if err != nil {
		t.Fatalf("ReadProjectID: %v", err)
	}
	if id != "ro-project-id" {
		t.Errorf("ReadProjectID = %q, want %q", id, "ro-project-id")
	}
	if r.skipped != 1 {
		t.Fatalf("skipped reports = %d, want 1", r.skipped)
	}

	id2, err2 := ReadProjectID(dir)
	if err2 != nil {
		t.Fatalf("second ReadProjectID: %v", err2)
	}
	if id2 != "ro-project-id" {
		t.Errorf("second ReadProjectID = %q, want %q", id2, "ro-project-id")
	}
	if r.skipped != 1 {
		t.Errorf("second call: skipped reports = %d, want still 1 (warned once)", r.skipped)
	}
}

// TestMigrateLegacyProject_Idempotent covers report deduplication in
// MigrateLegacyProject: a second call for the same directory, in the same
// process, with unchanged on-disk state, must not report Migrated again.
// Unlike an earlier version of this code, the *result* is not cached — see
// TestMigrateLegacyProject_StaleAfterDelete and
// TestMigrateLegacyProject_LateAppearingGroveID for why that matters.
func TestMigrateLegacyProject_Idempotent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "grove-id"), "idempotent-id")

	r := &fakeReporter{}
	first := MigrateLegacyProject(dir, r)
	if r.migrated != 1 {
		t.Fatalf("first call: migrated reports = %d, want 1", r.migrated)
	}
	if first.ProjectID != "" {
		t.Errorf("first call override = %q, want empty (project-id now exists on disk)", first.ProjectID)
	}

	second := MigrateLegacyProject(dir, r)
	if r.migrated != 1 {
		t.Errorf("second call: migrated reports = %d, want still 1 (silent)", r.migrated)
	}
	if second.ProjectID != "" {
		t.Errorf("second call override = %q, want empty", second.ProjectID)
	}
}

// TestMigrateLegacyProject_StaleAfterDelete guards against a regression
// where a successful migration's id was cached for the life of the
// process: once project-id is deleted, a later call — and ReadProjectID
// through it — must see that it is gone, never return the old value.
func TestMigrateLegacyProject_StaleAfterDelete(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "grove-id"), "old-id")

	r := &fakeReporter{}
	first := MigrateLegacyProject(dir, r)
	if first.ProjectID != "" {
		t.Fatalf("first call override = %q, want empty", first.ProjectID)
	}
	assertFileContent(t, filepath.Join(dir, "project-id"), "old-id")

	if err := os.Remove(filepath.Join(dir, "project-id")); err != nil {
		t.Fatal(err)
	}

	second := MigrateLegacyProject(dir, r)
	if second.ProjectID != "" {
		t.Errorf("after deleting project-id, override = %q, want empty (nothing to migrate; must not resurrect a stale id)", second.ProjectID)
	}
	if r.migrated != 1 {
		t.Errorf("migrated reports = %d, want still 1 (this call is a no-op, not a new migration)", r.migrated)
	}
	if _, err := os.Stat(filepath.Join(dir, "project-id")); !os.IsNotExist(err) {
		t.Errorf("project-id should still be gone, stat err = %v", err)
	}

	// The regression this guards against surfaces through ReadProjectID: it
	// must return a not-exist error, never a stale id.
	if _, err := ReadProjectID(dir); !os.IsNotExist(err) {
		t.Errorf("ReadProjectID after deleting project-id = (id, %v), want a not-exist error", err)
	}
}

// TestMigrateLegacyProject_LateAppearingGroveID guards against a regression
// where a "nothing to migrate" result was cached forever: a grove-id file
// that appears later in the process's life (e.g. a directory probed once,
// before a repo carrying a committed grove-id is cloned into it) must still
// be picked up.
func TestMigrateLegacyProject_LateAppearingGroveID(t *testing.T) {
	dir := t.TempDir()

	r := &fakeReporter{}
	first := MigrateLegacyProject(dir, r)
	if first.ProjectID != "" || r.migrated != 0 {
		t.Fatalf("first call (nothing present) = %+v, migrated=%d, want empty/0", first, r.migrated)
	}

	writeFile(t, filepath.Join(dir, "grove-id"), "late-id")

	second := MigrateLegacyProject(dir, r)
	if second.ProjectID != "" {
		t.Errorf("second call override = %q, want empty (migrated to disk)", second.ProjectID)
	}
	if r.migrated != 1 {
		t.Errorf("migrated reports = %d, want 1 once the late-appearing grove-id is picked up", r.migrated)
	}
	assertFileGone(t, filepath.Join(dir, "grove-id"))
	assertFileContent(t, filepath.Join(dir, "project-id"), "late-id")
}

// TestMigrateLegacyProject_ConcurrentRace runs MigrateLegacyProject from 8
// goroutines against the same directory. Exactly one of them must do the
// actual work and report Migrated; every caller must observe the same,
// correct result. Run with -race.
func TestMigrateLegacyProject_ConcurrentRace(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "grove-id"), "race-id")

	r := &fakeReporter{}
	const n = 8
	results := make([]ProjectOverrides, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = MigrateLegacyProject(dir, r)
		}(i)
	}
	wg.Wait()

	if r.migrated != 1 {
		t.Fatalf("migrated reports = %d, want exactly 1", r.migrated)
	}
	want := results[0]
	for i, got := range results {
		if got != want {
			t.Errorf("result[%d] = %+v, want %+v (every caller must agree)", i, got, want)
		}
	}
	assertFileGone(t, filepath.Join(dir, "grove-id"))
	assertFileContent(t, filepath.Join(dir, "project-id"), "race-id")
}

// TestMigrateLegacyProject_MutexSerializesConcurrentCallers makes the
// per-directory mutex's effect deterministic. The plain goroutine race in
// TestMigrateLegacyProject_ConcurrentRace only catches a dropped mutex
// probabilistically, since the whole algorithm usually finishes well inside
// one scheduling quantum. Here the injected linker holds a "critical
// section" open long enough — tracked with an atomic in-flight counter —
// that a missing mutex would reliably show more than one caller reaching
// Link at once.
func TestMigrateLegacyProject_MutexSerializesConcurrentCallers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "grove-id"), "race-id")

	var inFlight, maxInFlight int64
	origLink := linkFile
	linkFile = func(oldname, newname string) error {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			m := atomic.LoadInt64(&maxInFlight)
			if n <= m || atomic.CompareAndSwapInt64(&maxInFlight, m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return origLink(oldname, newname)
	}
	t.Cleanup(func() { linkFile = origLink })

	r := &fakeReporter{}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			MigrateLegacyProject(dir, r)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&maxInFlight); got != 1 {
		t.Errorf("max concurrent callers inside the linker = %d, want 1 (the per-directory mutex must serialise them)", got)
	}
	if r.migrated != 1 {
		t.Errorf("migrated reports = %d, want exactly 1", r.migrated)
	}
	assertFileGone(t, filepath.Join(dir, "grove-id"))
	assertFileContent(t, filepath.Join(dir, "project-id"), "race-id")
}

// TestProjectMigrationReporter covers the default/setter seam that lets the
// CLI switch per-project migration reporting to stderr while hub, runtime
// broker, and sciontool keep the slog default (SetProjectMigrationReporter).
func TestProjectMigrationReporter(t *testing.T) {
	orig := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(orig) })

	if _, ok := currentProjectMigrationReporter().(slogReporter); !ok {
		t.Fatalf("default reporter = %T, want slogReporter", currentProjectMigrationReporter())
	}

	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)
	if currentProjectMigrationReporter() != Reporter(fake) {
		t.Fatalf("SetProjectMigrationReporter did not take effect")
	}
}
