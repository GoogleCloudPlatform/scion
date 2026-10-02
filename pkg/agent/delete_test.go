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
	"context"
	"encoding/json"
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

// corruptSharerMarkerWorktreePath writes a sharer marker's WorktreePath field
// directly to the file, outside RegisterSharer, so the read-side validation
// can be exercised against values the normal write path would never record.
// branch must need no sanitization (see sanitizeBranchName in
// pkg/provision/provision.go) for the marker filename to match what the
// registry itself would use.
func corruptSharerMarkerWorktreePath(t *testing.T, base, branch string, sharers []string, worktreePath string) {
	t.Helper()
	type sharerMarker struct {
		Branch       string   `json:"branch"`
		WorktreePath string   `json:"worktreePath"`
		Sharers      []string `json:"sharers"`
	}
	m := sharerMarker{Branch: branch, WorktreePath: worktreePath, Sharers: sharers}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(base, ".git", "scion-sharers", branch+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0644); err != nil {
		t.Fatal(err)
	}
}

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
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("HOME", tmpDir)

	// Create a git repo to act as the project root
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	setupGitRepo(t, projectDir)

	// Create .scion directory structure
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0755); err != nil {
		t.Fatalf("mkdir scion agents: %v", err)
	}

	agentName := "stale-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}

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
	_ = os.RemoveAll(agentWorkspace)

	// Re-create the agent directory without .git (simulates a failed re-start
	// that created the dir structure but couldn't add the worktree)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}

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
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("HOME", tmpDir)

	// Set up a project with .scion + project-id (split-storage marker).
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	setupGitRepo(t, projectDir)

	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("mkdir scion: %v", err)
	}
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
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	setupGitRepo(t, projectDir)

	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0755); err != nil {
		t.Fatalf("mkdir scion agents: %v", err)
	}

	agentName := "normal-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}

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
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}

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
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}

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
	sharers, _, err := provision.ListSharers(base, "", "shared-branch")
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
	sharers, _, err = provision.ListSharers(base, "", "shared-branch")
	if err != nil {
		t.Fatalf("ListSharers after delete: %v", err)
	}
	if len(sharers) != 1 || sharers[0] != "agent-b" {
		t.Errorf("expected sharers=[agent-b], got %v", sharers)
	}

	// 4. agent-a is no longer registered.
	_, _, found, _ := provision.FindBranchForAgent(base, "", "agent-a")
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
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}

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
	sharers, _, err := provision.ListSharers(base, "", "shared-branch")
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
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}

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
	sharers, _, err := provision.ListSharers(base, "", "solo-agent")
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

// TestDeleteAgentFiles_OutOfTreeMarker_SoleSharer_LeaksRatherThanDeletes
// covers acceptance criterion 3 for the sole-sharer case: if the recorded
// WorktreePath points outside every scion-created shape, DeleteAgentFiles
// must never touch the out-of-tree path. The read
// boundary (pkg/provision.readMarker) degrades the marker — keeps the
// Sharers refcount, blanks only WorktreePath — so the sole sharer
// is still found and still unregistered; the teardown caller guard then
// skips removal on the blanked path. The real worktree (still at its
// original, valid location) is therefore LEAKED, not removed: a deliberate,
// accepted trade-off (see readMarker's doc comment) — never act on an
// untrusted path, even at the cost of a cleanup that would otherwise have
// happened safely via other means.
func TestDeleteAgentFiles_OutOfTreeMarker_SoleSharer_LeaksRatherThanDeletes(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}

	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{HostPath: base, Backend: "local"}

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

	// Write a marker whose WorktreePath points at an out-of-tree location —
	// an external directory with real content that must never be touched.
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep-me.txt")
	if err := os.WriteFile(marker, []byte("do not touch"), 0644); err != nil {
		t.Fatal(err)
	}
	corruptSharerMarkerWorktreePath(t, base, "solo-agent", []string{"solo-agent"}, outside)

	if _, err := DeleteAgentFiles("solo-agent", projectPath, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}

	// The external path must survive no matter what else happens.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("external content must survive an out-of-tree marker delete: %v", err)
	}

	// The real worktree is leaked, not removed: the marker degrades (refcount
	// kept, path blanked) rather than being discarded, so the refcount path
	// handles teardown and correctly refuses to act on the blanked path. This
	// is the accepted trade-off — a leak, never a wrongful delete.
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("real in-tree worktree should be leaked (left in place), not removed: stat err=%v", err)
	}
}

