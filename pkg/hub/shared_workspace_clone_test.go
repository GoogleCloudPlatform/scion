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

//go:build !no_sqlite

package hub

import (
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSharedWorkspaceCloneConfig(t *testing.T) {
	shared := &store.Project{
		GitRemote: "github.com/test/shared",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
			store.LabelDefaultBranch: "develop",
		},
	}
	gc := sharedWorkspaceCloneConfig(shared)
	require.NotNil(t, gc)
	assert.Equal(t, resolveCloneURL("", "github.com/test/shared"), gc.URL)
	assert.Equal(t, "develop", gc.Branch)
	require.NotNil(t, gc.Depth)
	assert.Equal(t, 0, *gc.Depth, "the shared workspace is a full clone, like the hub's own")

	override := &store.Project{
		GitRemote: "github.com/test/shared",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
			store.LabelCloneURL:      "https://git.example.com/mirror/shared.git",
		},
	}
	gc = sharedWorkspaceCloneConfig(override)
	require.NotNil(t, gc)
	assert.Equal(t, "https://git.example.com/mirror/shared.git", gc.URL)
	assert.Equal(t, "main", gc.Branch)

	for name, p := range map[string]*store.Project{
		"nil":                nil,
		"per-agent git":      {GitRemote: "github.com/test/repo"},
		"worktree-per-agent": {GitRemote: "github.com/test/repo", Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}},
		"non-git shared":     {Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}},
	} {
		assert.Nil(t, sharedWorkspaceCloneConfig(p), name)
	}
}

// newSharedCloneDispatchFixture creates a project with the given labels and
// git remote, a broker, and a dispatcher with a mock broker client.
func newSharedCloneDispatchFixture(t *testing.T, gitRemote string, labels map[string]string) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	project := &store.Project{
		ID:        tid("project-shared-clone"),
		Name:      "Shared Clone",
		Slug:      "shared-clone",
		GitRemote: gitRemote,
		Labels:    labels,
	}
	require.NoError(t, memStore.CreateProject(ctx, project))
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:       tid("broker-shared-clone"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}))
	client := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
	agent := &store.Agent{
		ID:              tid("agent-shared-clone"),
		Name:            "shared-agent",
		Slug:            "shared-agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: tid("broker-shared-clone"),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	return dispatcher, client, agent
}

func TestHTTPAgentDispatcher_SharedPlainGit_SendsSharedWorkspaceClone(t *testing.T) {
	labels := map[string]string{
		store.LabelWorkspaceMode: store.WorkspaceModeShared,
		store.LabelDefaultBranch: "trunk",
	}
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "github.com/test/shared", labels)
	agent.AppliedConfig.Workspace = "/home/user/.scion/projects/shared-clone"

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	cfg := client.lastCreateReq.Config
	require.NotNil(t, cfg)
	assert.True(t, cfg.SharedWorkspace)
	assert.Nil(t, cfg.GitClone, "a shared workspace is never cloned per agent")
	require.NotNil(t, cfg.SharedWorkspaceClone)
	assert.Equal(t, resolveCloneURL("", "github.com/test/shared"), cfg.SharedWorkspaceClone.URL)
	assert.Equal(t, "trunk", cfg.SharedWorkspaceClone.Branch)
	require.NotNil(t, cfg.SharedWorkspaceClone.Depth)
	assert.Equal(t, 0, *cfg.SharedWorkspaceClone.Depth)

	require.NoError(t, dispatcher.DispatchAgentStart(context.Background(), agent, "", false))
	ws := client.lastStartExtras.Workspace
	assert.Nil(t, ws.GitClone)
	require.NotNil(t, ws.SharedWorkspaceClone)
	assert.Equal(t, cfg.SharedWorkspaceClone.URL, ws.SharedWorkspaceClone.URL)
	assert.Equal(t, "trunk", ws.SharedWorkspaceClone.Branch)
}

func TestHTTPAgentDispatcher_PerAgentGit_NoSharedWorkspaceClone(t *testing.T) {
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "github.com/test/repo", nil)
	depth := 1
	agent.AppliedConfig.GitClone = &api.GitCloneConfig{URL: "https://github.com/test/repo.git", Branch: "main", Depth: &depth}

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	cfg := client.lastCreateReq.Config
	require.NotNil(t, cfg)
	assert.NotNil(t, cfg.GitClone)
	assert.False(t, cfg.SharedWorkspace)
	assert.Nil(t, cfg.SharedWorkspaceClone)

	require.NoError(t, dispatcher.DispatchAgentStart(context.Background(), agent, "", false))
	assert.NotNil(t, client.lastStartExtras.Workspace.GitClone)
	assert.Nil(t, client.lastStartExtras.Workspace.SharedWorkspaceClone)
}

func TestHTTPAgentDispatcher_NonGitProject_NoClone(t *testing.T) {
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "", nil)

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	cfg := client.lastCreateReq.Config
	require.NotNil(t, cfg)
	assert.Nil(t, cfg.GitClone)
	assert.Nil(t, cfg.SharedWorkspaceClone)
}

func TestApplyStartExtras_SharedWorkspaceClone(t *testing.T) {
	depth := 0
	gc := &api.GitCloneConfig{URL: "https://github.com/test/shared.git", Branch: "main", Depth: &depth}
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{Workspace: WorkspaceDispatchSpec{SharedWorkspaceClone: gc}})
	assert.Equal(t, gc, payload["sharedWorkspaceClone"])
	assert.NotContains(t, payload, "gitClone")

	empty := map[string]interface{}{}
	applyStartExtras(empty, StartExtras{})
	assert.NotContains(t, empty, "sharedWorkspaceClone")
}

func TestPopulateAgentConfig_SharedWorkspace_LeavesGitCloneNil(t *testing.T) {
	srv, _ := testServer(t)
	project := &store.Project{
		ID:        tid("project-shared-pop"),
		Slug:      "shared-pop",
		GitRemote: "github.com/test/shared",
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
	}
	agent := &store.Agent{ID: "agent-shared-pop", AppliedConfig: &store.AgentAppliedConfig{}}
	srv.populateAgentConfig(context.Background(), agent, project, nil)
	assert.Nil(t, agent.AppliedConfig.GitClone)

	perAgent := &store.Project{ID: tid("project-per-agent-pop"), Slug: "per-agent-pop", GitRemote: "github.com/test/repo"}
	agent2 := &store.Agent{ID: "agent-per-agent-pop", AppliedConfig: &store.AgentAppliedConfig{}}
	srv.populateAgentConfig(context.Background(), agent2, perAgent, nil)
	require.NotNil(t, agent2.AppliedConfig.GitClone)
	assert.Equal(t, resolveCloneURL("", "github.com/test/repo"), agent2.AppliedConfig.GitClone.URL)
	assert.Equal(t, "main", agent2.AppliedConfig.GitClone.Branch)
	require.NotNil(t, agent2.AppliedConfig.GitClone.Depth)
	assert.Equal(t, 1, *agent2.AppliedConfig.GitClone.Depth)
}
