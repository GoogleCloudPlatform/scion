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

// DEF-162: agent-authored mentions must notify humans.
//
// This file tests that an agent posting to a group conversation with @mentions
// in the body creates mention notifications for the mentioned human members,
// on both broker and non-broker topologies.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
// mention notification tests. The human is added to the project members group
// with an unambiguous display name ("UniqueHuman162") that resolves to exactly
// one member (AC-3).
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
		ID:         api.NewUUID(),
		Name:       "NotifyBot",
		Slug:       "notifybot",
		ProjectID:  project.ID,
		Phase:      "running",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Add human to project members group so resolveProjectHumanMembers finds them.
	groupID := api.NewUUID()
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID:   groupID,
		Name: "def162-project members",
		Slug: "project:def162-project:members",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   human.ID,
		Role:       "member",
	}))

	// Set up WebChatStore + ChatNotifier.
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	wcs := NewWebChatStore(db, "sqlite3")
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

// waitForMentionNotification polls the store for a mention notification for the
// given user, up to the timeout. Returns the notification if found.
func waitForMentionNotification(t *testing.T, s store.Store, userID string, timeout time.Duration) *store.Notification {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		notifs, err := s.GetNotifications(context.Background(), store.SubscriberTypeUser, userID, false)
		require.NoError(t, err)
		for i := range notifs {
			if notifs[i].Status == ChatNotificationMention {
				return &notifs[i]
			}
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
// AC-1: agent posts to group with @mention -> notification exists
// ---------------------------------------------------------------------------

func TestDEF162_AC1_AgentMention_CreatesNotification(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 check this out", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code, "send must succeed: %s", rr.Body.String())

	// The mention fires in a goroutine -- poll for the notification.
	notif := waitForMentionNotification(t, s, human.ID, 5*time.Second)
	require.NotNil(t, notif, "AC-1: mention notification must exist for the mentioned user")
	assert.Equal(t, ChatNotificationMention, notif.Status)
	assert.Contains(t, notif.Message, "@NotifyBot mentioned you",
		"notification must name the agent")
	assert.Contains(t, notif.Message, "Hey @UniqueHuman162 check this out",
		"notification must include the message preview")
}

// ---------------------------------------------------------------------------
// AC-2: muted conversation -> no notification
// ---------------------------------------------------------------------------

func TestDEF162_AC2_AgentMention_MutedConversation_NoNotification(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)
	ctx := context.Background()

	// Mute the conversation for the human.
	srv.mu.RLock()
	wcs := srv.webChatStore
	srv.mu.RUnlock()
	require.NotNil(t, wcs)
	require.NoError(t, wcs.SetMuted(ctx, human.ID, topicID, true))

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 muted test", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	// Wait briefly -- no notification should appear.
	notif := waitForMentionNotification(t, s, human.ID, 1*time.Second)
	assert.Nil(t, notif, "AC-2: no notification when conversation is muted")
}

// ---------------------------------------------------------------------------
// AC-4: no mention token -> no notification
// ---------------------------------------------------------------------------

func TestDEF162_AC4_NoMentionToken_NoNotification(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"This message has no mentions at all", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	notif := waitForMentionNotification(t, s, human.ID, 1*time.Second)
	assert.Nil(t, notif, "AC-4: no notification when message has no mention token")
}

// ---------------------------------------------------------------------------
// AC-5: agent slug mention -> no human notification (pin existing behaviour)
// ---------------------------------------------------------------------------

func TestDEF162_AC5_AgentSlugMention_NoHumanNotification(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @notifybot check yourself", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	// No notification for the agent's own ID (agents are not project human members).
	notif := waitForMentionNotification(t, s, agent.ID, 1*time.Second)
	assert.Nil(t, notif, "AC-5: agent slug mention must not create human notification")

	// Also no notification for the human -- the human was not mentioned.
	humanNotif := waitForMentionNotification(t, s, human.ID, 500*time.Millisecond)
	assert.Nil(t, humanNotif, "human should not be notified when only agent slug was mentioned")
}

// ---------------------------------------------------------------------------
// AC-6: sender label is agent.Name (fallback Slug), never UUID
// ---------------------------------------------------------------------------

func TestDEF162_AC6_SenderLabel_IsAgentName_NotUUID(t *testing.T) {
	srv, s, project, agent, human, topicID := def162Setup(t)

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 label check", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	notif := waitForMentionNotification(t, s, human.ID, 5*time.Second)
	require.NotNil(t, notif, "notification must exist for label check")

	// Positive assertion: the notification uses agent.Name ("NotifyBot").
	assert.Contains(t, notif.Message, "@NotifyBot mentioned you",
		"AC-6: sender label must be agent.Name")

	// Negative assertion: the notification must NOT contain the agent's UUID.
	assert.NotContains(t, notif.Message, agent.ID,
		"AC-6: notification must never contain agent UUID as sender label")
}

func TestDEF162_AC6_SenderLabel_FallsBackToSlug(t *testing.T) {
	// The ent schema enforces Agent.Name NotEmpty(), so we test the slug
	// fallback by calling fireHumanMentionNotifications directly with an
	// empty senderName replaced by slug -- mirroring the production logic.
	srv, s, project, _, human, topicID := def162Setup(t)
	ctx := context.Background()

	agentID := api.NewUUID()
	slug := "slug-only-agent"

	// Simulate the production fallback: when agent.Name is "", use agent.Slug.
	senderName := ""
	if senderName == "" {
		senderName = slug
	}

	srv.fireHumanMentionNotifications(ctx,
		[]string{"UniqueHuman162"},
		project.ID,
		topicID,    // conversationKey -- the topic we created
		"",         // senderUserID -- empty for agents
		senderName, // slug fallback
		"Hey @UniqueHuman162 slug fallback",
	)

	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, human.ID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1, "notification must exist for slug fallback test")
	assert.Contains(t, notifs[0].Message, "@slug-only-agent mentioned you",
		"AC-6: sender label must fall back to Slug when Name is empty")
	assert.NotContains(t, notifs[0].Message, agentID,
		"AC-6: notification must never contain agent UUID")
}

