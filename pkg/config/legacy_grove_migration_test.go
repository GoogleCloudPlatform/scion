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
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReporter records every Reporter call for assertions.
type fakeReporter struct {
	envIgnored    []struct{ name, replacement string }
	migrated      int
	conflicts     int
	skipped       int
	precedence    int
	lastOld       string
	lastNew       string
	lastManual    string
	lastReason    string
	lastDetail    string
	lastTracked   bool
	lastPrecValue string
	lastPrecOther string
}

func (f *fakeReporter) Migrated(old, new string, tracked bool) {
	f.migrated++
	f.lastOld, f.lastNew = old, new
	f.lastTracked = tracked
}
func (f *fakeReporter) Conflict(old, new, detail string) {
	f.conflicts++
	f.lastOld, f.lastNew = old, new
	f.lastDetail = detail
}
func (f *fakeReporter) Skipped(old, reason, manual string) {
	f.skipped++
	f.lastOld = old
	f.lastReason = reason
	f.lastManual = manual
}
func (f *fakeReporter) EnvIgnored(name, replacement string) {
	f.envIgnored = append(f.envIgnored, struct{ name, replacement string }{name, replacement})
}
func (f *fakeReporter) PrecedenceChanged(path, value, other string) {
	f.precedence++
	f.lastOld = path
	f.lastPrecValue = value
	f.lastPrecOther = other
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

	t.Run("reports SCION_GROVE_ID, SCION_GROVE and SCION_GROVE_PATH when set", func(t *testing.T) {
		env := map[string]string{
			"SCION_GROVE_ID":   "some-uuid",
			"SCION_GROVE":      "some-project",
			"SCION_GROVE_PATH": "/some/path",
		}
		r := &fakeReporter{}

		WarnRemovedLegacyEnv(func(k string) string { return env[k] }, r)

		if len(r.envIgnored) != 3 {
			t.Fatalf("EnvIgnored called %d times, want 3", len(r.envIgnored))
		}
		want := map[string]string{
			"SCION_GROVE_ID":   "SCION_PROJECT_ID",
			"SCION_GROVE":      "SCION_PROJECT",
			"SCION_GROVE_PATH": "SCION_PROJECT_PATH",
		}
		got := map[string]string{}
		for _, e := range r.envIgnored {
			got[e.name] = e.replacement
		}
		for name, replacement := range want {
			if got[name] != replacement {
				t.Fatalf("EnvIgnored(%q) replacement = %q, want %q", name, got[name], replacement)
			}
		}
		if r.migrated != 0 || r.conflicts != 0 || r.skipped != 0 {
			t.Fatalf("unexpected non-env reports: migrated=%d conflicts=%d skipped=%d", r.migrated, r.conflicts, r.skipped)
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

// TestReadProjectID_ReadsOnlyProjectIDFile pins ReadProjectID to the
// .scion/project-id file: a current project's id is returned as written,
// and a legacy grove-id file is neither read, renamed, nor reported.
func TestReadProjectID_ReadsOnlyProjectIDFile(t *testing.T) {
	const (
		currentID = "550e8400-e29b-41d4-a716-446655440000"
		legacyID  = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	)

	orig := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(orig) })
	r := &fakeReporter{}
	SetProjectMigrationReporter(r)

	t.Run("current project", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "project-id"), currentID+"\n")
		writeFile(t, filepath.Join(dir, "grove-id"), legacyID)

		got, err := ReadProjectID(dir)
		if err != nil {
			t.Fatalf("ReadProjectID: %v", err)
		}
		if got != currentID {
			t.Errorf("ReadProjectID = %q, want %q", got, currentID)
		}
		assertFileContent(t, filepath.Join(dir, "grove-id"), legacyID)
	})

	t.Run("legacy file only", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "grove-id"), legacyID)

		got, err := ReadProjectID(dir)
		if !os.IsNotExist(err) {
			t.Fatalf("ReadProjectID err = %v, want not-exist", err)
		}
		if got != "" {
			t.Errorf("ReadProjectID = %q, want empty", got)
		}
		assertFileGone(t, filepath.Join(dir, "project-id"))
		assertFileContent(t, filepath.Join(dir, "grove-id"), legacyID)
	})

	if r.migrated != 0 || r.conflicts != 0 || r.skipped != 0 {
		t.Errorf("unexpected reports: migrated=%d conflicts=%d skipped=%d", r.migrated, r.conflicts, r.skipped)
	}
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

// TestMigrateLegacyYAMLKeys_SurgicalPreservesBytes is table-driven over both
// callers of algorithm C (marker keys and hub.grove_id): when the canonical
// key is absent, the rewrite is a byte-for-byte key-token replacement that
// leaves comments, order and formatting untouched.
func TestMigrateLegacyYAMLKeys_SurgicalPreservesBytes(t *testing.T) {
	tests := []struct {
		name    string
		renames []legacyYAMLKeyRename
		orig    string
		want    string
	}{
		{
			name:    "marker file, three keys",
			renames: markerKeyRenames,
			orig: "# a project marker\n" +
				"grove-id: abc-123\n" +
				"grove-name: My Project  # display name\n" +
				"grove-slug: my-project\n" +
				"type: shadow\n",
			want: "# a project marker\n" +
				"project-id: abc-123\n" +
				"project-name: My Project  # display name\n" +
				"project-slug: my-project\n" +
				"type: shadow\n",
		},
		{
			name:    "settings file, nested hub.grove_id",
			renames: hubGroveIDRename,
			orig: "schema_version: \"1\"\n" +
				"hub:\n" +
				"  # linked via the old name\n" +
				"  grove_id: \"legacy-uuid\"  # inline comment\n" +
				"  endpoint: \"https://hub.example.com\"\n",
			want: "schema_version: \"1\"\n" +
				"hub:\n" +
				"  # linked via the old name\n" +
				"  project_id: \"legacy-uuid\"  # inline comment\n" +
				"  endpoint: \"https://hub.example.com\"\n",
		},
		{
			name:    "double-quoted key",
			renames: hubGroveIDRename,
			orig:    "hub:\n  \"grove_id\": \"abc\"\n",
			want:    "hub:\n  \"project_id\": \"abc\"\n",
		},
		{
			name:    "single-quoted key",
			renames: hubGroveIDRename,
			orig:    "hub:\n  'grove_id': abc\n",
			want:    "hub:\n  'project_id': abc\n",
		},
		{
			name:    "CRLF line endings",
			renames: hubGroveIDRename,
			orig:    "hub:\r\n  grove_id: \"abc\"  # comment\r\n  endpoint: x\r\n",
			want:    "hub:\r\n  project_id: \"abc\"  # comment\r\n  endpoint: x\r\n",
		},
		{
			name:    "flow mapping with multi-byte text before the key",
			renames: hubGroveIDRename,
			orig:    "hub: {endpoint: \"héllo wörld\", grove_id: abc}\nx:\n  y: 1\n",
			want:    "hub: {endpoint: \"héllo wörld\", project_id: abc}\nx:\n  y: 1\n",
		},
		{
			name:    "UTF-8 BOM",
			renames: hubGroveIDRename,
			orig:    "\xEF\xBB\xBFhub:\n  grove_id: abc\n",
			want:    "\xEF\xBB\xBFhub:\n  project_id: abc\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.yaml")
			if err := os.WriteFile(path, []byte(tt.orig), 0644); err != nil {
				t.Fatal(err)
			}
			r := &fakeReporter{}
			migrateLegacyYAMLKeys(path, tt.renames, r)

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("migrated content:\n%s\nwant:\n%s", got, tt.want)
			}
			if r.migrated != len(tt.renames) {
				t.Errorf("migrated reports = %d, want %d", r.migrated, len(tt.renames))
			}
		})
	}
}

// TestMigrateLegacyYAMLKeys_ConflictWritesBackup covers a canonical key
// already present with a different value: the legacy key is removed, the
// original file is preserved verbatim in a backup, and Conflict is
// reported. A second case pins the numeric-suffix fallback when the
// default backup name is already taken.
func TestMigrateLegacyYAMLKeys_ConflictWritesBackup(t *testing.T) {
	orig := "hub:\n  project_id: \"canonical-value\"\n  grove_id: \"legacy-value\"\n"

	t.Run("default backup name", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.yaml")
		if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)

		if r.conflicts != 1 {
			t.Fatalf("conflicts = %d, want 1", r.conflicts)
		}
		if r.migrated != 0 {
			t.Errorf("migrated = %d, want 0", r.migrated)
		}
		backup := path + ".grove-migration.bak"
		assertFileContent(t, backup, strings.TrimSpace(orig))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "grove_id") {
			t.Errorf("grove_id should be removed, got %q", got)
		}
		if !strings.Contains(string(got), "canonical-value") {
			t.Errorf("canonical value should survive, got %q", got)
		}
		if !strings.Contains(r.lastDetail, backup) {
			t.Errorf("conflict detail = %q, want it to name the backup path %q", r.lastDetail, backup)
		}
	})

	t.Run("backup name already taken", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.yaml")
		if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
			t.Fatal(err)
		}
		taken := path + ".grove-migration.bak"
		if err := os.WriteFile(taken, []byte("someone else's backup"), 0600); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)

		if r.conflicts != 1 {
			t.Fatalf("conflicts = %d, want 1", r.conflicts)
		}
		assertFileContent(t, taken, "someone else's backup")
		assertFileContent(t, taken+".1", strings.TrimSpace(orig))
	})
}

// TestMigrateLegacyYAMLKeys_Symlink covers a dotfile-managed settings file:
// the resolved target is rewritten in place and the symlink itself is left
// untouched.
func TestMigrateLegacyYAMLKeys_Symlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-settings.yaml")
	if err := os.WriteFile(real, []byte("hub:\n  grove_id: \"abc\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	r := &fakeReporter{}
	migrateLegacyYAMLKeys(link, hubGroveIDRename, r)

	if r.migrated != 1 {
		t.Fatalf("migrated = %d, want 1", r.migrated)
	}
	assertFileContent(t, real, "hub:\n  project_id: \"abc\"")
	target, err := os.Readlink(link)
	if err != nil || target != real {
		t.Errorf("symlink target = %q, err = %v; want unchanged link to %q", target, err, real)
	}
}

// TestMigrateLegacyYAMLKeys_ModePreserved pins that the migrated file keeps
// the original file's permission bits.
func TestMigrateLegacyYAMLKeys_ModePreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("hub:\n  grove_id: \"abc\"\n"), 0640); err != nil {
		t.Fatal(err)
	}

	migrateLegacyYAMLKeys(path, hubGroveIDRename, &fakeReporter{})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}

