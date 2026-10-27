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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/settingslockhook"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	yamlv3 "gopkg.in/yaml.v3"
)

// settingsHelperEnv selects a helper mode when this test binary is re-exec'd
// as a separate settings writer process (see TestMain and runSettingsHelper).
const settingsHelperEnv = "SCION_TEST_SETTINGS_HELPER"

// mustLockSettingsDir takes the settings lock for dir or fails the test.
func mustLockSettingsDir(t testing.TB, dir string) *SettingsLock {
	t.Helper()
	l, err := LockSettingsDir(dir)
	if err != nil {
		t.Fatalf("LockSettingsDir(%s): %v", dir, err)
	}
	return l
}

// withSettingsLock runs fn under the settings lock for dir.
func withSettingsLock(dir string, fn func(*SettingsLock) error) error {
	l, err := LockSettingsDir(dir)
	if err != nil {
		return err
	}
	defer l.Unlock()
	return fn(l)
}

// setSettingsLockWait shortens the settings lock wait for the test, through
// the one override (internal/settingslockhook).
func setSettingsLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	t.Cleanup(settingslockhook.Set(d))
}

// runSettingsHelper is the body of a re-exec'd helper process. The settings
// directory is in SCION_TEST_SETTINGS_DIR. Modes:
//
//   - update: SCION_TEST_SETTINGS_KEY is set n times (SCION_TEST_SETTINGS_N)
//     through UpdateSetting, to "v1".."vn". Before each write after the
//     first, the key must still hold this process's previous value: no other
//     process writes the key, so anything else means another writer's
//     read-modify-write overwrote it with stale data (a lost update). Exit 3.
//   - hold: takes the settings lock, prints "locked", and holds it until
//     stdin is closed.
//   - crash: sets default_template, and kills itself with SIGKILL after the
//     new file is written and synced but before it is renamed into place.
func runSettingsHelper(mode string) int {
	dir := os.Getenv("SCION_TEST_SETTINGS_DIR")
	switch mode {
	case "update":
		key := os.Getenv("SCION_TEST_SETTINGS_KEY")
		n, _ := strconv.Atoi(os.Getenv("SCION_TEST_SETTINGS_N"))
		for i := 1; i <= n; i++ {
			if i > 1 {
				want := fmt.Sprintf("v%d", i-1)
				got, err := readSettingValue(dir, key)
				if err != nil {
					fmt.Fprintf(os.Stderr, "read %s: %v\n", key, err)
					return 2
				}
				if got != want {
					fmt.Fprintf(os.Stderr, "LOST UPDATE: %s = %q before write %d, want %q\n", key, got, i, want)
					return 3
				}
			}
			if err := UpdateSetting(dir, key, fmt.Sprintf("v%d", i), false); err != nil {
				fmt.Fprintf(os.Stderr, "UpdateSetting %s: %v\n", key, err)
				return 2
			}
		}
		return 0
	case "shared-dirs":
		// Appends n shared directories through the whole-struct cycle
		// (LoadModifySaveVersionedSettings), as `scion shared-dir create`
		// does.
		prefix := os.Getenv("SCION_TEST_SETTINGS_KEY")
		n, _ := strconv.Atoi(os.Getenv("SCION_TEST_SETTINGS_N"))
		for i := 1; i <= n; i++ {
			err := LoadModifySaveVersionedSettings(dir, func(vs *VersionedSettings) error {
				vs.SharedDirs = append(vs.SharedDirs, api.SharedDir{Name: fmt.Sprintf("%s-%d", prefix, i)})
				return nil
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "LoadModifySaveVersionedSettings: %v\n", err)
				return 2
			}
		}
		return 0
	case "state":
		// Adds n synced agents to state.yaml through UpdateProjectState.
		prefix := os.Getenv("SCION_TEST_SETTINGS_KEY")
		n, _ := strconv.Atoi(os.Getenv("SCION_TEST_SETTINGS_N"))
		for i := 1; i <= n; i++ {
			err := UpdateProjectState(dir, func(st *ProjectState, _ error) error {
				st.SyncedAgents = append(st.SyncedAgents, fmt.Sprintf("%s-%d", prefix, i))
				return nil
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "UpdateProjectState: %v\n", err)
				return 2
			}
		}
		return 0
	case "hubid":
		// Persists the hub id in SCION_TEST_SETTINGS_KEY to <dir>/hub_id
		// and prints the id persistHubIDOnce returns. With
		// SCION_TEST_NO_LINKS=1 hard links fail, as on a file system
		// without them.
		if os.Getenv("SCION_TEST_NO_LINKS") == "1" {
			linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }
		}
		got, err := persistHubIDOnce(filepath.Join(dir, "hub_id"), os.Getenv("SCION_TEST_SETTINGS_KEY"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "persistHubIDOnce: %v\n", err)
			return 2
		}
		fmt.Println(got)
		return 0
	case "hold":
		l, err := LockSettingsDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "LockSettingsDir: %v\n", err)
			return 2
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		l.Unlock()
		return 0
	case "crash":
		beforeAtomicRename = func(tmpName, targetPath string) error {
			fmt.Println("tmp " + tmpName)
			// SIGKILL on unix, TerminateProcess on Windows.
			if p, err := os.FindProcess(os.Getpid()); err == nil {
				_ = p.Kill()
			}
			select {}
		}
		err := UpdateSetting(dir, "default_template", "crashed", false)
		fmt.Fprintf(os.Stderr, "crash helper survived the write: %v\n", err)
		return 4
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 2
}

