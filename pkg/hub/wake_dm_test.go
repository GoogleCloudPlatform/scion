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

// ---------------------------------------------------------------------------
// Shared wake helper and ExecuteAgentDM wake integration tests (#1691)
//
// AC-1: An authorized suspended agent DM invokes resume exactly once and
//       dispatch occurs only after readiness.
// AC-2: Denied, oversized or unsupported-attachment requests cannot resume
//       an agent.
// AC-3: Wake on native/legacy groups and human DMs succeeds according to
//       normal routing and invokes zero resumes.
// AC-4: Resume failure/readiness timeout returns an explicit failure and no
//       message dispatch; already-running targets do not restart.
// AC-5: Structured and outbound agent DMs exhibit the same wake behavior,
//       including explicit unsupported-runtime cases.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// createWakeDMFixtures creates a project, broker, project-provider, sender
// agent (always running), and target agent (with the given phase) for wake
// DM tests. Both agents are in the same project with project-level message
// mode so authorization succeeds.
func createWakeDMFixtures(t *testing.T, targetPhase string) (
	srv *Server, s store.Store,
	senderAgent, targetAgent *store.Agent,
) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-wake-dm-" + targetPhase),
		Name: "Wake DM Test Project",
		Slug: "wake-dm-test-project-" + targetPhase,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     tid("broker-wake-dm-" + targetPhase),
		Name:   "Wake DM Test Broker",
		Slug:   "wake-dm-test-broker-" + targetPhase,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	// Create an owner user for ancestry.
	owner := &store.User{
		ID:      tid("owner-wake-dm-" + targetPhase),
		Email:   "owner-wake-dm-" + targetPhase + "@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))

	senderAgent = &store.Agent{
		ID:              tid("sender-wake-dm-" + targetPhase),
		Slug:            "sender-wake-dm-" + targetPhase,
		Name:            "Sender Agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, senderAgent))

	targetAgent = &store.Agent{
		ID:              tid("target-wake-dm-" + targetPhase),
		Slug:            "target-wake-dm-" + targetPhase,
		Name:            "Target Agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           targetPhase,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	return srv, s, senderAgent, targetAgent
}

// wakeTrackingDispatcher records DispatchAgentStart and DispatchAgentMessage
// calls to verify wake invocations.
type wakeTrackingDispatcher struct {
	mu2            sync.Mutex
	messageCalls   []wakeDispatchMsg
	startCalls     []wakeDispatchStart
	startReturnErr error
	msgReturnErr   error
}

type wakeDispatchStart struct {
	AgentID  string
	Prompt   string
	Continue bool
}

type wakeDispatchMsg struct {
	AgentID   string
	Message   string
	Interrupt bool
}

func (d *wakeTrackingDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, prompt string, cont bool) error {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.startCalls = append(d.startCalls, wakeDispatchStart{AgentID: agent.ID, Prompt: prompt, Continue: cont})
	return d.startReturnErr
}

func (d *wakeTrackingDispatcher) DispatchAgentMessage(_ context.Context, agent *store.Agent, message string, interrupt bool, _ *messages.StructuredMessage) error {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	d.messageCalls = append(d.messageCalls, wakeDispatchMsg{AgentID: agent.ID, Message: message, Interrupt: interrupt})
	return d.msgReturnErr
}

func (d *wakeTrackingDispatcher) getStartCalls() []wakeDispatchStart {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	out := make([]wakeDispatchStart, len(d.startCalls))
	copy(out, d.startCalls)
	return out
}

func (d *wakeTrackingDispatcher) getMessageCalls() []wakeDispatchMsg {
	d.mu2.Lock()
	defer d.mu2.Unlock()
	out := make([]wakeDispatchMsg, len(d.messageCalls))
	copy(out, d.messageCalls)
	return out
}

// Implement remaining AgentDispatcher methods as no-ops.
func (d *wakeTrackingDispatcher) DispatchAgentCreate(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentProvision(_ context.Context, _ *store.Agent) error {
	return nil
}

func (d *wakeTrackingDispatcher) DispatchAgentReprovision(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentResetAuth(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	return nil
}
func (d *wakeTrackingDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *wakeTrackingDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *wakeTrackingDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *wakeTrackingDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return "", 0, nil
}
func (d *wakeTrackingDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

// ---------------------------------------------------------------------------
// Unit tests for wakeAgentForDM helper
// ---------------------------------------------------------------------------

func TestWakeAgentForDM_Suspended_ResumeSuccess(t *testing.T) {
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	// Simulate agent becoming ready after a short delay.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, WakeResumed, result.Outcome)

	// Verify DispatchAgentStart was called exactly once with continue=true (AC-1).
	starts := disp.getStartCalls()
	require.Len(t, starts, 1, "DispatchAgentStart should be called exactly once")
	assert.True(t, starts[0].Continue, "DispatchAgentStart should be called with continue=true")
	assert.Equal(t, target.ID, starts[0].AgentID)

	// Verify agent phase is now running.
	assert.Equal(t, string(state.PhaseRunning), target.Phase)
}

func TestWakeAgentForDM_Running_NoOp(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseRunning))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, WakeAlreadyRunning, result.Outcome)

	// No resume should have been invoked (AC-4: already-running targets do not restart).
	starts := disp.getStartCalls()
	assert.Empty(t, starts, "DispatchAgentStart should NOT be called for a running agent")
}

func TestWakeAgentForDM_Stopped_Error(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseStopped))

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeValidationError, dmErr.Code)
	assert.Equal(t, http.StatusBadRequest, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "Agent is stopped")
	assert.Contains(t, dmErr.Message, "scion resume")
}

