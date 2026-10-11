package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	runGitT(t, repo, "worktree", "add", "-q", "-b", "a", ws)
	t.Chdir(base)
	return base, repo, agentDir, agentHome, ws
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

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
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

// A worktree git cannot read (its .git file points at a missing admin dir)
// may hold work: the check fails closed and the dir is kept.
func TestHandleAgentDirWithoutConfig_UncheckableWorktreeIsKept(t *testing.T) {
	base := isolateGitTestEnv(t)
	agentDir := filepath.Join(base, "project", ".scion", "agents", "a")
	agentHome := filepath.Join(base, "external", "a", "home")
	ws := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(ws, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+filepath.Join(base, "missing")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitFile)
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
