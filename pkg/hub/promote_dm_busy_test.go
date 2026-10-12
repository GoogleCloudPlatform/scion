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
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	_ "github.com/mattn/go-sqlite3"
)

// nc-promote-busy: "Promote to thread" always failed with "Agent is still
// responding" because deliverToUser — the MessageBrokerProxy path that
// persists every agent reply into a user's DM in a real deployment (wired
// up via Server.StartMessageBroker in cmd/server_foreground.go) — never set
// DispatchState on the row it created. The Ent schema's dispatch_state field
// defaults to "pending" (pkg/ent/schema/message.go), and nothing ever
// transitioned that row to "dispatched" afterward, so the very first agent
// reply in any DM left a permanently-pending row keyed by the DM's own
// thread_id. The promote handler's CountPendingMessages guard (added by
// #1256) counts exactly that column, so every promote attempt after an
// agent had replied even once hit the IN_FLIGHT_MESSAGES 409 forever after —
// not intermittently, since a DM without at least one agent reply has
// nothing to promote.

// setupPromoteBusyFixture creates a project, a running agent, and the
// webchat DM registry row needed to exercise the promote endpoint. It
// returns the server, the shared store, the webchat store, and the DM key.
func setupPromoteBusyFixture(t *testing.T) (*Server, store.Store, WebChatStore, *store.Agent, string) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: tid("promote-busy-proj"), Name: "promote-busy-proj", Slug: "promote-busy-proj",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agentID := tid("promote-busy-agent")
	agent := &store.Agent{
		ID:        agentID,
		ProjectID: proj.ID,
		Name:      "Promote Busy Bot",
		Slug:      "promote-busy-bot",
		Phase:     "idle",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	dbProvider, ok := s.(interface{ DB() *sql.DB })
	if !ok {
		t.Fatal("store does not expose DB()")
	}
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	if err := wcs.Init(); err != nil {
		t.Fatalf("Init webchat store: %v", err)
	}
	srv.SetWebChatStore(wcs)
	enableWriteDenySwitch(t, srv)

	dmKey := "dm:agent:" + agentID + ":user:" + DevUserID
	if err := wcs.UpsertDM(ctx, WebChatDM{
		ConversationKey: dmKey,
		ParticipantID:   DevUserID,
		PeerID:          agentID,
		PeerKind:        "agent",
	}); err != nil {
		t.Fatalf("UpsertDM: %v", err)
	}

	// The user's own message to the agent — always persisted "dispatched" by
	// the chat v2 send handler (handlers_chat_v2.go), so it never trips the
	// guard on its own.
	userMsg := &store.Message{
		ID:            tid("promote-busy-user-msg"),
		ProjectID:     proj.ID,
		Sender:        "user:dev@localhost",
		SenderID:      DevUserID,
		Recipient:     "agent:" + agentID,
		Msg:           "hello agent",
		Type:          "chat",
		Channel:       "web",
		ThreadID:      dmKey,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     time.Now().Add(-5 * time.Second),
	}
	if err := s.CreateMessage(ctx, userMsg); err != nil {
		t.Fatalf("CreateMessage (user): %v", err)
	}

	return srv, s, wcs, agent, dmKey
}

// deliverAgentReplyViaBroker persists an agent's reply into a user's DM the
// same way the production hub does: through MessageBrokerProxy.deliverToUser,
// the subscriber invoked whenever the agent's outbound message is published
// to its "user messages" topic (the path taken whenever a message broker
// proxy is running — the default in cmd/server_foreground.go).
func deliverAgentReplyViaBroker(t *testing.T, s store.Store, wcs WebChatStore, agent *store.Agent, dmKey, body string) {
	t.Helper()
	ctx := context.Background()

	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	events := NewChannelEventPublisher()
	defer events.Close()

	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return nil }, slog.Default())
	proxy.webChatStore = wcs

	reply := messages.NewInstruction("agent:"+agent.Slug, "user:"+DevUserID, body)
	reply.SenderID = agent.ID
	reply.RecipientID = DevUserID
	reply.ThreadID = dmKey
	reply.Channel = "web"

	proxy.deliverToUser(ctx, agent.ProjectID, "project."+agent.ProjectID+".user.message", reply)
}

