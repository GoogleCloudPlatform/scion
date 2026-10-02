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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func setupGitRepo(t *testing.T) string {
	dir := t.TempDir()

	// Initialize git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Config user for commits
	configCmds := [][]string{
		{"config", "user.email", "you@example.com"},
		{"config", "user.name", "Your Name"},
		{"commit", "--allow-empty", "-m", "root commit"},
	}

	for _, args := range configCmds {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Fatalf("failed to run git %v: %v", args, err)
		}
	}

	return dir
}

func TestGitUtils(t *testing.T) {
	// Clear container context so worktree tests can create worktrees
	t.Setenv("SCION_HOST_UID", "")

	// Need to be inside the repo for most tests
	repoDir := setupGitRepo(t)

	// Save current working dir to restore later
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalWd) }()

	if err := os.Chdir(repoDir); err != nil {
		t.Fatal(err)
	}

	t.Run("IsGitRepo", func(t *testing.T) {
		if !IsGitRepo() {
			t.Error("expected true, got false")
		}
	})

	t.Run("RepoRoot", func(t *testing.T) {
		root, err := RepoRoot()
		if err != nil {
			t.Errorf("RepoRoot failed: %v", err)
		}
		// RepoRoot usually returns path with symlinks resolved, matching t.TempDir behavior
		// On macOS t.TempDir might be in /var/folders/... which is a symlink to /private/var/folders/...
		// We resolve both to compare safely.
		evalRoot, _ := filepath.EvalSymlinks(root)
		evalRepoDir, _ := filepath.EvalSymlinks(repoDir)

		if evalRoot != evalRepoDir {
			t.Errorf("expected root %q, got %q", evalRepoDir, evalRoot)
		}
	})

	t.Run("IsIgnored", func(t *testing.T) {
		ignoreFile := filepath.Join(repoDir, ".gitignore")
		if err := os.WriteFile(ignoreFile, []byte("ignored.txt"), 0644); err != nil {
			t.Fatal(err)
		}

		if !IsIgnored(repoDir, "ignored.txt") {
			t.Error("expected ignored.txt to be ignored")
		}

		if IsIgnored(repoDir, "not-ignored.txt") {
			t.Error("expected not-ignored.txt to NOT be ignored")
		}
	})

	t.Run("Worktrees", func(t *testing.T) {
		worktreePath := filepath.Join(repoDir, "wt-test")
		branchName := "test-branch"

		// Create
		if err := CreateWorktree(worktreePath, branchName); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}

		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			t.Errorf("worktree dir does not exist")
		}

		// Remove
		if _, err := RemoveWorktree(repoDir, worktreePath, false); err != nil {
			t.Fatalf("RemoveWorktree failed: %v", err)
		}
		// Wait/Check? git worktree remove deletes the directory usually.
		if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
			t.Errorf("worktree dir still exists after removal")
		}

		// Test PruneWorktrees
		prunePath := filepath.Join(repoDir, "prune-test")
		pruneBranch := "prune-branch"
		if err := CreateWorktree(prunePath, pruneBranch); err != nil {
			t.Fatalf("CreateWorktree for prune failed: %v", err)
		}
		// Manually remove directory to simulate stale worktree
		if err := os.RemoveAll(prunePath); err != nil {
			t.Fatalf("Failed to remove prune path: %v", err)
		}
		// Prune
		if err := PruneWorktrees(); err != nil {
			t.Fatalf("PruneWorktrees failed: %v", err)
		}
		// Verify we can create it again (if prune failed, this might fail with 'already exists')
		if err := CreateWorktree(prunePath, pruneBranch); err != nil {
			t.Errorf("Failed to recreate worktree after prune: %v", err)
		}
		// Clean up
		_, _ = RemoveWorktree(repoDir, prunePath, true)
	})

	t.Run("PruneWorktreesIn", func(t *testing.T) {
		prunePath := filepath.Join(repoDir, "prune-in-test")
		pruneBranch := "prune-in-branch"
		if err := CreateWorktree(prunePath, pruneBranch); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}
		// Manually remove directory to simulate stale worktree
		if err := os.RemoveAll(prunePath); err != nil {
			t.Fatalf("Failed to remove prune path: %v", err)
		}

		// PruneWorktreesIn should work even when CWD is outside the repo
		outsideDir := t.TempDir()
		prevWd, _ := os.Getwd()
		if err := os.Chdir(outsideDir); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(prevWd) }()

		if err := PruneWorktreesIn(repoDir); err != nil {
			t.Fatalf("PruneWorktreesIn failed: %v", err)
		}

		// Verify we can create the worktree again (prune cleared the stale record)
		if err := os.Chdir(prevWd); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorktree(prunePath, pruneBranch); err != nil {
			t.Errorf("Failed to recreate worktree after PruneWorktreesIn: %v", err)
		}
		// Clean up
		_, _ = RemoveWorktree(repoDir, prunePath, true)
	})

	t.Run("DeleteBranchIn", func(t *testing.T) {
		// Create a branch via worktree, then remove the worktree without deleting the branch
		wtPath := filepath.Join(repoDir, "branch-del-test")
		branch := "delete-me-branch"
		if err := CreateWorktree(wtPath, branch); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}
		if _, err := RemoveWorktree(repoDir, wtPath, false); err != nil {
			t.Fatalf("RemoveWorktree failed: %v", err)
		}

		// Branch should still exist
		if !BranchExists(branch) {
			t.Fatal("expected branch to still exist after RemoveWorktree(deleteBranch=false)")
		}

		// DeleteBranchIn should remove it
		if !DeleteBranchIn(repoDir, branch) {
			t.Error("DeleteBranchIn returned false, expected true")
		}

		// Branch should be gone
		if BranchExists(branch) {
			t.Error("expected branch to be deleted after DeleteBranchIn")
		}

		// Deleting a non-existent branch should return false
		if DeleteBranchIn(repoDir, "no-such-branch") {
			t.Error("DeleteBranchIn returned true for non-existent branch")
		}
	})

	t.Run("FindWorktreeByBranch", func(t *testing.T) {
		wtPath := filepath.Join(repoDir, "wt-find")
		branch := "find-branch"

		if err := CreateWorktree(wtPath, branch); err != nil {
			t.Fatalf("setup failed: %v", err)
		}

		foundPath, err := FindWorktreeByBranch(branch)
		if err != nil {
			t.Errorf("FindWorktreeByBranch failed: %v", err)
		}

		// Normalize paths for comparison (resolve symlinks)
		evalFound, _ := filepath.EvalSymlinks(foundPath)
		evalWt, _ := filepath.EvalSymlinks(wtPath)

		if evalFound != evalWt {
			t.Errorf("expected %q, got %q", evalWt, evalFound)
		}

		// Clean up
		_, _ = RemoveWorktree(repoDir, wtPath, true)
	})

	t.Run("RemoveWorktreeWithBranch", func(t *testing.T) {
		wtPath := filepath.Join(repoDir, "wt-rm-branch")
		branch := "rm-branch-test"

		if err := CreateWorktree(wtPath, branch); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}

		deleted, err := RemoveWorktree(repoDir, wtPath, true)
		if err != nil {
			t.Fatalf("RemoveWorktree failed: %v", err)
		}
		if !deleted {
			t.Error("expected branch to be deleted")
		}
		if BranchExists(branch) {
			t.Error("branch still exists after RemoveWorktree with deleteBranch=true")
		}
	})

	t.Run("CompareGitVersion", func(t *testing.T) {
		tests := []struct {
			version string
			major   int
			minor   int
			wantErr bool
		}{
			{"2.47.0", 2, 47, false},
			{"2.48.0", 2, 47, false},
			{"3.0.0", 2, 47, false},
			{"2.46.9", 2, 47, true},
			{"1.9.0", 2, 47, true},
			{"2.47.1.windows.1", 2, 47, false},
			{"invalid", 2, 47, true},
		}

		for _, tt := range tests {
			err := CompareGitVersion(tt.version, tt.major, tt.minor)
			if (err != nil) != tt.wantErr {
				t.Errorf("CompareGitVersion(%q, %d, %d) error = %v, wantErr %v", tt.version, tt.major, tt.minor, err, tt.wantErr)
			}
		}
	})

	t.Run("NormalizeGitRemote", func(t *testing.T) {
		tests := []struct {
			remote string
			want   string
		}{
			{"https://github.com/GoogleCloudPlatform/scion.git", "github.com/googlecloudplatform/scion"},
			{"http://github.com/GoogleCloudPlatform/scion.git", "github.com/googlecloudplatform/scion"},
			{"git@github.com:GoogleCloudPlatform/scion.git", "github.com/googlecloudplatform/scion"},
			{"github.com/GoogleCloudPlatform/scion.git", "github.com/googlecloudplatform/scion"},
			{"git@github.com:GoogleCloudPlatform/scion", "github.com/googlecloudplatform/scion"},
			{"HTTPS://github.com/GoogleCloudPlatform/scion.GIT", "github.com/googlecloudplatform/scion"},
			{"", ""},
		}

		for _, tt := range tests {
			got := NormalizeGitRemote(tt.remote)
			if got != tt.want {
				t.Errorf("NormalizeGitRemote(%q) = %q, want %q", tt.remote, got, tt.want)
			}
		}
	})
}

