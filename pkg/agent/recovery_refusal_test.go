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

// These tests run git against a temporary repository.
//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// recoveryFixture provisions an existing worktree agent and then removes its
// workspace, so the next GetAgent takes the worktree-recovery path. It returns
// the project dir, the .scion dir and the agent name.
func recoveryFixture(t *testing.T) (projectDir, scionDir, agentName string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("SCION_HOST_UID", "") // clear the container marker for worktree ops
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	projectDir = filepath.Join(tmpDir, "project")
	_ = os.MkdirAll(projectDir, 0o755)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	git("commit", "--allow-empty", "-m", "initial")

	scionDir = filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(filepath.Join(scionDir, "templates"), 0o755)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	_ = os.MkdirAll(filepath.Join(globalScionDir, "templates"), 0o755)
	seedTestHarnessConfig(t, globalScionDir, "generic", "generic")
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0o755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"generic"}`), 0o644)

	agentName = "ws-agent"
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	agentHome := config.GetAgentHomePath(scionDir, agentName)
	_ = os.MkdirAll(agentDir, 0o755)
	_ = os.MkdirAll(agentHome, 0o755)
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness":"generic","default_harness_config":"generic"}`), 0o644)
	_ = os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte(`{"name":"ws-agent","template":"default"}`), 0o644)

	// Remove the workspace and its worktree record so GetAgent must recreate it.
	_ = os.RemoveAll(agentWorkspace)
	git("worktree", "prune")
	return projectDir, scionDir, agentName
}

// A repository config refusal during worktree recovery refuses the start: it
// is not downgraded to a warning and an empty workspace.
func TestGetAgent_RecoveryRefusalFailsClosed(t *testing.T) {
	projectDir, scionDir, agentName := recoveryFixture(t)
	cmd := exec.Command("git", "config", "core.sshCommand", "sshpass -p recoverymarker-s1 ssh")
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plant: %v\n%s", err, out)
	}
	_, _, wsPath, _, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err == nil {
		t.Fatalf("GetAgent continued after a policy refusal (workspace %q)", wsPath)
	}
	var refused *util.RepoConfigRefusedError
	if !errors.As(err, &refused) || !strings.EqualFold(refused.Key, "core.sshcommand") {
		t.Fatalf("error = %v, want a refusal of core.sshCommand", err)
	}
	if strings.Contains(err.Error(), "recoverymarker-s1") {
		t.Errorf("error leaks the value: %s", err)
	}
}

// A recovery failure that is not a policy refusal keeps the existing
// behaviour: a warning, and the start continues without a workspace.
func TestGetAgent_RecoveryNonPolicyFailureContinues(t *testing.T) {
	projectDir, scionDir, agentName := recoveryFixture(t)
	// Another worktree holds the agent's branch, so recreating it fails.
	cmd := exec.Command("git", "worktree", "add", filepath.Join(t.TempDir(), "holder"), agentName)
	cmd.Dir = projectDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("holder worktree: %v\n%s", err, out)
	}
	_, _, wsPath, _, err := GetAgent(context.Background(), agentName, "", "", "", scionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("a non-policy recovery failure must not refuse the start: %v", err)
	}
	if wsPath != "" {
		t.Errorf("workspace = %q, want empty after a failed recovery", wsPath)
	}
}
