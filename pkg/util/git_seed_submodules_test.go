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

// These tests run git and create symlinks; the seed check reads files with a
// unix-only no-follow open.
//go:build unix

package util

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// subFixture is a superproject with a submodule libs/a added on main, so git
// itself creates <commonDir>/modules/libs/a, the gitlink and the gitfile.
type subFixture struct {
	t                *testing.T
	root, super, sub string
	commonDir        string
	ws, wsAdmin      string // set by addWorktree
}

func (f *subFixture) git(dir string, args ...string) {
	f.t.Helper()
	full := append([]string{"-c", "protocol.file.allow=always"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "SCION_HOST_UID=",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func newSubFixture(t *testing.T) *subFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &subFixture{t: t, root: root, super: filepath.Join(root, "super"), sub: filepath.Join(root, "sub")}
	f.git(root, "init", "-q", "-b", "main", f.sub)
	f.write(filepath.Join(f.sub, "f"), "x\n")
	f.git(f.sub, "add", "f")
	f.git(f.sub, "commit", "-q", "-m", "s")
	f.git(root, "init", "-q", "-b", "main", f.super)
	f.git(f.super, "submodule", "add", "-q", f.sub, "libs/a")
	f.git(f.super, "commit", "-q", "-m", "add")
	f.commonDir = filepath.Join(f.super, ".git")
	return f
}

// addWorktree adds a linked worktree ws on branch agent-x and initialises its
// submodule, so git creates <commonDir>/worktrees/ws/modules/libs/a.
func (f *subFixture) addWorktree() {
	f.t.Helper()
	f.ws = filepath.Join(f.root, "ws")
	f.git(f.super, "worktree", "add", "-q", "-b", "agent-x", f.ws)
	f.git(f.ws, "submodule", "update", "--init", "-q")
	name, ok := TrustedWorktreeAdminName(f.commonDir, f.ws)
	if !ok {
		f.t.Fatal("fixture: worktree admin name does not resolve")
	}
	f.wsAdmin = name
}

func (f *subFixture) write(path, body string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// adminDir makes a hand-built admin dir shaped like one git creates (HEAD,
// objects/, refs/), so git treats it as a repository and the cross-check
// compares against git's real reading of its config.
func (f *subFixture) adminDir(path string) {
	f.t.Helper()
	f.write(filepath.Join(path, "HEAD"), "ref: refs/heads/main\n")
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(path, d), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *subFixture) appendTo(path, body string) {
	f.t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := fh.WriteString(body); err != nil {
		f.t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// setWorktree replaces core.worktree in an admin dir's config.
func (f *subFixture) setWorktree(adminDir, value string) {
	f.t.Helper()
	cmd := exec.Command("git", "config", "--file", filepath.Join(adminDir, "config"), "core.worktree", value)
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("set core.worktree: %v\n%s", err, out)
	}
}

func (f *subFixture) mainModule() string { return filepath.Join(f.commonDir, "modules", "libs", "a") }

func (f *subFixture) ownModule() string {
	return filepath.Join(f.commonDir, "worktrees", f.wsAdmin, "modules", "libs", "a")
}

func (f *subFixture) checkMain() error {
	return CheckSeedSubmodules(context.Background(), f.commonDir, f.super, "")
}

func (f *subFixture) checkWorktree() error {
	return CheckSeedSubmodules(context.Background(), f.commonDir, f.ws, f.wsAdmin)
}

// mustRefuse requires a *RepoConfigRefusedError (optionally with the given key
// and a layer containing layerPart) and that marker is absent from it.
func mustRefuse(t *testing.T, err error, key, layerPart, marker string) *RepoConfigRefusedError {
	t.Helper()
	var r *RepoConfigRefusedError
	if !errors.As(err, &r) {
		t.Fatalf("error = %v, want *RepoConfigRefusedError", err)
	}
	if !r.Seeded {
		t.Error("refusal not marked Seeded")
	}
	if key != "" && r.Key != key {
		t.Errorf("key = %q, want %q", r.Key, key)
	}
	if layerPart != "" && !strings.Contains(r.Layer, layerPart) {
		t.Errorf("layer = %q, want it to contain %q", r.Layer, layerPart)
	}
	if marker != "" && (strings.Contains(r.Key, marker) || strings.Contains(r.Error(), marker)) {
		t.Errorf("refusal leaks %q: %s", marker, r.Error())
	}
	return r
}

// assertStructuralRemedy checks that a structural refusal (a <placeholder>
// key) gives the submodule remedy and points at no config file or setting.
func assertStructuralRemedy(t *testing.T, r *RepoConfigRefusedError) {
	t.Helper()
	msg := r.Error()
	if !strings.Contains(msg, "remove or repair the submodule directory or git link it names") {
		t.Errorf("structural refusal lacks the submodule remedy: %s", msg)
	}
	for _, bad := range []string{"config file", ".git/config", "global"} {
		if strings.Contains(msg, bad) {
			t.Errorf("structural refusal mentions %q: %s", bad, msg)
		}
	}
}

func TestCheckSeedSubmodules_CleanMainPasses(t *testing.T) {
	f := newSubFixture(t)
	if err := f.checkMain(); err != nil {
		t.Fatalf("clean submodule refused: %v", err)
	}
}

// Admin dir and core.worktree rules.

func TestCheckSeedSubmodules_R1MismatchRefuses(t *testing.T) {
	f := newSubFixture(t)
	other := filepath.Join(f.super, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	f.setWorktree(f.mainModule(), other)
	_ = mustRefuse(t, f.checkMain(), "core.worktree", "modules/libs/a/config", "")
}

func TestCheckSeedSubmodules_R1AbsentTargetRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.setWorktree(f.mainModule(), filepath.Join(f.super, "missing"))
	_ = mustRefuse(t, f.checkMain(), "core.worktree", "modules/libs/a/config", "")
}

func TestCheckSeedSubmodules_R2StaleAbsentTargetPasses(t *testing.T) {
	f := newSubFixture(t)
	// git rm removes the gitlink and the work tree; the admin dir stays, naming
	// the old (now absent) core.worktree.
	f.git(f.super, "rm", "-q", "libs/a")
	if _, err := os.Stat(f.mainModule()); err != nil {
		t.Fatalf("fixture: admin dir gone: %v", err)
	}
	if err := f.checkMain(); err != nil {
		t.Fatalf("stale admin dir refused: %v", err)
	}
}

func TestCheckSeedSubmodules_GitfileOutsideModulesRefuses(t *testing.T) {
	f := newSubFixture(t)
	fake := filepath.Join(f.root, "fake-admin")
	f.adminDir(fake)
	f.write(filepath.Join(f.super, "libs", "a", ".git"), "gitdir: "+fake+"\n")
	assertStructuralRemedy(t, mustRefuse(t, f.checkMain(), "<gitlink>", "submodule libs/a", ""))
}

// An absolute or empty gitlink path (only a hand-crafted index carries one)
// is refused under a fixed label; the path itself is never printed.
func TestResolveSeedGitlink_AbsoluteOrEmptyPathNotPrinted(t *testing.T) {
	for _, p := range []string{"/x/y", ""} {
		_, _, err := resolveSeedGitlink(t.TempDir(), p, nil)
		r := mustRefuse(t, err, "<gitlink>", "", "")
		if r.Layer != "submodule <absolute or empty path>" {
			t.Errorf("path %q: layer = %q, want the fixed label", p, r.Layer)
		}
		if p != "" && strings.Contains(r.Error(), p) {
			t.Errorf("refusal prints the path %q: %s", p, r.Error())
		}
		assertStructuralRemedy(t, r)
	}
}

func TestCheckSeedSubmodules_EmbeddedGitDirRefuses(t *testing.T) {
	f := newSubFixture(t)
	dotGit := filepath.Join(f.super, "libs", "a", ".git")
	if err := os.Remove(dotGit); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dotGit, 0o755); err != nil {
		t.Fatal(err)
	}
	r := mustRefuse(t, f.checkMain(), "<gitlink>", "submodule libs/a", "")
	if !strings.Contains(r.Reason, "absorbgitdirs") {
		t.Errorf("reason lacks the remedy: %q", r.Reason)
	}
}

func TestCheckSeedSubmodules_SymlinkedModuleDirRefuses(t *testing.T) {
	f := newSubFixture(t)
	elsewhere := filepath.Join(f.root, "elsewhere")
	f.adminDir(elsewhere)
	if err := os.Symlink(elsewhere, filepath.Join(f.commonDir, "modules", "x")); err != nil {
		t.Fatal(err)
	}
	r := mustRefuse(t, f.checkMain(), "<modules>", "", "")
	if r.Layer != "modules" {
		t.Errorf("layer = %q, want modules", r.Layer)
	}
	assertStructuralRemedy(t, r)
}

func TestCheckSeedSubmodules_WorktreeDotGitComponentRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.git(f.super, "rm", "-q", "libs/a") // unreferenced: the R2 rules apply
	for _, v := range []string{filepath.Join(f.super, ".GIT", "x"), filepath.Join(f.super, "d", ".Git. ", "x")} {
		f.setWorktree(f.mainModule(), v)
		_ = mustRefuse(t, f.checkMain(), "core.worktree", "modules/libs/a/config", "")
	}
}

func TestCheckSeedSubmodules_WorktreeIntoCommonDirRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.git(f.super, "rm", "-q", "libs/a")
	f.setWorktree(f.mainModule(), filepath.Join(f.commonDir, "hooks"))
	_ = mustRefuse(t, f.checkMain(), "core.worktree", "modules/libs/a/config", "")
}

// Nested modules, a config.worktree layer, includes.

func TestCheckSeedSubmodules_NestedAndLayerKeysRefuse(t *testing.T) {
	t.Run("nested module config", func(t *testing.T) {
		f := newSubFixture(t)
		nested := filepath.Join(f.mainModule(), "modules", "b")
		f.adminDir(nested)
		f.write(filepath.Join(nested, "config"), "[core]\n\tfsmonitor = /tmp/submarker-v1\n")
		_ = mustRefuse(t, f.checkMain(), "core.fsmonitor", "modules/libs/a/modules/b/config", "submarker-v1")
	})
	t.Run("config.worktree layer", func(t *testing.T) {
		f := newSubFixture(t)
		c := filepath.Join(f.commonDir, "modules", "c")
		f.adminDir(c)
		f.write(filepath.Join(c, "config.worktree"), "[core]\n\tsshCommand = ssh -i /tmp/submarker-v1\n")
		r := mustRefuse(t, f.checkMain(), "core.sshcommand", "modules/c/config.worktree", "submarker-v1")
		// A submodule-layer key gets the layer remedy and names its layer by
		// label, never by a host path.
		msg := r.Error()
		if !strings.Contains(msg, "remove it from the git config file that sets it (in modules/c/config.worktree)") {
			t.Errorf("submodule-layer refusal lacks the layer remedy: %s", msg)
		}
		if strings.Contains(msg, f.root) {
			t.Errorf("submodule-layer refusal prints the host path %q: %s", f.root, msg)
		}
	})
	t.Run("include in module config", func(t *testing.T) {
		f := newSubFixture(t)
		f.appendTo(filepath.Join(f.mainModule(), "config"), "[include]\n\tpath = /tmp/submarker-v1\n")
		r := mustRefuse(t, f.checkMain(), "include.path", "modules/libs/a/config", "submarker-v1")
		if r.Reason != "an include directive" {
			t.Errorf("reason = %q", r.Reason)
		}
	})
	t.Run("clean nested tree passes", func(t *testing.T) {
		f := newSubFixture(t)
		nested := filepath.Join(f.mainModule(), "modules", "b")
		f.adminDir(nested)
		f.write(filepath.Join(nested, "config"), "[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
		if err := f.checkMain(); err != nil {
			t.Fatalf("clean nested module refused: %v", err)
		}
	})
}

// Linked worktrees and gitlink resolution.

// 1: a linked worktree with a git-initialised submodule passes.
func TestCheckSeedSubmodules_LinkedWorktreeSubmodulePasses(t *testing.T) {
	f := newSubFixture(t)
	f.addWorktree()
	if _, err := os.Stat(filepath.Join(f.ownModule(), "HEAD")); err != nil {
		t.Fatalf("fixture: git did not create the own module admin dir: %v", err)
	}
	if err := f.checkWorktree(); err != nil {
		t.Fatalf("linked worktree submodule refused: %v", err)
	}
}

// 2: a refused key in <own>/modules/<name>/config refuses.
func TestCheckSeedSubmodules_OwnModuleKeyRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.addWorktree()
	f.appendTo(filepath.Join(f.ownModule(), "config"), "[core]\n\thooksPath = /tmp/submarker-v2\n")
	_ = mustRefuse(t, f.checkWorktree(), "core.hookspath", "worktree modules/libs/a/config", "submarker-v2")
}

// 3: a gitfile to a sibling worktree's modules refuses.
func TestCheckSeedSubmodules_SiblingWorktreeModuleRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.addWorktree()
	ws2 := filepath.Join(f.root, "ws2")
	f.git(f.super, "worktree", "add", "-q", "-b", "agent-y", ws2)
	f.git(ws2, "submodule", "update", "--init", "-q")
	name2, ok := TrustedWorktreeAdminName(f.commonDir, ws2)
	if !ok {
		t.Fatal("fixture: second worktree name")
	}
	sibling := filepath.Join(f.commonDir, "worktrees", name2, "modules", "libs", "a")
	f.write(filepath.Join(f.ws, "libs", "a", ".git"), "gitdir: "+sibling+"\n")
	_ = mustRefuse(t, f.checkWorktree(), "<gitlink>", "submodule libs/a", "")
}