// readSettingValue returns the string at key's v1 path in dir's settings
// file ("" when absent).
func readSettingValue(dir, key string) (string, error) {
	k, ok := versionedSettingKeys[key]
	if !ok {
		return "", fmt.Errorf("unknown key %s", key)
	}
	p := GetSettingsPath(dir)
	if p == "" {
		return "", errors.New("no settings file")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var node interface{}
	if err := yamlv3.Unmarshal(data, &node); err != nil {
		return "", err
	}
	for _, elem := range k.path {
		m, ok := node.(map[string]interface{})
		if !ok {
			return "", nil
		}
		node = m[elem]
	}
	s, _ := node.(string)
	return s, nil
}

// settingsHelper returns a command re-running this test binary as a helper
// process in mode for dir.
func settingsHelper(t *testing.T, mode, dir string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), settingsHelperEnv+"="+mode, "SCION_TEST_SETTINGS_DIR="+dir)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// startHolder starts a helper process holding the settings lock for dir and
// returns it once the lock is held, with the pipe that releases it on
// close.
func startHolder(t *testing.T, dir string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := settingsHelper(t, "hold", dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("holder did not take the lock: %q %v; stderr: %s", line, err, stderr.String())
	}
	return cmd, stdin
}

const lockTestSettings = "# operator notes\nschema_version: \"1\"\n# the profile\nactive_profile: local # inline\n"

func writeLockTestSettings(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Concurrent `scion config set` processes on one settings file never lose
// an update: each helper process owns one key and checks before every write
// that its previous value survived the other processes' writes.
func TestSettingsLock_MultiProcessUpdatesNotLost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"versioned", lockTestSettings},
		// The first write in some process auto-migrates the legacy file;
		// the others must see the migrated file, not migrate again.
		{"legacy auto-migration", "active_profile: local\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeLockTestSettings(t, tc.content)
			keys := []string{"default_template", "default_harness_config", "image_registry", "hub.endpoint"}
			const n = 25
			cmds := make([]*exec.Cmd, len(keys))
			outs := make([]*strings.Builder, len(keys))
			for i, key := range keys {
				cmds[i] = settingsHelper(t, "update", dir, "SCION_TEST_SETTINGS_KEY="+key, "SCION_TEST_SETTINGS_N="+strconv.Itoa(n))
				outs[i] = &strings.Builder{}
				cmds[i].Stdout, cmds[i].Stderr = outs[i], outs[i]
			}
			for _, c := range cmds {
				if err := c.Start(); err != nil {
					t.Fatal(err)
				}
			}
			for i, c := range cmds {
				if err := c.Wait(); err != nil {
					t.Errorf("helper for %s: %v\n%s", keys[i], err, outs[i].String())
				}
			}
			for _, key := range keys {
				got, err := readSettingValue(dir, key)
				if err != nil {
					t.Fatal(err)
				}
				if want := fmt.Sprintf("v%d", n); got != want {
					t.Errorf("%s = %q, want %q (an update was lost)", key, got, want)
				}
			}
			if _, err := LoadSingleFileVersioned(dir); err != nil {
				t.Errorf("settings no longer load: %v", err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
			if tc.content == lockTestSettings && !strings.Contains(string(data), "# operator notes") {
				t.Errorf("comments lost:\n%s", data)
			}
			backups, _ := filepath.Glob(filepath.Join(dir, "settings.yaml.bak*"))
			if wantBackups := map[bool]int{true: 0, false: 1}[tc.content == lockTestSettings]; len(backups) != wantBackups {
				t.Errorf("backups = %v, want %d (the legacy file must be migrated exactly once)", backups, wantBackups)
			}
		})
	}
}

