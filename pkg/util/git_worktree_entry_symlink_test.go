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

	got, state, err := resolveMissingPath(filepath.Join(link, "a", "b"))
	if err != nil || state != resolvedWithin {
		t.Fatalf("resolveMissingPath: state=%v err=%v", state, err)
	}
	if want := filepath.Join(wantReal, "a", "b"); got != want {
		t.Errorf("resolveMissingPath = %q, want %q", got, want)
	}

	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, state, err := resolveMissingPath(filepath.Join(dangling, "a")); err != nil || state != resolvedLiteralOnly {
		t.Errorf("dangling link: state=%v err=%v, want literal only and no error", state, err)
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

// Resolution is kept within the agent directory: an ancestor that is a
// symlink to a directory outside the bound (the worktree path's
// grandparent) is not followed.
func TestResolveMissingPath_KeepsResolutionWithinAgentDirectory(t *testing.T) {
	base := t.TempDir()
	agents := filepath.Join(base, "agents")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{agents, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(agents, "a")); err != nil {
		t.Fatal(err)
	}
	if _, state, err := resolveMissingPath(filepath.Join(agents, "a", "workspace")); err != nil || state != resolvedOutside {
		t.Errorf("ancestor linked outside the agent directory: state=%v err=%v, want outside and no error", state, err)
	}

	// An ancestor above the bound is never resolved.
	if _, state, err := resolveMissingPath(filepath.Join(base, "gone", "agents", "a", "workspace")); err != nil || state != resolvedLiteralOnly {
		t.Errorf("existing ancestor above the bound: state=%v err=%v, want literal only and no error", state, err)
	}
}

// RemoveWorktreeEntryIn does not remove the entry of a worktree outside the
// agent directory reached through an ancestor symlink.
func TestRemoveWorktreeEntryIn_AncestorLinkOutsideIsRefused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	repo := setupGitRepo(t)
	base := t.TempDir()
	agents := filepath.Join(base, "agents")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{agents, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := filepath.Join(outside, "workspace")
	runGit(t, repo, "worktree", "add", "-b", "elsewhere", elsewhere)
	if err := os.RemoveAll(elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(agents, "a")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveWorktreeEntryIn(repo, filepath.Join(agents, "a", "workspace")); err == nil || !strings.Contains(err.Error(), "other than the agent's own directory") {
		t.Errorf("RemoveWorktreeEntryIn through a link outside the agent directory = %v, want it refused", err)
	}
	if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/elsewhere") {
		t.Errorf("the entry of the worktree outside the agent directory should be kept; worktree list:\n%s", list)
	}
}

// A link from one agent's directory to a sibling agent's directory under the
// same bound is refused too: the resolved form would name the sibling's
// worktree, whose entry must not be removed.
func TestRemoveWorktreeEntryIn_SiblingAgentLinkIsRefused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	repo := setupGitRepo(t)
	agents := filepath.Join(t.TempDir(), "agents")
	siblingWs := filepath.Join(agents, "b", "workspace")
	if err := os.MkdirAll(filepath.Dir(siblingWs), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "worktree", "add", "-b", "b", siblingWs)
	if err := os.RemoveAll(siblingWs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agents, "b"), filepath.Join(agents, "a")); err != nil {
		t.Fatal(err)
	}

	own := filepath.Join(agents, "a", "workspace")
	if _, state, err := resolveMissingPath(own); err != nil || state != resolvedOutside {
		t.Errorf("link to a sibling agent's directory: state=%v err=%v, want outside and no error", state, err)
	}
	if err := RemoveWorktreeEntryIn(repo, own); err == nil {
		t.Error("RemoveWorktreeEntryIn through a link to a sibling agent's directory: want it refused")
	}
	if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/b\n") {
		t.Errorf("the sibling agent's entry should be kept; worktree list:\n%s", list)
	}
}

// The path is cleaned before any check and before git sees it: git would
// resolve a ".." after a link physically, so agents/c/../b/workspace, with
// agents/c linking into another directory, would name a different worktree
// than the agents/b/workspace the checks approve.
func TestRemoveWorktreeEntryIn_DotDotAfterLinkIsCleaned(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	repo := setupGitRepo(t)
	agents := filepath.Join(t.TempDir(), "agents")
	other := filepath.Join(agents, "z", "b", "workspace")
	for _, d := range []string{filepath.Dir(other), filepath.Join(agents, "z", "q")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "worktree", "add", "-b", "zb", other)
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agents, "z", "q"), filepath.Join(agents, "c")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveWorktreeEntryIn(repo, agents+"/c/../b/workspace"); err != nil {
		t.Fatalf("RemoveWorktreeEntryIn: %v", err)
	}
	if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/zb\n") {
		t.Errorf("the entry reached through the link and \"..\" should be kept; worktree list:\n%s", list)
	}
}
