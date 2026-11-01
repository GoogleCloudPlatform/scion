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

// These tests exercise the unix git-metadata bind checks (O_NOFOLLOW,
// symlinks); on other platforms checkedBindSource is an ErrUnsupported stub.
//go:build unix

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noopMount records nothing; the bind-source tests assert on the error, not the
// registered mounts.
func noopMount(string, string, bool, bool) {}

// TestCheckedBindSource exercises the single component-wise fail-closed helper
// every git-metadata bind source passes through: a symlink at the leaf or any
// intermediate component refuses, a non-directory intermediate refuses, the
// *Create modes create only through a verified chain (never through a symlink),
// and a dangling symlink at a create-first file is refused with nothing created.
func TestCheckedBindSource(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := func(target, name string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	// bindDirRequire: real dir present; missing absent.
	if err := os.Mkdir(filepath.Join(root, "realdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if src, present, err := checkedBindSource(root, bindDirRequire, "realdir"); err != nil || !present || src != filepath.Join(root, "realdir") {
		t.Errorf("real dir require: present=%v err=%v src=%q", present, err, src)
	}
	if _, present, err := checkedBindSource(root, bindDirRequire, "missing"); err != nil || present {
		t.Errorf("missing dir require must be absent, got present=%v err=%v", present, err)
	}

	// bindDirCreate: single and nested creation through verified components.
	if _, present, err := checkedBindSource(root, bindDirCreate, "made"); err != nil || !present {
		t.Errorf("dir create: present=%v err=%v", present, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "made")); err != nil || !fi.IsDir() {
		t.Errorf("dir create did not make a real directory: %v", err)
	}
	if _, present, err := checkedBindSource(root, bindDirCreate, "a", "b", "c"); err != nil || !present {
		t.Errorf("nested dir create: present=%v err=%v", present, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "a", "b", "c")); err != nil || !fi.IsDir() {
		t.Errorf("nested dir create did not make the chain: %v", err)
	}

	// bindFileRequire / bindFileCreate.
	if err := os.WriteFile(filepath.Join(root, "realfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, present, err := checkedBindSource(root, bindFileRequire, "realfile"); err != nil || !present {
		t.Errorf("file require real: present=%v err=%v", present, err)
	}
	if _, present, err := checkedBindSource(root, bindFileRequire, "nofile"); err != nil || present {
		t.Errorf("missing file require must be absent, got present=%v err=%v", present, err)
	}
	if _, present, err := checkedBindSource(root, bindFileCreate, "newfile"); err != nil || !present {
		t.Errorf("file create: present=%v err=%v", present, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "newfile")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("file create did not make a regular file: %v", err)
	}

	// A symlink leaf is refused in every mode.
	symlink(outside, "linkdir")
	for _, mode := range []bindSourceMode{bindDirRequire, bindDirCreate, bindFileRequire, bindFileCreate} {
		if _, _, err := checkedBindSource(root, mode, "linkdir"); err == nil {
			t.Errorf("a symlink leaf must be refused in mode %d", mode)
		}
	}

	// A symlinked INTERMEDIATE component is refused (no create-through-a-link).
	symlink(outside, "midlink")
	if _, _, err := checkedBindSource(root, bindDirCreate, "midlink", "leaf"); err == nil {
		t.Error("a symlinked intermediate component must be refused for dir-create")
	}
	if _, _, err := checkedBindSource(root, bindFileCreate, "midlink", "leaf"); err == nil {
		t.Error("a symlinked intermediate component must be refused for file-create")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("nothing must be created through the symlinked intermediate; outside has %d entries", len(entries))
	}

	// A DANGLING symlink at a create-first file is refused AND nothing is created.
	danglingTarget := filepath.Join(root, "never-created")
	symlink(danglingTarget, "danglink")
	if _, _, err := checkedBindSource(root, bindFileCreate, "danglink"); err == nil {
		t.Error("a dangling symlink at a create-first file must be refused")
	}
	if _, err := os.Lstat(danglingTarget); !os.IsNotExist(err) {
		t.Errorf("the dangling symlink target must NOT be created, stat err: %v", err)
	}

	// A non-directory intermediate is refused.
	if err := os.WriteFile(filepath.Join(root, "notadir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkedBindSource(root, bindDirCreate, "notadir", "leaf"); err == nil {
		t.Error("a non-directory intermediate component must be refused")
	}
}

// buildCheckedGitDir makes a minimal shared-base .git directory for exercising the
// narrow* bind-source helpers directly.
func buildCheckedGitDir(t *testing.T) (gitDir, outside string) {
	t.Helper()
	gitDir = filepath.Join(t.TempDir(), ".git")
	for _, d := range []string{"hooks", "info", "objects", filepath.Join("worktrees", "agent-1"), filepath.Join("modules", "sub")} {
		if err := os.MkdirAll(filepath.Join(gitDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A submodule admin dir is identified by a HEAD file.
	if err := os.WriteFile(filepath.Join(gitDir, "modules", "sub", "HEAD"), []byte("ref: refs/heads/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	return gitDir, outside
}

// TestNarrowSources_RefuseSymlink plants a symlink at each git-metadata bind
// source (by the function that resolves it) and asserts the start is refused.
// Coverage checklist by function: narrowGitAdminMounts,
// narrowSharerRegistryMounts, narrowObjectsInfo, narrowWorktreesDir,
// narrowModuleAdminDirs.
func TestNarrowSources_RefuseSymlink(t *testing.T) {
	type srcCase struct {
		name string
		rel  string // path under gitDir to replace with a symlink
		call func(gitDir string) error
	}
	admin := func(gitDir string) error { return narrowGitAdminMounts(noopMount, gitDir, "/repo-root/.git") }
	sharer := func(gitDir string) error { return narrowSharerRegistryMounts(noopMount, gitDir, "/repo-root/.git") }
	objinfo := func(gitDir string) error { return narrowObjectsInfo(noopMount, gitDir, "/repo-root/.git") }
	worktrees := func(gitDir string) error { return narrowWorktreesDir(noopMount, gitDir, "/repo-root/.git", "agent-1") }
	modules := func(gitDir string) error { return narrowModuleAdminDirs(noopMount, gitDir, "/repo-root/.git") }

	cases := []srcCase{
		{"hooks", "hooks", admin},
		{"info", "info", admin},
		{"config.worktree", "config.worktree", admin},
		{"scion-sharers", sharerRegistryDirName, sharer},
		{"objects/info", filepath.Join("objects", "info"), objinfo},
		{"objects (intermediate)", "objects", objinfo},
		{"worktrees", "worktrees", worktrees},
		{"worktrees/agent-1", filepath.Join("worktrees", "agent-1"), worktrees},
		{"worktrees/agent-1/gitdir", filepath.Join("worktrees", "agent-1", "gitdir"), worktrees},
		{"worktrees/agent-1/commondir", filepath.Join("worktrees", "agent-1", "commondir"), worktrees},
		{"worktrees/agent-1/config.worktree", filepath.Join("worktrees", "agent-1", "config.worktree"), worktrees},
		{"modules/sub/hooks", filepath.Join("modules", "sub", "hooks"), modules},
		{"modules/sub/info", filepath.Join("modules", "sub", "info"), modules},
		{"modules/sub/config", filepath.Join("modules", "sub", "config"), modules},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gitDir, outside := buildCheckedGitDir(t)
			src := filepath.Join(gitDir, tc.rel)
			_ = os.RemoveAll(src)
			if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, src); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := tc.call(gitDir); err == nil {
				t.Errorf("a symlinked %s must refuse the start, got nil", tc.name)
			}
		})
	}
}

// TestNarrowSources_FileCreateRefusesDanglingSymlink is the write-vector check:
// a create-first FILE source that is a symlink to a nonexistent target is refused
// and nothing is created at the target (no broker-uid host write through a link).
func TestNarrowSources_FileCreateRefusesDanglingSymlink(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		call func(gitDir string) error
	}{
		{"config.worktree", "config.worktree", func(g string) error { return narrowGitAdminMounts(noopMount, g, "/repo-root/.git") }},
		{"worktrees/agent-1/gitdir", filepath.Join("worktrees", "agent-1", "gitdir"), func(g string) error { return narrowWorktreesDir(noopMount, g, "/repo-root/.git", "agent-1") }},
		{"worktrees/agent-1/config.worktree", filepath.Join("worktrees", "agent-1", "config.worktree"), func(g string) error { return narrowWorktreesDir(noopMount, g, "/repo-root/.git", "agent-1") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gitDir, _ := buildCheckedGitDir(t)
			src := filepath.Join(gitDir, tc.rel)
			_ = os.RemoveAll(src)
			target := filepath.Join(t.TempDir(), "host-victim")
			if err := os.Symlink(target, src); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := tc.call(gitDir); err == nil {
				t.Errorf("a dangling-symlink %s must refuse the start, got nil", tc.name)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Errorf("the dangling symlink target must NOT be created for %s, stat err: %v", tc.name, err)
			}
		})
	}
}

// TestCheckedBindSource_CreateWindowComponentSwapRefused drives the post-create
// re-walk deterministically. The forward walk verifies each component one at a time,
// but O_NOFOLLOW (and Mkdir) guard only the leaf: an intermediate component verified
// as a real directory can be swapped for a symlink in the window before the leaf is
// created, which lands the new entry outside the repository. The test stands in for
// that concurrent swap with bindSourceRaceHook (fired after the walk, before the
// create) and asserts checkedBindSource fails closed and removes what it created.
// Deleting the reverifyChain call in either *Create branch makes this test fail.
func TestCheckedBindSource_CreateWindowComponentSwapRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode bindSourceMode
	}{
		{"bindFileCreate", bindFileCreate},
		{"bindDirCreate", bindDirCreate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitRoot := t.TempDir()
			// Verified real chain gitRoot/a/b; the leaf gitRoot/a/b/leaf is absent so
			// the *Create branch reaches its create (and the hook before it).
			mid := filepath.Join(gitRoot, "a", "b")
			if err := os.MkdirAll(mid, 0o755); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			victim := filepath.Join(outside, "leaf")

			fired := false
			bindSourceRaceHook = func(string) {
				if fired {
					return
				}
				fired = true
				// Swap the already-verified intermediate directory for a symlink to an
				// outside directory, exactly in the create window.
				if err := os.Remove(mid); err != nil {
					t.Fatalf("swap: remove verified dir: %v", err)
				}
				if err := os.Symlink(outside, mid); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			defer func() { bindSourceRaceHook = nil }()

			src, present, err := checkedBindSource(gitRoot, tc.mode, "a", "b", "leaf")
			if !fired {
				t.Fatal("race hook never fired; the create branch did not reach its create")
			}
			if err == nil {
				t.Errorf("a component swapped for a symlink in the create window must refuse the start, got src=%q present=%v", src, present)
			}
			if present {
				t.Errorf("present must be false on refusal, got true (src=%q)", src)
			}
			if _, lerr := os.Lstat(victim); !os.IsNotExist(lerr) {
				t.Errorf("the entry created through the swapped symlink must be removed, stat err: %v", lerr)
			}
		})
	}
}

// TestReadOnlyBaseWorktreeFiles_RefuseSymlink covers the production worktree
// read-only-base layout's own-worktree file binds: registerReadOnlyBaseGitMounts
// with a real worktree name binds worktrees/<name>/{gitdir,commondir,config.worktree}
// create-first through bindChecked(bindFileCreate). Each file is exercised with a
// symlink to an existing directory (refused) and a dangling symlink (refused, with
// nothing created at the target). Reverting that loop to unchecked
// fileExists+create code (which would stat/open through the link) makes this RED.
//
// It drives registerReadOnlyBaseGitMounts directly with the broker-trusted name
// "agent-1": routing through buildCommonRunArgs would mask the gitdir case, because a
// symlinked worktrees/agent-1/gitdir makes util.TrustedWorktreeAdminName fail to resolve
// the admin name, so the layout is never reached. The worktrees/agent-1 cases in
// TestNarrowSources_* go through narrowWorktreesDir, whose own-worktree
// branch this layout does not use.
func TestReadOnlyBaseWorktreeFiles_RefuseSymlink(t *testing.T) {
	for _, f := range []string{"gitdir", "commondir", "config.worktree"} {
		for _, variant := range []string{"symlink-to-dir", "dangling"} {
			t.Run(f+"/"+variant, func(t *testing.T) {
				repoRoot, _ := setupHubManagedBaseRepo(t, "agent-1")
				gitDir := filepath.Join(repoRoot, ".git")
				src := filepath.Join(gitDir, "worktrees", "agent-1", f)
				_ = os.RemoveAll(src) // gitdir exists as a regular file; the others do not

				var target string
				if variant == "symlink-to-dir" {
					target = filepath.Join(t.TempDir(), "outside")
					if err := os.MkdirAll(target, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					target = filepath.Join(t.TempDir(), "victim") // must not exist
				}
				if err := os.Symlink(target, src); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}

				err := registerReadOnlyBaseGitMounts(noopMount, gitDir, "/repo-root/.git", "agent-1")
				if err == nil || !strings.Contains(err.Error(), f) {
					t.Errorf("a symlinked worktrees/agent-1/%s (%s) must refuse the start naming %s, got: %v", f, variant, f, err)
				}
				if variant == "dangling" {
					if _, lerr := os.Lstat(target); !os.IsNotExist(lerr) {
						t.Errorf("the dangling symlink target must NOT be created for %s, stat err: %v", f, lerr)
					}
				}
			})
		}
	}
}