// While another process holds the settings lock, a write waits for the
// bounded wait and then fails with ErrSettingsLockTimeout naming the lock
// file; once the holder releases it, the write succeeds.
func TestSettingsLock_TimesOutWhileOtherProcessHoldsIt(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	cmd, release := startHolder(t, dir)
	setSettingsLockWait(t, 300*time.Millisecond)

	start := time.Now()
	err := UpdateSetting(dir, "default_template", "x", false)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSettingsLockTimeout) {
		t.Fatalf("err = %v, want ErrSettingsLockTimeout", err)
	}
	if !strings.Contains(err.Error(), settingsLockFileName) || !strings.Contains(err.Error(), "try again") {
		t.Errorf("error does not name the lock file or say what to do: %v", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("waited %s, want about the 300ms bound", elapsed)
	}
	if got, _ := readSettingValue(dir, "default_template"); got != "" {
		t.Errorf("default_template written despite the timeout: %q", got)
	}

	_ = release.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := UpdateSetting(dir, "default_template", "x", false); err != nil {
		t.Fatalf("UpdateSetting after release: %v", err)
	}
}

// The kernel releases the lock when its holder dies, so a crashed writer
// never leaves it stuck. The lock file stays.
func TestSettingsLock_ReleasedWhenHolderIsKilled(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	cmd, _ := startHolder(t, dir)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	setSettingsLockWait(t, 2*time.Second)
	l := mustLockSettingsDir(t, dir)
	l.Unlock()
	if _, err := os.Stat(filepath.Join(dir, settingsLockFileName)); err != nil {
		t.Errorf("lock file removed: %v", err)
	}
}

// A nested settings write in the same process (a bug) fails after the
// bounded wait instead of deadlocking.
func TestSettingsLock_NestedWriteFailsInsteadOfDeadlocking(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	setSettingsLockWait(t, 200*time.Millisecond)
	l := mustLockSettingsDir(t, dir)
	defer l.Unlock()
	done := make(chan error, 1)
	go func() { done <- UpdateSetting(dir, "default_template", "x", false) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSettingsLockTimeout) || !strings.Contains(err.Error(), "nested") {
			t.Fatalf("err = %v, want ErrSettingsLockTimeout mentioning a nested write", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested settings write deadlocked")
	}
}

// A released lock, or the lock of another directory, cannot be used for a
// write; Unlock is idempotent.
func TestSettingsLock_WriteNeedsHeldLockForTheDirectory(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	other := writeLockTestSettings(t, lockTestSettings)
	path := filepath.Join(dir, "settings.yaml")

	l := mustLockSettingsDir(t, other)
	if err := writeSettingsFileAtomic(l, path, []byte("x: 1\n")); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Errorf("write under another directory's lock: err = %v", err)
	}
	l.Unlock()
	l.Unlock()

	l = mustLockSettingsDir(t, dir)
	l.Unlock()
	if err := writeSettingsFileAtomic(l, path, []byte("x: 1\n")); err == nil || !strings.Contains(err.Error(), "without the settings lock") {
		t.Errorf("write under a released lock: err = %v", err)
	}
	if err := writeSettingsFileAtomic(nil, path, []byte("x: 1\n")); err == nil {
		t.Error("write without a lock succeeded")
	}
	if data, _ := os.ReadFile(path); string(data) != lockTestSettings {
		t.Errorf("file changed:\n%s", data)
	}
}

