package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// isolateGitTestEnv gives git a private HOME and no system config, and
// clears the SCION_* and git variables that redirect repository discovery
// (SCION_HOST_UID would otherwise turn worktree pruning into a no-op).
func isolateGitTestEnv(t *testing.T) string {
	t.Helper()
	isolateTestEnv(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", base)
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	return base
}

// runGitT runs git in dir and fails the test on error.
func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initTestRepo creates a git repository at dir with one commit on main.
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", "README")
	runGitT(t, dir, "commit", "-q", "-m", "init")
}

// worktreeAdminDir returns the admin dir git keeps for the worktree named
// name in repo.
func worktreeAdminDir(repo, name string) string {
	return filepath.Join(repo, ".git", "worktrees", name)
}

// With prune set and no workspace path (a shared-workspace agent), no
// worktree prune runs in the repository of the process's current directory:
// a stale worktree entry there survives (ptone/scion#4511).
func TestRemoveAgentDirAndPruneWorktrees_EmptyWorkspaceDoesNotPruneCwdRepo(t *testing.T) {
	base := isolateGitTestEnv(t)
	cwdRepo := filepath.Join(base, "caller")
	initTestRepo(t, cwdRepo)
	stale := filepath.Join(base, "stale-wt")
	runGitT(t, cwdRepo, "worktree", "add", "-q", "-b", "stale", stale)
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwdRepo)

	agentDir := filepath.Join(base, "agents", "a")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	removeAgentDirAndPruneWorktrees(agentDir, "", true)

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
	if _, err := os.Stat(worktreeAdminDir(cwdRepo, "stale-wt")); err != nil {
		t.Fatalf("expected the stale worktree entry in the current directory's repository to survive: %v", err)
	}
}

// With prune set and a workspace path, the worktrees of the repository
// holding the workspace are still pruned, wherever the process runs.
func TestRemoveAgentDirAndPruneWorktrees_WorkspacePrunesItsRepo(t *testing.T) {
	base := isolateGitTestEnv(t)
	repo := filepath.Join(base, "project")
	initTestRepo(t, repo)
	agentDir := filepath.Join(repo, ".scion", "agents", "a")
	ws := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repo, "worktree", "add", "-q", "-b", "a", ws)
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)

	removeAgentDirAndPruneWorktrees(agentDir, ws, true)

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
	if _, err := os.Stat(worktreeAdminDir(repo, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("expected the removed workspace's worktree entry to be pruned (stat err=%v)", err)
	}
}

// staleWorktreeLayout builds a git project whose agent dir has no
// scion-agent config file, an empty external home, and a workspace worktree
// on branch "a" inside the agent dir. It returns the base temp dir, the
// repository, the agent dir, the agent home and the workspace.
func staleWorktreeLayout(t *testing.T) (base, repo, agentDir, agentHome, ws string) {
	t.Helper()
	base = isolateGitTestEnv(t)
	repo = filepath.Join(base, "project")
	initTestRepo(t, repo)
	agentDir = filepath.Join(repo, ".scion", "agents", "a")
	agentHome = filepath.Join(base, "external", "a", "home")
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws = filepath.Join(agentDir, "workspace")
	// Provisioning creates worktrees with --relative-paths; match it.
	runGitT(t, repo, "worktree", "add", "-q", "--relative-paths", "-b", "a", ws)
	t.Chdir(base)
	return base, repo, agentDir, agentHome, ws
}

// countWorktreeGit wraps runWorktreeGit for the rest of the test and
// returns a pointer to the number of git commands it ran.
func countWorktreeGit(t *testing.T) *int {
	t.Helper()
	n := 0
	orig := runWorktreeGit
	runWorktreeGit = func(ws string, args ...string) (string, error) {
		n++
		return orig(ws, args...)
	}
	t.Cleanup(func() { runWorktreeGit = orig })
	return &n
}

