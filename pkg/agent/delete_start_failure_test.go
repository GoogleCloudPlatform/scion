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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Tests for DeleteStartFailureFiles: the cleanup after a failed start
// removes only what that start's provisioning added. It keeps every
// branch, and it keeps a worktree the start joined.

func startFailureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// startFailureCommit commits a new file in the worktree wt and returns the
// new commit.
func startFailureCommit(t *testing.T, wt, file string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, file), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	startFailureGit(t, wt, "add", file)
	startFailureGit(t, wt, "commit", "-q", "-m", "work")
	return startFailureGit(t, wt, "rev-parse", "HEAD")
}

// branchTip returns the commit branch points to in the repository at
// repo, or "" when there is no such branch.
func branchTip(t *testing.T, repo, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// reusedBranch provisions agent fix-x under uuid-old, commits on its branch
// fix-x and deletes it keeping the branch, then provisions a new agent
// fix-x under uuid-new, which checks out the existing branch in a new
// worktree. It returns the new worktree and the branch tip.
func reusedBranch(t *testing.T, p *sharerKeyProject) (string, string) {
	t.Helper()
	oldWt := p.add(t, "fix-x", "uuid-old", "fix-x")
	tip := startFailureCommit(t, oldWt, "work.txt")
	if _, err := DeleteHubAgentFiles("fix-x", "uuid-old", p.projectPath, "", false); err != nil {
		t.Fatalf("delete uuid-old keeping its branch: %v", err)
	}
	if got := branchTip(t, p.base, "fix-x"); got != tip {
		t.Fatalf("setup: branch fix-x = %q, want %s", got, tip)
	}
	wt := p.add(t, "fix-x", "uuid-new", "fix-x")
	if got := startFailureGit(t, wt, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("setup: the new worktree is at %s, want the existing branch tip %s", got, tip)
	}
	return wt, tip
}

// leftoverWorktree adds a worktree on branch fix-x at worktrees/leftover
// that no registry entry names, with a commit and an uncommitted file, and
// provisions agent fix-x under uuid-new, which joins it. It returns the
// leftover worktree, the uncommitted file and the branch tip.
func leftoverWorktree(t *testing.T, p *sharerKeyProject) (string, string, string) {
	t.Helper()
	p.add(t, "other", "uuid-other", "other") // clones the shared base
	leftover := filepath.Join(p.base, "worktrees", "leftover")
	startFailureGit(t, p.base, "worktree", "add", "--relative-paths", "-q", "-b", "fix-x", leftover)
	tip := startFailureCommit(t, leftover, "work.txt")
	dirty := filepath.Join(leftover, "uncommitted.txt")
	if err := os.WriteFile(dirty, []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.add(t, "fix-x", "uuid-new", "fix-x")
	_, joined, found, err := provision.FindBranchForAgent(p.base, "", "uuid-new")
	if err != nil || !found || joined != leftover {
		t.Fatalf("setup: uuid-new registered at %q (found %v, err %v), want the joined %s", joined, found, err, leftover)
	}
	return leftover, dirty, tip
}

// A start that checked out an existing branch keeps that branch: the
// cleanup removes the worktree the start added and nothing else.
func TestDeleteStartFailureFiles_ReusedBranchKept(t *testing.T) {
	p := newSharerKeyProject(t)
	wt, tip := reusedBranch(t, p)
	if err := DeleteStartFailureFiles("fix-x", "uuid-new", p.projectPath, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	if got := branchTip(t, p.base, "fix-x"); got != tip {
		t.Errorf("branch fix-x = %q after the start-failure cleanup, want kept at %s", got, tip)
	}
	p.assertTornDown(t, "fix-x", "uuid-new", wt, "fix-x")
}

// A start that joined a worktree no registry entry named keeps it: the
// cleanup only unregisters the key, and the worktree, its uncommitted work,
// git's entry for it and its branch stay.
func TestDeleteStartFailureFiles_JoinedLeftoverKept(t *testing.T) {
	p := newSharerKeyProject(t)
	leftover, dirty, tip := leftoverWorktree(t, p)
	if err := DeleteStartFailureFiles("fix-x", "uuid-new", p.projectPath, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Errorf("the joined worktree's uncommitted work is gone: %v", err)
	}
	if got := branchTip(t, p.base, "fix-x"); got != tip {
		t.Errorf("branch fix-x = %q, want kept at %s", got, tip)
	}
	if list := listWorktrees(t, p.base); !strings.Contains(list, leftover) {
		t.Errorf("git no longer lists the joined worktree %s:\n%s", leftover, list)
	}
	if _, _, found, err := provision.FindBranchForAgent(p.base, "", "uuid-new"); err != nil || found {
		t.Errorf("uuid-new still registered (found %v, err %v)", found, err)
	}
	if _, err := os.Stat(filepath.Join(p.scionDir, "agents", "fix-x")); !os.IsNotExist(err) {
		t.Errorf("agent directory still present: %v", err)
	}
}

// A start that created its branch: the cleanup removes the worktree, git's
// entry for it and the registry entry, and keeps the branch (accepted: it
// holds no commits of its own).
func TestDeleteStartFailureFiles_FreshBranch(t *testing.T) {
	p := newSharerKeyProject(t)
	wt := p.add(t, "dev-a", "uuid-a", "fresh")
	if err := DeleteStartFailureFiles("dev-a", "uuid-a", p.projectPath, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	p.assertTornDown(t, "dev-a", "uuid-a", wt, "fresh")
	if branchTip(t, p.base, "fresh") == "" {
		t.Error("branch fresh deleted, want kept")
	}
}

// While other agents share the worktree, the cleanup detaches the key and
// leaves the worktree, the other sharers and the branch.
func TestDeleteStartFailureFiles_OtherSharersDetach(t *testing.T) {
	p := newSharerKeyProject(t)
	wtA := p.add(t, "dev-a", "uuid-a", "shared")
	p.add(t, "dev-b", "uuid-b", "shared")
	if err := DeleteStartFailureFiles("dev-b", "uuid-b", p.projectPath, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	sharers, _, err := provision.ListSharers(p.base, "", "shared")
	if err != nil || !slices.Equal(sharers, []string{"uuid-a"}) {
		t.Errorf("sharers = %v (err %v), want [uuid-a]", sharers, err)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Errorf("shared worktree removed: %v", err)
	}
	if branchTip(t, p.base, "shared") == "" {
		t.Error("branch shared deleted, want kept")
	}
}

// An agent with no registry entry: the branch named after it is not
// deleted by name either.
func TestDeleteStartFailureFiles_NoRegistryEntryKeepsNamedBranch(t *testing.T) {
	p := newSharerKeyProject(t)
	p.add(t, "other", "uuid-other", "other") // clones the shared base
	startFailureGit(t, p.base, "branch", "dev-a")
	if err := os.MkdirAll(filepath.Join(p.scionDir, "agents", "dev-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := DeleteStartFailureFiles("dev-a", "uuid-a", p.projectPath, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	if branchTip(t, p.base, "dev-a") == "" {
		t.Error("branch dev-a deleted by name, want kept")
	}
	if _, err := os.Stat(filepath.Join(p.scionDir, "agents", "dev-a")); !os.IsNotExist(err) {
		t.Errorf("agent directory still present: %v", err)
	}
}

// The start-failure scope does not reach a normal delete: a last sharer's
// delete with removeBranch still removes the worktree and the branch it is
// on, whether the agent created the worktree on an existing branch or
// joined a worktree no registry entry named.
func TestDeleteHubAgentFiles_NormalDeleteUnchangedByStartFailureScope(t *testing.T) {
	t.Run("reused branch", func(t *testing.T) {
		p := newSharerKeyProject(t)
		wt, _ := reusedBranch(t, p)
		branchDeleted, err := DeleteHubAgentFiles("fix-x", "uuid-new", p.projectPath, "", true)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if !branchDeleted || branchTip(t, p.base, "fix-x") != "" {
			t.Errorf("branch fix-x kept (branchDeleted %v), want deleted", branchDeleted)
		}
		p.assertTornDown(t, "fix-x", "uuid-new", wt, "fix-x")
	})
	t.Run("joined worktree", func(t *testing.T) {
		p := newSharerKeyProject(t)
		leftover, _, _ := leftoverWorktree(t, p)
		branchDeleted, err := DeleteHubAgentFiles("fix-x", "uuid-new", p.projectPath, "", true)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if !branchDeleted || branchTip(t, p.base, "fix-x") != "" {
			t.Errorf("branch fix-x kept (branchDeleted %v), want deleted", branchDeleted)
		}
		if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
			t.Errorf("joined worktree still present: %v", err)
		}
		if _, _, found, err := provision.FindBranchForAgent(p.base, "", "uuid-new"); err != nil || found {
			t.Errorf("uuid-new still registered (found %v, err %v)", found, err)
		}
	})
}

// linkedCheckout is a project in a git checkout with no worktree-per-agent
// shared base (ProvisionAgent's layout). It returns the checkout, its
// .scion directory and the repository root as delete resolves it.
func linkedCheckout(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, projectDir)
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents", "dev-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := util.RepoRootDir(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	return projectDir, scionDir, root
}

// An agent registered by name with its worktree at
// <project>/agents/<name>/workspace (ProvisionAgent's create path, a linked
// checkout): the start-failure cleanup removes that worktree, its own, and
// keeps the branch.
func TestDeleteStartFailureFiles_ProvisionAgentLayout(t *testing.T) {
	_, scionDir, root := linkedCheckout(t)
	wt := filepath.Join(scionDir, "agents", "dev-a", "workspace")
	if err := util.CreateWorktree(wt, "dev-a"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if err := provision.RegisterSharer(root, scionDir, "dev-a", wt, "dev-a"); err != nil {
		t.Fatalf("RegisterSharer: %v", err)
	}
	if err := DeleteStartFailureFiles("dev-a", "uuid-a", scionDir, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Errorf("the agent's own worktree is still present: %v", err)
	}
	if _, _, found, err := provision.FindBranchForAgent(root, scionDir, "dev-a"); err != nil || found {
		t.Errorf("dev-a still registered (found %v, err %v)", found, err)
	}
	if branchTip(t, root, "dev-a") == "" {
		t.Error("branch dev-a deleted, want kept")
	}
}

// In a linked checkout an agent registered by name may have joined a
// worktree the user made under the checkout's worktrees directory. A
// linked checkout has no shared base, so that is not the agent's own path:
// the start-failure cleanup keeps the worktree, its uncommitted work, git's
// entry for it and the branch, and only unregisters the agent.
func TestDeleteStartFailureFiles_LinkedCheckoutJoinedWorktreeKept(t *testing.T) {
	projectDir, scionDir, root := linkedCheckout(t)
	joined := filepath.Join(projectDir, "worktrees", "dev-a")
	startFailureGit(t, projectDir, "worktree", "add", "-q", "-b", "dev-a", joined)
	tip := branchTip(t, root, "dev-a")
	dirty := filepath.Join(joined, "uncommitted.txt")
	if err := os.WriteFile(dirty, []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := provision.RegisterSharer(root, scionDir, "dev-a", joined, "dev-a"); err != nil {
		t.Fatalf("RegisterSharer: %v", err)
	}
	if err := DeleteStartFailureFiles("dev-a", "uuid-a", scionDir, ""); err != nil {
		t.Fatalf("DeleteStartFailureFiles: %v", err)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Errorf("the joined worktree's uncommitted work is gone: %v", err)
	}
	if list := listWorktrees(t, root); !strings.Contains(list, joined) {
		t.Errorf("git no longer lists the joined worktree %s:\n%s", joined, list)
	}
	if got := branchTip(t, root, "dev-a"); tip == "" || got != tip {
		t.Errorf("branch dev-a = %q, want kept at %q", got, tip)
	}
	if _, _, found, err := provision.FindBranchForAgent(root, scionDir, "dev-a"); err != nil || found {
		t.Errorf("dev-a still registered (found %v, err %v)", found, err)
	}
}

func TestStartFailureOwnWorktreePaths(t *testing.T) {
	shared := agentProjectLayout{sharedBase: "/p/workspace", sharedBaseIsRepo: true, repoRoot: "/p/workspace"}
	linked := agentProjectLayout{sharedBase: "/c/workspace", repoRoot: "/c"}
	for _, tc := range []struct {
		name           string
		layout         agentProjectLayout
		projectDir     string
		key, agentName string
		want           []string
	}{
		{"shared base, hub ID key", shared, "/p/.scion", "uuid-a", "dev-a", []string{"/p/workspace/worktrees/uuid-a"}},
		{"shared base, name key", shared, "/p/.scion", "dev-a", "dev-a", []string{"/p/workspace/worktrees/dev-a", "/p/.scion/agents/dev-a/workspace"}},
		{"linked checkout, name key", linked, "/c/.scion", "dev-a", "dev-a", []string{"/c/.scion/agents/dev-a/workspace"}},
		{"linked checkout, hub ID key", linked, "/c/.scion", "uuid-a", "dev-a", []string{}},
		{"no project", agentProjectLayout{}, "", "dev-a", "dev-a", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := startFailureOwnWorktreePaths(tc.layout, tc.projectDir, tc.key, tc.agentName)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("startFailureOwnWorktreePaths = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// isOwnWorktreePath fails closed: a path that does not resolve is never
// reported as the key's own, even when it is written the same.
func TestIsOwnWorktreePath(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "worktrees", "uuid-a")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "worktrees", "leftover")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "worktrees", "gone")
	for _, tc := range []struct {
		name   string
		wtPath string
		own    []string
		want   bool
	}{
		{"own path", own, []string{own}, true},
		{"own path, unclean", own + "/./", []string{own}, true},
		{"another path", other, []string{own}, false},
		{"symlink to own path", link, []string{own}, false},
		{"own path missing", missing, []string{missing}, false},
		{"empty own list", own, []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOwnWorktreePath(tc.wtPath, tc.own); got != tc.want {
				t.Errorf("isOwnWorktreePath(%q, %q) = %v, want %v", tc.wtPath, tc.own, got, tc.want)
			}
		})
	}
}