// In a directory where the lock file cannot be created, the write still
// works in place (the existing read-only fallback), locked within this
// process only.
func TestSettingsLock_ReadOnlyDirectoryFallsBackToInProcessLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := writeLockTestSettings(t, lockTestSettings)
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })
	if err := UpdateSetting(dir, "default_template", "ro", false); err != nil {
		t.Fatalf("UpdateSetting in a read-only directory: %v", err)
	}
	if got, _ := readSettingValue(dir, "default_template"); got != "ro" {
		t.Errorf("default_template = %q, want ro", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, settingsLockFileName)); !os.IsNotExist(err) {
		t.Errorf("lock file in a read-only directory: %v", err)
	}
}

// An existing .settings.lock this user may not write (created by another
// user, for example under sudo) still locks across processes: the write
// waits for the holder and times out instead of silently dropping to the
// in-process lock.
func TestSettingsLock_UnwritableLockFileStillLocksAcrossProcesses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file for writing")
	}
	dir := writeLockTestSettings(t, lockTestSettings)
	lockPath := filepath.Join(dir, settingsLockFileName)
	if err := os.WriteFile(lockPath, nil, 0444); err != nil {
		t.Fatal(err)
	}
	cmd, release := startHolder(t, dir)
	setSettingsLockWait(t, 200*time.Millisecond)
	if err := UpdateSetting(dir, "default_template", "x", false); !errors.Is(err, ErrSettingsLockTimeout) {
		t.Fatalf("err = %v, want ErrSettingsLockTimeout (the lock must still be cross-process)", err)
	}
	_ = release.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := UpdateSetting(dir, "default_template", "x", false); err != nil {
		t.Fatalf("UpdateSetting after release: %v", err)
	}
	if got, _ := readSettingValue(dir, "default_template"); got != "x" {
		t.Errorf("default_template = %q", got)
	}
}

// A settings lock file created inside a git work tree never shows up as
// untracked: it is added to info/exclude once, and not at all when git
// already ignores it.
func TestSettingsLock_LockFileIsGitIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	t.Run("added to info/exclude", func(t *testing.T) {
		repo := t.TempDir()
		git(repo, "init", "-q")
		dir := filepath.Join(repo, ".scion")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(lockTestSettings), 0644); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := UpdateSetting(dir, "default_template", fmt.Sprintf("t%d", i), false); err != nil {
				t.Fatal(err)
			}
		}
		status := git(repo, "status", "--porcelain", "--untracked-files=all")
		if strings.Contains(status, settingsLockFileName) {
			t.Errorf("lock file shows up in git status:\n%s", status)
		}
		if !strings.Contains(status, ".scion/settings.yaml") {
			t.Errorf("settings.yaml should still be untracked (only the lock file is excluded):\n%s", status)
		}
		exclude, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(exclude), "\n"+settingsLockFileName+"\n"); n != 1 {
			t.Errorf("info/exclude has the rule %d times:\n%s", n, exclude)
		}
	})

	t.Run("already ignored", func(t *testing.T) {
		repo := t.TempDir()
		git(repo, "init", "-q")
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".scion/\n"), 0644); err != nil {
			t.Fatal(err)
		}
		excludePath := filepath.Join(repo, ".git", "info", "exclude")
		before, _ := os.ReadFile(excludePath)
		dir := filepath.Join(repo, ".scion")
		if err := UpdateSetting(dir, "default_template", "t", false); err != nil {
			t.Fatal(err)
		}
		if after, _ := os.ReadFile(excludePath); string(after) != string(before) {
			t.Errorf("info/exclude changed although .gitignore already covers the lock file:\n%s", after)
		}
	})
}

