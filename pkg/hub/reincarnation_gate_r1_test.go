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

// Tests for the p2a-r1 review round-1 fixes (design agent-reincarnate §3.7,
// Amendment A25.1): R1 (deferred path must not skip notify/mentions), R2
// (a persist failure on the deferred path must not answer 202), R3 (every
// hub delivery path is gated, not just the three named in 2a.2), and O2
// (broadcasts keep the pre-existing rejection; agent senders on pub/sub get
// a deferred notice). All tests use the real SQLite store.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// R1: human-sender deferred path must still run notify + @mention fan-out
// ---------------------------------------------------------------------------

// setReincarnationState sets an agent's ReincarnationState via UpdateAgent.
// CreateAgent's ent mapping does not persist this field (see
// entadapter/agent_store.go: SetReincarnationState is wired only into
// UpdateAgent's mutation builder), so every test that needs a migrating
// agent must create it first and set this separately.
func setReincarnationState(t *testing.T, s store.Store, a *store.Agent, rs string) {
	t.Helper()
	a.ReincarnationState = rs
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

// setupR1TestAgents creates a project, a runtime broker, a migrating primary
// agent, and a running peer agent that can be @mentioned.
func setupR1TestAgents(t *testing.T, s store.Store) (projectID, primaryID, peerID string) {
	t.Helper()
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID: tid("r1-broker"), Name: "r1-broker", Slug: "r1-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	project := &store.Project{ID: tid("r1-project"), Slug: "r1-project", Name: "r1-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	primary := &store.Agent{
		ID: tid("r1-primary"), Slug: "r1-primary", Name: "r1-primary",
		ProjectID: project.ID, Phase: string(state.PhaseStopping),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, primary))
	setReincarnationState(t, s, primary, store.ReincarnationStateStopping)

	peer := &store.Agent{
		ID: tid("r1-peer"), Slug: "r1-peer", Name: "r1-peer",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, peer))

	return project.ID, primary.ID, peer.ID
}

// TestHandleAgentMessage_R1_DeferredStillRunsNotifyAndMentions is the R1
// (p2a-r1 review) regression test: before the fix, the deferred
// short-circuit in handleAgentMessage returned before the notify
// subscription and processMentions ran, silently dropping a mention to an
// unrelated, non-migrating peer and never creating the requested
// subscription.
func TestHandleAgentMessage_R1_DeferredStillRunsNotifyAndMentions(t *testing.T) {
	srv, s := testServer(t)
	projectID, primaryID, peerID := setupR1TestAgents(t, s)

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:r1-primary",
			Msg: "hey @r1-peer check this", Type: messages.TypeInstruction,
		},
		"mentions": []string{"r1-peer"},
		"notify":   true,
	}
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", primaryID), body)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "deferred", resp.Status)

	// The mention to the non-migrating peer must still be resolved and
	// reported to the sender.
	require.Len(t, resp.MentionResults, 1, "the deferred response must report the mention outcome")
	assert.Equal(t, "r1-peer", resp.MentionResults[0].Slug)
	assert.Equal(t, "delivered", resp.MentionResults[0].Status)

	// The peer (not the migrating primary) must have actually received a
	// dispatch for the mention.
	var sawPeerDispatch bool
	for _, d := range dispatcher.getMessages() {
		if d.agentSlug == "r1-peer" {
			sawPeerDispatch = true
		}
		assert.NotEqual(t, "r1-primary", d.agentSlug, "the migrating primary must never be dispatched to")
	}
	assert.True(t, sawPeerDispatch, "the mentioned, non-migrating peer must be dispatched to")

	// The notify subscription (for the primary's future status changes)
	// must exist even though this message was deferred.
	subs, err := s.GetNotificationSubscriptions(context.Background(), primaryID)
	require.NoError(t, err)
	require.Len(t, subs, 1, "a deferred send with notify:true must still create the subscription")

	_ = projectID
	_ = peerID
}

// ---------------------------------------------------------------------------
// R2: a persist failure on the deferred path must not answer 202
// ---------------------------------------------------------------------------

