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

// Agent status and launch-record writes (ptone/scion#5625): the heartbeat's
// reincarnation guard at the store, the launch error a synchronous start
// replaces, and the resolved config of a create kept after an unknown
// dispatch outcome.
package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHeartbeat_ReincarnationStartedAfterRead covers the window between the
// heartbeat handler's read of the agent and its status write
// (ptone/scion#5385): a reincarnation claimed in between still keeps the
// heartbeat from storing the old run's phase and exit fields, because the
// handler marks the write GuardReincarnation and the store re-checks the
// guard on the row it locks. The container status still applies.
func TestHeartbeat_ReincarnationStartedAfterRead(t *testing.T) {
	srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *reincarnateBeforeStatusStore {
		return &reincarnateBeforeStatusStore{Store: inner, fault: f, t: t}
	})
	grantDevUserRuntimeBrokerAccess(t, s)
	broker, project := newQuotaTestBrokerAndProject(t, s, "hb-reinc-race")
	agent := newQuotaTestAgent(t, s, broker, project, "hb-reinc-race-agent", state.PhaseRunning)
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{Activity: string(state.ActivityWorking)}))
	fault.Arm()

	ec := 1
	code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
		Slug:            agent.Slug,
		Phase:           "stopped",
		Activity:        "crashed",
		ContainerStatus: "Exited (1)",
		Message:         "boom",
		ExitCode:        &ec,
		ExitReason:      "crashed",
	})
	require.Equal(t, http.StatusOK, code)
	require.True(t, wrapped.done, "the store wrapper must have run")

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateStopping, after.ReincarnationState)
	assert.Equal(t, string(state.PhaseRunning), after.Phase, "a heartbeat read before the reincarnation must not change the phase")
	assert.Equal(t, string(state.ActivityWorking), after.Activity, "a heartbeat read before the reincarnation must not change the activity")
	assert.Nil(t, after.ExitCode, "a heartbeat read before the reincarnation must not store an exit code")
	assert.Empty(t, after.ExitReason, "a heartbeat read before the reincarnation must not store an exit reason")
	assert.Empty(t, after.Message, "a heartbeat read before the reincarnation must not store a message")
	assert.Equal(t, "Exited (1)", after.ContainerStatus, "the unguarded field must be written")
}

// TestSyncStart_ReplacesFailedAsyncStartLaunchError covers a failed
// asynchronous start followed by a failed synchronous start
// (ptone/scion#5273): the synchronous start clears the launch error and
// cause the asynchronous launch left, so the agent shows only the new
// failure.
func TestSyncStart_ReplacesFailedAsyncStartLaunchError(t *testing.T) {
	startErr := fakeBrokerStartErr(t, http.StatusInternalServerError, brokerErrorBody("runtime_error", "the second start failed"))
	disp := &skillFailDispatcher{startErr: startErr}
	srv, s, project := setupCreateAgentServer(t, disp)
	agent := createLifecycleTestAgent(t, s, project, "sync-after-async-fail", state.PhaseStopped)
	// The failed asynchronous start: a start launch from stopped, ended by
	// the broker's failed report.
	launchID := beginTestStartLaunch(t, s, agent, store.LaunchKindStart, string(state.PhaseStopped), false)
	answer, _, err := s.ApplyLaunchReport(context.Background(), agent.ID, agent.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "boom",
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)
	before := getAgent(t, s, agent.ID)
	require.Equal(t, "boom", before.LaunchError, "setup: the failed asynchronous start left its launch error")
	require.Equal(t, store.LaunchStateEnded, before.LaunchState, "setup: the asynchronous launch ended")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "the second start failed")

	got := getAgent(t, s, agent.ID)
	assert.Empty(t, got.LaunchError, "the synchronous start must clear the old launch error")
	assert.Empty(t, got.LaunchCause, "the synchronous start must clear the old launch cause")
	if view := store.ComputeAgentLaunch(got, time.Now()); view != nil {
		assert.Empty(t, view.Error, "the launch view must not show the old launch error")
	}
}