// TestMigrateLegacyYAMLKeys_ChangeBeforeLockReplanned injects a hook that
// rewrites the file between the unlocked read and the settings lock. The
// migration plans again from the bytes it reads under the lock, so the
// concurrent value is the one migrated.
func TestMigrateLegacyYAMLKeys_ChangeBeforeLockReplanned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("hub:\n  grove_id: \"orig\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	orig := betweenReadAndLock
	t.Cleanup(func() { betweenReadAndLock = orig })
	calls := 0
	betweenReadAndLock = func() {
		calls++
		if err := os.WriteFile(path, []byte("hub:\n  grove_id: \"changed\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	r := &fakeReporter{}
	overrides := migrateLegacyYAMLKeys(path, hubGroveIDRename, r)

	if calls != 1 {
		t.Fatalf("betweenReadAndLock called %d times, want 1", calls)
	}
	if r.migrated != 1 || r.skipped != 0 {
		t.Fatalf("migrated=%d skipped=%d, want migrated=1 skipped=0", r.migrated, r.skipped)
	}
	assertFileContent(t, path, "hub:\n  project_id: \"changed\"")
	if got := overrides["project_id"]; got != "changed" {
		t.Errorf("override = %q, want the value read under the lock", got)
	}
}

// TestMigrateLegacyYAMLKeys_LockBusyDefers: while the settings lock is held
// (here by this process, as LoadModifySaveVersionedSettings holds it around
// LoadSingleFileVersioned), the migration neither waits nor writes. It
// returns the legacy value for use in memory, reports nothing, and a later
// call with the lock free migrates the file.
func TestMigrateLegacyYAMLKeys_LockBusyDefers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	orig := "hub:\n  grove_id: \"legacy\"\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	lock, err := LockSettingsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeReporter{}
	overrides := migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
	lock.Unlock()

	if got := overrides["project_id"]; got != "legacy" {
		t.Errorf("override = %q, want the legacy value in memory", got)
	}
	if r.migrated != 0 || r.skipped != 0 || r.conflicts != 0 {
		t.Fatalf("migrated=%d skipped=%d conflicts=%d, want nothing reported", r.migrated, r.skipped, r.conflicts)
	}
	assertFileContent(t, path, strings.TrimSuffix(orig, "\n"))

	migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
	if r.migrated != 1 {
		t.Fatalf("migrated=%d after the lock was released, want 1", r.migrated)
	}
	assertFileContent(t, path, "hub:\n  project_id: \"legacy\"")
}

// TestLoadVersionedSettings_HubProjectIDPrecedenceChangeLogged covers the
// precedence-change note: migrating a project's own hub.grove_id, while a
// global settings file already has a different hub.project_id, reports the
// precedence change exactly once.
func TestLoadVersionedSettings_HubProjectIDPrecedenceChangeLogged(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	for _, e := range []string{"SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS"} {
		if orig, ok := os.LookupEnv(e); ok {
			_ = os.Unsetenv(e)
			t.Cleanup(func() { _ = os.Setenv(e, orig) })
		}
	}

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  project_id: \"global-value\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"project-value\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	orig := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(orig) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	vs, err := LoadVersionedSettings(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.Hub.ProjectID != "project-value" {
		t.Fatalf("ProjectID = %q, want project-value", vs.Hub.ProjectID)
	}
	if fake.precedence != 1 {
		t.Fatalf("precedence reports = %d, want 1", fake.precedence)
	}
	if fake.lastPrecValue != "project-value" || fake.lastPrecOther != "global-value" {
		t.Errorf("precedence value/other = %q/%q, want project-value/global-value", fake.lastPrecValue, fake.lastPrecOther)
	}
}

// TestMigrateLegacyYAMLKeys_NoLegacyKeysNoWrite pins that a file with no
// legacy keys is never opened for writing: its mtime and bytes are
// unchanged.
func TestMigrateLegacyYAMLKeys_NoLegacyKeysNoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	content := "schema_version: \"1\"\nhub:\n  project_id: \"already-canonical\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	migrated, override := migrateProjectSettingsFile(path)
	if migrated || override != "" {
		t.Fatalf("migrateProjectSettingsFile() = (%v, %q), want (false, \"\")", migrated, override)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// os.SameFile (inode identity) is the reliable signal that no rewrite
	// happened: a CreateTemp+Rename replacement can land back on an
	// unchanged mtime often enough on a coarse-grained filesystem clock to
	// let a real rewrite slip past an mtime-only check.
	if !os.SameFile(before, after) {
		t.Error("file was replaced (different inode); no write should have happened at all")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("content changed:\n%s", got)
	}
}

// TestMigrateLegacyYAMLKeys_NotOwner covers the owner-check failure path,
// via the deterministic geteuid seam: the file is left untouched, Skipped
// is reported, and the legacy value is returned for in-memory use. A
// genuinely read-only filesystem is a different branch (the file is still
// owned by this process, so CreateTemp fails instead) — see
// TestHubProjectIDOverride_UnwritableProjectFileOutranksGlobal's real-chmod
// subtest for that path.
func TestMigrateLegacyYAMLKeys_NotOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	content := "grove-id: abc-123\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return -1 }

	r := &fakeReporter{}
	overrides := migrateLegacyYAMLKeys(path, markerKeyRenames, r)

	if r.skipped != 1 || r.migrated != 0 {
		t.Fatalf("skipped=%d migrated=%d, want skipped=1 migrated=0", r.skipped, r.migrated)
	}
	if r.lastReason != "owned by another user" {
		t.Errorf("reason = %q", r.lastReason)
	}
	if overrides["project-id"] != "abc-123" {
		t.Errorf("overrides[project-id] = %q, want abc-123", overrides["project-id"])
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("file should be untouched, got %q", got)
	}
}

// TestMigrateLegacyYAMLKeys_ReportWireFormat pins the exact text a Migrated
// or Skipped report carries for a nested key like hub.grove_id: the "hub."
// qualifier must survive in both, matching the design's wire format
// ("scion: migrated hub.grove_id -> hub.project_id in <file>"). A mutation
// that reverts to the bare "grove_id"/"project_id" (dropping the qualifier)
// is caught by asserting on lastOld/lastNew directly, not just on counts.
func TestMigrateLegacyYAMLKeys_ReportWireFormat(t *testing.T) {
	t.Run("Migrated", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.yaml")
		if err := os.WriteFile(path, []byte("hub:\n  grove_id: abc\n"), 0644); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
		if r.lastOld != "hub.grove_id" {
			t.Errorf("lastOld = %q, want hub.grove_id", r.lastOld)
		}
		if r.lastNew != "hub.project_id in "+path {
			t.Errorf("lastNew = %q, want %q", r.lastNew, "hub.project_id in "+path)
		}
	})

	t.Run("Skipped", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.yaml")
		if err := os.WriteFile(path, []byte("hub:\n  grove_id: abc\n"), 0644); err != nil {
			t.Fatal(err)
		}
		origEuid := geteuid
		t.Cleanup(func() { geteuid = origEuid })
		geteuid = func() int { return -1 }

		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
		if r.lastOld != path+":hub.grove_id" {
			t.Errorf("lastOld = %q, want %q", r.lastOld, path+":hub.grove_id")
		}
	})
}

// TestMigrateLegacyYAMLKeys_Idempotent covers both idempotency shapes: a
// successful migration naturally goes silent once the legacy key is gone,
// and a persistent conflict is reported only once per process even though
// the condition itself repeats on every call.
func TestMigrateLegacyYAMLKeys_Idempotent(t *testing.T) {
	t.Run("successful migration goes silent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")
		if err := os.WriteFile(path, []byte("grove-id: abc-123\n"), 0644); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, markerKeyRenames, r)
		migrateLegacyYAMLKeys(path, markerKeyRenames, r)
		if r.migrated != 1 {
			t.Errorf("migrated = %d, want 1 (second call finds nothing left to migrate)", r.migrated)
		}
	})

	t.Run("conflict repeats on disk but reports once per process", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")
		orig := "hub:\n  project_id: \"canonical\"\n  grove_id: \"legacy\"\n"
		if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
		if r.conflicts != 1 {
			t.Errorf("conflicts = %d, want 1 (deduped per process)", r.conflicts)
		}
	})
}

// TestMigrateLegacyYAMLKeys_ConcurrentRace runs 8 goroutines against the
// same file; the deterministic transform plus the per-path mutex must
// produce exactly one Migrated report and a correctly migrated file.
func TestMigrateLegacyYAMLKeys_ConcurrentRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	if err := os.WriteFile(path, []byte("grove-id: race-id\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &fakeReporter{}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			migrateLegacyYAMLKeys(path, markerKeyRenames, r)
		}()
	}
	wg.Wait()

	if r.migrated != 1 {
		t.Errorf("migrated reports = %d, want exactly 1", r.migrated)
	}
	assertFileContent(t, path, "project-id: race-id")
}

// TestMigrateLegacyYAMLKeys_GitTrackedHint covers the "commit the rename"
// hint for a marker file tracked by a real git repository.
func TestMigrateLegacyYAMLKeys_GitTrackedHint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	path := filepath.Join(dir, ".scion")
	if err := os.WriteFile(path, []byte("grove-id: tracked-id\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", ".scion")
	run("commit", "-q", "-m", "add marker")

	r := &fakeReporter{}
	migrateLegacyYAMLKeys(path, markerKeyRenames, r)

	if r.migrated != 1 {
		t.Fatalf("migrated = %d, want 1", r.migrated)
	}
	if !r.lastTracked {
		t.Error("expected tracked=true for a git-tracked marker file")
	}
}

// TestReadProjectMarker_MigratesLegacyKeysOnDisk covers the ReadProjectMarker
// hook point directly (not just the algorithm underneath it): a writable
// legacy marker file is migrated on disk and the returned fields are
// correct. Kills a mutation that drops the migrateLegacyMarkerFile call.
func TestReadProjectMarker_MigratesLegacyKeysOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".scion")
	if err := os.WriteFile(path, []byte("grove-id: abc-123\ngrove-name: My Project\ngrove-slug: my-project\n"), 0644); err != nil {
		t.Fatal(err)
	}

	marker, err := ReadProjectMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if marker.ProjectID != "abc-123" || marker.ProjectName != "My Project" || marker.ProjectSlug != "my-project" {
		t.Errorf("marker = %+v, want ProjectID=abc-123 ProjectName=\"My Project\" ProjectSlug=my-project", marker)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "grove-") {
		t.Errorf("file should be migrated on disk, still has legacy keys: %q", data)
	}
	if !strings.Contains(string(data), "project-id: abc-123") {
		t.Errorf("file should hold canonical keys, got %q", data)
	}
}

// TestReadProjectMarker_UnwritableUsesOverride covers the fallback half of
// the same hook point: when the file cannot be rewritten, ReadProjectMarker
// still returns the right fields (from the migrator's override), reports
// Skipped, and leaves the file untouched. Kills a mutation that drops the
// override merge in ReadProjectMarker.
func TestReadProjectMarker_UnwritableUsesOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".scion")
	content := "grove-id: abc-123\ngrove-name: My Project\ngrove-slug: my-project\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return -1 }

	fake := &fakeReporter{}
	orig := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(orig) })
	SetProjectMigrationReporter(fake)

	marker, err := ReadProjectMarker(path)
	if err != nil {
		t.Fatal(err)
	}
	if marker.ProjectID != "abc-123" || marker.ProjectName != "My Project" || marker.ProjectSlug != "my-project" {
		t.Errorf("marker = %+v, want fields recovered from the override", marker)
	}
	if fake.skipped == 0 {
		t.Error("expected at least one Skipped report")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("file should be untouched, got %q", got)
	}
}

// TestLoadSingleFileVersioned_UnwritableUsesOverride covers the
// LoadSingleFileVersioned hook point (the config get/set read path): a
// legacy hub.grove_id that cannot be rewritten still resolves in memory.
// Kills a mutation that drops the override in LoadSingleFileVersioned.
func TestLoadSingleFileVersioned_UnwritableUsesOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("schema_version: \"1\"\nhub:\n  grove_id: \"legacy-ro\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return -1 }

	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.Hub == nil || vs.Hub.ProjectID != "legacy-ro" {
		t.Errorf("Hub = %+v, want ProjectID=legacy-ro", vs.Hub)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "grove_id") {
		t.Errorf("file should be untouched, got %q", data)
	}
}

// unsetTestEnv unsets name for the duration of the test (restoring it via
// t.Cleanup), the same pattern used throughout koanf_test.go/settings_v1_test.go
// to keep this container's own SCION_HUB_ENDPOINT / SCION_AUTO_EXPOSE_PORTS
// from leaking into settings-loading tests.
func unsetTestEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if orig, ok := os.LookupEnv(name); ok {
			_ = os.Unsetenv(name)
			t.Cleanup(func() { _ = os.Setenv(name, orig) })
		}
	}
}