// TestHandleAgentMessage_R2_PersistFailureOnDeferredPathIsNot202 is the R2
// (p2a-r1 review) regression test: before the fix, a CreateMessage failure
// while reincarnating still fell into the `if reincarnating` 202 branch,
// telling the sender the message was "saved to history" when it was neither
// saved nor dispatched.
func TestHandleAgentMessage_R2_PersistFailureOnDeferredPathIsNot202(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseStopping))

	agent, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	agent.ReincarnationState = store.ReincarnationStateStopping
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	// Swap in a store whose CreateMessage always fails, simulating a
	// persistence failure for this specific request.
	srv.store = &createMessageFailStore{Store: s}

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello during migration", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), body)

	require.NotEqual(t, http.StatusAccepted, rec.Code,
		"a message that failed to persist must never be reported as 202 deferred; body: %s", rec.Body.String())
	assert.GreaterOrEqual(t, rec.Code, 500, "an unpersisted, undispatched message must be a server error")

	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Contains(t, errResp.Error.Message, "reincarnating")
}

// ---------------------------------------------------------------------------
// R3: every hub delivery path is gated
// ---------------------------------------------------------------------------

// TestHandleGroupMessage_R3_MigratingRecipientDeferred covers the group[]
// fan-out path (handleAgentMessage -> handleGroupMessage), reached from the
// same POST /agents/{id}/message endpoint before the three-path gate was
// computed.
func TestHandleGroupMessage_R3_MigratingRecipientDeferred(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("r3-group-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("r3-group-project"), Slug: "r3-group-project", Name: "r3-group-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	anchor := &store.Agent{ID: tid("r3-group-anchor"), Slug: "r3-group-anchor", Name: "r3-group-anchor", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, anchor))
	target := &store.Agent{
		ID: tid("r3-group-target"), Slug: "r3-group-target", Name: "r3-group-target",
		ProjectID: project.ID, Phase: string(state.PhaseProvisioning),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	setReincarnationState(t, s, target, store.ReincarnationStateProvisioning)
	// A second, non-migrating recipient — the group[] syntax expects a
	// multi-recipient set (matching TestDEF19_GroupRecipient_FullHandlerPath's
	// two-recipient form) and this also proves the migrating recipient's
	// deferral doesn't affect delivery to its non-migrating group peer.
	peer := &store.Agent{ID: tid("r3-group-peer"), Slug: "r3-group-peer", Name: "r3-group-peer", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, peer))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:test", SenderID: tid("r3-group-sender"),
			Recipient: "group[agent:r3-group-target,agent:r3-group-peer]",
			Msg:       "hello", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", anchor.ID), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp GroupMessageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Results, 2)
	var targetResult, peerResult *GroupMessageRecipientResult
	for i := range resp.Results {
		switch resp.Results[i].Recipient {
		case "agent:r3-group-target":
			targetResult = &resp.Results[i]
		case "agent:r3-group-peer":
			peerResult = &resp.Results[i]
		}
	}
	require.NotNil(t, targetResult)
	require.NotNil(t, peerResult)
	assert.Equal(t, "deferred", targetResult.Status,
		"a migrating group[] recipient must be reported as deferred, not delivered")
	assert.Equal(t, "delivered", peerResult.Status,
		"a non-migrating group[] recipient must be delivered normally")

	var sawTargetDispatch bool
	for _, d := range dispatcher.getMessages() {
		assert.NotEqual(t, "r3-group-target", d.agentSlug, "no dispatch call may be made to a migrating group[] recipient")
		if d.agentSlug == "r3-group-target" {
			sawTargetDispatch = true
		}
	}
	assert.False(t, sawTargetDispatch)

	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, store.MessageDispatchDeferred, rows.Items[0].DispatchState)
}