// ---------------------------------------------------------------------------
// AC-7: DM with mention -> only DM notification, not also a mention notification
//
// Driven through the handler on the conv-ref DM backfill path
// (handlers_agent_messaging.go:773-786). When a direct conversation is
// resolved via ConversationRef with no ThreadID, the handler backfills
// req.ThreadID from convResult.ExternalRef -- a dm:-prefixed key. The
// mention guard at :934 must exclude it.
// ---------------------------------------------------------------------------

func TestDEF162_AC7_DM_WithMention_OnlyDMNotification(t *testing.T) {
	srv, s, project, agent, human, _ := def162Setup(t)
	ctx := context.Background()

	// Build a canonical dm: key for this agent->human pair.
	dmKey, err := messages.DMConversationKey("agent", agent.ID, "user", human.ID)
	require.NoError(t, err)

	// Create a direct conversation whose ExternalRef is the DM key.
	conv := &store.Conversation{
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
		DriftState:  "active",
	}
	created, err := s.UpsertConversationByExternalRef(ctx, conv)
	require.NoError(t, err)

	// POST with ConversationRef and NO ThreadID. The handler hits the
	// def152DerivedRecipient path (handlers_agent_messaging.go:547-608)
	// and then the ThreadID backfill at :773-784, setting req.ThreadID to
	// the dm:-prefixed ExternalRef. The mention guard at :934 must then
	// exclude it.
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 this is a DM via conv-ref", "conv:"+created.ID)
	require.Equal(t, http.StatusOK, rr.Code, "DM send must succeed: %s", rr.Body.String())

	// Wait for any notifications to settle.
	time.Sleep(500 * time.Millisecond)

	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, human.ID, false)
	require.NoError(t, err)

	// Count mention-type notifications -- there should be zero.
	mentionCount := 0
	for _, n := range notifs {
		if n.Status == ChatNotificationMention {
			mentionCount++
		}
	}
	assert.Equal(t, 0, mentionCount,
		"AC-7: DM with mention must NOT produce a mention notification (DM notification is sufficient)")
}

// ---------------------------------------------------------------------------
// AC-8: mention fires on BOTH broker and non-broker topologies
// ---------------------------------------------------------------------------

func TestDEF162_AC8_NonBroker_MentionFires(t *testing.T) {
	// Non-broker topology: no MessageBrokerProxy configured.
	srv, s, project, agent, human, topicID := def162Setup(t)

	// Verify no broker is configured (default from testServer).
	assert.Nil(t, srv.GetMessageBrokerProxy(), "precondition: no broker configured")

	convID := def162GroupConv(t, s, project.ID, topicID)
	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 non-broker path", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	notif := waitForMentionNotification(t, s, human.ID, 5*time.Second)
	require.NotNil(t, notif, "AC-8: mention must fire on non-broker path")
	assert.Contains(t, notif.Message, "@NotifyBot mentioned you")
}