// TestDeleteAgentFiles_OutOfTreeMarker_JoinedAgent_FailsClosed covers Phase 2
// acceptance criterion 3 for a joined (non-creator) sharer: when that agent
// is the last sharer and the recorded WorktreePath is out-of-tree, there is
// no independent trusted path to fall back to (a joiner never had its own worktree
// directory), so DeleteAgentFiles must do nothing rather than guess — the
// external path is untouched AND the real shared worktree the joiner was
// sharing is left alone too.
func TestDeleteAgentFiles_OutOfTreeMarker_JoinedAgent_FailsClosed(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}

	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{HostPath: base, Backend: "local"}

	// Agent A creates, Agent B joins the same branch.
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

	// Delete agent-a (creator) first — agent-b remains as sole sharer,
	// sharing agent-a's original worktree directory.
	if _, err := DeleteAgentFiles("agent-a", projectPath, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-a): %v", err)
	}
	if _, err := os.Stat(wtA); err != nil {
		t.Fatalf("setup: shared worktree should persist while agent-b remains: %v", err)
	}

	// Write a marker whose WorktreePath points at an out-of-tree location for
	// agent-b (now sole sharer) — an external directory with real content.
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep-me.txt")
	if err := os.WriteFile(marker, []byte("do not touch"), 0644); err != nil {
		t.Fatal(err)
	}
	corruptSharerMarkerWorktreePath(t, base, "shared-branch", []string{"agent-b"}, outside)

	if _, err := DeleteAgentFiles("agent-b", projectPath, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-b): %v", err)
	}

	// The external path must survive.
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("external content must survive an out-of-tree marker delete: %v", err)
	}

	// agent-b never had its own worktree directory (it joined agent-a's), so
	// there is no independent trusted path to fall back to — the shared
	// worktree the joiner was using must be left alone too (leaked, not
	// deleted; a lost refcount is the acceptable cost of failing closed).
	if _, err := os.Stat(wtA); err != nil {
		t.Errorf("shared worktree must survive when the last-sharer marker is out-of-tree: %v", err)
	}
}

// TestDeleteAgentFiles_ProvisionAgentLayout_FirstDeleteKeepsWorktree_LastDeleteRemoves
// is a required regression test: two agents attached to
// the same branch via the local ProvisionAgent path (pkg/agent/provision.go's
// worktree-mode create + attach, NOT ProvisionShared) — worktree at
// <projectDir>/agents/<name>/workspace, outside repoRoot/worktrees/. Before
// this fix, readMarker validated WorktreePath only against
// WorktreeIsLexicallyUnderBase(repoRoot, ...), which this shape never
// matches — every legitimate marker here was discarded, the refcount was
// lost, and deleting the FIRST agent removed a worktree the second was still
// using (data loss). Both clauses matter: the worktree must survive the
// first (non-last) delete AND actually be removed on the last one — a fix
// that only avoids over-deletion by leaking forever (never removing) would
// pass the first clause and fail the second.
func TestDeleteAgentFiles_ProvisionAgentLayout_FirstDeleteKeepsWorktree_LastDeleteRemoves(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	runProvisionAgentLayoutFirstLastDelete(t, scionDir)
}

