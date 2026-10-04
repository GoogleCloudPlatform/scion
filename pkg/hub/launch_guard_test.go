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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// launchSeed puts an agent created in phase "created" into one of the
// launch states the start guard distinguishes.
type launchSeed int

const (
	seedInFlight launchSeed = iota
	seedIncompleteActive
	seedIncompleteEnded
)

func seedLaunch(t *testing.T, s store.Store, agent *store.Agent, seed launchSeed) *store.Agent {
	t.Helper()
	ctx := context.Background()
	launchID, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)
	switch seed {
	case seedIncompleteActive:
		require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStopped)}))
	case seedIncompleteEnded:
		require.NotEmpty(t, agent.RuntimeBrokerID, "an ended seed needs the agent's broker")
		ans, _, err := s.ApplyLaunchReport(ctx, agent.ID, agent.RuntimeBrokerID, store.LaunchReport{
			LaunchID: launchID, InstanceID: "i1", State: "failed",
			Step: "pull", Message: "image not found", ErrorCode: "image_pull_failed",
		})
		require.NoError(t, err)
		require.Equal(t, 0, ans.HTTPStatus, "report must apply")
	}
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	switch seed {
	case seedInFlight:
		require.True(t, got.IsInFlight())
	default:
		require.True(t, got.IsIncompleteCreate())
	}
	return got
}

func TestLaunchStartRefusal(t *testing.T) {
	now := time.Now()
	base := func() *store.Agent {
		return &store.Agent{
			Slug: "a1", Template: "tmpl", Phase: string(state.PhaseProvisioning),
			LaunchKind: store.LaunchKindCreate, LaunchState: store.LaunchStateActive,
			LaunchDeadline: now.Add(time.Minute),
			AppliedConfig:  &store.AgentAppliedConfig{Task: "the task"},
		}
	}

	t.Run("in flight before deadline", func(t *testing.T) {
		r := launchStartRefusal(base(), now)
		require.NotNil(t, r)
		assert.True(t, r.InFlight)
		assert.Equal(t, "agent_launching", r.Code)
		assert.Equal(t, http.StatusConflict, r.HTTPStatus)
	})
	t.Run("in flight past deadline proceeds", func(t *testing.T) {
		a := base()
		a.LaunchDeadline = now.Add(-time.Second)
		assert.Nil(t, launchStartRefusal(a, now))
	})
	t.Run("ended launch on a running agent proceeds", func(t *testing.T) {
		a := base()
		a.Phase = string(state.PhaseRunning)
		a.LaunchState = store.LaunchStateEnded
		assert.Nil(t, launchStartRefusal(a, now))
	})
	t.Run("stopped agent that has run proceeds", func(t *testing.T) {
		a := base()
		a.Phase = string(state.PhaseStopped)
		a.LaunchState = store.LaunchStateEnded
		a.LaunchError = ""
		assert.Nil(t, launchStartRefusal(a, now))
	})
	t.Run("deleted agent proceeds", func(t *testing.T) {
		a := base()
		a.DeletedAt = now
		assert.Nil(t, launchStartRefusal(a, now))
	})
	t.Run("incomplete create still stopping", func(t *testing.T) {
		a := base()
		a.Phase = string(state.PhaseStopping)
		r := launchStartRefusal(a, now)
		require.NotNil(t, r)
		assert.False(t, r.InFlight)
		assert.Equal(t, "agent_create_incomplete", r.Code)
		assert.Equal(t, "agent a1 cannot be started: its create is still stopping; "+incompleteCreateRecoveryHint, r.Message)
		assert.Equal(t, "tmpl", r.Details["template"])
		assert.Equal(t, "the task", r.Details["task"])
	})
	t.Run("incomplete create ended", func(t *testing.T) {
		a := base()
		a.Phase = string(state.PhaseError)
		a.LaunchState = store.LaunchStateEnded
		a.LaunchError = "launch_timeout"
		r := launchStartRefusal(a, now)
		require.NotNil(t, r)
		assert.Equal(t, "agent_create_incomplete", r.Code)
		assert.Equal(t, "agent a1 cannot be started: its create did not complete (launch_timeout); "+incompleteCreateRecoveryHint, r.Message)
	})
}

