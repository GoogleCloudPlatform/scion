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
	"testing"
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
	// Need to be inside the repo for most tests
	repoDir := setupGitRepo(t)
	
	// Save current working dir to restore later
	originalWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(originalWd)

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
		
		if !IsIgnored("ignored.txt") {
			t.Error("expected ignored.txt to be ignored")
		}
		
		if IsIgnored("not-ignored.txt") {
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
		if _, err := RemoveWorktree(worktreePath, false); err != nil {
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
		_, _ = RemoveWorktree(prunePath, true)
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
		_, _ = RemoveWorktree(wtPath, true)
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
			{"https://github.com/ptone/scion.git", "github.com/ptone/scion"},
			{"http://github.com/ptone/scion.git", "github.com/ptone/scion"},
			{"git@github.com:ptone/scion.git", "github.com/ptone/scion"},
			{"github.com/ptone/scion.git", "github.com/ptone/scion"},
			{"git@github.com:ptone/scion", "github.com/ptone/scion"},
			{"HTTPS://GITHUB.COM/ptone/scion.GIT", "github.com/ptone/scion"},
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
