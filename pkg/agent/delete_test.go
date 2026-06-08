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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func setupGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

func listWorktrees(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repoDir, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git worktree list failed: %v", err)
	}
	return string(out)
}

func TestDeleteAgentFiles_CleansStaleWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "") // Clear container context for worktree ops
	tmpDir := t.TempDir()

	// Set CWD and HOME to tmpDir so config resolution works
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create a git repo to act as the project root
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	setupGitRepo(t, projectDir)

	// Create .scion directory structure
	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(filepath.Join(scionDir, "agents"), 0755)

	agentName := "stale-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	os.MkdirAll(agentDir, 0755)

	// Create a worktree at the workspace path (simulates a successful start)
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Verify worktree is registered
	wtList := listWorktrees(t, projectDir)
	if !strings.Contains(wtList, agentName) {
		t.Fatalf("expected worktree %q in list, got:\n%s", agentName, wtList)
	}

	// Manually remove the workspace directory (simulating incomplete cleanup).
	// This leaves the worktree registered but the directory gone ("prunable").
	os.RemoveAll(agentWorkspace)

	// Re-create the agent directory without .git (simulates a failed re-start
	// that created the dir structure but couldn't add the worktree)
	os.MkdirAll(agentDir, 0755)

	// Call DeleteAgentFiles — it should clean up the stale worktree record
	branchDeleted, err := DeleteAgentFiles(agentName, scionDir, true)
	if err != nil {
		t.Fatalf("DeleteAgentFiles failed: %v", err)
	}

	// Verify the stale worktree record was pruned
	wtList = listWorktrees(t, projectDir)
	if strings.Contains(wtList, "stale-agent") {
		t.Errorf("expected stale worktree to be pruned, but still found in:\n%s", wtList)
	}

	// Verify the branch was deleted
	if !branchDeleted {
		t.Error("expected branch to be deleted")
	}
	if util.BranchExists(agentName) {
		t.Error("expected branch to be gone after DeleteAgentFiles")
	}

	// Verify agent directory was removed
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("expected agent directory to be removed")
	}
}

// TestDeleteAgentFiles_CleansSharedWorkspaceExternalState verifies that for
// shared-workspace git projects (whose per-agent state lives outside the project
// tree per .design/hub-shared-workspace-isolation.md), DeleteAgentFiles
// removes the external <project-configs>/<slug>__<uuid>/.scion/agents/<name>
// directory in addition to any in-project residue.
func TestDeleteAgentFiles_CleansSharedWorkspaceExternalState(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Set up a project with .scion + project-id (split-storage marker).
	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	setupGitRepo(t, projectDir)

	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(scionDir, 0755)
	if err := config.WriteProjectID(scionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID failed: %v", err)
	}

	agentName := "shared-agent"

	// Resolve the external dir the same way production code does.
	extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir)
	if err != nil || extAgentsDir == "" {
		t.Fatalf("GetGitProjectExternalAgentsDir: dir=%q err=%v", extAgentsDir, err)
	}
	extAgentDir := filepath.Join(extAgentsDir, agentName)
	if err := os.MkdirAll(extAgentDir, 0755); err != nil {
		t.Fatalf("mkdir extAgentDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extAgentDir, "prompt.md"), []byte("task"), 0644); err != nil {
		t.Fatalf("write prompt.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extAgentDir, "scion-agent.json"), []byte(`{}`), 0644); err != nil {
		t.Fatalf("write scion-agent.json: %v", err)
	}
	// Also seed an external home/ subdir to mirror real layout.
	if err := os.MkdirAll(filepath.Join(extAgentDir, "home"), 0755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}

	// DeleteAgentFiles takes projectPath; pass scionDir like real callers do.
	if _, err := DeleteAgentFiles(agentName, scionDir, false); err != nil {
		t.Fatalf("DeleteAgentFiles failed: %v", err)
	}

	if _, err := os.Stat(extAgentDir); !os.IsNotExist(err) {
		t.Errorf("expected external agent dir %s to be removed, stat err=%v", extAgentDir, err)
	}
}

func TestDeleteAgentFiles_CleansWorktreeWithGitFile(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "") // Clear container context for worktree ops
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(projectDir, 0755)
	setupGitRepo(t, projectDir)

	scionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(filepath.Join(scionDir, "agents"), 0755)

	agentName := "normal-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	os.MkdirAll(agentDir, 0755)

	// Create a proper worktree (has .git file)
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Verify .git file exists
	if _, err := os.Stat(filepath.Join(agentWorkspace, ".git")); os.IsNotExist(err) {
		t.Fatal("expected .git to exist in workspace")
	}

	// DeleteAgentFiles should properly clean up via RemoveWorktree
	branchDeleted, err := DeleteAgentFiles(agentName, scionDir, true)
	if err != nil {
		t.Fatalf("DeleteAgentFiles failed: %v", err)
	}

	if !branchDeleted {
		t.Error("expected branch to be deleted")
	}

	// Verify worktree is gone
	wtList := listWorktrees(t, projectDir)
	if strings.Contains(wtList, agentName) {
		t.Errorf("expected worktree to be removed, but still found in:\n%s", wtList)
	}

	// Verify agent directory was removed
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("expected agent directory to be removed")
	}
}