// TestDeleteAgentFiles_ProvisionAgentLayout_SymlinkedProjectAncestor is the
// REQUIRED symlinked-ancestor regression: the same first-delete/last-delete
// scenario, but the project directory itself is reached only through a
// symlink. The write side (ProvisionAgent, via config.GetResolvedProjectDir/
// util.RepoRootDir) and the read side (DeleteAgentFiles's resolvedProjectDir,
// threaded into WorktreePathIsScionCreated) must derive the SAME lexical
// projectDir form from the same symlinked path for the ProvisionAgent-layout
// shape check to accept a legitimate marker — proving the classification
// check really is symlink-form-agnostic (no EvalSymlinks anywhere in it), not
// simply untested for this case.
func TestDeleteAgentFiles_ProvisionAgentLayout_SymlinkedProjectAncestor(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	projectDir := filepath.Dir(scionDir)
	tmpDir := filepath.Dir(projectDir)

	realProjectDir := filepath.Join(tmpDir, "real-project")
	if err := os.Rename(projectDir, realProjectDir); err != nil {
		t.Fatal(err)
	}
	symlinkedProjectDir := projectDir // reuse the original path as the symlink
	if err := os.Symlink(realProjectDir, symlinkedProjectDir); err != nil {
		t.Fatal(err)
	}

	runProvisionAgentLayoutFirstLastDelete(t, filepath.Join(symlinkedProjectDir, config.DotScion))
}