// TestSyncStart_KeepsUnconfirmedLaunchOutcome covers an ended start launch
// whose outcome the hub declared without the broker (broker_lost,
// launch_timeout), followed by two failed synchronous starts
// (ptone/scion#5273): the start claim keeps that outcome, so the second
// start still treats the agent's runtime state as unknown, exactly as the
// first did, and the agent's existing token record is unchanged.
func TestSyncStart_KeepsUnconfirmedLaunchOutcome(t *testing.T) {
	for _, code := range []string{store.LaunchErrorBrokerLost, store.LaunchErrorLaunchTimeout} {
		t.Run(code, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "unconf-"+code, state.PhaseStopped)
			launchID := beginTestStartLaunch(t, s, agent, store.LaunchKindStart, string(state.PhaseStopped), false)
			require.NoError(t, s.EndLaunchWithCode(ctx, agent.ID, launchID, store.LaunchEndReasonLost, code))
			require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseError)}))
			insertTestAgentCredential(t, s, agent.ID, agent.ProjectID, "unconf-prior-jti-"+code)

			client := &mockRuntimeBrokerClient{returnErr: &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"conflict","message":"container name in use"}}`}}
			disp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
			gen := &fakeMintingTokenGenerator{store: s}
			disp.SetTokenGenerator(gen)
			srv.SetDispatcher(disp)

			for i := 1; i <= 2; i++ {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
				require.NotEqual(t, http.StatusOK, rec.Code, "start %d: %s", i, rec.Body.String())
				got := getAgent(t, s, agent.ID)
				assert.Equal(t, code, got.LaunchError, "start %d: the start claim must keep the unconfirmed launch outcome", i)
			}
			require.Equal(t, 2, client.startCallCount, "setup: both starts reached the broker")
			assert.Nil(t, getTestAgentCredential(t, s, "unconf-prior-jti-"+code).RevokedAt,
				"the agent's existing token record must be unchanged after the second start")
		})
	}
}

// keptCreateHeartbeatAgent stores an agent in phase starting with an empty
// applied config and run run-1, as a create looks when the hub kept it after
// an unknown dispatch outcome; claimKind and claimState say which claim it
// holds (state "" for none), and runtime is its stored runtime. Each opt
// changes the agent before it is stored.
func keptCreateHeartbeatAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, claimKind store.StartClaimKind, claimState store.StartClaimState, runtime string, opts ...func(*store.Agent)) *store.Agent {
	t.Helper()
	ctx := context.Background()
	agent := &store.Agent{
		ID: tid("agent-" + name), Slug: name, Name: name,
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase:         string(state.PhaseStarting),
		Runtime:       runtime,
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	for _, opt := range opts {
		opt(agent)
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-1", nil)
	require.NoError(t, err)
	if claimState != "" {
		c, err := s.ClaimAgentStart(ctx, agent.ID, "hub-x", claimKind, "", time.Hour)
		require.NoError(t, err)
		if claimState == store.StartClaimUnconfirmed {
			held, err := s.MarkStartUnconfirmed(ctx, agent.ID, c.ID, "hub-x", time.Hour)
			require.NoError(t, err)
			require.True(t, held)
		}
	}
	return agent
}

// TestHeartbeat_KeptCreateGetsResolvedConfig covers a create kept after an
// unknown dispatch outcome (ptone/scion#4987): the first heartbeat entry of
// the create's own run records the template, harness config and image (and
// runtime) the runtime resolved, as a successful create's answer would. Both
// gates are required: an entry with another or no run ID, or an agent with no
// unconfirmed create claim (an unconfirmed claim of another kind, a live
// claim, or none), records nothing. An empty runtime in the entry never
// clears the stored one.
func TestHeartbeat_KeptCreateGetsResolvedConfig(t *testing.T) {
	cases := []struct {
		name         string
		claimKind    store.StartClaimKind
		claimState   store.StartClaimState
		runID        string
		hbRuntime    string
		agentRuntime string
		wantApply    bool
	}{
		{"kept create, own run", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-1", "kubernetes", "", true},
		{"kept create, own run, empty runtime keeps stored", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-1", "", "docker", true},
		{"kept create, other run", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-other", "kubernetes", "", false},
		{"kept create, no run ID", store.StartClaimCreate, store.StartClaimUnconfirmed, "", "kubernetes", "", false},
		{"kept start claim, own run", store.StartClaimUser, store.StartClaimUnconfirmed, "run-1", "kubernetes", "", false},
		{"live create claim", store.StartClaimCreate, store.StartClaimLive, "run-1", "kubernetes", "", false},
		{"no claim", store.StartClaimCreate, "", "run-1", "kubernetes", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, s := testServer(t)
			grantDevUserRuntimeBrokerAccess(t, s)
			broker, project := newQuotaTestBrokerAndProject(t, s, "kept-cfg")
			agent := keptCreateHeartbeatAgent(t, s, broker, project, "kept-cfg-agent", c.claimKind, c.claimState, c.agentRuntime)

			code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
				Slug:                agent.Slug,
				Phase:               "running",
				Activity:            "working",
				ContainerStatus:     "Up 2 seconds",
				RunID:               c.runID,
				Template:            "resolved-template",
				HarnessConfig:       "resolved-harness-config",
				HarnessConfigSource: "hub-hydrated",
				Image:               "registry.example/resolved:v1",
				Runtime:             c.hbRuntime,
			})
			require.Equal(t, http.StatusOK, code)

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.NotNil(t, got.AppliedConfig)
			if c.wantApply {
				assert.Equal(t, "resolved-template", got.Template, "the kept create must record the resolved template")
				assert.Equal(t, "resolved-harness-config", got.AppliedConfig.HarnessConfig, "the kept create must record the resolved harness config")
				assert.Equal(t, "hub-hydrated", got.AppliedConfig.HarnessConfigSource, "the kept create must record the harness config source")
				assert.Equal(t, "registry.example/resolved:v1", got.AppliedConfig.Image, "the kept create must record the resolved image")
				if c.hbRuntime != "" {
					assert.Equal(t, c.hbRuntime, got.Runtime, "the kept create must record the resolved runtime")
				} else {
					assert.Equal(t, c.agentRuntime, got.Runtime, "an empty runtime in the heartbeat must leave the stored runtime")
				}
			} else {
				assert.Empty(t, got.Template, "this heartbeat must not record a template")
				assert.Empty(t, got.AppliedConfig.HarnessConfig, "this heartbeat must not record a harness config")
				assert.Empty(t, got.AppliedConfig.Image, "this heartbeat must not record an image")
				assert.Equal(t, c.agentRuntime, got.Runtime, "this heartbeat must not record a runtime")
			}
		})
	}
}

// TestHeartbeat_KeptCreateRecordsHarnessAuthAndProfile pins which of the
// harness auth and profile a kept create's heartbeat records
// (ptone/scion#6232). The agent starts with both already stored, so only the
// kept-create path can change them: with every gate met (an unconfirmed
// create claim and the entry's own run ID), the reported values replace the
// stored ones; an empty reported value keeps the stored one; with any gate
// unmet, neither changes.
func TestHeartbeat_KeptCreateRecordsHarnessAuthAndProfile(t *testing.T) {
	const (
		storedAuth      = "stored-auth"
		storedProfile   = "stored-profile"
		reportedAuth    = "reported-auth"
		reportedProfile = "reported-profile"
	)
	cases := []struct {
		name        string
		claimKind   store.StartClaimKind
		claimState  store.StartClaimState
		runID       string
		hbAuth      string
		hbProfile   string
		pinned      bool
		wantAuth    string
		wantProfile string
	}{
		{"kept create, own run, reported values replace stored", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-1", reportedAuth, reportedProfile, false, reportedAuth, reportedProfile},
		{"kept create, own run, empty values keep stored", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-1", "", "", false, storedAuth, storedProfile},
		{"kept create, other run", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-other", reportedAuth, reportedProfile, false, storedAuth, storedProfile},
		{"kept create, no run ID", store.StartClaimCreate, store.StartClaimUnconfirmed, "", reportedAuth, reportedProfile, false, storedAuth, storedProfile},
		{"kept start claim, own run", store.StartClaimUser, store.StartClaimUnconfirmed, "run-1", reportedAuth, reportedProfile, false, storedAuth, storedProfile},
		{"live create claim", store.StartClaimCreate, store.StartClaimLive, "run-1", reportedAuth, reportedProfile, false, storedAuth, storedProfile},
		{"no claim", store.StartClaimCreate, "", "run-1", reportedAuth, reportedProfile, false, storedAuth, storedProfile},
		// Pins current behaviour, which matches the dispatch-answer path
		// (both use store.ApplyBrokerAgentConfig): a pinned agent keeps its
		// stored profile, and its harness auth is the reported value.
		{"pinned agent, kept create, own run", store.StartClaimCreate, store.StartClaimUnconfirmed, "run-1", reportedAuth, reportedProfile, true, reportedAuth, storedProfile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, s := testServer(t)
			grantDevUserRuntimeBrokerAccess(t, s)
			broker, project := newQuotaTestBrokerAndProject(t, s, "kept-auth")
			agent := keptCreateHeartbeatAgent(t, s, broker, project, "kept-auth-agent", c.claimKind, c.claimState, "", func(a *store.Agent) {
				a.AppliedConfig.HarnessAuth = storedAuth
				a.AppliedConfig.Profile = storedProfile
				if c.pinned {
					a.PinnedRuntimeBrokerID, a.PinnedRuntimeTargetID, a.PinnedRuntimeTargetType = broker.ID, "t-1", "docker"
				}
			})
			before, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.NotNil(t, before.AppliedConfig)
			require.Equal(t, storedAuth, before.AppliedConfig.HarnessAuth, "setup: harness auth stored")
			require.Equal(t, storedProfile, before.AppliedConfig.Profile, "setup: profile stored")
			require.Equal(t, c.pinned, before.IsPinned(), "setup: pin stored")

			code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
				Slug:            agent.Slug,
				Phase:           "running",
				Activity:        "working",
				ContainerStatus: "Up 2 seconds",
				RunID:           c.runID,
				Template:        "resolved-template",
				HarnessConfig:   "resolved-harness-config",
				HarnessAuth:     c.hbAuth,
				Image:           "registry.example/resolved:v1",
				Profile:         c.hbProfile,
			})
			require.Equal(t, http.StatusOK, code)

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.NotNil(t, got.AppliedConfig)
			assert.Equal(t, c.wantAuth, got.AppliedConfig.HarnessAuth, "recorded harness auth")
			assert.Equal(t, c.wantProfile, got.AppliedConfig.Profile, "recorded profile")
		})
	}
}

// reincarnateWithStartAgentEcho runs a self reincarnation of an agent that
// starts with template old-template, runtime docker, runtime state
// container:old and container status "Up 1 hour", with a start dispatch
// whose broker answer echoes template new-template, runtime kubernetes,
// runtime state container:new and container status "Up 1 second". During
// the start, duringStart (when set) runs first, with the store. It returns
// the agent row once the reincarnation settled.
func reincarnateWithStartAgentEcho(t *testing.T, duringStart func(s store.Store, agentID string)) *store.Agent {
	t.Helper()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = "old-template"
		a.Runtime = "docker"
		a.RuntimeState = "container:old"
		a.ContainerStatus = "Up 1 hour"
	})
	disp.mu.Lock()
	disp.startAgentEcho = func(a *store.Agent) {
		if duringStart != nil {
			duringStart(s, a.ID)
		}
		a.Template = "new-template"
		a.Runtime = "kubernetes"
		a.RuntimeState = "container:new"
		a.ContainerStatus = "Up 1 second"
	}
	disp.mu.Unlock()
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, agent.Generation+1, final.Generation, "setup: the reincarnation must complete")
	return final
}

// TestReincarnateAgent_WorkerPersistsStartAgentEcho covers the agent-level
// echo of the reincarnation's start dispatch (ptone/scion#3032): the
// completion write persists the template, runtime, runtime state and
// container status the broker answered with, which the worker's re-read of
// the row would otherwise drop.
func TestReincarnateAgent_WorkerPersistsStartAgentEcho(t *testing.T) {
	final := reincarnateWithStartAgentEcho(t, nil)
	assert.Equal(t, "new-template", final.Template, "the start-echoed template must be persisted at completion")
	assert.Equal(t, "kubernetes", final.Runtime, "the start-echoed runtime must be persisted at completion")
	assert.Equal(t, "container:new", final.RuntimeState, "the start-echoed runtime state must be persisted at completion")
	assert.Equal(t, "Up 1 second", final.ContainerStatus, "the start-echoed container status must be persisted at completion")
}

// TestReincarnateAgent_StartEchoKeepsNewerContainerStatus: a heartbeat
// that records a container status after the starting step's write (the
// reincarnation guard leaves that field to heartbeats) keeps it; the start
// echo's container status does not overwrite it. The other echoed fields
// are still persisted.
func TestReincarnateAgent_StartEchoKeepsNewerContainerStatus(t *testing.T) {
	final := reincarnateWithStartAgentEcho(t, func(s store.Store, agentID string) {
		require.NoError(t, s.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{ContainerStatus: "Up 3 seconds"}))
	})
	assert.Equal(t, "Up 3 seconds", final.ContainerStatus, "a container status recorded after the starting step must be kept")
	assert.Equal(t, "new-template", final.Template, "the start-echoed template must still be persisted")
}
