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

package substrate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// sandboxHomeDir is the throwaway directory TestMain points HOME and the
// XDG base directories at, for the lifetime of this test binary. Tests that
// need to check what did or didn't get written under it (see
// TestGoCachesEscapeSandboxHOME) read this instead of recomputing it.
var sandboxHomeDir string

// resolveRealGoCaches finds this machine's real GOCACHE, GOMODCACHE and
// GOPATH before TestMain redirects HOME and XDG_CACHE_HOME to a throwaway
// directory, and exports them explicitly so any child `go` process this
// test binary launches keeps writing to the real caches instead of
// deriving a fresh module cache and build cache under the throwaway
// directory. Go's module cache is written read-only, so a child `go`
// process that populated one there would survive TestMain's own cleanup.
// Each variable is taken from the environment if already set; otherwise
// this runs `go env` once for whichever ones are still missing. If that
// fails, it reports the failure to stderr and leaves the missing variables
// unset, so a child `go` process falls back to deriving its caches from the
// about-to-be-sandboxed HOME/XDG_CACHE_HOME — the pre-existing behaviour —
// rather than aborting the test binary.
func resolveRealGoCaches() {
	names := []string{"GOCACHE", "GOMODCACHE", "GOPATH"}
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	out, err := exec.Command("go", append([]string{"env"}, missing...)...).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: failed to resolve real %s via `go env`: %v; child go processes may derive caches from the sandboxed HOME instead\n", strings.Join(missing, ", "), err)
		return
	}
	values := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(values) != len(missing) {
		fmt.Fprintf(os.Stderr, "TestMain: unexpected `go env %s` output (%d line(s), want %d); child go processes may derive caches from the sandboxed HOME instead\n", strings.Join(missing, " "), len(values), len(missing))
		return
	}
	for i, name := range missing {
		if values[i] == "" {
			continue
		}
		_ = os.Setenv(name, values[i])
	}
}

// removeSandboxHome makes every file and directory under dir writable by
// its owner, then removes the tree, and returns whatever os.RemoveAll still
// couldn't clear. A real module cache directory (which resolveRealGoCaches
// exists to prevent from ever being written here, but this is the backstop)
// contains read-only files and directories by design, so a plain
// os.RemoveAll can silently leave them behind. filepath.Walk uses Lstat and
// never descends into a symlink, and the chmod pass never follows one either
// (a symlink's own Lstat mode is always 0o777 on Linux, so the
// mode&0o200==0 guard never fires for one) — a symlink inside dir that
// points outside it is removed as a link, but its target is left untouched.
// The caller decides whether and how to report a non-nil error; this
// function does not print anything itself.
func removeSandboxHome(dir string) error {
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // best effort; the RemoveAll below reports what's left.
		}
		if info.Mode()&0o200 == 0 {
			_ = os.Chmod(path, info.Mode()|0o200)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// disableGoTelemetry writes the go command's own telemetry mode file
// ("off") under configHome, the directory a child `go` process will use as
// os.UserConfigDir(). In the default "local" mode every `go` invocation
// writes counters under $XDG_CONFIG_HOME/go/telemetry and may start a
// detached helper that recreates that tree after the parent exits — and so
// after TestMain's cleanup, or t.TempDir's, has already removed it.
// GOTELEMETRY is not settable through the environment; the mode file is
// the supported switch (it is what `go telemetry off` writes).
func disableGoTelemetry(configHome string) error {
	dir := filepath.Join(configHome, "go", "telemetry")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "mode"), []byte("off"), 0o644)
}

