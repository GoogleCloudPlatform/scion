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

// The hold check applies to every send to an agent (ptone/scion#6859): a
// user send and an agent send, with or without wake, to a held target is
// refused with the held-target refusal (409 agent_not_running) before any
// wake, dispatch or write; the same send to a target that is not held still
// goes through.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldSendFixture is a sender and target (in targetPhase) with a dispatcher
// that records starts and message dispatches; the target is held when held
// is set.
func heldSendFixture(t *testing.T, targetPhase string, held bool) (*Server, store.Store, *store.Agent, *store.Agent, *wakeTrackingDispatcher) {
	t.Helper()
	srv, s, sender, target := createWakeDMFixtures(t, targetPhase)
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	if held {
		holdAgentFor(t, s, target.ID, target.ProjectID, target.Ancestry[0])
	}
	return srv, s, sender, target, disp
}

// assertNothingHappened checks that a refused send resumed, dispatched and
// stored nothing, and left the target's phase alone.
func assertNothingHappened(t *testing.T, s store.Store, target *store.Agent, disp *wakeTrackingDispatcher) {
	t.Helper()
	assert.Empty(t, disp.getStartCalls(), "no resume may be dispatched")
	assert.Empty(t, disp.getMessageCalls(), "no message may be dispatched")
	assert.Zero(t, storedMessagesFor(t, s, target.ID), "no message may be stored")
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, target.Phase, got.Phase)
}

func TestHeldTargetSend_User(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		wake  bool
	}{
		{"noWakeRunning", string(state.PhaseRunning), false},
		{"wakeRunning", string(state.PhaseRunning), true},
		// Before ptone/scion#6859 this reached the wake and was refused by
		// the start gate as a 409 "conflict".
		{"wakeSuspended", string(state.PhaseSuspended), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, target, disp := heldSendFixture(t, tc.phase, true)
			u := wakeLifecycleUser(t, s, "held-user-"+tc.name, target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")

			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
				map[string]interface{}{"message": "to held", "wake": tc.wake})

			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			apiErr := decodeTargetAPIError(t, rec)
			assert.Equal(t, ErrCodeAgentNotRunning, apiErr.Code)
			assert.Equal(t, heldTargetDMError(target).Message, apiErr.Message)
			assertNothingHappened(t, s, target, disp)
		})
	}

	// Control: the same sends to a running target that is not held go
	// through.
	for _, wake := range []bool{false, true} {
		srv, s, _, target, disp := heldSendFixture(t, string(state.PhaseRunning), false)
		u := wakeLifecycleUser(t, s, "unheld-user", target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")
		rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
			map[string]interface{}{"message": "to unheld", "wake": wake})
		require.Equal(t, http.StatusOK, rec.Code, "wake=%v: %s", wake, rec.Body.String())
		require.Len(t, disp.getMessageCalls(), 1, "wake=%v", wake)
		assert.Equal(t, 1, storedMessagesFor(t, s, target.ID), "wake=%v", wake)
	}
}

func TestHeldTargetSend_Agent(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		wake  bool
	}{
		{"noWakeRunning", string(state.PhaseRunning), false},
		// Before ptone/scion#6859 the hold check was skipped when wake was
		// requested: a held running target was dispatched to.
		{"wakeRunning", string(state.PhaseRunning), true},
		{"wakeSuspended", string(state.PhaseSuspended), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, sender, target, disp := heldSendFixture(t, tc.phase, true)
			wakeStarted := false
			result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent:    sender,
				SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
				TargetAgent:    target,
				Msg:            "to held",
				Type:           "instruction",
				ProjectID:      sender.ProjectID,
				Wake:           tc.wake,
				BeforeWake:     func() { wakeStarted = true },
			})

			assert.Nil(t, result)
			require.NotNil(t, dmErr)
			assert.Equal(t, heldTargetDMError(target), dmErr)
			assert.False(t, wakeStarted, "a held target is never woken")
			assertNothingHappened(t, s, target, disp)
		})
	}

	// Control: the same sends to a running target that is not held go
	// through.
	for _, wake := range []bool{false, true} {
		srv, _, sender, target, disp := heldSendFixture(t, string(state.PhaseRunning), false)
		result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
			SenderAgent:    sender,
			SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
			TargetAgent:    target,
			Msg:            "to unheld",
			Type:           "instruction",
			ProjectID:      sender.ProjectID,
			Wake:           wake,
		})
		require.Nil(t, dmErr, "wake=%v", wake)
		require.NotNil(t, result)
		assert.Equal(t, AgentDMAccepted, result.Outcome, "wake=%v", wake)
		require.Len(t, disp.getMessageCalls(), 1, "wake=%v", wake)
	}
}

