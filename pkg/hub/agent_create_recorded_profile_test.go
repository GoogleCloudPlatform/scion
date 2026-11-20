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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// lifecycleProfileRecordingDispatcher records the profile sent on create,
// provision, start, and restart dispatches.
type lifecycleProfileRecordingDispatcher struct {
	createAgentDispatcher
	provisionedProfile string
	provisionCalled    bool
	startedProfile     string
	restartedProfile   string
	restartCalled      bool
}

func (d *lifecycleProfileRecordingDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	d.provisionCalled = true
	d.capturedAgent = agent
	if agent.AppliedConfig != nil {
		d.provisionedProfile = agent.AppliedConfig.Profile
	}
	agent.Phase = string(state.PhaseCreated)
	return nil
}

func (d *lifecycleProfileRecordingDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCalled = true
	if agent.AppliedConfig != nil {
		d.startedProfile = agent.AppliedConfig.Profile
	}
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *lifecycleProfileRecordingDispatcher) DispatchAgentRestart(_ context.Context, agent *store.Agent) error {
	d.restartCalled = true
	if agent.AppliedConfig != nil {
		d.restartedProfile = agent.AppliedConfig.Profile
	}
	agent.Phase = string(state.PhaseRunning)
	return nil
}

// TestAgentCreate_RecordsResolvedProfileAcrossLifecycle verifies ptone/scion#5149:
// an agent created without an explicit profile records the resolved runtime
// profile (from the project's active profile or the broker's default/sole
// profile) on both AppliedConfig.Profile and CreateInputs.Profile, so later
// starts, restarts and reincarnations keep that profile even after the
// project's active profile or the broker's default profile changes.
func TestAgentCreate_RecordsResolvedProfileAcrossLifecycle(t *testing.T) {
	t.Parallel()

	localAndK8s := []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "k8s", Type: "kubernetes", Available: true},
	}

	tests := []struct {
		name              string
		profiles          []store.BrokerProfile
		brokerDefault     string
		projectActive     string
		laterBrokerDef    string
		laterProjectAct   string
		wantProfile       string
		checkLifecycleOps bool
	}{
		{
			name:              "broker default profile survives default and project active change",
			profiles:          localAndK8s,
			brokerDefault:     "local",
			projectActive:     "",
			laterBrokerDef:    "k8s",
			laterProjectAct:   "k8s",
			wantProfile:       "local",
			checkLifecycleOps: true,
		},
		{
			name:              "project active profile survives active profile change",
			profiles:          localAndK8s,
			brokerDefault:     "k8s",
			projectActive:     "local",
			laterBrokerDef:    "k8s",
			laterProjectAct:   "k8s",
			wantProfile:       "local",
			checkLifecycleOps: true,
		},
		{
			name:              "single broker profile without DefaultProfile is recorded",
			profiles:          []store.BrokerProfile{{Name: "only", Type: "docker", Available: true}},
			brokerDefault:     "",
			projectActive:     "",
			laterBrokerDef:    "k8s",
			laterProjectAct:   "k8s",
			wantProfile:       "only",
			checkLifecycleOps: true,
		},
		{
			name:              "ambiguous multi-profile broker without DefaultProfile stays empty",
			profiles:          localAndK8s,
			brokerDefault:     "",
			projectActive:     "",
			wantProfile:       "",
			checkLifecycleOps: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			disp := &lifecycleProfileRecordingDispatcher{
				createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)},
			}
			srv, s, project := setupCreateAgentServer(t, disp)

			broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
			require.NoError(t, err)
			broker.Profiles = tc.profiles
			broker.DefaultProfile = tc.brokerDefault
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

			if tc.projectActive != "" {
				setProjectActiveProfile(t, s, project.ID, tc.projectActive)
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      "recorded-profile-agent",
				ProjectID: project.ID,
				Task:      "record resolved profile",
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)

			require.NotNil(t, disp.capturedAgent)
			require.NotNil(t, disp.capturedAgent.AppliedConfig)
			assert.Equal(t, tc.wantProfile, disp.capturedAgent.AppliedConfig.Profile, "create dispatch profile")

			stored, err := s.GetAgent(ctx, resp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.AppliedConfig)
			assert.Equal(t, tc.wantProfile, stored.AppliedConfig.Profile, "persisted AppliedConfig.Profile")
			require.NotNil(t, stored.AppliedConfig.CreateInputs)
			assert.Equal(t, tc.wantProfile, stored.AppliedConfig.CreateInputs.Profile, "persisted CreateInputs.Profile")

			if !tc.checkLifecycleOps {
				return
			}

			// Mutate both the broker default and the project's active profile.
			broker, err = s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
			require.NoError(t, err)
			broker.DefaultProfile = tc.laterBrokerDef
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			setProjectActiveProfile(t, s, project.ID, tc.laterProjectAct)

			// Start after stop must keep the create-time profile.
			stored.Phase = string(state.PhaseStopped)
			require.NoError(t, s.UpdateAgent(ctx, stored))
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stored.ID+"/start", nil)
			require.Equal(t, http.StatusOK, rec.Code, "start body: %s", rec.Body.String())
			require.True(t, disp.startCalled)
			assert.Equal(t, tc.wantProfile, disp.startedProfile, "start dispatch must keep create-time profile")

			// Restart must keep the create-time profile.
			disp.startCalled = false
			disp.startedProfile = ""
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stored.ID+"/restart", nil)
			require.Equal(t, http.StatusOK, rec.Code, "restart body: %s", rec.Body.String())
			if disp.restartCalled {
				assert.Equal(t, tc.wantProfile, disp.restartedProfile, "restart dispatch must keep create-time profile")
			} else {
				require.True(t, disp.startCalled, "restart must dispatch via start or restart")
				assert.Equal(t, tc.wantProfile, disp.startedProfile, "restart start-leg must keep create-time profile")
			}

			// Reincarnate must replay the create-time profile, not re-derive the new active/default profile.
			stored, err = s.GetAgent(ctx, stored.ID)
			require.NoError(t, err)
			proj, err := s.GetProject(ctx, project.ID)
			require.NoError(t, err)
			fresh, _, err := srv.buildFreshAppliedConfig(ctx, stored, proj, "")
			require.NoError(t, err)
			require.NotNil(t, fresh)
			assert.Equal(t, tc.wantProfile, fresh.Profile, "reincarnate must keep create-time profile")
			require.NotNil(t, fresh.CreateInputs)
			assert.Equal(t, tc.wantProfile, fresh.CreateInputs.Profile, "reincarnate CreateInputs must keep create-time profile")
		})
	}
}

