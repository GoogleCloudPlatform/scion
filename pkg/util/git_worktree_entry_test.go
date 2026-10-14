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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// worktreeEntries returns git's porcelain worktree list for repo.
func worktreeEntries(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	return string(out)
}

// hiddenSiblingRepo creates a repository with a worktree "own" under the
// repository and a sibling worktree in a separate directory, then moves the
// sibling's parent directory away so the sibling's path is missing, as it is
// for a worktree on a volume this process cannot see. restore puts the
// sibling back.
func hiddenSiblingRepo(t *testing.T) (repo, own, sibling string, restore func()) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	repo = setupGitRepo(t)
	own = filepath.Join(repo, "agents", "own", "workspace")
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(own, "own"); err != nil {
		t.Fatalf("CreateWorktree own: %v", err)
	}
	other := t.TempDir()
	siblingParent := filepath.Join(other, "volume")
	sibling = filepath.Join(siblingParent, "sibling")
	if err := os.MkdirAll(siblingParent, 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree outside the repository, added the way a user would.
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "sibling", sibling).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add sibling: %v\n%s", err, out)
	}
	hidden := filepath.Join(other, "volume-hidden")
	if err := os.Rename(siblingParent, hidden); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		if err := os.Rename(hidden, siblingParent); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return repo, own, sibling, restore
}

// assertSiblingIntact checks that git still knows the sibling worktree and
// that git commands still work inside it.
func assertSiblingIntact(t *testing.T, repo, sibling string, restore func()) {
	t.Helper()
	restore()
	if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/sibling") {
		t.Errorf("sibling worktree entry was removed; worktree list:\n%s", list)
	}
	if out, err := exec.Command("git", "-C", sibling, "status", "--short").CombinedOutput(); err != nil {
		t.Errorf("git status in sibling worktree failed: %v\n%s", err, out)
	}
}

func TestRemoveWorktree_KeepsHiddenSiblingEntry(t *testing.T) {
	repo, own, sibling, restore := hiddenSiblingRepo(t)

	if _, err := RemoveWorktree(repo, own, true); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	list := worktreeEntries(t, repo)
	if strings.Contains(list, "branch refs/heads/own") {
		t.Errorf("own worktree entry should be removed; worktree list:\n%s", list)
	}
	if BranchExistsIn(repo, "own") {
		t.Error("own branch should be deleted")
	}
	assertSiblingIntact(t, repo, sibling, restore)
}

// A RemoveWorktree that cannot find the repository root (the path is not in
// a repository) must not prune the repository of the current directory.
func TestRemoveWorktree_NoRepoRoot_DoesNotPrune(t *testing.T) {
	repo, _, sibling, restore := hiddenSiblingRepo(t)

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveWorktree(base, plain, false); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	assertSiblingIntact(t, repo, sibling, restore)
}

// When removing its own entry fails, RemoveWorktree reports nothing extra
// and does not fall back to a prune.
func TestRemoveWorktree_EntryRemovalFailure_DoesNotPrune(t *testing.T) {
	repo, own, sibling, restore := hiddenSiblingRepo(t)

	// A locked entry makes "git worktree remove" fail.
	if out, err := exec.Command("git", "-C", repo, "worktree", "lock", own).CombinedOutput(); err != nil {
		t.Fatalf("git worktree lock: %v\n%s", err, out)
	}
	if _, err := RemoveWorktree(repo, own, false); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if err := RemoveWorktreeEntryIn(repo, own); err == nil {
		t.Error("RemoveWorktreeEntryIn on a locked entry: expected an error")
	}
	assertSiblingIntact(t, repo, sibling, restore)
}

func TestRemoveWorktreeEntryIn(t *testing.T) {
	t.Run("removes only the entry for path", func(t *testing.T) {
		repo, own, sibling, restore := hiddenSiblingRepo(t)
		if err := os.RemoveAll(own); err != nil {
			t.Fatal(err)
		}
		if err := RemoveWorktreeEntryIn(repo, own); err != nil {
			t.Fatalf("RemoveWorktreeEntryIn: %v", err)
		}
		if list := worktreeEntries(t, repo); strings.Contains(list, "branch refs/heads/own") {
			t.Errorf("own entry should be removed; worktree list:\n%s", list)
		}
		assertSiblingIntact(t, repo, sibling, restore)
	})

	t.Run("entry already removed is a no-op", func(t *testing.T) {
		repo, own, _, _ := hiddenSiblingRepo(t)
		if err := os.RemoveAll(own); err != nil {
			t.Fatal(err)
		}
		if err := RemoveWorktreeEntryIn(repo, own); err != nil {
			t.Fatalf("first RemoveWorktreeEntryIn: %v", err)
		}
		if err := RemoveWorktreeEntryIn(repo, own); err != nil {
			t.Errorf("second RemoveWorktreeEntryIn: %v", err)
		}
		if err := RemoveWorktreeEntryIn(repo, filepath.Join(repo, "never-registered")); err != nil {
			t.Errorf("RemoveWorktreeEntryIn on an unregistered path: %v", err)
		}
	})

	t.Run("relative path is refused", func(t *testing.T) {
		repo, _, _, _ := hiddenSiblingRepo(t)
		if err := RemoveWorktreeEntryIn(repo, filepath.Join("agents", "own", "workspace")); err == nil {
			t.Error("RemoveWorktreeEntryIn with a relative path: expected an error")
		}
		if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/own") {
			t.Errorf("entry should be kept; worktree list:\n%s", list)
		}
	})

	t.Run("existing directory is left alone", func(t *testing.T) {
		repo, own, _, _ := hiddenSiblingRepo(t)
		if err := RemoveWorktreeEntryIn(repo, own); err != nil {
			t.Fatalf("RemoveWorktreeEntryIn: %v", err)
		}
		if _, err := os.Stat(filepath.Join(own, ".git")); err != nil {
			t.Errorf("worktree files should be kept: %v", err)
		}
		if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/own") {
			t.Errorf("entry of an existing worktree should be kept; worktree list:\n%s", list)
		}
	})

	t.Run("skipped inside agent containers", func(t *testing.T) {
		repo, own, _, _ := hiddenSiblingRepo(t)
		if err := os.RemoveAll(own); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SCION_HOST_UID", "1000")
		if err := RemoveWorktreeEntryIn(repo, own); err != nil {
			t.Fatalf("RemoveWorktreeEntryIn: %v", err)
		}
		if list := worktreeEntries(t, repo); !strings.Contains(list, "branch refs/heads/own") {
			t.Errorf("entry should be kept inside an agent container; worktree list:\n%s", list)
		}
	})
}