// TestDeleteAgentFiles_ProvisionAgentLayout_ScionInSubdirectory locks the
// non-top-level layout the earlier <base>/.scion reconstruction attempt
// would have false-rejected: .scion lives in a subdirectory of a LARGER
// enclosing git repository, so repoRoot (util.RepoRootDir(projectDir), the
// outer repo's top level) is NOT projectDir's parent. The classification
// check must use the real, resolved projectDir passed through explicitly —
// not derive it from repoRoot — or a legitimate marker here false-rejects
// and the worktree leaks at the last sharer instead of being removed.
func TestDeleteAgentFiles_ProvisionAgentLayout_ScionInSubdirectory(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	outerRepoDir := filepath.Dir(scionDir) // reprovisionSetup already git-init'd this
	nestedProjectDir := filepath.Join(outerRepoDir, "nested", "project")
	if err := os.MkdirAll(nestedProjectDir, 0755); err != nil {
		t.Fatal(err)
	}
	nestedScionDir := filepath.Join(nestedProjectDir, config.DotScion)
	if err := os.MkdirAll(filepath.Join(nestedScionDir, "templates"), 0755); err != nil {
		t.Fatal(err)
	}
	// reprovisionSetup's .gitignore (".scion/agents/") is rooted at the outer
	// repo's top level (git treats a pattern containing a "/" other than a
	// trailing one as anchored to the .gitignore's own directory), so it does
	// not cover the nested project's agents dir. Add one scoped to here,
	// matching how a real nested project would be set up.
	if err := os.WriteFile(filepath.Join(nestedProjectDir, ".gitignore"), []byte(".scion/agents/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Sanity: the outer repo root is NOT the nested project directory —
	// confirming this test actually exercises the non-top-level layout.
	if outer, err := util.RepoRootDir(nestedProjectDir); err != nil {
		t.Fatalf("RepoRootDir: %v", err)
	} else if outer == nestedProjectDir {
		t.Fatalf("setup: expected the outer repo root %q to differ from the nested project dir", outer)
	}

	runProvisionAgentLayoutFirstLastDelete(t, nestedScionDir)
}

// runProvisionAgentLayoutFirstLastDelete implements a required regression
// test: two agents attached to the same branch
// via the local ProvisionAgent path (pkg/agent/provision.go's worktree-mode
// create + attach, NOT ProvisionShared) — worktree at
// <projectDir>/agents/<name>/workspace, outside repoRoot/worktrees/. Before
// this fix, readMarker validated WorktreePath only against
// WorktreeIsLexicallyUnderBase(repoRoot, ...), which this shape never
// matches — every legitimate marker here was discarded, the refcount was
// lost, and deleting the FIRST agent removed a worktree the second was still
// using (data loss). Both clauses matter: the worktree must survive the
// first (non-last) delete AND actually be removed on the last one — a fix
// that only avoids over-deletion by leaking forever (never removing) would
// pass the first clause and fail the second.
func runProvisionAgentLayoutFirstLastDelete(t *testing.T, scionDir string) {
	t.Helper()
	projectDir := filepath.Dir(scionDir)
	// reprovisionSetup leaves CWD outside the repo (broker-like), which is
	// deliberate for the CWD-dependent bug it exists to catch elsewhere. This
	// test instead needs util.BranchExists/FindWorktreeByBranch (which run
	// git from the process CWD) to actually see the branch/worktree agent-b
	// attaches to — a local/CLI-like CWD, inside the repo.
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	// Production code (both ProvisionAgent and DeleteAgentFiles) resolves the
	// caller-supplied scionDir through config.GetResolvedProjectDir, which
	// evaluates symlinks — so the registry's recorded WorktreePath is always
	// in resolved form, regardless of whether the caller's own path was
	// symlinked. Do the same resolution here before building the expected
	// path strings and making direct registry calls, so this test's
	// expectations match what production actually records rather than
	// asserting against a form nothing ever writes.
	resolvedScionDir, err := config.GetResolvedProjectDir(scionDir)
	if err != nil {
		t.Fatalf("GetResolvedProjectDir: %v", err)
	}

	branch := "shared-branch"
	if _, _, _, err := ProvisionAgent(context.Background(), "agent-a", "default", "", "", scionDir, "", "created", branch, ""); err != nil {
		t.Fatalf("provision agent-a: %v", err)
	}
	wtA := filepath.Join(resolvedScionDir, "agents", "agent-a", "workspace")
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Fatalf("setup: expected a worktree at %s: %v", wtA, err)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), "agent-b", "default", "", "", scionDir, "", "created", branch, ""); err != nil {
		t.Fatalf("provision agent-b (attach): %v", err)
	}
	// agent-b must attach to agent-a's worktree, not create its own.
	wtB := filepath.Join(resolvedScionDir, "agents", "agent-b", "workspace")
	if _, err := os.Stat(filepath.Join(wtB, ".git")); err == nil {
		t.Fatalf("agent-b should have attached to agent-a's worktree, not created its own at %s", wtB)
	}

	root, err := util.RepoRootDir(projectDir)
	if err != nil {
		t.Fatalf("RepoRootDir: %v", err)
	}
	sharers, wtPath, err := provision.ListSharers(root, resolvedScionDir, branch)
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if len(sharers) != 2 {
		t.Fatalf("setup: expected 2 sharers, got %v", sharers)
	}
	if wtPath != wtA {
		t.Fatalf("setup: worktreePath = %q, want %q (this must be recognized as scion-created)", wtPath, wtA)
	}

	// Delete agent-a FIRST (not the last sharer). The worktree must remain,
	// and agent-b's refcount registration must survive.
	if _, err := DeleteAgentFiles("agent-a", scionDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-a): %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtA, ".git")); err != nil {
		t.Fatalf("worktree must persist after deleting the first (non-last) sharer: %v", err)
	}
	sharers, wtPath, err = provision.ListSharers(root, resolvedScionDir, branch)
	if err != nil {
		t.Fatalf("ListSharers after first delete: %v", err)
	}
	if len(sharers) != 1 || sharers[0] != "agent-b" {
		t.Fatalf("expected sharers=[agent-b] after deleting agent-a, got %v", sharers)
	}
	if wtPath != wtA {
		t.Fatalf("worktreePath should still be %q after deleting the non-last sharer, got %q", wtA, wtPath)
	}

	// Delete agent-b LAST. The worktree must actually be removed now — this
	// is the clause a leak-only fix would fail.
	if _, err := DeleteAgentFiles("agent-b", scionDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-b): %v", err)
	}
	if _, err := os.Stat(wtA); !os.IsNotExist(err) {
		t.Errorf("worktree must be removed after the LAST sharer's teardown, stat err=%v", err)
	}
}