// targetHoldFaultStore fails the hold lookup for one agent while its fault
// switch is armed. It is installed once, before any setup, through
// testServerWithStoreFault (store_fault_helpers_test.go), so the test arms
// the switch instead of reassigning srv.store.
type targetHoldFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
}

func (h *targetHoldFaultStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	if h.fault.Active() && agentID == h.agentID {
		return false, errors.New("injected hold lookup fault")
	}
	return h.Store.HasActiveAgentHold(ctx, agentID)
}

// newTargetHoldFaultStore is the wrap function for testServerWithStoreFault.
func newTargetHoldFaultStore(inner store.Store, fault *storeFaultSwitch) *targetHoldFaultStore {
	return &targetHoldFaultStore{Store: inner, fault: fault}
}

// A user send whose target's hold cannot be read is refused with the
// lookup-fault refusal (500), and nothing is resumed, dispatched or stored.
func TestHeldTargetSend_UserLookupFault(t *testing.T) {
	srv, s, _, a, dispatcher, faulty, sw := fanoutHoldFaultFixture(t, "user-fault", "user-fault-target")
	target := a["user-fault-target"]
	faulty.agentID = target.ID
	sw.Arm()

	for _, wake := range []bool{false, true} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
			map[string]interface{}{"message": "fault", "wake": wake})
		require.Equal(t, http.StatusInternalServerError, rec.Code, "wake=%v: %s", wake, rec.Body.String())
		apiErr := decodeTargetAPIError(t, rec)
		assert.Equal(t, heldLookupFaultDMError().Code, apiErr.Code, "wake=%v", wake)
		assert.Equal(t, heldLookupFaultDMError().Message, apiErr.Message, "wake=%v", wake)
	}
	assert.False(t, dispatchedTo(dispatcher, target.Slug), "no dispatch")
	assert.Zero(t, len(storedRowsFor(t, s, target.ID)), "nothing stored")
	// No resume: the target's phase is unchanged (brokerMockDispatcher does
	// not record start dispatches, so the stored phase is the check).
	got, err := s.GetAgent(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, target.Phase, got.Phase, "a lookup fault resumes nothing")
}

// routedHeldRequest is a chat-integration message from env's user to alpha,
// with msg as its text.
func routedHeldRequest(env routedTestEnv, msg string) routedInboundRequest {
	return routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + env.user.Email,
			Msg:       msg,
			Type:      messages.TypeInstruction,
		},
	}
}

// storedRowsFor returns the persisted messages for agentID.
func storedRowsFor(t *testing.T, s store.Store, agentID string) []store.Message {
	t.Helper()
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agentID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return rows.Items
}

// A routed inbound (chat integration) message to a held, running primary is
// refused like a stopped primary; nothing is dispatched or stored.
func TestHeldTargetSend_RoutedPrimary(t *testing.T) {
	env := setupRoutedTestEnv(t)
	holdAgentFor(t, env.store, env.agent1.ID, env.project.ID, env.user.ID)

	rec := env.doRoutedRequest(t, routedHeldRequest(env, "hello @beta"))

	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, ErrCodeAgentNotRunning, errResp.Error.Code)
	assert.Empty(t, env.dispatcher.getCalls(), "no dispatch to a held primary, and no fan-out")
	assert.Zero(t, len(storedRowsFor(t, env.store, env.agent1.ID)))
	assert.Zero(t, len(storedRowsFor(t, env.store, env.agent2.ID)))
}

// A routed inbound message that mentions a held, running agent is delivered
// to the primary only; the held mention is reported not_running.
func TestHeldTargetSend_RoutedMention(t *testing.T) {
	env := setupRoutedTestEnv(t)
	holdAgentFor(t, env.store, env.agent2.ID, env.project.ID, env.user.ID)

	rec := env.doRoutedRequest(t, routedHeldRequest(env, "hello @beta"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp routedInboundResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Results, 2)
	assert.Equal(t, "delivered", resp.Results[0].Status, "the primary is not held")
	assert.Equal(t, "beta", resp.Results[1].AgentSlug)
	assert.Equal(t, "not_running", resp.Results[1].Status, "the held mention is refused")
	assert.Equal(t, heldTargetDMError(env.agent2).Message, resp.Results[1].Error)
	calls := env.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, env.agent1.ID, calls[0].Agent.ID)
	assert.Zero(t, len(storedRowsFor(t, env.store, env.agent2.ID)))
}

