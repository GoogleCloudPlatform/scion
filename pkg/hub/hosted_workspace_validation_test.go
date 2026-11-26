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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsAbsWorkspace(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"", false},
		{"sub/dir", false},
		{"./sub/dir", false},
		{"packages/web", false},
		{"/var/log", true},
		{"/tmp/work", true},
		{`\server\share`, true},
		{`\\server\share`, true},
		{`C:\work\project`, true},
		{`c:/work/project`, true},
		{`D:relative-on-drive`, true},
	} {
		assert.Equal(t, tc.want, isAbsWorkspace(tc.path), "path=%q", tc.path)
	}
}

// TestCreateAgent_HostedRefusesAbsoluteWorkspace verifies that a hosted hub
// refuses caller-supplied absolute workspace paths for every project type
// (hub-managed non-git, shared-workspace git, git-clone, and linked) and for
// provision-only requests, returning 400 naming the workspace field without
// storing or dispatching an agent.
func TestCreateAgent_HostedRefusesAbsoluteWorkspace(t *testing.T) {
	projectTypes := []struct {
		name           string
		gitRemote      string
		labels         map[string]string
		providerPath   string
		provisionOnly  bool
		workspaceInput string
	}{
		{
			name:           "hub-managed non-git unix abs",
			workspaceInput: "/var/log",
		},
		{
			name:           "hub-managed non-git windows abs",
			workspaceInput: `C:\work\dir`,
		},
		{
			name:           "hub-managed non-git provisionOnly",
			provisionOnly:  true,
			workspaceInput: "/var/log",
		},
		{
			name:           "shared-workspace git",
			gitRemote:      "https://github.com/example/repo.git",
			labels:         map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
			workspaceInput: "/var/log",
		},
		{
			name:           "git-clone",
			gitRemote:      "https://github.com/example/repo.git",
			workspaceInput: "/var/log",
		},
		{
			name:           "linked project",
			gitRemote:      "https://github.com/example/repo.git",
			providerPath:   "/local/linked/repo",
			workspaceInput: "/var/log",
		},
	}

	for _, tc := range projectTypes {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			require.False(t, srv.workstation, "test servers run hosted")

			ctx := context.Background()
			if tc.gitRemote != "" || tc.labels != nil {
				project.GitRemote = tc.gitRemote
				project.Labels = tc.labels
				require.NoError(t, s.UpdateProject(ctx, project))
			}
			if tc.providerPath != "" {
				providers, err := s.GetProjectProviders(ctx, project.ID)
				require.NoError(t, err)
				require.NotEmpty(t, providers)
				providers[0].LocalPath = tc.providerPath
				require.NoError(t, s.AddProjectProvider(ctx, &providers[0]))
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:          "abs-ws-agent",
				ProjectID:     project.ID,
				Workspace:     tc.workspaceInput,
				ProvisionOnly: tc.provisionOnly,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

			var resp struct {
				Error struct {
					Code    string                 `json:"code"`
					Message string                 `json:"message"`
					Details map[string]interface{} `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
			assert.Equal(t, ErrCodeValidationError, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, "workspace")
			assert.Equal(t, "workspace", resp.Error.Details["field"])
			assert.Nil(t, disp.capturedAgent, "nothing dispatched")

			_, err := s.GetAgentBySlug(ctx, project.ID, "abs-ws-agent")
			assert.ErrorIs(t, err, store.ErrNotFound, "refused create stores nothing")
		})
	}
}

// TestCreateAgent_HostedAllowsRelativeAndEmptyWorkspace verifies that a
// project-relative subdirectory workspace and an empty workspace (Hub-filled
// project workspace path) continue to succeed on a hosted hub.
func TestCreateAgent_HostedAllowsRelativeAndEmptyWorkspace(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	t.Run("relative workspace accepted", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		require.False(t, srv.workstation)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "rel-ws-agent",
			ProjectID: project.ID,
			Workspace: "sub/dir",
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		got, err := s.GetAgentBySlug(context.Background(), project.ID, "rel-ws-agent")
		require.NoError(t, err)
		assert.Equal(t, "sub/dir", got.AppliedConfig.Workspace)
	})

	t.Run("empty workspace on hub-managed project fills HostPath and passes L2", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		require.False(t, srv.workstation)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "empty-ws-agent",
			ProjectID: project.ID,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		got, err := s.GetAgentBySlug(context.Background(), project.ID, "empty-ws-agent")
		require.NoError(t, err)
		wantResolved, err := srv.projectWorkspaceResolved(project)
		require.NoError(t, err)
		assert.Equal(t, wantResolved.HostPath, got.AppliedConfig.Workspace)

		// Verify the Hub-filled absolute path passes the L2 dispatch check.
		d := srv.CreateAuthenticatedDispatcher()
		require.NoError(t, d.hostedWorkspacePrecheck(context.Background(), got))
	})
}

// TestCreateAgent_WorkstationAllowsAbsoluteWorkspace verifies that workstation
// mode preserves caller-supplied absolute workspace paths.
func TestCreateAgent_WorkstationAllowsAbsoluteWorkspace(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.workstation = true

	absPath := filepath.Join(t.TempDir(), "local-ws")
	require.NoError(t, os.MkdirAll(absPath, 0o755))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "workstation-ws-agent",
		ProjectID: project.ID,
		Workspace: absPath,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got, err := s.GetAgentBySlug(context.Background(), project.ID, "workstation-ws-agent")
	require.NoError(t, err)
	assert.Equal(t, absPath, got.AppliedConfig.Workspace)
	require.NotNil(t, disp.capturedAgent, "dispatched")
}

// TestDispatch_HostedWorkspacePrecheck verifies L2 dispatch validation:
// pre-existing rows with a stored absolute workspace that does not match the
// project's resolved workspace are refused across every dispatch op with a
// typed workspace_not_permitted error and no broker call, while Hub-filled
// local and shared storage paths, relative paths, and workstation mode pass.
func TestDispatch_HostedWorkspacePrecheck(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	ctx := context.Background()

	newSetup := func(t *testing.T, hosted bool, ws string, psCfg *config.V1ProjectStorageConfig) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent, *Server, *store.Project) {
		t.Helper()
		cfg := testServerConfig()
		cfg.Workstation = !hosted
		cfg.ProjectStorageConfig = psCfg
		s := createTestStore(t)
		srv, _ := testServerWithStoreConfig(t, s, cfg)

		brokerID := tid("ws-precheck-broker")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "ws-precheck-broker", Slug: "ws-precheck-broker",
			Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		}))
		project := &store.Project{ID: tid("ws-precheck-project"), Name: "ws-precheck", Slug: "ws-precheck"}
		require.NoError(t, s.CreateProject(ctx, project))
		agent := &store.Agent{
			ID: tid("ws-precheck-agent"), Name: "ws-precheck-agent", Slug: "ws-precheck-agent",
			ProjectID: project.ID, RuntimeBrokerID: brokerID,
			AppliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: "claude",
				Workspace:     ws,
			},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		m := &mockRuntimeBrokerClient{}
		d := NewHTTPAgentDispatcherWithClient(s, m, false, slog.Default())
		d.SetHostedMode(hosted)
		d.SetHostedWorkspaceResolver(srv.projectWorkspaceResolved)
		return d, m, agent, srv, project
	}

	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" refuses stored foreign absolute workspace", func(t *testing.T) {
			d, m, a, _, _ := newSetup(t, true, "/var/tmp/foreign-dir", nil)
			err := op.run(d, a)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "refused with typed refusal: %v", err)
			assert.Equal(t, http.StatusBadRequest, refusal.Status)
			assert.Equal(t, api.LaunchCodeWorkspaceNotPermitted, refusal.Code)
			assert.Equal(t, "workspace", refusal.Details["field"])
			assert.False(t, op.called(m), "no broker call made on refusal")
		})
	}

	t.Run("local hub-managed project workspace matches and passes L2", func(t *testing.T) {
		d, _, a, srv, project := newSetup(t, true, "", nil)
		want, err := srv.projectWorkspaceResolved(project)
		require.NoError(t, err)
		a.AppliedConfig.Workspace = want.HostPath
		require.NoError(t, d.hostedWorkspacePrecheck(ctx, a))
	})

	t.Run("shared NFS project workspace matches and passes L2", func(t *testing.T) {
		mountRoot := t.TempDir()
		psCfg := &config.V1ProjectStorageConfig{
			Backend: config.WorkspaceStorageBackendNFS,
			NFS: &config.V1NFSConfig{
				MountRoot:   mountRoot,
				SubPathRoot: "projects",
				Shares: []config.V1NFSShare{
					{ID: "share1", Server: "10.0.0.1", Export: "/export"},
				},
			},
		}
		d, _, a, srv, project := newSetup(t, true, "", psCfg)
		want, err := srv.projectWorkspaceResolved(project)
		require.NoError(t, err)
		a.AppliedConfig.Workspace = want.HostPath
		require.NoError(t, d.hostedWorkspacePrecheck(ctx, a))

		// A foreign absolute path is still refused on the shared backend.
		a.AppliedConfig.Workspace = filepath.Join(t.TempDir(), "other")
		err = d.hostedWorkspacePrecheck(ctx, a)
		var refusal *RuntimeTargetRefusal
		require.True(t, errors.As(err, &refusal))
		assert.Equal(t, api.LaunchCodeWorkspaceNotPermitted, refusal.Code)
	})

	t.Run("relative workspace passes L2", func(t *testing.T) {
		d, _, a, _, _ := newSetup(t, true, "sub/dir", nil)
		require.NoError(t, d.hostedWorkspacePrecheck(ctx, a))
	})

	t.Run("workstation mode allows stored absolute workspace", func(t *testing.T) {
		d, _, a, _, _ := newSetup(t, false, "/var/tmp/workstation-dir", nil)
		require.NoError(t, d.hostedConfigPrecheck(ctx, a))
	})
}