func TestCreateWorktree_FromWorktreeSucceeds(t *testing.T) {
	// Clear container context so worktree creation is allowed
	t.Setenv("SCION_HOST_UID", "")

	// Creating a worktree from within an existing worktree is a legitimate
	// operation (git supports it natively via the common git dir). This test
	// verifies that CreateWorktree resolves the main repo root correctly and
	// creates the sibling worktree.
	mainRepo := setupGitRepo(t)

	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalWd) }()
	if err := os.Chdir(mainRepo); err != nil {
		t.Fatal(err)
	}

	// Create a worktree from the main repo
	wtPath := filepath.Join(mainRepo, "child-wt")
	if err := CreateWorktree(wtPath, "child-branch"); err != nil {
		t.Fatalf("failed to create initial worktree: %v", err)
	}

	// Change into the worktree and create another worktree from there.
	// This should succeed — the new worktree is a peer managed by the main repo.
	if err := os.Chdir(wtPath); err != nil {
		t.Fatal(err)
	}
	siblingPath := filepath.Join(mainRepo, "sibling-wt")
	if err := CreateWorktree(siblingPath, "sibling-branch"); err != nil {
		t.Fatalf("expected worktree creation from within a worktree to succeed, got: %v", err)
	}

	// Clean up
	_, _ = RemoveWorktree(mainRepo, siblingPath, true)
	_, _ = RemoveWorktree(mainRepo, wtPath, true)
}

func TestCreateWorktree_RejectsInsideContainer(t *testing.T) {
	// When SCION_HOST_UID is set (agent container), worktree creation should
	// be refused to prevent path-identity mismatches from container mounts.
	t.Setenv("SCION_HOST_UID", "1000")

	mainRepo := setupGitRepo(t)

	wtPath := filepath.Join(mainRepo, "container-wt")
	err := CreateWorktree(wtPath, "container-branch")
	if err == nil {
		t.Fatal("expected error creating worktree inside container context")
	}
	if !strings.Contains(err.Error(), "SCION_HOST_UID") {
		t.Errorf("unexpected error: %v", err)
	}
}

// addWorktreeDirect runs `git -C repoRoot worktree add` directly, not
// through CreateWorktree, whose own directory computation assumes the new
// worktree's parent directory is already inside a git working tree -- not
// true for a genuine sibling of repoRoot, which is exactly the shape these
// tests need to create.
func addWorktreeDirect(t *testing.T, repoRoot, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "-b", branch, path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, output)
	}
}

// TestSplitPorcelainRecords_HandlesCRLF proves CRLF-terminated porcelain
// output is still split into the correct per-worktree records: a literal
// "\r\n\r\n" blank-line separator shares no "\n\n" substring with an
// unnormalized split, so without the CRLF-to-LF normalization this would
// return the whole input as a single record instead of two, and each
// record's "worktree <path>" line would carry a trailing "\r" into the
// parsed path.
func TestSplitPorcelainRecords_HandlesCRLF(t *testing.T) {
	input := "worktree /repo\r\nHEAD abc123\r\nbranch refs/heads/main\r\n\r\nworktree /repo/.scion-worktrees/wt1\r\nHEAD def456\r\nbranch refs/heads/feature\r\n"
	records := splitPorcelainRecords(input)
	if len(records) != 2 {
		t.Fatalf("splitPorcelainRecords(CRLF input) returned %d record(s), want 2: %q", len(records), records)
	}
	if !strings.Contains(records[0], "worktree /repo\n") {
		t.Errorf("record[0] = %q, want a \"worktree /repo\" line with no trailing \\r", records[0])
	}
	if !strings.Contains(records[1], "worktree /repo/.scion-worktrees/wt1\n") {
		t.Errorf("record[1] = %q, want a \"worktree /repo/.scion-worktrees/wt1\" line with no trailing \\r", records[1])
	}
}