// heldBrokerProxy is a message broker proxy over s with a recording
// dispatcher.
func heldBrokerProxy(t *testing.T, s store.Store) (*MessageBrokerProxy, *brokerMockDispatcher) {
	t.Helper()
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	b := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = b.Close() })
	dispatcher := &brokerMockDispatcher{}
	return NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default()), dispatcher
}

// dispatchedTo reports whether any recorded dispatch went to slug.
func dispatchedTo(d *brokerMockDispatcher, slug string) bool {
	for _, m := range d.getMessages() {
		if m.agentSlug == slug {
			return true
		}
	}
	return false
}

// A topic message through the message broker to a held, running agent is
// refused: not dispatched, not stored, and the agent sender is told.
func TestHeldTargetSend_BrokerTopicMessage(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "held-topic-sender", "running")
	target := setupBrokerTestAgent(t, s, projectID, "held-topic-target", "running")
	holdAgentFor(t, s, target.ID, projectID, tid("held-topic-root"))
	proxy, dispatcher := heldBrokerProxy(t, s)

	msg := messages.NewInstruction("agent:"+sender.Slug, "agent:"+target.Slug, "hello")
	msg.SenderID = sender.ID
	msg.RecipientID = target.ID
	proxy.deliverToAgent(context.Background(), projectID, target.Slug, msg)

	assert.False(t, dispatchedTo(dispatcher, target.Slug), "no dispatch to a held agent")
	assert.Zero(t, len(storedRowsFor(t, s, target.ID)), "nothing stored for a held agent")
	var notice *brokerDispatchedMsg
	for _, d := range dispatcher.getMessages() {
		if d.agentSlug == sender.Slug {
			d := d
			notice = &d
		}
	}
	require.NotNil(t, notice, "the agent sender is told")
	assert.Equal(t, "DELIVERY_FAILED", notice.structured.Status)

	// Control: the same message to a running agent that is not held is
	// delivered.
	other := setupBrokerTestAgent(t, s, projectID, "unheld-topic-target", "running")
	msg2 := messages.NewInstruction("agent:"+sender.Slug, "agent:"+other.Slug, "hello")
	msg2.SenderID = sender.ID
	msg2.RecipientID = other.ID
	proxy.deliverToAgent(context.Background(), projectID, other.Slug, msg2)
	assert.True(t, dispatchedTo(dispatcher, other.Slug))
	assert.Equal(t, 1, len(storedRowsFor(t, s, other.ID)))
}

// A project broadcast skips a held, running agent and still reaches its
// running, unheld sibling.
func TestHeldTargetSend_BrokerProjectBroadcast(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	held := setupBrokerTestAgent(t, s, projectID, "held-bcast-target", "running")
	sibling := setupBrokerTestAgent(t, s, projectID, "unheld-bcast-sibling", "running")
	holdAgentFor(t, s, held.ID, projectID, tid("held-bcast-root"))
	proxy, dispatcher := heldBrokerProxy(t, s)

	msg := messages.NewInstruction("user:alice", "project:all", "broadcast hello")
	msg.SenderID = tid("held-bcast-user")
	msg.Broadcasted = true
	proxy.fanOutToProject(context.Background(), projectID, msg)

	assert.False(t, dispatchedTo(dispatcher, held.Slug), "no broadcast dispatch to a held agent")
	assert.Zero(t, len(storedRowsFor(t, s, held.ID)), "nothing stored for a held agent")
	assert.True(t, dispatchedTo(dispatcher, sibling.Slug), "the unheld sibling still gets the broadcast")
	assert.Equal(t, 1, len(storedRowsFor(t, s, sibling.ID)))
}

// A routed inbound message whose primary's hold cannot be read is refused
// with the lookup-fault refusal (500), not a dispatch failure; nothing is
// dispatched or stored.
func TestHeldTargetSend_RoutedLookupFault(t *testing.T) {
	srv, s, faulty, sw := testServerWithStoreFault(t, newTargetHoldFaultStore)
	env := setupRoutedTestEnvOn(t, srv, s)
	faulty.agentID = env.agent1.ID
	sw.Arm()

	rec := env.doRoutedRequest(t, routedHeldRequest(env, "hello @beta"))

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp))
	assert.Equal(t, heldLookupFaultDMError().Code, errResp.Error.Code)
	assert.Equal(t, heldLookupFaultDMError().Message, errResp.Error.Message)
	assert.Empty(t, env.dispatcher.getCalls(), "no dispatch, and no fan-out")
	assert.Zero(t, len(storedRowsFor(t, env.store, env.agent1.ID)))
	assert.Zero(t, len(storedRowsFor(t, env.store, env.agent2.ID)))
}