func TestDispatchAgentStart_LaunchGuard(t *testing.T) {
	cases := []struct {
		name    string
		seed    launchSeed
		wantErr error
	}{
		{"in-flight", seedInFlight, ErrLaunchInFlight},
		{"incomplete-active", seedIncompleteActive, ErrAgentCreateIncomplete},
		{"incomplete-ended", seedIncompleteEnded, ErrAgentCreateIncomplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "guard-"+tc.name, state.PhaseCreated)
			stale := *agent // the caller's copy predates the launch
			seedLaunch(t, s, agent, tc.seed)

			client := &mockRuntimeBrokerClient{}
			d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())

			err := d.DispatchAgentStart(context.Background(), &stale, "", false)
			require.ErrorIs(t, err, tc.wantErr)
			assert.False(t, client.startCalled, "no broker call")

			err = d.DispatchAgentRestart(context.Background(), &stale)
			require.ErrorIs(t, err, tc.wantErr)
			assert.False(t, client.restartCalled, "no broker call")
		})
	}
}

func TestDispatchAgentStart_LaunchPastDeadlineProceeds(t *testing.T) {
	_, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "guard-past-deadline", state.PhaseCreated)
	_, err := s.BeginLaunch(context.Background(), agent.ID, store.LaunchKindCreate, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	require.NoError(t, d.DispatchAgentStart(context.Background(), agent, "", false))
	assert.True(t, client.startCalled)
}

func decodeLaunchGuardError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Error
}

func TestAgentLifecycle_LaunchGuard(t *testing.T) {
	setup := func(t *testing.T, suffix string, seed launchSeed) (*Server, *mockRuntimeBrokerClient, *store.Agent) {
		srv, s := testServer(t)
		agent := setupBrokerAgentInPhase(t, s, suffix, state.PhaseCreated)
		agent = seedLaunch(t, s, agent, seed)
		client := &mockRuntimeBrokerClient{}
		srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
		return srv, client, agent
	}

	t.Run("restart while launching is not performed", func(t *testing.T) {
		srv, client, agent := setup(t, "lg-restart", seedInFlight)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.False(t, client.stopCalled, "the stop leg must not run")
		assert.False(t, client.startCalled)
		var resp AgentWithWarnings
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, agent.ID, resp.ID)
		assert.Equal(t, []string{launchRestartNotPerformedWarning}, resp.Warnings)
		require.NotNil(t, resp.Launch)
		assert.True(t, resp.Launch.Active)
	})

	t.Run("start while launching returns the agent", func(t *testing.T) {
		srv, client, agent := setup(t, "lg-start", seedInFlight)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.False(t, client.startCalled)
		var resp AgentWithWarnings
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Empty(t, resp.Warnings, "no inputs, no warning")
		assert.Equal(t, string(state.PhaseCreated), resp.Phase, "phase is not changed")
	})

	// A body that decodes to no inputs is not a request with inputs.
	for name, body := range map[string]interface{}{
		"empty-object":       map[string]bool{},
		"force-resume-false": map[string]bool{"forceResume": false},
	} {
		t.Run("start with "+name+" body while launching does not warn", func(t *testing.T) {
			srv, client, agent := setup(t, "lg-start-"+name, seedInFlight)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.False(t, client.startCalled)
			var resp AgentWithWarnings
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Empty(t, resp.Warnings)
		})
	}

	// A body that does not decode is treated as carrying inputs.
	t.Run("start with a malformed body while launching warns", func(t *testing.T) {
		srv, client, agent := setup(t, "lg-start-malformed", seedInFlight)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", strings.NewReader("{"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.False(t, client.startCalled)
		var resp AgentWithWarnings
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, []string{launchInFlightInputsWarning}, resp.Warnings)
	})

	t.Run("start with inputs while launching warns", func(t *testing.T) {
		srv, _, agent := setup(t, "lg-start-inputs", seedInFlight)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", map[string]bool{"forceResume": true})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AgentWithWarnings
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, []string{launchInFlightInputsWarning}, resp.Warnings)
	})

	for _, action := range []string{"start", "restart"} {
		t.Run(action+" on an incomplete create is refused", func(t *testing.T) {
			srv, client, agent := setup(t, "lg-incomplete-"+action, seedIncompleteEnded)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			apiErr := decodeLaunchGuardError(t, rec)
			assert.Equal(t, "agent_create_incomplete", apiErr.Code)
			assert.Contains(t, apiErr.Message, "(image_pull_failed)")
			assert.Contains(t, apiErr.Details, "template")
			assert.Contains(t, apiErr.Details, "task")
			assert.False(t, client.stopCalled)
			assert.False(t, client.startCalled)
		})
	}
}