// TestSplitPorcelainRecords_PlainLFUnchanged proves the normalization step
// is a no-op on ordinary LF-only output (what git actually emits on Linux),
// so the CRLF handling added for TestSplitPorcelainRecords_HandlesCRLF does
// not change behavior on the common path.
func TestSplitPorcelainRecords_PlainLFUnchanged(t *testing.T) {
	input := "worktree /repo\nHEAD abc123\n\nworktree /repo/.scion-worktrees/wt1\nHEAD def456\n"
	records := splitPorcelainRecords(input)
	if len(records) != 2 {
		t.Fatalf("splitPorcelainRecords(LF input) returned %d record(s), want 2: %q", len(records), records)
	}
}

func TestIsRegisteredWorktree_MainWorktreeAccepted(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	ok, err := IsRegisteredWorktree(mainRepo, mainRepo)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if !ok {
		t.Error("expected the main repository itself to be a registered worktree")
	}
}

// TestIsRegisteredWorktree_MainWorktreeAcceptedWhenRepoRootIsLinkedWorktree
// covers a project that lives in a linked worktree rather than the main
// one: repoRoot (derived from the project's own directory) is the linked
// worktree's path, not the main worktree's. The main worktree must still be
// recognized as belonging to the same repository -- recognized because its
// .git is a directory that resolves to the repository's common git dir, not
// by equality with repoRoot, which never holds in this shape even though
// both worktrees share one repository.
func TestIsRegisteredWorktree_MainWorktreeAcceptedWhenRepoRootIsLinkedWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	linkedPath := filepath.Join(filepath.Dir(mainRepo), "linked-repo-feature")
	addWorktreeDirect(t, mainRepo, linkedPath, "feature")
	defer func() { _, _ = RemoveWorktree(linkedPath, true) }()

	// repoRoot is the LINKED worktree here, simulating a project that lives
	// there; path is the MAIN worktree, which must still be recognized as
	// this repository's own.
	ok, err := IsRegisteredWorktree(linkedPath, mainRepo)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if !ok {
		t.Error("expected the main worktree to be accepted when repoRoot is a linked worktree of the same repository")
	}
}

func TestIsRegisteredWorktree_SiblingWorktreeAccepted(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	// A plain sibling worktree, not under any particular convention --
	// `git worktree add` accepts any destination.
	siblingPath := filepath.Join(filepath.Dir(mainRepo), "sibling-repo-feature")
	addWorktreeDirect(t, mainRepo, siblingPath, "feature")
	defer func() { _, _ = RemoveWorktree(siblingPath, true) }()

	ok, err := IsRegisteredWorktree(mainRepo, siblingPath)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if !ok {
		t.Error("expected a plain sibling worktree to be accepted as a registered worktree")
	}
}

func TestIsRegisteredWorktree_ScionWorktreesConventionAccepted(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	// The legacy {parent}/.scion_worktrees/{project}/{agent} layout is just
	// another caller-named destination as far as git worktree add is concerned
	// -- it needs no special-case handling, only real git registration.
	wtPath := filepath.Join(filepath.Dir(mainRepo), ".scion_worktrees", "proj", "agent")
	addWorktreeDirect(t, mainRepo, wtPath, "agent-branch")
	defer func() { _, _ = RemoveWorktree(wtPath, true) }()

	ok, err := IsRegisteredWorktree(mainRepo, wtPath)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if !ok {
		t.Error("expected a .scion_worktrees-convention worktree to be accepted as a registered worktree")
	}
}

func TestIsRegisteredWorktree_NonWorktreeDirRejected(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	// An ordinary directory that merely sits next to the repo, never
	// registered with git as a worktree of it.
	notAWorktree := filepath.Join(filepath.Dir(mainRepo), "just-a-directory")
	if err := os.MkdirAll(notAWorktree, 0755); err != nil {
		t.Fatal(err)
	}

	ok, err := IsRegisteredWorktree(mainRepo, notAWorktree)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if ok {
		t.Error("expected a non-worktree directory to be rejected")
	}
}

func TestIsRegisteredWorktree_DifferentRepoWorktreeRejected(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	repoA := setupGitRepo(t)
	repoB := setupGitRepo(t)

	// A worktree that genuinely belongs to a DIFFERENT repository must not
	// be accepted as a worktree of repoA.
	wtOfB := filepath.Join(filepath.Dir(repoB), "repoB-feature")
	addWorktreeDirect(t, repoB, wtOfB, "feature")
	defer func() { _, _ = RemoveWorktree(wtOfB, true) }()

	ok, err := IsRegisteredWorktree(repoA, wtOfB)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if ok {
		t.Error("expected a worktree of a different repository to be rejected")
	}
}

func TestIsRegisteredWorktree_SymlinkToNonRegisteredPathRejected(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	outside := filepath.Join(filepath.Dir(mainRepo), "outside-target")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(mainRepo), "link-to-outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	ok, err := IsRegisteredWorktree(mainRepo, link)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if ok {
		t.Error("expected a symlink resolving to a non-registered path to be rejected")
	}
}

func TestIsRegisteredWorktree_GitListFailureFailsClosed(t *testing.T) {
	// repoRoot is not a git repository at all, so `git worktree list` fails.
	// The failure itself must be surfaced as an error, not silently reported
	// as "not a worktree" -- and certainly never as "is a worktree."
	notARepo := t.TempDir()
	somePath := t.TempDir()

	ok, err := IsRegisteredWorktree(notARepo, somePath)
	if err == nil {
		t.Fatal("expected an error when git worktree list fails, got nil")
	}
	if ok {
		t.Error("expected false alongside the error (fail closed, never fail open)")
	}
}

