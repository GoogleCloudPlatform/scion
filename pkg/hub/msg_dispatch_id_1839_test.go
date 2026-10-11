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

// Tests for ptone/scion#1839: every synchronous dispatch path that persists a
// per-agent row carries that row's ID to the broker (so post-acceptance
// failures can be reported back), and group[] members are phase-gated.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rowsByAgent returns every persisted message row for agentID.
func rowsByAgent(t *testing.T, s store.Store, agentID string) []store.Message {
	t.Helper()
	rows, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agentID}, store.ListOptions{})
	require.NoError(t, err)
	return rows.Items
}

// mockDispatchesTo returns the recorded dispatches to slug.
func mockDispatchesTo(d *brokerMockDispatcher, slug string) []brokerDispatchedMsg {
	var out []brokerDispatchedMsg
	for _, m := range d.getMessages() {
		if m.agentSlug == slug {
			out = append(out, m)
		}
	}
	return out
}

func TestHandleAgentMessage_HumanBrokerPathCarriesMessageID(t *testing.T) {
	srv, s := testServer(t)
	_, agentID := setupMessageTestAgent(t, s, string(state.PhaseRunning))
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/agents/%s/message", agentID), map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Recipient: "agent:msg-agent",
			Msg: "hello", Type: messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rows := rowsByAgent(t, s, agentID)
	require.Len(t, rows, 1)
	got := mockDispatchesTo(dispatcher, "msg-agent")
	require.Len(t, got, 1)
	assert.Equal(t, rows[0].ID, got[0].messageID,
		"human broker dispatch must carry the persisted message ID")
}

// setupGroupTest creates a project on an online broker plus one agent per
// (slug, phase) pair. Returns the project ID and agents keyed by slug.
func setupGroupTest(t *testing.T, s store.Store, prefix string, phases map[string]string) (string, map[string]*store.Agent) {
	t.Helper()
	ctx := context.Background()
	projectID := tid(prefix + "-project")
	brokerID := tid(prefix + "-broker")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: prefix + "-project", Slug: prefix + "-project"}))
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: prefix + "-broker", Slug: prefix + "-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: prefix + "-broker", Status: store.BrokerStatusOnline,
	}))
	_ = s.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Development User"})
	agents := map[string]*store.Agent{}
	for slug, phase := range phases {
		a := &store.Agent{
			ID: api.NewUUID(), Name: slug, Slug: slug, ProjectID: projectID,
			RuntimeBrokerID: brokerID, Phase: phase,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		agents[slug] = a
	}
	return projectID, agents
}

