package agent

import (
	"bytes"
	"errors"
	"log/slog"
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
	runWorktreeGit = func(wt projectWorktree, args ...string) (string, error) {
		n++
		return orig(wt, args...)
	}
	t.Cleanup(func() { runWorktreeGit = orig })
	return &n
}

// recordWorktreeGit wraps runWorktreeGit for the rest of the test and
// returns a pointer to the git subcommands it ran, in order.
func recordWorktreeGit(t *testing.T) *[]string {
	t.Helper()
	var subcommands []string
	orig := runWorktreeGit
	runWorktreeGit = func(wt projectWorktree, args ...string) (string, error) {
		if len(args) > 0 {
			subcommands = append(subcommands, args[0])
		}
		return orig(wt, args...)
	}
	t.Cleanup(func() { runWorktreeGit = orig })
	return &subcommands
}

// countSubcommand returns how many times name appears in subcommands.
func countSubcommand(subcommands []string, name string) int {
	n := 0
	for _, s := range subcommands {
		if s == name {
			n++
		}
	}
	return n
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
	// The metadata points back to the project repository, so the check
	// trusts it, but the admin dir has no HEAD, so git fails on it.
	files := map[string]string{
		gitFile:                            "gitdir: " + broken,
		filepath.Join(broken, "commondir"): filepath.Join(repo, ".git"),
		filepath.Join(broken, "gitdir"):    gitFile,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, gitFile)
	if !strings.Contains(err.Error(), "could not check") {
		t.Errorf("error = %v, want the could-not-check conflict", err)
	}
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

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "ignored_count=1") || !strings.Contains(logs, "settings.local") {
		t.Errorf("logs = %q, want a Warn with the ignored file count and path", logs)
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
// check: config set through GIT_CONFIG_COUNT that would ignore every
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

// requireKeptWithoutGit checks that the dir was kept with the conflict error
// and that no git command ran on the workspace.
func requireKeptWithoutGit(t *testing.T, err error, base, keep string, gitRuns *int) {
	t.Helper()
	requireKeptWithConflict(t, err, base, keep)
	if *gitRuns != 0 {
		t.Errorf("git ran %d times, want 0", *gitRuns)
	}
}

// Worktree metadata whose commondir names a directory other than the project
// repository's common dir is not trusted: the dir is kept and no git runs.
func TestHandleAgentDirWithoutConfig_CommondirElsewhereIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeAdminDir(repo, "workspace"), "commondir"), []byte(elsewhere+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// Worktree metadata with no commondir file is not trusted either.
func TestHandleAgentDirWithoutConfig_MissingCommondirIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	if err := os.Remove(filepath.Join(worktreeAdminDir(repo, "workspace"), "commondir")); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// A config.worktree file in the worktree's metadata is not trusted: the dir
// is kept and no git runs.
func TestHandleAgentDirWithoutConfig_ConfigWorktreePresentIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	if err := os.WriteFile(filepath.Join(worktreeAdminDir(repo, "workspace"), "config.worktree"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// A gitdir nested two levels under the project's .git/worktrees/ is not a
// worktree git registered, even with consistent metadata files: the dir is
// kept and no git runs.
func TestHandleAgentDirWithoutConfig_NestedGitdirIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	admin := worktreeAdminDir(repo, "workspace")
	nested := filepath.Join(admin, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HEAD", "index"} {
		data, err := os.ReadFile(filepath.Join(admin, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitFile := filepath.Join(ws, ".git")
	files := map[string]string{
		filepath.Join(nested, "commondir"): filepath.Join(repo, ".git"),
		filepath.Join(nested, "gitdir"):    gitFile,
		gitFile:                            "gitdir: " + nested,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, gitFile, gitRuns)
}

// Worktree metadata whose gitdir file names another workspace's .git does
// not point back to this workspace: the dir is kept and no git runs.
func TestHandleAgentDirWithoutConfig_GitdirBackPointerToOtherWorkspaceIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	other := filepath.Join(base, "other-ws")
	runGitT(t, repo, "worktree", "add", "-q", "-b", "other", other)
	if err := os.WriteFile(filepath.Join(worktreeAdminDir(repo, "workspace"), "gitdir"), []byte(filepath.Join(other, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// Git runs with the paths the check verified, not by re-reading the
// worktree's metadata: a commondir rewritten after the check to name a copy
// of the repository whose info/exclude ignores every file does not hide an
// untracked file, so the dir is kept.
func TestHandleAgentDirWithoutConfig_MetadataChangedAfterCheckIsNotFollowed(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	untracked := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(untracked, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyCommon := filepath.Join(base, "copy-common")
	if out, err := exec.Command("cp", "-R", filepath.Join(repo, ".git"), copyCommon).CombinedOutput(); err != nil {
		t.Fatalf("copy the common dir: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(copyCommon, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyCommon, "info", "exclude"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commondir := filepath.Join(worktreeAdminDir(repo, "workspace"), "commondir")
	orig := runWorktreeGit
	runWorktreeGit = func(wt projectWorktree, args ...string) (string, error) {
		if err := os.WriteFile(commondir, []byte(copyCommon+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return orig(wt, args...)
	}
	t.Cleanup(func() { runWorktreeGit = orig })

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, untracked)
}

// In shared-workspace mode, a clean leftover in-dir worktree of the project
// repository is removed and its worktree entry pruned through the in-dir
// path, so its branch is free again. The repository of the process's
// current directory is not pruned (ptone/scion#4511).
func TestHandleAgentDirWithoutConfig_SharedModeCleanLeftoverIsRemovedAndPruned(t *testing.T) {
	base, repo, agentDir, agentHome, _ := staleWorktreeLayout(t)
	cwdRepo := filepath.Join(base, "caller")
	initTestRepo(t, cwdRepo)
	stale := filepath.Join(base, "stale-wt")
	runGitT(t, cwdRepo, "worktree", "add", "-q", "-b", "stale", stale)
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwdRepo)
	gitRuns := countWorktreeGit(t)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ""); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if *gitRuns == 0 {
		t.Error("expected the leftover worktree to be inspected with git")
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
	if _, err := os.Stat(worktreeAdminDir(repo, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("expected the leftover worktree entry to be pruned (stat err=%v)", err)
	}
	if _, err := os.Stat(worktreeAdminDir(cwdRepo, "stale-wt")); err != nil {
		t.Fatalf("expected the stale worktree entry in the current directory's repository to survive: %v", err)
	}
}

// An edit to a file with the assume-unchanged bit does not show in git
// status, so the bit makes a clean status untrustworthy: the dir is kept.
func TestHandleAgentDirWithoutConfig_AssumeUnchangedEditIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "update-index", "--assume-unchanged", "README")
	edited := filepath.Join(ws, "README")
	if err := os.WriteFile(edited, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("precondition: git status = %q, want the edit hidden", got)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, edited)
	if data, _ := os.ReadFile(edited); string(data) != "local edit\n" {
		t.Fatalf("edited file content = %q, want it unchanged", data)
	}
}

// The same holds for the skip-worktree bit: the dir is kept.
func TestHandleAgentDirWithoutConfig_SkipWorktreeEditIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "update-index", "--skip-worktree", "README")
	edited := filepath.Join(ws, "README")
	if err := os.WriteFile(edited, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("precondition: git status = %q, want the edit hidden", got)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, edited)
}

// A clean worktree whose index bits were set and cleared again has neither
// bit: the dir is removed.
func TestHandleAgentDirWithoutConfig_ClearedIndexBitsAreRemoved(t *testing.T) {
	_, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "update-index", "--assume-unchanged", "--skip-worktree", "README")
	runGitT(t, ws, "update-index", "--no-assume-unchanged", "--no-skip-worktree", "README")

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// A global excludes file (set in the global config, or the default
// XDG location) does not hide an untracked file: the dir is kept.
func TestHandleAgentDirWithoutConfig_GlobalExcludesDoNotHideUntrackedFile(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	home := os.Getenv("HOME")
	excludes := filepath.Join(home, "global-excludes")
	if err := os.WriteFile(excludes, []byte("notes.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\texcludesFile = "+excludes+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	xdgIgnore := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git", "ignore")
	if err := os.MkdirAll(filepath.Dir(xdgIgnore), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xdgIgnore, []byte("draft.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(ws, "notes.txt")
	if err := os.WriteFile(untracked, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("precondition: git status = %q, want the global excludes file to hide the untracked file", got)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, untracked)
}

// With per-worktree config on in the project repository, a clean worktree
// is kept and no git runs in it.
func TestHandleAgentDirWithoutConfig_WorktreeConfigExtensionIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, repo, "config", "extensions.worktreeConfig", "true")
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// extensions.worktreeConfig set to false changes nothing: a clean worktree
// is inspected and removed.
func TestHandleAgentDirWithoutConfig_WorktreeConfigExtensionOffIsInspected(t *testing.T) {
	_, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, repo, "config", "extensions.worktreeConfig", "false")
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

// A locked worktree is one git worktree lock marked as not to be removed:
// the dir is kept and no git runs in it.
func TestHandleAgentDirWithoutConfig_LockedWorktreeIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, repo, "worktree", "lock", ws)
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// A worktree whose index has a submodule (gitlink) entry is kept even when
// clean and on a branch, and git status never runs in it.
func TestHandleAgentDirWithoutConfig_SubmoduleWorktreeIsKeptWithoutStatus(t *testing.T) {
	base, repo, agentDir, agentHome, ws := noConfigGitProject(t)
	sub := filepath.Join(base, "subrepo")
	initTestRepo(t, sub)
	runGitT(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	runGitT(t, repo, "commit", "-q", "-m", "add submodule")
	runGitT(t, repo, "worktree", "add", "-q", "--relative-paths", "-b", "a", ws)
	runGitT(t, ws, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: git status = %q, want a clean worktree", got)
	}
	subcommands := recordWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, ".git"))
	if n := countSubcommand(*subcommands, "ls-files"); n == 0 {
		t.Errorf("git subcommands = %q, want the index listed", *subcommands)
	}
	if n := countSubcommand(*subcommands, "status"); n != 0 {
		t.Errorf("git subcommands = %q, want no git status", *subcommands)
	}
}

// A worktree without a submodule entry is inspected as before: the index is
// listed, then git status runs, and a clean worktree on a branch is removed.
func TestHandleAgentDirWithoutConfig_WorktreeWithoutSubmoduleRunsStatus(t *testing.T) {
	_, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	subcommands := recordWorktreeGit(t)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	// Once for the check, once to list the ignored files before removal.
	if n := countSubcommand(*subcommands, "status"); n != 2 {
		t.Errorf("git subcommands = %q, want git status to run twice", *subcommands)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// A per-worktree config setting git cannot read as a boolean fails the
// check: the dir is kept with the could-not-check conflict and no git runs
// in the worktree.
func TestHandleAgentDirWithoutConfig_UnreadableWorktreeConfigSettingIsKept(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, repo, "config", "extensions.worktreeConfig", "maybe")
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, ".git"))
	if !strings.Contains(err.Error(), "could not check") {
		t.Errorf("error = %v, want the could-not-check conflict", err)
	}
	if *gitRuns != 0 {
		t.Errorf("git ran %d times in the worktree, want 0", *gitRuns)
	}
}

// HEAD on a symbolic ref outside refs/heads/ is not on a branch: a commit
// that only that ref reaches would be lost, so the dir is kept.
func TestHandleAgentDirWithoutConfig_NonBranchSymbolicHeadIsKept(t *testing.T) {
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "checkout", "-q", "--detach")
	runGitT(t, ws, "commit", "-q", "--allow-empty", "-m", "work on no branch")
	runGitT(t, ws, "update-ref", "refs/worktree/x", "HEAD")
	runGitT(t, ws, "symbolic-ref", "HEAD", "refs/worktree/x")
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: git status = %q, want a clean worktree", got)
	}

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, filepath.Join(ws, ".git"))
}

// A rebase paused with --autostash before any commit is rewritten leaves a
// clean tree and HEAD on a commit the branch reaches, while the uncommitted
// changes live only in the rebase state of the worktree's admin dir: the
// dir is kept and no git runs.
func TestHandleAgentDirWithoutConfig_PausedAutostashRebaseIsKeptWithoutGit(t *testing.T) {
	base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "commit", "-q", "--allow-empty", "-m", "agent work")
	if err := os.WriteFile(filepath.Join(ws, "README"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", ws, "rebase", "-q", "-i", "--autostash", "HEAD~1")
	cmd.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR=sed -i 1s/^pick/edit/")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup: rebase: %v\n%s", err, out)
	}
	if got := runGitT(t, ws, "status", "--porcelain"); got != "" {
		t.Fatalf("setup: git status = %q, want a clean worktree", got)
	}
	if _, err := os.Stat(filepath.Join(worktreeAdminDir(repo, "workspace"), "rebase-merge", "autostash")); err != nil {
		t.Fatalf("setup: expected the rebase to hold an autostash: %v", err)
	}
	gitRuns := countWorktreeGit(t)

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
}

// Any admin dir entry an idle worktree does not have, such as the state of
// an in-progress operation or a per-worktree ref, keeps the dir without
// running git.
func TestHandleAgentDirWithoutConfig_InProgressAdminStateIsKeptWithoutGit(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  bool
	}{
		{"rebase-merge", true},
		{"rebase-apply", true},
		{"MERGE_HEAD", false},
		{"MERGE_AUTOSTASH", false},
		{"CHERRY_PICK_HEAD", false},
		{"REVERT_HEAD", false},
		{"sequencer", true},
		{"BISECT_LOG", false},
		{"refs/bisect/bad", false},
		{"refs/worktree/x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
			p := filepath.Join(worktreeAdminDir(repo, "workspace"), filepath.FromSlash(tc.name))
			if tc.dir {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				head := runGitT(t, ws, "rev-parse", "HEAD")
				if err := os.WriteFile(p, []byte(head+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			gitRuns := countWorktreeGit(t)

			err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
			requireKeptWithoutGit(t, err, base, filepath.Join(ws, ".git"), gitRuns)
		})
	}
}

// The admin dir entries an idle worktree gains from normal use (a commit,
// a fetch, a stash push and pop, a per-worktree ref created and deleted)
// do not keep a clean worktree on a branch: the dir is removed.
func TestHandleAgentDirWithoutConfig_IdleAdminEntriesAreRemoved(t *testing.T) {
	_, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	runGitT(t, ws, "commit", "-q", "--allow-empty", "-m", "committed work")
	runGitT(t, ws, "fetch", "-q", repo, "main")
	if err := os.WriteFile(filepath.Join(ws, "README"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, ws, "stash", "-q")
	runGitT(t, ws, "stash", "pop", "-q")
	runGitT(t, ws, "checkout", "-q", "README")
	runGitT(t, ws, "update-ref", "refs/worktree/x", "HEAD")
	runGitT(t, ws, "update-ref", "-d", "refs/worktree/x")
	admin := worktreeAdminDir(repo, "workspace")
	for _, name := range []string{"COMMIT_EDITMSG", "FETCH_HEAD", "ORIG_HEAD", "refs"} {
		if _, err := os.Stat(filepath.Join(admin, name)); err != nil {
			t.Fatalf("setup: expected %s in the worktree admin dir: %v", name, err)
		}
	}

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
}

// git status skips an untracked directory it cannot read, warns on stderr
// and exits 0: the check fails closed and the dir is kept.
func TestHandleAgentDirWithoutConfig_UnreadableUntrackedDirIsKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode 000 directory")
	}
	base, _, agentDir, agentHome, ws := staleWorktreeLayout(t)
	unreadable := filepath.Join(ws, "private")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unreadable, "notes"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })

	err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws)
	requireKeptWithConflict(t, err, base, unreadable)
	if !strings.Contains(err.Error(), "could not check") {
		t.Errorf("error = %v, want the could-not-check conflict", err)
	}
}
