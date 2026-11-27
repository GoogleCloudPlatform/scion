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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// watermarkOrderRecorder records watermark updates and SSE publishes in order.
type watermarkOrderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *watermarkOrderRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *watermarkOrderRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type recordingWatermarkWebChatStore struct {
	WebChatStore
	rec *watermarkOrderRecorder
}

func (s recordingWatermarkWebChatStore) TouchTopicActivity(ctx context.Context, topicID, messageID string) error {
	if messageID != "" {
		s.rec.add("topic-watermark:" + topicID + ":" + messageID)
	}
	return s.WebChatStore.TouchTopicActivity(ctx, topicID, messageID)
}

func (s recordingWatermarkWebChatStore) TouchDMActivity(ctx context.Context, conversationKey, messageID string) error {
	if messageID != "" {
		s.rec.add("dm-watermark:" + conversationKey + ":" + messageID)
	}
	return s.WebChatStore.TouchDMActivity(ctx, conversationKey, messageID)
}

type recordingWatermarkEvents struct {
	EventPublisher
	rec *watermarkOrderRecorder
}

func (e recordingWatermarkEvents) PublishUserMessage(ctx context.Context, msg *store.Message, a []AttachmentRef, r []artifacts.MessageRef, opts ...ChatEventOption) {
	e.rec.add("sse:" + msg.ID)
	e.EventPublisher.PublishUserMessage(ctx, msg, a, r, opts...)
}

// nonPersistingTestBus is an EventBus that accepts Publish calls without
// supporting the hub's in-process persistence subscriber (so
// subscribeProjectUserMessages returns false and triggers the !persisting
// fallback in handleAgentOutboundMessage).
type nonPersistingTestBus struct {
	mu        sync.Mutex
	published []string
}

func (b *nonPersistingTestBus) Publish(_ context.Context, topic string, _ *messages.StructuredMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, topic)
	return nil
}

type noopTestSubscription struct{}

func (noopTestSubscription) Unsubscribe() error { return nil }

func (b *nonPersistingTestBus) Subscribe(_ string, _ eventbus.EventHandler) (eventbus.Subscription, error) {
	return noopTestSubscription{}, nil
}

func (b *nonPersistingTestBus) Close() error { return nil }

func sendAgentOutboundJSON(t *testing.T, srv *Server, agent *store.Agent, payload OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+agent.ID+"/outbound-message",
		bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: agent.ProjectID,
		Ancestry:  agent.Ancestry,
	}}))

	rr := httptest.NewRecorder()
	srv.handleAgentOutboundMessage(rr, req, agent.ID)
	return rr
}

func decodeOutboundMessageID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.NotEmpty(t, body.MessageID)
	return body.MessageID
}

func fetchUnreadCountForUser(t *testing.T, srv *Server, user *store.User) chatUnreadCountResponse {
	t.Helper()
	ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/unread-count", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), ident))
	rr := httptest.NewRecorder()
	srv.handleChatUnreadCount(rr, req)
	return getUnreadCount(t, rr)
}

