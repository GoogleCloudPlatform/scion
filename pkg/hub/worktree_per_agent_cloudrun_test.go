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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestCheckWorktreePerAgentCloudRun(t *testing.T) {
	wtProject := &store.Project{
		ID:        "proj-wt",
		GitRemote: "github.com/acme/repo",
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent},
	}
	cloneProject := &store.Project{
		ID:        "proj-clone",
		GitRemote: "github.com/acme/repo",
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent},
	}
	unlabelledGitProject := &store.Project{
		ID:        "proj-unlabelled",
		GitRemote: "github.com/acme/repo",
	}
	sharedGitProject := &store.Project{
		ID:        "proj-shared",
		GitRemote: "github.com/acme/repo",
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
	}
	nonGitProject := &store.Project{
		ID: "proj-plain",
	}

	singleCRBroker := &store.RuntimeBroker{
		ID:             "b-cr",
		Name:           "cloudrun-usw2",
		DefaultProfile: "cloudrun",
		Profiles:       []store.BrokerProfile{{Name: "cloudrun", Type: "cloudrun", Available: true}},
	}
	singleCRInstancesBroker := &store.RuntimeBroker{
		ID:       "b-cri",
		Name:     "cloudrun-inst",
		Profiles: []store.BrokerProfile{{Name: "cri", Type: "cloudrun-instances", Available: true}},
	}
	multiBrokerDockerDefault := &store.RuntimeBroker{
		ID:             "b-multi",
		Name:           "hybrid-broker",
		DefaultProfile: "docker",
		Profiles: []store.BrokerProfile{
			{Name: "docker", Type: "docker", Available: true},
			{Name: "cloudrun", Type: "cloudrun", Available: true},
			{Name: "sandbox", Type: "cloudrun-sandbox", Available: true},
		},
	}
	multiBrokerNoDefault := &store.RuntimeBroker{
		ID:   "b-multi-nodef",
		Name: "hybrid-nodef",
		Profiles: []store.BrokerProfile{
			{Name: "docker", Type: "docker", Available: true},
			{Name: "cloudrun", Type: "cloudrun", Available: true},
		},
	}
	allCloudRunMultiBroker := &store.RuntimeBroker{
		ID:   "b-all-cr",
		Name: "all-cr-broker",
		Profiles: []store.BrokerProfile{
			{Name: "cr-usw1", Type: "cloudrun", Available: true},
			{Name: "cr-use1", Type: "cloudrun-instances", Available: true},
		},
	}
	flatCRBroker := &store.RuntimeBroker{
		ID:            "b-flat-cr",
		Name:          "flat-cloudrun",
		RuntimeTarget: &api.RuntimeTargetDescriptor{Type: "cloudrun"},
	}

	t.Run("refuses single-profile cloudrun and cloudrun-instances", func(t *testing.T) {
		for _, b := range []*store.RuntimeBroker{singleCRBroker, singleCRInstancesBroker} {
			err := checkWorktreePerAgentCloudRun(wtProject, b, "", nil)
			require.ErrorIs(t, err, errCloudRunLacksWorktreePerAgent)
			msg := err.Error()
			require.Contains(t, msg, b.Name)
			require.Contains(t, msg, `"Worktree per agent" (worktree-per-agent)`)
			require.Contains(t, msg, "Cloud Run")
			require.Contains(t, msg, "clone-per-agent")
			require.Contains(t, msg, "shared-plain")
		}
	})

	t.Run("multi-profile broker refuses cloudrun profile and allows docker or cloudrun-sandbox", func(t *testing.T) {
		err := checkWorktreePerAgentCloudRun(wtProject, multiBrokerDockerDefault, "cloudrun", nil)
		require.ErrorIs(t, err, errCloudRunLacksWorktreePerAgent)
		require.Contains(t, err.Error(), "hybrid-broker")
		require.Contains(t, err.Error(), "cloudrun")

		require.NoError(t, checkWorktreePerAgentCloudRun(wtProject, multiBrokerDockerDefault, "", nil))
		require.NoError(t, checkWorktreePerAgentCloudRun(wtProject, multiBrokerDockerDefault, "docker", nil))
		require.NoError(t, checkWorktreePerAgentCloudRun(wtProject, multiBrokerDockerDefault, "sandbox", nil))
		require.NoError(t, checkWorktreePerAgentCloudRun(wtProject, singleCRBroker, ManagedAgentsProfile, nil))
	})

	t.Run("project active-profile resolves to cloudrun", func(t *testing.T) {
		crActive := "cloudrun"
		projWithActiveCR := &store.Project{
			ID:        "proj-wt-active",
			GitRemote: "github.com/acme/repo",
			Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent},
			Annotations: map[string]string{
				projectSettingActiveProfile: crActive,
			},
		}
		err := checkWorktreePerAgentCloudRun(projWithActiveCR, multiBrokerDockerDefault, "", nil)
		require.ErrorIs(t, err, errCloudRunLacksWorktreePerAgent)

		// Explicit docker request profile overrides project active_profile.
		require.NoError(t, checkWorktreePerAgentCloudRun(projWithActiveCR, multiBrokerDockerDefault, "docker", nil))
	})

	t.Run("unresolvable profile defers on mixed broker, refuses when all profiles are cloudrun", func(t *testing.T) {
		require.NoError(t, checkWorktreePerAgentCloudRun(wtProject, multiBrokerNoDefault, "", nil))
		require.ErrorIs(t, checkWorktreePerAgentCloudRun(wtProject, allCloudRunMultiBroker, "", nil), errCloudRunLacksWorktreePerAgent)
	})

	t.Run("refuses flat cloudrun broker and cloudrun-pinned agent", func(t *testing.T) {
		require.ErrorIs(t, checkWorktreePerAgentCloudRun(wtProject, flatCRBroker, "", nil), errCloudRunLacksWorktreePerAgent)
		require.ErrorIs(t, checkWorktreePerAgentCloudRun(wtProject, &store.RuntimeBroker{ID: "b-no-prof", Name: "skewed"}, "", &store.Agent{
			Runtime: "cloudrun",
		}), errCloudRunLacksWorktreePerAgent)
		require.ErrorIs(t, checkWorktreePerAgentCloudRun(wtProject, &store.RuntimeBroker{ID: "b-no-prof", Name: "skewed"}, "", &store.Agent{
			PinnedRuntimeTargetType: "cloudrun",
		}), errCloudRunLacksWorktreePerAgent)
	})

	t.Run("regression: clone-per-agent and shared-plain pass on Cloud Run", func(t *testing.T) {
		for _, p := range []*store.Project{nil, cloneProject, unlabelledGitProject, sharedGitProject, nonGitProject} {
			for _, b := range []*store.RuntimeBroker{singleCRBroker, singleCRInstancesBroker, flatCRBroker} {
				require.NoError(t, checkWorktreePerAgentCloudRun(p, b, "", nil))
			}
		}
	})
}