func TestWakeAgentForDM_Error_Error(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseError))

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeValidationError, dmErr.Code)
	assert.Equal(t, http.StatusBadRequest, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "Agent is in error state")
}

func TestWakeAgentForDM_Provisioning_Error(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseProvisioning))

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeValidationError, dmErr.Code)
	assert.Equal(t, http.StatusBadRequest, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "Agent is not yet running")
}

func TestWakeAgentForDM_Suspended_NoDispatcher(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	// No dispatcher set — srv.GetDispatcher() returns nil.

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "dispatch not available")
}

func TestWakeAgentForDM_Suspended_NoBrokerID(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	target.RuntimeBrokerID = "" // Clear broker ID.

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeUnavailable, dmErr.Code)
	assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "no runtime broker")
}

func TestWakeAgentForDM_Suspended_DispatchStartFails(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))

	disp := &wakeTrackingDispatcher{
		startReturnErr: fmt.Errorf("container crashed"),
	}
	srv.SetDispatcher(disp)

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeRuntimeError, dmErr.Code)
	assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "Failed to wake agent")
	assert.Contains(t, dmErr.Message, "container crashed")
}

func TestWakeAgentForDM_Suspended_ReadinessTimeout(t *testing.T) {
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	// Agent never becomes ready → timeout.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, dmErr := srv.wakeAgentForDM(ctx, target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeRuntimeError, dmErr.Code)
	assert.Equal(t, http.StatusBadGateway, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "did not become ready")

	// A readiness timeout must not move the agent to an uncounted phase
	// (ptone/scion#1984): the container may still be running and occupying
	// the broker slot, so the stored phase must stay "starting" — the phase
	// this same helper set just before waiting — rather than "error".
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStarting), got.Phase,
		"readiness timeout must leave the agent in a counted phase, not error")
	assert.Contains(t, got.Message, "Failed to become ready after wake")
}

func TestWakeAgentForDM_ManagedRuntime_Unsupported(t *testing.T) {
	srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	target.Runtime = "managed:test-backend" // Mark as managed runtime.

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.wakeAgentForDM(context.Background(), target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeUnsupportedCapability, dmErr.Code)
	assert.Equal(t, http.StatusUnprocessableEntity, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "managed agent runtimes")
	assert.Contains(t, dmErr.Message, "suspend/resume")

	// No resume should have been invoked (AC-5: explicit unsupported-runtime).
	starts := disp.getStartCalls()
	assert.Empty(t, starts, "DispatchAgentStart should NOT be called for managed runtimes")
}

