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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Instance ownership on the manager (ptone/scion#3274).

func ownedEntries() []api.AgentInfo {
	return []api.AgentInfo{
		{ID: "c-a", ContainerID: "c-a", Name: "worker", Labels: map[string]string{"scion.name": "worker", api.LabelRuntimeBrokerID: "broker-a"}},
		{ID: "c-b", ContainerID: "c-b", Name: "worker", Labels: map[string]string{"scion.name": "worker", api.LabelRuntimeBrokerID: "broker-b"}},
		{ID: "c-legacy", ContainerID: "c-legacy", Name: "worker", Labels: map[string]string{"scion.name": "worker"}},
	}
}

func TestOwner_ListSeesOnlyOwnedRuntimeObjects(t *testing.T) {
	rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return ownedEntries(), nil }}
	m := NewManager(rt).(*AgentManager)

	legacy, err := m.listRuntime(context.Background(), map[string]string{"scion.name": "worker"})
	require.NoError(t, err)
	assert.Len(t, legacy, 3, "a legacy manager is unchanged")

	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	owned, err := m.listRuntime(context.Background(), map[string]string{"scion.name": "worker"})
	require.NoError(t, err)
	require.Len(t, owned, 1)
	assert.Equal(t, "c-a", owned[0].ContainerID, "another instance's and unlabeled objects are not owned")
	assert.Equal(t, "broker-a", m.OwnerRuntimeBrokerID())
}

func TestOwner_ListErrorIsNotRetriedUnfiltered(t *testing.T) {
	calls := 0
	rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		calls++
		return nil, errors.New("daemon unavailable")
	}}
	m := NewManager(rt).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	_, err := m.listRuntime(context.Background(), map[string]string{"scion.name": "worker"})
	require.Error(t, err)
	assert.Equal(t, 1, calls)
}

func TestOwner_StopNeverPassesAnUnresolvedName(t *testing.T) {
	var stopped []string
	rt := &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return ownedEntries()[1:], nil },
		StopFunc: func(_ context.Context, ref runtime.RunRef) error { stopped = append(stopped, ref.ID); return nil },
	}
	m := NewManager(rt).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})

	err := m.Stop(context.Background(), "worker", "", "")
	assert.ErrorIs(t, err, ErrNotOwned)
	assert.Empty(t, stopped, "neither another instance's object nor the bare name is stopped")

	rt.ListFunc = func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, errors.New("down") }
	assert.Error(t, m.Stop(context.Background(), "worker", "", ""))
	assert.Empty(t, stopped, "a list failure never falls back to the bare name")
}

func TestOwner_LabelsCarryTheReservedOwner(t *testing.T) {
	m := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	assert.Nil(t, m.ownerLabels())
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	assert.Equal(t, map[string]string{api.LabelRuntimeBrokerID: "broker-a"}, m.ownerLabels())

	assert.False(t, m.ownsFileAgent("p", "worker"), "no record callback: no file-only agent is owned")
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a", FileAgentOwned: func(p, s string) bool { return p == "p" && s == "worker" }})
	assert.True(t, m.ownsFileAgent("p", "worker"))
	assert.False(t, m.ownsFileAgent("p", "other"))
}

func TestOwner_UnresolvedLabelledObjectIsNotOwned(t *testing.T) {
	rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{ID: "c-1", ContainerID: "c-1", Name: "worker", Labels: map[string]string{"scion.name": "worker", "agent_id": "agent-1", api.LabelRuntimeBrokerID: "broker-a"}},
			{ID: "c-2", ContainerID: "c-2", Name: "helper", Labels: map[string]string{"scion.name": "helper", "agent_id": "agent-2", api.LabelRuntimeBrokerID: "broker-a"}},
		}, nil
	}}
	m := NewManager(rt).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a", EntryUnresolved: func(l map[string]string) bool { return l["agent_id"] == "agent-1" }})
	owned, err := m.listRuntime(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, owned, 1, "an object of a conflicting key is not owned even with this instance's label")
	assert.Equal(t, "c-2", owned[0].ContainerID)
}