func fetchTopicHasUnread(t *testing.T, srv *Server, user *store.User, projectID, topicID string) bool {
	t.Helper()
	ident := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/spaces/"+projectID+"/threads", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), ident))
	rr := httptest.NewRecorder()
	srv.handleChatSpaceRoutes(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var body chatTopicListResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	for _, tp := range body.Threads {
		if tp.ID == topicID {
			return tp.HasUnread
		}
	}
	t.Fatalf("topic %s not found in /api/v1/chat/spaces/%s/threads response", topicID, projectID)
	return false
}

func requireWatermarkBeforeSSE(t *testing.T, events []string, wantWatermark, wantSSE string) {
	t.Helper()
	wm, sse := -1, -1
	for i, e := range events {
		if e == wantWatermark && wm < 0 {
			wm = i
		}
		if e == wantSSE && sse < 0 {
			sse = i
		}
	}
	require.GreaterOrEqual(t, wm, 0, "expected watermark event %q in %v", wantWatermark, events)
	require.GreaterOrEqual(t, sse, 0, "expected SSE event %q in %v", wantSSE, events)
	require.Less(t, wm, sse, "watermark must be stamped before SSE publish: %v", events)
}

// TestAgentOutboundDirect_StampsTopicWatermarkAndUnreadBadge verifies that
// when the message broker is disabled (deliveryUserDirect), an agent posting
// to a space topic stamps last_message_id and last_activity_at before SSE
// publish so /api/v1/chat/unread-count and topic hasUnread reflect the unread
// message (ptone/scion#9641).
func TestAgentOutboundDirect_StampsTopicWatermarkAndUnreadBadge(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	require.Nil(t, f.srv.GetMessageBrokerProxy(), "broker must be disabled for deliveryUserDirect")

	rec := &watermarkOrderRecorder{}
	f.srv.SetWebChatStore(recordingWatermarkWebChatStore{WebChatStore: f.wcs, rec: rec})
	f.srv.events = recordingWatermarkEvents{EventPublisher: f.srv.events, rec: rec}

	require.Equal(t, chatUnreadCountResponse{}, fetchUnreadCountForUser(t, f.srv, f.ua))
	require.False(t, fetchTopicHasUnread(t, f.srv, f.ua, f.projA.ID, f.topicA))

	topicBefore, err := f.wcs.GetTopic(ctx, f.topicA)
	require.NoError(t, err)
	require.NotEmpty(t, topicBefore.ConversationID)
	require.NoError(t, f.st.AddParticipant(ctx, &store.ConversationParticipant{
		ConversationID: topicBefore.ConversationID,
		PrincipalKind:  "agent",
		PrincipalID:    f.aa.ID,
		JoinedAt:       time.Now(),
	}))

	rr := sendAgentOutboundJSON(t, f.srv, f.aa, OutboundMessageRequest{
		ConversationRef: "conv:" + topicBefore.ConversationID,
		Msg:             "hello from direct topic",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	msgID := decodeOutboundMessageID(t, rr)

	requireWatermarkBeforeSSE(t, rec.list(), "topic-watermark:"+f.topicA+":"+msgID, "sse:"+msgID)

	topic, err := f.wcs.GetTopic(ctx, f.topicA)
	require.NoError(t, err)
	require.Equal(t, msgID, topic.LastMessageID)
	require.False(t, topic.LastActivityAt.IsZero(), "last_activity_at must be stamped")

	require.True(t, fetchTopicHasUnread(t, f.srv, f.ua, f.projA.ID, f.topicA))
	require.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 1, DMs: 0},
		fetchUnreadCountForUser(t, f.srv, f.ua))
}

// TestAgentOutboundDirect_StampsDMWatermarkAndUnreadBadge verifies that when
// the message broker is disabled (deliveryUserDirect), an agent DM stamps
// webchat_dm last_message_id and last_activity_at before SSE publish and
// increments the DM unread count (ptone/scion#9641).
func TestAgentOutboundDirect_StampsDMWatermarkAndUnreadBadge(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	require.Nil(t, f.srv.GetMessageBrokerProxy(), "broker must be disabled for deliveryUserDirect")

	rec := &watermarkOrderRecorder{}
	f.srv.SetWebChatStore(recordingWatermarkWebChatStore{WebChatStore: f.wcs, rec: rec})
	f.srv.events = recordingWatermarkEvents{EventPublisher: f.srv.events, rec: rec}

	rr := sendAgentOutboundJSON(t, f.srv, f.aa, OutboundMessageRequest{
		Recipient: "user:" + f.ua.Email,
		Msg:       "hello from direct dm",
	})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	msgID := decodeOutboundMessageID(t, rr)

	dmKey, err := messages.DMConversationKey("agent", f.aa.ID, "user", f.ua.ID)
	require.NoError(t, err)
	requireWatermarkBeforeSSE(t, rec.list(), "dm-watermark:"+dmKey+":"+msgID, "sse:"+msgID)

	dms, err := f.wcs.ListDMs(ctx, f.ua.ID)
	require.NoError(t, err)
	var found *WebChatDM
	for i := range dms {
		if dms[i].ConversationKey == dmKey {
			found = &dms[i]
			break
		}
	}
	require.NotNil(t, found, "expected DM %s in ListDMs", dmKey)
	require.Equal(t, msgID, found.LastMessageID)
	require.False(t, found.LastActivityAt.IsZero(), "last_activity_at must be stamped")

	require.Equal(t, chatUnreadCountResponse{Conversations: 1, Threads: 0, DMs: 1},
		fetchUnreadCountForUser(t, f.srv, f.ua))
}

// TestAgentOutboundBrokerFallback_StampsTopicAndDMWatermarks verifies that
// when deliveryUserBroker falls back to storeOutboundRow because no persisting
// subscriber is active (!persisting), both topic and DM watermarks are stamped
// before SSE publish (ptone/scion#9641).
func TestAgentOutboundBrokerFallback_StampsTopicAndDMWatermarks(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	// Configure a broker proxy backed by a FanOutEventBus with no in-process
	// spoke, so subscribeProjectUserMessages returns false (!persisting).
	spoke := &nonPersistingTestBus{}
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: "web", Bus: spoke},
	}, slog.Default())
	proxy := NewMessageBrokerProxy(fanout, f.st, f.srv.events, func() AgentDispatcher { return nil }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	f.srv.SetMessageBrokerProxy(proxy)

	rec := &watermarkOrderRecorder{}
	f.srv.SetWebChatStore(recordingWatermarkWebChatStore{WebChatStore: f.wcs, rec: rec})
	f.srv.events = recordingWatermarkEvents{EventPublisher: f.srv.events, rec: rec}

	// 1. Space topic message through !persisting broker fallback.
	rrTopic := sendAgentOutboundJSON(t, f.srv, f.aa, OutboundMessageRequest{
		Recipient: "user:" + f.ua.Email,
		Msg:       "hello from broker fallback topic",
		Channel:   "web",
		ThreadID:  f.topicA,
	})
	require.Equal(t, http.StatusOK, rrTopic.Code, rrTopic.Body.String())
	topicMsgID := decodeOutboundMessageID(t, rrTopic)

	requireWatermarkBeforeSSE(t, rec.list(), "topic-watermark:"+f.topicA+":"+topicMsgID, "sse:"+topicMsgID)

	topic, err := f.wcs.GetTopic(ctx, f.topicA)
	require.NoError(t, err)
	require.Equal(t, topicMsgID, topic.LastMessageID)
	require.False(t, topic.LastActivityAt.IsZero())
	require.True(t, fetchTopicHasUnread(t, f.srv, f.ua, f.projA.ID, f.topicA))

	// 2. DM message through !persisting broker fallback.
	rrDM := sendAgentOutboundJSON(t, f.srv, f.aa, OutboundMessageRequest{
		Recipient: "user:" + f.ua.Email,
		Msg:       "hello from broker fallback dm",
	})
	require.Equal(t, http.StatusOK, rrDM.Code, rrDM.Body.String())
	dmMsgID := decodeOutboundMessageID(t, rrDM)

	dmKey, err := messages.DMConversationKey("agent", f.aa.ID, "user", f.ua.ID)
	require.NoError(t, err)
	requireWatermarkBeforeSSE(t, rec.list(), "dm-watermark:"+dmKey+":"+dmMsgID, "sse:"+dmMsgID)

	dms, err := f.wcs.ListDMs(ctx, f.ua.ID)
	require.NoError(t, err)
	var found *WebChatDM
	for i := range dms {
		if dms[i].ConversationKey == dmKey {
			found = &dms[i]
			break
		}
	}
	require.NotNil(t, found, "expected DM %s in ListDMs", dmKey)
	require.Equal(t, dmMsgID, found.LastMessageID)
	require.False(t, found.LastActivityAt.IsZero())

	require.Equal(t, chatUnreadCountResponse{Conversations: 2, Threads: 1, DMs: 1},
		fetchUnreadCountForUser(t, f.srv, f.ua))
}