// TestSendAgentRouted_R3_MigratingPrimaryDuringProvisioningDeferred covers
// chat v2's sendAgentRouted: isAgentUnreachable only treats
// suspended/stopping/stopped/error as unreachable, so a primary mid-`scion
// reincarnate` during "provisioning" or "starting" (neither phase is in
// that set) previously fell through and dispatched normally.
func TestSendAgentRouted_R3_MigratingPrimaryDuringProvisioningDeferred(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)

	broker := &store.RuntimeBroker{ID: tid("r3-chatv2-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("r3-chatv2-project"), Slug: "r3-chatv2-project", Name: "r3-chatv2-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{
		ID: tid("r3-chatv2-primary"), Slug: "r3-chatv2-primary", Name: "r3-chatv2-primary",
		ProjectID: project.ID, Phase: string(state.PhaseProvisioning),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, primary))
	setReincarnationState(t, s, primary, store.ReincarnationStateProvisioning)

	userID := api.NewUUID()
	owner := NewAuthenticatedUser(userID, "r3-chatv2-owner@test.com", "Owner", "member", "cli")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "r3-chatv2-owner@test.com", DisplayName: "Owner"}))
	ensureHubMembership(ctx, s, userID)
	srv.seedProjectCreatorMembership(ctx, project)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, userID))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/conversations/topic:"+project.ID+"/messages", nil)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), owner))
	rr := httptest.NewRecorder()

	msgID := writeChatSendOutcome(rr)(srv.sendAgentRouted(req.Context(), "topic:"+project.ID, project.ID, owner,
		"hello", "Owner", []*store.Agent{primary}, nil, nil, nil, time.Now(), "", nil, chatSendOptions{}))

	require.NotEmpty(t, msgID, "the message must still be persisted; response: %d %s", rr.Code, rr.Body.String())
	require.Empty(t, dispatcher.getMessages(), "a migrating primary during provisioning must not be dispatched to")

	stored, err := s.GetMessage(ctx, msgID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDeferred, stored.DispatchState,
		"a migrating primary must be deferred, not dispatched, during provisioning")
}

// TestProcessMentions_R3_MigratingMentionedAgentDeferred covers
// processMentions: a mentioned agent (distinct from the primary recipient)
// that is itself migrating must be deferred, not dispatched.
func TestProcessMentions_R3_MigratingMentionedAgentDeferred(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("r3-mention-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("r3-mention-project"), Slug: "r3-mention-project", Name: "r3-mention-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	primary := &store.Agent{ID: tid("r3-mention-primary"), Slug: "r3-mention-primary", Name: "r3-mention-primary", ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID}
	require.NoError(t, s.CreateAgent(ctx, primary))
	mentioned := &store.Agent{
		ID: tid("r3-mention-target"), Slug: "r3-mention-target", Name: "r3-mention-target",
		ProjectID: project.ID, Phase: string(state.PhaseStarting),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, mentioned))
	setReincarnationState(t, s, mentioned, store.ReincarnationStateStarting)

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	// An unscoped local admin identity pierces per-mention authorization
	// (D6 super-admin bypass), so this test can focus on the migration gate
	// rather than message-mode setup for two more agents.
	admin := NewAuthenticatedUser(tid("r3-mention-admin"), "r3-mention-admin@test.com", "Admin", "admin", "cli")
	mentionCtx := contextWithIdentity(ctx, admin)

	originalMsg := messages.NewInstruction("user:tester", "agent:r3-mention-primary", "hey @r3-mention-target")
	originalMsg.SenderID = tid("r3-mention-user")

	results := srv.processMentions(mentionCtx, []string{"r3-mention-target"}, primary, originalMsg, "", "")
	require.Len(t, results, 1)
	assert.Equal(t, "deferred", results[0].Status)

	assert.Empty(t, dispatcher.getMessages(), "a migrating mentioned agent must not be dispatched to")

	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: mentioned.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1)
	assert.Equal(t, store.MessageDispatchDeferred, rows.Items[0].DispatchState)
}