// A write that fails between writing the temporary file and renaming it
// leaves the old file byte-for-byte intact and no temporary file behind.
func TestSettingsWrite_FailureBeforeRenameKeepsOldFile(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	old := beforeAtomicRename
	beforeAtomicRename = func(string, string) error { return errors.New("simulated crash") }
	t.Cleanup(func() { beforeAtomicRename = old })

	err := UpdateSetting(dir, "default_template", "x", false)
	if err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatalf("err = %v, want the simulated failure", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml")); string(data) != lockTestSettings {
		t.Errorf("settings.yaml changed:\n%s", data)
	}
	assertOnlySettingsFiles(t, dir, "settings.yaml")
}

// A writer process killed after writing the temporary file and before the
// rename leaves the old file intact and loadable, and the lock free. The
// orphaned temporary file is not a settings file name, so loaders ignore it.
func TestSettingsWrite_KilledBeforeRenameKeepsOldFile(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	cmd := settingsHelper(t, "crash", dir)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("crash helper: %v, want killed by SIGKILL\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "tmp ") {
		t.Fatalf("crash helper did not reach the rename:\n%s", out.String())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml")); string(data) != lockTestSettings {
		t.Errorf("settings.yaml changed:\n%s", data)
	}
	if p := GetSettingsPath(dir); p != filepath.Join(dir, "settings.yaml") {
		t.Errorf("GetSettingsPath = %q", p)
	}
	if _, err := LoadSingleFileVersioned(dir); err != nil {
		t.Errorf("settings no longer load: %v", err)
	}
	setSettingsLockWait(t, 2*time.Second)
	if err := UpdateSetting(dir, "default_template", "after", false); err != nil {
		t.Fatalf("write after the crash: %v", err)
	}
}

// assertOnlySettingsFiles fails unless dir holds exactly names plus the
// settings lock file.
func assertOnlySettingsFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	want := map[string]bool{settingsLockFileName: true}
	for _, n := range names {
		want[n] = true
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("unexpected file %s", e.Name())
		}
	}
}

