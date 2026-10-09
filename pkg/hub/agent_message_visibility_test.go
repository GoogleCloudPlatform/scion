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

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSEHandler_AgentMessagesFollowHistoryRule checks that user message
// events on agent.<id>.message and project.<id>.user.message follow the
// agent message history rule on the web events stream: a caller with attach
// on the agent sees every message, a reader without attach only messages it
// sent or received. Legacy agent:<slug> threads and DMs relayed from other
// channels follow the same rule.
func TestSSEHandler_AgentMessagesFollowHistoryRule(t *testing.T) {
	agentID, projectID := tid("sse-vis-agent"), tid("sse-vis-project")
	const owner, reader, other = "user-owner", "user-vis-reader", "user-vis-other"

	messages := []*store.Message{
		// Agent reply to the reader: agent and project subjects.
		{ID: "to-reader", ProjectID: projectID, AgentID: agentID, Channel: "web",
			Sender: "agent:a", SenderID: agentID, Recipient: "user:" + reader, RecipientID: reader},
		// Reader prompt to the agent: agent subject only.
		{ID: "from-reader", ProjectID: projectID, AgentID: agentID, Channel: "web",
			Sender: "user:" + reader, SenderID: reader, Recipient: "agent:a", RecipientID: agentID},
		// Agent reply to another user.
		{ID: "to-other", ProjectID: projectID, AgentID: agentID, Channel: "web",
			Sender: "agent:a", SenderID: agentID, Recipient: "user:" + other, RecipientID: other},
		// Legacy agent:<slug> thread with another user.
		{ID: "legacy-thread", ProjectID: projectID, AgentID: agentID, Channel: "web", ThreadID: "agent:a",
			Sender: "agent:a", SenderID: agentID, Recipient: "user:" + other, RecipientID: other},
		// DM with another user relayed from another channel.
		{ID: "relayed-dm", ProjectID: projectID, AgentID: agentID, Channel: "slack", ThreadID: "C123:1700000000.0001",
			Sender: "user:" + other, SenderID: other, Recipient: "agent:a", RecipientID: agentID},
		{ID: "relayed-reply", ProjectID: projectID, AgentID: agentID, Channel: "slack",
			Sender: "agent:a", SenderID: agentID, Recipient: "user:" + other, RecipientID: other},
		// Human to human row in the project: project subject only.
		{ID: "human-to-other", ProjectID: projectID, Channel: "web",
			Sender: "user:" + owner, SenderID: owner, Recipient: "user:" + other, RecipientID: other},
		{ID: "human-to-reader", ProjectID: projectID, Channel: "web",
			Sender: "user:" + other, SenderID: other, Recipient: "user:" + reader, RecipientID: reader},
	}

	agentSubjects := []string{"agent." + agentID + ".message", "agent." + agentID + ".>", "agent." + agentID + ".*"}
	projectSubjects := []string{"project." + projectID + ".user.message", "project." + projectID + ".>", "project.>"}

	for _, tc := range []struct {
		user     string
		subjects []string
		want     []string
	}{
		{reader, agentSubjects, []string{"to-reader", "from-reader"}},
		{reader, projectSubjects, []string{"to-reader", "human-to-reader"}},
		{owner, agentSubjects, []string{"to-reader", "from-reader", "to-other", "legacy-thread", "relayed-dm", "relayed-reply"}},
		{owner, projectSubjects, []string{"to-reader", "to-other", "legacy-thread", "relayed-reply", "human-to-other"}},
	} {
		for _, subject := range tc.subjects {
			t.Run(tc.user+"/"+subject, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ws, pub := newSSEVisibilityServer(projectID, agentID, reader)
					defer pub.Close()

					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					w := httptest.NewRecorder()
					done := make(chan struct{})
					go func() {
						defer close(done)
						ws.handleSSE(w, sseAgentRequest(ctx, tc.user, "user", subject))
					}()
					synctest.Wait()
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())

					for _, msg := range messages {
						msg.CreatedAt = time.Unix(0, 0)
						pub.PublishUserMessage(ctx, msg, nil)
					}
					synctest.Wait()
					cancel()
					<-done

					assert.Equal(t, tc.want, sseMessageIDs(t, w.Body.String()))
					assertNoSSESubscribers(t, pub)
				})
			})
		}
	}
}

func newSSEVisibilityServer(projectID, agentID, reader string) (*WebServer, *ChannelEventPublisher) {
	ms := &mockAuthzStore{
		projects: []store.Project{{ID: projectID, OwnerID: "user-owner"}},
		projectMemberships: map[string]*store.ProjectMembership{
			projectID + ":" + reader:  {ProjectID: projectID, UserID: reader, Role: store.ProjectRoleMember},
			projectID + ":user-owner": {ProjectID: projectID, UserID: "user-owner", Role: store.ProjectRoleMember},
		},
	}
	s := &sseAgentStore{mockAuthzStore: ms, agents: map[string]*store.Agent{
		agentID: {ID: agentID, ProjectID: projectID, OwnerID: "user-owner"},
	}}
	pub := NewChannelEventPublisher()
	return &WebServer{store: s, events: pub, authzService: NewAuthzService(s, slog.Default())}, pub
}

