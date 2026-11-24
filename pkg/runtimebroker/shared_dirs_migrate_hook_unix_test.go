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

package runtimebroker

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs/migrate"
)

func newTestLogBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), &buf
}

// TestMigrateLocalSharedDirs_SymlinksListedInDryRunAndReport verifies that
// symbolic links inside node-local shared directories are never copied, that a
// dry run changes nothing on disk while listing every symbolic link that will
// not be copied, and that the startup migration report names every skipped
// link (not just a refused count).
func TestMigrateLocalSharedDirs_SymlinksListedInDryRunAndReport(t *testing.T) {
	f := newSharedRootFixture(t, true)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	legacy := filepath.Join(globalDir, config.ProjectConfigsDir, "web__0f9e8d7c", "shared-dirs")
	if err := os.MkdirAll(filepath.Join(legacy, "scratchpad", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "scratchpad", "notes.txt"), []byte("from broker"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes.txt", filepath.Join(legacy, "scratchpad", "latest.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, filepath.Join(legacy, "scratchpad", "sub", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacy, "links-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing.txt", filepath.Join(legacy, "links-only", "dangling.md")); err != nil {
		t.Fatal(err)
	}

	// Dry run: changes nothing on disk and lists every symbolic link that will not be copied.
	dryRes, err := f.srv.migrateLocalSharedDirsOpts(context.Background(), migrate.Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	dryLogger, dryBuf := newTestLogBuffer()
	logSharedDirsMigrationResults(dryLogger, dryRes)

	if _, err := os.Stat(filepath.Join(f.root, "shared-dirs")); !os.IsNotExist(err) {
		t.Fatalf("dry run created shared-dirs on share (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "provision")); !os.IsNotExist(err) {
		t.Fatalf("dry run created provision state dir on share (err=%v)", err)
	}
	for _, want := range []string{
		"outcome=would-merge",
		"latest.md (symlink)",
		"sub/escape.txt (symlink)",
		"dangling.md (symlink)",
	} {
		if !strings.Contains(dryBuf.String(), want) {
			t.Fatalf("dry-run report lacks %q:\n%s", want, dryBuf.String())
		}
	}

	// Real startup migration: copies regular files, skips symlinks, and names every skipped link.
	res, err := f.srv.migrateLocalSharedDirs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runLogger, runBuf := newTestLogBuffer()
	logSharedDirsMigrationResults(runLogger, res)

	shared := filepath.Join(f.root, "shared-dirs")
	got, err := os.ReadFile(filepath.Join(shared, "scratchpad", "notes.txt"))
	if err != nil || string(got) != "from broker" {
		t.Fatalf("notes.txt = (%q, %v), want %q", got, err, "from broker")
	}
	for _, skipped := range []string{
		filepath.Join(shared, "scratchpad", "latest.md"),
		filepath.Join(shared, "scratchpad", "sub", "escape.txt"),
		filepath.Join(shared, "links-only", "dangling.md"),
	} {
		if _, err := os.Lstat(skipped); !os.IsNotExist(err) {
			t.Fatalf("symlink %s should not exist on target, got err=%v", skipped, err)
		}
	}
	for _, want := range []string{
		"outcome=merged",
		"level=WARN",
		"latest.md (symlink)",
		"sub/escape.txt (symlink)",
		"dangling.md (symlink)",
		".migrated-",
	} {
		if !strings.Contains(runBuf.String(), want) {
			t.Fatalf("migration report lacks %q:\n%s", want, runBuf.String())
		}
	}
}

// TestMigrateLocalSharedDirs_SymlinkedPathComponentsRefusedAndReported checks
// that a symbolic link at project-configs/<slug>__<id8>, at its shared-dirs
// directory, or at shared-dirs/<name> refuses that move without following the
// link and is named in the migration report.
func TestMigrateLocalSharedDirs_SymlinkedPathComponentsRefusedAndReported(t *testing.T) {
	t.Run("shared-dir leaf is symlink", func(t *testing.T) {
		f := newSharedRootFixture(t, true)
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "a.txt"), []byte("SECRET"), 0o600); err != nil {
			t.Fatal(err)
		}
		sdDir := filepath.Join(globalDir, config.ProjectConfigsDir, "web__0f9e8d7c", "shared-dirs")
		if err := os.MkdirAll(sdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(sdDir, "scratchpad")); err != nil {
			t.Fatal(err)
		}

		res, err := f.srv.migrateLocalSharedDirs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		logger, buf := newTestLogBuffer()
		logSharedDirsMigrationResults(logger, res)
		if !strings.Contains(buf.String(), "scratchpad") || !strings.Contains(buf.String(), "symlink") {
			t.Fatalf("report did not name symlinked shared-dir leaf:\n%s", buf.String())
		}
		if _, err := os.Lstat(filepath.Join(f.root, "shared-dirs", "scratchpad")); !os.IsNotExist(err) {
			t.Fatalf("target created for symlinked shared-dir leaf: %v", err)
		}
	})

	t.Run("shared-dirs parent is symlink", func(t *testing.T) {
		f := newSharedRootFixture(t, true)
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir() // empty outside dir: os.ReadDir would silently see 0 entries if followed
		cfgDir := filepath.Join(globalDir, config.ProjectConfigsDir, "web__0f9e8d7c")
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(cfgDir, "shared-dirs")); err != nil {
			t.Fatal(err)
		}

		res, err := f.srv.migrateLocalSharedDirs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		logger, buf := newTestLogBuffer()
		logSharedDirsMigrationResults(logger, res)
		if !strings.Contains(buf.String(), "shared-dirs") || !strings.Contains(buf.String(), "symlink") {
			t.Fatalf("report did not name symlinked shared-dirs directory:\n%s", buf.String())
		}
	})

	t.Run("project-configs entry is symlink", func(t *testing.T) {
		f := newSharedRootFixture(t, true)
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		root := filepath.Join(globalDir, config.ProjectConfigsDir)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "web__0f9e8d7c")); err != nil {
			t.Fatal(err)
		}

		res, err := f.srv.migrateLocalSharedDirs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		logger, buf := newTestLogBuffer()
		logSharedDirsMigrationResults(logger, res)
		if !strings.Contains(buf.String(), "web__0f9e8d7c") || !strings.Contains(buf.String(), "symlink") {
			t.Fatalf("report did not name symlinked project-configs entry:\n%s", buf.String())
		}
	})
}

