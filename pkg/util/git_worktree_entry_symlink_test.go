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

package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepoAt initializes a repository with one commit at dir.
func initRepoAt(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "you@example.com")
	runGit(t, dir, "config", "user.name", "Your Name")
	runGit(t, dir, "commit", "--allow-empty", "-m", "root commit")
}

// addHiddenSibling adds a worktree on branch "sibling" outside the tree and
// moves its parent away, so its path is missing from this process's view.
// It returns the sibling path and a function that puts it back.
func addHiddenSibling(t *testing.T, repo string) (string, func()) {
	t.Helper()
	other := t.TempDir()
	volume := filepath.Join(other, "volume")
	sibling := filepath.Join(volume, "sibling")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "worktree", "add", "-b", "sibling", sibling)
	hidden := filepath.Join(other, "volume-hidden")
	if err := os.Rename(volume, hidden); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := os.Rename(hidden, volume); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return sibling, restore
}

// symlinkedTree builds <tmp>/tree/{repo,agents/own/workspace} where tree is
// reached through a symlink, and returns the repository, the own worktree
// path (both in their symlinked form) and the hidden sibling.
//
// With linkFirst, the symlink exists before anything is created, so the
// repository and worktree are created through it. Otherwise the tree is
// created as a real directory, then moved and a symlink left at its old
// location (a relocated directory). relative selects "git worktree add
// --relative-paths" (what scion writes) over plain absolute entries (what
// older versions or a manual add wrote).
func symlinkedTree(t *testing.T, linkFirst, relative bool) (repo, own, sibling string, restore func()) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	realTree := filepath.Join(base, "tree-real")
	if linkFirst {
		if err := os.MkdirAll(realTree, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realTree, tree); err != nil {
			t.Fatal(err)
		}
	}
	repo = filepath.Join(tree, "repo")
	own = filepath.Join(tree, "agents", "own", "workspace")
	initRepoAt(t, repo)
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	add := []string{"worktree", "add"}
	if relative {
		add = append(add, "--relative-paths")
	}
	runGit(t, repo, append(add, "-b", "own", own)...)
	if !linkFirst {
		if err := os.Rename(tree, realTree); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realTree, tree); err != nil {
			t.Fatal(err)
		}
	}
	sibling, restore = addHiddenSibling(t, repo)
	// Remove the whole agent directory, so more than one trailing
	// component of own is missing.
	if err := os.RemoveAll(filepath.Dir(own)); err != nil {
		t.Fatal(err)
	}
	return repo, own, sibling, restore
}

func TestRemoveWorktreeEntryIn_SymlinkedLayouts(t *testing.T) {
	cases := []struct {
		name      string
		linkFirst bool
		relative  bool
	}{
		// git records the resolved path; the literal (symlinked) path is
		// unknown to git, so only the resolved retry removes the entry.
		{name: "symlinked parent at creation", linkFirst: true, relative: true},
		// Absolute entries keep the old, symlinked form; only the literal
		// attempt removes the entry (the resolved form is unknown to git).
		{name: "moved behind a symlink, absolute entries", linkFirst: false, relative: false},
		// Relative entries resolve to the real location; the resolved retry
		// removes the entry.
		{name: "moved behind a symlink, relative entries", linkFirst: false, relative: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, own, sibling, restore := symlinkedTree(t, tc.linkFirst, tc.relative)
			if err := RemoveWorktreeEntryIn(repo, own); err != nil {
				t.Fatalf("RemoveWorktreeEntryIn: %v", err)
			}
			if list := worktreeEntries(t, repo); strings.Contains(list, "branch refs/heads/own") {
				t.Errorf("own entry should be removed; worktree list:\n%s", list)
			}
			assertSiblingIntact(t, repo, sibling, restore)
		})
	}
}

func TestResolveMissingPath(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	wantReal, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}

	got, ok, err := resolveMissingPath(filepath.Join(link, "a", "b"))
	if err != nil || !ok {
		t.Fatalf("resolveMissingPath: ok=%v err=%v", ok, err)
	}
	if want := filepath.Join(wantReal, "a", "b"); got != want {
		t.Errorf("resolveMissingPath = %q, want %q", got, want)
	}

	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveMissingPath(filepath.Join(dangling, "a")); err != nil || ok {
		t.Errorf("dangling link: ok=%v err=%v, want ok=false and no error", ok, err)
	}
}

// A branch held by another worktree whose path is not visible is not taken
// over: CreateWorktree reports the holder and how to clear it, and the
// holder keeps its registration.
func TestCreateWorktree_BranchHeldByHiddenWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	repo := setupGitRepo(t)
	sibling, restore := addHiddenSibling(t, repo)

	path := filepath.Join(repo, "agents", "a", "workspace")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	err := CreateWorktree(path, "sibling")
	if err == nil {
		t.Fatal("CreateWorktree on a branch held by another worktree: expected an error")
	}
	if !strings.Contains(err.Error(), sibling) || !strings.Contains(err.Error(), fmt.Sprintf("git worktree remove %q", sibling)) {
		t.Errorf("error should name the holder and how to clear it; got: %v", err)
	}
	assertSiblingIntact(t, repo, sibling, restore)
}