// A reader (readers take no lock) finds a loadable settings file at every
// step of a migration, and the backup holds the original.
func TestMigrateSettingsFile_ReaderAlwaysFindsSettingsFile(t *testing.T) {
	const legacyYAML = "active_profile: local\ndefault_template: claude\n"
	const legacyJSON = `{"active_profile": "local", "default_template": "claude"}`
	for _, tc := range []struct {
		name, file, content string
		symlink             bool
	}{
		{"settings.yaml", "settings.yaml", legacyYAML, false},
		{"settings.yml", "settings.yml", legacyYAML, false},
		{"settings.json", "settings.json", legacyJSON, false},
		{"symlinked settings.yaml", "settings.yaml", legacyYAML, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(dir, tc.file)
			linkTarget := ""
			if tc.symlink {
				linkTarget = filepath.Join(t.TempDir(), "real-settings.yaml")
				if err := os.WriteFile(linkTarget, []byte(tc.content), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(linkTarget, src); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(src, []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}

			var steps []string
			old := migrateStepHook
			migrateStepHook = func(step string) {
				steps = append(steps, step)
				p := GetSettingsPath(dir)
				if p == "" {
					t.Errorf("step %s: no settings file", step)
					return
				}
				if _, err := os.ReadFile(p); err != nil {
					t.Errorf("step %s: read %s: %v", step, p, err)
				}
			}
			t.Cleanup(func() { migrateStepHook = old })

			res, err := MigrateSettingsFile(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(steps) != "[backed-up written]" {
				t.Errorf("steps = %v", steps)
			}
			if p := GetSettingsPath(dir); p != filepath.Join(dir, "settings.yaml") {
				t.Errorf("settings file after migration = %q", p)
			}
			if tc.file != "settings.yaml" {
				if _, err := os.Lstat(src); !os.IsNotExist(err) {
					t.Errorf("original %s still present: %v", tc.file, err)
				}
			}
			vs, err := LoadSingleFileVersioned(dir)
			if err != nil {
				t.Fatal(err)
			}
			if vs.DefaultTemplate != "claude" {
				t.Errorf("default_template = %q", vs.DefaultTemplate)
			}
			if res.BackupPath != src+".bak" {
				t.Errorf("backup = %q", res.BackupPath)
			}
			backup, err := os.ReadFile(res.BackupPath)
			if err != nil || string(backup) != tc.content {
				t.Errorf("backup content = %q, %v; want the original", backup, err)
			}
			if tc.symlink {
				if fi, _ := os.Lstat(src); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
					t.Errorf("settings.yaml should now be a regular file")
				}
				if got, _ := os.Readlink(res.BackupPath); got != linkTarget {
					t.Errorf("backup link = %q, want %q", got, linkTarget)
				}
				if data, _ := os.ReadFile(linkTarget); string(data) != tc.content {
					t.Errorf("link target changed: %q", data)
				}
			}
		})
	}
}

// A migration whose write fails leaves the original untouched and no
// backup behind.
func TestMigrateSettingsFile_WriteFailureLeavesOriginal(t *testing.T) {
	const legacyYAML = "active_profile: local\n"
	dir := writeLockTestSettings(t, legacyYAML)
	old := beforeAtomicRename
	beforeAtomicRename = func(string, string) error { return errors.New("simulated crash") }
	t.Cleanup(func() { beforeAtomicRename = old })
	if _, err := MigrateSettingsFile(dir, false); err == nil {
		t.Fatal("migration succeeded despite the failed write")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml")); string(data) != legacyYAML {
		t.Errorf("settings.yaml changed:\n%s", data)
	}
	assertOnlySettingsFiles(t, dir, "settings.yaml")
}

// persistHubIDOnce keeps the first id written and replaces an empty file.
func TestPersistHubIDOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub_id")
	if got, err := persistHubIDOnce(path, "first"); err != nil || got != "first" {
		t.Fatalf("first = %q, %v", got, err)
	}
	if got, err := persistHubIDOnce(path, "second"); err != nil || got != "first" {
		t.Fatalf("second = %q, %v; want the first id kept", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := persistHubIDOnce(path, "third"); err != nil || got != "third" {
		t.Fatalf("after empty = %q, %v", got, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "third\n" {
		t.Errorf("content = %q", data)
	}
	assertOnlySettingsFiles(t, filepath.Dir(path), "hub_id")
}

// runHelpers runs one helper process per prefix in mode, n iterations each,
// concurrently, and fails the test if any of them fails.
func runHelpers(t *testing.T, mode, dir string, prefixes []string, n int) {
	t.Helper()
	cmds := make([]*exec.Cmd, len(prefixes))
	outs := make([]*strings.Builder, len(prefixes))
	for i, p := range prefixes {
		cmds[i] = settingsHelper(t, mode, dir, "SCION_TEST_SETTINGS_KEY="+p, "SCION_TEST_SETTINGS_N="+strconv.Itoa(n))
		outs[i] = &strings.Builder{}
		cmds[i].Stdout, cmds[i].Stderr = outs[i], outs[i]
	}
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("helper %s: %v\n%s", prefixes[i], err, outs[i].String())
		}
	}
}

// Concurrent whole-struct read-modify-write cycles in separate processes
// (scion shared-dir create) keep every process's change.
func TestSettingsLock_MultiProcessLoadModifySaveKeepsAll(t *testing.T) {
	dir := writeLockTestSettings(t, lockTestSettings)
	prefixes := []string{"a", "b", "c"}
	const n = 15
	runHelpers(t, "shared-dirs", dir, prefixes, n)
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range vs.SharedDirs {
		got[d.Name] = true
	}
	for _, p := range prefixes {
		for i := 1; i <= n; i++ {
			if name := fmt.Sprintf("%s-%d", p, i); !got[name] {
				t.Errorf("shared dir %s lost", name)
			}
		}
	}
}

// Concurrent state.yaml updates in separate processes (hub sync) keep every
// process's change.
func TestUpdateProjectState_MultiProcessKeepsAll(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	prefixes := []string{"a", "b", "c"}
	const n = 15
	runHelpers(t, "state", dir, prefixes, n)
	st, err := LoadProjectState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.SyncedAgents) != len(prefixes)*n {
		t.Errorf("synced agents = %d, want %d: %v", len(st.SyncedAgents), len(prefixes)*n, st.SyncedAgents)
	}
}

// UpdateProjectState: ErrSkipSave writes nothing; an unreadable state.yaml
// reaches modify as loadErr with an empty state.
func TestUpdateProjectState_SkipAndLoadError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.yaml")
	if err := UpdateProjectState(dir, func(*ProjectState, error) error { return ErrSkipSave }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("state.yaml written despite ErrSkipSave: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("synced_agents: [unclosed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var sawErr error
	err := UpdateProjectState(dir, func(st *ProjectState, loadErr error) error {
		sawErr = loadErr
		if len(st.SyncedAgents) != 0 {
			t.Errorf("state not empty on a load error: %+v", st)
		}
		st.LastSyncedAt = "now"
		return nil
	})
	if err != nil || sawErr == nil {
		t.Fatalf("err = %v, loadErr = %v", err, sawErr)
	}
	if st, err := LoadProjectState(dir); err != nil || st.LastSyncedAt != "now" {
		t.Errorf("state after replace = %+v, %v", st, err)
	}
}

// RewriteSettingsFile rewrites under the lock, skips on ErrSkipSave, and
// does nothing without a settings file.
func TestRewriteSettingsFile(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := RewriteSettingsFile(empty, func(string, []byte) ([]byte, error) { called = true; return nil, nil }); err != nil || called {
		t.Errorf("no settings file: err = %v, called = %v", err, called)
	}

	dir := writeLockTestSettings(t, lockTestSettings)
	path := filepath.Join(dir, "settings.yaml")
	if err := RewriteSettingsFile(dir, func(string, []byte) ([]byte, error) { return nil, ErrSkipSave }); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != lockTestSettings {
		t.Errorf("file changed on ErrSkipSave:\n%s", data)
	}
	err := RewriteSettingsFile(dir, func(p string, data []byte) ([]byte, error) {
		if p != path || string(data) != lockTestSettings {
			t.Errorf("rewrite got %s %q", p, data)
		}
		return append(data, []byte("default_template: x\n")...), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readSettingValue(dir, "default_template"); got != "x" {
		t.Errorf("default_template = %q", got)
	}
	assertOnlySettingsFiles(t, dir, "settings.yaml")
}

// Where hard links are not supported, persistHubIDOnce creates the file
// exclusively instead, and still keeps the first id.
func TestPersistHubIDOnce_NoHardLinks(t *testing.T) {
	old := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }
	t.Cleanup(func() { linkFile = old })
	path := filepath.Join(t.TempDir(), "hub_id")
	if got, err := persistHubIDOnce(path, "first"); err != nil || got != "first" {
		t.Fatalf("first = %q, %v", got, err)
	}
	if got, err := persistHubIDOnce(path, "second"); err != nil || got != "first" {
		t.Fatalf("second = %q, %v; want the first id kept", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	assertOnlySettingsFiles(t, filepath.Dir(path), "hub_id")
}

// Concurrent first runs in separate processes agree on one hub id and it is
// the one on disk, also where an earlier interrupted write left an empty
// file and where hard links are not supported.
func TestPersistHubIDOnce_MultiProcessAgreeOnOneID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		empty   bool
		noLinks bool
	}{
		{"empty file", true, false},
		{"no hard links", false, true},
		{"empty file, no hard links", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "hub_id")
			if tc.empty {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ids := []string{"id-a", "id-b", "id-c", "id-d"}
			cmds := make([]*exec.Cmd, len(ids))
			outs := make([]*strings.Builder, len(ids))
			errs := make([]*strings.Builder, len(ids))
			for i, id := range ids {
				env := []string{"SCION_TEST_SETTINGS_KEY=" + id}
				if tc.noLinks {
					env = append(env, "SCION_TEST_NO_LINKS=1")
				}
				cmds[i] = settingsHelper(t, "hubid", dir, env...)
				outs[i] = &strings.Builder{}
				errs[i] = &strings.Builder{}
				cmds[i].Stdout = outs[i]
				cmds[i].Stderr = errs[i]
			}
			for _, c := range cmds {
				if err := c.Start(); err != nil {
					t.Fatal(err)
				}
			}
			for i, c := range cmds {
				if err := c.Wait(); err != nil {
					t.Fatalf("helper %s: %v\nstdout: %s\nstderr: %s", ids[i], err, outs[i].String(), errs[i].String())
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			onDisk := strings.TrimSpace(string(data))
			for i := range ids {
				if got := strings.TrimSpace(outs[i].String()); got != onDisk {
					t.Errorf("helper %s returned %q, but %q is on disk", ids[i], got, onDisk)
				}
			}
		})
	}
}

// persistHubIDOnce runs under the settings lock for the file's directory:
// while it is held (here by this goroutine, so the call fails after the
// shortened wait), nothing is written.
func TestPersistHubIDOnce_HoldsSettingsLock(t *testing.T) {
	dir := t.TempDir()
	setSettingsLockWait(t, 100*time.Millisecond)
	l := mustLockSettingsDir(t, dir)
	defer l.Unlock()
	if _, err := persistHubIDOnce(filepath.Join(dir, "hub_id"), "x"); !errors.Is(err, ErrSettingsLockTimeout) {
		t.Fatalf("err = %v, want ErrSettingsLockTimeout", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hub_id")); !os.IsNotExist(err) {
		t.Errorf("hub_id written without the lock: %v", err)
	}
}

// A lock file this user cannot open even read-only, in a writable
// directory, is a hard error naming the file and what to check.
func TestSettingsLock_UnreadableLockFileIsAClearError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	dir := writeLockTestSettings(t, lockTestSettings)
	lockPath := filepath.Join(dir, settingsLockFileName)
	if err := os.WriteFile(lockPath, nil, 0); err != nil {
		t.Fatal(err)
	}
	err := UpdateSetting(dir, "default_template", "x", false)
	if err == nil || !strings.Contains(err.Error(), lockPath) || !strings.Contains(err.Error(), "owner and mode") {
		t.Fatalf("err = %v, want an error naming %s and its owner and mode", err, lockPath)
	}
	if got, _ := readSettingValue(dir, "default_template"); got != "" {
		t.Errorf("written despite the error: %q", got)
	}
}

// CreateSettingsFileIfMissing writes a missing file, never replaces an
// existing one, creates the target of a dangling symlink (keeping the link),
// and writes nothing when the lock cannot be taken.
func TestCreateSettingsFileIfMissing(t *testing.T) {
	const seed = "schema_version: \"1\"\n"
	t.Run("missing file is written", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.yaml")
		if err := CreateSettingsFileIfMissing(path, []byte(seed)); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(path); string(data) != seed {
			t.Errorf("content = %q", data)
		}
	})
	t.Run("existing file is left byte-identical", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.yaml")
		if err := os.WriteFile(path, []byte(lockTestSettings), 0644); err != nil {
			t.Fatal(err)
		}
		if err := CreateSettingsFileIfMissing(path, []byte(seed)); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(path); string(data) != lockTestSettings {
			t.Errorf("existing file replaced: %q", data)
		}
	})
	t.Run("dangling symlink: target created, link kept", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "real-settings.yaml")
		path := filepath.Join(dir, "settings.yaml")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if err := CreateSettingsFileIfMissing(path, []byte(seed)); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("link not kept: %v %v", fi, err)
		}
		if data, _ := os.ReadFile(target); string(data) != seed {
			t.Errorf("target content = %q", data)
		}
	})
	t.Run("held lock: timeout, nothing written", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.yaml")
		setSettingsLockWait(t, 100*time.Millisecond)
		l := mustLockSettingsDir(t, dir)
		defer l.Unlock()
		if err := CreateSettingsFileIfMissing(path, []byte(seed)); !errors.Is(err, ErrSettingsLockTimeout) {
			t.Fatalf("err = %v, want ErrSettingsLockTimeout", err)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("written without the lock: %v", err)
		}
	})
}