// TestAgentCreate_ProvisionOnly_RecordsResolvedProfile verifies that a
// provision-only create records the resolved broker default profile so a
// subsequent start after the default/active profile changes still uses it.
func TestAgentCreate_ProvisionOnly_RecordsResolvedProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	disp := &lifecycleProfileRecordingDispatcher{
		createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseCreated)},
	}
	srv, s, project := setupCreateAgentServer(t, disp)

	broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	broker.Profiles = []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "k8s", Type: "kubernetes", Available: true},
	}
	broker.DefaultProfile = "local"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "provision-only-profile-agent",
		ProjectID:     project.ID,
		ProvisionOnly: true,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.True(t, disp.provisionCalled)
	assert.Equal(t, "local", disp.provisionedProfile)

	stored, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.AppliedConfig)
	assert.Equal(t, "local", stored.AppliedConfig.Profile)
	require.NotNil(t, stored.AppliedConfig.CreateInputs)
	assert.Equal(t, "local", stored.AppliedConfig.CreateInputs.Profile)

	// Change broker default and project active profile before first start.
	broker.DefaultProfile = "k8s"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
	setProjectActiveProfile(t, s, project.ID, "k8s")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stored.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, "start body: %s", rec.Body.String())
	require.True(t, disp.startCalled)
	assert.Equal(t, "local", disp.startedProfile, "first start after provision-only must keep create-time profile")
}

// TestAgentCreateHeadless_RecordsResolvedProfile verifies that scheduled /
// headless creates record the resolved runtime profile onto both
// AppliedConfig.Profile and CreateInputs.Profile so reincarnation keeps it.
func TestAgentCreateHeadless_RecordsResolvedProfile(t *testing.T) {
	t.Parallel()

	localAndK8s := []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "k8s", Type: "kubernetes", Available: true},
	}

	tests := []struct {
		name          string
		brokerDefault string
		projectActive string
		wantProfile   string
	}{
		{
			name:          "broker default profile",
			brokerDefault: "local",
			projectActive: "",
			wantProfile:   "local",
		},
		{
			name:          "project active profile",
			brokerDefault: "k8s",
			projectActive: "local",
			wantProfile:   "local",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := bypassAgentsSetup(t)

			b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
			require.NoError(t, err)
			b.Profiles = localAndK8s
			b.DefaultProfile = tc.brokerDefault
			require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))

			if tc.projectActive != "" {
				setProjectActiveProfile(t, f.store, f.proj.ID, tc.projectActive)
			}

			const agentName = "sched-recorded-profile"
			require.NoError(t, fireScheduledDispatchAsOwner(t, f, agentName))

			got, err := f.store.GetAgentBySlug(ctx, f.proj.ID, agentName)
			require.NoError(t, err)
			require.NotNil(t, got.AppliedConfig)
			assert.Equal(t, tc.wantProfile, got.AppliedConfig.Profile)
			require.NotNil(t, got.AppliedConfig.CreateInputs)
			assert.Equal(t, tc.wantProfile, got.AppliedConfig.CreateInputs.Profile)

			// Change both defaults and verify reincarnate keeps the create-time profile.
			b.DefaultProfile = "k8s"
			require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
			setProjectActiveProfile(t, f.store, f.proj.ID, "k8s")

			proj, err := f.store.GetProject(ctx, f.proj.ID)
			require.NoError(t, err)
			fresh, _, err := f.srv.buildFreshAppliedConfig(ctx, got, proj, "")
			require.NoError(t, err)
			require.NotNil(t, fresh)
			assert.Equal(t, tc.wantProfile, fresh.Profile)
			require.NotNil(t, fresh.CreateInputs)
			assert.Equal(t, tc.wantProfile, fresh.CreateInputs.Profile)
		})
	}
}