// The broker start hook moves shared dirs it kept node-local
// (~/.scion/project-configs/<slug>__<id8>/shared-dirs/<name>) into the
// project's shared-dir leaves on the share: copy-only, never overwriting,
// the source kept as .migrated-<ts>; a second run moves nothing; projects
// not on the share are left alone.
func TestMigrateLocalSharedDirs(t *testing.T) {
	f := newSharedRootFixture(t, true)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(globalDir, config.ProjectConfigsDir, "web__0f9e8d7c", "shared-dirs")
	other := filepath.Join(globalDir, config.ProjectConfigsDir, "other__11111111", "shared-dirs", "scratchpad", "keep.txt")
	for path, content := range map[string]string{
		filepath.Join(legacy, "scratchpad", "notes.txt"): "from broker",
		filepath.Join(legacy, "scratchpad", "clash.txt"): "broker version",
		filepath.Join(legacy, "data", "d.txt"):           "data",
		other:                                            "other project",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(f.root, "shared-dirs")
	if err := os.MkdirAll(filepath.Join(shared, "scratchpad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "scratchpad", "clash.txt"), []byte("share version"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := f.srv.migrateLocalSharedDirs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Failed() {
			t.Fatalf("move failed: %+v", r)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(shared, "scratchpad", "notes.txt"): "from broker",
		filepath.Join(shared, "scratchpad", "clash.txt"): "share version",
		filepath.Join(shared, "data", "d.txt"):           "data",
		other:                                            "other project",
	} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s = (%q, %v), want %q", path, got, err, want)
		}
	}
	kept, _ := filepath.Glob(filepath.Join(legacy, "scratchpad.migrated-*", "clash.txt"))
	if len(kept) != 1 {
		t.Errorf("the source must be kept as .migrated-<ts> with its conflicting file, got %v", kept)
	}

	res, err = f.srv.migrateLocalSharedDirs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Moved() || r.Failed() {
			t.Errorf("second run: %+v, want no moves", r)
		}
	}
}

// The hook does nothing on a broker whose shared dirs are not on a shared
// Layout, and refuses (retrying at the next start) when the share is not
// mounted.
func TestMigrateLocalSharedDirs_Preconditions(t *testing.T) {
	setupTestScionEnv(t)
	srv, _ := newSharedLayoutServer(t, projectfs.Layout{Backend: projectfs.BackendLocal})
	if res, err := srv.migrateLocalSharedDirs(context.Background()); err != nil || res != nil {
		t.Fatalf("local layout: (%v, %v), want nothing", res, err)
	}
	f := newSharedRootFixture(t, true)
	if err := os.RemoveAll(f.layout.HostBase); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.migrateLocalSharedDirs(context.Background()); err == nil {
		t.Fatal("an unmounted share must refuse")
	}
}