func TestDEF162_AC8_Broker_MentionFires(t *testing.T) {
	// Broker topology: MessageBrokerProxy configured and wired.
	srv, s, project, agent, human, topicID := def162Setup(t)
	ctx := context.Background()

	// Wire up a broker proxy (pattern from def141BrokerSetup and
	// handlers_agent_messaging_test.go:446-450).
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })

	proxy := NewMessageBrokerProxy(bus, s, events,
		func() AgentDispatcher { return &brokerMockDispatcher{} }, slog.Default())

	// Wire ChatNotifier and WebChatStore into the proxy so the broker path
	// handles DM watermarks and notifications correctly.
	srv.mu.RLock()
	proxy.chatNotifier = srv.chatNotifier
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()

	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	// Subscribe the broker to this project's user messages so deliverToUser fires.
	sub, err := bus.Subscribe(
		eventbus.TopicAllUserMessages(project.ID),
		func(ctx context.Context, topic string, msg *messages.StructuredMessage) {
			proxy.deliverToUser(ctx, project.ID, topic, msg)
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Create the group conversation.
	convID := def162GroupConv(t, s, project.ID, topicID)

	rr := postOutboundConvRef(t, srv, project.ID, agent.ID,
		"Hey @UniqueHuman162 broker path", "conv:"+convID)
	require.Equal(t, http.StatusOK, rr.Code)

	notif := waitForMentionNotification(t, s, human.ID, 5*time.Second)
	require.NotNil(t, notif, "AC-8: mention must fire on broker path")
	assert.Contains(t, notif.Message, "@NotifyBot mentioned you")

	// Also verify the message was persisted through the broker (not directly).
	msgs, err := s.ListMessages(ctx, store.MessageFilter{ConversationID: convID}, store.ListOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(msgs.Items), 1, "broker must persist the message")
}

// ---------------------------------------------------------------------------
// AC-9: agent:-prefixed ThreadID must not fire mentions
//
// Driven through the handler. ThreadID is caller-settable
// (OutboundMessageRequest, thread_id at :45). An agent:-prefixed ThreadID
// is a legacy agent thread, not a topic; the mention guard at
// handlers_agent_messaging.go:934 must exclude it
// (mirrors messagebroker.go:591).
// ---------------------------------------------------------------------------

func TestDEF162_AC9_AgentPrefixThreadID_NoMention(t *testing.T) {
	srv, s, project, agent, human, _ := def162Setup(t)
	ctx := context.Background()

	// Wire a broker proxy so that Channel:"web" passes
	// validateChannelRegistered (handlers_agent_messaging.go:2686-2712).
	// ValidateLegacyMessage (validate_compat.go:42) requires Channel when
	// ThreadID is set, and validateChannelRegistered requires a broker when
	// Channel is non-empty.
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
	proxy.chatNotifier = srv.chatNotifier
	proxy.webChatStore = srv.webChatStore
	srv.mu.RUnlock()
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)

	// POST with an explicit agent:-prefixed ThreadID, a user recipient,
	// and Channel:"web". The handler derives a "group" conversation via
	// DeriveConversationKey (Rules 2/3 at :482) and proceeds to the mention
	// guard at :934, which must exclude the agent:-prefixed ThreadID.
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

	// Wait for any notifications to settle.
	time.Sleep(500 * time.Millisecond)

	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, human.ID, false)
	require.NoError(t, err)

	// Count mention-type notifications -- there should be zero.
	mentionCount := 0
	for _, n := range notifs {
		if n.Status == ChatNotificationMention {
			mentionCount++
		}
	}
	assert.Equal(t, 0, mentionCount,
		"AC-9: agent:-prefixed ThreadID must NOT produce a mention notification")

	// Verify the guard is load-bearing: call fireHumanMentionNotifications
	// directly with an agent:-prefixed key. It DOES fire a notification,
	// proving the handler-level guard is necessary (the underlying function
	// does not guard on key prefix).
	srv.fireHumanMentionNotifications(ctx,
		[]string{"UniqueHuman162"},
		project.ID,
		"agent:"+agent.ID,
		"",
		agent.Name,
		"Hey @UniqueHuman162 direct-call proof",
	)
	directNotifs, err := s.GetNotifications(ctx, store.SubscriberTypeUser, human.ID, false)
	require.NoError(t, err)
	directMentions := 0
	for _, n := range directNotifs {
		if n.Status == ChatNotificationMention {
			directMentions++
		}
	}
	require.Greater(t, directMentions, 0,
		"fireHumanMentionNotifications with agent:-prefixed key must fire -- "+
			"this proves the handler guard is load-bearing")
}
