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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createWakeTestFixtures creates a project, broker, project-provider and agent
// with the given phase. It returns the server, store, and agent for further
// assertions. The broker is always online so checkBrokerAvailability passes.
func createWakeTestFixtures(t *testing.T, agentPhase string) (*Server, store.Store, *store.Agent) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-wake-" + agentPhase,
		Name: "Wake Test Project",
		Slug: "wake-test-project-" + agentPhase,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-wake-" + agentPhase,
		Name:   "Wake Test Broker",
		Slug:   "wake-test-broker-" + agentPhase,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-wake-" + agentPhase,
		Slug:            "agent-wake-" + agentPhase,
		Name:            "Wake Agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           agentPhase,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, agent
}

// TestHandleAgentMessage_WakeStopped verifies that sending a wake message to
// a stopped agent returns a 400 error.
func TestHandleAgentMessage_WakeStopped(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseStopped))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
		"wake":    true,
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Agent is stopped")
}

// TestHandleAgentMessage_WakeError verifies that sending a wake message to
// an agent in error state returns a 400 error.
func TestHandleAgentMessage_WakeError(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseError))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
		"wake":    true,
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Agent is in error state")
}

// TestHandleAgentMessage_WakeRunning verifies that sending a wake message to
// an already-running agent is a no-op (the message is delivered normally).
func TestHandleAgentMessage_WakeRunning(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseRunning))

	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello after wake noop",
		"wake":    true,
	})

	assert.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	calls := disp.getCalls()
	require.Len(t, calls, 1, "expected message to be dispatched")
	assert.Equal(t, "hello after wake noop", calls[0].Message)
}

// TestHandleAgentMessage_WakeUnknownPhase verifies that sending a wake message
// to an agent in an intermediate phase (e.g. provisioning) returns a 400 error.
func TestHandleAgentMessage_WakeUnknownPhase(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseProvisioning))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
		"wake":    true,
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Agent is not yet running")
}

// TestWaitForAgentReady_Timeout verifies that waitForAgentReady returns a
// timeout error when the agent never reports activity.
func TestWaitForAgentReady_Timeout(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseStarting))

	ctx := context.Background()
	err := srv.waitForAgentReady(ctx, agent.ID, 100*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out waiting for agent to become ready")
}

// TestWaitForAgentReady_ActivityReported verifies that waitForAgentReady
// returns successfully once the agent's Activity field is non-empty.
func TestWaitForAgentReady_ActivityReported(t *testing.T) {
	srv, s, agent := createWakeTestFixtures(t, string(state.PhaseStarting))
	ctx := context.Background()

	// In a goroutine, update the agent's activity after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Activity: "idle",
		})
	}()

	err := srv.waitForAgentReady(ctx, agent.ID, 2*time.Second)
	require.NoError(t, err)
}

// TestWaitForAgentReady_UnexpectedPhase verifies that waitForAgentReady
// returns an error when the agent transitions to an unexpected phase.
func TestWaitForAgentReady_UnexpectedPhase(t *testing.T) {
	srv, s, agent := createWakeTestFixtures(t, string(state.PhaseStarting))
	ctx := context.Background()

	// In a goroutine, change the agent's phase to stopped after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStopped),
		})
	}()

	err := srv.waitForAgentReady(ctx, agent.ID, 2*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected phase")
}