// ---------------------------------------------------------------------------
// Unit tests for validateAgentDeliverable
// ---------------------------------------------------------------------------

func TestValidateAgentDeliverable_Running(t *testing.T) {
	agent := &store.Agent{Slug: "test-agent", Phase: string(state.PhaseRunning)}
	assert.Nil(t, validateAgentDeliverable(agent))
}

func TestValidateAgentDeliverable_Suspended(t *testing.T) {
	agent := &store.Agent{Slug: "test-agent", Phase: string(state.PhaseSuspended)}
	dmErr := validateAgentDeliverable(agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "suspended")
	assert.Contains(t, dmErr.Message, "--wake")
}

func TestValidateAgentDeliverable_Stopped(t *testing.T) {
	agent := &store.Agent{Slug: "test-agent", Phase: string(state.PhaseStopped)}
	dmErr := validateAgentDeliverable(agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "stopped")
}

func TestValidateAgentDeliverable_Error(t *testing.T) {
	agent := &store.Agent{Slug: "test-agent", Phase: string(state.PhaseError)}
	dmErr := validateAgentDeliverable(agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "error state")
}

func TestValidateAgentDeliverable_Unknown(t *testing.T) {
	agent := &store.Agent{Slug: "test-agent", Phase: string(state.PhaseProvisioning)}
	dmErr := validateAgentDeliverable(agent)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "not yet running")
}

// ---------------------------------------------------------------------------
// Integration: ExecuteAgentDM with wake
// ---------------------------------------------------------------------------

// wakeDMTestIdentity implements the AgentIdentity interface for test agent senders.
type wakeDMTestIdentity struct {
	id        string
	projectID string
	ancestry  []string
}

func (i *wakeDMTestIdentity) ID() string                      { return i.id }
func (i *wakeDMTestIdentity) Type() string                    { return "agent" }
func (i *wakeDMTestIdentity) ProjectID() string               { return i.projectID }
func (i *wakeDMTestIdentity) Scopes() []AgentTokenScope       { return nil }
func (i *wakeDMTestIdentity) HasScope(_ AgentTokenScope) bool { return true }
func (i *wakeDMTestIdentity) Ancestry() []string              { return i.ancestry }
func (i *wakeDMTestIdentity) OriginUserID() string {
	if len(i.ancestry) > 0 {
		return i.ancestry[0]
	}
	return ""
}
func (i *wakeDMTestIdentity) TokenID() string { return "test-token-id" }

func TestExecuteAgentDM_Wake_Suspended_Delivers(t *testing.T) {
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	// Simulate agent becoming ready.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            "hello after wake",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	require.Nil(t, dmErr, "ExecuteAgentDM should succeed")
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAccepted, result.Outcome)
	assert.NotEmpty(t, result.MessageID)

	// Verify resume was invoked exactly once (AC-1).
	starts := disp.getStartCalls()
	require.Len(t, starts, 1)
	assert.True(t, starts[0].Continue)
}

func TestExecuteAgentDM_Wake_Running_NoResume(t *testing.T) {
	srv, _, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            "hello already running",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAccepted, result.Outcome)

	// No resume should have been invoked (AC-4).
	starts := disp.getStartCalls()
	assert.Empty(t, starts)
}

func TestExecuteAgentDM_Wake_Stopped_NoDispatch(t *testing.T) {
	srv, _, sender, target := createWakeDMFixtures(t, string(state.PhaseStopped))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            "should not be delivered",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	assert.Nil(t, result, "result should be nil on wake failure")
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusBadRequest, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "stopped")

	// No message dispatch should have occurred (AC-4).
	msgs := disp.getMessageCalls()
	assert.Empty(t, msgs)
}

