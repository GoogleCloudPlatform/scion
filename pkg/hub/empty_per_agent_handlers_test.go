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
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/require"
)

// Handler-side tests for the empty-per-agent workspace mode (design #2703).

// newEmptyPerAgentHandlerProject creates a non-git project labelled
// per-agent (=> empty-per-agent) with one provider broker, which advertises
// the emptyPerAgentWorkspace capability iff capable.
func newEmptyPerAgentHandlerProject(t *testing.T, s store.Store, suffix string, capable bool) (*store.Project, *store.RuntimeBroker) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:       tid("broker-epah-" + suffix),
		Name:     "epah-broker-" + suffix,
		Slug:     "epah-broker-" + suffix,
		Endpoint: "http://localhost:9801",
		Status:   store.BrokerStatusOnline,
	}
	if capable {
		broker.Capabilities = &store.BrokerCapabilities{EmptyPerAgentWorkspace: true}
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID:     tid("proj-epah-" + suffix),
		Name:   "epah-" + suffix,
		Slug:   "epah-" + suffix,
		Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent},
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	return project, broker
}

func listProjectAgents(t *testing.T, s store.Store, projectID string) []store.Agent {
	t.Helper()
	res, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	return res.Items
}

func TestCreateAgent_EmptyPerAgent_RejectsWorkspacePath(t *testing.T) {
	srv, s := testServer(t)
	project, _ := newEmptyPerAgentHandlerProject(t, s, "ws", true)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "ws-agent",
		ProjectID: project.ID,
		Workspace: "./sub",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "do not take a workspace path")
	require.Empty(t, listProjectAgents(t, s, project.ID))
}

func TestCreateAgent_EmptyPerAgent_BrokerWithoutCapability_412(t *testing.T) {
	srv, s := testServer(t)
	project, _ := newEmptyPerAgentHandlerProject(t, s, "nocap", false)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "nocap-agent",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), ErrCodeUnsupportedCapability)
	require.Contains(t, rec.Body.String(), "empty-per-agent")
	require.Empty(t, listProjectAgents(t, s, project.ID), "no agent may be persisted when failing closed")
}

func TestCreateAgent_EmptyPerAgent_NoSharedWorkspaceOrStorage(t *testing.T) {
	srv, s := testServer(t)
	project, _ := newEmptyPerAgentHandlerProject(t, s, "cap", true)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "cap-agent",
		ProjectID: project.ID,
	})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())

	agents := listProjectAgents(t, s, project.ID)
	require.Len(t, agents, 1)
	cfg := agents[0].AppliedConfig
	require.NotNil(t, cfg)
	require.Empty(t, cfg.Workspace, "empty-per-agent must not mount the hub project workspace")
	require.Empty(t, cfg.WorkspaceStoragePath, "empty-per-agent must not upload the project workspace")
}

// TestCreateAgent_EmptyPerAgent_IgnoresWorkspaceFiles: workspace bootstrap
// files are ignored with a warning (not a 400), so no upload URLs are issued
// and no per-agent storage path is set (review #2717 N2). Without the skip,
// this server has no storage configured and would fail the bootstrap.
func TestCreateAgent_EmptyPerAgent_IgnoresWorkspaceFiles(t *testing.T) {
	srv, s := testServer(t)
	project, _ := newEmptyPerAgentHandlerProject(t, s, "files", true)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:           "files-agent",
		ProjectID:      project.ID,
		Task:           "do something",
		WorkspaceFiles: []transfer.FileInfo{{Path: "main.go", Size: 10, Hash: "sha256:abc"}},
	})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Contains(t, resp.Warnings, api.WarningEmptyPerAgentWorkspaceFilesIgnored)
	require.Empty(t, resp.UploadURLs, "no bootstrap upload for empty-per-agent")

	agents := listProjectAgents(t, s, project.ID)
	require.Len(t, agents, 1)
	if cfg := agents[0].AppliedConfig; cfg != nil {
		require.Empty(t, cfg.WorkspaceStoragePath)
	}
}

func TestSyncsHubProjectWorkspace(t *testing.T) {
	cases := []struct {
		name    string
		project *store.Project
		want    bool
	}{
		{"nil", nil, false},
		{"hub-managed non-git", &store.Project{}, true},
		{"empty-per-agent", &store.Project{Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent}}, false},
		{"git shared", &store.Project{GitRemote: "github.com/a/b", Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}}, true},
		{"git per-agent clone", &store.Project{GitRemote: "github.com/a/b"}, false},
		{"git worktree-per-agent", &store.Project{GitRemote: "github.com/a/b", Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncsHubProjectWorkspace(tc.project); got != tc.want {
				t.Errorf("syncsHubProjectWorkspace = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStartAgent_EmptyPerAgent_BrokerWithoutCapability_412: the lifecycle
// start path maps the dispatcher's fail-closed error to 412 and never calls
// the broker.
func TestStartAgent_EmptyPerAgent_BrokerWithoutCapability_412(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	project, broker := newEmptyPerAgentHandlerProject(t, s, "start", false)
	agent := newQuotaTestAgent(t, s, broker, project, "epah-start", state.PhaseStopped)

	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	require.True(t, strings.Contains(rec.Body.String(), ErrCodeUnsupportedCapability), rec.Body.String())
	require.False(t, client.startCalled || client.createCalled, "broker must not be called")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseStopped), got.Phase, "a refused start must not change the agent phase")
}

// TestRestartAgent_EmptyPerAgent_BrokerWithoutCapability_412: restart is
// refused before its stop leg, so the running agent is not stopped
// (review #2717 N3).
func TestRestartAgent_EmptyPerAgent_BrokerWithoutCapability_412(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	project, broker := newEmptyPerAgentHandlerProject(t, s, "restart", false)
	agent := newQuotaTestAgent(t, s, broker, project, "epah-restart", state.PhaseRunning)

	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), ErrCodeUnsupportedCapability)
	require.False(t, client.stopCalled, "restart must not stop the agent when the start would be refused")
	require.False(t, client.startCalled || client.restartCalled, "broker must not be started")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseRunning), got.Phase)
}

// TestWakeAgentForDM_EmptyPerAgent_BrokerWithoutCapability_412: waking a
// suspended agent for a DM maps the fail-closed dispatch error to 412
// unsupported_capability, like the other sites (review #2717 r2 finding 3).
func TestWakeAgentForDM_EmptyPerAgent_BrokerWithoutCapability_412(t *testing.T) {
	srv, s := testServer(t)
	project, broker := newEmptyPerAgentHandlerProject(t, s, "wake", false)
	agent := newQuotaTestAgent(t, s, broker, project, "epah-wake", state.PhaseSuspended)

	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

	_, dmErr := srv.wakeAgentForDM(context.Background(), agent)
	require.NotNil(t, dmErr)
	require.Equal(t, http.StatusPreconditionFailed, dmErr.HTTPStatus, dmErr.Message)
	require.Equal(t, ErrCodeUnsupportedCapability, dmErr.Code)
	require.False(t, client.startCalled, "broker must not be called")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, string(state.PhaseSuspended), got.Phase)
}
