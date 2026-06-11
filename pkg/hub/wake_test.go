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
	"fmt"
	"net/http"
	"sync"
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
		ID:   tid("project-wake-" + agentPhase),
		Name: "Wake Test Project",
		Slug: "wake-test-project-" + agentPhase,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     tid("broker-wake-" + agentPhase),
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
		ID:              tid("agent-wake-" + agentPhase),
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

// wakeRecordingDispatcher records DispatchAgentStart and DispatchAgentMessage
// calls for wake tests. Both methods return their configured error values.
type wakeRecordingDispatcher struct {
	recordingDispatcher
	mu2            sync.Mutex
	startCalls     []wakeStartCall
	startReturnErr error
}

type wakeStartCall struct {
	Agent    *store.Agent
	Prompt   string
	Continue bool
}

func (d *wakeRecordingDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.startCalls = append(d.startCalls, wakeStartCall{Agent: agent, Prompt: prompt, Continue: cont})
	return d.startReturnErr
}

func (d *wakeRecordingDispatcher) getStartCalls() []wakeStartCall {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	result := make([]wakeStartCall, len(d.startCalls))
	copy(result, d.startCalls)
	return result
}

// TestHandleAgentMessage_WakeSuspended verifies the primary wake use case:
// sending a message with wake=true to a suspended agent resumes it and
// delivers the message.
func TestHandleAgentMessage_WakeSuspended(t *testing.T) {
	srv, s, agent := createWakeTestFixtures(t, string(state.PhaseSuspended))

	disp := &wakeRecordingDispatcher{}
	srv.SetDispatcher(disp)

	// Simulate the agent becoming ready after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello after wake",
		"wake":    true,
	})

	assert.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	// Verify DispatchAgentStart was called with continue=true.
	startCalls := disp.getStartCalls()
	require.Len(t, startCalls, 1)
	assert.True(t, startCalls[0].Continue, "DispatchAgentStart should be called with continue=true")
	assert.Equal(t, agent.ID, startCalls[0].Agent.ID)

	// Verify the message was dispatched.
	calls := disp.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "hello after wake", calls[0].Message)
}

// TestHandleAgentMessage_WakeSuspendedStartFails verifies that when
// DispatchAgentStart fails, the handler returns 502 and does NOT set
// the agent to error state.
func TestHandleAgentMessage_WakeSuspendedStartFails(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseSuspended))

	disp := &wakeRecordingDispatcher{
		startReturnErr: fmt.Errorf("container start failed"),
	}
	srv.SetDispatcher(disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
		"wake":    true,
	})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "Failed to wake agent")
	assert.Contains(t, rec.Body.String(), "container start failed")
}

// TestHandleAgentMessage_WakeSuspendedDeliveryFails verifies the distinct
// error when wake succeeds but message delivery fails.
func TestHandleAgentMessage_WakeSuspendedDeliveryFails(t *testing.T) {
	srv, s, agent := createWakeTestFixtures(t, string(state.PhaseSuspended))

	disp := &wakeRecordingDispatcher{}
	disp.returnErr = fmt.Errorf("broker unavailable")
	srv.SetDispatcher(disp)

	// Simulate the agent becoming ready after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
		"wake":    true,
	})

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Contains(t, rec.Body.String(), "Agent resumed successfully but message delivery failed")
	assert.Contains(t, rec.Body.String(), "broker unavailable")
}

// TestHandleAgentMessage_SuspendedWithoutWake verifies that messaging a
// suspended agent without --wake returns a clear error.
func TestHandleAgentMessage_SuspendedWithoutWake(t *testing.T) {
	srv, _, agent := createWakeTestFixtures(t, string(state.PhaseSuspended))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
	})

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "suspended")
	assert.Contains(t, rec.Body.String(), "--wake")
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