// TestMain clears every Hub- and privilege-drop-related environment
// variable, and redirects HOME/XDG_*/SCION_WORKSPACE_PATH/the Hub token
// home/pkg/sciontool/log's own log file to one per-binary temp directory,
// before any test in this package runs — see cmd/sciontool/commands's
// TestMain (testmain_test.go) for the incidents this defends against. This
// package's own production code doesn't import pkg/sciontool/hub or
// resolve a fixed real home directory itself (every test here drives
// WithInitRunner/WithPrivilegeDropChecker with local fakes, never the real
// RunInit — see cmd/sciontool/commands's newSubstrateServeServer for where
// the real ones are wired instead), so the blast radius here is smaller.
// But handleBootstrap does read SCION_HOST_UID/GID out of the real process
// environment (set there by a test's own req.Env, via os.Setenv, exactly
// like a real bootstrap request), so a test that forgets to reset them
// could otherwise leak a previous test's values into a later one; server.go
// does call pkg/sciontool/log directly, which would otherwise lazily
// default to this container's real /home/scion/agent.log the same way
// cmd/sciontool/commands's incident 4 did; and the HOME/XDG/workspace
// redirection is cheap insurance against any future test in this package
// that does end up resolving a real path. resolveRealGoCaches runs before
// that redirection, so any future test that shells out to `go` keeps using
// the real GOCACHE/GOMODCACHE/GOPATH instead of writing fresh ones under
// the throwaway sandbox home.
func TestMain(m *testing.M) {
	for _, v := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_AUTH_TOKEN",
		"SCION_AGENT_ID", "SCION_AGENT_MODE",
		"SCION_HOST_UID", "SCION_HOST_GID", "SCION_KEEPID_UID",
	} {
		_ = os.Unsetenv(v)
	}

	resolveRealGoCaches()

	tmpHome, err := os.MkdirTemp("", "sciontool-substrate-test-home-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: failed to create sandbox home: %v\n", err)
		os.Exit(1)
	}
	sandboxHomeDir = tmpHome
	_ = os.Setenv("HOME", tmpHome)
	_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpHome, ".config"))
	if err := disableGoTelemetry(filepath.Join(tmpHome, ".config")); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: failed to disable go telemetry in the sandbox home: %v\n", err)
	}
	_ = os.Setenv("XDG_DATA_HOME", filepath.Join(tmpHome, ".local", "share"))
	_ = os.Setenv("XDG_CACHE_HOME", filepath.Join(tmpHome, ".cache"))
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, ".local", "state"))
	_ = os.Setenv("SCION_WORKSPACE_PATH", filepath.Join(tmpHome, "workspace"))
	log.SetLogPath(filepath.Join(tmpHome, "agent.log"))
	restoreTokenHome := hub.SetTokenHome(tmpHome)

	// Redirect ensurePrivateTmpDir's target at a throwaway directory this
	// (unprivileged) test binary itself owns, for the same reason HOME/XDG
	// are redirected above: production never reassigns privateRootTmpDir,
	// and the real path it defaults to ("/run/scion/tmp") lives under a
	// root-owned "/run" this test binary has no permission to create
	// anything under. Any individual test that specifically exercises the
	// real default (e.g. proving the production path name itself is right)
	// overrides and restores this var itself, the same way
	// withEnforcedHooksFixture does for enforcedHooksDir.
	privateRootTmpDir = filepath.Join(tmpHome, "run", "scion", "tmp")

	code := m.Run()
	restoreTokenHome()
	if err := removeSandboxHome(tmpHome); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: failed to remove sandbox home %s: %v\n", tmpHome, err)
	}
	os.Exit(code)
}

// TestGoCachesEscapeSandboxHOME proves resolveRealGoCaches works: a child
// `go` process launched under this test binary's own sandboxed
// HOME/XDG_CACHE_HOME still reports the real machine's GOCACHE and
// GOMODCACHE, not paths under the throwaway sandbox home TestMain created
// and will remove. It also checks the sandbox home directly, so a cache
// that got written there by some other path (not just this test's own `go
// env` call) is still caught.
func TestGoCachesEscapeSandboxHOME(t *testing.T) {
	if sandboxHomeDir == "" {
		t.Fatal("sandboxHomeDir was not set by TestMain")
	}

	// HOME and XDG_CONFIG_HOME stay exactly what TestMain set them to:
	// GOMODCACHE/GOPATH default to paths under $HOME, and TestMain's
	// disableGoTelemetry seed under $XDG_CONFIG_HOME is what keeps this
	// child's own telemetry out of the sandbox home (checked below).
	cmd := exec.Command("go", "env", "GOCACHE", "GOMODCACHE")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOCACHE GOMODCACHE: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("go env GOCACHE GOMODCACHE: got %d line(s), want 2: %q", len(lines), out)
	}
	for name, path := range map[string]string{"GOCACHE": lines[0], "GOMODCACHE": lines[1]} {
		if path == "" {
			t.Errorf("child go process reports an empty %s", name)
			continue
		}
		if strings.HasPrefix(path, sandboxHomeDir) {
			t.Errorf("child go process's %s = %q is inside the sandbox home %q; the real cache was not exported to it", name, path, sandboxHomeDir)
		}
	}

	for _, rel := range []string{filepath.Join(".cache", "go-build"), filepath.Join("go", "pkg", "mod"), filepath.Join(".config", "go", "telemetry", "local")} {
		if _, err := os.Stat(filepath.Join(sandboxHomeDir, rel)); err == nil {
			t.Errorf("sandbox home contains %s after a child go invocation; the child go process wrote there instead of outside the sandbox", rel)
		}
	}
}

