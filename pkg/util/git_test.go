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
		os.Chdir(outsideDir)
		defer os.Chdir(prevWd)

		if err := PruneWorktreesIn(repoDir); err != nil {
			t.Fatalf("PruneWorktreesIn failed: %v", err)
		}

		// Verify we can create the worktree again (prune cleared the stale record)
		os.Chdir(prevWd)
		if err := CreateWorktree(prunePath, pruneBranch); err != nil {
			t.Errorf("Failed to recreate worktree after PruneWorktreesIn: %v", err)
		}
		// Clean up
		_, _ = RemoveWorktree(prunePath, true)
	})

	t.Run("DeleteBranchIn", func(t *testing.T) {
		// Create a branch via worktree, then remove the worktree without deleting the branch
		wtPath := filepath.Join(repoDir, "branch-del-test")
		branch := "delete-me-branch"
		if err := CreateWorktree(wtPath, branch); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}
		if _, err := RemoveWorktree(wtPath, false); err != nil {
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
		_, _ = RemoveWorktree(wtPath, true)
	})

	t.Run("RemoveWorktreeWithBranch", func(t *testing.T) {
		wtPath := filepath.Join(repoDir, "wt-rm-branch")
		branch := "rm-branch-test"

		if err := CreateWorktree(wtPath, branch); err != nil {
			t.Fatalf("CreateWorktree failed: %v", err)
		}

		deleted, err := RemoveWorktree(wtPath, true)
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
	defer os.Chdir(originalWd)
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
	_, _ = RemoveWorktree(siblingPath, true)
	_, _ = RemoveWorktree(wtPath, true)
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
	// (HashProjectID is no longer used for grove IDs, but is retained for deterministic identifiers.)
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