// TestSSEMessageViewer covers the per-connection attach cache: attach is
// asked once per agent, a transient lookup failure is asked again, and the
// cached answer stands for the rest of the connection.
func TestSSEMessageViewer(t *testing.T) {
	const me = "user-me"
	evt := func(subject, data string) Event { return Event{Subject: subject, Data: []byte(data)} }
	othersMsg := `{"agentId":"agent-1","senderId":"agent-1","recipientId":"user-x"}`

	t.Run("attach asked once per agent", func(t *testing.T) {
		calls := map[string]int{}
		v := &sseMessageViewer{userID: me, attach: map[string]bool{}, decideAttach: func(id string) (bool, bool) {
			calls[id]++
			return id == "agent-1", true
		}}
		for range 3 {
			assert.True(t, v.visible(evt("agent.agent-1.message", othersMsg)))
			assert.True(t, v.visible(evt("project.p.user.message", othersMsg)))
			assert.False(t, v.visible(evt("agent.agent-2.message", othersMsg)))
			// An empty agent id token is denied, not taken from the payload.
			assert.False(t, v.visible(evt("agent..message", othersMsg)))
		}
		assert.Equal(t, map[string]int{"agent-1": 1, "agent-2": 1}, calls)
	})

	t.Run("cached answer stands for the connection", func(t *testing.T) {
		allowed := true
		v := &sseMessageViewer{userID: me, attach: map[string]bool{}, decideAttach: func(string) (bool, bool) {
			return allowed, true
		}}
		assert.True(t, v.visible(evt("agent.agent-1.message", othersMsg)))
		allowed = false // revoked mid-connection: applies from the next connection
		assert.True(t, v.visible(evt("agent.agent-1.message", othersMsg)))
	})

	t.Run("uncacheable failure is asked again", func(t *testing.T) {
		calls := 0
		v := &sseMessageViewer{userID: me, attach: map[string]bool{}, decideAttach: func(string) (bool, bool) {
			calls++
			return false, false
		}}
		assert.False(t, v.visible(evt("agent.agent-1.message", othersMsg)))
		assert.False(t, v.visible(evt("agent.agent-1.message", othersMsg)))
		assert.Equal(t, 2, calls)
	})

	t.Run("participants and other events need no attach", func(t *testing.T) {
		v := &sseMessageViewer{userID: me, attach: map[string]bool{}, decideAttach: func(string) (bool, bool) {
			t.Fatal("attach asked")
			return false, false
		}}
		assert.True(t, v.visible(evt("agent.agent-1.message", `{"senderId":"user-me","recipientId":"agent-1"}`)))
		assert.True(t, v.visible(evt("project.p.user.message", `{"agentId":"agent-1","recipientId":"user-me"}`)))
		assert.True(t, v.visible(evt("agent.agent-1.status", `{}`)))
		assert.True(t, v.visible(evt("project.p.chat.message", othersMsg)))
		assert.True(t, v.visible(evt("user.user-me.message", othersMsg)))
		// No agent on the row: participants only.
		assert.False(t, v.visible(evt("project.p.user.message", `{"senderId":"user-a","recipientId":"user-b"}`)))
		assert.False(t, v.visible(evt("agent.agent-1.message", `not json`)))
	})

	t.Run("no session user sees no other messages", func(t *testing.T) {
		v := &sseMessageViewer{attach: map[string]bool{}}
		assert.False(t, v.visible(evt("agent.agent-1.message", `{"senderId":"","recipientId":""}`)))
	})
}

func TestSSEMessageViewer_DecideAttach(t *testing.T) {
	agentID, projectID := tid("sse-vis-decide-agent"), tid("sse-vis-decide-project")
	ws, pub := newSSEVisibilityServer(projectID, agentID, "user-vis-reader")
	defer pub.Close()
	ctx := context.Background()

	owner := ws.newSSEMessageViewer(sseAgentRequest(ctx, "user-owner", "user"))
	allowed, cacheable := owner.decideAttach(agentID)
	assert.True(t, allowed)
	assert.True(t, cacheable)

	reader := ws.newSSEMessageViewer(sseAgentRequest(ctx, "user-vis-reader", "user"))
	allowed, cacheable = reader.decideAttach(agentID)
	assert.False(t, allowed)
	assert.True(t, cacheable)

	allowed, cacheable = owner.decideAttach(tid("sse-vis-missing-agent"))
	assert.False(t, allowed)
	assert.True(t, cacheable, "a missing agent is cached")

	ws.store.(*sseAgentStore).lookupErr = errors.New("transient")
	allowed, cacheable = owner.decideAttach(agentID)
	assert.False(t, allowed)
	assert.False(t, cacheable, "a lookup failure is asked again")
}

