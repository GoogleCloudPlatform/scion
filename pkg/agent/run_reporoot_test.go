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

// TestValidateProvisionedWorktreeRepoRoot_ValidWorktree is the accept case: a
// real git repo root with a workspace that is a genuine git worktree nested
// under root/worktrees/<id> — the exact relationship tryProvisionWorktree
// creates, not just a matching directory shape. Confirms the returned root is
// the ORIGINAL argument (round-2 review finding R1: must stay lexically
// consistent with the unresolved effectiveWorkspace the caller already has),
// not an EvalSymlinks'd one.
func TestValidateProvisionedWorktreeRepoRoot_ValidWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	setupGitRepo(t, root)
	worktree := createRealWorktree(t, root, "agent-1")

	got := validateProvisionedWorktreeRepoRoot(root, worktree)
	if got != root {
		t.Fatalf("validateProvisionedWorktreeRepoRoot(root, worktree) = %q, want the original root %q unchanged", got, root)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsPlainMkdirWorktree is the
// round-2 review's required regression test: a plain mkdir'd
// root/worktrees/<name> — the right directory shape but no actual git
// worktree relationship — must be rejected. This is exactly what round-1's
// validator missed (it accepted this shape) and what makes the deep gitfile
// checks necessary rather than just the path-shape checks.
func TestValidateProvisionedWorktreeRepoRoot_RejectsPlainMkdirWorktree(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot(root, worktree); got != "" {
		t.Fatalf("plain mkdir'd worktree: got %q, want \"\" (not a real git worktree)", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsWorktreesRootExactly covers
// N1: a workspace exactly AT root/worktrees (no name segment) is not the
// layout tryProvisionWorktree creates and must be rejected.
func TestValidateProvisionedWorktreeRepoRoot_RejectsWorktreesRootExactly(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	worktreesDir := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("mkdir worktrees dir: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot(root, worktreesDir); got != "" {
		t.Fatalf("workspace == root/worktrees exactly: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsNestedBeyondName covers a
// workspace nested one level deeper than the exact layout, e.g.
// root/worktrees/<name>/extra — must be rejected, not just anything "under"
// worktrees/.
func TestValidateProvisionedWorktreeRepoRoot_RejectsNestedBeyondName(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	setupGitRepo(t, root)
	worktree := createRealWorktree(t, root, "agent-1")
	nested := filepath.Join(worktree, "extra")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot(root, nested); got != "" {
		t.Fatalf("workspace nested beyond worktrees/<name>: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsForgedAdminDir covers a
// forged pairing that gets past the path-shape and gitfile-presence checks: a
// workspace whose .git gitfile points at a directory that exists and looks
// like a worktree admin dir, but whose own back-link does not point back at
// the workspace. This is what the back-link check (round-2 review C1 step 2,
// "if cheap") catches that the gitdir-resolves-to-admin-dir check alone does
// not.
func TestValidateProvisionedWorktreeRepoRoot_RejectsForgedAdminDir(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)

	worktree := filepath.Join(root, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// A gitfile pointing at a real, correctly-shaped admin dir under root...
	adminDir := filepath.Join(root, ".git", "worktrees", "agent-1")
	if err := os.MkdirAll(adminDir, 0755); err != nil {
		t.Fatalf("mkdir admin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+adminDir+"\n"), 0644); err != nil {
		t.Fatalf("write workspace gitfile: %v", err)
	}
	// ...but whose back-link points somewhere else entirely, not back at
	// worktree/.git — the tell that this admin dir was hand-crafted, not
	// created by a real `git worktree add`.
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(elsewhere+"\n"), 0644); err != nil {
		t.Fatalf("write forged back-link: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot(root, worktree); got != "" {
		t.Fatalf("forged admin dir with mismatched back-link: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsNonGitRoot is the direct
// regression test for the round-1 review's PoC (C1): a candidate root with no
// .git directory at all — standing in for the review's literal "/etc" and "/"
// examples without touching real system paths — must be rejected rather than
// mounted.
func TestValidateProvisionedWorktreeRepoRoot_RejectsNonGitRoot(t *testing.T) {
	root := t.TempDir() // no git repo here
	worktree := filepath.Join(root, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot(root, worktree); got != "" {
		t.Fatalf("non-git root: got %q, want \"\" (must not trust a root with no .git)", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsWorkspaceOutsideRoot covers
// the review's "workspace outside root" case: even a real git repo root must
// be rejected if the workspace it's paired with isn't actually inside it —
// otherwise a stale or mismatched pairing could still reach the full-root
// mount branch in pkg/runtime/common.go.
func TestValidateProvisionedWorktreeRepoRoot_RejectsWorkspaceOutsideRoot(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	outside := t.TempDir() // sibling, not under root at all

	if got := validateProvisionedWorktreeRepoRoot(root, outside); got != "" {
		t.Fatalf("workspace outside root: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsWorkspaceAtRoot covers the
// case where the workspace IS the root itself (rel == "."): this is not the
// worktrees/<id> layout tryProvisionWorktree creates, and letting it through
// would put the whole repo root at risk of the common.go "shared workspace"
// or full-root-mount branches for a signal that is supposed to be scoped to
// a single worktree subdirectory.
func TestValidateProvisionedWorktreeRepoRoot_RejectsWorkspaceAtRoot(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)

	if got := validateProvisionedWorktreeRepoRoot(root, root); got != "" {
		t.Fatalf("workspace == root: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsWorkspaceOutsideWorktreesSubdir
// covers a workspace that IS inside root, but not under the worktrees/
// subdirectory — e.g. root/some-other-dir. Only the exact layout
// tryProvisionWorktree creates should validate.
func TestValidateProvisionedWorktreeRepoRoot_RejectsWorkspaceOutsideWorktreesSubdir(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	notWorktrees := filepath.Join(root, "some-other-dir")
	if err := os.MkdirAll(notWorktrees, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot(root, notWorktrees); got != "" {
		t.Fatalf("workspace outside worktrees/ subdir: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_EmptyInputsRejected covers the
// trivial empty cases.
func TestValidateProvisionedWorktreeRepoRoot_EmptyInputsRejected(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	if got := validateProvisionedWorktreeRepoRoot("", worktree); got != "" {
		t.Fatalf("empty root: got %q, want \"\"", got)
	}
	if got := validateProvisionedWorktreeRepoRoot(root, ""); got != "" {
		t.Fatalf("empty workspace: got %q, want \"\"", got)
	}
}

// TestValidateProvisionedWorktreeRepoRoot_RejectsNonexistentPaths covers
// paths that don't resolve at all (EvalSymlinks failure) — e.g. a persisted
// value pointing at a since-deleted directory.
func TestValidateProvisionedWorktreeRepoRoot_RejectsNonexistentPaths(t *testing.T) {
	root := t.TempDir()
	setupGitRepo(t, root)
	worktree := filepath.Join(root, "worktrees", "agent-1")
	if err := os.MkdirAll(worktree, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	nonexistentRoot := filepath.Join(t.TempDir(), "does-not-exist")
	if got := validateProvisionedWorktreeRepoRoot(nonexistentRoot, worktree); got != "" {
		t.Fatalf("nonexistent root: got %q, want \"\"", got)
	}

	nonexistentWorkspace := filepath.Join(root, "worktrees", "no-such-agent")
	if got := validateProvisionedWorktreeRepoRoot(root, nonexistentWorkspace); got != "" {
		t.Fatalf("nonexistent workspace: got %q, want \"\"", got)
	}
}