// TestHandleBrokerInbound_R3_MigratingRecipientDeferred covers the legacy
// external-channel inbound endpoint, which dispatches before persisting
// (the reverse order of every other path in this package) and already has
// its own "reject non-running agents" 409 check (handlers_broker_inbound.go,
// "Reject messages to non-running agents") that fires for phases like
// stopping/provisioning/starting BEFORE the migration gate is ever reached.
// The gate's own reachable window is therefore the "pending" reincarnation
// state, where phase is still "running" — modeled here.
func TestHandleBrokerInbound_R3_MigratingRecipientDeferred(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("r3-inbound-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("r3-inbound-project"), Slug: "r3-inbound-project", Name: "r3-inbound-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	target := &store.Agent{
		ID: tid("r3-inbound-target"), Slug: "r3-inbound-target", Name: "r3-inbound-target",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
		RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	setReincarnationState(t, s, target, store.ReincarnationStatePending)

	senderEmail := "r3-inbound-sender@test.com"
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("r3-inbound-sender"), Email: senderEmail, DisplayName: "Sender",
		Role: "admin", Status: store.UserStatusActive,
	}))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	reqBody := inboundMessageRequest{
		Topic: fmt.Sprintf("scion.project.%s.agent.%s.messages", project.ID, target.Slug),
		Message: &messages.StructuredMessage{
			Sender: "user:" + senderEmail, Recipient: "agent:" + target.Slug,
			Msg: "hello from discord", Type: messages.TypeInstruction,
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(broker.ID)))
	rec := httptest.NewRecorder()

	srv.handleBrokerInbound(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, false, resp["delivered"], "a migrating recipient must not be reported as delivered")
	assert.Equal(t, "agent is reincarnating", resp["deferred"])

	assert.Empty(t, dispatcher.getMessages(), "no dispatch call may be made to a migrating recipient")

	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rows.Items, 1, "the message must still be persisted for catch-up")
	assert.Equal(t, store.MessageDispatchDeferred, rows.Items[0].DispatchState)
}

// TestMessageEventHandler_R3_MigratingTargetFailsLoudly covers the
// scheduler: a scheduled message firing while the target is mid-migration
// must fail (recording an error on the event), not silently dispatch into a
// stopped or absent container.
func TestMessageEventHandler_R3_MigratingTargetFailsLoudly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("r3-sched-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("r3-sched-project"), Slug: "r3-sched-project", Name: "r3-sched-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	// O-a (p2a-r2 review): the reincarnation check now runs AFTER fire-time
	// authorization, so the fixture needs a creator that is actually
	// authorized to message the target — otherwise the test would pass for
	// the wrong reason (an authz denial, not the migration gate).
	creator := &store.Agent{
		ID: tid("r3-sched-creator"), Slug: "r3-sched-creator", Name: "r3-sched-creator",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID,
		MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, creator))
	target := &store.Agent{
		ID: tid("r3-sched-target"), Slug: "r3-sched-target", Name: "r3-sched-target",
		ProjectID: project.ID, Phase: string(state.PhaseStarting),
		RuntimeBrokerID: broker.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	setReincarnationState(t, s, target, store.ReincarnationStateStarting)

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	payload, err := json.Marshal(MessageEventPayload{AgentID: target.ID, Message: "wake up"})
	require.NoError(t, err)
	evt := withAgentRevision(t, srv, store.ScheduledEvent{
		ID: tid("r3-sched-event"), ProjectID: project.ID, EventType: "message", Payload: string(payload),
		CreatedBy: creator.ID,
	}, creator.ID)

	handler := srv.messageEventHandler()
	err = handler(ctx, evt)
	require.Error(t, err, "a scheduled message to a migrating agent must fail, not silently succeed")
	assert.Contains(t, err.Error(), "reincarnating")
	assert.Empty(t, dispatcher.getMessages())
}