// TestDeleteAgentFiles_ProvisionAgentAttachToUserWorktree_NeverRemoved covers
// the second required regression case: ProvisionAgent's attach-to-an-
// existing-worktree path (pkg/agent/provision.go, via util.FindWorktreeByBranch)
// can register a sharer for a worktree at a location scion did not create —
// most commonly a worktree a user set up by hand before running scion against
// that branch. Such a WorktreePath matches neither scion-created shape, so it
// is refcounted (the agent is a real, tracked sharer) but must NEVER be
// removed by scion teardown, at any point — scion didn't create it and has no
// business deleting it, independent of any modification question.
func TestDeleteAgentFiles_ProvisionAgentAttachToUserWorktree_NeverRemoved(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	projectDir := filepath.Dir(scionDir)
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	branch := "user-created-branch"
	// A worktree a user created by hand, entirely outside any scion-managed
	// directory: not under <projectDir>/agents/ (ProvisionAgent's own shape)
	// and not under repoRoot/worktrees/ (ProvisionShared's shape).
	userWorktree := filepath.Join(filepath.Dir(projectDir), "user-created-worktree")
	cmd := exec.Command("git", "worktree", "add", "--relative-paths", "-b", branch, userWorktree)
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add (simulating a user-created worktree): %v\n%s", err, out)
	}

	if _, _, _, err := ProvisionAgent(context.Background(), "agent-x", "default", "", "", scionDir, "", "created", branch, ""); err != nil {
		t.Fatalf("provision agent-x (attach to user-created worktree): %v", err)
	}

	root, err := util.RepoRootDir(projectDir)
	if err != nil {
		t.Fatalf("RepoRootDir: %v", err)
	}
	sharers, wtPath, err := provision.ListSharers(root, scionDir, branch)
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if len(sharers) != 1 || sharers[0] != "agent-x" {
		t.Fatalf("expected sharers=[agent-x], got %v", sharers)
	}
	if wtPath != "" {
		t.Fatalf("worktreePath should be blanked for a non-scion-created worktree, got %q", wtPath)
	}

	// agent-x is the sole/last sharer; teardown must never remove the
	// user-created worktree, even with removeBranch=true.
	if _, err := DeleteAgentFiles("agent-x", scionDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-x): %v", err)
	}
	if _, err := os.Stat(filepath.Join(userWorktree, ".git")); err != nil {
		t.Errorf("user-created worktree must survive teardown of its last scion-tracked sharer: %v", err)
	}
}

func intPtr(i int) *int { return &i }

// TestDeleteAgentFiles_GlobalProject_DeletesOnlyTargetWorkspace is the
// global-project counterpart to
// TestDeleteAgentFiles_WorktreePerAgent_DeletesOnlyTargetWorktree: each
// global-project agent gets its own workspace subdirectory under
// ~/.scion/workspace/<agentName> (ProvisionAgent's Case 3), which
// DeleteAgentFiles must remove on that agent's own deletion without
// touching a sibling global agent's own subdirectory.
func TestDeleteAgentFiles_GlobalProject_DeletesOnlyTargetWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	mockRuntimeForTest(t)

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()
	_ = os.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	if err := config.InitGlobal(getTestHarnesses()); err != nil {
		t.Fatalf("InitGlobal: %v", err)
	}
	globalScionDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatalf("GetGlobalDir: %v", err)
	}

	for _, name := range []string{"global-agent-a", "global-agent-b"} {
		if _, _, _, err := ProvisionAgent(context.Background(), name, "default", "", "", globalScionDir, "", "", "", ""); err != nil {
			t.Fatalf("ProvisionAgent(%s): %v", name, err)
		}
	}

	workspaceRoot := filepath.Join(globalScionDir, "workspace")
	wsA := filepath.Join(workspaceRoot, "global-agent-a")
	wsB := filepath.Join(workspaceRoot, "global-agent-b")

	for _, p := range []string{wsA, wsB} {
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			t.Fatalf("setup: expected %s to exist as a directory: %v", p, err)
		}
	}

	if _, err := DeleteAgentFiles("global-agent-b", globalScionDir, false); err != nil {
		t.Fatalf("DeleteAgentFiles(global-agent-b): %v", err)
	}

	if _, err := os.Stat(wsB); !os.IsNotExist(err) {
		t.Errorf("global-agent-b's own workspace directory should be removed, stat err=%v", err)
	}
	if info, err := os.Stat(wsA); err != nil || !info.IsDir() {
		t.Errorf("sibling global-agent-a's workspace directory should survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(globalScionDir, "agents", "global-agent-b")); !os.IsNotExist(err) {
		t.Errorf("global-agent-b config dir should be removed, stat err=%v", err)
	}
}
