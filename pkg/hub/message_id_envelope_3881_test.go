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

// ptone/scion#3881: the agent-facing delivery envelope carries "message_id",
// the ID of the message row the hub stored for that delivery. Deliveries
// with no stored row (scheduler, notifications) omit the key.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envelopeMessageID3881 decodes the rendered envelope and returns its
// message_id value and whether the key is present at all.
func envelopeMessageID3881(t *testing.T, deliveryText string) (string, bool) {
	t.Helper()
	require.NotEmpty(t, deliveryText, "DeliveryText must be rendered (envelope switch ON)")
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(extractDEF169JSON(t, deliveryText)), &raw))
	v, ok := raw["message_id"]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// requireStoredFor3881 asserts that id names a stored message addressed to
// the given agent slug.
func requireStoredFor3881(t *testing.T, s store.Store, id, slug string) *store.Message {
	t.Helper()
	require.NotEmpty(t, id, "envelope for %s must carry message_id", slug)
	row, err := s.GetMessage(context.Background(), id)
	require.NoError(t, err, "message_id %q must name a stored message", id)
	assert.Equal(t, "agent:"+slug, row.Recipient)
	return row
}

// seed3881AgentProject creates a project with an online broker and running
// agents with the given slugs.
func seed3881AgentProject(t *testing.T, s store.Store, prefix string, slugs ...string) string {
	t.Helper()
	ctx := context.Background()
	projectID := tid(prefix + "-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: prefix + "-project", Slug: prefix + "-project"}))
	brokerID := tid(prefix + "-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: prefix + "-broker", Slug: prefix + "-broker", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: prefix + "-broker", Status: store.BrokerStatusOnline,
	}))
	for _, slug := range slugs {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: tid(slug), Name: slug, Slug: slug, ProjectID: projectID, RuntimeBrokerID: brokerID, Phase: "running",
		}))
	}
	_ = s.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Development User"})
	return projectID
}

func TestMessageID3881_DirectAgentMessage(t *testing.T) {
	srv, s := testServer(t)
	projectID := seed3881AgentProject(t, s, "msgid3881-direct", "msgid3881-direct-a")
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableReadSwitch(t, srv)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/msgid3881-direct-a/message",
		MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender: "user:dev", SenderID: DevUserID, Recipient: "agent:msgid3881-direct-a",
			Msg: "direct message", Type: messages.TypeInstruction,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	id, ok := envelopeMessageID3881(t, calls[0].StructuredMessage.DeliveryText)
	require.True(t, ok, "direct agent message envelope must carry message_id")
	assert.Equal(t, resp.MessageID, id, "envelope message_id must match the ID the send returned")
	requireStoredFor3881(t, s, id, "msgid3881-direct-a")
}

func TestMessageID3881_GroupFanOut(t *testing.T) {
	srv, s := testServer(t)
	projectID := seed3881AgentProject(t, s, "msgid3881-grp", "msgid3881-grp-a", "msgid3881-grp-b")
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableReadSwitch(t, srv)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/msgid3881-grp-a/message",
		MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender: "user:dev", SenderID: DevUserID,
			Recipient: "group[agent:msgid3881-grp-a,agent:msgid3881-grp-b]",
			Msg:       "group message", Type: messages.TypeInstruction,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls := dispatcher.getCalls()
	require.Len(t, calls, 2)
	seen := map[string]bool{}
	for _, c := range calls {
		id, ok := envelopeMessageID3881(t, c.StructuredMessage.DeliveryText)
		require.True(t, ok, "group envelope for %s must carry message_id", c.StructuredMessage.Recipient)
		row := requireStoredFor3881(t, s, id, c.StructuredMessage.Recipient[len("agent:"):])
		assert.NotEmpty(t, row.GroupID, "group fan-out row must carry the group ID")
		assert.False(t, seen[id], "each recipient gets its own stored message")
		seen[id] = true
	}
}

func TestMessageID3881_ChatRoutedToAgents(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	for _, slug := range []string{"msgid3881-chat-a", "msgid3881-chat-b"} {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: tid(slug), ProjectID: proj.ID, Name: slug, Slug: slug,
			Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}))
	}
	topicID := tid("msgid3881-chat-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "msgid3881-chat",
		CreatedBy: "dev", CreatedAt: time.Now().UTC()}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "@msgid3881-chat-a @msgid3881-chat-b please look"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	dispatched := dispatcher.getMessages()
	require.Len(t, dispatched, 2, "primary and mention fan-out")
	seen := map[string]bool{}
	for _, d := range dispatched {
		require.NotNil(t, d.structured)
		id, ok := envelopeMessageID3881(t, d.structured.DeliveryText)
		require.True(t, ok, "chat envelope for %s must carry message_id", d.agentSlug)
		requireStoredFor3881(t, s, id, d.agentSlug)
		assert.False(t, seen[id], "primary and mention deliveries are separate stored messages")
		seen[id] = true
	}
}