// streamAgentMessages opens GET /agents/{id}/messages/stream as identity and
// calls publish until an event carrying "marker-message" arrives.
func streamAgentMessages(t *testing.T, srv *Server, identity UserIdentity, agentID string, publish func()) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID+"/messages/stream", nil)
	rctx := context.WithValue(contextWithIdentity(ctx, identity), userContextKey{}, identity)
	rec := &syncRecorder{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.mux.ServeHTTP(rec, req.WithContext(rctx))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(rec.String(), "marker-message") {
		publish()
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	body := rec.String()
	require.Contains(t, body, "marker-message", "stream never delivered the marker: %s", body)
	return body
}

// TestAgentMessagesStream_ConversationSettingOwnDM checks that with the
// conversation setting on, the agent messages stream matches REST's default
// path: attach holders and other readers alike see only their own DM
// conversation with the agent. Legacy agent:<slug> threads (a thread
// conversation) are left out; a DM relayed from another channel shows when
// it belongs to the caller's DM conversation.
func TestAgentMessagesStream_ConversationSettingOwnDM(t *testing.T) {
	srv, s, alice, bob, agentID := setupMessagePrivacyTest(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.events = events
	enableReadSwitch(t, srv)

	aliceConv := seedConversation(t, s, "native", makeDMKey(agentID, alice.ID), "direct")
	bobConv := seedConversation(t, s, "native", makeDMKey(agentID, bob.ID), "direct")
	carol := tid("stream-conv-carol")
	subject := "agent." + agentID + ".message"
	publish := func(markerTo, markerConv string) func() {
		return func() {
			for _, e := range []UserMessageEvent{
				{Msg: "alice-dm", SenderID: alice.ID, RecipientID: agentID, ConversationID: aliceConv},
				{Msg: "bob-dm", SenderID: agentID, RecipientID: bob.ID, ConversationID: bobConv},
				{Msg: "bob-relayed-dm", Channel: "slack", SenderID: bob.ID, RecipientID: agentID, ConversationID: bobConv},
				{Msg: "alice-legacy-thread", ThreadID: "agent:agent-msg-priv", SenderID: agentID, RecipientID: alice.ID, ConversationID: uuid.NewString()},
				{Msg: "carol-message", SenderID: carol, RecipientID: agentID, ConversationID: uuid.NewString()},
				{Msg: "alice-no-conversation", SenderID: agentID, RecipientID: alice.ID},
				{Msg: "marker-message", SenderID: agentID, RecipientID: markerTo, ConversationID: markerConv},
			} {
				e.ID = uuid.NewString()
				events.PublishRaw(subject, e)
			}
		}
	}

	t.Run("attach holder sees only its own DM", func(t *testing.T) {
		body := streamAgentMessages(t, srv, authUser(alice), agentID, publish(alice.ID, aliceConv))
		assert.Contains(t, body, "alice-dm")
		for _, hidden := range []string{"bob-dm", "bob-relayed-dm", "alice-legacy-thread", "carol-message", "alice-no-conversation"} {
			assert.NotContains(t, body, hidden)
		}
	})
	t.Run("reader without attach sees only its own DM", func(t *testing.T) {
		body := streamAgentMessages(t, srv, authUser(bob), agentID, publish(bob.ID, bobConv))
		assert.Contains(t, body, "bob-dm")
		assert.Contains(t, body, "bob-relayed-dm")
		for _, hidden := range []string{"alice-dm", "alice-legacy-thread", "carol-message", "alice-no-conversation"} {
			assert.NotContains(t, body, hidden)
		}
	})
}

// TestAgentStreamConversation covers the DM that does not exist when the
// stream connects: it is looked up again only for a message the caller takes
// part in, and kept once found.
func TestAgentStreamConversation(t *testing.T) {
	calls := 0
	current := ""
	c := &agentStreamConversation{enabled: true, resolve: func() (string, error) {
		calls++
		return current, nil
	}}
	assert.False(t, c.includes("conv-1", false), "not a participant: no lookup")
	assert.Equal(t, 0, calls)
	assert.False(t, c.includes("", true), "no conversation on the message: no lookup")
	assert.Equal(t, 0, calls)
	assert.False(t, c.includes("conv-1", true), "DM still absent")
	assert.Equal(t, 1, calls)

	current = "conv-1"
	assert.True(t, c.includes("conv-1", true), "DM created mid-connection")
	assert.Equal(t, 2, calls)
	assert.True(t, c.includes("conv-1", false))
	assert.False(t, c.includes("conv-2", true))
	assert.Equal(t, 2, calls)

	failing := &agentStreamConversation{enabled: true, resolve: func() (string, error) {
		return "", errors.New("lookup failed")
	}}
	assert.False(t, failing.includes("conv-1", true))
	assert.True(t, (&agentStreamConversation{}).includes("", false), "setting off: no filter")
}