// TestRemoveSandboxHome_RemovesReadOnlyModuleCacheShapedTree pins
// removeSandboxHome's contract: a tree shaped like a Go module cache
// (read-only directories holding read-only files, which a plain
// os.RemoveAll cannot unlink) is removed completely.
func TestRemoveSandboxHome_RemovesReadOnlyModuleCacheShapedTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sandbox")
	modDir := filepath.Join(root, "go", "pkg", "mod", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/m\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{modDir, filepath.Dir(modDir)} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	// If removeSandboxHome fails, restore write access so t.TempDir's own
	// cleanup can still remove the tree.
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(p, info.Mode()|0o200)
			}
			return nil
		})
	})

	if err := removeSandboxHome(root); err != nil {
		t.Errorf("removeSandboxHome(%s) = %v, want nil", root, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("removeSandboxHome left %s behind (Lstat err = %v); read-only module-cache files must not survive cleanup", root, err)
	}
}

// TestRemoveSandboxHome_ReportsErrorWhenRemovalFails pins the other half of
// removeSandboxHome's contract: when the chmod pass cannot make a directory
// removable — here, a directory with no read or execute bit, which the
// mode|0o200 chmod pass only ever adds a write bit to, never read or
// execute — the underlying os.RemoveAll failure is returned rather than
// swallowed.
func TestRemoveSandboxHome_ReportsErrorWhenRemovalFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root bypasses the permission check this test depends on")
	}
	root := filepath.Join(t.TempDir(), "sandbox")
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "file"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No read or execute bit: removeSandboxHome's chmod pass only ORs in
	// 0o200 (owner-write), so it cannot restore the read+execute access
	// os.RemoveAll needs to list and remove blocked's own contents.
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(blocked, 0o755)
	})

	err := removeSandboxHome(root)
	if err == nil {
		t.Fatal("removeSandboxHome(root) = nil, want a non-nil error because blocked could not be emptied")
	}
	if _, statErr := os.Lstat(blocked); statErr != nil {
		t.Errorf("Lstat(%s) = %v after a failed removeSandboxHome; want the blocked directory to still exist, matching the reported error", blocked, statErr)
	}
}

// TestRemoveSandboxHome_DoesNotTouchSymlinkTargetsOutsideTree pins the
// symlink-safety property removeSandboxHome depends on filepath.Walk and
// os.RemoveAll for: a symlink inside dir that points at a file outside it
// is itself removed, but the file it points at is never chmod'd, its
// content never touched, and it still exists afterward. filepath.Walk's own
// Lstat-based info always reports 0o777 for a symlink regardless of its
// target, so the write-bit guard never fires for one; a change that re-stats
// the path instead (following the link) would defeat that.
func TestRemoveSandboxHome_DoesNotTouchSymlinkTargetsOutsideTree(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "keep-me")
	if err := os.WriteFile(outsideFile, []byte("do not touch"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outsideFile, 0o600) })
	wantMode := os.FileMode(0o400)
	if fi, err := os.Stat(outsideFile); err != nil {
		t.Fatal(err)
	} else {
		wantMode = fi.Mode()
	}

	root := filepath.Join(t.TempDir(), "sandbox")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outsideFile, escape); err != nil {
		t.Fatal(err)
	}

	if err := removeSandboxHome(root); err != nil {
		t.Fatalf("removeSandboxHome(%s) = %v, want nil", root, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("removeSandboxHome left %s behind (Lstat err = %v)", root, err)
	}

	fi, err := os.Stat(outsideFile)
	if err != nil {
		t.Fatalf("the symlink target %s was removed or is unreachable: %v", outsideFile, err)
	}
	if fi.Mode() != wantMode {
		t.Errorf("the symlink target %s has mode %v, want unchanged %v; removeSandboxHome must not chmod through a symlink", outsideFile, fi.Mode(), wantMode)
	}
	raw, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("reading the symlink target %s: %v", outsideFile, err)
	}
	if string(raw) != "do not touch" {
		t.Errorf("the symlink target %s content changed to %q", outsideFile, raw)
	}
}