// TestMessageEventHandler_OA_AuthzDenialPrecedesReincarnationCheck is the
// O-a (p2a-r2 review) regression test: a denied creator must see the authz
// denial, not "target agent is reincarnating" — the same invariant
// deliverToAgent already states explicitly ("Runs after reauthorization so
// a denied sender learns nothing about the recipient's phase"). Before the
// fix, the reincarnation check ran first and would leak the recipient's
// migration state to a creator who should learn nothing about the target.
func TestMessageEventHandler_OA_AuthzDenialPrecedesReincarnationCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{ID: tid("oa-sched-broker"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("oa-sched-project"), Slug: "oa-sched-project", Name: "oa-sched-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	target := &store.Agent{
		ID: tid("oa-sched-target"), Slug: "oa-sched-target", Name: "oa-sched-target",
		ProjectID: project.ID, Phase: string(state.PhaseStarting),
		RuntimeBrokerID: broker.ID, MessageMode: store.MessageModeNone,
	}
	require.NoError(t, s.CreateAgent(ctx, target))
	setReincarnationState(t, s, target, store.ReincarnationStateStarting)

	// A creator with no path to message a message_mode=none target: denied
	// regardless of the target's migration state.
	creator := &store.Agent{
		ID: tid("oa-sched-creator"), Slug: "oa-sched-creator", Name: "oa-sched-creator",
		ProjectID: project.ID, Phase: "running", RuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateAgent(ctx, creator))

	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	payload, err := json.Marshal(MessageEventPayload{AgentID: target.ID, Message: "wake up"})
	require.NoError(t, err)
	evt := withAgentRevision(t, srv, store.ScheduledEvent{
		ID: tid("oa-sched-event"), ProjectID: project.ID, EventType: "message", Payload: string(payload),
		CreatedBy: creator.ID,
	}, creator.ID)

	handler := srv.messageEventHandler()
	err = handler(ctx, evt)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "reincarnating",
		"a denied creator must not learn that the target is mid-migration")
	assert.Contains(t, err.Error(), "scheduled_message_denied")
	assert.Empty(t, dispatcher.getMessages())
}

// ---------------------------------------------------------------------------
// O2: broadcasts keep the pre-existing rejection; agent senders on pub/sub
// get a deferred notice
// ---------------------------------------------------------------------------

// TestDeliverToAgent_O2_BroadcastToMigratingAgentKeepsRejection covers the
// broadcast carve-out: a broadcast has no conversation, so a deferred
// broadcast could never be found by catch-up. Broadcasts to a migrating
// agent keep the pre-existing #1820 rejection (drop, no persistence)
// instead of being deferred.
func TestDeliverToAgent_O2_BroadcastToMigratingAgentKeepsRejection(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	target := setupBrokerTestAgent(t, s, projectID, "o2-broadcast-target", string(state.PhaseStopping))
	target.ReincarnationState = store.ReincarnationStateStopping
	require.NoError(t, s.UpdateAgent(context.Background(), target))

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())

	msg := messages.NewInstruction("agent:someone", "agent:o2-broadcast-target", "broadcast hello")
	msg.SenderID = tid("o2-broadcast-sender")
	msg.RecipientID = target.ID
	msg.Broadcasted = true
	proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

	assert.Empty(t, dispatcher.getMessages(), "no dispatch to the migrating target")
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: target.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "a broadcast to a migrating agent must keep the #1820 no-persist rejection, not be deferred")
}

// TestDeliverToAgent_O2_AgentSenderGetsDeferredNotice covers the pub/sub
// deferred-notice: an agent sender whose message to a migrating recipient
// was deferred must be told, mirroring publishDeliveryFailed for an
// unreachable recipient.
func TestDeliverToAgent_O2_AgentSenderGetsDeferredNotice(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "o2-notice-sender", "running")
	target := setupBrokerTestAgent(t, s, projectID, "o2-notice-target", string(state.PhaseStopping))
	target.ReincarnationState = store.ReincarnationStateStopping
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

	var notice *brokerDispatchedMsg
	for _, d := range dispatcher.getMessages() {
		if d.agentSlug == sender.Slug {
			d := d
			notice = &d
		}
	}
	require.NotNil(t, notice, "the agent sender must receive a DELIVERY_DEFERRED notice")
	assert.Equal(t, "DELIVERY_DEFERRED", notice.structured.Status)
	assert.Contains(t, notice.msg, "reincarnating")
	// O-c (p2a-r2 review): system_category must be present and distinct
	// from delivery-failed, so mapSystemCategory (pkg/messaging) does not
	// fall to its default/WARN branch for this notice.
	require.NotNil(t, notice.structured.Metadata)
	assert.Equal(t, "delivery-deferred", notice.structured.Metadata["system_category"])
}