func TestExecuteAgentDM_NoWake_Suspended_Rejected(t *testing.T) {
	srv, _, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            "should not be delivered",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           false,
	})

	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeAgentNotRunning, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "suspended")
	assert.Contains(t, dmErr.Message, "--wake")
}

// TestExecuteAgentDM_Wake_DeniedDoesNotResume verifies that an
// authorization-denied request does not invoke resume (AC-2).
func TestExecuteAgentDM_Wake_DeniedDoesNotResume(t *testing.T) {
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	ctx := context.Background()

	// Set message modes to deny: sender is none (sealed), target is project.
	// Must update the store record because authorizeAgentMessage re-reads
	// the sender from the store.
	sender.MessageMode = store.MessageModeNone
	require.NoError(t, s.UpdateAgent(ctx, sender))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            "this should be denied",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeMessageDenied, dmErr.Code)

	// No resume should have been invoked — admission denied before wake (AC-2).
	starts := disp.getStartCalls()
	assert.Empty(t, starts, "denied request must NOT invoke resume")
}

// TestExecuteAgentDM_Wake_OversizedDoesNotResume verifies that an oversized
// message does not invoke resume (AC-2).
func TestExecuteAgentDM_Wake_OversizedDoesNotResume(t *testing.T) {
	srv, _, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	// Create a message that exceeds the length limit.
	longMsg := make([]byte, 200_001)
	for i := range longMsg {
		longMsg[i] = 'x'
	}

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            string(longMsg),
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeValidationError, dmErr.Code)

	// No resume should have been invoked — validation failed before wake (AC-2).
	starts := disp.getStartCalls()
	assert.Empty(t, starts, "oversized message must NOT invoke resume")
}

func TestExecuteAgentDM_Wake_ManagedRuntime_Unsupported(t *testing.T) {
	srv, _, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	target.Runtime = "managed:test-backend"

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
		TargetAgent:    target,
		Msg:            "wake managed",
		Type:           "instruction",
		ProjectID:      sender.ProjectID,
		Wake:           true,
	})

	assert.Nil(t, result)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeUnsupportedCapability, dmErr.Code)
	assert.Equal(t, http.StatusUnprocessableEntity, dmErr.HTTPStatus)
	assert.Contains(t, dmErr.Message, "managed agent runtimes")

	// No resume should have been invoked (AC-5).
	starts := disp.getStartCalls()
	assert.Empty(t, starts)
}

// ---------------------------------------------------------------------------
// Wire-level test: wake:true targeting a human recipient invokes zero resumes
// (AC-3 regression guard)
// ---------------------------------------------------------------------------

func TestOutboundMessage_WakeHumanRecipient_ZeroResumes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "Wake Human Test",
		Slug: "wake-human-test",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	user := &store.User{
		ID:     api.NewUUID(),
		Email:  "human-wake-test@test.example",
		Role:   store.UserRoleMember,
		Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	agent := &store.Agent{
		ID:          api.NewUUID(),
		Name:        "wake-human-sender",
		Slug:        "wake-human-sender",
		ProjectID:   project.ID,
		Phase:       string(state.PhaseRunning),
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	// Send an outbound message with wake:true to a human user.
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email,
		Msg:       "hello human with wake",
		Wake:      true,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)

	// Expect success — wake is silently ignored for human recipients.
	assert.Equal(t, http.StatusOK, rr.Code,
		"outbound message to human with wake:true should succeed; body: %s", rr.Body.String())

	// Zero DispatchAgentStart calls — wake must not invoke resume on a human.
	starts := disp.getStartCalls()
	assert.Empty(t, starts, "wake:true targeting a human recipient must invoke zero resumes")
}

// ---------------------------------------------------------------------------
// Wake readiness-timeout quota accounting (ptone/scion#1984)
//
// A DM wake re-reserves the target's max_agents_per_broker slot before
// dispatch (ptone/scion#1963). If the resumed container then fails to signal
// readiness in time, the container itself may still be running — a readiness
// timeout is not a confirmed exit. Before this fix, the timeout path wrote
// phase=error directly, an uncounted phase, so the reservation looked stale
// to ReconcileStaleBrokerQuotaReservations and was released out from under a
// (possibly still-running) container, letting the broker exceed its cap.
// ---------------------------------------------------------------------------

