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

// DEF-162: agent-authored mentions of humans.
//
// This file tests that an agent posting to a group conversation with @mentions
// in the body makes the mentioned human members members of the thread, on both
// broker and non-broker topologies, and creates no notification rows.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/mattn/go-sqlite3"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// def162Setup creates a server, project, agent, and human user wired for
// agent mention tests. The human is added as a project member via a
// role binding (PM1) with an unambiguous display name ("UniqueHuman162") that
// resolves to exactly one member (AC-3).
func def162Setup(t *testing.T) (srv *Server, s store.Store, project *store.Project, agent *store.Agent, human *store.User, topicID string) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	project = &store.Project{
		ID:   api.NewUUID(),
		Name: "def162-project",
		Slug: "def162-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	human = &store.User{
		ID:          api.NewUUID(),
		Email:       "uniquehuman162@example.com",
		DisplayName: "UniqueHuman162",
		Role:        "member",
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, human))

	agent = &store.Agent{
		ID:        api.NewUUID(),
		Name:      "NotifyBot",
		Slug:      "notifybot",
		ProjectID: project.ID,
		Phase:     "running",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Add human as a project member via role binding (PM1).
	// resolveProjectHumanMembers now queries ListProjectMembers (role bindings).
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err, "project-member role definition must exist")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      human.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Set up WebChatStore on the hub store's own database, as in
	// production: the topic's linked conversation is then the same row as
	// the thread:<project>:<topic> conversation an agent resolves, which
	// thread membership requires.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB()")
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Create a topic for group conversations.
	topicID = api.NewUUID()
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: topicID, ProjectID: project.ID, Name: "def162-room",
		CreatedBy: human.ID, CreatedAt: time.Now(),
	}))

	return srv, s, project, agent, human, topicID
}

// def162GroupConv creates a group conversation whose ExternalRef encodes the
// given topicKey, and returns the conversation ID.
func def162GroupConv(t *testing.T, s store.Store, projectID, topicKey string) string {
	t.Helper()
	conv := &store.Conversation{
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + projectID + ":" + topicKey,
		ProjectID:   &projectID,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(context.Background(), conv)
	require.NoError(t, err)
	return created.ID
}

// postOutboundConvRef sends an agent outbound message to a conversation ref
// with the given message body. Returns the response recorder.
func postOutboundConvRef(t *testing.T, srv *Server, projectID, agentID, msg, convRef string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(OutboundMessageRequest{
		Msg:             msg,
		ConversationRef: convRef,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agentID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
	}}))
	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agentID)
	return rr
}

// def162MentionWaitTimeout is the deadline for a positive wait on the
// background thread-membership write (handlers_agent_messaging.go). 30s is
// generous headroom for a loaded CI runner; a passing run returns as soon as
// the row appears. Absence checks keep a short settle delay instead.
const def162MentionWaitTimeout = 30 * time.Second

// def162Settle is how long absence checks wait for any background write.
const def162Settle = 500 * time.Millisecond