// 4: a linked-worktree gitfile to <commonDir>/modules/<name>, whose
// core.worktree names the main checkout's path, refuses (R1 alias).
func TestCheckSeedSubmodules_CrossCheckoutAliasRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.addWorktree()
	f.write(filepath.Join(f.ws, "libs", "a", ".git"), "gitdir: "+f.mainModule()+"\n")
	_ = mustRefuse(t, f.checkWorktree(), "core.worktree", "modules/libs/a/config", "")
}

// 5: two gitlinks resolving to one admin dir refuse.
func TestCheckSeedSubmodules_AliasedGitlinksRefuse(t *testing.T) {
	f := newSubFixture(t)
	f.git(f.super, "submodule", "add", "-q", f.sub, "libs/b")
	f.git(f.super, "commit", "-q", "-m", "add b")
	f.write(filepath.Join(f.super, "libs", "b", ".git"), "gitdir: "+f.mainModule()+"\n")
	_ = mustRefuse(t, f.checkMain(), "<gitlink>", "submodule libs/", "")
}

// 6: a gitfile target holding commondir refuses (D_main and D_own).
func TestCheckSeedSubmodules_CommondirInTargetRefuses(t *testing.T) {
	t.Run("main", func(t *testing.T) {
		f := newSubFixture(t)
		f.write(filepath.Join(f.mainModule(), "commondir"), "../..\n")
		_ = mustRefuse(t, f.checkMain(), "<gitlink>", "submodule libs/a", "")
	})
	t.Run("own", func(t *testing.T) {
		f := newSubFixture(t)
		f.addWorktree()
		f.write(filepath.Join(f.ownModule(), "commondir"), "../..\n")
		_ = mustRefuse(t, f.checkWorktree(), "<gitlink>", "submodule libs/a", "")
	})
}