// TestNotificationDispatcher_CreateInboxMessageStampsDMWatermark verifies that
// NotificationDispatcher.createInboxMessage (used when the message broker is
// disabled or falls back) registers the DM and stamps its watermark before
// publishing SSE, matching the broker's deliverToUser path (ptone/scion#9641).
func TestNotificationDispatcher_CreateInboxMessageStampsDMWatermark(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	rec := &watermarkOrderRecorder{}
	f.srv.SetWebChatStore(recordingWatermarkWebChatStore{WebChatStore: f.wcs, rec: rec})
	events := recordingWatermarkEvents{EventPublisher: f.srv.events, rec: rec}

	nd := NewNotificationDispatcher(f.st, events, func() AgentDispatcher { return nil }, slog.Default())
	nd.webChatStoreFn = f.srv.currentWebChatStore

	sub := &store.NotificationSubscription{
		ID:             api.NewUUID(),
		ProjectID:      f.projA.ID,
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   f.ua.ID,
	}
	notif := &store.Notification{
		ID:        api.NewUUID(),
		ProjectID: f.projA.ID,
		AgentID:   f.aa.ID,
		Status:    "WAITING_FOR_INPUT",
		Message:   "agent needs input",
		CreatedAt: time.Now(),
	}

	nd.createInboxMessage(ctx, sub, notif, f.aa)

	dmKey, err := messages.DMConversationKey("agent", f.aa.ID, "user", f.ua.ID)
	require.NoError(t, err)

	dms, err := f.wcs.ListDMs(ctx, f.ua.ID)
	require.NoError(t, err)
	var found *WebChatDM
	for i := range dms {
		if dms[i].ConversationKey == dmKey {
			found = &dms[i]
			break
		}
	}
	require.NotNil(t, found, "expected DM %s in ListDMs", dmKey)
	require.NotEmpty(t, found.LastMessageID)
	require.False(t, found.LastActivityAt.IsZero())

	requireWatermarkBeforeSSE(t, rec.list(), "dm-watermark:"+dmKey+":"+found.LastMessageID, "sse:"+found.LastMessageID)
}