func newCloudRunWorktreeHandlerSetup(t *testing.T, suffix, label string, profiles []store.BrokerProfile, defaultProfile string) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)
	mountRoot := t.TempDir()
	srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares:    []config.V1NFSShare{{ID: "main", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	broker := &store.RuntimeBroker{
		ID:             tid("broker-wtcr-" + suffix),
		Name:           "cloudrun-broker-" + suffix,
		Slug:           "cloudrun-broker-" + suffix,
		Endpoint:       "http://localhost:9802",
		Status:         store.BrokerStatusOnline,
		DefaultProfile: defaultProfile,
		Profiles:       profiles,
		WorkspaceStorage: &api.BrokerWorkspaceStorage{
			Backend: "nfs",
			Homes:   "nfs",
			NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/scion", SubPathRoot: "projects", Healthy: true},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	var labels map[string]string
	if label != "" {
		labels = map[string]string{store.LabelWorkspaceMode: label}
	}
	project := &store.Project{
		ID:                     tid("proj-wtcr-" + suffix),
		Name:                   "wtcr-" + suffix,
		Slug:                   "wtcr-" + suffix,
		GitRemote:              "github.com/acme/repo",
		Labels:                 labels,
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))
	return srv, s, project, broker
}

func TestWorktreePerAgent_CloudRun_CreateRefused412AndSupportedModesPass(t *testing.T) {
	crProfiles := []store.BrokerProfile{{Name: "cloudrun", Type: "cloudrun", Available: true}}

	t.Run("worktree-per-agent refused 412 before persistence or broker call", func(t *testing.T) {
		srv, s, project, broker := newCloudRunWorktreeHandlerSetup(t, "create-412", store.WorkspaceModeWorktreePerAgent, crProfiles, "cloudrun")
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "wt-agent",
			ProjectID: project.ID,
		})
		require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), ErrCodeUnsupportedCapability)
		msg := decodeErrorMessage(t, rec.Body.Bytes())
		require.Contains(t, msg, broker.Name)
		require.Contains(t, msg, `"Worktree per agent" (worktree-per-agent)`)
		require.Contains(t, msg, "clone-per-agent")
		require.Contains(t, msg, "shared-plain")
		require.False(t, client.createCalled, "broker must not be called")

		agents, err := s.ListAgents(context.Background(), store.AgentFilter{ProjectID: project.ID}, store.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, agents.Items, "no agent row may be persisted when refused")
	})

	t.Run("multi-profile broker refuses cloudrun profile and allows docker profile", func(t *testing.T) {
		hybridProfiles := []store.BrokerProfile{
			{Name: "docker", Type: "docker", Available: true},
			{Name: "cloudrun", Type: "cloudrun", Available: true},
		}
		srv, s, project, _ := newCloudRunWorktreeHandlerSetup(t, "hybrid", store.WorkspaceModeWorktreePerAgent, hybridProfiles, "docker")
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

		recCR := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "wt-cr-prof",
			ProjectID: project.ID,
			Profile:   "cloudrun",
		})
		require.Equal(t, http.StatusPreconditionFailed, recCR.Code, recCR.Body.String())
		require.False(t, client.createCalled)

		recDocker := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "wt-docker-prof",
			ProjectID: project.ID,
			Profile:   "docker",
		})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, recDocker.Code, recDocker.Body.String())
		require.True(t, client.createCalled)
	})

	t.Run("regression: clone-per-agent and shared modes succeed on Cloud Run", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			label string
		}{
			{"clone-per-agent-explicit", store.WorkspaceModePerAgent},
			{"clone-per-agent-default", ""},
			{"shared-plain", store.WorkspaceModeShared},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, s, project, _ := newCloudRunWorktreeHandlerSetup(t, tc.name, tc.label, crProfiles, "cloudrun")
				client := &mockRuntimeBrokerClient{}
				srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
					Name:      "ok-agent",
					ProjectID: project.ID,
				})
				require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
				require.True(t, client.createCalled)
			})
		}
	})
}