// TestOwner_ListWritesOnlyUnderTrustedEntryPath: an owned manager's List
// converges agent-info.json to the runtime's terminal phase only under a
// project path its EntryPathTrusted accepts.
func TestOwner_ListWritesOnlyUnderTrustedEntryPath(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%v", trusted), func(t *testing.T) {
			projectPath := filepath.Join(t.TempDir(), ".scion")
			agentHome := filepath.Join(projectPath, "agents", "worker", "home")
			require.NoError(t, os.MkdirAll(agentHome, 0o755))
			infoPath := filepath.Join(agentHome, "agent-info.json")
			require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"worker","phase":"running","activity":"thinking"}`), 0o644))
			zero := 0
			rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{Name: "worker", ProjectPath: projectPath, Phase: "stopped", ExitCode: &zero,
					ContainerStatus: "Exited (0) 1 minute ago",
					Labels:          map[string]string{"scion.name": "worker", "scion.project_id": "proj-1", api.LabelRuntimeBrokerID: "broker-a"}}}, nil
			}}
			m := NewManager(rt).(*AgentManager)
			var gotPath, gotProject string
			m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a", EntryPathTrusted: func(p, id string) bool {
				gotPath, gotProject = p, id
				return trusted
			}})
			_, err := m.List(context.Background(), map[string]string{"scion.name": "worker"})
			require.NoError(t, err)
			assert.Equal(t, projectPath, gotPath)
			assert.Equal(t, "proj-1", gotProject)
			data, err := os.ReadFile(infoPath)
			require.NoError(t, err)
			assert.Equal(t, trusted, strings.Contains(string(data), `"phase": "stopped"`), "agent-info.json written: %s", data)
		})
	}
}

// TestOwner_DeliverWithoutProjectNeverPicksAmongSeveral: an owned manager
// refuses to deliver to a name that matches several of its agents when no
// project is given; with a project, or for a legacy manager, delivery is
// unchanged.
func TestOwner_DeliverWithoutProjectNeverPicksAmongSeveral(t *testing.T) {
	var execs []string
	rt := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			all := []api.AgentInfo{
				{ContainerID: "c-1", Name: "worker", ContainerStatus: "Up 1 minute", Labels: map[string]string{"scion.name": "worker", "scion.project_id": "proj-1", api.LabelRuntimeBrokerID: "broker-a"}},
				{ContainerID: "c-2", Name: "worker", ContainerStatus: "Up 1 minute", Labels: map[string]string{"scion.name": "worker", "scion.project_id": "proj-2", api.LabelRuntimeBrokerID: "broker-a"}},
			}
			var out []api.AgentInfo
			for _, a := range all {
				if p := filter["scion.project_id"]; p == "" || a.Labels["scion.project_id"] == p {
					out = append(out, a)
				}
			}
			return out, nil
		},
		ExecFunc: func(_ context.Context, id string, _ []string) (string, error) {
			execs = append(execs, id)
			return "", nil
		},
	}
	m := NewManager(rt).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	err := m.deliverImmediate(context.Background(), "worker", "", "hello", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Empty(t, execs, "nothing delivered to an ambiguous name")

	require.NoError(t, m.deliverImmediate(context.Background(), "worker", "proj-2", "hello", true))
	require.NotEmpty(t, execs)
	for _, id := range execs {
		assert.Equal(t, "c-2", id)
	}
}

// TestOwner_StartVerifiesOnlyItsOwnProjectsAgent: after the runtime creates
// the container, an owned manager's verification never matches another
// project's stopped agent of the same name (which would delete the new
// container and fail the start); a legacy manager's name match is unchanged.
func TestOwner_StartVerifiesOnlyItsOwnProjectsAgent(t *testing.T) {
	mockRuntimeForTest(t)
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	require.NoError(t, config.InitMachine(getTestHarnesses()))
	root := filepath.Join(tmpDir, "project")
	require.NoError(t, config.InitProject(filepath.Join(root, ".scion"), getTestHarnesses()))
	require.NoError(t, os.Chdir(root))

	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprintf("owned=%v", owned), func(t *testing.T) {
			var deleted []string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{{ContainerID: "other-id", Name: "worker", Phase: "stopped", ContainerStatus: "Exited (0)",
						Labels: map[string]string{"scion.name": "worker", "scion.project_id": "proj-2", api.LabelRuntimeBrokerID: "broker-a"}}}, nil
				},
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) { return "new-id", nil },
				DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
					deleted = append(deleted, ref.ID)
					return nil
				},
			}
			m := NewManager(rt).(*AgentManager)
			if owned {
				m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
			}
			_, err := m.Start(context.Background(), api.StartOptions{Name: "worker", ProjectPath: root, HarnessConfig: "claude", NoAuth: true,
				Env: map[string]string{"SCION_PROJECT_ID": "proj-1", "SCION_AGENT_ID": "agent-1"}})
			if owned {
				require.NoError(t, err)
				assert.NotContains(t, deleted, "new-id", "the new container is kept")
			} else {
				require.Error(t, err, "legacy behaviour: the name match reports an immediate exit")
			}
		})
	}
}

// TestOwner_CleanupLaunchDeletesOnlyOwnedHandles: an owned manager deletes
// (with the UID precondition) only the handles its instance owns; others
// are reported as not owned and never reach the runtime.
func TestOwner_CleanupLaunchDeletesOnlyOwnedHandles(t *testing.T) {
	rt := &uidPreconditionFakeRuntime{Runtime: &runtime.MockRuntime{}}
	m := &AgentManager{Runtime: rt}
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a", LaunchHandleOwned: func(h api.ResourceHandle) bool { return h.UID == "uid-mine" }})
	err := m.CleanupLaunch(context.Background(), []ResourceHandle{
		{Kind: "pod", Namespace: "ns", Name: "worker", UID: "uid-mine"},
		{Kind: "pod", Namespace: "ns", Name: "worker", UID: "uid-other"},
	})
	require.ErrorIs(t, err, ErrNotOwned)
	require.Len(t, rt.deleted, 1)
	assert.Equal(t, "uid-mine", rt.deleted[0].UID)
	assert.Empty(t, rt.plainDeletes)

	none := &uidPreconditionFakeRuntime{Runtime: &runtime.MockRuntime{}}
	m = &AgentManager{Runtime: none}
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	require.ErrorIs(t, m.CleanupLaunch(context.Background(), []ResourceHandle{{Kind: "pod", Name: "worker", UID: "uid-mine"}}), ErrNotOwned)
	assert.Empty(t, none.deleted, "no ownership callback: nothing is deleted")
}

// leftoverCleanerRuntime records which leftover cleanup a manager calls.
type leftoverCleanerRuntime struct {
	runtime.Runtime
	calls []string
}

func (c *leftoverCleanerRuntime) CleanupAgentResources(_ context.Context, name, projectID, _ string) error {
	c.calls = append(c.calls, "unscoped:"+projectID+"/"+name)
	return nil
}

type ownedLeftoverCleanerRuntime struct{ *leftoverCleanerRuntime }

func (c ownedLeftoverCleanerRuntime) CleanupOwnedAgentResources(_ context.Context, name, projectID, brokerID, runID string) error {
	c.calls = append(c.calls, "owned:"+projectID+"/"+name+"/"+brokerID+"@"+runID)
	return nil
}

// TestOwner_LeftoverCleanupIsOwnerScoped: an owned manager uses only the
// owner-scoped cleanup, and on a runtime without it removes nothing; a
// legacy manager keeps the unscoped cleanup.
func TestOwner_LeftoverCleanupIsOwnerScoped(t *testing.T) {
	base := &leftoverCleanerRuntime{Runtime: &runtime.MockRuntime{}}
	legacy := &AgentManager{Runtime: base}
	require.NoError(t, legacy.CleanupAgentResources(context.Background(), "worker", "p1", ""))
	assert.Equal(t, []string{"unscoped:p1/worker"}, base.calls)

	plain := &leftoverCleanerRuntime{Runtime: &runtime.MockRuntime{}}
	m := &AgentManager{Runtime: plain}
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	require.NoError(t, m.CleanupAgentResources(context.Background(), "worker", "p1", "run-2"))
	assert.Empty(t, plain.calls, "no unscoped cleanup for an owned manager")

	inner := &leftoverCleanerRuntime{Runtime: &runtime.MockRuntime{}}
	m = &AgentManager{Runtime: ownedLeftoverCleanerRuntime{inner}}
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	require.NoError(t, m.CleanupAgentResources(context.Background(), "worker", "p1", ""))
	require.NoError(t, m.CleanupAgentResources(context.Background(), "worker", "p1", "run-2"))
	assert.Equal(t, []string{"owned:p1/worker/broker-a@", "owned:p1/worker/broker-a@run-2"}, inner.calls,
		"the owned cleanup receives the delete's run unchanged")
}