// noConfigGitProject builds a git project with an agent dir that has no
// scion-agent config file and an empty external home, but no workspace.
func noConfigGitProject(t *testing.T) (base, repo, agentDir, agentHome, ws string) {
	t.Helper()
	base = isolateGitTestEnv(t)
	repo = filepath.Join(base, "project")
	initTestRepo(t, repo)
	agentDir = filepath.Join(repo, ".scion", "agents", "a")
	agentHome = filepath.Join(base, "external", "a", "home")
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(base)
	return base, repo, agentDir, agentHome, filepath.Join(agentDir, "workspace")
}

// requireKeptWithConflict checks that handleAgentDirWithoutConfig refused
// with config.ErrAgentStateConflict, named no host path, and left keep in
// place.
func requireKeptWithConflict(t *testing.T, err error, base, keep string) {
	t.Helper()
	if !errors.Is(err, config.ErrAgentStateConflict) {
		t.Fatalf("error = %v, want it to wrap config.ErrAgentStateConflict", err)
	}
	if strings.Contains(err.Error(), base) || strings.Contains(err.Error(), string(filepath.Separator)) {
		t.Errorf("error text %q contains a host path", err.Error())
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Fatalf("expected %s to be kept: %v", filepath.Base(keep), statErr)
	}
}

// A missing scion-agent.json and an empty external home do not make the
// agent dir stale when its workspace worktree has an uncommitted change: the
// dir is kept and the call returns the conflict error (ptone/scion#4460).
func TestHandleAgentDirWithoutConfig_WorktreeWithUncommittedChangeIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	edited := filepath.Join(ws, "README")
	if err := os.WriteFile(edited, []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, edited)
	if data, _ := os.ReadFile(edited); string(data) != "work in progress\n" {
		t.Fatalf("edited file content = %q, want it unchanged", data)
	}
}

// An untracked file alone is work too: the dir is kept.
func TestHandleAgentDirWithoutConfig_WorktreeWithUntrackedFileIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	untracked := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(untracked, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, untracked)
}

// A detached HEAD on a commit that no branch or remote ref reaches would be
// lost with the worktree: the dir is kept.
func TestHandleAgentDirWithoutConfig_DetachedUnreachableCommitIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "checkout", "-q", "--detach")
	runGitT(t, ws, "commit", "-q", "--allow-empty", "-m", "detached work")

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, ".git"))
}

// A detached HEAD on a commit a branch reaches loses nothing: the dir is
// removed as before.
func TestHandleAgentDirWithoutConfig_DetachedReachableCommitIsRemoved(t *testing.T) {
	_, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "checkout", "-q", "--detach")

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// A clean worktree on a branch holds no work the branch does not keep: the
// dir is removed and its worktree entry pruned, as before.
func TestHandleAgentDirWithoutConfig_CleanWorktreeOnBranchIsRemoved(t *testing.T) {
	_, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "commit", "-q", "--allow-empty", "-m", "committed work")
	gitRuns := countWorktreeGit(t)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if *gitRuns == 0 {
		t.Error("expected a worktree of the project repository to be inspected with git")
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
	if _, err := os.Stat(worktreeAdminDir(repo, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("expected the worktree entry to be pruned (stat err=%v)", err)
	}
	if got := runGitT(t, repo, "log", "-1", "--format=%s", "a"); got != "committed work" {
		t.Fatalf("branch a tip = %q, want the committed work to stay on the branch", got)
	}
}

// No home, no record and no worktree: the dir is still removed.
func TestHandleAgentDirWithoutConfig_NoHomeNoWorktreeIsRemoved(t *testing.T) {
	base := isolateGitTestEnv(t)
	agentDir := filepath.Join(base, "project", ".scion", "agents", "a")
	agentHome := filepath.Join(base, "external", "a", "home")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "leftover.txt"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, filepath.Join(agentDir, "workspace")); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// A worktree of the project repository that git cannot read (its gitdir is
// an empty directory under .git/worktrees/) may hold work: the check fails
// closed and the dir is kept.
func TestHandleAgentDirWithoutConfig_UncheckableWorktreeIsKept(t *testing.T) {
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	broken := filepath.Join(repo, ".git", "worktrees", "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(ws, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+broken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitFile)
	if *gitRuns == 0 {
		t.Error("expected git to run (and fail) on a worktree that belongs to the project repository")
	}
}

// A standalone clone (a .git directory) keeps its own branches and stash,
// which removing it would delete: it is kept even when clean and on a
// branch, without running git.
func TestHandleAgentDirWithoutConfig_StandaloneCloneIsKept(t *testing.T) {
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	runGitT(t, base, "clone", "-q", repo, ws)
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, ".git"))
	if *gitRuns != 0 {
		t.Errorf("git ran %d times on a standalone clone, want 0", *gitRuns)
	}
}

