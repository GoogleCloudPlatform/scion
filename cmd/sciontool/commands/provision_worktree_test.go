/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// provisionTestRepo creates a bare repository with one commit on main and
// returns its path, for use as SCION_CLONE_URL.
func provisionTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "origin.git")
	gitT(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(dir, "work")
	gitT(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "README.md")
	gitT(t, work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	gitT(t, work, "push", "origin", "main")
	return bare
}

// gitT runs git (in dir when set) and returns its trimmed output.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// setupProvisionCmd points runProvision at workspace with the current
// user's uid/gid and the given --mode flag value, clears the env it reads
// (the test process may inherit some of it), and restores everything after
// the test.
func setupProvisionCmd(t *testing.T, workspace, modeFlag, cloneURL string) {
	t.Helper()
	oldWorkspace, oldMode, oldDepth, oldUID, oldGID := provisionWorkspace, provisionMode, provisionDepth, provisionUID, provisionGID
	t.Cleanup(func() {
		provisionWorkspace, provisionMode, provisionDepth, provisionUID, provisionGID = oldWorkspace, oldMode, oldDepth, oldUID, oldGID
	})
	provisionWorkspace = workspace
	provisionMode = modeFlag
	provisionDepth = 0
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()
	t.Setenv("SCION_CLONE_URL", cloneURL)
	t.Setenv("SCION_CLONE_BRANCH", "main")
	t.Setenv("SCION_PROJECT_ID", "proj-1")
	t.Setenv("SCION_SHARED_DIR_PATHS", "")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_AGENT_SLUG", "")
	t.Setenv("SCION_AGENT_BRANCH", "")
	t.Setenv(provision.ChownBestEffortEnv, "")
	t.Setenv(provisionStateDirEnv, "")
	// Worktree mode sets GIT_CONFIG_* for its git commands; restore them.
	for _, key := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// setWorktreeEnv sets the env the Kubernetes runtime passes to the
// provisioning init container in worktree-per-agent mode.
func setWorktreeEnv(t *testing.T, agentSlug, branch string) {
	t.Helper()
	t.Setenv("SCION_WORKSPACE_MODE", "worktree-per-agent")
	t.Setenv("SCION_AGENT_SLUG", agentSlug)
	t.Setenv("SCION_AGENT_BRANCH", branch)
}

// assertAgentWorktree checks that workspace/worktrees/<agent name> is a git
// worktree of the shared checkout, on branch, linked with relative paths
// (so it also resolves at /repo-root in the agent container).
func assertAgentWorktree(t *testing.T, workspace, name, branch string) {
	t.Helper()
	wt := provision.WorktreePath(workspace, name)
	if !provision.IsRealWorktreeDir(wt, workspace) {
		t.Fatalf("%s is not a worktree of %s", wt, workspace)
	}
	gitFile, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(gitFile)), "gitdir: ../../.git/worktrees/"+name; got != want {
		t.Errorf(".git file = %q, want %q", got, want)
	}
	if got := gitT(t, wt, "branch", "--show-current"); got != branch {
		t.Errorf("worktree branch = %q, want %q", got, branch)
	}
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Errorf("worktree has no checked-out files: %v", err)
	}
}

// First start of a worktree-per-agent project: the init container clones
// the shared checkout and adds the agent's worktree. SCION_WORKSPACE_MODE
// takes precedence over the --mode flag.
func TestRunProvision_WorktreeMode_FirstStartAddsWorktree(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "agent-one")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspace, provision.ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not written: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "agent-one")
	if got := gitT(t, workspace, "config", "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q, want 0", got)
	}
	exclude, _ := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	if !strings.Contains(string(exclude), "\nworktrees/\n") {
		t.Errorf("worktrees/ not excluded:\n%s", exclude)
	}
	if status := gitT(t, workspace, "status", "--porcelain", "--untracked-files=all"); strings.Contains(status, "worktrees") {
		t.Errorf("worktrees show up in the shared checkout's status:\n%s", status)
	}
	// A shared checkout first provisioned in this mode has a detached HEAD,
	// so every branch is free for the agents' worktrees.
	if got := gitT(t, workspace, "branch", "--show-current"); got != "" {
		t.Errorf("shared checkout branch = %q, want a detached HEAD", got)
	}
	// The git commands of this run list exactly the shared checkout and the
	// agent's worktree as safe directories.
	want := map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "safe.directory",
		"GIT_CONFIG_VALUE_0": workspace,
		"GIT_CONFIG_KEY_1":   "safe.directory",
		"GIT_CONFIG_VALUE_1": provision.WorktreePath(workspace, "agent-1"),
	}
	for key, value := range want {
		if got := os.Getenv(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

// The branch is used as given, slashes and capitals included.
func TestRunProvision_WorktreeMode_BranchWithSlash(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "feature/Login-Fix")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "feature/Login-Fix")
}