func TestIsRegisteredWorktree_PrunableRecreatedPathRejected(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	staleWt := filepath.Join(filepath.Dir(mainRepo), "stale-wt")
	addWorktreeDirect(t, mainRepo, staleWt, "stale-branch")

	// Remove the worktree directory directly (not via `git worktree
	// remove`), leaving git's own registration in place but pointing at a
	// gitdir that no longer resolves -- exactly what makes `git worktree
	// list --porcelain` report the entry as prunable.
	if err := os.RemoveAll(staleWt); err != nil {
		t.Fatal(err)
	}
	// Recreate a plain directory at the same path: nothing git-related,
	// just a directory that happens to have the same name.
	if err := os.MkdirAll(staleWt, 0755); err != nil {
		t.Fatal(err)
	}

	ok, err := IsRegisteredWorktree(mainRepo, staleWt)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree: %v", err)
	}
	if ok {
		t.Error("expected a prunable (stale) worktree registration, now a plain recreated directory, to be rejected")
	}

	_ = PruneWorktreesIn(mainRepo)
}

// TestIsRegisteredWorktree_RecreatedWithForeignGitDirRefused covers a
// worktree path that was removed and then recreated as the root of a
// completely different, independent repository (via `git init`, not `git
// worktree add`) -- as opposed to
// TestIsRegisteredWorktree_PrunableRecreatedPathRejected's plain directory,
// which git's own porcelain output flags "prunable" and IsRegisteredWorktree
// filters out before either structural check ever runs. A fresh `git init`
// leaves a real directory at .git, so the record is NOT prunable (git only
// checks that something is there, not that it points back correctly), and
// the path-equality check against the registered worktree path still
// matches. Recognition then depends entirely on isMainWorktreeOf resolving
// the recreated .git directory and finding it does NOT equal the original
// repository's common git dir (see git.go's isMainWorktreeOf) -- no other
// existing test depends on that specific comparison, which is what makes
// this case worth pinning directly.
func TestIsRegisteredWorktree_RecreatedWithForeignGitDirRefused(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mainRepo := setupGitRepo(t)

	linkedPath := filepath.Join(filepath.Dir(mainRepo), "linked-repo-for-foreign-gitdir-test")
	addWorktreeDirect(t, mainRepo, linkedPath, "linked-branch")
	defer func() { _, _ = RemoveWorktree(linkedPath, true) }()

	recreatedWt := filepath.Join(filepath.Dir(mainRepo), "recreated-wt")
	addWorktreeDirect(t, mainRepo, recreatedWt, "recreated-branch")

	// Remove the worktree directory entirely, then recreate it as the root
	// of a brand new, unrelated repository -- not via `git worktree add`, so
	// it is never registered with mainRepo at all. What is left on disk at
	// recreatedWt is a normal (directory) .git, structurally identical in
	// shape to a main worktree's .git, but it resolves to this new repo's
	// own git dir, not mainRepo's common git dir.
	if err := os.RemoveAll(recreatedWt); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(recreatedWt, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", recreatedWt)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}

	ok, err := IsRegisteredWorktree(mainRepo, recreatedWt)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree(mainRepo): %v", err)
	}
	if ok {
		t.Error("expected a worktree path recreated with a foreign repo's own .git dir to be rejected against the main repo")
	}

	ok, err = IsRegisteredWorktree(linkedPath, recreatedWt)
	if err != nil {
		t.Fatalf("IsRegisteredWorktree(linkedPath): %v", err)
	}
	if ok {
		t.Error("expected a worktree path recreated with a foreign repo's own .git dir to be rejected against a linked worktree repoRoot")
	}

	_ = PruneWorktreesIn(mainRepo)
}