// A .git pointer to a worktree of another repository is not inspected: the
// dir is kept and no git runs.
func TestHandleAgentDirWithoutConfig_PointerOutsideProjectRepoIsKeptWithoutGit(t *testing.T) {
	base, _, agentDir, agentHome, ws := noConfigGitProject(t)
	other := filepath.Join(base, "other")
	initTestRepo(t, other)
	otherWt := filepath.Join(base, "other-wt")
	runGitT(t, other, "worktree", "add", "-q", "-b", "x", otherWt)
	pointer, err := os.ReadFile(filepath.Join(otherWt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(ws, ".git")
	if err := os.WriteFile(gitFile, pointer, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err = handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitFile)
	if *gitRuns != 0 {
		t.Errorf("git ran %d times for a worktree of another repository, want 0", *gitRuns)
	}
}

// A gitdir that sits under the project's .git/worktrees/ only by a symlink
// is compared after resolving it: it is kept and no git runs.
func TestHandleAgentDirWithoutConfig_SymlinkedGitdirOutsideProjectRepoIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	other := filepath.Join(base, "other")
	initTestRepo(t, other)
	otherWt := filepath.Join(base, "other-wt")
	runGitT(t, other, "worktree", "add", "-q", "-b", "x", otherWt)
	worktrees := filepath.Join(repo, ".git", "worktrees")
	if err := os.MkdirAll(worktrees, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(worktrees, "linked")
	if err := os.Symlink(filepath.Join(other, ".git", "worktrees", "other-wt"), link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(ws, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+link+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitFile)
	if *gitRuns != 0 {
		t.Errorf("git ran %d times for a symlinked gitdir outside the project repository, want 0", *gitRuns)
	}
}

// A project checked out in a linked worktree (its .git is a pointer file)
// resolves to the main repository's common dir, where provisioning adds the
// agent's worktree: a clean one is inspected and removed as before.
func TestHandleAgentDirWithoutConfig_ProjectInLinkedWorktreeIsInspected(t *testing.T) {
	base := isolateGitTestEnv(t)
	mainRepo := filepath.Join(base, "main")
	initTestRepo(t, mainRepo)
	project := filepath.Join(base, "project-wt")
	runGitT(t, mainRepo, "worktree", "add", "-q", "--relative-paths", "-b", "project", project)
	agentDir := filepath.Join(project, ".scion", "agents", "a")
	agentHome := filepath.Join(base, "external", "a", "home")
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := filepath.Join(agentDir, "workspace")
	runGitT(t, mainRepo, "worktree", "add", "-q", "--relative-paths", "-b", "a", ws)
	t.Chdir(base)
	gitRuns := countWorktreeGit(t)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if *gitRuns == 0 {
		t.Error("expected the worktree to be inspected with git")
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// A workspace with files but no .git may hold work: it is kept.
func TestHandleAgentDirWithoutConfig_PlainWorkspaceWithFilesIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := noConfigGitProject(t)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(file, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, file)
}

// An empty workspace with no .git holds nothing: the dir is removed.
func TestHandleAgentDirWithoutConfig_EmptyPlainWorkspaceIsRemoved(t *testing.T) {
	_, _, agentDir, agentHome, ws := noConfigGitProject(t)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// pruneWorktreesForWorkspace with an empty workspace prunes nothing, even
// when the current directory is a repository with a stale worktree entry,
// and reports that it found no repository (ptone/scion#4511).
func TestPruneWorktreesForWorkspace_EmptyWorkspaceDoesNotPrune(t *testing.T) {
	base := isolateGitTestEnv(t)
	cwdRepo := filepath.Join(base, "caller")
	initTestRepo(t, cwdRepo)
	stale := filepath.Join(base, "stale-wt")
	runGitT(t, cwdRepo, "worktree", "add", "-q", "-b", "stale", stale)
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwdRepo)

	if pruneWorktreesForWorkspace("") {
		t.Error("pruneWorktreesForWorkspace(\"\") = true, want false")
	}
	if _, err := os.Stat(worktreeAdminDir(cwdRepo, "stale-wt")); err != nil {
		t.Fatalf("expected the stale worktree entry in the current directory's repository to survive: %v", err)
	}
}

// pruneWorktreesForWorkspace with a workspace path prunes the repository
// holding it, wherever the process runs, and reports that it found it.
func TestPruneWorktreesForWorkspace_WorkspacePrunesItsRepo(t *testing.T) {
	base := isolateGitTestEnv(t)
	repo := filepath.Join(base, "project")
	initTestRepo(t, repo)
	stale := filepath.Join(repo, ".scion", "agents", "a", "workspace")
	runGitT(t, repo, "worktree", "add", "-q", "-b", "a", stale)
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)

	if !pruneWorktreesForWorkspace(stale) {
		t.Error("pruneWorktreesForWorkspace = false, want true for a workspace in a repository")
	}
	if _, err := os.Stat(worktreeAdminDir(repo, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("expected the stale worktree entry to be pruned (stat err=%v)", err)
	}
}

// A workspace outside any repository has nothing to prune: the helper
// reports false, so the provision call site falls back as before.
func TestPruneWorktreesForWorkspace_NoRepoReportsFalse(t *testing.T) {
	base := isolateGitTestEnv(t)
	t.Chdir(base)
	if pruneWorktreesForWorkspace(filepath.Join(base, "agents", "a", "workspace")) {
		t.Error("pruneWorktreesForWorkspace = true, want false outside a repository")
	}
}

// A shared-workspace agent passes no workspace path, but its state dir can
// still hold an in-dir workspace/ from an earlier worktree-per-agent life.
// A dirty one is kept (ptone/scion#4526).
func TestHandleAgentDirWithoutConfig_SharedModeLeftoverDirtyWorktreeIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	edited := filepath.Join(ws, "README")
	if err := os.WriteFile(edited, []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, "")
	requireKeptWithConflict(t, err, base, edited)
}

// In shared-workspace mode, a leftover in-dir workspace/ with files but no
// .git is kept.
func TestHandleAgentDirWithoutConfig_SharedModeLeftoverPlainWorkspaceIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := noConfigGitProject(t)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(file, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, "")
	requireKeptWithConflict(t, err, base, file)
}

// In shared-workspace mode with no leftover workspace/, the stale dir is
// removed as before.
func TestHandleAgentDirWithoutConfig_SharedModeNoLeftoverIsRemoved(t *testing.T) {
	_, _, agentDir, agentHome, _ := noConfigGitProject(t)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ""); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// Ignored files are a deliberate exception: a clean worktree on a branch
// whose only extra file is ignored is removed, ignored file included.
func TestHandleAgentDirWithoutConfig_CleanWorktreeWithOnlyIgnoredFileIsRemoved(t *testing.T) {
	_, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("*.local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, ws, "add", ".gitignore")
	runGitT(t, ws, "commit", "-q", "-m", "ignore local files")
	if err := os.WriteFile(filepath.Join(ws, "settings.local"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// A staged-only change is work: the dir is kept.
func TestHandleAgentDirWithoutConfig_StagedOnlyChangeIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	staged := filepath.Join(ws, "staged.txt")
	if err := os.WriteFile(staged, []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, ws, "add", "staged.txt")

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, staged)
}

// A dirty submodule is reported even when the repository config hides it
// from git status: the dir is kept.
func TestHandleAgentDirWithoutConfig_DirtySubmoduleHiddenByConfigIsKept(t *testing.T) {
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	sub := filepath.Join(base, "subrepo")
	initTestRepo(t, sub)
	runGitT(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	runGitT(t, repo, "commit", "-q", "-m", "add submodule")
	runGitT(t, repo, "worktree", "add", "-q", "--relative-paths", "-b", "a", ws)
	runGitT(t, ws, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	runGitT(t, repo, "config", "submodule.sub.ignore", "all")
	runGitT(t, repo, "config", "diff.ignoreSubmodules", "all")
	edited := filepath.Join(ws, "sub", "README")
	if err := os.WriteFile(edited, []byte("submodule work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: git status without --ignore-submodules=none = %q, want the submodule hidden", got)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, edited)
}

// GIT_* variables in the process environment do not reach the workspace
// check: config injected through GIT_CONFIG_COUNT that would ignore every
// file does not hide an untracked file, so the dir is kept.
func TestHandleAgentDirWithoutConfig_InheritedGitEnvIsIgnored(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	untracked := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(untracked, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	excludeAll := filepath.Join(base, "exclude-all")
	if err := os.WriteFile(excludeAll, []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_0", excludeAll)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, untracked)
}

// A git command that exceeds the timeout fails the check, so the dir is
// kept, even for a clean worktree on a branch.
func TestHandleAgentDirWithoutConfig_GitTimeoutIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	orig := worktreeGitTimeout
	worktreeGitTimeout = time.Nanosecond
	t.Cleanup(func() { worktreeGitTimeout = orig })

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, ".git"))
	if !strings.Contains(err.Error(), "could not check") {
		t.Errorf("error = %q, want the could-not-check text", err.Error())
	}
}

// A .git that is a symlink is not inspected: the dir is kept and no git
// runs.
func TestHandleAgentDirWithoutConfig_SymlinkedDotGitIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitLink := filepath.Join(ws, ".git")
	if err := os.Symlink(filepath.Join(repo, ".git"), gitLink); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitLink)
	if *gitRuns != 0 {
		t.Errorf("git ran %d times for a symlinked .git, want 0", *gitRuns)
	}
}

// A workspace that is itself a symlink is kept, and no git runs.
func TestHandleAgentDirWithoutConfig_SymlinkedWorkspaceIsKeptWithoutGit(t *testing.T) {
	base, _, agentDir, agentHome, ws := noConfigGitProject(t)
	target := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ws); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, ws)
	if *gitRuns != 0 {
		t.Errorf("git ran %d times for a symlinked workspace, want 0", *gitRuns)
	}
}

// A workspace that is a regular file is kept, and no git runs.
func TestHandleAgentDirWithoutConfig_RegularFileWorkspaceIsKeptWithoutGit(t *testing.T) {
	base, _, agentDir, agentHome, ws := noConfigGitProject(t)
	if err := os.WriteFile(ws, []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, ws)
	if *gitRuns != 0 {
		t.Errorf("git ran %d times for a regular-file workspace, want 0", *gitRuns)
	}
}

// A rebase stopped after rewriting a commit leaves a clean tree and a
// detached HEAD that no branch reaches: the dir is kept.
func TestHandleAgentDirWithoutConfig_MidRebaseRewrittenHeadIsKept(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, repo, "commit", "-q", "--allow-empty", "-m", "main moves on")
	if err := os.WriteFile(filepath.Join(ws, "f1"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, ws, "add", "f1")
	runGitT(t, ws, "commit", "-q", "-m", "agent work")
	// --exec false stops the rebase right after the first pick.
	if out, err := exec.Command("git", "-C", ws, "rebase", "-q", "--exec", "false", "main").CombinedOutput(); err == nil {
		t.Fatalf("setup: expected the rebase to stop at the failing exec\n%s", out)
	}
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: worktree not clean mid-rebase: %q", got)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, "f1"))
}
