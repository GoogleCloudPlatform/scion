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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Agent file deletion is scoped to the target project.

// scopeFixture is a HOME with a global project holding agent "worker"
// (its directory, a nested worktree of repo globalRepo on branch "worker",
// and its global workspace), and a separate git project X.
type scopeFixture struct {
	globalDir, globalAgentDir, globalWorkspace string
	globalRepo                                 string
	projectDir                                 string // X's .scion
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	f := scopeFixture{globalDir: globalDir}
	f.globalAgentDir = filepath.Join(globalDir, "agents", "worker")
	if err := os.MkdirAll(filepath.Join(f.globalAgentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.globalWorkspace = filepath.Join(globalDir, "workspace", "worker")
	if err := os.MkdirAll(f.globalWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// A worktree nested in the global agent's directory, on branch worker.
	f.globalRepo = filepath.Join(t.TempDir(), "global-repo")
	if err := os.MkdirAll(f.globalRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, f.globalRepo)
	if out, err := exec.Command("git", "-C", f.globalRepo, "worktree", "add", "-b", "worker",
		filepath.Join(f.globalAgentDir, "workspace")).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	// Project X (a git repository) with its own agent "worker".
	root := filepath.Join(t.TempDir(), "project-x")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, root)
	f.projectDir = filepath.Join(root, ".scion")
	if err := os.MkdirAll(filepath.Join(f.projectDir, "agents", "worker", "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// assertGlobalAgentIntact: the global project's agent directory, its nested
// worktree and that worktree's branch all survive.
func (f scopeFixture) assertGlobalAgentIntact(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.globalAgentDir); err != nil {
		t.Errorf("the global project's agent directory was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.globalAgentDir, "workspace", ".git")); err != nil {
		t.Errorf("the global project's agent worktree was removed: %v", err)
	}
	if err := exec.Command("git", "-C", f.globalRepo, "rev-parse", "--verify", "--quiet", "refs/heads/worker").Run(); err != nil {
		t.Error("the global project's agent branch was deleted")
	}
	if _, err := os.Stat(f.globalWorkspace); err != nil {
		t.Errorf("the global project's agent workspace was removed: %v", err)
	}
}

// TestDeleteAgentFiles_OtherProjectLeavesGlobalAgentIntact: deleting agent
// "worker" in project X removes X's agent and leaves the global project's
// agent of the same name intact: directory, nested worktree and branch.
func TestDeleteAgentFiles_OtherProjectLeavesGlobalAgentIntact(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := DeleteAgentFiles("worker", f.projectDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.projectDir, "agents", "worker")); !os.IsNotExist(err) {
		t.Errorf("project X's agent was not removed: %v", err)
	}
	f.assertGlobalAgentIntact(t)
}

// TestDeleteAgentFiles_EmptyOrUnresolvablePathLeavesGlobalAgentIntact: a
// project path that resolves to no existing project directory is reported
// and deletes nothing, and an empty path inside project X targets X only;
// neither touches the global project's agent.
func TestDeleteAgentFiles_EmptyOrUnresolvablePathLeavesGlobalAgentIntact(t *testing.T) {
	f := newScopeFixture(t)
	_, err := DeleteAgentFiles("worker", filepath.Join(t.TempDir(), "gone", ".scion"), true)
	if !errors.Is(err, ErrAgentProjectUnresolved) {
		t.Fatalf("missing project: err = %v, want ErrAgentProjectUnresolved", err)
	}
	f.assertGlobalAgentIntact(t)

	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(filepath.Dir(f.projectDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteAgentFiles("worker", "", true); err != nil {
		t.Fatalf("empty path inside project X: %v", err)
	}
	f.assertGlobalAgentIntact(t)
}

// TestDeleteAgentFiles_GlobalProjectTargetStillDeletes: with the global
// project as the target, the global project's agent is removed: its
// directory (with its worktree), its branch and its global workspace.
func TestDeleteAgentFiles_GlobalProjectTargetStillDeletes(t *testing.T) {
	f := newScopeFixture(t)
	if _, err := DeleteAgentFiles("worker", f.globalDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	if _, err := os.Stat(f.globalAgentDir); !os.IsNotExist(err) {
		t.Errorf("the global project's agent directory remains: %v", err)
	}
	if _, err := os.Stat(f.globalWorkspace); !os.IsNotExist(err) {
		t.Errorf("the global project's agent workspace remains: %v", err)
	}
	if err := exec.Command("git", "-C", f.globalRepo, "rev-parse", "--verify", "--quiet", "refs/heads/worker").Run(); err == nil {
		t.Error("the global project's agent branch remains")
	}
}

// TestDeleteTarget_MissingProjectIsNotAnError: the manager's delete treats
// a project that no longer exists as nothing left to delete.
func TestDeleteTarget_MissingProjectIsNotAnError(t *testing.T) {
	f := newScopeFixture(t)
	m := &AgentManager{}
	if _, err := m.deleteResolved(context.Background(), "worker", runtime.RunRef{}, true, filepath.Join(t.TempDir(), "gone", ".scion"), true); err != nil {
		t.Fatalf("delete with a missing project: %v", err)
	}
	f.assertGlobalAgentIntact(t)
}