// TestHubProjectIDOverride_UnwritableProjectFileOutranksGlobal covers the
// precedence invariant directly: the global settings file has a canonical
// hub.project_id, the project's own settings file has only an unwritable
// legacy hub.grove_id, and the project's value must still win — through
// both LoadSettings and LoadVersionedSettings, and whether the file is
// unwritable via the geteuid seam (not owner) or a real chmod 0555 project
// directory (skipped when running as root).
func TestHubProjectIDOverride_UnwritableProjectFileOutranksGlobal(t *testing.T) {
	setUp := func(t *testing.T) (projectDir string) {
		t.Helper()
		tmpDir := t.TempDir()
		origHome := os.Getenv("HOME")
		t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
		_ = os.Setenv("HOME", tmpDir)
		unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

		globalDir := filepath.Join(tmpDir, ".scion")
		if err := os.MkdirAll(globalDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nhub:\n  project_id: \"global-ro\"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		projectDir = filepath.Join(tmpDir, "my-project", ".scion")
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
			[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"proj-ro\"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		return projectDir
	}

	t.Run("geteuid seam, LoadSettings and LoadVersionedSettings", func(t *testing.T) {
		projectDir := setUp(t)
		origEuid := geteuid
		t.Cleanup(func() { geteuid = origEuid })
		geteuid = func() int { return -1 }

		s, err := LoadSettings(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		if s.ProjectID != "proj-ro" {
			t.Errorf("LoadSettings: ProjectID = %q, want proj-ro (the project's own value must outrank the global)", s.ProjectID)
		}

		vs, err := LoadVersionedSettings(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		if vs.Hub.ProjectID != "proj-ro" {
			t.Errorf("LoadVersionedSettings: ProjectID = %q, want proj-ro", vs.Hub.ProjectID)
		}
	})

	t.Run("real chmod 0555 directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; chmod 0555 does not prevent writes")
		}
		projectDir := setUp(t)
		if err := os.Chmod(projectDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

		s, err := LoadSettings(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		if s.ProjectID != "proj-ro" {
			t.Errorf("ProjectID = %q, want proj-ro", s.ProjectID)
		}
		data, err := os.ReadFile(filepath.Join(projectDir, "settings.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		// The migration writes through the locked settings writer, which
		// rewrites a writable file in place when its directory refuses
		// new files (see writeSettingsFileAtomic).
		if strings.Contains(string(data), "grove_id") || !strings.Contains(string(data), "project_id: \"proj-ro\"") {
			t.Errorf("file should be migrated in place (the file is writable, its directory is not), got %q", data)
		}
	})
}

// TestLoadSettings_PrecedenceChangeLoggedViaCWDProject covers the real
// CLI load shape: LoadSettings("") resolves the project through the
// current directory (resolveEffectiveProjectPath -> FindProjectRoot), which
// only ever reaches step 4 (external/effective path), never step 3
// (projectPath is empty). Kills a mutation that drops the precedence call
// in LoadSettings specifically — the sibling test through
// LoadVersionedSettings(projectDir) alone does not exercise this call site.
func TestLoadSettings_PrecedenceChangeLoggedViaCWDProject(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  project_id: \"global-canon\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	projectRoot := filepath.Join(tmpDir, "my-project")
	projectScionDir := filepath.Join(projectRoot, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"proj-legacy\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(projectRoot); err != nil {
		t.Fatal(err)
	}

	origReporter := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	// The project file is left unwritable (owner check via geteuid) for the
	// whole test: a real successful migration removes hub.grove_id from
	// disk, which would make a second load naturally silent regardless of
	// dedup. Keeping the file unwritable means "migrated" (the surgical,
	// in-memory-override case) keeps coming back true on every single call —
	// exactly the shape that needs the dedup below, since a command that
	// loads settings several times per invocation (as the real CLI does)
	// would otherwise repeat the note every time.
	origEuid := geteuid
	t.Cleanup(func() { geteuid = origEuid })
	geteuid = func() int { return -1 }

	s, err := LoadSettings("")
	if err != nil {
		t.Fatal(err)
	}
	if s.ProjectID != "proj-legacy" {
		t.Fatalf("ProjectID = %q, want proj-legacy", s.ProjectID)
	}
	if fake.precedence != 1 {
		t.Errorf("precedence reports = %d, want 1", fake.precedence)
	}
	if fake.lastPrecValue != "proj-legacy" || fake.lastPrecOther != "global-canon" {
		t.Errorf("precedence value/other = %q/%q, want proj-legacy/global-canon", fake.lastPrecValue, fake.lastPrecOther)
	}

	if _, err := LoadSettings(""); err != nil {
		t.Fatal(err)
	}
	if fake.precedence != 1 {
		t.Errorf("precedence reports after a second load = %d, want still 1 (deduped)", fake.precedence)
	}
}

// TestMigrateLegacyYAMLKeys_AliasValuesResolved covers YAML anchors/aliases:
// a legacy or canonical value given via an alias must be read as the value
// it refers to, not the anchor name, and an aliased mapping (the whole
// "hub:" value being an alias) must still be searched.
func TestMigrateLegacyYAMLKeys_AliasValuesResolved(t *testing.T) {
	t.Run("legacy value is an alias", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")
		if err := os.WriteFile(path, []byte("anchor: &v abc\ngrove-id: *v\n"), 0644); err != nil {
			t.Fatal(err)
		}
		overrides := migrateLegacyYAMLKeys(path, markerKeyRenames, &fakeReporter{})
		if v := overrides["project-id"]; v != "" && v != "abc" {
			t.Errorf("override = %q, want \"\" (written) or abc, never the anchor name", v)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "project-id: *v") {
			t.Errorf("expected the key renamed with the alias preserved, got %q", got)
		}
	})

	t.Run("canonical alias value used for same-value comparison", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")
		orig := "anchor: &v abc-123\nproject-id: *v\ngrove-id: abc-123\n"
		if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, markerKeyRenames, r)
		if r.conflicts != 0 {
			t.Errorf("conflicts = %d, want 0 (the alias resolves to the same value as grove-id)", r.conflicts)
		}
		if r.migrated != 1 {
			t.Errorf("migrated = %d, want 1", r.migrated)
		}
	})

	t.Run("aliased hub mapping is still found", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")
		if err := os.WriteFile(path, []byte("base: &h\n  grove_id: abc\nhub: *h\n"), 0644); err != nil {
			t.Fatal(err)
		}
		r := &fakeReporter{}
		migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
		if r.migrated != 1 {
			t.Fatalf("migrated = %d, want 1 (an aliased hub: value must still be searched)", r.migrated)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "project_id: abc") {
			t.Errorf("expected project_id: abc in the anchor definition, got %q", got)
		}
	})
}

// TestMigrateLegacyYAMLKeys_MultiDocumentRefusesReencode covers a settings
// file with more than one "---"-separated YAML document: since a re-encode
// only ever writes the first document, migrating a key that requires one
// (canonical already present, or a fallback from the surgical path) must
// refuse with Skipped rather than silently drop everything after the first
// document — with or without a value conflict.
func TestMigrateLegacyYAMLKeys_MultiDocumentRefusesReencode(t *testing.T) {
	cases := []struct {
		name string
		orig string
	}{
		{name: "same-value removal", orig: "grove-id: a\nproject-id: a\n---\nother: 1\n"},
		{name: "conflict", orig: "grove-id: a\nproject-id: b\n---\nother: 1\n"},
		{name: "malformed second document", orig: "grove-id: a\nproject-id: a\n---\nkey: [unterminated\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f.yaml")
			if err := os.WriteFile(path, []byte(tc.orig), 0644); err != nil {
				t.Fatal(err)
			}
			r := &fakeReporter{}
			migrateLegacyYAMLKeys(path, markerKeyRenames, r)

			if r.migrated != 0 || r.conflicts != 0 {
				t.Errorf("migrated=%d conflicts=%d, want 0/0 (refused, not silently re-encoded)", r.migrated, r.conflicts)
			}
			if r.skipped != 1 {
				t.Fatalf("skipped = %d, want 1", r.skipped)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.orig {
				t.Errorf("file should be untouched, got %q", got)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.Contains(e.Name(), "grove-migration.bak") {
					t.Errorf("no backup should be written when refusing, found %s", e.Name())
				}
			}
		})
	}
}

// TestMigrateLegacyYAMLKeys_ConflictBackupNotWrittenWhenLockBusy covers the
// backup-timing invariant: a backup only ever belongs on disk paired with a
// completed migration, so a deferred migration (the settings lock was busy)
// leaves no backup file behind.
func TestMigrateLegacyYAMLKeys_ConflictBackupNotWrittenWhenLockBusy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	orig := "hub:\n  project_id: \"canonical\"\n  grove_id: \"legacy\"\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	lock, err := LockSettingsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeReporter{}
	migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
	lock.Unlock()

	if r.skipped != 0 || r.conflicts != 0 {
		t.Fatalf("skipped=%d conflicts=%d, want 0/0", r.skipped, r.conflicts)
	}
	assertFileContent(t, path, strings.TrimSuffix(orig, "\n"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "grove-migration.bak") {
			t.Errorf("no backup should exist after a deferred migration, found %s", e.Name())
		}
	}
}

// TestMigrateLegacyYAMLKeys_ConflictBackupRemovedOnRenameFailure covers the
// other half of the backup-timing invariant: a backup only belongs on disk
// paired with a completed migration, so a rename failure after the backup
// has already been written must remove it again, along with the temp file.
func TestMigrateLegacyYAMLKeys_ConflictBackupRemovedOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	orig := "hub:\n  project_id: \"canonical\"\n  grove_id: \"legacy\"\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { beforeAtomicRename = nil })
	beforeAtomicRename = func(tmpName, targetPath string) error {
		return fmt.Errorf("simulated rename failure")
	}

	r := &fakeReporter{}
	migrateLegacyYAMLKeys(path, hubGroveIDRename, r)

	if r.skipped != 1 || r.conflicts != 0 {
		t.Fatalf("skipped=%d conflicts=%d, want 1/0", r.skipped, r.conflicts)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != orig {
		t.Errorf("file should be untouched, got %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == filepath.Base(path) || e.Name() == settingsLockFileName {
			continue
		}
		t.Errorf("no backup or temp file should remain after a rename failure, found %s", e.Name())
	}
}

// TestWriteConflictBackup_PartialFileRemovedOnFailure covers
// writeConflictBackup's own cleanup: a write or close failure on the
// backup file it just created must remove that partial file, never leave
// it behind under the final backup name.
func TestWriteConflictBackup_PartialFileRemovedOnFailure(t *testing.T) {
	t.Run("write failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")

		origWrite := backupWriteFile
		t.Cleanup(func() { backupWriteFile = origWrite })
		backupWriteFile = func(f *os.File, data []byte) (int, error) {
			return 0, fmt.Errorf("simulated write failure")
		}

		if _, err := writeConflictBackup(path, []byte("orig-content")); err == nil {
			t.Fatal("expected an error")
		}
		if _, statErr := os.Stat(path + ".grove-migration.bak"); !os.IsNotExist(statErr) {
			t.Errorf("partial backup should have been removed, stat err = %v", statErr)
		}
	})

	t.Run("close failure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.yaml")

		origClose := backupCloseFile
		t.Cleanup(func() { backupCloseFile = origClose })
		backupCloseFile = func(f *os.File) error {
			_ = f.Close()
			return fmt.Errorf("simulated close failure")
		}

		if _, err := writeConflictBackup(path, []byte("orig-content")); err == nil {
			t.Fatal("expected an error")
		}
		if _, statErr := os.Stat(path + ".grove-migration.bak"); !os.IsNotExist(statErr) {
			t.Errorf("partial backup should have been removed, stat err = %v", statErr)
		}
	})
}

