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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Partition validation (ptone/scion#3274): a start in flight on one flat instance never
// removes another instance's (or an unlabeled) object on the shared
// daemon, even one with the same name in the same project; a legacy
// manager's pre-start cleanup is unchanged.
func TestOwnerPartition_StartPreCleanNeverRemovesForeignObject(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	require.NoError(t, config.InitMachine(getTestHarnesses()))
	root := filepath.Join(tmpDir, "project")
	require.NoError(t, config.InitProject(filepath.Join(root, ".scion"), getTestHarnesses()))
	require.NoError(t, os.Chdir(root))

	foreign := map[string]map[string]string{
		"another instance": {"scion.name": "worker", "scion.project_id": "proj-1", api.LabelRuntimeBrokerID: "broker-b"},
		"unlabeled":        {"scion.name": "worker", "scion.project_id": "proj-1"},
	}
	for name, labels := range foreign {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s owned=%v", name, owned), func(t *testing.T) {
				var deleted []string
				rt := &runtime.MockRuntime{
					ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
						return []api.AgentInfo{{ID: "cid-foreign", ContainerID: "cid-foreign", Name: "worker", Phase: "stopped",
							ContainerStatus: "Exited (0)", Labels: labels}}, nil
					},
					RunFunc: func(context.Context, runtime.RunConfig) (string, error) { return "cid-new", nil },
					DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
						deleted = append(deleted, ref.ID)
						return nil
					},
				}
				m := NewManager(rt).(*AgentManager)
				if owned {
					m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
				}
				_, _ = m.Start(context.Background(), api.StartOptions{Name: "worker", ProjectPath: root, HarnessConfig: "claude", NoAuth: true,
					Env: map[string]string{"SCION_PROJECT_ID": "proj-1", "SCION_AGENT_ID": "agent-a1"}})
				if owned {
					assert.NotContains(t, deleted, "cid-foreign", "an owned start removed a foreign object")
				} else {
					assert.Contains(t, deleted, "cid-foreign", "legacy pre-start cleanup is unchanged")
				}
			})
		}
	}
}

// TestOwnerPartition_ProvisionTakesWorkspaceLockOnRepo: an owned manager's
// provisioning of an agent in a git project takes the host's workspace lock
// on the repository root before the worktree is created and releases it
// afterwards; a legacy manager takes none.
func TestOwnerPartition_ProvisionTakesWorkspaceLockOnRepo(t *testing.T) {
	mockRuntimeForTest(t)
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	require.NoError(t, config.InitMachine(getTestHarnesses()))
	projectDir := filepath.Join(tmpDir, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	setupGitRepo(t, projectDir)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte(".scion/agents/\n"), 0o644))
	require.NoError(t, config.InitProject(filepath.Join(projectDir, ".scion"), getTestHarnesses()))
	require.NoError(t, os.Chdir(projectDir))
	repoRoot, err := filepath.EvalSymlinks(projectDir)
	require.NoError(t, err)

	var events []string
	lock := func(_ context.Context, paths ...string) (func(), error) {
		for _, p := range paths {
			real, _ := filepath.EvalSymlinks(p)
			events = append(events, "lock:"+real)
		}
		worktree := filepath.Join(projectDir, ".scion", "agents", "locked-agent", "workspace", ".git")
		if _, err := os.Stat(worktree); err == nil {
			events = append(events, "worktree-before-lock")
		}
		return func() {
			if _, err := os.Stat(worktree); err == nil {
				events = append(events, "unlock-after-worktree")
			} else {
				events = append(events, "unlock")
			}
		}, nil
	}
	m := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a", WorkspaceLock: lock})
	_, err = m.Provision(context.Background(), api.StartOptions{Name: "locked-agent", ProjectPath: projectDir, HarnessConfig: "claude", NoAuth: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"lock:" + repoRoot, "unlock-after-worktree"}, events)

	events = nil
	legacy := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	_, err = legacy.Provision(context.Background(), api.StartOptions{Name: "legacy-agent", ProjectPath: projectDir, HarnessConfig: "claude", NoAuth: true})
	require.NoError(t, err)
	assert.Empty(t, events, "a legacy manager takes no workspace lock")
}
