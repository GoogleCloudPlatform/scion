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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// eval resolves symlinks so comparisons hold on platforms (e.g. macOS) where
// t.TempDir() lives under a symlinked prefix while git reports the real path.
func eval(t *testing.T, p string) string {
	t.Helper()
	if p == "" {
		return ""
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

// TestDetectRepoRoot_ExplicitWorkspaceSkipsGitDetection is the regression guard:
// an explicit --workspace inside a git repo must return "" (plain mount), not the
// repo root — otherwise the whole repo, including sibling dirs, leaks in.
func TestDetectRepoRoot_ExplicitWorkspaceSkipsGitDetection(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	subA := filepath.Join(root, "subA")
	if err := os.MkdirAll(subA, 0755); err != nil {
		t.Fatalf("mkdir subA: %v", err)
	}

	// Explicit workspace = a subdir inside the repo -> no repo-root detection.
	if got := detectRepoRoot(true, subA, root); got != "" {
		t.Fatalf("explicit workspace inside repo: got repoRoot %q, want \"\"", got)
	}

	// Explicit workspace = the repo root itself -> still plain-mounted, "".
	if got := detectRepoRoot(true, root, root); got != "" {
		t.Fatalf("explicit workspace at repo root: got repoRoot %q, want \"\"", got)
	}
}

// TestDetectRepoRoot_ExplicitWorkspaceResume covers resume/restart: opts.Workspace
// is empty but the persisted ExplicitWorkspace flag re-derives explicit=true, so
// the recovered explicit path must stay plain-mounted rather than widening to the
// enclosing repo — for both a subdir and the repo root itself.
func TestDetectRepoRoot_ExplicitWorkspaceResume(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	subA := filepath.Join(root, "subA")
	if err := os.MkdirAll(subA, 0755); err != nil {
		t.Fatalf("mkdir subA: %v", err)
	}

	// Explicit subdir recovered as effectiveWorkspace on resume -> "".
	if got := detectRepoRoot(true, subA, root); got != "" {
		t.Fatalf("resume explicit subdir workspace: got repoRoot %q, want \"\"", got)
	}

	// Explicit repo root recovered as effectiveWorkspace on resume -> "".
	if got := detectRepoRoot(true, root, root); got != "" {
		t.Fatalf("resume explicit repo-root workspace: got repoRoot %q, want \"\"", got)
	}
}

// TestDetectRepoRoot_ExplicitPlainWorkspace confirms a non-git explicit
// workspace is unaffected (unchanged behavior).
func TestDetectRepoRoot_ExplicitPlainWorkspace(t *testing.T) {
	dir := t.TempDir() // plain dir, no git
	if got := detectRepoRoot(true, dir, dir); got != "" {
		t.Fatalf("explicit plain workspace: got repoRoot %q, want \"\"", got)
	}
}

// TestDetectRepoRoot_AutoDetectFromWorkspace is the counterpart regression
// guard: with NO explicit workspace, git auto-detection still runs and resolves
// the repository root from the effective workspace.
func TestDetectRepoRoot_AutoDetectFromWorkspace(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	subA := filepath.Join(root, "subA")
	if err := os.MkdirAll(subA, 0755); err != nil {
		t.Fatalf("mkdir subA: %v", err)
	}

	// explicit == false -> detection runs; effective workspace is subA,
	// which is inside the repo, so repoRoot is the repository root.
	got := eval(t, detectRepoRoot(false, subA, root))
	if want := eval(t, root); got != want {
		t.Fatalf("auto-detect from workspace: got repoRoot %q, want %q", got, want)
	}
}

// TestDetectRepoRoot_AutoDetectFromProjectDir confirms the fallback branch: no
// explicit workspace, a non-git effective workspace, but a git project dir ->
// repoRoot resolves from the project dir.
func TestDetectRepoRoot_AutoDetectFromProjectDir(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	plain := t.TempDir() // effective workspace, not a git repo

	got := eval(t, detectRepoRoot(false, plain, root))
	if want := eval(t, root); got != want {
		t.Fatalf("auto-detect from project dir: got repoRoot %q, want %q", got, want)
	}
}

// TestDetectRepoRoot_NoGitAnywhere confirms the fully non-git case returns "".
func TestDetectRepoRoot_NoGitAnywhere(t *testing.T) {
	ws := t.TempDir()
	proj := t.TempDir()
	if got := detectRepoRoot(false, ws, proj); got != "" {
		t.Fatalf("no git anywhere: got repoRoot %q, want \"\"", got)
	}
}

// createRealWorktree creates root/worktrees/<name> as a genuine git worktree
// of root (via util.CreateWorktree, the same helper production code uses),
// and returns its path. root must already be a git repo (setupGitRepo).
func createRealWorktree(t *testing.T, root, name string) string {
	t.Helper()
	worktreesDir := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("mkdir worktrees dir: %v", err)
	}
	worktree := filepath.Join(worktreesDir, name)
	if err := util.CreateWorktree(worktree, name); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	return worktree
}
