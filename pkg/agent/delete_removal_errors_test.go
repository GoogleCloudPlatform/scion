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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// errInjectedRemoval is the failure the removal stubs below return; it wraps
// errors.ErrUnsupported, as RemoveAllSafe's does where a symlink cannot be
// removed safely.
var errInjectedRemoval = fmt.Errorf("injected removal failure: %w", errors.ErrUnsupported)

// failRemoveAllSafeAt makes removeAllSafe fail with errInjectedRemoval for a
// path whose last two elements are parent/name (the guard may hand over a
// resolved path, so the full path is not compared); every other path is
// removed by util.RemoveAllSafe. It returns the paths the stub failed.
func failRemoveAllSafeAt(t *testing.T, parent, name string) *[]string {
	t.Helper()
	var failed []string
	setSeam(t, &removeAllSafe, func(path string) error {
		if filepath.Base(path) == name && filepath.Base(filepath.Dir(path)) == parent {
			failed = append(failed, path)
			return errInjectedRemoval
		}
		return util.RemoveAllSafe(path)
	})
	return &failed
}

// failRemoveWorktree makes removeWorktree fail with err without touching the
// filesystem.
func failRemoveWorktree(t *testing.T, err error) {
	t.Helper()
	setSeam(t, &removeWorktree, func(base, path, repoRoot, branch string) (bool, error) {
		return false, err
	})
}

// setupDeleteRemovalProject creates a git project with an agents directory
// in a fresh HOME and working directory, and returns the project root and
// its .scion directory.
func setupDeleteRemovalProject(t *testing.T) (string, string) {
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
	scionDir := filepath.Join(projectDir, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return projectDir, scionDir
}

// setupWorktreePerAgentProject creates a worktree-per-agent project layout
// (<project>/workspace is the shared base repository) with the agent's
// directory and a worktree directory at <project>/workspace/worktrees/<name>
// holding one file. It returns the project path and the worktree directory.
func setupWorktreePerAgentProject(t *testing.T, agentName string) (string, string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	projectPath := filepath.Join(tmpDir, "proj")
	if err := os.MkdirAll(filepath.Join(projectPath, config.DotScion, "agents", agentName), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(projectPath, "workspace")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, base)
	wt := filepath.Join(base, "worktrees", agentName)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectPath, wt
}

// assertRemovalFailureReturned checks that DeleteAgentFiles returned the
// injected failure, that the stub failed exactly once, and that path is
// still present.
func assertRemovalFailureReturned(t *testing.T, err error, failed *[]string, path string) {
	t.Helper()
	if !errors.Is(err, errInjectedRemoval) || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("DeleteAgentFiles error = %v; want the injected removal failure (errors.ErrUnsupported)", err)
	}
	if len(*failed) != 1 {
		t.Fatalf("injected removal failed %d times (%v); want once", len(*failed), *failed)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("%s should be left in place after the failed removal: %v", path, statErr)
	}
}

// TestDeleteAgentFiles_WorktreePerAgentFallbackRemovalErrorReturned covers
// the removal of a worktree-per-agent worktree after RemoveWorktree failed:
// its failure is returned and deletion stops (the agent directory remains).
func TestDeleteAgentFiles_WorktreePerAgentFallbackRemovalErrorReturned(t *testing.T) {
	const agentName = "wt-fallback"
	projectPath, wt := setupWorktreePerAgentProject(t, agentName)
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failRemoveWorktree(t, errors.New("injected worktree removal failure"))
	failed := failRemoveAllSafeAt(t, "worktrees", agentName)

	_, err := DeleteAgentFiles(agentName, projectPath, false)
	assertRemovalFailureReturned(t, err, failed, wt)
	if _, statErr := os.Stat(filepath.Join(projectPath, config.DotScion, "agents", agentName)); statErr != nil {
		t.Fatalf("agent directory should remain when deletion stops: %v", statErr)
	}
}