func TestGroupMessage_CarriesMessageIDAndGatesNonRunningMember(t *testing.T) {
	srv, s := testServer(t)
	projectID, agents := setupGroupTest(t, s, "1839-grp", map[string]string{
		"grp-running": string(state.PhaseRunning),
		"grp-stopped": string(state.PhaseStopped),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/grp-running/message",
		MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:1839",
			SenderID:  DevUserID,
			Recipient: "group[agent:grp-running,agent:grp-stopped]",
			Msg:       "group hello",
			Type:      messages.TypeInstruction,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp GroupMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Delivered)

	// Running member: dispatched with its own row's ID.
	runningRows := rowsByAgent(t, s, agents["grp-running"].ID)
	require.Len(t, runningRows, 1)
	got := mockDispatchesTo(dispatcher, "grp-running")
	require.Len(t, got, 1)
	assert.Equal(t, runningRows[0].ID, got[0].messageID,
		"group dispatch must carry the per-member persisted message ID")

	// Stopped member: never dispatched, row marked failed, failure result.
	assert.Empty(t, mockDispatchesTo(dispatcher, "grp-stopped"), "a stopped member must not be dispatched to")
	stoppedRows := rowsByAgent(t, s, agents["grp-stopped"].ID)
	require.Len(t, stoppedRows, 1)
	assert.Equal(t, store.MessageDispatchFailed, stoppedRows[0].DispatchState)
	require.NotNil(t, stoppedRows[0].DispatchFailureReason,
		"the gate reason must be persisted with the row (born failed, one write)")
	assert.Contains(t, *stoppedRows[0].DispatchFailureReason, "stopped")
	var stoppedResult *GroupMessageRecipientResult
	for i := range resp.Results {
		if resp.Results[i].Recipient == "agent:grp-stopped" {
			stoppedResult = &resp.Results[i]
		}
	}
	require.NotNil(t, stoppedResult)
	assert.Equal(t, "failed", stoppedResult.Status)
	assert.Contains(t, stoppedResult.Error, "stopped")
}

func TestBroadcastDirect_CarriesPerAgentMessageID(t *testing.T) {
	srv, s := testServer(t)
	projectID, agents := setupGroupTest(t, s, "1839-bcast", map[string]string{
		"bcast-a": string(state.PhaseRunning),
		"bcast-b": string(state.PhaseRunning),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/broadcast", projectID), map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Msg: "hello all", Type: messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	for slug, a := range agents {
		rows := rowsByAgent(t, s, a.ID)
		require.Len(t, rows, 1, slug)
		got := mockDispatchesTo(dispatcher, slug)
		require.Len(t, got, 1, slug)
		assert.Equal(t, rows[0].ID, got[0].messageID,
			"broadcast dispatch to %s must carry its persisted message ID", slug)
	}
}

func TestProcessMentions_CarriesMessageID(t *testing.T) {
	srv, s := testServer(t)
	_, agents := setupGroupTest(t, s, "1839-mention", map[string]string{
		"mention-primary": string(state.PhaseRunning),
		"mention-target":  string(state.PhaseRunning),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	primary := agents["mention-primary"]
	orig := &messages.StructuredMessage{
		Sender:    "user:tester",
		SenderID:  tid("user-tester"),
		Recipient: "agent:" + primary.Slug,
		Msg:       "hello @mention-target",
		Type:      messages.TypeInstruction,
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	ctx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(tid("user-tester"), "tester@example.com", "Tester", "admin", "web"))
	results := srv.processMentions(ctx, []string{"mention-target"}, primary, orig, "", "")
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status, "%+v", results[0])

	rows := rowsByAgent(t, s, agents["mention-target"].ID)
	require.Len(t, rows, 1)
	got := mockDispatchesTo(dispatcher, "mention-target")
	require.Len(t, got, 1)
	assert.Equal(t, rows[0].ID, got[0].messageID, "mention dispatch must carry its persisted message ID")
}

// groupResult returns the per-recipient result for recipient, failing the
// test if it is absent.
func groupResult(t *testing.T, resp GroupMessageResponse, recipient string) GroupMessageRecipientResult {
	t.Helper()
	for _, r := range resp.Results {
		if r.Recipient == recipient {
			return r
		}
	}
	t.Fatalf("no result for %s in %+v", recipient, resp.Results)
	return GroupMessageRecipientResult{}
}

// sendGroup posts a group[] message from a user to recipients via the
// handler and returns the decoded response.
func sendGroup(t *testing.T, srv *Server, projectID, via string, recipients string) GroupMessageResponse {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/"+via+"/message",
		MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:1839",
			SenderID:  DevUserID,
			Recipient: recipients,
			Msg:       "group hello",
			Type:      messages.TypeInstruction,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp GroupMessageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// A suspended member gets a group-specific reason: group sends never wake,
// so the generic "use --wake" hint would point at a flag the group path
// does not accept.
func TestGroupMessage_SuspendedMemberGetsGroupSpecificReason(t *testing.T) {
	srv, s := testServer(t)
	projectID, agents := setupGroupTest(t, s, "1839-susp", map[string]string{
		"grp-running":   string(state.PhaseRunning),
		"grp-suspended": string(state.PhaseSuspended),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	resp := sendGroup(t, srv, projectID, "grp-running", "group[agent:grp-running,agent:grp-suspended]")
	assert.Equal(t, 1, resp.Delivered)

	res := groupResult(t, resp, "agent:grp-suspended")
	assert.Equal(t, "failed", res.Status)
	assert.Contains(t, res.Error, "group messages do not wake agents")
	assert.Contains(t, res.Error, "--wake")

	assert.Empty(t, mockDispatchesTo(dispatcher, "grp-suspended"))
	rows := rowsByAgent(t, s, agents["grp-suspended"].ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.MessageDispatchFailed, rows[0].DispatchState)
	require.NotNil(t, rows[0].DispatchFailureReason)
	assert.Equal(t, res.Error, *rows[0].DispatchFailureReason)

	// Direct sends keep the generic reason.
	assert.NotContains(t, validateAgentDeliverable(agents["grp-suspended"]).Message, "group messages")
}

// A running member that cannot be dispatched (no runtime broker, or no
// dispatcher at all) must not be left as a silent "dispatched" row.
func TestGroupMessage_UndispatchableMemberRowMarkedFailed(t *testing.T) {
	t.Run("no runtime broker", func(t *testing.T) {
		srv, s := testServer(t)
		projectID, agents := setupGroupTest(t, s, "1839-nobrk", map[string]string{
			"grp-running":  string(state.PhaseRunning),
			"grp-nobroker": string(state.PhaseRunning),
		})
		agents["grp-nobroker"].RuntimeBrokerID = ""
		require.NoError(t, s.UpdateAgent(context.Background(), agents["grp-nobroker"]))
		srv.SetDispatcher(&brokerMockDispatcher{})
		events := subscribeAgentMessageEvents(t, srv, agents["grp-nobroker"].ID)

		resp := sendGroup(t, srv, projectID, "grp-running", "group[agent:grp-running,agent:grp-nobroker]")
		res := groupResult(t, resp, "agent:grp-nobroker")
		assert.Equal(t, "failed", res.Status)
		assert.Equal(t, "agent has no runtime broker", res.Error)

		rows := rowsByAgent(t, s, agents["grp-nobroker"].ID)
		require.Len(t, rows, 1)
		assert.Equal(t, store.MessageDispatchFailed, rows[0].DispatchState)
		require.NotNil(t, rows[0].DispatchFailureReason)
		assert.Equal(t, "agent has no runtime broker", *rows[0].DispatchFailureReason)

		// The SSE event carries the true state: the row is born failed,
		// never published as "dispatched" first.
		evts := events()
		require.Len(t, evts, 1)
		assert.Equal(t, store.MessageDispatchFailed, evts[0].DispatchState)
		assert.Equal(t, "agent has no runtime broker", evts[0].DispatchFailureReason)
	})

	t.Run("no dispatcher", func(t *testing.T) {
		srv, s := testServer(t)
		projectID, agents := setupGroupTest(t, s, "1839-nodisp", map[string]string{
			"grp-a": string(state.PhaseRunning),
			"grp-b": string(state.PhaseRunning),
		})
		require.Nil(t, srv.GetDispatcher(), "test server must start without a dispatcher")
		events := map[string]func() []UserMessageEvent{}
		for _, slug := range []string{"grp-a", "grp-b"} {
			events[slug] = subscribeAgentMessageEvents(t, srv, agents[slug].ID)
		}

		resp := sendGroup(t, srv, projectID, "grp-a", "group[agent:grp-a,agent:grp-b]")
		assert.Equal(t, 0, resp.Delivered)
		for _, slug := range []string{"grp-a", "grp-b"} {
			res := groupResult(t, resp, "agent:"+slug)
			assert.Equal(t, "failed", res.Status)
			assert.Equal(t, "dispatcher not available", res.Error)
			rows := rowsByAgent(t, s, agents[slug].ID)
			require.Len(t, rows, 1)
			assert.Equal(t, store.MessageDispatchFailed, rows[0].DispatchState, slug)
			require.NotNil(t, rows[0].DispatchFailureReason)
			assert.Equal(t, "dispatcher not available", *rows[0].DispatchFailureReason)

			evts := events[slug]()
			require.Len(t, evts, 1, slug)
			assert.Equal(t, store.MessageDispatchFailed, evts[0].DispatchState, slug)
			assert.Equal(t, "dispatcher not available", evts[0].DispatchFailureReason, slug)
		}
	})
}

// subscribeAgentMessageEvents installs a fresh event publisher on srv and
// returns a func that drains the user.message events published for agentID.
// Publishing is a synchronous non-blocking send, so once the request has
// returned every event is already buffered.
func subscribeAgentMessageEvents(t *testing.T, srv *Server, agentID string) func() []UserMessageEvent {
	t.Helper()
	pub, ok := srv.events.(*ChannelEventPublisher)
	if !ok {
		pub = NewChannelEventPublisher()
		t.Cleanup(pub.Close)
		srv.SetEventPublisher(pub)
	}
	ch, unsub := pub.Subscribe("agent." + agentID + ".message")
	t.Cleanup(unsub)
	return func() []UserMessageEvent {
		var out []UserMessageEvent
		for {
			select {
			case e, ok := <-ch:
				if !ok { // publisher closed: a closed channel never blocks
					return out
				}
				var evt UserMessageEvent
				require.NoError(t, json.Unmarshal(e.Data, &evt))
				out = append(out, evt)
			default:
				return out
			}
		}
	}
}