// fanoutHoldFixture is setupGroupTest's project, runtime broker, project
// provider and running agents (named by slugs), with a recording
// dispatcher.
func fanoutHoldFixture(t *testing.T, name string, slugs ...string) (*Server, store.Store, string, map[string]*store.Agent, *brokerMockDispatcher) {
	t.Helper()
	srv, s := testServer(t)
	phases := map[string]string{}
	for _, slug := range slugs {
		phases[slug] = string(state.PhaseRunning)
	}
	projectID, agents := setupGroupTest(t, s, name, phases)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)
	return srv, s, projectID, agents, dispatcher
}

// fanoutHoldFaultFixture is fanoutHoldFixture on a server whose store is
// wrapped by a targetHoldFaultStore (disarmed) before any setup; the setup
// writes through the raw store.
func fanoutHoldFaultFixture(t *testing.T, name string, slugs ...string) (*Server, store.Store, string, map[string]*store.Agent, *brokerMockDispatcher, *targetHoldFaultStore, *storeFaultSwitch) {
	t.Helper()
	srv, s, faulty, sw := testServerWithStoreFault(t, newTargetHoldFaultStore)
	phases := map[string]string{}
	for _, slug := range slugs {
		phases[slug] = string(state.PhaseRunning)
	}
	projectID, agents := setupGroupTest(t, s, name, phases)
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)
	return srv, s, projectID, agents, dispatcher, faulty, sw
}

// groupResults sends a group[] message through anchor to the named agents
// and returns the per-recipient results by recipient.
func groupResults(t *testing.T, srv *Server, anchor *store.Agent, slugs ...string) map[string]GroupMessageRecipientResult {
	t.Helper()
	recipient := "group["
	for i, slug := range slugs {
		if i > 0 {
			recipient += ","
		}
		recipient += "agent:" + slug
	}
	recipient += "]"
	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender: "user:test", SenderID: tid("group-hold-sender"),
			Recipient: recipient, Msg: "hello", Type: messages.TypeInstruction,
		},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+anchor.ID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp GroupMessageResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	out := map[string]GroupMessageRecipientResult{}
	for _, r := range resp.Results {
		out[r.Recipient] = r
	}
	return out
}

// A group[] send does not deliver to a held member: nothing is stored or
// dispatched for it, and the unheld member still gets the message.
func TestHeldTargetSend_GroupMembers(t *testing.T) {
	srv, s, projectID, a, dispatcher := fanoutHoldFixture(t, "group-hold", "group-hold-anchor", "group-hold-held", "group-hold-peer")
	held, peer := a["group-hold-held"], a["group-hold-peer"]
	holdAgentFor(t, s, held.ID, projectID, tid("group-hold-root"))

	res := groupResults(t, srv, a["group-hold-anchor"], held.Slug, peer.Slug)

	require.Contains(t, res, "agent:"+held.Slug)
	assert.Equal(t, "failed", res["agent:"+held.Slug].Status)
	assert.Equal(t, heldTargetDMError(held).Message, res["agent:"+held.Slug].Error)
	assert.False(t, dispatchedTo(dispatcher, held.Slug), "no dispatch to a held member")
	assert.Zero(t, len(storedRowsFor(t, s, held.ID)), "nothing stored for a held member")
	assert.Equal(t, "delivered", res["agent:"+peer.Slug].Status, "the unheld member is delivered")
	assert.True(t, dispatchedTo(dispatcher, peer.Slug))
	assert.Equal(t, 1, len(storedRowsFor(t, s, peer.ID)))
}

