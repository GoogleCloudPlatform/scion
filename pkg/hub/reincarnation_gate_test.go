//go:build !no_sqlite

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

package hub

// Tests for the migration gate (design agent-reincarnate §3.7, Amendment
// A25 2a.2): while agents.reincarnation_state is non-terminal, all three
// hub delivery paths — handleAgentMessage (human sender), ExecuteAgentDM
// (agent-to-agent DM), and MessageBrokerProxy.deliverToAgent (pub/sub) —
// must persist the message (so it is visible on catch-up) and must NOT
// dispatch it. The sender gets a 202 with a "deferred" field. The gate
// turns off once reincarnation_state clears, whether by completion ("")
// or by failure ("failed") — reincarnationInFlight (reincarnate_worker.go)
// is the single shared predicate every path below uses.
//
// All tests use the real SQLite store (testServer / newBrokerTestStore),
// not a mock of the gate.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Path 1: MessageBrokerProxy.deliverToAgent (pub/sub, conversation, group)
// ---------------------------------------------------------------------------

func TestDeliverToAgent_Reincarnating_PersistsDeferred_NoDispatch(t *testing.T) {
	for _, rs := range []string{
		store.ReincarnationStatePending,
		store.ReincarnationStateStopping,
		store.ReincarnationStateProvisioning,
		store.ReincarnationStateStarting,
	} {
		t.Run(rs, func(t *testing.T) {
			s := newBrokerTestStore(t)
			projectID := setupBrokerTestProject(t, s)
			sender := setupBrokerTestAgent(t, s, projectID, "gate-sender-"+rs, "running")
			target := setupBrokerTestAgent(t, s, projectID, "gate-target-"+rs, string(state.PhaseStopping))
			target.ReincarnationState = rs
			require.NoError(t, s.UpdateAgent(context.Background(), target))

			events := NewChannelEventPublisher()
			defer events.Close()
			b := eventbus.NewInProcessEventBus(slog.Default())
			defer func() { _ = b.Close() }()
			dispatcher := &brokerMockDispatcher{}
			proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

			msg := messages.NewInstruction("agent:"+sender.Slug, "agent:"+target.Slug, "hello during migration")
			msg.SenderID = sender.ID
			msg.RecipientID = target.ID
			proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

			// Not dispatched to the target (no DispatchAgentMessage call
			// naming it), and no DELIVERY_FAILED notice either — deferred is
			// not a failure.
			for _, d := range dispatcher.getMessages() {
				assert.NotEqual(t, target.Slug, d.agentSlug,
					"a migrating agent (state=%s) must not receive a dispatch", rs)
			}

			// The persisted row is present in history with DispatchState
			// "deferred" — this is the row `scion conversation catch-up`
			// will show the new generation.
			rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
			require.NoError(t, err)
			require.Len(t, rows.Items, 1, "the message must be persisted to history even though it is not dispatched")
			assert.Equal(t, "hello during migration", rows.Items[0].Msg)
			assert.Equal(t, store.MessageDispatchDeferred, rows.Items[0].DispatchState,
				"a deferred row must be distinguishable from pending/dispatched/failed")
		})
	}
}

func TestDeliverToAgent_ReincarnationFailed_DispatchesNormally(t *testing.T) {
	// The gate turns off after a failed migration: reincarnationInFlight
	// treats "failed" as terminal (reincarnate_worker.go), so a "failed"
	// agent that is otherwise running is delivered to normally.
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "gate-sender-failed", "running")
	target := setupBrokerTestAgent(t, s, projectID, "gate-target-failed", "running")
	target.ReincarnationState = store.ReincarnationStateFailed
	require.NoError(t, s.UpdateAgent(context.Background(), target))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("agent:"+sender.Slug, "agent:"+target.Slug, "hello after failed migration")
	msg.SenderID = sender.ID
	msg.RecipientID = target.ID
	proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

	dispatched := dispatcher.getMessages()
	var sawTarget bool
	for _, d := range dispatched {
		if d.agentSlug == target.Slug {
			sawTarget = true
		}
	}
	assert.True(t, sawTarget, "a 'failed' reincarnation state must not gate delivery — the migration ended")

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, store.MessageDispatchDispatched, rows.Items[0].DispatchState)
}

func TestDeliverToAgent_ReincarnationNone_DispatchesNormally(t *testing.T) {
	// The gate turns off after a successful migration: reincarnation_state
	// clears to "" (store.ReincarnationStateNone) on completion.
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "gate-sender-none", "running")
	target := setupBrokerTestAgent(t, s, projectID, "gate-target-none", "running")
	require.Equal(t, store.ReincarnationStateNone, target.ReincarnationState, "sanity: default state is empty")

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("agent:"+sender.Slug, "agent:"+target.Slug, "hello after completed migration")
	msg.SenderID = sender.ID
	msg.RecipientID = target.ID
	proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

	dispatched := dispatcher.getMessages()
	var sawTarget bool
	for _, d := range dispatched {
		if d.agentSlug == target.Slug {
			sawTarget = true
		}
	}
	assert.True(t, sawTarget, "an empty reincarnation state must not gate delivery")

	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, store.MessageDispatchDispatched, rows.Items[0].DispatchState)
}

// ---------------------------------------------------------------------------
// Path 2: ExecuteAgentDM (agent-to-agent DM)
// ---------------------------------------------------------------------------

