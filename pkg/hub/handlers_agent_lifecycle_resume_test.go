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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifecycleResumeDispatcher extends createAgentDispatcher to record the
// resume flag passed to DispatchAgentStart, so tests of the /start lifecycle
// action can assert whether a harness resume was requested.
type lifecycleResumeDispatcher struct {
	createAgentDispatcher
	lastStartResume bool
}

func (d *lifecycleResumeDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, resume bool) error {
	d.startCalled = true
	d.lastStartResume = resume
	// Mimic a real broker's start acknowledgment: handleAgentLifecycle reuses
	// the broker-reported phase off the agent pointer it passed in.
	agent.Phase = string(state.PhaseRunning)
	return nil
}

// setupBrokerAgentInPhase creates a project, an online runtime broker, and an
// agent assigned to that broker in the given phase.
func setupBrokerAgentInPhase(t *testing.T, s store.Store, suffix string, phase state.Phase) *store.Agent {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("proj-lc-resume-" + suffix),
		Name: "LC Resume Project " + suffix,
		Slug: "lc-resume-project-" + suffix,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid("broker-lc-resume-" + suffix),
		Name:     "LC Resume Broker " + suffix,
		Slug:     "lc-resume-broker-" + suffix,
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              tid("agent-lc-resume-" + suffix),
		Slug:            "agent-lc-resume-" + suffix + "-slug",
		Name:            "Agent LC Resume " + suffix,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	return agent
}

// TestAgentLifecycle_Start_ErrorPhase_ForceResume verifies that POSTing
// {"forceResume": true} to /start for an error-phase agent asks the broker
// to resume the harness session (best-effort resume), closing the gap noted
// in #1868: the lifecycle start handler previously had no way to request a
// resume for an agent that landed in phase=error.
func TestAgentLifecycle_Start_ErrorPhase_ForceResume(t *testing.T) {
	srv, s := testServer(t)
	disp := &lifecycleResumeDispatcher{}
	srv.SetDispatcher(disp)

	agent := setupBrokerAgentInPhase(t, s, "force", state.PhaseError)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start",
		map[string]bool{"forceResume": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, disp.startCalled, "DispatchAgentStart should be called")
	assert.True(t, disp.lastStartResume,
		"forceResume on an error-phase agent should request a harness resume")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
}

// TestAgentLifecycle_Start_ErrorPhase_NoForceResume_StartsFresh verifies that
// a plain start (no body, or forceResume omitted) on an error-phase agent
// keeps starting fresh — best-effort resume is opt-in only.
func TestAgentLifecycle_Start_ErrorPhase_NoForceResume_StartsFresh(t *testing.T) {
	srv, s := testServer(t)
	disp := &lifecycleResumeDispatcher{}
	srv.SetDispatcher(disp)

	agent := setupBrokerAgentInPhase(t, s, "plain", state.PhaseError)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, disp.startCalled, "DispatchAgentStart should be called")
	assert.False(t, disp.lastStartResume,
		"a plain start on an error-phase agent must not resume the harness session")
}

// TestAgentLifecycle_Start_StoppedPhase_ForceResumeIgnored verifies that
// forceResume only ever applies to phase=error. A stopped agent restarts
// fresh via the plain start action regardless of forceResume, matching the
// create-agent resume path's resumeInPlaceDecision semantics.
func TestAgentLifecycle_Start_StoppedPhase_ForceResumeIgnored(t *testing.T) {
	srv, s := testServer(t)
	disp := &lifecycleResumeDispatcher{}
	srv.SetDispatcher(disp)

	agent := setupBrokerAgentInPhase(t, s, "stopped", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start",
		map[string]bool{"forceResume": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, disp.startCalled, "DispatchAgentStart should be called")
	assert.False(t, disp.lastStartResume,
		"forceResume must not apply outside phase=error")
}

// TestAgentLifecycle_Start_SuspendedPhase_AlwaysResumes verifies the
// pre-existing suspended-agent resume behavior is unaffected by the new
// forceResume handling.
func TestAgentLifecycle_Start_SuspendedPhase_AlwaysResumes(t *testing.T) {
	srv, s := testServer(t)
	disp := &lifecycleResumeDispatcher{}
	srv.SetDispatcher(disp)

	agent := setupBrokerAgentInPhase(t, s, "suspended", state.PhaseSuspended)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.True(t, disp.startCalled, "DispatchAgentStart should be called")
	assert.True(t, disp.lastStartResume, "a suspended agent should always resume on start")
}