// A group[] member whose hold cannot be read is refused; the other member
// still gets the message.
func TestHeldTargetSend_GroupMemberLookupFault(t *testing.T) {
	srv, s, _, a, dispatcher, faulty, sw := fanoutHoldFaultFixture(t, "group-fault", "group-fault-anchor", "group-fault-target", "group-fault-peer")
	target, peer := a["group-fault-target"], a["group-fault-peer"]
	faulty.agentID = target.ID
	sw.Arm()

	res := groupResults(t, srv, a["group-fault-anchor"], target.Slug, peer.Slug)

	assert.Equal(t, "failed", res["agent:"+target.Slug].Status)
	assert.Equal(t, heldLookupFaultDMError().Message, res["agent:"+target.Slug].Error)
	assert.False(t, dispatchedTo(dispatcher, target.Slug))
	assert.Zero(t, len(storedRowsFor(t, s, target.ID)))
	assert.Equal(t, "delivered", res["agent:"+peer.Slug].Status)
	assert.True(t, dispatchedTo(dispatcher, peer.Slug))
}

// The broker-less project broadcast (broadcastDirect) does not deliver to a
// held agent: nothing is stored or dispatched for it, the agent sender is
// told, and the unheld sibling still gets the broadcast. A hold lookup
// fault is refused the same way.
func TestHeldTargetSend_BroadcastFallback(t *testing.T) {
	srv, s, projectID, a, dispatcher, faulty, sw := fanoutHoldFaultFixture(t, "bcast-hold",
		"bcast-hold-sender", "bcast-hold-held", "bcast-hold-fault", "bcast-hold-sibling")
	sender, held, fault, sibling := a["bcast-hold-sender"], a["bcast-hold-held"], a["bcast-hold-fault"], a["bcast-hold-sibling"]
	holdAgentFor(t, s, held.ID, projectID, tid("bcast-hold-root"))
	faulty.agentID = fault.ID
	sw.Arm()

	msg := messages.NewInstruction("agent:"+sender.Slug, "project:all", "broadcast hello")
	msg.SenderID = sender.ID
	msg.Broadcasted = true
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/broadcast", nil)
	ok := srv.broadcastDirect(httptest.NewRecorder(), req, projectID, msg, false, []store.Agent{*held, *fault, *sibling})
	require.True(t, ok)

	for _, refused := range []*store.Agent{held, fault} {
		assert.False(t, dispatchedTo(dispatcher, refused.Slug), "no broadcast dispatch to %s", refused.Slug)
		assert.Zero(t, len(storedRowsFor(t, s, refused.ID)), "nothing stored for %s", refused.Slug)
	}
	assert.True(t, dispatchedTo(dispatcher, sibling.Slug), "the unheld sibling still gets the broadcast")
	assert.Equal(t, 1, len(storedRowsFor(t, s, sibling.ID)))
	notices := 0
	for _, d := range dispatcher.getMessages() {
		if d.agentSlug == sender.Slug && d.structured != nil && d.structured.Status == "DELIVERY_FAILED" {
			notices++
		}
	}
	assert.Equal(t, 2, notices, "the agent sender is told once per refused agent")
}

// A user send that mentions a held agent delivers to the primary and to the
// unheld mentioned agent; the held mention is refused with nothing stored
// or dispatched for it.
func TestHeldTargetSend_UserMentions(t *testing.T) {
	srv, s, projectID, a, dispatcher := fanoutHoldFixture(t, "mention-hold",
		"mention-hold-primary", "mention-hold-held", "mention-hold-peer")
	primary, held, peer := a["mention-hold-primary"], a["mention-hold-held"], a["mention-hold-peer"]
	holdAgentFor(t, s, held.ID, projectID, tid("mention-hold-root"))

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:" + primary.Slug,
			Msg: "hey @mention-hold-held and @mention-hold-peer", Type: messages.TypeInstruction,
		},
		"mentions": []string{held.Slug, peer.Slug},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+primary.ID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	assert.True(t, dispatchedTo(dispatcher, primary.Slug), "the primary send is delivered")
	byslug := map[string]messages.MentionResult{}
	for _, r := range resp.MentionResults {
		byslug[r.Slug] = r
	}
	require.Contains(t, byslug, held.Slug)
	assert.Equal(t, "error", byslug[held.Slug].Status)
	assert.Equal(t, heldTargetDMError(held).Message, byslug[held.Slug].Error)
	assert.False(t, dispatchedTo(dispatcher, held.Slug), "no dispatch to a held mentioned agent")
	assert.Zero(t, len(storedRowsFor(t, s, held.ID)), "nothing stored for a held mentioned agent")
	assert.Equal(t, "delivered", byslug[peer.Slug].Status)
	assert.True(t, dispatchedTo(dispatcher, peer.Slug))
	assert.Equal(t, 1, len(storedRowsFor(t, s, peer.ID)))
}

