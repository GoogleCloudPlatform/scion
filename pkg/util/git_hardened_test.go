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

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// IsRegisteredLinkedWorktree is true only for a linked worktree git has
// registered for the repository; the main worktree, a plain directory, a
// removed (prunable) worktree and a directory containing the repository are
// not.
func TestIsRegisteredLinkedWorktree(t *testing.T) {
	repo := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "linked", linked)
	removed := filepath.Join(t.TempDir(), "removed")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "removed", removed)
	if err := os.RemoveAll(removed); err != nil {
		t.Fatal(err)
	}
	// Recreate the directory so the path resolves; git still lists the
	// registration as prunable (its .git pointer is gone).
	if err := os.MkdirAll(removed, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := t.TempDir()

	for _, tt := range []struct {
		name string
		path string
		want bool
	}{
		{"registered linked worktree", linked, true},
		{"main worktree", repo, false},
		{"plain directory", plain, false},
		{"removed (prunable) worktree", removed, false},
		{"directory containing the repository", filepath.Dir(repo), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsRegisteredLinkedWorktree(repo, tt.path)
			if err != nil {
				t.Fatalf("IsRegisteredLinkedWorktree: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	if _, err := IsRegisteredLinkedWorktree(repo, filepath.Join(plain, "missing")); err == nil {
		t.Error("a path that does not resolve: want an error (not verified)")
	}
	if ok, err := IsRegisteredLinkedWorktree(plain, linked); ok || err == nil {
		t.Errorf("a repoRoot that is not a repository: (ok=%v, err=%v), want (false, error)", ok, err)
	}
}

// The hardened git calls read no system or global configuration: a global
// and system config that would make plain git fail is ignored.
func TestHardenedGit_IgnoresGlobalAndSystemConfig(t *testing.T) {
	repo := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGitIn(t, repo, "worktree", "add", "-q", "-b", "linked", linked)

	hostile := filepath.Join(t.TempDir(), "hostile.gitconfig")
	if err := os.WriteFile(hostile, []byte("this is not [valid git config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", hostile)
	t.Setenv("GIT_CONFIG_SYSTEM", hostile)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")

	// Plain git now fails on the hostile config, so the hardened calls
	// below succeed only because they ignore it.
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").CombinedOutput(); err == nil {
		t.Fatalf("fixture: plain git should fail on the hostile config, got %s", out)
	}

	top, err := HardenedRepoRootDir(repo)
	if err != nil {
		t.Fatalf("HardenedRepoRootDir: %v", err)
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(top); got != want {
		t.Errorf("HardenedRepoRootDir = %q, want %q", top, want)
	}
	if ok, err := IsRegisteredLinkedWorktree(repo, linked); err != nil || !ok {
		t.Errorf("IsRegisteredLinkedWorktree with hostile config = (%v, %v), want (true, nil)", ok, err)
	}
	if _, err := HardenedRepoRootDir(t.TempDir()); err == nil {
		t.Error("HardenedRepoRootDir outside a repository: want an error")
	}
}