// TestIsLinkedWorktreeOf_RejectsGitdirEqualToWorktreesDir covers a real
// linked worktree's gitdir always naming a specific entry under worktrees/,
// never the worktrees directory itself -- there is no registration that is
// the whole administrative directory, so a gitdir resolving to exactly
// worktreesDir must not be treated as a match.
func TestIsLinkedWorktreeOf_RejectsGitdirEqualToWorktreesDir(t *testing.T) {
	mainRepo := setupGitRepo(t)

	commonDir, err := GetCommonGitDir(mainRepo)
	if err != nil {
		t.Fatalf("GetCommonGitDir: %v", err)
	}
	worktreesDir := filepath.Join(commonDir, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(filepath.Dir(mainRepo), "gitdir-equals-worktreesdir")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(wtPath, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+worktreesDir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if isLinkedWorktreeOf(wtPath, worktreesDir) {
		t.Error("expected a gitdir equal to worktreesDir itself to be rejected, got true")
	}

	// A gitdir naming a real entry under worktreesDir is still accepted --
	// this fix must not over-refuse the legitimate shape.
	namedEntry := filepath.Join(worktreesDir, "some-worktree")
	if err := os.MkdirAll(namedEntry, 0755); err != nil {
		t.Fatal(err)
	}
	wtPath2 := filepath.Join(filepath.Dir(mainRepo), "gitdir-names-real-entry")
	if err := os.MkdirAll(wtPath2, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath2, ".git"), []byte("gitdir: "+namedEntry+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !isLinkedWorktreeOf(wtPath2, worktreesDir) {
		t.Error("expected a gitdir naming a real entry under worktreesDir to be accepted, got false")
	}
}

func TestPruneWorktrees_SkipsInsideContainer(t *testing.T) {
	// When SCION_HOST_UID is set (agent container), pruning should be a no-op
	// to prevent destroying sibling worktree metadata that appears stale from
	// the container's mount layout.
	t.Setenv("SCION_HOST_UID", "1000")

	// Both prune functions should return nil without running git at all.
	if err := PruneWorktrees(); err != nil {
		t.Errorf("PruneWorktrees should no-op inside container, got: %v", err)
	}
	if err := PruneWorktreesIn("/nonexistent/path"); err != nil {
		t.Errorf("PruneWorktreesIn should no-op inside container, got: %v", err)
	}
}

// TestRemoveWorktree_RefusesOutOfTreePath covers Phase 2 acceptance criterion
// 8: RemoveWorktree must refuse to remove a path whose resolved (symlink-free)
// location does not lie under base, and must not touch anything under the
// real external target while refusing.
func TestRemoveWorktree_RefusesOutOfTreePath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	t.Run("path directly outside base", func(t *testing.T) {
		base := setupGitRepo(t)
		outside := t.TempDir()
		marker := filepath.Join(outside, "keep-me")
		if err := os.WriteFile(marker, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := RemoveWorktree(base, outside, false)
		if !errors.Is(err, ErrPathNotContained) {
			t.Fatalf("expected ErrPathNotContained, got: %v", err)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("external content must survive a refused removal: %v", statErr)
		}
	})

	t.Run("candidate equals base", func(t *testing.T) {
		base := setupGitRepo(t)
		_, err := RemoveWorktree(base, base, false)
		if !errors.Is(err, ErrPathNotContained) {
			t.Fatalf("expected ErrPathNotContained for candidate==base, got: %v", err)
		}
		if _, statErr := os.Stat(base); statErr != nil {
			t.Errorf("base must survive a refused removal: %v", statErr)
		}
	})

	t.Run("symlinked leaf pointing outside base", func(t *testing.T) {
		base := setupGitRepo(t)
		outside := t.TempDir()
		target := filepath.Join(outside, "real-content")
		if err := os.MkdirAll(filepath.Join(target, "keep"), 0755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(target, "keep", "important.txt")
		if err := os.WriteFile(marker, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		worktreesDir := filepath.Join(base, "worktrees")
		if err := os.MkdirAll(worktreesDir, 0755); err != nil {
			t.Fatal(err)
		}
		leaf := filepath.Join(worktreesDir, "out-of-tree-name")
		if err := os.Symlink(target, leaf); err != nil {
			t.Fatal(err)
		}

		_, err := RemoveWorktree(base, leaf, false)
		if !errors.Is(err, ErrPathNotContained) {
			t.Fatalf("expected ErrPathNotContained, got: %v", err)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("symlink target content must survive a refused removal: %v", statErr)
		}
		if _, statErr := os.Lstat(leaf); statErr != nil {
			t.Errorf("the symlink itself must be left alone by a refused removal: %v", statErr)
		}
	})

	t.Run("symlinked intermediate directory pointing outside base", func(t *testing.T) {
		base := setupGitRepo(t)
		outside := t.TempDir()
		target := filepath.Join(outside, "external-worktrees")
		named := filepath.Join(target, "name")
		if err := os.MkdirAll(named, 0755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(named, "important.txt")
		if err := os.WriteFile(marker, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		// base/worktrees itself is a symlink to the external directory, so
		// base/worktrees/name is lexically inside base but resolves outside it.
		worktreesLink := filepath.Join(base, "worktrees")
		if err := os.Symlink(target, worktreesLink); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(base, "worktrees", "name")

		_, err := RemoveWorktree(base, candidate, false)
		if !errors.Is(err, ErrPathNotContained) {
			t.Fatalf("expected ErrPathNotContained, got: %v", err)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("content behind the symlinked intermediate dir must survive: %v", statErr)
		}
	})

	t.Run("legitimate in-tree worktree is unaffected", func(t *testing.T) {
		base := setupGitRepo(t)
		originalWd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chdir(originalWd) }()
		if err := os.Chdir(base); err != nil {
			t.Fatal(err)
		}

		if err := os.MkdirAll(filepath.Join(base, "worktrees"), 0755); err != nil {
			t.Fatal(err)
		}
		wtPath := filepath.Join(base, "worktrees", "good")
		if err := CreateWorktree(wtPath, "good-branch"); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}

		if _, err := RemoveWorktree(base, wtPath, false); err != nil {
			t.Fatalf("RemoveWorktree of a legitimate in-tree worktree should succeed, got: %v", err)
		}
		if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
			t.Errorf("legitimate in-tree worktree should have been removed, stat err=%v", statErr)
		}
	})

	t.Run("non-existent path is a no-op, not an error", func(t *testing.T) {
		base := setupGitRepo(t)
		deleted, err := RemoveWorktree(base, filepath.Join(base, "worktrees", "never-existed"), false)
		if err != nil {
			t.Errorf("removing a non-existent path should be a no-op, got: %v", err)
		}
		if deleted {
			t.Error("expected deleted=false for a non-existent path")
		}
	})
}

func TestIsGitURL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// Valid URLs
		{"https://github.com/org/repo.git", true},
		{"https://github.com/org/repo", true},
		{"http://github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"git@github.com:org/repo", true},
		{"ssh://git@github.com/org/repo", true},
		{"git://github.com/org/repo.git", true},
		{"HTTPS://GITHUB.COM/org/repo.git", true},
		{"git@gitlab.com:group/subgroup/repo.git", true},

		// Invalid inputs
		{"", false},
		{"/local/path/to/repo", false},
		{"./relative/path", false},
		{"../parent/path", false},
		{"github.com", false},          // bare hostname, no scheme recognized
		{"git@github.com:", false},     // no path after colon
		{"git@github.com:repo", false}, // no '/' in path
		{"https://github.com/", false}, // path is just '/'
		{"https://github.com", false},  // no path
	}

	for _, tt := range tests {
		got := IsGitURL(tt.input)
		if got != tt.want {
			t.Errorf("IsGitURL(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestToHTTPSCloneURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// SSH shorthand → HTTPS
		{"git@github.com:org/repo.git", "https://github.com/org/repo.git"},
		{"git@github.com:org/repo", "https://github.com/org/repo.git"},

		// ssh:// → HTTPS
		{"ssh://git@github.com/org/repo", "https://github.com/org/repo.git"},
		{"ssh://git@github.com/org/repo.git", "https://github.com/org/repo.git"},

		// HTTPS passthrough
		{"https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"https://github.com/org/repo", "https://github.com/org/repo.git"},

		// git:// → HTTPS
		{"git://github.com/org/repo.git", "https://github.com/org/repo.git"},

		// http:// → HTTPS
		{"http://github.com/org/repo.git", "https://github.com/org/repo.git"},

		// Azure DevOps — must NOT append .git
		{"https://dev.azure.com/org/project/_git/repo", "https://dev.azure.com/org/project/_git/repo"},
		// ADO with erroneous .git suffix — must be stripped
		{"https://dev.azure.com/org/project/_git/repo.git", "https://dev.azure.com/org/project/_git/repo"},
		// ADO via visualstudio.com
		{"https://myorg.visualstudio.com/project/_git/repo", "https://myorg.visualstudio.com/project/_git/repo"},

		// Empty
		{"", ""},
	}

	for _, tt := range tests {
		got := ToHTTPSCloneURL(tt.input)
		if got != tt.want {
			t.Errorf("ToHTTPSCloneURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExtractOrgRepo(t *testing.T) {
	tests := []struct {
		input    string
		wantOrg  string
		wantRepo string
	}{
		{"https://github.com/acme/widgets.git", "acme", "widgets"},
		{"git@github.com:acme/widgets.git", "acme", "widgets"},
		{"ssh://git@github.com/acme/widgets", "acme", "widgets"},
		{"https://github.com/Acme/Widgets.git", "acme", "widgets"},
		{"git://github.com/org/repo.git", "org", "repo"},
		{"", "", ""},
	}

	for _, tt := range tests {
		org, repo := ExtractOrgRepo(tt.input)
		if org != tt.wantOrg || repo != tt.wantRepo {
			t.Errorf("ExtractOrgRepo(%q) = (%q, %q), want (%q, %q)", tt.input, org, repo, tt.wantOrg, tt.wantRepo)
		}
	}
}

func TestNormalizeGitRemote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"https", "https://github.com/org/repo.git", "github.com/org/repo"},
		{"ssh shorthand", "git@github.com:org/repo.git", "github.com/org/repo"},
		{"ssh scheme", "ssh://git@github.com/org/repo.git", "github.com/org/repo"},
		{"git scheme", "git://github.com/org/repo.git", "github.com/org/repo"},
		{"http", "http://github.com/org/repo.git", "github.com/org/repo"},
		{"https no .git", "https://github.com/org/repo", "github.com/org/repo"},
		{"https token auth", "https://x-access-token:ghp_abc123@github.com/org/repo.git", "github.com/org/repo"},
		{"https oauth", "https://user:x-oauth-basic@github.com/org/repo.git", "github.com/org/repo"},
		{"https user only", "https://user@github.com/org/repo.git", "github.com/org/repo"},
		{"uppercase host", "https://GitHub.COM/org/repo.git", "github.com/org/repo"},
		{"https trailing slash", "https://github.com/org/repo/", "github.com/org/repo"},
		{"https trailing slash with .git", "https://github.com/org/repo.git/", "github.com/org/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeGitRemote(tt.input)
			if got != tt.want {
				t.Errorf("NormalizeGitRemote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeGitRemote_CrossProtocolConsistency(t *testing.T) {
	// All of these refer to the same repository and must produce the same normalized form.
	// (HashProjectID is retained for deterministic identifiers.)
	variants := []string{
		"git@github.com:ptone/gamegame.git",
		"https://github.com/ptone/gamegame.git",
		"ssh://git@github.com/ptone/gamegame.git",
		"https://x-access-token:TOKEN@github.com/ptone/gamegame.git",
		"git://github.com/ptone/gamegame.git",
		"https://github.com/ptone/gamegame/",
		"https://github.com/ptone/gamegame",
	}

	want := "github.com/ptone/gamegame"
	for _, url := range variants {
		got := NormalizeGitRemote(url)
		if got != want {
			t.Errorf("NormalizeGitRemote(%q) = %q, want %q", url, got, want)
		}
	}

	// All should produce the same deterministic hash
	ids := make(map[string]bool)
	for _, url := range variants {
		ids[HashProjectID(NormalizeGitRemote(url))] = true
	}
	if len(ids) != 1 {
		t.Errorf("expected all URL variants to produce the same deterministic hash, got %d distinct IDs", len(ids))
	}
}

func TestHashProjectID(t *testing.T) {
	// Determinism: same input → same output
	id1 := HashProjectID("github.com/acme/widgets")
	id2 := HashProjectID("github.com/acme/widgets")
	if id1 != id2 {
		t.Errorf("HashProjectID not deterministic: %q != %q", id1, id2)
	}

	// Must be a valid UUID (36 chars, parseable)
	if len(id1) != 36 {
		t.Errorf("HashProjectID length = %d, want 36 (UUID format)", len(id1))
	}
	if _, err := uuid.Parse(id1); err != nil {
		t.Errorf("HashProjectID produced invalid UUID %q: %v", id1, err)
	}

	// Different inputs → different outputs
	id3 := HashProjectID("github.com/acme/gadgets")
	if id1 == id3 {
		t.Errorf("HashProjectID collision: %q == %q for different inputs", id1, id3)
	}

	// Branch qualifier produces different ID
	id4 := HashProjectID("github.com/acme/widgets@release/v2")
	if id1 == id4 {
		t.Errorf("HashProjectID collision with branch qualifier: %q == %q", id1, id4)
	}
}

func TestCloneSharedWorkspace(t *testing.T) {
	// Create a source repo to clone from (local path as "remote")
	sourceDir := setupGitRepo(t)

	// Add a file so the clone has content
	testFile := filepath.Join(sourceDir, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "hello.txt")
	cmd.Dir = sourceDir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit", "-m", "add hello")
	cmd.Dir = sourceDir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	t.Run("SuccessfulClone", func(t *testing.T) {
		destDir := filepath.Join(t.TempDir(), "workspace")
		err := CloneSharedWorkspace(destDir, sourceDir, "", "")
		if err != nil {
			t.Fatalf("CloneSharedWorkspace failed: %v", err)
		}

		// Verify file exists
		content, err := os.ReadFile(filepath.Join(destDir, "hello.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "hello world" {
			t.Errorf("unexpected content: %q", content)
		}

		// Verify git identity was configured
		cmd := exec.Command("git", "-C", destDir, "config", "user.name")
		output, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(output)); got != "Scion" {
			t.Errorf("expected user.name 'Scion', got %q", got)
		}

		cmd = exec.Command("git", "-C", destDir, "config", "user.email")
		output, err = cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(output)); got != "agent@scion.dev" {
			t.Errorf("expected user.email 'agent@scion.dev', got %q", got)
		}
	})

	t.Run("CloneWithBranch", func(t *testing.T) {
		// Create a branch in the source repo
		cmd := exec.Command("git", "-C", sourceDir, "branch", "feature")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}

		destDir := filepath.Join(t.TempDir(), "workspace")
		err := CloneSharedWorkspace(destDir, sourceDir, "feature", "")
		if err != nil {
			t.Fatalf("CloneSharedWorkspace with branch failed: %v", err)
		}

		// Verify we're on the feature branch
		cmd = exec.Command("git", "-C", destDir, "branch", "--show-current")
		output, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(output)); got != "feature" {
			t.Errorf("expected branch 'feature', got %q", got)
		}
	})

	t.Run("CloneWithNonExistentBranch_FallsBack", func(t *testing.T) {
		destDir := filepath.Join(t.TempDir(), "workspace")
		err := CloneSharedWorkspace(destDir, sourceDir, "branch-does-not-exist", "")
		if err != nil {
			t.Fatalf("CloneSharedWorkspace should fall back to default branch: %v", err)
		}

		// Verify we're on the requested branch (created locally)
		cmd := exec.Command("git", "-C", destDir, "branch", "--show-current")
		output, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(output)); got != "branch-does-not-exist" {
			t.Errorf("expected branch 'branch-does-not-exist', got %q", got)
		}

		// Verify file content was cloned from the default branch
		content, err := os.ReadFile(filepath.Join(destDir, "hello.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "hello world" {
			t.Errorf("unexpected content: %q", content)
		}

		// Verify git identity was still configured
		cmd = exec.Command("git", "-C", destDir, "config", "user.name")
		output, err = cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(output)); got != "Scion" {
			t.Errorf("expected user.name 'Scion', got %q", got)
		}
	})

	t.Run("CloneFailure_BadURL", func(t *testing.T) {
		destDir := filepath.Join(t.TempDir(), "workspace")
		err := CloneSharedWorkspace(destDir, "/nonexistent/repo", "", "")
		if err == nil {
			t.Fatal("expected clone to fail with bad URL")
		}
		if !strings.Contains(err.Error(), "git clone failed") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("TokenSanitizedInRemote", func(t *testing.T) {
		// Clone with a fake token — since it's a local path, the token won't
		// actually be used for auth, but we can verify the remote URL is sanitized
		destDir := filepath.Join(t.TempDir(), "workspace")
		cloneURL := "https://example.com/org/repo.git"

		// This will fail because the URL is not a real repo, but we can test
		// sanitizeGitOutput separately
		err := CloneSharedWorkspace(destDir, cloneURL, "", "secret-token-123")
		if err != nil {
			// Expected failure — verify token is not in the error message
			if strings.Contains(err.Error(), "secret-token-123") {
				t.Error("token leaked in error message")
			}
		}
	})
}

func TestPullSharedWorkspace(t *testing.T) {
	// Create a source repo to pull from
	sourceDir := setupGitRepo(t)

	// Add initial content
	if err := os.WriteFile(filepath.Join(sourceDir, "initial.txt"), []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "initial.txt")
	cmd.Dir = sourceDir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "commit", "-m", "initial commit")
	cmd.Dir = sourceDir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	// Clone from the source
	cloneDir := filepath.Join(t.TempDir(), "workspace")
	if err := CloneSharedWorkspace(cloneDir, sourceDir, "", ""); err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	t.Run("PullNoChanges", func(t *testing.T) {
		result, err := PullSharedWorkspace(cloneDir, "")
		if err != nil {
			t.Fatalf("Pull failed: %v", err)
		}
		if result.Updated {
			t.Error("expected Updated=false when already up to date")
		}
	})

	t.Run("PullNewChanges", func(t *testing.T) {
		// Add a new file to the source repo
		if err := os.WriteFile(filepath.Join(sourceDir, "new.txt"), []byte("new content"), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "add", "new.txt")
		cmd.Dir = sourceDir
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command("git", "commit", "-m", "add new file")
		cmd.Dir = sourceDir
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}

		result, err := PullSharedWorkspace(cloneDir, "")
		if err != nil {
			t.Fatalf("Pull failed: %v", err)
		}
		if !result.Updated {
			t.Error("expected Updated=true after pull with new commits")
		}
		if len(result.Commits) == 0 {
			t.Error("expected at least one commit in result")
		}
		if len(result.Commits) > 0 && result.Commits[0].Subject != "add new file" {
			t.Errorf("expected commit subject %q, got %q", "add new file", result.Commits[0].Subject)
		}

		// Verify the new file was pulled
		content, err := os.ReadFile(filepath.Join(cloneDir, "new.txt"))
		if err != nil {
			t.Fatal("new.txt should exist after pull")
		}
		if string(content) != "new content" {
			t.Errorf("unexpected content: %q", content)
		}
	})

	t.Run("PullFailure_NotARepo", func(t *testing.T) {
		notARepo := t.TempDir()
		_, err := PullSharedWorkspace(notARepo, "")
		if err == nil {
			t.Fatal("expected pull to fail for non-repo directory")
		}
		if !strings.Contains(err.Error(), "git pull failed") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestSanitizeGitOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		token  string
		want   string
	}{
		{"empty token", "fatal: error", "", "fatal: error"},
		{"token in URL", "fatal: could not read from https://oauth2:mytoken@github.com", "mytoken", "fatal: could not read from https://oauth2:***@github.com"},
		{"no token present", "fatal: some other error", "mytoken", "fatal: some other error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeGitOutput(tt.output, tt.token)
			if got != tt.want {
				t.Errorf("sanitizeGitOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyGitError(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		wantKind GitErrorKind
	}{
		{"auth failure 401", "fatal: Authentication failed for 'https://github.com/org/repo.git/': 401", GitErrAuth},
		{"auth failure 403", "remote: Permission denied (403)", GitErrAuth},
		{"invalid credentials", "fatal: Invalid credentials", GitErrAuth},
		{"could not read username", "fatal: could not read Username for 'https://github.com'", GitErrAuth},
		{"not found", "fatal: repository 'https://github.com/org/repo.git/' not found", GitErrNotFound},
		{"404", "ERROR: Repository not found. 404", GitErrNotFound},
		{"network error", "fatal: unable to access: Could not resolve host: github.com", GitErrNetwork},
		{"connection refused", "fatal: unable to connect: connection refused", GitErrNetwork},
		{"timed out", "fatal: unable to access: timed out", GitErrNetwork},
		{"non-fast-forward", "fatal: Not possible to fast-forward, aborting.", GitErrNonFastForward},
		{"unknown error", "fatal: some unknown error", GitErrUnknown},
		{"empty stderr", "", GitErrUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gitErr := ClassifyGitError(tt.stderr)
			if gitErr.Kind != tt.wantKind {
				t.Errorf("ClassifyGitError(%q).Kind = %v, want %v", tt.stderr, gitErr.Kind, tt.wantKind)
			}
			if gitErr.Message != tt.stderr {
				t.Errorf("ClassifyGitError(%q).Message = %q, want %q", tt.stderr, gitErr.Message, tt.stderr)
			}
		})
	}
}

func TestIsRemoteBranchNotFound(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{"exact git message", "fatal: Remote branch a2a-bridge not found in upstream origin", true},
		{"lowercase variant", "fatal: remote branch my-branch not found in upstream origin", true},
		{"repo not found", "fatal: repository 'https://github.com/org/repo.git/' not found", false},
		{"auth failure", "fatal: Authentication failed for 'https://github.com/org/repo.git/'", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRemoteBranchNotFound(tt.stderr); got != tt.want {
				t.Errorf("isRemoteBranchNotFound(%q) = %v, want %v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestGitError_UserGuidance(t *testing.T) {
	tests := []struct {
		kind     GitErrorKind
		wantHint bool
	}{
		{GitErrAuth, true},
		{GitErrNotFound, true},
		{GitErrNetwork, true},
		{GitErrNonFastForward, true},
		{GitErrUnknown, false},
	}
	for _, tt := range tests {
		err := &GitError{Kind: tt.kind, Message: "test"}
		guidance := err.UserGuidance()
		if tt.wantHint && guidance == "" {
			t.Errorf("GitError{Kind: %v}.UserGuidance() returned empty, want non-empty", tt.kind)
		}
		if !tt.wantHint && guidance != "" {
			t.Errorf("GitError{Kind: %v}.UserGuidance() = %q, want empty", tt.kind, guidance)
		}
	}
}

func TestAuthenticatedCloneURL(t *testing.T) {
	const token = "ghp_exampletoken"

	tests := []struct {
		name     string
		cloneURL string
		token    string
		want     string
	}{
		{
			name:     "plain https remote gets credentials",
			cloneURL: "https://github.com/org/repo.git",
			token:    token,
			want:     "https://oauth2:" + token + "@github.com/org/repo.git",
		},
		{
			name:     "remote that already carries userinfo is not doubled up",
			cloneURL: "https://org@dev.azure.com/org/project/_git/repo",
			token:    token,
			want:     "https://oauth2:" + token + "@dev.azure.com/org/project/_git/repo",
		},
		{
			name:     "empty token leaves the URL alone",
			cloneURL: "https://github.com/org/repo.git",
			token:    "",
			want:     "https://github.com/org/repo.git",
		},
		{
			name:     "token with reserved characters is escaped",
			cloneURL: "https://github.com/org/repo.git",
			token:    "p@ss/word",
			want:     "https://oauth2:p%40ss%2Fword@github.com/org/repo.git",
		},
		{
			name:     "non-default port is preserved",
			cloneURL: "https://host.example.com:8443/org/repo.git",
			token:    token,
			want:     "https://oauth2:" + token + "@host.example.com:8443/org/repo.git",
		},
		{
			name:     "scp-style ssh remote is left alone",
			cloneURL: "git@github.com:org/repo.git",
			token:    token,
			want:     "git@github.com:org/repo.git",
		},
		{
			name:     "ssh scheme is left alone rather than given an oauth token",
			cloneURL: "ssh://git@github.com/org/repo.git",
			token:    token,
			want:     "ssh://git@github.com/org/repo.git",
		},
		{
			name:     "plain http is left alone",
			cloneURL: "http://internal.example.com/org/repo.git",
			token:    token,
			want:     "http://internal.example.com/org/repo.git",
		},
		{
			name:     "unparseable remote is returned unchanged",
			cloneURL: "not a url",
			token:    token,
			want:     "not a url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := authenticatedCloneURL(tt.cloneURL, tt.token)
			if got != tt.want {
				t.Errorf("authenticatedCloneURL(%q, token) = %q, want %q", tt.cloneURL, got, tt.want)
			}
			if tt.token != "" && strings.Count(got, "@") > 1 {
				t.Errorf("result contains more than one @ separator: %q", got)
			}
		})
	}
}

// fakeGitBinary writes a shell script that reports the given `git --version`
// output and returns its path, for pointing SCION_GIT_BINARY at a specific
// version without depending on whatever git happens to be installed on the
// host running the test.
func fakeGitBinary(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	script := fmt.Sprintf("#!/bin/sh\necho 'git version %s'\n", version)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake git binary: %v", err)
	}
	return path
}

// TestCheckGitVersion_Gate is the required regression guard for the
// worktree-per-agent git-version bump (2.47 -> 2.48): `git worktree add
// --relative-paths` did not exist until 2.48, so a 2.47.x host must be
// rejected by CheckGitVersion rather than silently falling back to
// clone-per-agent later. 2.48.0 must still be accepted.
func TestCheckGitVersion_Gate(t *testing.T) {
	t.Run("2.47.x is rejected", func(t *testing.T) {
		t.Setenv("SCION_GIT_BINARY", fakeGitBinary(t, "2.47.2"))
		err := CheckGitVersion()
		if err == nil {
			t.Fatal("CheckGitVersion() = nil, want an error for git 2.47.2")
		}
		if !strings.Contains(err.Error(), "2.48.0") {
			t.Errorf("error %q should name the 2.48.0 requirement", err.Error())
		}
	})

	t.Run("2.48.0 is accepted", func(t *testing.T) {
		t.Setenv("SCION_GIT_BINARY", fakeGitBinary(t, "2.48.0"))
		if err := CheckGitVersion(); err != nil {
			t.Errorf("CheckGitVersion() = %v, want nil for git 2.48.0", err)
		}
	})

	t.Run("2.49.0 (above minimum) is accepted", func(t *testing.T) {
		t.Setenv("SCION_GIT_BINARY", fakeGitBinary(t, "2.49.0"))
		if err := CheckGitVersion(); err != nil {
			t.Errorf("CheckGitVersion() = %v, want nil for git 2.49.0", err)
		}
	})
}