// initBareRepo creates a bare git repo seeded with one commit, for use as a
// clone URL in worktree-per-agent provisioning tests.
func initBareRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	wc := filepath.Join(dir, "wc")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, strings.TrimSpace(string(out)))
		}
	}
	run("init", "--bare", "-b", "main", bare)
	run("clone", bare, wc)
	if err := os.WriteFile(filepath.Join(wc, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", wc, "add", "-A")
	run("-C", wc, "commit", "-m", "init")
	run("-C", wc, "push", "origin", "main")
	return bare
}

// TestDeleteAgentFiles_WorktreePerAgent_DeletesOnlyTargetWorktree is the
// regression test for Phase 2 T2: verifies that deleting one agent in a
// worktree-per-agent layout removes only that agent's worktree directory
// and .git/worktrees registration, while leaving the shared base and
// sibling worktrees intact.
func TestDeleteAgentFiles_WorktreePerAgent_DeletesOnlyTargetWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: 0}

	// Set up a hub-managed project layout: projectPath with .scion inside.
	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The workspace backend computes HostPath = projectPath + "/workspace".
	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{
		HostPath: base,
		Backend:  "local",
	}

	// Provision agent-a.
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  resolved,
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a",
		GitClone: gc,
	}); err != nil {
		t.Fatalf("provision agent-a: %v", err)
	}

	// Provision agent-b.
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved:  resolved,
		Mode:      store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-b", AgentName: "agent-b",
		GitClone: gc,
	}); err != nil {
		t.Fatalf("provision agent-b: %v", err)
	}

	wtA := provision.WorktreePath(base, "agent-a")
	wtB := provision.WorktreePath(base, "agent-b")

	// Sanity: both worktrees + base exist.
	for _, p := range []string{
		filepath.Join(base, ".git"),
		wtA, wtB,
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("setup: expected %s to exist: %v", p, err)
		}
	}

	// Create agent config dirs (as the broker would).
	for _, name := range []string{"agent-a", "agent-b"} {
		agentDir := filepath.Join(scionDir, "agents", name)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Delete agent-b via DeleteAgentFiles (pass projectPath, not scionDir,
	// to match the hub-managed broker flow).
	branchDeleted, err := DeleteAgentFiles("agent-b", projectPath, true)
	if err != nil {
		t.Fatalf("DeleteAgentFiles(agent-b): %v", err)
	}

	// --- Assertions ---

	// 1. agent-b's worktree directory is gone.
	if _, err := os.Stat(wtB); !os.IsNotExist(err) {
		t.Errorf("agent-b worktree dir should be removed, stat err=%v", err)
	}

	// 2. agent-b's .git/worktrees registration is pruned.
	wtListStr := listWorktrees(t, base)
	if strings.Contains(wtListStr, "agent-b") {
		t.Errorf("agent-b should be pruned from worktree list:\n%s", wtListStr)
	}

	// 3. agent-b's branch is deleted.
	if !branchDeleted {
		t.Error("expected agent-b branch to be deleted")
	}
	branchCheck := exec.Command("git", "-C", base, "branch", "--list", "agent-b")
	if out, _ := branchCheck.Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("agent-b branch should be gone, got: %s", strings.TrimSpace(string(out)))
	}

	// 4. Shared base .git survives.
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Errorf("shared base .git should survive: %v", err)
	}

	// 5. Sibling agent-a worktree survives.
	if _, err := os.Stat(wtA); err != nil {
		t.Errorf("sibling agent-a worktree should survive: %v", err)
	}

	// 6. Sibling agent-a is still registered.
	if !strings.Contains(wtListStr, "agent-a") {
		t.Errorf("agent-a should still be in worktree list:\n%s", wtListStr)
	}

	// 7. agent-b config dir is removed.
	if _, err := os.Stat(filepath.Join(scionDir, "agents", "agent-b")); !os.IsNotExist(err) {
		t.Errorf("agent-b config dir should be removed, stat err=%v", err)
	}

	// 8. agent-a config dir survives.
	if _, err := os.Stat(filepath.Join(scionDir, "agents", "agent-a")); err != nil {
		t.Errorf("agent-a config dir should survive: %v", err)
	}
}