// TestDeleteAgentFiles_WorktreePerAgentPlainDirRemovalErrorReturned covers
// the removal of a worktree-per-agent directory that holds no .git: its
// failure is returned and deletion stops.
func TestDeleteAgentFiles_WorktreePerAgentPlainDirRemovalErrorReturned(t *testing.T) {
	const agentName = "wt-plain"
	projectPath, wt := setupWorktreePerAgentProject(t, agentName)
	failed := failRemoveAllSafeAt(t, "worktrees", agentName)

	_, err := DeleteAgentFiles(agentName, projectPath, false)
	assertRemovalFailureReturned(t, err, failed, wt)
	if _, statErr := os.Stat(filepath.Join(projectPath, config.DotScion, "agents", agentName)); statErr != nil {
		t.Fatalf("agent directory should remain when deletion stops: %v", statErr)
	}
}

// TestDeleteAgentFiles_AgentWorkspaceFallbackRemovalErrorReturned covers the
// removal of the agent's workspace worktree after RemoveWorktree failed: its
// failure is returned and deletion stops.
func TestDeleteAgentFiles_AgentWorkspaceFallbackRemovalErrorReturned(t *testing.T) {
	const agentName = "ws-fallback"
	_, scionDir := setupDeleteRemovalProject(t)
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	failRemoveWorktree(t, errors.New("injected worktree removal failure"))
	failed := failRemoveAllSafeAt(t, agentName, "workspace")

	_, err := DeleteAgentFiles(agentName, scionDir, true)
	assertRemovalFailureReturned(t, err, failed, agentWorkspace)
}

// TestDeleteAgentFiles_LiveWorkspaceEntryRemovalErrorReturned covers the
// removal of the other entries of an agent directory whose workspace is still
// a live worktree: an entry's removal failure is returned.
func TestDeleteAgentFiles_LiveWorkspaceEntryRemovalErrorReturned(t *testing.T) {
	const agentName = "ws-live"
	_, scionDir := setupDeleteRemovalProject(t)
	agentDir := filepath.Join(scionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	entry := filepath.Join(agentDir, "scion-agent.json")
	if err := os.WriteFile(entry, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A containment refusal leaves the workspace in place (no fallback), so
	// phase 2 removes the other entries one by one.
	failRemoveWorktree(t, fmt.Errorf("injected: %w", util.ErrPathNotContained))
	failed := failRemoveAllSafeAt(t, agentName, "scion-agent.json")

	_, err := DeleteAgentFiles(agentName, scionDir, true)
	assertRemovalFailureReturned(t, err, failed, entry)
}

// TestDeleteAgentFiles_AgentDirRemovalErrorReturned covers the removal of
// the agent directory itself: its failure is returned.
func TestDeleteAgentFiles_AgentDirRemovalErrorReturned(t *testing.T) {
	const agentName = "plain-dir"
	_, scionDir := setupDeleteRemovalProject(t)
	agentDir := filepath.Join(scionDir, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	failed := failRemoveAllSafeAt(t, "agents", agentName)

	_, err := DeleteAgentFiles(agentName, scionDir, false)
	assertRemovalFailureReturned(t, err, failed, agentDir)
}

// setupExternalStateDir creates a split-storage git project and the agent's
// external state directory, and returns the .scion directory and the state
// directory.
func setupExternalStateDir(t *testing.T, agentName string) (string, string) {
	t.Helper()
	_, scionDir := setupDeleteRemovalProject(t)
	if err := config.WriteProjectID(scionDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatalf("WriteProjectID: %v", err)
	}
	extAgentsDir, err := config.GetGitProjectExternalAgentsDir(scionDir)
	if err != nil || extAgentsDir == "" {
		t.Fatalf("GetGitProjectExternalAgentsDir: dir=%q err=%v", extAgentsDir, err)
	}
	extAgentDir := filepath.Join(extAgentsDir, agentName)
	if err := os.MkdirAll(filepath.Join(extAgentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	return scionDir, extAgentDir
}

// TestDeleteAgentFiles_ExternalStateDirUnsupportedErrorReturned covers the
// removal of the agent's external state directory: an error wrapping
// errors.ErrUnsupported is returned, on every platform.
func TestDeleteAgentFiles_ExternalStateDirUnsupportedErrorReturned(t *testing.T) {
	const agentName = "ext-state"
	scionDir, extAgentDir := setupExternalStateDir(t, agentName)
	failed := failRemoveAllSafeAt(t, filepath.Base(filepath.Dir(extAgentDir)), agentName)

	_, err := DeleteAgentFiles(agentName, scionDir, false)
	assertRemovalFailureReturned(t, err, failed, extAgentDir)
}