// TestBrokerQuota_WakeReadinessTimeoutHoldsSlotThroughReconcile is the RED/GREEN
// regression test for ptone/scion#1984: a wake readiness timeout must keep the
// broker slot reserved through a reconcile pass, and a subsequent start at cap
// must still be refused.
func TestBrokerQuota_WakeReadinessTimeoutHoldsSlotThroughReconcile(t *testing.T) {
	// createWakeDMFixtures also creates a permanently-running sender agent on
	// the same broker (needed for message authorization), created directly
	// via the store rather than the reservation-creating HTTP flow. It holds
	// no reservation of its own until a reconcile pass backfills one, so the
	// cap below is sized for target + sender: 2.
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	setBrokerAgentCeiling(t, s, 2)
	require.Equal(t, sender.RuntimeBrokerID, target.RuntimeBrokerID)

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	// Target never reports activity, so waitForAgentReady times out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, dmErr := srv.wakeAgentForDM(ctx, target)
	assert.Nil(t, result)
	require.NotNil(t, dmErr, "readiness timeout must surface as a wake failure")

	require.EqualValues(t, 1, brokerReservationCount(t, s, target.RuntimeBrokerID),
		"wake must reserve the slot before dispatch")

	// Run the periodic sweep that reconciles reservations against observed
	// phase. It also backfills the sender's until-now-unreserved running
	// agent, bringing the count to 2 (= the cap) — unless the timeout
	// wrongly freed the target's slot first, in which case it would stay at
	// 1. On base this releases the target's slot because phase=error is
	// uncounted; on head the agent is still "starting" (counted), so its
	// reservation survives alongside the backfilled sender.
	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	require.EqualValues(t, 2, brokerReservationCount(t, s, target.RuntimeBrokerID),
		"reconcile must not release a wake-timeout agent's reservation while its container may still be running")

	// At cap, starting a third agent on the same broker/project must be
	// refused — on base this incorrectly returns 200 because the reconcile
	// pass above already freed the target's slot.
	candidate := &store.Agent{
		ID:              tid("wake-cap-candidate"),
		Slug:            "wake-cap-candidate",
		Name:            "Wake Cap Candidate",
		ProjectID:       target.ProjectID,
		RuntimeBrokerID: target.RuntimeBrokerID,
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(context.Background(), candidate))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+candidate.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

// TestBrokerQuota_WakeReadinessTimeoutThenConfirmedStopReleases is the positive
// control for #1984: once the runtime broker actually confirms the container
// exited (reported over heartbeat, the normal "hub observes reality" path),
// the reservation must still be released.
func TestBrokerQuota_WakeReadinessTimeoutThenConfirmedStopReleases(t *testing.T) {
	srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	setBrokerAgentCeiling(t, s, 1)
	grantDevUserRuntimeBrokerAccess(t, s)

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, dmErr := srv.wakeAgentForDM(ctx, target)
	require.NotNil(t, dmErr)

	require.EqualValues(t, 1, brokerReservationCount(t, s, target.RuntimeBrokerID))

	// The broker later reports, via heartbeat, that the container actually
	// exited. This must drive reconcileBrokerQuotaOnPhaseChange and free the
	// slot the normal way.
	ec := 1
	code := sendHeartbeat(t, srv, target.RuntimeBrokerID, target.ProjectID, brokerAgentHeartbeat{
		Slug:       target.Slug,
		Phase:      "stopped",
		ExitCode:   &ec,
		ExitReason: "crashed",
	})
	require.Equal(t, http.StatusOK, code)

	assert.EqualValues(t, 0, brokerReservationCount(t, s, target.RuntimeBrokerID),
		"a confirmed container stop must free the slot")
}
