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

package runtimebroker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ptone/scion#3274: two Runtime Broker instances of one host
// provisioning worktrees for the same shared project serialize on the
// host's workspace lock.

func provisionInput(name, projectPath, repo string) startContextInputs {
	return startContextInputs{
		Name: name, AgentID: name, ProjectID: "p-shared", ProjectSlug: "shared", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: repo, Branch: "main"}},
	}
}

// TestWorkspaceLocks_ProvisioningWaitsForTheOtherInstance: while one
// instance holds the project, the other instance's worktree provisioning
// for the same project (through a symlink alias of its path) waits, and it
// completes once the project is released.
func TestWorkspaceLocks_ProvisioningWaitsForTheOtherInstance(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skipf("worktree mode not eligible on this host: %s", reason)
	}
	repo := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "shared-project")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(projectPath, alias))

	locks := NewWorkspaceLocks()
	a := &Server{workspaceLocks: locks}
	b := &Server{workspaceLocks: locks}

	unlock, err := a.lockProjectWorkspace(context.Background(), projectPath, "")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, _, err := b.tryProvisionWorktree(context.Background(), provisionInput("agent-b", alias, repo), &api.StartOptions{}, map[string]string{}, "")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("B provisioned (%v) while A holds the project", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(projectPath, "workspace")); err == nil {
		t.Fatal("B touched the shared base while A holds the project")
	}
	unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("B did not provision after A released the project")
	}
}

// TestWorkspaceLocks_ConcurrentProvisioningKeepsTheWorkspaceConsistent:
// two instances provisioning several agents of one project at the same
// time all succeed, each agent gets its own worktree, and the shared base
// stays a consistent repository with exactly those worktrees.
func TestWorkspaceLocks_ConcurrentProvisioningKeepsTheWorkspaceConsistent(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skipf("worktree mode not eligible on this host: %s", reason)
	}
	repo := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "shared-project")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	locks := NewWorkspaceLocks()
	servers := []*Server{{workspaceLocks: locks}, {workspaceLocks: locks}}

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("agent-%d", i)
			ok, _, err := servers[i%2].tryProvisionWorktree(context.Background(), provisionInput(name, projectPath, repo), &api.StartOptions{}, map[string]string{}, "")
			if err == nil && !ok {
				err = fmt.Errorf("%s: not provisioned", name)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	base := filepath.Join(projectPath, "workspace")
	out, err := exec.Command("git", "-C", base, "worktree", "list", "--porcelain").CombinedOutput()
	require.NoError(t, err, string(out))
	for i := 0; i < 6; i++ {
		wt := filepath.Join(base, "worktrees", fmt.Sprintf("agent-%d", i))
		require.Contains(t, string(out), wt, "agent-%d's worktree is registered", i)
		require.FileExists(t, filepath.Join(wt, ".git"))
	}
	require.Equal(t, 7, strings.Count(string(out), "worktree "), "the base plus exactly one worktree per agent")
	fsck, err := exec.Command("git", "-C", base, "fsck", "--no-progress").CombinedOutput()
	require.NoError(t, err, string(fsck))
}