// 7: a gitlink path with a symlinked leading component refuses.
func TestCheckSeedSubmodules_SymlinkedGitlinkPathRefuses(t *testing.T) {
	f := newSubFixture(t)
	libs := filepath.Join(f.super, "libs")
	real := filepath.Join(f.super, "real-libs")
	if err := os.Rename(libs, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, libs); err != nil {
		t.Fatal(err)
	}
	_ = mustRefuse(t, f.checkMain(), "<gitlink>", "submodule libs/a", "")
}

// 8: an unreferenced stale <own>/modules dir with an absent target inside the
// workspace passes; with a target outside the workspace it refuses.
func TestCheckSeedSubmodules_OwnStaleModule(t *testing.T) {
	setup := func(t *testing.T, target func(f *subFixture) string) *subFixture {
		f := newSubFixture(t)
		f.addWorktree()
		stale := filepath.Join(f.commonDir, "worktrees", f.wsAdmin, "modules", "stale")
		f.adminDir(stale)
		f.write(filepath.Join(stale, "config"), "[core]\n\trepositoryformatversion = 0\n")
		f.setWorktree(stale, target(f))
		return f
	}
	t.Run("absent target inside workspace passes", func(t *testing.T) {
		f := setup(t, func(f *subFixture) string { return filepath.Join(f.ws, "gone") })
		if err := f.checkWorktree(); err != nil {
			t.Fatalf("stale own module refused: %v", err)
		}
	})
	t.Run("target outside workspace refuses", func(t *testing.T) {
		f := setup(t, func(f *subFixture) string { return filepath.Join(f.root, "elsewhere", "gone") })
		_ = mustRefuse(t, f.checkWorktree(), "core.worktree", "worktree modules/stale/config", "")
	})
}