// waitForMember polls until userID is an active user participant of convID.
func waitForMember(t *testing.T, s store.Store, convID, userID string) bool {
	t.Helper()
	deadline := time.Now().Add(def162MentionWaitTimeout)
	for time.Now().Before(deadline) {
		if isUserParticipant(t, s, convID, userID) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// requireNoNotifications asserts userID has no notification rows.
func requireNoNotifications(t *testing.T, s store.Store, userID string) {
	t.Helper()
	notifs, err := s.GetNotifications(t.Context(), store.SubscriberTypeUser, userID, false)
	require.NoError(t, err)
	require.Empty(t, notifs, "chat messages must not create notification rows")
}

// waitForBrokerMessage polls the store for at least one persisted message in
// the given conversation, up to the timeout. Returns the messages found (nil
// if none appeared within the deadline).
//
// On the broker path, persistence happens in the eventbus subscriber callback
// (proxy.deliverToUser), which runs asynchronously relative to the publish
// call in the handler -- the same class of goroutine-scheduling exposure as
// the membership write, so it uses the same def162MentionWaitTimeout
// headroom.
func waitForBrokerMessage(t *testing.T, s store.Store, conversationID string, timeout time.Duration) []store.Message {
	t.Helper()
	ctx := t.Context()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := s.ListMessages(ctx, store.MessageFilter{ConversationID: conversationID}, store.ListOptions{})
		require.NoError(t, err)
		if len(msgs.Items) > 0 {
			return msgs.Items
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// ---------------------------------------------------------------------------
// AC-3: fixture unambiguity assertion
// ---------------------------------------------------------------------------

func TestDEF162_AC3_MentionFixtureIsUnambiguous(t *testing.T) {
	// Every mention token used in DEF-162 tests must resolve to exactly one
	// member (design F3). This test verifies that "UniqueHuman162" is
	// unambiguous in the project members group.
	srv, s, project, _, human, topicID := def162Setup(t)
	ctx := context.Background()

	members := srv.resolveProjectHumanMembers(ctx, project.ID)
	require.NotEmpty(t, members, "project must have human members")

	// Count how many members match our mention token.
	matches := 0
	for _, m := range members {
		if m.DisplayName == "UniqueHuman162" || m.Email == "uniquehuman162@example.com" {
			assert.Equal(t, human.ID, m.ID, "matched member must be our test human")
			matches++
		}
	}
	require.Equal(t, 1, matches, "AC-3: mention token must resolve to exactly one member")

	// Verify the topic exists.
	_ = s
	_ = topicID
}

// ---------------------------------------------------------------------------
// AC-1: agent posts to group with @mention -> human becomes a member
// ---------------------------------------------------------------------------

func TestDEF162_AC1_AgentMention_MakesMember(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 check this out", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code, "send must succeed: %s", rr.Body.String())

	require.True(t, waitForMember(t, s, convID, human.ID),
		"AC-1: the mentioned human must become a member of the thread")
	requireNoNotifications(t, s, human.ID)
}

// ---------------------------------------------------------------------------
// AC-4: no mention token -> no membership
// ---------------------------------------------------------------------------

func TestDEF162_AC4_NoMentionToken_NoMember(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"This message has no mentions at all", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	time.Sleep(def162Settle)
	assert.False(t, isUserParticipant(t, s, convID, human.ID),
		"AC-4: no membership when the message has no mention token")
}

// ---------------------------------------------------------------------------
// AC-5: agent slug mention -> no human membership
// ---------------------------------------------------------------------------

func TestDEF162_AC5_AgentSlugMention_NoHumanMember(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @notifybot check yourself", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	time.Sleep(def162Settle)
	assert.False(t, isUserParticipant(t, s, convID, human.ID),
		"AC-5: an agent slug mention must not make a human a member")
	requireNoNotifications(t, s, agent.ID)
}

// ---------------------------------------------------------------------------
// AC-7: DM with mention -> no notification rows
//
// Driven through the handler on the conv-ref DM backfill path. When a direct
// conversation is resolved via ConversationRef with no ThreadID, the handler
// backfills req.ThreadID from convResult.ExternalRef -- a dm:-prefixed key,
// which the thread-membership guard excludes.
// ---------------------------------------------------------------------------

func TestDEF162_AC7_DM_WithMention_NoNotification(t *testing.T) {
	srv, s, project, agent, human, _ := def162Setup(t)
	ctx := context.Background()

	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", human.ID)
	require.NoError(t, err)
	created, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	})
	require.NoError(t, err)

	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 this is a DM via conv-ref", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code, "DM send must succeed: %s", rr.Body.String())

	time.Sleep(def162Settle)
	requireNoNotifications(t, s, human.ID)
}

// ---------------------------------------------------------------------------
// AC-8: membership is recorded on BOTH broker and non-broker topologies
// ---------------------------------------------------------------------------

func TestDEF162_AC8_NonBroker_MentionMakesMember(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)
	assert.Nil(t, srv.GetMessageBrokerProxy(), "precondition: no broker configured")

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 non-broker path", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	require.True(t, waitForMember(t, s, convID, human.ID),
		"AC-8: mention must make a member on the non-broker path")
	requireNoNotifications(t, s, human.ID)
}

func TestDEF162_AC8_Broker_MentionMakesMember(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })

	proxy := NewMessageBrokerProxy(bus, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	sub, err := bus.Subscribe(
		eventbus.TopicAllUserMessages(project.ID),
		func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
			proxy.deliverToUser(ctx, project.ID, topic, msg)
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 broker path", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	require.True(t, waitForMember(t, s, convID, human.ID),
		"AC-8: mention must make a member on the broker path")

	msgs := waitForBrokerMessage(t, s, convID, def162MentionWaitTimeout)
	require.GreaterOrEqual(t, len(msgs), 1, "broker must persist the message")
	requireNoNotifications(t, s, human.ID)
}

// ---------------------------------------------------------------------------
// AC-9: agent:-prefixed ThreadID must not record membership
//
// ThreadID is caller-settable. An agent:-prefixed ThreadID is a legacy agent
// thread, not a topic; the guard in handlers_agent_messaging.go excludes it.
// ---------------------------------------------------------------------------

