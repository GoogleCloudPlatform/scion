package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