func TestWorktreePerAgent_CloudRun_LifecycleAndDispatcherGates(t *testing.T) {
	crProfiles := []store.BrokerProfile{{Name: "cloudrun", Type: "cloudrun", Available: true}}

	t.Run("dispatcher fails closed on all provisioning ops, allows stop and delete", func(t *testing.T) {
		_, s, project, broker := newCloudRunWorktreeHandlerSetup(t, "disp", store.WorkspaceModeWorktreePerAgent, crProfiles, "cloudrun")
		client := &mockRuntimeBrokerClient{}
		d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
		a := &store.Agent{
			ID:              tid("agent-wtcr-disp"),
			Name:            "wtcr-disp",
			Slug:            "wtcr-disp",
			ProjectID:       project.ID,
			RuntimeBrokerID: broker.ID,
			AppliedConfig:   &store.AgentAppliedConfig{},
		}

		for _, op := range []struct {
			name string
			run  func() error
		}{
			{"create", func() error { _, err := d.DispatchAgentCreate(context.Background(), a); return err }},
			{"create-with-gather", func() error { _, err := d.DispatchAgentCreateWithGather(context.Background(), a); return err }},
			{"provision", func() error { return d.DispatchAgentProvision(context.Background(), a) }},
			{"finalize-env", func() error {
				_, err := d.DispatchFinalizeEnv(context.Background(), a, map[string]string{"K": "v"})
				return err
			}},
			{"start", func() error { return d.DispatchAgentStart(context.Background(), a, "", false) }},
			{"restart", func() error { return d.DispatchAgentRestart(context.Background(), a) }},
		} {
			err := op.run()
			require.ErrorIs(t, err, errCloudRunLacksWorktreePerAgent, op.name)
			require.False(t, client.createCalled || client.startCalled || client.restartCalled, op.name)
		}

		require.NoError(t, d.DispatchAgentStop(context.Background(), a))
		require.NoError(t, d.DispatchAgentDelete(context.Background(), a, false, false, false, time.Time{}))
	})

	t.Run("start returns 412 and keeps stopped phase", func(t *testing.T) {
		srv, s, project, broker := newCloudRunWorktreeHandlerSetup(t, "start", store.WorkspaceModeWorktreePerAgent, crProfiles, "cloudrun")
		grantDevUserRuntimeBrokerAccess(t, s)
		agent := newQuotaTestAgent(t, s, broker, project, "wtcr-start", state.PhaseStopped)
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), ErrCodeUnsupportedCapability)
		require.False(t, client.startCalled || client.createCalled)

		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		require.Equal(t, string(state.PhaseStopped), got.Phase)
	})

	t.Run("restart returns 412 before stop leg and keeps running phase", func(t *testing.T) {
		srv, s, project, broker := newCloudRunWorktreeHandlerSetup(t, "restart", store.WorkspaceModeWorktreePerAgent, crProfiles, "cloudrun")
		grantDevUserRuntimeBrokerAccess(t, s)
		agent := newQuotaTestAgent(t, s, broker, project, "wtcr-restart", state.PhaseRunning)
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), ErrCodeUnsupportedCapability)
		require.False(t, client.stopCalled, "restart must refuse before stopping running agent")
		require.False(t, client.startCalled || client.restartCalled)

		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		require.Equal(t, string(state.PhaseRunning), got.Phase)
	})

	t.Run("wakeAgentForDM returns 412 unsupported_capability and keeps suspended phase", func(t *testing.T) {
		srv, s, project, broker := newCloudRunWorktreeHandlerSetup(t, "wake", store.WorkspaceModeWorktreePerAgent, crProfiles, "cloudrun")
		agent := newQuotaTestAgent(t, s, broker, project, "wtcr-wake", state.PhaseSuspended)
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

		_, dmErr := srv.wakeAgentForDM(context.Background(), agent)
		require.NotNil(t, dmErr)
		require.Equal(t, http.StatusPreconditionFailed, dmErr.HTTPStatus, dmErr.Message)
		require.Equal(t, ErrCodeUnsupportedCapability, dmErr.Code)
		require.False(t, client.startCalled)

		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		require.Equal(t, string(state.PhaseSuspended), got.Phase)
	})

	t.Run("unprofiled broker 412 unsupported_capability refusal is relayed cleanly", func(t *testing.T) {
		srv, s, project, broker := newCloudRunWorktreeHandlerSetup(t, "relay", store.WorkspaceModeWorktreePerAgent, nil, "")
		grantDevUserRuntimeBrokerAccess(t, s)

		bodyBytes, err := json.Marshal(map[string]any{
			"error": map[string]any{
				"code":    api.BrokerErrCodeUnsupportedCapability,
				"message": `agent "wtcr-relay": "Worktree per agent" (worktree-per-agent) workspaces are not supported on the Cloud Run runtime: Cloud Run supports "Clone per agent" (clone-per-agent) and "Shared workspace" (shared-plain) modes`,
			},
		})
		require.NoError(t, err)
		broker412 := &brokerStatusError{StatusCode: http.StatusPreconditionFailed, Body: string(bodyBytes)}

		client := &mockRuntimeBrokerClient{
			returnErr: broker412,
		}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

		recCreate := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "wtcr-relay",
			ProjectID: project.ID,
		})
		require.Equal(t, http.StatusPreconditionFailed, recCreate.Code, recCreate.Body.String())
		require.Contains(t, recCreate.Body.String(), ErrCodeUnsupportedCapability)
		require.Contains(t, recCreate.Body.String(), "clone-per-agent")

		agent := newQuotaTestAgent(t, s, broker, project, "wtcr-relay-start", state.PhaseStopped)
		recStart := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusPreconditionFailed, recStart.Code, recStart.Body.String())
		require.Contains(t, recStart.Body.String(), ErrCodeUnsupportedCapability)

		suspended := newQuotaTestAgent(t, s, broker, project, "wtcr-relay-wake", state.PhaseSuspended)
		_, dmErr := srv.wakeAgentForDM(context.Background(), suspended)
		require.NotNil(t, dmErr)
		require.Equal(t, http.StatusPreconditionFailed, dmErr.HTTPStatus)
		require.Equal(t, ErrCodeUnsupportedCapability, dmErr.Code)
	})
}

func TestWorktreePerAgent_StartClaimClassifiesAsDidNotHappen(t *testing.T) {
	err := &cloudRunLacksWorktreePerAgentError{broker: "cloudrun-usw2"}
	require.True(t, errors.Is(err, errCloudRunLacksWorktreePerAgent))
	require.Equal(t, startReleased, startOutcomeOf(err))
	require.True(t, startDidNotHappen(err))
}