// 9: the gitlinks come from the agent's own index. libs/b exists only on the
// worktree's branch; a gitlink-resolution violation on it refuses, which a read of the
// main index (no libs/b) would miss. The twin with a valid gitfile passes.
func TestCheckSeedSubmodules_ReadsTheAgentIndex(t *testing.T) {
	setup := func(t *testing.T) *subFixture {
		f := newSubFixture(t)
		f.addWorktree()
		f.git(f.ws, "submodule", "add", "-q", f.sub, "libs/b")
		f.git(f.ws, "commit", "-q", "-m", "add b on agent-x")
		return f
	}
	t.Run("valid gitfile passes", func(t *testing.T) {
		f := setup(t)
		if err := f.checkWorktree(); err != nil {
			t.Fatalf("agent-only submodule refused: %v", err)
		}
	})
	t.Run("embedded git dir refuses", func(t *testing.T) {
		f := setup(t)
		dotGit := filepath.Join(f.ws, "libs", "b", ".git")
		if err := os.Remove(dotGit); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dotGit, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = mustRefuse(t, f.checkWorktree(), "<gitlink>", "submodule libs/b", "")
	})
}

// The seed's git environment carries no inherited GIT_DIR, GIT_COMMON_DIR,
// GIT_WORK_TREE or GIT_INDEX_FILE, only the explicit dirs.
func TestSeedGitEnv_NoInheritedRepoSelection(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", "/tmp/envmarker-w1/index")
	t.Setenv("GIT_WORK_TREE", "/tmp/envmarker-w1")
	t.Setenv("GIT_DIR", "/tmp/envmarker-w1/.git")
	t.Setenv("GIT_COMMON_DIR", "/tmp/envmarker-w1/.git")
	env := seedGitEnv("/g", "/c")
	got := map[string][]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = append(got[k], v)
	}
	for _, k := range []string{"GIT_INDEX_FILE", "GIT_WORK_TREE"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s inherited", k)
		}
	}
	if len(got["GIT_DIR"]) != 1 || got["GIT_DIR"][0] != "/g" || len(got["GIT_COMMON_DIR"]) != 1 || got["GIT_COMMON_DIR"][0] != "/c" {
		t.Errorf("git dirs = %v / %v, want exactly /g and /c", got["GIT_DIR"], got["GIT_COMMON_DIR"])
	}
	for _, kv := range env {
		if strings.Contains(kv, "envmarker-w1") {
			t.Error("an inherited repository selection survived")
		}
	}
}

