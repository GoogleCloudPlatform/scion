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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// lifecycleProfileRecordingDispatcher records the profile sent on create,
// provision, start, and restart dispatches, and optionally simulates a broker
// echoing a resolved profile back through store.ApplyBrokerAgentConfig.
type lifecycleProfileRecordingDispatcher struct {
	createAgentDispatcher
	dispatchedCreateProfile    string
	dispatchedProvisionProfile string
	echoProfile                string
	provisionCalled            bool
	startedProfile             string
	restartedProfile           string
	restartCalled              bool
}

func (d *lifecycleProfileRecordingDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if agent.AppliedConfig != nil {
		d.dispatchedCreateProfile = agent.AppliedConfig.Profile
	}
	res, err := d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
	if err != nil {
		return res, err
	}
	if d.echoProfile != "" {
		store.ApplyBrokerAgentConfig(agent, store.BrokerAgentConfig{Profile: d.echoProfile})
	}
	return res, nil
}

func (d *lifecycleProfileRecordingDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if agent.AppliedConfig != nil {
		d.dispatchedCreateProfile = agent.AppliedConfig.Profile
	}
	res, err := d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		return res, err
	}
	if d.echoProfile != "" {
		store.ApplyBrokerAgentConfig(agent, store.BrokerAgentConfig{Profile: d.echoProfile})
	}
	return res, nil
}

func (d *lifecycleProfileRecordingDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	d.provisionCalled = true
	d.capturedAgent = agent
	if agent.AppliedConfig != nil {
		d.dispatchedProvisionProfile = agent.AppliedConfig.Profile
	}
	if d.echoProfile != "" {
		store.ApplyBrokerAgentConfig(agent, store.BrokerAgentConfig{Profile: d.echoProfile})
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
//   - When the Hub project sets scion.io/active-profile, create pins it before
//     dispatch onto both AppliedConfig.Profile and CreateInputs.Profile.
//   - When neither the request nor the Hub project names a profile, create
//     dispatches an empty profile so the broker resolves its project-effective
//     active_profile (which may differ from broker.DefaultProfile), and the
//     broker's echoed profile is recorded onto both AppliedConfig.Profile and
//     CreateInputs.Profile via store.ApplyBrokerAgentConfig.
//   - A broker with a synthetic sole "default" profile is not pinned pre-dispatch.
//   - Later starts, restarts, and reincarnations keep the recorded create-time
//     profile even after the broker default and project active profile change.
func TestAgentCreate_RecordsResolvedProfileAcrossLifecycle(t *testing.T) {
	t.Parallel()

	localAndRemote := []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "remote", Type: "kubernetes", Available: true},
		{Name: "k8s", Type: "kubernetes", Available: true},
	}

	tests := []struct {
		name              string
		profiles          []store.BrokerProfile
		brokerDefault     string
		projectActive     string
		requestProfile    string
		brokerEchoProfile string
		wantDispatch      string
		wantRecorded      string
		laterBrokerDef    string
		laterProjectAct   string
		checkLifecycleOps bool
	}{
		{
			name:              "broker project-effective profile differs from global broker default",
			profiles:          localAndRemote,
			brokerDefault:     "local",
			projectActive:     "",
			brokerEchoProfile: "remote",
			wantDispatch:      "",
			wantRecorded:      "remote",
			laterBrokerDef:    "k8s",
			laterProjectAct:   "k8s",
			checkLifecycleOps: true,
		},
		{
			name:              "project active profile pinned pre-dispatch survives active profile change",
			profiles:          localAndRemote,
			brokerDefault:     "k8s",
			projectActive:     "local",
			wantDispatch:      "local",
			wantRecorded:      "local",
			laterBrokerDef:    "k8s",
			laterProjectAct:   "k8s",
			checkLifecycleOps: true,
		},
		{
			name:              "explicit request profile is not overwritten by project active profile",
			profiles:          localAndRemote,
			brokerDefault:     "local",
			projectActive:     "k8s",
			requestProfile:    "remote",
			wantDispatch:      "remote",
			wantRecorded:      "remote",
			laterBrokerDef:    "local",
			laterProjectAct:   "local",
			checkLifecycleOps: true,
		},
		{
			name:              "synthetic sole default profile is not pinned pre-dispatch",
			profiles:          []store.BrokerProfile{{Name: "default", Type: "docker", Available: true}},
			brokerDefault:     "",
			projectActive:     "",
			brokerEchoProfile: "",
			wantDispatch:      "",
			wantRecorded:      "",
			checkLifecycleOps: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			disp := &lifecycleProfileRecordingDispatcher{
				createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)},
				echoProfile:           tc.brokerEchoProfile,
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
				Profile:   tc.requestProfile,
				Task:      "record resolved profile",
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)

			assert.Equal(t, tc.wantDispatch, disp.dispatchedCreateProfile, "pre-dispatch create profile")

			stored, err := s.GetAgent(ctx, resp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.AppliedConfig)
			assert.Equal(t, tc.wantRecorded, stored.AppliedConfig.Profile, "persisted AppliedConfig.Profile")
			require.NotNil(t, stored.AppliedConfig.CreateInputs)
			assert.Equal(t, tc.wantRecorded, stored.AppliedConfig.CreateInputs.Profile, "persisted CreateInputs.Profile")

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
			assert.Equal(t, tc.wantRecorded, disp.startedProfile, "start dispatch must keep create-time profile")

			// Restart must keep the create-time profile.
			disp.startCalled = false
			disp.startedProfile = ""
			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stored.ID+"/restart", nil)
			require.Equal(t, http.StatusOK, rec.Code, "restart body: %s", rec.Body.String())
			if disp.restartCalled {
				assert.Equal(t, tc.wantRecorded, disp.restartedProfile, "restart dispatch must keep create-time profile")
			} else {
				require.True(t, disp.startCalled, "restart must dispatch via start or restart")
				assert.Equal(t, tc.wantRecorded, disp.startedProfile, "restart start-leg must keep create-time profile")
			}

			// Reincarnate must replay the create-time profile, not re-derive the new active/default profile.
			stored, err = s.GetAgent(ctx, stored.ID)
			require.NoError(t, err)
			proj, err := s.GetProject(ctx, project.ID)
			require.NoError(t, err)
			fresh, _, err := srv.buildFreshAppliedConfig(ctx, stored, proj, "")
			require.NoError(t, err)
			require.NotNil(t, fresh)
			assert.Equal(t, tc.wantRecorded, fresh.Profile, "reincarnate must keep create-time profile")
			require.NotNil(t, fresh.CreateInputs)
			assert.Equal(t, tc.wantRecorded, fresh.CreateInputs.Profile, "reincarnate CreateInputs must keep create-time profile")
		})
	}
}