func TestHandleExistingAgent_LaunchGuard(t *testing.T) {
	t.Run("in flight returns the agent with a warning for inputs", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		disp := &createAgentDispatcher{}
		f.srv.SetDispatcher(disp)
		agent := f.agent(t, "hea-launching", string(state.PhaseCreated))
		seedLaunch(t, f.store, agent, seedInFlight)

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": agent.Slug, "projectId": f.project.ID, "task": "new task",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		assert.Equal(t, agent.ID, resp.Agent.ID)
		assert.Equal(t, []string{launchInFlightInputsWarning}, resp.Warnings)
		assert.False(t, disp.startCalled)
		assert.Nil(t, disp.capturedAgent, "no create dispatch")
	})

	t.Run("provisioning env-gather recreate is not run while in flight", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		disp := &createAgentDispatcher{}
		f.srv.SetDispatcher(disp)
		agent := f.agent(t, "hea-launching-gather", string(state.PhaseCreated))
		seedLaunch(t, f.store, agent, seedInFlight)
		_, err := f.store.MarkLaunchAccepted(context.Background(), agent.ID, mustAgent(t, f.store, agent.ID).LaunchID, "i1")
		require.NoError(t, err)
		require.Equal(t, string(state.PhaseProvisioning), mustAgent(t, f.store, agent.ID).Phase)

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": agent.Slug, "projectId": f.project.ID, "gatherEnv": true,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.False(t, disp.deleteCalled)
		mustAgent(t, f.store, agent.ID) // still there
	})

	t.Run("incomplete create is refused", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		disp := &createAgentDispatcher{}
		f.srv.SetDispatcher(disp)
		agent := f.agent(t, "hea-incomplete", string(state.PhaseCreated))
		seedLaunch(t, f.store, agent, seedIncompleteActive)

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": agent.Slug, "projectId": f.project.ID, "resume": true,
		})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		apiErr := decodeLaunchGuardError(t, rec)
		assert.Equal(t, "agent_create_incomplete", apiErr.Code)
		assert.Contains(t, apiErr.Message, "still stopping")
		assert.False(t, disp.startCalled)
	})
}

func mustAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func TestReincarnateAgent_LaunchGuard(t *testing.T) {
	cases := []struct {
		name string
		seed launchSeed
		code string
	}{
		{"in-flight", seedInFlight, "agent_launching"},
		{"incomplete", seedIncompleteActive, "agent_create_incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) { a.Phase = string(state.PhaseCreated) })
			seedLaunch(t, s, agent, tc.seed)

			req := reincarnateRequest(t, agent.ID, agentIdentityFor(agent.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"})
			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, req, agent.ID)

			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, decodeLaunchGuardError(t, rec).Code)
			got := mustAgent(t, s, agent.ID)
			assert.Equal(t, store.ReincarnationStateNone, got.ReincarnationState, "no reincarnation is claimed")
		})
	}
}

func TestWakeAgentForDM_LaunchGuard(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "wake-incomplete", state.PhaseCreated)
	seedLaunch(t, s, agent, seedInFlight)
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))
	agent = mustAgent(t, s, agent.ID)
	require.True(t, agent.IsIncompleteCreate())

	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))

	res, dmErr := srv.wakeAgentForDM(context.Background(), agent)
	assert.Nil(t, res)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeRuntimeError, dmErr.Code)
	assert.Contains(t, dmErr.Message, "still stopping")
	assert.False(t, client.startCalled)
}