// A gitfile is parsed exactly as git reads it: only trailing newline and
// carriage-return bytes are stripped and the prefix is exactly "gitdir: ".
// Any pointer git could resolve differently refuses; a valid pointer passes
// with or without a trailing newline.
func TestCheckSeedSubmodules_GitfileParsedAsGitDoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   func(ptr string) string
		refuse bool
		setup  func(f *subFixture)
	}{
		{"leading extra space", func(p string) string { return "gitdir:  " + p + "\n" }, true, nil},
		{"trailing space", func(p string) string { return "gitdir: " + p + " \n" }, true, nil},
		{"no space after prefix", func(p string) string { return "gitdir:" + p + "\n" }, true, nil},
		{"embedded newline", func(p string) string { return "gitdir: " + p + "\nextra\n" }, true, nil},
		{"valid without newline", func(p string) string { return "gitdir: " + p }, false, nil},
		{"valid with newline", func(p string) string { return "gitdir: " + p + "\n" }, false, nil},
		{"valid with CRLF", func(p string) string { return "gitdir: " + p + "\r\n" }, false, nil},
		// Lexically, ../../lnk/../.git/modules/libs/a cleans to the checked
		// admin dir; git resolves ".." after following lnk, which lands outside
		// the superproject. This row fails if the join reverts to filepath.Join.
		{"dot-dot through a symlink", func(string) string { return "gitdir: ../../lnk/../.git/modules/libs/a\n" }, true, func(f *subFixture) {
			inner := filepath.Join(f.root, "outside", "inner")
			if err := os.MkdirAll(inner, 0o755); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(inner, filepath.Join(f.super, "lnk")); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubFixture(t)
			gitfile := filepath.Join(f.super, "libs", "a", ".git")
			orig, err := os.ReadFile(gitfile)
			if err != nil {
				t.Fatal(err)
			}
			ptr := strings.TrimRight(strings.TrimPrefix(string(orig), "gitdir: "), "\r\n")
			if tc.setup != nil {
				tc.setup(f)
			}
			f.write(gitfile, tc.body(ptr))
			err = f.checkMain()
			if tc.refuse {
				_ = mustRefuse(t, err, "<gitlink>", "submodule libs/a", "")
			} else if err != nil {
				t.Fatalf("valid pointer refused: %v", err)
			}
		})
	}
}