// TestAgentCreate_ProvisionOnly_RecordsBrokerEchoedProfile verifies that a
// provision-only create without a Hub project active profile dispatches an
// empty profile, records the broker's echoed project-effective profile onto
// both AppliedConfig.Profile and CreateInputs.Profile, and uses it on a later
// start after the broker default and project active profile change.
func TestAgentCreate_ProvisionOnly_RecordsBrokerEchoedProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	disp := &lifecycleProfileRecordingDispatcher{
		createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseCreated)},
		echoProfile:           "remote",
	}
	srv, s, project := setupCreateAgentServer(t, disp)

	broker, err := s.GetRuntimeBroker(ctx, project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	broker.Profiles = []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "remote", Type: "kubernetes", Available: true},
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
	assert.Empty(t, disp.dispatchedProvisionProfile, "broker default must not be sent as explicit profile")

	stored, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.AppliedConfig)
	assert.Equal(t, "remote", stored.AppliedConfig.Profile)
	require.NotNil(t, stored.AppliedConfig.CreateInputs)
	assert.Equal(t, "remote", stored.AppliedConfig.CreateInputs.Profile)

	// Change broker default and project active profile before first start.
	broker.DefaultProfile = "k8s"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
	setProjectActiveProfile(t, s, project.ID, "k8s")

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+stored.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, "start body: %s", rec.Body.String())
	require.True(t, disp.startCalled)
	assert.Equal(t, "remote", disp.startedProfile, "first start after provision-only must keep broker-echoed profile")
}