// The safe.directory entries name exactly the shared checkout and the
// agent's worktree, after any GIT_CONFIG_* entries already set.
func TestWorktreeSafeDirectoryEnv(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(key string) string { return vars[key] }
	}
	got := worktreeSafeDirectoryEnv(env(nil), "/workspace", "agent-1")
	want := map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "safe.directory",
		"GIT_CONFIG_VALUE_0": "/workspace",
		"GIT_CONFIG_KEY_1":   "safe.directory",
		"GIT_CONFIG_VALUE_1": "/workspace/worktrees/agent-1",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}

	got = worktreeSafeDirectoryEnv(env(map[string]string{"GIT_CONFIG_COUNT": "1"}), "/workspace", "agent-1")
	if got["GIT_CONFIG_COUNT"] != "3" || got["GIT_CONFIG_VALUE_1"] != "/workspace" ||
		got["GIT_CONFIG_VALUE_2"] != "/workspace/worktrees/agent-1" || got["GIT_CONFIG_KEY_0"] != "" {
		t.Errorf("existing entries not kept: %v", got)
	}
}

// In worktree mode the branch is passed through and the lock wait is the
// --timeout value.
func TestSetWorktreeInput(t *testing.T) {
	var in provision.ProvisionInput
	setWorktreeInput(&in, "agent-1", "feature/x", 300)
	if in.AgentID != "agent-1" || in.AgentName != "feature/x" || !in.MountedWorktree {
		t.Errorf("unexpected input %+v", in)
	}
	if in.LockWait != 300*time.Second {
		t.Errorf("LockWait = %s, want 5m0s", in.LockWait)
	}
}

// Later starts: a second agent gets its own worktree in the empty directory
// the broker created for its mount, and re-running for the first agent
// keeps its worktree and its work.
func TestRunProvision_WorktreeMode_LaterStarts(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("first agent: %v", err)
	}
	work := filepath.Join(provision.WorktreePath(workspace, "agent-1"), "work.txt")
	if err := os.WriteFile(work, []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(provision.WorktreePath(workspace, "agent-2"), 0o770); err != nil {
		t.Fatal(err)
	}
	setWorktreeEnv(t, "agent-2", "agent-two")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("second agent into a pre-created empty directory: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-2", "agent-two")

	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("restart of the first agent: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "agent-one")
	if _, err := os.Stat(work); err != nil {
		t.Errorf("the first agent's work is gone after a restart: %v", err)
	}
}

// Without SCION_WORKSPACE_MODE the command provisions the shared checkout
// only, as before: no worktrees directory.
func TestRunProvision_SharedPlain_NoWorktree(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	t.Setenv("SCION_AGENT_SLUG", "agent-1")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "worktrees")); !os.IsNotExist(err) {
		t.Errorf("shared-plain must not create worktrees/, stat err = %v", err)
	}
	if got := gitT(t, workspace, "branch", "--show-current"); got != "main" {
		t.Errorf("shared checkout branch = %q, want main", got)
	}
}