// TestDeliverToUser_PersistsDispatchedNotPending is the root-cause
// regression test. It reproduces the bug at the lowest level: a single
// deliverToUser call (the real production write path for an agent's reply
// to a user) must persist DispatchState "dispatched", matching every other
// message-creation call site in the codebase — never leave it to fall
// through to the Ent schema's "pending" default.
func TestDeliverToUser_PersistsDispatchedNotPending(t *testing.T) {
	srv, s, wcs, agent, dmKey := setupPromoteBusyFixture(t)
	_ = srv

	deliverAgentReplyViaBroker(t, s, wcs, agent, dmKey, "hi back from agent")

	result, err := s.ListMessages(context.Background(), store.MessageFilter{ThreadID: dmKey}, store.ListOptions{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var replyMsg *store.Message
	for i := range result.Items {
		if result.Items[i].Sender == "agent:"+agent.Slug {
			replyMsg = &result.Items[i]
		}
	}
	if replyMsg == nil {
		t.Fatalf("expected agent reply to be persisted, found %d messages", len(result.Items))
	}
	if replyMsg.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("expected agent reply DispatchState %q, got %q — a permanently "+
			"pending row will make every future promote attempt on this DM fail "+
			"with IN_FLIGHT_MESSAGES", store.MessageDispatchDispatched, replyMsg.DispatchState)
	}
}

// TestPromoteDM_AfterAgentReplyViaBroker_Succeeds is the end-to-end
// reproduction: a realistic DM (one user message, one agent reply delivered
// through the actual broker path) must be promotable. Before the fix, the
// agent's reply above is persisted "pending" and this always returns 409
// IN_FLIGHT_MESSAGES — the exact bug reported ("fails on every attempt").
func TestPromoteDM_AfterAgentReplyViaBroker_Succeeds(t *testing.T) {
	srv, s, wcs, agent, dmKey := setupPromoteBusyFixture(t)

	deliverAgentReplyViaBroker(t, s, wcs, agent, dmKey, "hi back from agent")

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+dmKey+"/promote",
		map[string]string{"name": "Promoted Thread"})

	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("expected promote to succeed (200/201), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPromoteDM_GenuinelyPendingMessage_StillBlocks proves the guard is not
// simply removed by the fix above: a message that is truly still in flight
// (dispatch_state = "pending", e.g. a synchronous dispatch attempt that has
// not yet resolved) must still cause promote to fail with IN_FLIGHT_MESSAGES.
func TestPromoteDM_GenuinelyPendingMessage_StillBlocks(t *testing.T) {
	srv, s, _, agent, dmKey := setupPromoteBusyFixture(t)
	ctx := context.Background()

	// Simulate a message whose dispatch is genuinely still in flight: the
	// durable dispatch intent has been written, but the broker/runtime has
	// not yet acknowledged it (same shape as agent_dm_operation.go's
	// pre-dispatch "pending" write).
	pending := &store.Message{
		ID:            tid("promote-busy-pending-msg"),
		ProjectID:     agent.ProjectID,
		Sender:        "user:dev@localhost",
		SenderID:      DevUserID,
		Recipient:     "agent:" + agent.ID,
		Msg:           "are you still there?",
		Type:          "chat",
		Channel:       "web",
		ThreadID:      dmKey,
		DispatchState: store.MessageDispatchPending,
		CreatedAt:     time.Now(),
	}
	if err := s.CreateMessage(ctx, pending); err != nil {
		t.Fatalf("CreateMessage (pending): %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/chat/conversations/"+dmKey+"/promote",
		map[string]string{"name": "Promoted Thread"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected promote to be blocked with 409 while a message is "+
			"genuinely pending, got %d: %s", rec.Code, rec.Body.String())
	}

	var body ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "IN_FLIGHT_MESSAGES" {
		t.Fatalf("expected error code IN_FLIGHT_MESSAGES, got %q", body.Error.Code)
	}
}