func TestMessageID3881_ChatReply(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	enableEnvelopeSwitch(t, srv, s)
	ctx := t.Context()
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)

	sender := &store.Agent{ID: tid("msgid3881-reply-sender"), ProjectID: proj.ID, Name: "Sender", Slug: "msgid3881-reply-sender",
		Phase: "idle", OwnerID: DevUserID, CreatedBy: DevUserID}
	require.NoError(t, s.CreateAgent(ctx, sender))
	topicID := tid("msgid3881-reply-topic")
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "msgid3881-reply",
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: sender.Slug}))
	setTopicConversationID(t, db, s, topicID, proj.ID)
	origMsgID := seedAgentMessage(t, s, proj, topicID, sender, "original")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": "replying", "reply_to_id": origMsgID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	dispatched := dispatcher.getMessages()
	require.Len(t, dispatched, 1)
	text := dispatched[0].structured.DeliveryText
	id, ok := envelopeMessageID3881(t, text)
	require.True(t, ok, "reply envelope must carry message_id")
	requireStoredFor3881(t, s, id, sender.Slug)
	assert.NotEqual(t, origMsgID, id, "message_id names the reply itself, not the replied-to message")

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(extractDEF169JSON(t, text)), &raw))
	assert.Equal(t, "reply", raw["type"])
	assert.Equal(t, origMsgID, raw["reply_to"], "reply_to keeps naming the replied-to message")
}

// Scheduled message deliveries store no message row, so the envelope omits
// message_id rather than inventing one.
func TestMessageID3881_SchedulerOmitsMessageID(t *testing.T) {
	srv, s := testServer(t)
	projectID := seed3881AgentProject(t, s, "msgid3881-sched", "msgid3881-sched-a")
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableReadSwitch(t, srv)

	payload, _ := json.Marshal(MessageEventPayload{AgentName: "msgid3881-sched-a", Message: "scheduled"})
	evt := withAgentRevision(t, srv, store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: projectID, EventType: "message", Payload: string(payload),
		Status: store.ScheduledEventPending, CreatedBy: tid("msgid3881-sched-a"),
	}, tid("msgid3881-sched-a"))
	require.NoError(t, srv.messageEventHandler()(context.Background(), evt))

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	_, ok := envelopeMessageID3881(t, calls[0].StructuredMessage.DeliveryText)
	assert.False(t, ok, "a delivery with no stored message must omit message_id")
}

// Status notifications store no message row either.
func TestMessageID3881_NotificationOmitsMessageID(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.writeDenyEnabled = func() bool { return true }
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")
	require.Eventually(t, func() bool { return len(env.dispatcher.getCalls()) == 1 }, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	_, ok := envelopeMessageID3881(t, calls[0].StructuredMessage.DeliveryText)
	assert.False(t, ok, "a notification has no stored message, so message_id is omitted")
}

// An agent sending to another agent goes through ExecuteAgentDM. The
// envelope's message_id, the response's message_id and the stored row agree.
func TestMessageID3881_AgentSenderDirect(t *testing.T) {
	srv, s, _, sender, target, _, dispatcher := deliverySetup(t)
	enableReadSwitch(t, srv)

	rr := sendViaStructured(t, srv, sender, target, "agent to agent")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.MessageID)

	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	id, ok := envelopeMessageID3881(t, calls[0].StructuredMessage.DeliveryText)
	require.True(t, ok, "agent-sender envelope must carry message_id")
	assert.Equal(t, resp.MessageID, id, "envelope message_id must match the ID the send returned")
	requireStoredFor3881(t, s, id, target.Slug)
}

// A broadcast recipient whose row could not be stored gets no message_id;
// the other recipient's envelope names its stored row.
func TestMessageID3881_BroadcastOmitsMessageIDWhenRowNotStored(t *testing.T) {
	srv, s, failing, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failCreateMessageForAgentStore {
		return &failCreateMessageForAgentStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	projectID := tid("msgid3881-bcast-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Name: "msgid3881-bcast", Slug: "msgid3881-bcast"}))
	stored := store.Agent{ID: tid("msgid3881-bcast-ok"), Name: "msgid3881-bcast-ok", Slug: "msgid3881-bcast-ok", ProjectID: projectID, Phase: "running"}
	unstored := store.Agent{ID: tid("msgid3881-bcast-fail"), Name: "msgid3881-bcast-fail", Slug: "msgid3881-bcast-fail", ProjectID: projectID, Phase: "running"}
	for _, a := range []store.Agent{stored, unstored} {
		a := a
		require.NoError(t, s.CreateAgent(ctx, &a))
	}
	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)
	enableReadSwitch(t, srv)

	failing.agentID = unstored.ID
	fault.Arm()

	msg := &messages.StructuredMessage{
		Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender: "user:dev", SenderID: DevUserID, Msg: "broadcast", Type: messages.TypeInstruction, Broadcasted: true,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/broadcast", nil)
	rr := httptest.NewRecorder()
	require.True(t, srv.broadcastDirect(rr, req, projectID, msg, false, []store.Agent{stored, unstored}))

	calls := dispatcher.getCalls()
	require.Len(t, calls, 2, "both recipients are still dispatched")
	for _, c := range calls {
		id, ok := envelopeMessageID3881(t, c.StructuredMessage.DeliveryText)
		switch c.StructuredMessage.Recipient {
		case "agent:" + stored.Slug:
			require.True(t, ok, "stored broadcast row must be named in the envelope")
			requireStoredFor3881(t, s, id, stored.Slug)
		case "agent:" + unstored.Slug:
			assert.False(t, ok, "no row was stored, so message_id must be absent (got %q)", id)
		default:
			t.Errorf("unexpected recipient %q", c.StructuredMessage.Recipient)
		}
	}
	rows, err := s.ListMessages(ctx, store.MessageFilter{AgentID: unstored.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, rows.Items, "the injected failure must leave no row for that recipient")
}