// A mentioned agent whose hold cannot be read is refused; the other mention
// is delivered.
func TestHeldTargetSend_MentionLookupFault(t *testing.T) {
	srv, s, _, a, dispatcher, faulty, sw := fanoutHoldFaultFixture(t, "mention-fault",
		"mention-fault-primary", "mention-fault-target", "mention-fault-peer")
	primary, target, peer := a["mention-fault-primary"], a["mention-fault-target"], a["mention-fault-peer"]
	faulty.agentID = target.ID
	sw.Arm()

	admin := NewAuthenticatedUser(tid("mention-fault-admin"), "mention-fault-admin@test.com", "Admin", "admin", "cli")
	original := messages.NewInstruction("user:tester", "agent:"+primary.Slug, "hey @mention-fault-target @mention-fault-peer")
	original.SenderID = tid("mention-fault-user")
	results := srv.processMentions(contextWithIdentity(context.Background(), admin), []string{target.Slug, peer.Slug}, primary, original, "", "")

	byslug := map[string]messages.MentionResult{}
	for _, r := range results {
		byslug[r.Slug] = r
	}
	assert.Equal(t, "error", byslug[target.Slug].Status)
	assert.Equal(t, heldLookupFaultDMError().Message, byslug[target.Slug].Error)
	assert.False(t, dispatchedTo(dispatcher, target.Slug))
	assert.Zero(t, len(storedRowsFor(t, s, target.ID)))
	assert.Equal(t, "delivered", byslug[peer.Slug].Status)
	assert.True(t, dispatchedTo(dispatcher, peer.Slug))
}

// A held group[] member that is reincarnating is saved for catch-up
// (status deferred, one deferred row, no dispatch), not refused.
func TestHeldTargetSend_GroupMemberReincarnating(t *testing.T) {
	srv, s, projectID, a, dispatcher := fanoutHoldFixture(t, "group-reinc", "group-reinc-anchor", "group-reinc-target", "group-reinc-peer")
	target, peer := a["group-reinc-target"], a["group-reinc-peer"]
	target.Phase = string(state.PhaseProvisioning)
	setReincarnationState(t, s, target, store.ReincarnationStateProvisioning)
	holdAgentFor(t, s, target.ID, projectID, tid("group-reinc-root"))

	res := groupResults(t, srv, a["group-reinc-anchor"], target.Slug, peer.Slug)

	assert.Equal(t, "deferred", res["agent:"+target.Slug].Status)
	assert.False(t, dispatchedTo(dispatcher, target.Slug), "no dispatch to a reincarnating member")
	rows := storedRowsFor(t, s, target.ID)
	require.Len(t, rows, 1, "the deferred member's message is saved for catch-up")
	assert.Equal(t, store.MessageDispatchDeferred, rows[0].DispatchState)
	assert.Equal(t, "delivered", res["agent:"+peer.Slug].Status)
}

// A held mentioned agent that is reincarnating is saved for catch-up on a
// user send (status deferred, one deferred row with its catch-up
// conversation, no dispatch), not refused.
func TestHeldTargetSend_MentionReincarnating(t *testing.T) {
	srv, s, projectID, a, dispatcher := fanoutHoldFixture(t, "mention-reinc", "mention-reinc-primary", "mention-reinc-target")
	primary, target := a["mention-reinc-primary"], a["mention-reinc-target"]
	target.Phase = string(state.PhaseStarting)
	setReincarnationState(t, s, target, store.ReincarnationStateStarting)
	holdAgentFor(t, s, target.ID, projectID, tid("mention-reinc-root"))

	body := map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:" + primary.Slug,
			Msg: "hey @mention-reinc-target", Type: messages.TypeInstruction,
		},
		"mentions": []string{target.Slug},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+primary.ID+"/message", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Len(t, resp.MentionResults, 1)
	assert.Equal(t, "deferred", resp.MentionResults[0].Status, resp.MentionResults[0].Error)
	assert.False(t, dispatchedTo(dispatcher, target.Slug), "no dispatch to a reincarnating mentioned agent")
	rows := storedRowsFor(t, s, target.ID)
	require.Len(t, rows, 1, "the deferred mention is saved for catch-up")
	assert.Equal(t, store.MessageDispatchDeferred, rows[0].DispatchState)
	assert.NotEmpty(t, rows[0].ConversationID, "the deferred mention has its catch-up conversation")
	assert.True(t, dispatchedTo(dispatcher, primary.Slug), "the primary send is delivered")
}