// TestLoadSettings_JSONSettingsWithHubGroveIDWarns covers the
// settings.json case: the legacy hub.grove_id key is not rewritten (only
// YAML is in scope), but its presence must produce an explicit Skipped
// warning rather than silently going unread, matching the "no silent
// unlinking" rule already applied to every other legacy surface.
func TestLoadSettings_JSONSettingsWithHubGroveIDWarns(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(projectDir, "settings.json")
	if err := os.WriteFile(jsonPath, []byte(`{"schema_version":"1","hub":{"grove_id":"json-legacy"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	origReporter := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	if _, err := LoadSettings(projectDir); err != nil {
		t.Fatal(err)
	}
	if fake.skipped != 1 {
		t.Fatalf("skipped = %d, want 1", fake.skipped)
	}
	if !strings.Contains(fake.lastReason, "JSON") {
		t.Errorf("reason = %q, want it to mention JSON", fake.lastReason)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "grove_id") {
		t.Errorf("json file must never be touched, got %q", data)
	}

	if _, err := LoadSettings(projectDir); err != nil {
		t.Fatal(err)
	}
	if fake.skipped != 1 {
		t.Errorf("skipped after a second load = %d, want still 1 (deduped)", fake.skipped)
	}
}

// TestLoadSettings_YAMLMergeKeyHubGroveIDWarns covers a hub.grove_id
// reachable only through a YAML merge key ("<<: *anchor"): the migrator's
// own key search walks the raw YAML node tree, which does not resolve a
// merge key the way koanf's parser does, so this value is invisible to it.
// Rewriting the file is out of scope (the merge key's target may be shared
// by other mappings), but the value is still honoured in memory for this
// call — matching what koanf itself resolves — and a Skipped warning names
// the gap instead of the project silently losing its link.
func TestLoadSettings_YAMLMergeKeyHubGroveIDWarns(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(projectDir, "settings.yaml")
	content := "base: &b\n  grove_id: merged-id\nhub:\n  <<: *b\n"
	if err := os.WriteFile(yamlPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	origReporter := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	s, err := LoadSettings(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProjectID != "merged-id" {
		t.Errorf("ProjectID = %q, want merged-id (the value must still resolve, in memory, for this invocation)", s.ProjectID)
	}
	if fake.skipped != 1 {
		t.Fatalf("skipped = %d, want 1", fake.skipped)
	}
	if !strings.Contains(fake.lastReason, "merge key") {
		t.Errorf("reason = %q, want it to mention the merge key", fake.lastReason)
	}
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Errorf("file must never be touched, got %q", data)
	}

	if _, err := LoadSettings(projectDir); err != nil {
		t.Fatal(err)
	}
	if fake.skipped != 1 {
		t.Errorf("skipped after a second load = %d, want still 1 (deduped)", fake.skipped)
	}
}

// TestLoadSettings_YAMLMergeKeyWithCanonicalPresentNoWarn covers the
// canonical-present guard on the merge-key check: when the project file
// already has its own hub.project_id alongside the merge key, that key
// already wins normally, so there is nothing unreachable to report.
func TestLoadSettings_YAMLMergeKeyWithCanonicalPresentNoWarn(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "base: &b\n  grove_id: merged-id\nhub:\n  <<: *b\n  project_id: canonical-id\n"
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	origReporter := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	s, err := LoadSettings(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProjectID != "canonical-id" {
		t.Errorf("ProjectID = %q, want canonical-id", s.ProjectID)
	}
	if fake.skipped != 0 {
		t.Errorf("skipped = %d, want 0 (the file already has its own hub.project_id)", fake.skipped)
	}
}

// TestPrecedenceNotLoggedWhenGlobalItselfMigrated covers the over-firing
// guard, for both loaders: two legacy hub.grove_id values resolve to the
// same project-over-global precedence whether or not either side has been
// migrated yet, so migrating the global layer's own value must not, by
// itself, produce a precedence report.
func TestPrecedenceNotLoggedWhenGlobalItselfMigrated(t *testing.T) {
	loaders := map[string]func(projectDir string) (string, error){
		"LoadSettings": func(projectDir string) (string, error) {
			s, err := LoadSettings(projectDir)
			if err != nil {
				return "", err
			}
			return s.ProjectID, nil
		},
		"LoadVersionedSettings": func(projectDir string) (string, error) {
			vs, err := LoadVersionedSettings(projectDir)
			if err != nil {
				return "", err
			}
			return vs.Hub.ProjectID, nil
		},
	}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			origHome := os.Getenv("HOME")
			t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
			_ = os.Setenv("HOME", tmpDir)
			unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

			globalDir := filepath.Join(tmpDir, ".scion")
			if err := os.MkdirAll(globalDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
				[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"g-legacy\"\n"), 0644); err != nil {
				t.Fatal(err)
			}

			projectDir := filepath.Join(tmpDir, "my-project", ".scion")
			if err := os.MkdirAll(projectDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
				[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"p-legacy\"\n"), 0644); err != nil {
				t.Fatal(err)
			}

			origReporter := currentProjectMigrationReporter()
			t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
			fake := &fakeReporter{}
			SetProjectMigrationReporter(fake)

			projectID, err := load(projectDir)
			if err != nil {
				t.Fatal(err)
			}
			if projectID != "p-legacy" {
				t.Fatalf("ProjectID = %q, want p-legacy", projectID)
			}
			if fake.precedence != 0 {
				t.Errorf("precedence reports = %d, want 0 (the global value just came from its own migration, not a stable pre-existing canonical value)", fake.precedence)
			}
		})
	}
}

// TestLoadSettings_PrecedenceChangeLoggedExplicitProjectPath covers the
// koanf.go step-3 precedence call site (an explicit projectPath, as opposed
// to the step-4/cwd-resolved shape TestLoadSettings_
// PrecedenceChangeLoggedViaCWDProject already covers).
func TestLoadSettings_PrecedenceChangeLoggedExplicitProjectPath(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  project_id: \"global-canon\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpDir, "my-project", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"proj-legacy\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origReporter := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	// projectDir passed explicitly (not ""): this is step 3
	// (`projectPath != ""`), not step 4.
	s, err := LoadSettings(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ProjectID != "proj-legacy" {
		t.Fatalf("ProjectID = %q, want proj-legacy", s.ProjectID)
	}
	if fake.precedence != 1 {
		t.Errorf("precedence reports = %d, want 1", fake.precedence)
	}
}

// TestLoadVersionedSettings_PrecedenceChangeLoggedViaCWDProject covers the
// settings_v1.go step-4 precedence call site (LoadVersionedSettings("")
// resolving the project through the current directory), the sibling of the
// koanf.go cwd-resolved test.
func TestLoadVersionedSettings_PrecedenceChangeLoggedViaCWDProject(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", tmpDir)
	unsetTestEnv(t, "SCION_HUB_ENDPOINT", "SCION_AUTO_EXPOSE_PORTS")

	globalDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  project_id: \"global-canon\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	projectRoot := filepath.Join(tmpDir, "my-project")
	projectScionDir := filepath.Join(projectRoot, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  grove_id: \"proj-legacy\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(projectRoot); err != nil {
		t.Fatal(err)
	}

	origReporter := currentProjectMigrationReporter()
	t.Cleanup(func() { SetProjectMigrationReporter(origReporter) })
	fake := &fakeReporter{}
	SetProjectMigrationReporter(fake)

	vs, err := LoadVersionedSettings("")
	if err != nil {
		t.Fatal(err)
	}
	if vs.Hub.ProjectID != "proj-legacy" {
		t.Fatalf("ProjectID = %q, want proj-legacy", vs.Hub.ProjectID)
	}
	if fake.precedence != 1 {
		t.Errorf("precedence reports = %d, want 1", fake.precedence)
	}
}

// TestMigrateLegacyYAMLKeys_BOMKeyOnFirstLine pins the BOM-skip half of
// runeColumnToByteOffset specifically: the golden BOM case in
// TestMigrateLegacyYAMLKeys_SurgicalPreservesBytes puts the legacy key on
// line 2, so it never exercises the line-1 BOM-skip branch at all. This
// case puts a quoted legacy key directly on line 1, right after the BOM.
func TestMigrateLegacyYAMLKeys_BOMKeyOnFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.yaml")
	orig := "\xEF\xBB\xBF\"grove-id\": abc-123\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	migrateLegacyYAMLKeys(path, markerKeyRenames, &fakeReporter{})

	want := "\xEF\xBB\xBF\"project-id\": abc-123\n"
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("migrated content = %q, want %q", got, want)
	}
}

// migratedEvent, conflictEvent and skippedEvent capture one call each to
// globalLayoutRecorder, below.
type migratedEvent struct {
	old, new string
	tracked  bool
}
type conflictEvent struct{ old, new, detail string }
type skippedEvent struct{ old, reason, manual string }

// globalLayoutRecorder records every Reporter call made while migrating the
// global ~/.scion layout, unlike fakeReporter (used by the per-project
// algorithm above), which only needs the last call and a count: a single
// MigrateLegacyGlobalLayout call can report on several independent entries
// at once, so tests need to tell them apart. Safe for concurrent use, since
// entries under different legacy roots — or different entries racing under
// the same root — can report from different goroutines at once.
type globalLayoutRecorder struct {
	mu        sync.Mutex
	migrated  []migratedEvent
	conflicts []conflictEvent
	skipped   []skippedEvent
}

func (r *globalLayoutRecorder) Migrated(old, new string, tracked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.migrated = append(r.migrated, migratedEvent{old, new, tracked})
}

func (r *globalLayoutRecorder) Conflict(old, new, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conflicts = append(r.conflicts, conflictEvent{old, new, detail})
}

func (r *globalLayoutRecorder) Skipped(old, reason, manual string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skipped = append(r.skipped, skippedEvent{old, reason, manual})
}

func (r *globalLayoutRecorder) EnvIgnored(name, replacement string) {}

func (r *globalLayoutRecorder) PrecedenceChanged(path, value, other string) {}

func (r *globalLayoutRecorder) totalEvents() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.migrated) + len(r.conflicts) + len(r.skipped)
}

// TestMigrateLegacyGlobalLayout_NoLegacyRoot covers the common case: neither
// legacy root exists, so both Lstat calls return ENOENT and nothing else
// happens (no canonical directory is created, nothing is reported).
func TestMigrateLegacyGlobalLayout_NoLegacyRoot(t *testing.T) {
	home := t.TempDir()
	r := &globalLayoutRecorder{}

	MigrateLegacyGlobalLayout(home, r)

	if got := r.totalEvents(); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); !os.IsNotExist(err) {
		t.Errorf("canonical dir should not be created when nothing legacy exists, stat err = %v", err)
	}
}

// TestMigrateLegacyGlobalLayout_ReadDirFailureDoesNotNest covers a legacy
// root that Lstat and MkdirAll can both see (so the canonical root gets
// created) but ReadDir cannot list (e.g. execute-only permissions): the
// printed command must not be a bare "mv legacyRoot canonicalRoot", which
// would nest the legacy root's entries one level too deep inside the
// (already-existing) canonical root instead of merging them.
func TestMigrateLegacyGlobalLayout_ReadDirFailureDoesNotNest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	home := t.TempDir()
	legacyRoot := filepath.Join(home, "groves")
	writeFile(t, filepath.Join(legacyRoot, "p1", "workspace.txt"), "hello")
	if err := os.Chmod(legacyRoot, 0311); err != nil { // execute+write, no read
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacyRoot, 0755) })

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != legacyRoot {
		t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, legacyRoot)
	}
	wantManual := "chmod u+rwx " + shellQuote(legacyRoot) + ", then re-run scion"
	if r.skipped[0].manual != wantManual {
		t.Errorf("manual = %q, want %q", r.skipped[0].manual, wantManual)
	}
}

// TestMigrateLegacyGlobalLayout_MkdirAllFailureUsesBareRename covers the
// other root-level failure, where the canonical root does not exist yet (so
// there is nothing to nest into): a bare mv is the right — and only —
// suggestion here, since MkdirAll never got far enough to create anything.
func TestMigrateLegacyGlobalLayout_MkdirAllFailureUsesBareRename(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}

	t.Run("exact command text", func(t *testing.T) {
		home := t.TempDir()
		legacyRoot := filepath.Join(home, "groves")
		writeFile(t, filepath.Join(legacyRoot, "p1", "workspace.txt"), "hello")
		// projects/ cannot be created because home itself is read-only.
		if err := os.Chmod(home, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(home, 0755) })

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)

		if len(r.skipped) != 1 || r.skipped[0].old != legacyRoot {
			t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, legacyRoot)
		}
		wantManual := "mv " + shellQuote(legacyRoot) + " " + shellQuote(filepath.Join(home, "projects"))
		if r.skipped[0].manual != wantManual {
			t.Errorf("manual = %q, want %q (canonicalRoot does not exist yet, so a bare mv is correct)", r.skipped[0].manual, wantManual)
		}
	})

	t.Run("HOME contains a space", func(t *testing.T) {
		parent := t.TempDir()
		home := filepath.Join(parent, "with space")
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
		legacyRoot := filepath.Join(home, "groves")
		writeFile(t, filepath.Join(legacyRoot, "p1", "workspace.txt"), "hello")
		if err := os.Chmod(home, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(home, 0755) })

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 {
			t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
		}

		// Fix the permission problem the command's own reason names, then
		// actually run the printed command through a shell: unquoted, "with
		// space" would split into two operands and either fail outright or
		// move the wrong thing.
		if err := os.Chmod(home, 0755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", r.skipped[0].manual)
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("command failed: %v\noutput: %s\ncommand: %s", err, out.String(), r.skipped[0].manual)
		}
		assertFileContent(t, filepath.Join(home, "projects", "p1", "workspace.txt"), "hello")
	})
}

// TestMigrateLegacyGlobalLayout_RenameAndSymlinkResolves covers the common
// case for one entry under one legacy root (groves -> projects): the entry
// is renamed, a relative symlink is left at the old path, and anything that
// recorded the old absolute path outside scion's control — modelled here by
// a git worktree gitdir pointer — keeps resolving through it.
func TestMigrateLegacyGlobalLayout_RenameAndSymlinkResolves(t *testing.T) {
	home := t.TempDir()
	legacyRoot := filepath.Join(home, "groves")
	acme := filepath.Join(legacyRoot, "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")

	// A worktree gitdir pointer recorded before migration, holding the
	// legacy absolute path (as `<repo>/.git/worktrees/*/gitdir` and the
	// worktree's own `.git` file do in practice).
	oldWorktreeGitDir := filepath.Join(acme, ".git", "worktrees", "wt")
	if err := os.MkdirAll(oldWorktreeGitDir, 0755); err != nil {
		t.Fatal(err)
	}
	gitdirFile := filepath.Join(t.TempDir(), ".git")
	writeFile(t, gitdirFile, "gitdir: "+oldWorktreeGitDir+"\n")

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	wantNew := filepath.Join(home, "projects", "acme")
	if len(r.migrated) != 1 || r.migrated[0].old != acme || r.migrated[0].new != wantNew {
		t.Fatalf("migrated = %+v, want exactly one event (%q, %q)", r.migrated, acme, wantNew)
	}
	if len(r.conflicts) != 0 || len(r.skipped) != 0 {
		t.Fatalf("unexpected conflicts=%+v skipped=%+v", r.conflicts, r.skipped)
	}

	// The old path is now a relative symlink into the canonical root.
	linkInfo, err := os.Lstat(acme)
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink after migration", acme)
	}
	wantTarget := filepath.Join("..", "projects", "acme")
	if target, err := os.Readlink(acme); err != nil || target != wantTarget {
		t.Errorf("symlink target = %q, %v, want %q", target, err, wantTarget)
	}

	// The content is reachable both directly and through the old path.
	assertFileContent(t, filepath.Join(acme, "workspace.txt"), "hello")
	assertFileContent(t, filepath.Join(wantNew, "workspace.txt"), "hello")

	// The gitdir pointer, still holding the old absolute path, resolves to
	// the same place the canonical path does.
	resolvedOld, err := filepath.EvalSymlinks(oldWorktreeGitDir)
	if err != nil {
		t.Fatalf("legacy gitdir path no longer resolves: %v", err)
	}
	resolvedNew, err := filepath.EvalSymlinks(filepath.Join(wantNew, ".git", "worktrees", "wt"))
	if err != nil {
		t.Fatal(err)
	}
	if resolvedOld != resolvedNew {
		t.Errorf("resolved gitdir = %q, want %q", resolvedOld, resolvedNew)
	}

	// The legacy root itself is never removed: it now holds the symlink.
	if info, err := os.Lstat(legacyRoot); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("legacy root %s should still exist as a real directory, info=%+v err=%v", legacyRoot, info, err)
	}
}

// TestMigrateLegacyGlobalLayout_EmptyCanonicalReplaced covers an existing but
// empty canonical directory: syscall.Rename succeeds because the destination
// is an empty directory, so the entry migrates the same as if dst were
// absent.
func TestMigrateLegacyGlobalLayout_EmptyCanonicalReplaced(t *testing.T) {
	home := t.TempDir()
	acme := filepath.Join(home, "groves", "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")
	if err := os.MkdirAll(filepath.Join(home, "projects", "acme"), 0755); err != nil {
		t.Fatal(err)
	}

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.migrated) != 1 {
		t.Fatalf("migrated = %+v, want exactly one event", r.migrated)
	}
	assertFileContent(t, filepath.Join(home, "projects", "acme", "workspace.txt"), "hello")
}

// TestMigrateLegacyGlobalLayout_ConflictRepeatsEveryRun covers a non-empty
// canonical directory: syscall.Rename fails with ENOTEMPTY, so both directories
// are left in place (no data loss) and the warning is not deduplicated the
// way a successful migration is — it repeats on every call until a person
// resolves it by hand.
func TestMigrateLegacyGlobalLayout_ConflictRepeatsEveryRun(t *testing.T) {
	home := t.TempDir()
	acme := filepath.Join(home, "groves", "acme")
	writeFile(t, filepath.Join(acme, "legacy.txt"), "legacy-content")
	canonical := filepath.Join(home, "projects", "acme")
	writeFile(t, filepath.Join(canonical, "canonical.txt"), "canonical-content")

	r1 := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r1)

	if len(r1.conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one event", r1.conflicts)
	}
	if len(r1.migrated) != 0 {
		t.Fatalf("migrated = %+v, want none (conflict, not migration)", r1.migrated)
	}
	wantDetail := "both " + acme + " and " + canonical + " exist; using " + canonical +
		". Inspect " + acme + " and remove it or merge manually."
	if r1.conflicts[0].detail != wantDetail {
		t.Errorf("conflict detail = %q, want %q", r1.conflicts[0].detail, wantDetail)
	}

	// No data loss: both sides keep their own content.
	assertFileContent(t, filepath.Join(acme, "legacy.txt"), "legacy-content")
	assertFileContent(t, filepath.Join(canonical, "canonical.txt"), "canonical-content")

	// Nothing on disk changed, so a second run reports the same conflict
	// again rather than staying silent.
	r2 := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r2)
	if len(r2.conflicts) != 1 {
		t.Errorf("second run conflicts = %d, want 1 (conflicts repeat every run)", len(r2.conflicts))
	}
}

// TestMigrateLegacyGlobalLayout_DanglingSymlinks covers both symlink cases in
// one pass: a leftover pointing at its own now-deleted canonical target is
// cleaned up, while one pointing anywhere else — meaning something other
// than this migration manages it — is left alone.
func TestMigrateLegacyGlobalLayout_DanglingSymlinks(t *testing.T) {
	home := t.TempDir()
	legacyRoot := filepath.Join(home, "groves")
	if err := os.MkdirAll(legacyRoot, 0755); err != nil {
		t.Fatal(err)
	}

	ownDangling := filepath.Join(legacyRoot, "own")
	if err := os.Symlink(filepath.Join("..", "projects", "own"), ownDangling); err != nil {
		t.Fatal(err)
	}
	foreignDangling := filepath.Join(legacyRoot, "foreign")
	if err := os.Symlink("/nonexistent/elsewhere", foreignDangling); err != nil {
		t.Fatal(err)
	}

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if got := r.totalEvents(); got != 0 {
		t.Fatalf("events = %d, want 0 (dangling-symlink handling is silent)", got)
	}
	if _, err := os.Lstat(ownDangling); !os.IsNotExist(err) {
		t.Errorf("own dangling symlink should have been removed, stat err = %v", err)
	}
	if info, err := os.Lstat(foreignDangling); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("foreign dangling symlink should be left alone, info=%+v err=%v", info, err)
	}
}

// TestRemoveDanglingOwnGlobalSymlink_NonNotExistErrorLeavesLinkAlone covers a
// target Stat failure other than "not exist" (e.g. ELOOP, or EACCES on the
// target's parent): it proves nothing about whether the target exists, so
// the link must be left alone rather than treated as dangling. The symlink
// here points at itself, which makes Stat fail with ELOOP deterministically
// (including when running as root, unlike a permission-based setup) while
// Readlink still returns the same wantTarget text this migration wrote.
func TestRemoveDanglingOwnGlobalSymlink_NonNotExistErrorLeavesLinkAlone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "own")
	wantTarget := "own"
	if err := os.Symlink(wantTarget, src); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(src); err != nil || target != wantTarget {
		t.Fatalf("Readlink(%q) = %q, %v, want %q (test setup)", src, target, err, wantTarget)
	}
	if _, err := os.Stat(src); err == nil || os.IsNotExist(err) {
		t.Fatalf("os.Stat(%q) = %v, want a non-not-exist error to set up this test", src, err)
	}

	removeDanglingOwnGlobalSymlink(src, wantTarget)

	if _, err := os.Lstat(src); err != nil {
		t.Errorf("symlink should be left alone after a non-not-exist Stat error, Lstat err = %v", err)
	}
}

// TestMigrateLegacyGlobalLayout_LegacyRootIsSymlinkSkipped covers the legacy
// root itself being a symlink (user-managed): it is left alone entirely, and
// the canonical root is never even created.
func TestMigrateLegacyGlobalLayout_LegacyRootIsSymlinkSkipped(t *testing.T) {
	home := t.TempDir()
	realElsewhere := t.TempDir()
	legacyRoot := filepath.Join(home, "groves")
	if err := os.Symlink(realElsewhere, legacyRoot); err != nil {
		t.Fatal(err)
	}

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != legacyRoot {
		t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, legacyRoot)
	}
	if !strings.Contains(r.skipped[0].reason, "symlink") {
		t.Errorf("reason = %q, want it to mention the symlink", r.skipped[0].reason)
	}
	canonicalRoot := filepath.Join(home, "projects")
	wantManual := "{ [ ! -e " + shellQuote(canonicalRoot) + " ] || rmdir " + shellQuote(canonicalRoot) + "; } && mv " +
		shellQuote(legacyRoot) + " " + shellQuote(canonicalRoot) + " && ln -s " + shellQuote("projects") + " " + shellQuote(legacyRoot)
	if r.skipped[0].manual != wantManual {
		t.Errorf("manual = %q, want %q (an actual runnable command, not just a description)", r.skipped[0].manual, wantManual)
	}
	if _, err := os.Stat(canonicalRoot); !os.IsNotExist(err) {
		t.Errorf("canonical root should not be created, stat err = %v", err)
	}
	// The symlink itself is untouched.
	if target, err := os.Readlink(legacyRoot); err != nil || target != realElsewhere {
		t.Errorf("legacy root symlink target = %q, %v, want %q", target, err, realElsewhere)
	}
}

// TestMigrateLegacyGlobalLayout_LegacyRootSymlinkAlreadyMigrated is the
// deterministic, direct version of the idempotency check exercised through
// the shell in TestMigrateLegacyGlobalLayout_LegacyRootSymlinkManualCommand:
// a legacy root that is a symlink resolving to the canonical root itself —
// whether the link was written as a relative or an absolute path — is
// recognised as already migrated and produces no report at all, the same as
// a migrated per-entry symlink. Without this, every scion command would
// warn forever after a user (or the migrator's own suggested command) turns
// the legacy root into exactly this kind of symlink.
func TestMigrateLegacyGlobalLayout_LegacyRootSymlinkAlreadyMigrated(t *testing.T) {
	t.Run("relative link to the canonical root", func(t *testing.T) {
		home := t.TempDir()
		canonicalRoot := filepath.Join(home, "projects")
		writeFile(t, filepath.Join(canonicalRoot, "acme", "workspace.txt"), "hello")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink("projects", legacyRoot); err != nil {
			t.Fatal(err)
		}

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if got := r.totalEvents(); got != 0 {
			t.Fatalf("events = %+v, want none: a legacy root already pointing at the canonical root is already migrated", r)
		}
	})

	t.Run("absolute link to the canonical root", func(t *testing.T) {
		home := t.TempDir()
		canonicalRoot := filepath.Join(home, "projects")
		writeFile(t, filepath.Join(canonicalRoot, "acme", "workspace.txt"), "hello")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(canonicalRoot, legacyRoot); err != nil {
			t.Fatal(err)
		}

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if got := r.totalEvents(); got != 0 {
			t.Fatalf("events = %+v, want none: an absolute link to the canonical root is also recognised", r)
		}
	})

	t.Run("dangling legacy root is reported, not silently treated as migrated", func(t *testing.T) {
		// legacyRootAlreadyMigrated must return false (not "already
		// migrated") when the legacy root's own target doesn't resolve,
		// even though the canonical root exists: an unresolvable link is
		// never proof that it points at the canonical root. Without this,
		// a dangling or looping legacy root would silently stop being
		// reported instead of warning the user to fix it by hand.
		home := t.TempDir()
		canonicalRoot := filepath.Join(home, "projects")
		writeFile(t, filepath.Join(canonicalRoot, "acme", "workspace.txt"), "hello")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(filepath.Join(home, "nonexistent"), legacyRoot); err != nil {
			t.Fatal(err)
		}

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 || r.skipped[0].old != legacyRoot {
			t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, legacyRoot)
		}
		if len(r.migrated) != 0 || len(r.conflicts) != 0 {
			t.Errorf("migrated=%+v conflicts=%+v, want both empty", r.migrated, r.conflicts)
		}
	})
}

// TestMigrateLegacyGlobalLayout_LegacyRootSymlinkManualCommand actually runs
// the command TestMigrateLegacyGlobalLayout_LegacyRootIsSymlinkSkipped only
// asserts the text of, covering the edge cases a naive
// `mv legacyRoot/* canonicalRoot/` would get wrong: it must handle a
// dotfile-only target (no shell glob involved, since it renames the whole
// symlink rather than copying entries), an empty or absent canonical target
// (succeed), a non-empty canonical target (refuse, leaving the symlink in
// place so the warning can repeat), and it must never touch anything outside
// scionHome (so it can never trigger a cross-device copy the way moving the
// symlink's *contents* could).
func TestMigrateLegacyGlobalLayout_LegacyRootSymlinkManualCommand(t *testing.T) {
	runCommand := func(t *testing.T, command string) (stdout, stderr string, exitErr error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", command)
		var outBuf, errBuf strings.Builder
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		exitErr = cmd.Run()
		return outBuf.String(), errBuf.String(), exitErr
	}

	t.Run("dotfile-only target, canonical absent", func(t *testing.T) {
		home := t.TempDir()
		real := t.TempDir()
		if err := os.WriteFile(filepath.Join(real, ".hidden"), []byte("secret"), 0644); err != nil {
			t.Fatal(err)
		}
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(real, legacyRoot); err != nil {
			t.Fatal(err)
		}
		canonicalRoot := filepath.Join(home, "projects")

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 {
			t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
		}

		if _, stderr, err := runCommand(t, r.skipped[0].manual); err != nil {
			t.Fatalf("command failed: %v\nstderr: %s", err, stderr)
		}

		// The dotfile survived, reachable both directly and through the
		// preserved old path.
		assertFileContent(t, filepath.Join(canonicalRoot, ".hidden"), "secret")
		assertFileContent(t, filepath.Join(legacyRoot, ".hidden"), "secret")
		if target, err := os.Readlink(legacyRoot); err != nil || target != "projects" {
			t.Errorf("legacy root symlink target = %q, %v, want %q", target, err, "projects")
		}
		if target, err := os.Readlink(canonicalRoot); err != nil || target != real {
			t.Errorf("canonical root symlink target = %q, %v, want %q", target, err, real)
		}

		// Two further runs of the migrator, after the manual command has
		// left groves -> projects, must both be completely silent: the
		// legacy root is now recognised as already migrated, not reported
		// as a fresh symlink to investigate.
		for i := 0; i < 2; i++ {
			r2 := &globalLayoutRecorder{}
			MigrateLegacyGlobalLayout(home, r2)
			if got := r2.totalEvents(); got != 0 {
				t.Fatalf("run %d after the manual command reported %+v, want silence", i+1, r2)
			}
		}
	})

	t.Run("empty canonical target", func(t *testing.T) {
		home := t.TempDir()
		real := t.TempDir()
		writeFile(t, filepath.Join(real, "workspace.txt"), "hello")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(real, legacyRoot); err != nil {
			t.Fatal(err)
		}
		canonicalRoot := filepath.Join(home, "projects")
		if err := os.MkdirAll(canonicalRoot, 0755); err != nil {
			t.Fatal(err)
		}

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 {
			t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
		}

		if _, stderr, err := runCommand(t, r.skipped[0].manual); err != nil {
			t.Fatalf("command failed against an empty target: %v\nstderr: %s", err, stderr)
		}
		assertFileContent(t, filepath.Join(canonicalRoot, "workspace.txt"), "hello")

		for i := 0; i < 2; i++ {
			r2 := &globalLayoutRecorder{}
			MigrateLegacyGlobalLayout(home, r2)
			if got := r2.totalEvents(); got != 0 {
				t.Fatalf("run %d after the manual command reported %+v, want silence", i+1, r2)
			}
		}
	})

	t.Run("HOME contains a space", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "with space")
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
		real := t.TempDir()
		writeFile(t, filepath.Join(real, "workspace.txt"), "hello")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(real, legacyRoot); err != nil {
			t.Fatal(err)
		}
		canonicalRoot := filepath.Join(home, "projects")

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 {
			t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
		}

		// Unquoted, this command would split "with space" into two
		// operands and fail (or silently touch the wrong paths).
		if _, stderr, err := runCommand(t, r.skipped[0].manual); err != nil {
			t.Fatalf("command failed against a HOME containing a space: %v\ncommand: %s\nstderr: %s",
				err, r.skipped[0].manual, stderr)
		}
		assertFileContent(t, filepath.Join(canonicalRoot, "workspace.txt"), "hello")
		assertFileContent(t, filepath.Join(legacyRoot, "workspace.txt"), "hello")

		r2 := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r2)
		if got := r2.totalEvents(); got != 0 {
			t.Fatalf("run after the manual command reported %+v, want silence", r2)
		}
	})

	t.Run("HOME contains a single quote", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "it's mine")
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
		real := t.TempDir()
		writeFile(t, filepath.Join(real, "workspace.txt"), "hello")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(real, legacyRoot); err != nil {
			t.Fatal(err)
		}
		canonicalRoot := filepath.Join(home, "projects")

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 {
			t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
		}

		if _, stderr, err := runCommand(t, r.skipped[0].manual); err != nil {
			t.Fatalf("command failed against a HOME containing a single quote: %v\ncommand: %s\nstderr: %s",
				err, r.skipped[0].manual, stderr)
		}
		assertFileContent(t, filepath.Join(canonicalRoot, "workspace.txt"), "hello")
	})

	t.Run("non-empty canonical target refuses", func(t *testing.T) {
		home := t.TempDir()
		real := t.TempDir()
		writeFile(t, filepath.Join(real, "workspace.txt"), "legacy-content")
		legacyRoot := filepath.Join(home, "groves")
		if err := os.Symlink(real, legacyRoot); err != nil {
			t.Fatal(err)
		}
		canonicalRoot := filepath.Join(home, "projects")
		writeFile(t, filepath.Join(canonicalRoot, "other.txt"), "canonical-precious")

		r := &globalLayoutRecorder{}
		MigrateLegacyGlobalLayout(home, r)
		if len(r.skipped) != 1 {
			t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
		}

		if _, _, err := runCommand(t, r.skipped[0].manual); err == nil {
			t.Fatalf("command unexpectedly succeeded against a non-empty target")
		}

		// Nothing was clobbered or moved: the chain aborted at rmdir.
		assertFileContent(t, filepath.Join(canonicalRoot, "other.txt"), "canonical-precious")
		if target, err := os.Readlink(legacyRoot); err != nil || target != real {
			t.Errorf("legacy root symlink should be untouched, target = %q, %v", target, err)
		}
	})
}

// TestMigrateLegacyGlobalLayout_EXDEV covers a legacy root on a different
// filesystem from the canonical root: the migration never falls back to a
// copy (workspaces under these directories can be many GB), only a manual
// mv-and-relink command — a bare mv would lose the symlink this migration
// exists to leave at the old path.
func TestMigrateLegacyGlobalLayout_EXDEV(t *testing.T) {
	home := t.TempDir()
	acme := filepath.Join(home, "groves", "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")

	orig := renameDir
	renameDir = func(oldname, newname string) error {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameDir = orig })

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != acme {
		t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, acme)
	}
	if r.skipped[0].reason != "on a different filesystem" {
		t.Errorf("reason = %q, want %q", r.skipped[0].reason, "on a different filesystem")
	}
	wantManual := "mv " + shellQuote(acme) + " " + shellQuote(filepath.Join(home, "projects", "acme")) +
		" && ln -s " + shellQuote(filepath.Join("..", "projects", "acme")) + " " + shellQuote(acme)
	if r.skipped[0].manual != wantManual {
		t.Errorf("manual = %q, want %q (never a copy, and keeps the old path resolving)", r.skipped[0].manual, wantManual)
	}

	// Nothing was copied: the legacy content is untouched, and no canonical
	// entry was created for it.
	assertFileContent(t, filepath.Join(acme, "workspace.txt"), "hello")
	if _, err := os.Stat(filepath.Join(home, "projects", "acme")); !os.IsNotExist(err) {
		t.Errorf("canonical entry should not exist after EXDEV, stat err = %v", err)
	}
}

// TestMigrateLegacyGlobalLayout_NotOwner covers an entry owned by another
// user (e.g. `sudo scion ...` run over another user's files): the owner
// check, via the geteuid seam, stops a root-owned rewrite.
func TestMigrateLegacyGlobalLayout_NotOwner(t *testing.T) {
	home := t.TempDir()
	acme := filepath.Join(home, "groves", "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")

	origEuid := geteuid
	geteuid = func() int { return origEuid() + 1 }
	t.Cleanup(func() { geteuid = origEuid })

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != acme {
		t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, acme)
	}
	if r.skipped[0].reason != "owned by another user" {
		t.Errorf("reason = %q, want %q", r.skipped[0].reason, "owned by another user")
	}
	wantManual := "mv " + shellQuote(acme) + " " + shellQuote(filepath.Join(home, "projects", "acme")) +
		" && ln -s " + shellQuote(filepath.Join("..", "projects", "acme")) + " " + shellQuote(acme)
	if r.skipped[0].manual != wantManual {
		t.Errorf("manual = %q, want %q", r.skipped[0].manual, wantManual)
	}
	assertFileContent(t, filepath.Join(acme, "workspace.txt"), "hello")
	if _, err := os.Stat(filepath.Join(home, "projects", "acme")); !os.IsNotExist(err) {
		t.Errorf("canonical entry should not exist, stat err = %v", err)
	}
}

// TestMigrateLegacyGlobalLayout_EntryLstatErrorSkipped covers a non-ENOENT
// Lstat failure on an entry: the legacy root is readable (ReadDir succeeds
// and lists the entry) but not searchable, so Lstat on the entry itself
// fails with EACCES rather than ENOENT. This must be reported, not silently
// dropped — after this PR there is no pkg/config fallback left to fall back
// to, so a silently-dropped entry disappears from discovery entirely.
func TestMigrateLegacyGlobalLayout_EntryLstatErrorSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	home := t.TempDir()
	legacyRoot := filepath.Join(home, "groves")
	acme := filepath.Join(legacyRoot, "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")
	if err := os.Chmod(legacyRoot, 0444); err != nil { // read, no search
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacyRoot, 0755) })

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != acme {
		t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, acme)
	}
	if r.skipped[0].reason != "permission denied" {
		t.Errorf("reason = %q, want %q", r.skipped[0].reason, "permission denied")
	}
	wantManual := "mv " + shellQuote(acme) + " " + shellQuote(filepath.Join(home, "projects", "acme")) +
		" && ln -s " + shellQuote(filepath.Join("..", "projects", "acme")) + " " + shellQuote(acme)
	if r.skipped[0].manual != wantManual {
		t.Errorf("manual = %q, want %q", r.skipped[0].manual, wantManual)
	}
	if len(r.migrated) != 0 {
		t.Errorf("migrated = %+v, want none", r.migrated)
	}
}

// TestRaceLostAfterConcurrentMigration_NonNotExistErrorIsNotARace pins
// raceLostAfterConcurrentMigration's own contract directly: only ENOENT (a
// concurrent process finished the migration) or a symlink (same thing, seen
// via a successful Lstat) counts as a lost race. Any other Lstat error is a
// real failure and must not be masked, so the function returns false and
// lets the caller fall through to classify the original rename error.
func TestRaceLostAfterConcurrentMigration_NonNotExistErrorIsNotARace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	dir := t.TempDir()
	unsearchable := filepath.Join(dir, "unsearchable")
	target := filepath.Join(unsearchable, "entry")
	if err := os.MkdirAll(unsearchable, 0444); err != nil { // read, no search
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unsearchable, 0755) })

	if got := raceLostAfterConcurrentMigration(target); got {
		t.Errorf("raceLostAfterConcurrentMigration(%q) = true for a permission error, want false", target)
	}

	gone := filepath.Join(dir, "does-not-exist")
	if got := raceLostAfterConcurrentMigration(gone); !got {
		t.Errorf("raceLostAfterConcurrentMigration(%q) = false for ENOENT, want true", gone)
	}
}

// TestMigrateLegacyGlobalLayout_UnrecognizedRenameErrorSkipped covers an
// unrecognized rename error (e.g. EIO): reported Skipped with the unwrapped
// errno text and, like every other per-entry Skipped case, a manual command
// that includes the relink — a bare mv would lose the symlink this
// migration exists to leave at the old path.
func TestMigrateLegacyGlobalLayout_UnrecognizedRenameErrorSkipped(t *testing.T) {
	home := t.TempDir()
	acme := filepath.Join(home, "groves", "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")

	orig := renameDir
	renameDir = func(oldname, newname string) error {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: syscall.EIO}
	}
	t.Cleanup(func() { renameDir = orig })

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.skipped) != 1 || r.skipped[0].old != acme {
		t.Fatalf("skipped = %+v, want exactly one event for %q", r.skipped, acme)
	}
	if r.skipped[0].reason != syscall.EIO.Error() {
		t.Errorf("reason = %q, want %q", r.skipped[0].reason, syscall.EIO.Error())
	}
	wantManual := "mv " + shellQuote(acme) + " " + shellQuote(filepath.Join(home, "projects", "acme")) +
		" && ln -s " + shellQuote(filepath.Join("..", "projects", "acme")) + " " + shellQuote(acme)
	if r.skipped[0].manual != wantManual {
		t.Errorf("manual = %q, want %q", r.skipped[0].manual, wantManual)
	}
	assertFileContent(t, filepath.Join(acme, "workspace.txt"), "hello")
}

// TestMigrateLegacyGlobalLayout_ConcurrentRacers pins the concurrency safety
// the directory migration relies on (the same safety a multi-replica hub on
// a shared filesystem needs at boot, exercised here in-process): 8 goroutines
// racing the same legacy root must migrate each entry exactly once, because
// only one goroutine's syscall.Rename per entry can win. Every other
// goroutine either sees the entry already gone, or loses the race between
// its own Lstat and rename call and sees the entry already turned into this
// migration's own symlink — either way it must move on silently: a loser
// that reports Skipped or Conflict here would be a regression (see
// TestMigrateLegacyGlobalEntry_LosesRaceAfterLstat for the deterministic,
// single-goroutine version of exactly that failure mode).
func TestMigrateLegacyGlobalLayout_ConcurrentRacers(t *testing.T) {
	home := t.TempDir()
	names := []string{"acme", "bravo", "charlie"}
	for _, name := range names {
		writeFile(t, filepath.Join(home, "groves", name, "workspace.txt"), name)
	}

	r := &globalLayoutRecorder{}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			MigrateLegacyGlobalLayout(home, r)
		}()
	}
	wg.Wait()

	if len(r.migrated) != len(names) {
		t.Fatalf("migrated events = %d, want %d: %+v", len(r.migrated), len(names), r.migrated)
	}
	if len(r.skipped) != 0 || len(r.conflicts) != 0 {
		t.Fatalf("a losing racer reported skipped=%+v conflicts=%+v, want both empty", r.skipped, r.conflicts)
	}
	seen := map[string]bool{}
	for _, ev := range r.migrated {
		if seen[ev.old] {
			t.Errorf("entry %q reported migrated more than once", ev.old)
		}
		seen[ev.old] = true
	}
	for _, name := range names {
		old := filepath.Join(home, "groves", name)
		if !seen[old] {
			t.Errorf("entry %q was never reported migrated", old)
		}
		assertFileContent(t, filepath.Join(home, "projects", name, "workspace.txt"), name)
		if target, err := os.Readlink(old); err != nil || target != filepath.Join("..", "projects", name) {
			t.Errorf("symlink at %q = %q, %v, want %q", old, target, err, filepath.Join("..", "projects", name))
		}
	}
}

// TestMigrateLegacyGlobalEntry_LosesRaceAfterLstat is the deterministic,
// single-goroutine version of the race TestMigrateLegacyGlobalLayout_ConcurrentRacers
// exercises probabilistically: a caller's Lstat confirms a plain directory,
// then — before its rename call — a concurrent process finishes the entire
// migration (rename, then symlink) out from under it. The losing rename must
// fail silently, not report Skipped: a Skipped report here would print a
// manual `mv` command that, if followed, moves this migration's own symlink
// into the project and deletes the legacy path, breaking every old absolute
// reference to it (worktree gitdirs, container bind mounts, hub records).
func TestMigrateLegacyGlobalEntry_LosesRaceAfterLstat(t *testing.T) {
	home := t.TempDir()
	acme := filepath.Join(home, "groves", "acme")
	writeFile(t, filepath.Join(acme, "workspace.txt"), "hello")
	canonicalRoot := filepath.Join(home, "projects")
	if err := os.MkdirAll(canonicalRoot, 0755); err != nil {
		t.Fatal(err)
	}

	origRename := renameDir
	renameDir = func(oldname, newname string) error {
		// Simulate the concurrent winner: it renames and symlinks first,
		// so by the time our own rename call runs, oldname is already a
		// symlink and newname is already the fully-migrated directory.
		if err := origRename(oldname, newname); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join("..", "projects", "acme"), oldname); err != nil {
			return err
		}
		// Now perform the actual call under test: a second, losing
		// rename attempt against the now-symlinked oldname and the
		// now-populated newname.
		return origRename(oldname, newname)
	}
	t.Cleanup(func() { renameDir = origRename })

	entries, err := os.ReadDir(filepath.Join(home, "groves"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ReadDir(groves) = %d entries, want 1", len(entries))
	}

	r := &globalLayoutRecorder{}
	migrateLegacyGlobalEntry(filepath.Join(home, "groves"), canonicalRoot, "projects", entries[0], r)

	if got := r.totalEvents(); got != 0 {
		t.Fatalf("events = %+v, want none: a lost race must be silent", r)
	}
	assertFileContent(t, filepath.Join(canonicalRoot, "acme", "workspace.txt"), "hello")
	if target, err := os.Readlink(acme); err != nil || target != filepath.Join("..", "projects", "acme") {
		t.Errorf("symlink at %q = %q, %v", acme, target, err)
	}
}

// TestMigrateLegacyGlobalLayout_ConcurrentRacersMultiProcess is the real
// multi-process version of TestMigrateLegacyGlobalLayout_ConcurrentRacers:
// several actual OS processes (not just goroutines in one process) race a
// shared legacy root with many entries, re-invoking this test binary as a
// subprocess helper. This is the shape of the real failure mode a
// multi-replica hub on a shared filesystem hits at boot: a losing process
// must never report Skipped or Conflict. All subprocesses are started first
// and then released together past a shared start barrier (a file they poll
// for), so they actually contend for the same entries instead of finishing
// one after another — with 8 subprocesses and only a handful of entries
// each, sequential process-startup jitter alone is enough to mean no two
// subprocesses are ever actually racing the same entry at the same time,
// which defeats the point of this test. With the barrier and enough
// entries, reverting either race fix in migrateLegacyGlobalEntry reliably
// fails this test; TestMigrateLegacyGlobalEntry_LosesRaceAfterLstat remains
// the deterministic, single-goroutine version of the same regression.
func TestMigrateLegacyGlobalLayout_ConcurrentRacersMultiProcess(t *testing.T) {
	if home := os.Getenv("SCION_TEST_HELPER_MIGRATE_GLOBAL_LAYOUT_HOME"); home != "" {
		runMigrateGlobalLayoutHelperProcess(
			home,
			os.Getenv("SCION_TEST_HELPER_MIGRATE_GLOBAL_LAYOUT_REPORT"),
			os.Getenv("SCION_TEST_HELPER_MIGRATE_GLOBAL_LAYOUT_BARRIER"),
		)
		return
	}
	if testing.Short() {
		t.Skip("spawns real subprocesses; skipped in -short mode")
	}

	home := t.TempDir()
	const entriesPerRoot = 40
	var names []string
	for i := 0; i < entriesPerRoot; i++ {
		name := fmt.Sprintf("p%02d", i)
		names = append(names, name)
		writeFile(t, filepath.Join(home, "groves", name, "workspace.txt"), name)
		writeFile(t, filepath.Join(home, "grove-configs", name+"__cfg", ".scion", "settings.yaml"), "workspace_path: /tmp\n")
	}
	wantMigrated := 2 * entriesPerRoot // both legacy roots

	const n = 8
	reportDir := t.TempDir()
	barrier := filepath.Join(t.TempDir(), "go")
	reportPaths := make([]string, n)
	cmds := make([]*exec.Cmd, n)
	outputs := make([]*strings.Builder, n)
	waited := make([]bool, n)
	// Covers t.Fatal or a panic in this test before every subprocess has
	// been Wait'ed on below — a later cmd.Start failing, or the barrier
	// write failing — by killing every started-but-not-yet-waited-on child
	// immediately. This Cleanup does NOT run if go test's own -timeout
	// fires: the testing package kills the whole process without running
	// Cleanup funcs in that case, so already-started children are still
	// only bounded by their own barrier-poll deadline (see
	// runMigrateGlobalLayoutHelperProcess), which is the sole protection
	// against that specific failure mode, not a backstop for this one.
	t.Cleanup(func() {
		for i, cmd := range cmds {
			if cmd != nil && !waited[i] && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	})
	for i := 0; i < n; i++ {
		reportPath := filepath.Join(reportDir, fmt.Sprintf("report-%d.json", i))
		reportPaths[i] = reportPath
		cmd := exec.Command(os.Args[0], "-test.run=^TestMigrateLegacyGlobalLayout_ConcurrentRacersMultiProcess$")
		cmd.Env = append(os.Environ(),
			"SCION_TEST_HELPER_MIGRATE_GLOBAL_LAYOUT_HOME="+home,
			"SCION_TEST_HELPER_MIGRATE_GLOBAL_LAYOUT_REPORT="+reportPath,
			"SCION_TEST_HELPER_MIGRATE_GLOBAL_LAYOUT_BARRIER="+barrier,
		)
		var out strings.Builder
		outputs[i] = &out
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting subprocess %d: %v", i, err)
		}
		cmds[i] = cmd
	}

	// Give every subprocess time to reach its barrier poll loop before
	// releasing them together: without this, subprocess start-up jitter
	// alone (fork/exec, Go runtime init) spaces the migrations out enough
	// that they never actually contend for the same entry.
	time.Sleep(300 * time.Millisecond)
	if err := os.WriteFile(barrier, nil, 0644); err != nil {
		t.Fatalf("creating start barrier: %v", err)
	}

	for i, cmd := range cmds {
		err := cmd.Wait()
		waited[i] = true
		if err != nil {
			t.Fatalf("subprocess %d: %v: %s", i, err, outputs[i].String())
		}
	}

	var totalMigrated, totalSkipped, totalConflicts int
	for _, p := range reportPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading subprocess report %s: %v", p, err)
		}
		var rep migrateGlobalLayoutHelperReport
		if err := json.Unmarshal(data, &rep); err != nil {
			t.Fatalf("parsing subprocess report %s: %v", p, err)
		}
		totalMigrated += rep.Migrated
		totalSkipped += rep.Skipped
		totalConflicts += rep.Conflicts
	}

	if totalMigrated != wantMigrated {
		t.Errorf("total migrated across %d processes = %d, want %d", n, totalMigrated, wantMigrated)
	}
	if totalSkipped != 0 || totalConflicts != 0 {
		t.Errorf("total skipped=%d conflicts=%d across %d processes, want 0 and 0 (a losing process must never warn)",
			totalSkipped, totalConflicts, n)
	}
	for _, name := range names {
		assertFileContent(t, filepath.Join(home, "projects", name, "workspace.txt"), name)
		if target, err := os.Readlink(filepath.Join(home, "groves", name)); err != nil || target != filepath.Join("..", "projects", name) {
			t.Errorf("symlink at groves/%s = %q, %v", name, target, err)
		}
	}
}

// migrateGlobalLayoutHelperReport is the JSON shape
// runMigrateGlobalLayoutHelperProcess writes for its parent to read.
type migrateGlobalLayoutHelperReport struct {
	Migrated  int
	Skipped   int
	Conflicts int
}

// runMigrateGlobalLayoutHelperProcess is the subprocess body for
// TestMigrateLegacyGlobalLayout_ConcurrentRacersMultiProcess: it waits for
// the shared start barrier to appear, then runs one real migration attempt
// against the shared home and writes its own report to reportPath,
// following the standard os/exec "helper process" test pattern (as used by,
// e.g., os/exec_test.go) so this can run inside the same compiled test
// binary instead of needing a separate helper command.
func runMigrateGlobalLayoutHelperProcess(home, reportPath, barrier string) {
	if barrier != "" {
		// A deadline, not just a t.Cleanup on the parent's side: the parent
		// can fail before ever creating (or trying to create) the barrier
		// file, in which case nothing else would ever stop this loop.
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(barrier); err == nil {
				break
			}
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "timed out waiting for the start barrier")
				os.Exit(2)
			}
			time.Sleep(time.Millisecond)
		}
	}
	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)
	data, err := json.Marshal(migrateGlobalLayoutHelperReport{
		Migrated:  len(r.migrated),
		Skipped:   len(r.skipped),
		Conflicts: len(r.conflicts),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal report:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(reportPath, data, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(1)
	}
}

// TestMigrateLegacyGlobalLayout_IdempotentSecondRunSilent covers the common
// steady state after a successful migration: nothing under the legacy root
// is a plain directory any more (every migrated entry is now a symlink), so
// a second call finds nothing to do and reports nothing.
func TestMigrateLegacyGlobalLayout_IdempotentSecondRunSilent(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "groves", "acme", "workspace.txt"), "hello")

	r1 := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r1)
	if len(r1.migrated) != 1 {
		t.Fatalf("first run migrated = %+v, want exactly one event", r1.migrated)
	}

	r2 := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r2)
	if got := r2.totalEvents(); got != 0 {
		t.Errorf("second run events = %d, want 0 (silent)", got)
	}
}

// TestMigrateLegacyGlobalLayout_NonDirectoryEntryLeftAlone covers a
// non-directory entry under a legacy root: syscall.Rename would otherwise
// replace a same-named canonical *file* silently (the ENOTEMPTY/EEXIST
// no-clobber guard only fires when the source is a directory too), so a
// plain file is left alone rather than risking that clobber.
func TestMigrateLegacyGlobalLayout_NonDirectoryEntryLeftAlone(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "groves", "notes"), "legacy")
	writeFile(t, filepath.Join(home, "projects", "notes"), "canonical-precious")

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	if len(r.migrated) != 0 {
		t.Fatalf("migrated = %+v, want none: a file entry must never be migrated", r.migrated)
	}
	if len(r.skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly one event", r.skipped)
	}
	if r.skipped[0].reason != "not a directory" {
		t.Errorf("reason = %q, want %q", r.skipped[0].reason, "not a directory")
	}
	// The canonical file must survive untouched: canonical-wins, and no
	// clobber.
	assertFileContent(t, filepath.Join(home, "projects", "notes"), "canonical-precious")
	assertFileContent(t, filepath.Join(home, "groves", "notes"), "legacy")
}

// TestMigrateLegacyGlobalLayoutOnce covers the shared boot-hook Once: hub and
// broker both call MigrateLegacyGlobalLayoutOnce in the same process
// (`scion server start --enable-hub --enable-runtime-broker`), and it must
// migrate exactly once total, not once per caller. The fixture is a
// conflict, not a successful migration: a successful Migrated is already
// naturally idempotent (nothing under the legacy root is a plain directory
// after the first run, so a second unwrapped call would find nothing to do
// anyway), so it would pass even with the sync.Once dropped entirely and
// prove nothing. A conflict is not deduplicated at that lower level — it
// repeats on every call for as long as the same on-disk state persists — so
// only the Once wrapper itself can keep the second call from reporting it
// again.
func TestMigrateLegacyGlobalLayoutOnce(t *testing.T) {
	migrateLegacyGlobalLayoutOnce = sync.Once{}
	t.Cleanup(func() { migrateLegacyGlobalLayoutOnce = sync.Once{} })

	home := t.TempDir()
	writeFile(t, filepath.Join(home, "groves", "acme", "legacy.txt"), "legacy")
	writeFile(t, filepath.Join(home, "projects", "acme", "canonical.txt"), "canonical")

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayoutOnce(home, r) // hub's call
	MigrateLegacyGlobalLayoutOnce(home, r) // broker's call, same process

	if len(r.conflicts) != 1 {
		t.Fatalf("conflicts across two MigrateLegacyGlobalLayoutOnce calls = %d, want exactly 1 "+
			"(the Once must prevent the second call from reporting the same conflict again)", len(r.conflicts))
	}
}

// TestMigrateLegacyGlobalLayout_ProjectConfigsRoot covers the second legacy
// root (grove-configs -> project-configs), exercised end to end so a
// mistake wiring only one of the two legacyGlobalRoots entries would be
// caught.
func TestMigrateLegacyGlobalLayout_ProjectConfigsRoot(t *testing.T) {
	home := t.TempDir()
	dirName := "acme__12345678"
	writeFile(t, filepath.Join(home, "grove-configs", dirName, ".scion", "settings.yaml"), "workspace_path: /tmp/acme\n")

	r := &globalLayoutRecorder{}
	MigrateLegacyGlobalLayout(home, r)

	wantOld := filepath.Join(home, "grove-configs", dirName)
	wantNew := filepath.Join(home, "project-configs", dirName)
	if len(r.migrated) != 1 || r.migrated[0].old != wantOld || r.migrated[0].new != wantNew {
		t.Fatalf("migrated = %+v, want exactly one event (%q, %q)", r.migrated, wantOld, wantNew)
	}
	if target, err := os.Readlink(wantOld); err != nil || target != filepath.Join("..", "project-configs", dirName) {
		t.Errorf("symlink at %q = %q, %v", wantOld, target, err)
	}
}

// TestMigrateLegacyGlobalLayout_SlogSubsystem is the hub/broker-side check
// that a real migration logs through logging.Subsystem("layout-migration"),
// exercised end to end (not just through the Reporter methods directly, as
// TestSlogReporter above does) so a mistake in how MigrateLegacyGlobalLayout
// invokes the Reporter would be caught here too.
func TestMigrateLegacyGlobalLayout_SlogSubsystem(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	home := t.TempDir()
	writeFile(t, filepath.Join(home, "groves", "acme", "workspace.txt"), "hello")

	MigrateLegacyGlobalLayout(home, NewSlogReporter())

	out := buf.String()
	if !strings.Contains(out, `"subsystem":"layout-migration"`) {
		t.Fatalf("expected a layout-migration log record, got:\n%s", out)
	}
	if !strings.Contains(out, `"msg":"migrated legacy layout"`) {
		t.Fatalf("expected a migrated-legacy-layout record, got:\n%s", out)
	}
}

// TestDiscoverProjects_FindsProjectMigratedFromGroves is the acceptance-level
// check that discovery finds a project that used to live under groves/ once
// the global layout has migrated: readWorkspaceMarkerForSlug (used by
// DiscoverProjects' git-external path) only looks under ProjectsDir, so this
// also guards against reintroducing the deleted groves/ fallback there.
func TestDiscoverProjects_FindsProjectMigratedFromGroves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A legacy git-project workspace: a directory under groves/ whose only
	// scion content is a marker *file* (not a directory) pointing at itself,
	// as split-storage git projects use once externalized.
	slug := "myslug"
	marker := &ProjectMarker{
		ProjectID:   "11111111-2222-3333-4444-555555555555",
		ProjectName: slug,
		ProjectSlug: slug,
	}
	markerPath := filepath.Join(home, ".scion", "groves", slug, DotScion)
	if err := os.MkdirAll(filepath.Dir(markerPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectMarker(markerPath, marker); err != nil {
		t.Fatal(err)
	}

	MigrateLegacyGlobalLayout(filepath.Join(home, ".scion"), NewSlogReporter())

	got, workspacePath, err := readWorkspaceMarkerForSlug(slug)
	if err != nil {
		t.Fatalf("readWorkspaceMarkerForSlug(%q) after migration: %v", slug, err)
	}
	wantWorkspace := filepath.Join(home, ".scion", "projects", slug)
	if workspacePath != wantWorkspace {
		t.Errorf("workspacePath = %q, want %q", workspacePath, wantWorkspace)
	}
	if got.ProjectID != marker.ProjectID {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, marker.ProjectID)
	}

	// The old path still resolves too, through the symlink left behind:
	// agents and tools holding the old path keep working.
	oldMarkerPath := filepath.Join(home, ".scion", "groves", slug, DotScion)
	if _, err := ReadProjectMarker(oldMarkerPath); err != nil {
		t.Errorf("marker no longer resolves through the legacy path: %v", err)
	}
}

// TestUpdateVersionedSetting_MigratesLegacyHubProjectKey checks that the
// in-place settings write-back still migrates a legacy hub key and keeps
// the file's comments.
func TestUpdateVersionedSetting_MigratesLegacyHubProjectKey(t *testing.T) {
	legacy := hubGroveIDRename[0].legacy
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("schema_version: \"1\"\n# hub section\nhub:\n  "+legacy+": p1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateVersionedSetting(dir, "hub.endpoint", "https://h"); err != nil {
		t.Fatalf("UpdateVersionedSetting: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, legacy+":") || !strings.Contains(got, "# hub section") {
		t.Errorf("unexpected file after update:\n%s", got)
	}
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
	if vs.Hub == nil || vs.Hub.ProjectID != "p1" || vs.Hub.Endpoint != "https://h" {
		t.Errorf("hub = %+v, want project_id p1 and endpoint https://h", vs.Hub)
	}
}

// TestUpdateVersionedSetting_LegacyHubProjectKeyParity runs the struct-path
// parity check (runStructParity) on a file that still has the legacy hub
// project key, for project_id, hub.project_id and every other key.
func TestUpdateVersionedSetting_LegacyHubProjectKeyParity(t *testing.T) {
	legacy := hubGroveIDRename[0].legacy
	runStructParity(t, []structParityBase{
		{name: "legacy-hub-key", file: "settings.yaml", content: "schema_version: \"1\"\n# hub\nhub:\n  " + legacy + ": p1 # legacy\n  endpoint: https://e\n"},
	})
}

// TestLoadSettings_GlobalLegacyHubKeyDoesNotLeak checks the legacy key name
// in the global file: it is migrated to hub.project_id when the global
// layer loads, and must not leak into a project either.
func TestLoadSettings_GlobalLegacyHubKeyDoesNotLeak(t *testing.T) {
	legacy := hubGroveIDRename[0].legacy
	globalDir := setupHubIDScopeHome(t, "  "+legacy+": \""+scopeGlobalHubID+"\"\n")
	scionDir, _ := setupHubIDScopeSplitProject(t, filepath.Dir(globalDir))

	s, err := LoadSettings(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.GetHubProjectID(); got != "" {
		t.Errorf("project GetHubProjectID() = %q, want empty (no leak from global)", got)
	}
	if s.ProjectID != scopeLocalID {
		t.Errorf("project ProjectID = %q, want %q", s.ProjectID, scopeLocalID)
	}

	// The global project still sees its own (migrated) value.
	g, err := LoadSettings(globalDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.GetHubProjectID(); got != scopeGlobalHubID {
		t.Errorf("global GetHubProjectID() = %q, want %q", got, scopeGlobalHubID)
	}
}

// --- The key migration under a settings lock its caller holds
// (ptone/scion#3919). ---

// runWithin fails the test when f does not return within d (a held-lock
// migration that tried to take the settings lock again would wait).
func runWithin(t *testing.T, d time.Duration, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("did not return within %s", d)
		return nil
	}
}

func writeLegacyHubKeySettings(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  grove_id: p1\n"), 0o644))
	return dir
}

func assertMigratedOnce(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	got := string(data)
	assert.NotContains(t, got, "grove_id")
	assert.Equal(t, 1, strings.Count(got, "project_id"), "one canonical key:\n%s", got)
	return got
}

// LoadModifySaveVersionedSettings holds the settings lock around its load,
// so the key migration in that load writes under the held lock: no wait,
// the file migrated, one canonical key.
func TestLoadModifySaveVersionedSettings_MigratesUnderHeldLock(t *testing.T) {
	t.Run("save", func(t *testing.T) {
		dir := writeLegacyHubKeySettings(t)
		err := runWithin(t, 5*time.Second, func() error {
			// Runs on runWithin's goroutine: report with assert and an
			// error, never require (FailNow would end that goroutine).
			return LoadModifySaveVersionedSettings(dir, func(vs *VersionedSettings) error {
				if !assert.NotNil(t, vs.Hub) {
					return errors.New("no hub settings loaded")
				}
				assert.Equal(t, "p1", vs.Hub.ProjectID)
				vs.ActiveProfile = "dev"
				return nil
			})
		})
		require.NoError(t, err)
		got := assertMigratedOnce(t, dir)
		assert.Contains(t, got, "active_profile: dev")
	})
	t.Run("skip save", func(t *testing.T) {
		dir := writeLegacyHubKeySettings(t)
		err := runWithin(t, 5*time.Second, func() error {
			return LoadModifySaveVersionedSettings(dir, func(*VersionedSettings) error { return ErrSkipSave })
		})
		require.NoError(t, err)
		assertMigratedOnce(t, dir)
	})
}

// setProjectTypeGit holds the settings lock for its edit, so the key
// migration it runs writes under that lock.
func TestSetProjectTypeGit_MigratesUnderHeldLock(t *testing.T) {
	dir := writeLegacyHubKeySettings(t)
	err := runWithin(t, 5*time.Second, func() error { return setProjectTypeGit(dir) })
	require.NoError(t, err)
	got := assertMigratedOnce(t, dir)
	assert.Contains(t, got, "project_type: git")
}

// --- config unset (ptone/scion#3919) and the key migration. ---

// The alias fallback starts from the file as the edit attempt left it, so
// a legacy hub.grove_id the attempt migrated stays migrated.
func TestUnsetSetting_AliasFallbackKeepsKeyMigration(t *testing.T) {
	content := "schema_version: \"1\"\nhub:\n  grove_id: p-legacy\nbase: &b\n  autohelp: false\n  mode: agent\ncli: *b\n"
	projectDir := setupSettingsViewTest(t, "", content)

	res, err := UnsetSetting(projectDir, "cli.autohelp", false)
	require.NoError(t, err)
	assert.True(t, res.Removed)
	got := readUnsetSettingsFile(t, projectDir)
	assert.Contains(t, got, "cli:\n    mode: agent\n")
	assert.NotContains(t, got, "grove_id")
	assert.Contains(t, got, "project_id: p-legacy")
}

// Legacy fixtures for the no-write switch tests (writes_on_read_test.go,
// ptone/scion#5057): the legacy literals live in this allow-listed
// compatibility test file.

// legacyHubProjectIDYAMLForTest is a settings-file fragment holding the legacy
// hub project ID key with value.
func legacyHubProjectIDYAMLForTest(value string) string {
	return "hub:\n  grove_id: \"" + value + "\"\n"
}

// legacyMarkerYAMLForTest is a .scion marker holding the legacy project keys.
func legacyMarkerYAMLForTest(id, name, slug string) string {
	return "grove-id: " + id + "\ngrove-name: " + name + "\ngrove-slug: " + slug + "\n"
}

// migrateLegacyHubProjectIDForTest runs the settings-file key migration of the
// legacy hub project ID key on path.
func migrateLegacyHubProjectIDForTest(path string, r Reporter) map[string]string {
	return migrateLegacyYAMLKeys(path, hubGroveIDRename, r)
}

// writeLegacyGlobalLayoutForTest gives home a legacy global layout: a real
// project directory and a leftover dangling back-link of an earlier
// migration.
func writeLegacyGlobalLayoutForTest(t *testing.T, home string) {
	t.Helper()
	writeFile(t, filepath.Join(home, "groves", "acme", "workspace.txt"), "hello")
	if err := os.Symlink(filepath.Join("..", "projects", "gone"), filepath.Join(home, "groves", "gone")); err != nil {
		t.Fatal(err)
	}
}