// TestAgentCreateHeadless_RecordsResolvedProfile verifies that scheduled /
// headless creates record both:
//   - a Hub project active profile (pinned pre-dispatch), and
//   - a broker-echoed project-effective profile (differing from broker.DefaultProfile),
//
// onto AppliedConfig.Profile and CreateInputs.Profile so reincarnation keeps it.
func TestAgentCreateHeadless_RecordsResolvedProfile(t *testing.T) {
	t.Parallel()

	localAndRemote := []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "remote", Type: "kubernetes", Available: true},
		{Name: "k8s", Type: "kubernetes", Available: true},
	}

	tests := []struct {
		name          string
		brokerDefault string
		projectActive string
		echoProfile   string
		wantDispatch  string
		wantProfile   string
	}{
		{
			name:          "project active profile pinned pre-dispatch",
			brokerDefault: "k8s",
			projectActive: "local",
			wantDispatch:  "local",
			wantProfile:   "local",
		},
		{
			name:          "broker-echoed project-effective profile differs from broker default",
			brokerDefault: "local",
			projectActive: "",
			echoProfile:   "remote",
			wantDispatch:  "",
			wantProfile:   "remote",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := bypassAgentsSetup(t)
			disp := &lifecycleProfileRecordingDispatcher{
				createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)},
				echoProfile:           tc.echoProfile,
			}
			f.srv.SetDispatcher(disp)

			b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
			require.NoError(t, err)
			b.Profiles = localAndRemote
			b.DefaultProfile = tc.brokerDefault
			require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))

			if tc.projectActive != "" {
				setProjectActiveProfile(t, f.store, f.proj.ID, tc.projectActive)
			}

			const agentName = "sched-recorded-profile"
			require.NoError(t, fireScheduledDispatchAsOwner(t, f, agentName))

			assert.Equal(t, tc.wantDispatch, disp.dispatchedCreateProfile, "pre-dispatch headless profile")

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

// TestPinResolvedCreateProfile_SkipArms verifies every guard arm of
// pinResolvedCreateProfile directly: nil inputs, explicit profile not
// overwritten, managed flag, managed runtime, pinned agent, flat broker,
// and missing project active profile.
func TestPinResolvedCreateProfile_SkipArms(t *testing.T) {
	t.Parallel()

	projWithActive := &store.Project{
		Annotations: map[string]string{projectSettingActiveProfile: "project-active"},
	}
	legacyBroker := &store.RuntimeBroker{
		DefaultProfile: "broker-default",
		Profiles: []store.BrokerProfile{
			{Name: "broker-default", Type: "docker", Available: true},
		},
	}
	flatBroker := &store.RuntimeBroker{
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-1", Type: "docker"},
	}

	t.Run("nil agent or applied config is a no-op", func(t *testing.T) {
		pinResolvedCreateProfile(nil, projWithActive, legacyBroker, false)
		pinResolvedCreateProfile(&store.Agent{}, projWithActive, legacyBroker, false)
	})

	t.Run("explicit profile is not overwritten", func(t *testing.T) {
		a := &store.Agent{
			AppliedConfig: &store.AgentAppliedConfig{
				Profile:      "explicit",
				CreateInputs: &store.AgentCreateInputs{Profile: "explicit"},
			},
		}
		pinResolvedCreateProfile(a, projWithActive, legacyBroker, false)
		assert.Equal(t, "explicit", a.AppliedConfig.Profile)
		assert.Equal(t, "explicit", a.AppliedConfig.CreateInputs.Profile)
	})

	t.Run("managed create flag skips pinning", func(t *testing.T) {
		a := &store.Agent{
			AppliedConfig: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
		}
		pinResolvedCreateProfile(a, projWithActive, legacyBroker, true)
		assert.Empty(t, a.AppliedConfig.Profile)
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile)
	})

	t.Run("managed agent runtime skips pinning", func(t *testing.T) {
		a := &store.Agent{
			Runtime:       ManagedRuntimePrefix + "test",
			AppliedConfig: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
		}
		pinResolvedCreateProfile(a, projWithActive, legacyBroker, false)
		assert.Empty(t, a.AppliedConfig.Profile)
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile)
	})

	t.Run("pinned agent skips pinning", func(t *testing.T) {
		a := &store.Agent{
			PinnedRuntimeTargetID: "rt-1",
			AppliedConfig:         &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
		}
		pinResolvedCreateProfile(a, projWithActive, legacyBroker, false)
		assert.Empty(t, a.AppliedConfig.Profile)
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile)
	})

	t.Run("flat broker skips pinning", func(t *testing.T) {
		a := &store.Agent{
			AppliedConfig: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
		}
		pinResolvedCreateProfile(a, projWithActive, flatBroker, false)
		assert.Empty(t, a.AppliedConfig.Profile)
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile)
	})

	t.Run("no project active profile does not pin broker default", func(t *testing.T) {
		a := &store.Agent{
			AppliedConfig: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
		}
		pinResolvedCreateProfile(a, &store.Project{}, legacyBroker, false)
		assert.Empty(t, a.AppliedConfig.Profile)
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile)
	})

	t.Run("project active profile is pinned onto both fields", func(t *testing.T) {
		a := &store.Agent{
			AppliedConfig: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
		}
		pinResolvedCreateProfile(a, projWithActive, legacyBroker, false)
		assert.Equal(t, "project-active", a.AppliedConfig.Profile)
		assert.Equal(t, "project-active", a.AppliedConfig.CreateInputs.Profile)
	})
}