func TestExecuteAgentDM_ReincarnatingTarget_Defers(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	target.ReincarnationState = store.ReincarnationStateStarting
	target.Phase = string(state.PhaseStarting)
	require.NoError(t, s.UpdateAgent(ctx, target))

	input := deliveryDMInput(sender, target, "hello during migration")
	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "a migrating target must not be reported as an ordinary phase-conflict error")
	require.NotNil(t, result)
	assert.Equal(t, AgentDMDeferred, result.Outcome)
	require.NotEmpty(t, result.MessageID)

	assert.Empty(t, dispatcher.calls, "no dispatch call may be made to a migrating target")

	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDeferred, msg.DispatchState)
	assert.Equal(t, "hello during migration", msg.Msg, "the row must be present in history")
}

func TestExecuteAgentDM_ReincarnatingTarget_WakeRequested_StillDefers(t *testing.T) {
	// Without the gate, Wake:true against a "starting" (non-suspended)
	// target would hit wakeAgentForDM's default case and fail with a 400
	// ("Agent is not yet running"). The gate must short-circuit before that.
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	target.ReincarnationState = store.ReincarnationStateProvisioning
	target.Phase = string(state.PhaseProvisioning)
	require.NoError(t, s.UpdateAgent(ctx, target))

	input := deliveryDMInput(sender, target, "hello with wake")
	input.Wake = true
	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr, "wake must be skipped for a migrating target, not attempted and failed")
	require.NotNil(t, result)
	assert.Equal(t, AgentDMDeferred, result.Outcome)
	assert.Empty(t, dispatcher.calls)
}

func TestExecuteAgentDM_ReincarnationFailed_NotGated(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	target.ReincarnationState = store.ReincarnationStateFailed
	// Phase is already "running" from deliverySetup.
	require.NoError(t, s.UpdateAgent(ctx, target))

	input := deliveryDMInput(sender, target, "hello after failed migration")
	result, dmErr := srv.ExecuteAgentDM(ctx, input)
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, AgentDMAccepted, result.Outcome, "a failed migration must not gate future delivery")
	require.Len(t, dispatcher.calls, 1)

	msg, err := s.GetMessage(ctx, result.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, msg.DispatchState)
}

// ---------------------------------------------------------------------------
// Path 3: handleAgentMessage — human sender
// ---------------------------------------------------------------------------

func TestHandleAgentMessage_HumanSender_ReincarnatingAgent_Returns202Deferred(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseStopping))

	agent, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	agent.ReincarnationState = store.ReincarnationStateStopping
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello during migration", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), body)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "deferred", resp.Status)
	assert.Equal(t, "agent is reincarnating", resp.Deferred)
	require.NotEmpty(t, resp.MessageID)

	assert.Empty(t, dispatcher.getMessages(), "no dispatch call may be made to a migrating agent")

	msg, err := s.GetMessage(context.Background(), resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDeferred, msg.DispatchState)
	assert.Equal(t, "hello during migration", msg.Msg, "the row must be present in history for catch-up")
}

func TestHandleAgentMessage_HumanSender_ReincarnatingAgent_WakeRequested_StillDefers(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseProvisioning))

	agent, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	agent.ReincarnationState = store.ReincarnationStateProvisioning
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello with wake", Type: messages.TypeInstruction,
		},
		"wake": true,
	}
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), body)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "deferred", resp.Status)
	assert.Empty(t, dispatcher.getMessages(), "wake must be skipped, not attempted, for a migrating agent")
}

func TestHandleAgentMessage_HumanSender_ReincarnationFailed_OrdinaryPhaseCheckApplies(t *testing.T) {
	// Boundary check: a "failed" reincarnation state must not accidentally
	// bypass the ordinary (unrelated, non-goal) phase-conflict 409 for an
	// agent that is ALSO genuinely stopped. The migration gate only changes
	// behavior while reincarnationInFlight is true.
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseStopped))

	agent, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	agent.ReincarnationState = store.ReincarnationStateFailed
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), body)

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeAgentNotRunning, errResp.Error.Code)
}

// ---------------------------------------------------------------------------
// Path 2 via HTTP: handleAgentMessage — agent sender (forks to ExecuteAgentDM)
// ---------------------------------------------------------------------------

func TestHandleAgentMessage_AgentSender_ReincarnatingTarget_Returns202Deferred(t *testing.T) {
	srv, s, project, sender, target, _, dispatcher := deliverySetup(t)
	ctx := context.Background()

	target.ReincarnationState = store.ReincarnationStateStarting
	target.Phase = string(state.PhaseStarting)
	require.NoError(t, s.UpdateAgent(ctx, target))

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "agent:" + sender.Slug, Recipient: "agent:" + target.Slug,
			Msg: "hello from a peer agent", Type: messages.TypeInstruction,
		},
	}

	// reincarnateRequest is a generic authenticated-request builder (body +
	// identity in context); the URL path it stamps is irrelevant here since
	// handleAgentMessage is invoked directly, bypassing the router.
	req := reincarnateRequest(t, target.ID, agentIdentityFor(sender.ID, project.ID), body)
	rec := httptest.NewRecorder()
	srv.handleAgentMessage(rec, req, target.ID)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "deferred", resp.Status)
	assert.Equal(t, "agent is reincarnating", resp.Deferred)

	assert.Empty(t, dispatcher.calls, "no dispatch call may be made to a migrating target")

	msg, err := s.GetMessage(ctx, resp.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDeferred, msg.DispatchState)
}