// The per-dir cross-check is wired for discovered admin dirs: a reader that
// drops one key from the worktree's own module config (a parser differential)
// makes git's listing differ from the checked set, and the seed refuses.
func TestCheckSeedSubmodules_ModuleParserDifferentialRefuses(t *testing.T) {
	f := newSubFixture(t)
	f.addWorktree()
	if err := checkSeedSubmodules(context.Background(), f.commonDir, f.ws, f.wsAdmin, readSeedConfigLayer); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	dropping := func(path, label string) ([]gitConfigVar, error) {
		vars, err := readSeedConfigLayer(path, label)
		if label != "worktree modules/libs/a/config" {
			return vars, err
		}
		var kept []gitConfigVar
		for _, v := range vars {
			if v.Key() != "core.logallrefupdates" {
				kept = append(kept, v)
			}
		}
		return kept, err
	}
	err := checkSeedSubmodules(context.Background(), f.commonDir, f.ws, f.wsAdmin, dropping)
	r := mustRefuse(t, err, "core.logallrefupdates", "", "")
	if !strings.Contains(r.Reason, "read differently") {
		t.Errorf("reason = %q, want a cross-check refusal", r.Reason)
	}
}

// The module cross-check is skipped only when no layer file exists: an existing
// config that the parser reads as empty while git reads a key from it still
// refuses, naming the module's layer. A dir with no layer files passes.
func TestCheckSeedSubmodules_EmptyParseOfExistingConfigStillCrossChecks(t *testing.T) {
	t.Run("existing config parsed empty refuses", func(t *testing.T) {
		f := newSubFixture(t)
		x := filepath.Join(f.commonDir, "modules", "x")
		f.adminDir(x)
		f.write(filepath.Join(x, "config"), "[core]\n\thooksPath = /tmp/emptyparse-x1\n")
		empty := func(path, label string) ([]gitConfigVar, error) {
			if label == "modules/x/config" {
				return nil, nil
			}
			return readSeedConfigLayer(path, label)
		}
		err := checkSeedSubmodules(context.Background(), f.commonDir, f.super, "", empty)
		r := mustRefuse(t, err, "core.hookspath", "", "emptyparse-x1")
		if r.Layer != "modules/x" {
			t.Errorf("layer = %q, want modules/x (the module label)", r.Layer)
		}
		if !strings.Contains(r.Reason, "read differently") {
			t.Errorf("reason = %q, want a cross-check refusal", r.Reason)
		}
	})
	t.Run("no layer files passes", func(t *testing.T) {
		f := newSubFixture(t)
		f.adminDir(filepath.Join(f.commonDir, "modules", "y"))
		if err := f.checkMain(); err != nil {
			t.Fatalf("admin dir without layer files refused: %v", err)
		}
	})
}