// A project first provisioned in shared-plain mode keeps its checkout (HEAD
// and files) when an agent starts in worktree-per-agent mode; only the
// worktrees/ exclude and gc.auto 0 are added.
func TestRunProvision_WorktreeMode_ExistingSharedCheckoutKept(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("shared-plain provisioning: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("worktree mode on an existing shared checkout: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "agent-one")
	if got := gitT(t, workspace, "branch", "--show-current"); got != "main" {
		t.Errorf("shared checkout branch = %q, want main (left as it was)", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, "notes.txt")); err != nil {
		t.Errorf("shared checkout content changed: %v", err)
	}
	if got := gitT(t, workspace, "config", "gc.auto"); got != "0" {
		t.Errorf("gc.auto = %q, want 0", got)
	}
	exclude, _ := os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	if strings.Count(string(exclude), "\nworktrees/\n") != 1 {
		t.Errorf("want exactly one worktrees/ exclude line:\n%s", exclude)
	}

	// Running again adds nothing twice.
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	exclude, _ = os.ReadFile(filepath.Join(workspace, ".git", "info", "exclude"))
	if strings.Count(string(exclude), "\nworktrees/\n") != 1 {
		t.Errorf("want exactly one worktrees/ exclude line after a second run:\n%s", exclude)
	}
}

// An agent whose branch is checked out in the shared checkout gets an
// error that says what to do, and no worktree.
func TestRunProvision_WorktreeMode_BranchCheckedOutInSharedCheckout(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("shared-plain provisioning: %v", err)
	}

	setWorktreeEnv(t, "agent-1", "main")
	err := runProvision(context.Background())
	if err == nil {
		t.Fatal("expected an error for a branch checked out in the shared checkout")
	}
	for _, want := range []string{`branch "main" is checked out in the project's shared checkout`, "switch --detach", "start the agent with a different branch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if provision.IsRealWorktreeDir(provision.WorktreePath(workspace, "agent-1"), workspace) {
		t.Error("no worktree should be added")
	}
}

// A branch already checked out in another agent's worktree cannot be used:
// the pod mounts only the agent's own directory.
func TestRunProvision_WorktreeMode_BranchInAnotherWorktree(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "shared-branch")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("first agent: %v", err)
	}

	setWorktreeEnv(t, "agent-2", "shared-branch")
	assertBranchInOtherWorktreeError(t, runProvision(context.Background()))

	// The same without the sharer registry, found through git's worktree
	// list instead.
	sharers, err := filepath.Glob(filepath.Join(workspace, ".git", "*sharer*"))
	if err != nil || len(sharers) == 0 {
		t.Fatalf("sharer registry not found under .git (err=%v)", err)
	}
	for _, p := range sharers {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	assertBranchInOtherWorktreeError(t, runProvision(context.Background()))
	if _, err := os.Stat(provision.WorktreePath(workspace, "agent-2")); !os.IsNotExist(err) {
		t.Errorf("no directory should be created for the second agent, stat err = %v", err)
	}
}