// TestDeleteAgentFiles_SharedWorktree_DeleteCreatorWhileJoinerRemains verifies
// that deleting the creator agent of a shared worktree does NOT remove the
// shared worktree or branch when another sharer (joiner) remains.
func TestDeleteAgentFiles_SharedWorktree_DeleteCreatorWhileJoinerRemains(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: 0}

	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{HostPath: base, Backend: "local"}

	// Agent A creates worktree on branch "shared-branch".
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "shared-branch",
		GitClone: gc,
	}); err != nil {
		t.Fatalf("provision agent-a: %v", err)
	}

	// Agent B joins the same branch "shared-branch".
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-b", AgentName: "shared-branch",
		GitClone: gc,
	}); err != nil {
		t.Fatalf("provision agent-b: %v", err)
	}

	// The shared worktree lives under agent-a's path (it was the creator).
	wtA := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(wtA); err != nil {
		t.Fatalf("setup: shared worktree should exist at %s: %v", wtA, err)
	}

	// Sanity: both are registered.
	sharers, _, err := provision.ListSharers(base, "shared-branch")
	if err != nil || len(sharers) != 2 {
		t.Fatalf("setup: expected 2 sharers, got %v (err=%v)", sharers, err)
	}

	// Create agent config dirs (as the broker would).
	for _, name := range []string{"agent-a", "agent-b"} {
		if err := os.MkdirAll(filepath.Join(scionDir, "agents", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Delete agent-a (the creator) while agent-b (joiner) remains.
	branchDeleted, err := DeleteAgentFiles("agent-a", projectPath, true)
	if err != nil {
		t.Fatalf("DeleteAgentFiles(agent-a): %v", err)
	}

	// 1. Shared worktree PERSISTS (dir + .git still present).
	if _, err := os.Stat(wtA); err != nil {
		t.Errorf("shared worktree should persist while joiner remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Errorf("shared worktree .git should persist: %v", err)
	}

	// 2. Branch NOT deleted.
	if branchDeleted {
		t.Error("branch should NOT be deleted while other sharers remain")
	}
	branchCheck := exec.Command("git", "-C", base, "branch", "--list", "shared-branch")
	if out, _ := branchCheck.Output(); strings.TrimSpace(string(out)) == "" {
		t.Error("branch 'shared-branch' should still exist in the repo")
	}

	// 3. agent-b is still registered as a sharer.
	sharers, _, err = provision.ListSharers(base, "shared-branch")
	if err != nil {
		t.Fatalf("ListSharers after delete: %v", err)
	}
	if len(sharers) != 1 || sharers[0] != "agent-b" {
		t.Errorf("expected sharers=[agent-b], got %v", sharers)
	}

	// 4. agent-a is no longer registered.
	_, _, found, _ := provision.FindBranchForAgent(base, "agent-a")
	if found {
		t.Error("agent-a should no longer be in the sharer registry")
	}

	// 5. agent-a's config dir is removed.
	if _, err := os.Stat(filepath.Join(scionDir, "agents", "agent-a")); !os.IsNotExist(err) {
		t.Errorf("agent-a config dir should be removed, stat err=%v", err)
	}
}

// TestDeleteAgentFiles_SharedWorktree_DeleteLastSharer_RemovesWorktree verifies
// that deleting the last remaining sharer removes the shared worktree and
// optionally the branch.
func TestDeleteAgentFiles_SharedWorktree_DeleteLastSharer_RemovesWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: 0}

	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{HostPath: base, Backend: "local"}

	// Agent A creates, Agent B joins.
	for _, id := range []string{"agent-a", "agent-b"} {
		if err := provision.ProvisionShared(provision.ProvisionInput{
			Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
			ProjectID: "p1", AgentID: id, AgentName: "shared-branch",
			GitClone: gc,
		}); err != nil {
			t.Fatalf("provision %s: %v", id, err)
		}
		if err := os.MkdirAll(filepath.Join(scionDir, "agents", id), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	wtA := provision.WorktreePath(base, "agent-a")

	// Delete agent-a first (not last → detach only).
	if _, err := DeleteAgentFiles("agent-a", projectPath, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-a): %v", err)
	}

	// Worktree should still exist.
	if _, err := os.Stat(wtA); err != nil {
		t.Fatalf("worktree should persist after deleting first sharer: %v", err)
	}

	// Now delete agent-b (last sharer) with removeBranch=true.
	branchDeleted, err := DeleteAgentFiles("agent-b", projectPath, true)
	if err != nil {
		t.Fatalf("DeleteAgentFiles(agent-b): %v", err)
	}

	// 1. Shared worktree is removed.
	if _, err := os.Stat(wtA); !os.IsNotExist(err) {
		t.Errorf("shared worktree should be removed after last sharer deleted, stat err=%v", err)
	}

	// 2. Branch is deleted.
	if !branchDeleted {
		t.Error("expected branch to be deleted when last sharer is removed with removeBranch=true")
	}
	branchCheck := exec.Command("git", "-C", base, "branch", "--list", "shared-branch")
	if out, _ := branchCheck.Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("branch 'shared-branch' should be gone, got: %s", strings.TrimSpace(string(out)))
	}

	// 3. Sharer registry is empty.
	sharers, _, err := provision.ListSharers(base, "shared-branch")
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if len(sharers) != 0 {
		t.Errorf("expected no sharers remaining, got %v", sharers)
	}

	// 4. agent-b's config dir is removed.
	if _, err := os.Stat(filepath.Join(scionDir, "agents", "agent-b")); !os.IsNotExist(err) {
		t.Errorf("agent-b config dir should be removed, stat err=%v", err)
	}
}

// TestDeleteAgentFiles_SharedWorktree_SoleSharer_DeleteRemoves verifies that a
// unique-branch agent (sole sharer in the registry) still has its worktree
// removed on delete — no regression from the refcount path.
func TestDeleteAgentFiles_SharedWorktree_SoleSharer_DeleteRemoves(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: 0}

	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{HostPath: base, Backend: "local"}

	// Provision a single agent with a unique branch name.
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "solo-agent", AgentName: "solo-agent",
		GitClone: gc,
	}); err != nil {
		t.Fatalf("provision solo-agent: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(scionDir, "agents", "solo-agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	wtPath := provision.WorktreePath(base, "solo-agent")
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("setup: worktree should exist at %s: %v", wtPath, err)
	}

	// Delete the sole sharer.
	branchDeleted, err := DeleteAgentFiles("solo-agent", projectPath, true)
	if err != nil {
		t.Fatalf("DeleteAgentFiles(solo-agent): %v", err)
	}

	// Worktree is removed.
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("sole agent's worktree should be removed, stat err=%v", err)
	}

	// Branch is deleted.
	if !branchDeleted {
		t.Error("expected branch to be deleted for sole sharer")
	}

	// No sharers remain.
	sharers, _, err := provision.ListSharers(base, "solo-agent")
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if len(sharers) != 0 {
		t.Errorf("expected no sharers remaining, got %v", sharers)
	}

	// Shared base .git survives.
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Errorf("shared base .git should survive: %v", err)
	}
}
