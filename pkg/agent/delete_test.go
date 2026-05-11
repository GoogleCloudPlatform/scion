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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
// shared-workspace git groves (whose per-agent state lives outside the grove
// tree per .design/hub-shared-workspace-isolation.md), DeleteAgentFiles
// removes the external <project-configs>/<slug>__<uuid>/.scion/agents/<name>
// directory in addition to any in-grove residue.
func TestDeleteAgentFiles_CleansSharedWorkspaceExternalState(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	os.Chdir(tmpDir)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Set up a project with .scion + grove-id (split-storage marker).
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