func assertBranchInOtherWorktreeError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error for a branch used by another worktree")
	}
	for _, want := range []string{`branch "shared-branch" is already checked out in worktrees/agent-1 of the shared checkout of project proj-1`, "different branch", "git worktree remove worktrees/agent-1, run in the shared checkout", "on a broker that mounts the export"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// A standalone checkout in the agent's directory (cloned by the agent
// container of an older image) is kept as it is.
func TestRunProvision_WorktreeMode_StandaloneCheckoutKept(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("shared-plain provisioning: %v", err)
	}
	agentDir := provision.WorktreePath(workspace, "agent-1")
	gitT(t, "", "clone", origin, agentDir)
	work := filepath.Join(agentDir, "work.txt")
	if err := os.WriteFile(work, []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(agentDir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("standalone checkout's .git directory replaced: %v", err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Errorf("work in the standalone checkout is gone: %v", err)
	}
}

// Worktree mode needs the agent's slug, which names its directory.
func TestRunProvision_WorktreeMode_RequiresAgentSlug(t *testing.T) {
	for _, slug := range []string{"", "Agent-1", "../x", "a/b", "a.b"} {
		workspace := filepath.Join(t.TempDir(), "workspace")
		setupProvisionCmd(t, workspace, "shared-plain", "")
		setWorktreeEnv(t, slug, "")
		err := runProvision(context.Background())
		if err == nil || !strings.Contains(err.Error(), "SCION_AGENT_SLUG") {
			t.Fatalf("slug %q: expected an error naming SCION_AGENT_SLUG, got %v", slug, err)
		}
		if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
			t.Errorf("slug %q: nothing should be provisioned, stat err = %v", slug, statErr)
		}
	}
}

// Delete then recreate with the same name: deleting the agent removes its
// worktree (what the broker does on delete), so the new agent with the
// same name and branch gets a fresh worktree instead of a branch-in-use
// error. Other agents' worktrees are untouched, and the branch is kept.
func TestRunProvision_WorktreeMode_DeleteThenRecreateSameName(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("first agent: %v", err)
	}
	setWorktreeEnv(t, "agent-2", "agent-two")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("second agent: %v", err)
	}
	old := filepath.Join(provision.WorktreePath(workspace, "agent-1"), "old-work.txt")
	if err := os.WriteFile(old, []byte("from the deleted agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(provision.WorktreePath(workspace, "agent-2"), "other-work.txt")
	if err := os.WriteFile(other, []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RemoveMountedWorktree(context.Background(), workspace, "", "agent-1", 30*time.Second); err != nil {
		t.Fatalf("remove on delete: %v", err)
	}
	if _, err := os.Lstat(provision.WorktreePath(workspace, "agent-1")); !os.IsNotExist(err) {
		t.Fatalf("deleted agent's worktree still present, lstat err = %v", err)
	}
	if got := gitT(t, workspace, "branch", "--list", "agent-one"); got == "" {
		t.Error("the deleted agent's branch should be kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another agent's worktree was touched: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-2", "agent-two")

	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("recreated agent with the same name: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "agent-one")
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("recreated agent should get a fresh worktree, stat err = %v", err)
	}
}

// When the delete left the worktree in place (files kept), an agent
// recreated with the same name reuses it, work included.
func TestRunProvision_WorktreeMode_RecreateSameNameReusesKeptWorktree(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("first agent: %v", err)
	}
	work := filepath.Join(provision.WorktreePath(workspace, "agent-1"), "work.txt")
	if err := os.WriteFile(work, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("recreated agent with the same name: %v", err)
	}
	assertAgentWorktree(t, workspace, "agent-1", "agent-one")
	if _, err := os.Stat(work); err != nil {
		t.Errorf("kept work is gone: %v", err)
	}
}

// Older image: its init container ignores the worktree env and provisions
// the shared checkout only, so the agent's directory (created empty by the
// broker for the mount) stays empty. The agent container's clone step,
// pointed at that directory by SCION_WORKSPACE_PATH, then gives the agent a
// working checkout of its own; a newer init container keeps it later.
func TestOlderImageFallback_AgentContainerClonesIntoItsDirectory(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("shared checkout provisioning: %v", err)
	}
	agentDir := provision.WorktreePath(workspace, "agent-1")
	if err := os.MkdirAll(agentDir, 0o770); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_HUB_TOKEN", "SCION_AGENT_MODE", "GITHUB_TOKEN", "SCION_GIT_DEPTH"} {
		t.Setenv(k, "")
	}
	t.Setenv("SCION_GIT_CLONE_URL", "file://"+origin)
	t.Setenv("SCION_GIT_BRANCH", "main")
	t.Setenv("SCION_AGENT_NAME", "agent-one")
	t.Setenv("SCION_AGENT_BRANCH", "")
	t.Setenv("SCION_WORKSPACE_PATH", agentDir)
	if err := gitCloneWorkspace(os.Getuid(), os.Getgid(), t.TempDir(), false); err != nil {
		t.Fatalf("agent container clone: %v", err)
	}

	if got := gitT(t, agentDir, "rev-parse", "--show-toplevel"); got != agentDir {
		t.Errorf("checkout top level = %q, want the agent's own directory %q", got, agentDir)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "README.md")); err != nil {
		t.Errorf("no checked-out files in the agent's directory: %v", err)
	}
	if got := gitT(t, workspace, "branch", "--show-current"); got != "main" {
		t.Errorf("shared checkout branch = %q, want main (untouched)", got)
	}

	// A newer init container later keeps the agent's checkout.
	setWorktreeEnv(t, "agent-1", "agent-one")
	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("newer init container: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(agentDir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("the agent's own checkout was replaced: %v", err)
	}
}