// core.worktree is resolved physically. A relative value is joined, uncleaned,
// to the admin dir's path; here that path's parent is a symlink, so the lexical
// and physical results of "../../wt" differ, and the physical one decides R1.
// For R2, a ".." after another component refuses.
func TestCheckSeedModuleWorktree_PhysicalJoin(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mk("elsewhere/sub/admin")
	physical := mk("elsewhere/wt") // elsewhere/sub/admin/../../wt
	lexical := mk("wt")            // lnk/admin/../../wt, cleaned
	if err := os.Symlink(filepath.Join(root, "elsewhere", "sub"), filepath.Join(root, "lnk")); err != nil {
		t.Fatal(err)
	}
	// Not canonical on purpose: its parent "lnk" is a symlink.
	d := seedAdminDir{abs: filepath.Join(root, "lnk", "admin"), canon: filepath.Join(root, "lnk", "admin"), root: root}
	commonDir := filepath.Join(root, ".git")
	if !checkSeedModuleWorktree(d, "../../wt", physical, true, commonDir) {
		t.Error("R1 rejected the physical target")
	}
	if checkSeedModuleWorktree(d, "../../wt", lexical, true, commonDir) {
		t.Error("R1 accepted the lexical target; the join was cleaned")
	}
	// R2: only leading ".." components.
	real := seedAdminDir{abs: filepath.Join(root, "elsewhere", "sub", "admin"), canon: filepath.Join(root, "elsewhere", "sub", "admin"), root: root}
	if !checkSeedModuleWorktree(real, "../../gone", "", false, commonDir) {
		t.Error("R2 rejected a leading-.. value inside the root")
	}
	if checkSeedModuleWorktree(real, "../x/../gone", "", false, commonDir) {
		t.Error("R2 accepted a .. after another component")
	}
}

// The seed check over 50 git-initialised submodules passes; its wall time is
// logged (no timing assertion) for the evidence record.
func TestCheckSeedSubmodules_FiftyModulesWallTime(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 50 submodules; skipped under -short")
	}
	f := newSubFixture(t)
	for i := 0; i < 50; i++ {
		f.git(f.super, "submodule", "add", "-q", f.sub, fmt.Sprintf("mods/m%02d", i))
	}
	f.git(f.super, "commit", "-q", "-m", "fifty")
	start := time.Now()
	err := f.checkMain()
	elapsed := time.Since(start)
	t.Logf("fifty-module seed check: %s", elapsed)
	if err != nil {
		t.Fatalf("fifty clean submodules refused: %v", err)
	}
}