func TestDEF162_AC9_AgentPrefixThreadID_NoMember(t *testing.T) {
	srv, s, project, agent, human, _ := def162Setup(t)

	inproc := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inproc.Close() })
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "web", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	threadConv := seedThreadConversation(t, s, project.ID, "agent:"+agent.ID)
	body, _ := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + human.Email,
		Msg:       "Hey @UniqueHuman162 via agent thread",
		ThreadID:  "agent:" + agent.ID,
		Channel:   "web",
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
	require.Equal(t, http.StatusOK, rr.Code, "agent-thread send must succeed: %s", rr.Body.String())

	time.Sleep(def162Settle)
	assert.False(t, isUserParticipant(t, s, threadConv.ID, human.ID),
		"AC-9: an agent:-prefixed ThreadID must not record membership")
	requireNoNotifications(t, s, human.ID)
}

// expectMemberMessage waits for a thread message on user.<userID>.chat.message.
func expectMemberMessage(t *testing.T, events <-chan Event, userID, topicID string) {
	t.Helper()
	select {
	case evt := <-events:
		var payload UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &payload))
		assert.Equal(t, "user."+userID+".chat.message", evt.Subject)
		assert.Equal(t, topicID, payload.ThreadID)
	case <-time.After(5 * time.Second):
		t.Fatal("the mentioned human did not receive the agent's message on their user subject")
	}
}

// A human an agent @mentions into a thread receives that message on their
// user subject (non-broker path): membership is written before the
// message is published and fanned out.
func TestDEF162_AgentMention_NonBroker_FannedOutToMentioned(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.SetEventPublisher(events)
	sub, unsub := events.Subscribe("user." + human.ID + ".chat.message")
	defer unsub()

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 fan-out", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	expectMemberMessage(t, sub, human.ID, topicID)
}

// syncTestBus is an event bus that runs matching handlers inline, inside
// Publish. With it, the broker path's deliverToUser (and its member
// fan-out hook) runs before the publisher's Publish call returns, so a
// test can observe what the publisher had written before publishing.
type syncTestBus struct {
	mu   sync.Mutex
	subs map[int]syncTestSub
	next int
}

type syncTestSub struct {
	pattern string
	handler eventbus.EventHandler
}

func newSyncTestBus() *syncTestBus { return &syncTestBus{subs: map[int]syncTestSub{}} }

// syncTestMatch matches NATS-style patterns: '*' is one token, '>' the rest.
func syncTestMatch(pattern, topic string) bool {
	pt, tt := strings.Split(pattern, "."), strings.Split(topic, ".")
	for i, p := range pt {
		if p == ">" {
			return len(tt) > i
		}
		if i >= len(tt) || (p != "*" && p != tt[i]) {
			return false
		}
	}
	return len(pt) == len(tt)
}

func (b *syncTestBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.mu.Lock()
	var handlers []eventbus.EventHandler
	for _, sub := range b.subs {
		if sub.handler != nil && syncTestMatch(sub.pattern, topic) {
			handlers = append(handlers, sub.handler)
		}
	}
	b.mu.Unlock()
	for _, h := range handlers {
		h(ctx, topic, msg)
	}
	return nil
}

func (b *syncTestBus) Subscribe(pattern string, handler eventbus.EventHandler) (eventbus.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	b.subs[id] = syncTestSub{pattern: pattern, handler: handler}
	return syncTestUnsub(func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}), nil
}

func (b *syncTestBus) Close() error { return nil }

type syncTestUnsub func()

func (u syncTestUnsub) Unsubscribe() error { u(); return nil }

// The same on the broker path, where deliverToUser stores the message and
// runs the member fan-out hook wired as StartMessageBroker wires it. The
// synchronous bus runs deliverToUser inside the publish, and the hook
// records whether the mentioned human was already a member when it ran:
// membership written after the publish (or in the background) fails here.
func TestDEF162_AgentMention_Broker_FannedOutToMentioned(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.SetEventPublisher(events)
	bus := newSyncTestBus()

	convID := def162GroupConv(t, s, project.ID, topicID)

	proxy := NewMessageBrokerProxy(bus, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())
	srv.mu.RLock()
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()
	var hookCalls, memberAtHook atomic.Int32
	proxy.memberFanout = func(ctx context.Context, msg *store.Message, attachments []AttachmentRef) {
		if isWebThreadMessage(msg) {
			hookCalls.Add(1)
			parts, err := s.ListParticipants(ctx, convID)
			if err == nil {
				for _, p := range parts {
					if p.PrincipalKind == "user" && p.PrincipalID == human.ID && p.LeftAt == nil {
						memberAtHook.Add(1)
						break
					}
				}
			}
		}
		srv.fanOutThreadMessageToMembersAsync(ctx, msg, attachments)
	}
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	sub, unsub := events.Subscribe("user." + human.ID + ".chat.message")
	defer unsub()

	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 broker fan-out", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.GreaterOrEqual(t, hookCalls.Load(), int32(1), "deliverToUser must run the member fan-out hook")
	require.Equal(t, hookCalls.Load(), memberAtHook.Load(),
		"the mentioned human must already be a member when the broker fans the message out")
	expectMemberMessage(t, sub, human.ID, topicID)
}