// R2 canonicalises an absolute core.worktree too: with a symlinked ancestor of
// the root, (a) a value through the linked path to a dir inside the root is
// accepted, (b) a value that resolves outside the root through an in-root
// link is refused, and (c) the root itself, named through the link, is
// refused.
func TestCheckSeedModuleWorktree_AbsoluteR2(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "real", "repo")
	for _, d := range []string{filepath.Join(repo, "inside"), filepath.Join(base, "outside")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lnk := filepath.Join(base, "lnk")
	if err := os.Symlink(filepath.Join(base, "real"), lnk); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(repo, "out")); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(lnk, "repo")
	d := seedAdminDir{abs: filepath.Join(repo, ".git", "modules", "x"), canon: filepath.Join(repo, ".git", "modules", "x"), root: linkedRoot}
	commonDir := filepath.Join(linkedRoot, ".git")
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"(a) linked path to a dir inside root", filepath.Join(linkedRoot, "inside", "wt"), true},
		{"(b) in-root link leading outside", filepath.Join(linkedRoot, "out", "wt"), false},
		{"(c) root itself through the link", linkedRoot, false},
	} {
		if got := checkSeedModuleWorktree(d, tc.value, "", false, commonDir); got != tc.want {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The agent-start check's listings (seedGitlinks, and crossCheckAgainstGit
// through CheckSeedRepoConfigLayers and CheckSeedSubmodules) run with the
// HostGitArgs pins first, an explicit GIT_DIR and GIT_COMMON_DIR (no
// discovery), the global and system config off as seedGitEnv sets them, and
// no inherited variable that adds config.
func TestHostGitCheckers_SeedListingsPinGitDir(t *testing.T) {
	f := newSubFixture(t)
	// The fixture adds the submodule from a local path, a URL the seed policy
	// refuses in the base config; give the base an https URL so the layer
	// check runs through to its cross-check. CheckSeedSubmodules does not read
	// this key.
	f.git(f.super, "config", "submodule.libs/a.url", "https://example.invalid/sub.git")
	log := fakeGitOnPath(t)
	plantInheritedGitEnv(t)
	if err := CheckSeedRepoConfigLayers(context.Background(), f.commonDir, ""); err != nil {
		t.Fatalf("CheckSeedRepoConfigLayers: %v", err)
	}
	if err := CheckSeedSubmodules(context.Background(), f.commonDir, f.super, ""); err != nil {
		t.Fatalf("CheckSeedSubmodules: %v", err)
	}
	pins := strings.Join(HostGitArgs(), " ")
	var lsFiles, layers, module int
	for _, c := range fakeGitCalls(t, log) {
		if !strings.HasPrefix(c.argv, pins+" ") {
			t.Errorf("call %q does not start with the HostGitArgs pins", c.argv)
		}
		if c.env["GIT_DIR"] == "" || c.env["GIT_COMMON_DIR"] == "" {
			t.Errorf("call %q runs without an explicit GIT_DIR and GIT_COMMON_DIR", c.argv)
		}
		if c.env["GIT_CONFIG_GLOBAL"] != os.DevNull || c.env["GIT_CONFIG_NOSYSTEM"] != "1" {
			t.Errorf("call %q reads global or system config", c.argv)
		}
		// The whole GIT_* environment is exactly seedGitEnv's for the call's
		// directories, so no inherited variable (GIT_INDEX_FILE, GIT_WORK_TREE,
		// GIT_CONFIG_*) gets through.
		want := gitEnvMap(seedGitEnv(c.env["GIT_DIR"], c.env["GIT_COMMON_DIR"]))
		if d := diffEnvKeys(c.env, want); len(d) > 0 {
			t.Errorf("call %q: environment differs from seedGitEnv in %v", c.argv, d)
		}
		for _, k := range plantedGitEnvKeys {
			if k == "GIT_DIR" {
				continue // set by seedGitEnv, checked above
			}
			if _, ok := c.env[k]; ok {
				t.Errorf("call %q received the inherited %s", c.argv, k)
			}
		}
		switch {
		case strings.HasSuffix(c.argv, " ls-files --stage -z"):
			lsFiles++
			if c.env["GIT_DIR"] != f.commonDir {
				t.Errorf("gitlink listing GIT_DIR is not the common dir")
			}
		case strings.HasSuffix(c.argv, " config --show-scope -z --list") && c.env["GIT_DIR"] == f.commonDir:
			layers++
		case strings.HasSuffix(c.argv, " config --show-scope -z --list") && c.env["GIT_DIR"] == f.mainModule():
			module++
		}
	}
	if lsFiles < 1 || layers < 1 || module < 1 {
		t.Errorf("recorded %d gitlink listings, %d layer cross-checks and %d module cross-checks; want at least one each", lsFiles, layers, module)
	}
}
