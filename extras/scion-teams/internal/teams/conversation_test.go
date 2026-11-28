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

package teams

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamsBroker_Publish_ConversationRoutingAndFallback(t *testing.T) {
	var mu sync.Mutex
	var delivered []string // conversation IDs extracted from request URL

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delivered = append(delivered, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ActivityResponse{ID: "resp-1"})
	}))
	defer apiServer.Close()

	broker := NewBroker(slog.Default())
	configureBrokerWithAPI(t, broker, apiServer.URL)

	ctx := context.Background()
	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "19:chan-1@thread.tacv2;messageid=100",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))
	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "19:parent-chan@thread.tacv2",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))
	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "19:legacy@thread.tacv2",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))

	// 1. Outbound with msg.Conversation only (empty Channel/ThreadID) routes via ExternalRef.
	err := broker.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "phase 11 teams message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "teams",
			ExternalRef: "19:chan-1@thread.tacv2;messageid=100",
			ParentRef:   "19:chan-1@thread.tacv2",
			DisplayName: "General",
		},
	})
	require.NoError(t, err)

	// 1b. Outbound with msg.Conversation falls back to ParentRef when ExternalRef is not stored directly.
	err = broker.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "phase 11 parent ref fallback",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-2",
		Conversation: &messages.Conversation{
			Surface:     "teams",
			ExternalRef: "19:parent-chan@thread.tacv2;messageid=unseen",
			ParentRef:   "19:parent-chan@thread.tacv2",
		},
	})
	require.NoError(t, err)

	// 2. Outbound targeting another external surface ("slack") is ignored.
	err = broker.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "slack message",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC",
		},
	})
	require.NoError(t, err)

	// 2b. Foreign non-empty Channel ("web") is skipped by Teams.
	err = broker.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "webchat message",
		Type:      messages.TypeInstruction,
		Channel:   "web",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "thread:p1:topic-1",
		},
	})
	require.NoError(t, err)

	// 2c. Defeat scenario (b): agent-to-agent observer copy with native DM conv
	// (Channel == "") is delivered to ShowAgentToAgent links, not dropped.
	require.NoError(t, broker.store.CreateChannelLink(ctx, &ChannelLink{
		ConversationID:   "19:parent-chan@thread.tacv2",
		ProjectID:        "p1",
		Active:           true,
		ShowAgentToAgent: true,
	}))
	err = broker.Publish(ctx, "scion.project.p1.agent.reviewer.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "agent-to-agent observer copy",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-a2a",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:agent:a2",
		},
	})
	require.NoError(t, err)

	// 3. Legacy fallback (older hub sends Channel/ThreadID without Conversation).
	err = broker.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "legacy hub message",
		Type:      messages.TypeInstruction,
		Channel:   "teams",
		ThreadID:  "19:legacy@thread.tacv2",
	})
	require.NoError(t, err)

	// Drain send queue so all deliveries finish.
	broker.mu.Lock()
	sq := broker.sendQueue
	broker.mu.Unlock()
	sq.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, delivered, 4)
	assert.Contains(t, delivered, "/v3/conversations/19:chan-1@thread.tacv2;messageid=100/activities")
	assert.Contains(t, delivered, "/v3/conversations/19:parent-chan@thread.tacv2/activities")
	assert.Contains(t, delivered, "/v3/conversations/19:legacy@thread.tacv2/activities")
}

func TestTeamsHubClient_DeliverInbound_ConversationPayload(t *testing.T) {
	var mu sync.Mutex
	var gotBodies []map[string]any

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		gotBodies = append(gotBodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"delivered":true}`))
	}))
	defer hubSrv.Close()

	client := NewHubClient(hubSrv.URL, "", "", slog.Default())

	// Threaded channel conversation: without hub_inbound_conversation flag
	// (older hub), top-level conversation fields must be omitted.
	threadedMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello in thread",
		Type:      messages.TypeInstruction,
		Channel:   "teams",
		ThreadID:  "1700000000100",
		Metadata: map[string]string{
			"teams_conversation_id": "19:chan-1@thread.tacv2;messageid=1700000000100",
		},
	}
	require.NoError(t, client.DeliverInbound(context.Background(), "scion.project.p1.agent.coder.messages", threadedMsg))

	mu.Lock()
	require.Len(t, gotBodies, 1)
	for _, field := range []string{"surface", "external_ref", "parent_ref", "display_name", "legacy_ref"} {
		_, hasField := gotBodies[0][field]
		assert.False(t, hasField, "older hub (no flag) must omit %q", field)
	}
	gotBodies = nil
	mu.Unlock()

	// Enable hub_inbound_conversation (P4+/P5a hub).
	client.SetHubInboundConversation(true)

	// Threaded channel conversation includes surface, external_ref, parent_ref, and legacy_ref.
	require.NoError(t, client.DeliverInbound(context.Background(), "scion.project.p1.agent.coder.messages", threadedMsg))

	// Non-threaded conversation includes surface, external_ref, and legacy_ref without parent_ref.
	topLevelMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello top-level",
		Type:      messages.TypeInstruction,
		Channel:   "teams",
		ThreadID:  "19:chan-2@thread.tacv2",
		Metadata: map[string]string{
			"teams_conversation_id": "19:chan-2@thread.tacv2",
		},
	}
	require.NoError(t, client.DeliverInbound(context.Background(), "scion.project.p1.agent.coder.messages", topLevelMsg))

	// Bare message (empty metadata and ThreadID) still always sets surface:"teams".
	bareMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello bare",
		Type:      messages.TypeInstruction,
		Channel:   "teams",
	}
	require.NoError(t, client.DeliverInbound(context.Background(), "scion.project.p1.agent.coder.messages", bareMsg))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotBodies, 3)

	assert.Equal(t, "teams", gotBodies[0]["surface"])
	assert.Equal(t, "19:chan-1@thread.tacv2;messageid=1700000000100", gotBodies[0]["external_ref"])
	assert.Equal(t, "19:chan-1@thread.tacv2", gotBodies[0]["parent_ref"])
	assert.Equal(t, "1700000000100", gotBodies[0]["legacy_ref"])

	assert.Equal(t, "teams", gotBodies[1]["surface"])
	assert.Equal(t, "19:chan-2@thread.tacv2", gotBodies[1]["external_ref"])
	_, hasParent := gotBodies[1]["parent_ref"]
	assert.False(t, hasParent)
	assert.Equal(t, "19:chan-2@thread.tacv2", gotBodies[1]["legacy_ref"])

	assert.Equal(t, "teams", gotBodies[2]["surface"])
	_, hasExt := gotBodies[2]["external_ref"]
	assert.False(t, hasExt)
	_, hasLegacy := gotBodies[2]["legacy_ref"]
	assert.False(t, hasLegacy)
}

// TestTeamsBroker_Inbound_LegacyRefContinuity verifies through handleMessage
// that both a channel thread reply (replyToId) and a root post (stripped
// conversation ID) emit surface:"teams" and legacy_ref equal to the exact
// pre-P4 message.thread_id value.
func TestTeamsBroker_Inbound_LegacyRefContinuity(t *testing.T) {
	var mu sync.Mutex
	var gotBodies []map[string]any

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		gotBodies = append(gotBodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"delivered":true}`))
	}))
	defer hubSrv.Close()

	broker := NewBroker(slog.Default())
	configureBrokerWithAPI(t, broker, hubSrv.URL)
	broker.hubClient = NewHubClient(hubSrv.URL, "", "", slog.Default())
	broker.hubClient.SetHubInboundConversation(true)

	ctx := context.Background()
	require.NoError(t, broker.store.CreateChannelLink(ctx, &ChannelLink{
		ConversationID: "19:chan-1@thread.tacv2",
		ProjectID:      "p1",
		DefaultAgent:   "coder",
		Active:         true,
	}))
	require.NoError(t, broker.store.CreateUserMapping(ctx, &TeamsUserMapping{
		TeamsUserID: "aad-alice",
		ScionUserID: "user-alice",
		ScionEmail:  "alice@example.com",
	}))

	// 1. Thread reply: ReplyToID is set; legacy_ref == message.thread_id == replyToId.
	require.NoError(t, broker.handleMessage(ctx, &Activity{
		Type:      "message",
		ID:        "act-reply-1",
		ReplyToID: "1700000000100",
		Text:      "reply in existing thread",
		From:      ChannelAccount{ID: "teams-u1", AadObjectID: "aad-alice", Name: "Alice"},
		Conversation: ConversationAccount{
			ID: "19:chan-1@thread.tacv2;messageid=1700000000100",
		},
	}))

	// 2. Root post: ReplyToID is empty; legacy_ref == message.thread_id == stripped convID.
	require.NoError(t, broker.handleMessage(ctx, &Activity{
		Type: "message",
		ID:   "act-root-1",
		Text: "root post in channel",
		From: ChannelAccount{ID: "teams-u1", AadObjectID: "aad-alice", Name: "Alice"},
		Conversation: ConversationAccount{
			ID: "19:chan-1@thread.tacv2;messageid=1700000000200",
		},
	}))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotBodies, 2)

	msg0 := gotBodies[0]["message"].(map[string]any)
	assert.Equal(t, "teams", gotBodies[0]["surface"])
	assert.Equal(t, "19:chan-1@thread.tacv2;messageid=1700000000100", gotBodies[0]["external_ref"])
	assert.Equal(t, "19:chan-1@thread.tacv2", gotBodies[0]["parent_ref"])
	assert.Equal(t, "1700000000100", gotBodies[0]["legacy_ref"])
	assert.Equal(t, "1700000000100", msg0["thread_id"])

	msg1 := gotBodies[1]["message"].(map[string]any)
	assert.Equal(t, "teams", gotBodies[1]["surface"])
	assert.Equal(t, "19:chan-1@thread.tacv2;messageid=1700000000200", gotBodies[1]["external_ref"])
	assert.Equal(t, "19:chan-1@thread.tacv2", gotBodies[1]["parent_ref"])
	assert.Equal(t, "19:chan-1@thread.tacv2", gotBodies[1]["legacy_ref"])
	assert.Equal(t, "19:chan-1@thread.tacv2", msg1["thread_id"])
}

type hubConvStore struct {
	store.Store
	convs map[string]*store.Conversation
}

func (s *hubConvStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	c, ok := s.convs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

type teamsForwardingBus struct {
	broker *TeamsBroker
}

func (f *teamsForwardingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	// Round-trip through the real gRPC proto conversion so the Teams plugin
	// receives the exact wire shape emitted by a real hub.
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	return f.broker.Publish(ctx, topic, wire)
}

func (f *teamsForwardingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *teamsForwardingBus) Close() error { return nil }

// TestTeamsBroker_Publish_HubEnrichedOptionAShapes drives the Teams broker
// from REAL hub-enriched shapes produced by hub.MessageBrokerProxy (backed by
// real store.Conversation rows and round-tripped through grpcbroker proto
// conversion):
//  1. External Teams thread reply (Kind:"group", Surface:"teams", translated ExternalRef, empty ThreadID)
//  2. User DM reply (Kind:"direct", Surface:"native", empty ExternalRef) routed by Recipient,
//     ignoring any legacy ThreadID when Conversation is present
//  3. User DM reply for an unmapped user does not broadcast to public project channel links
//  4. Native-surface user-addressed group conversation for an unmapped user does not broadcast
//  5. Agent-to-agent observer DM (Kind:"direct", Surface:"native") still delivers to channel links
//  6. Defense-in-depth: Kind:"direct" with unexpected non-empty ExternalRef ignores ExternalRef
//  7. Pre-P5a hub (Conversation == nil) with dm: ThreadID for unmapped user does not broadcast
func TestTeamsBroker_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	var mu sync.Mutex
	var delivered []string

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delivered = append(delivered, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ActivityResponse{ID: "resp-1"})
	}))
	defer apiServer.Close()

	broker := NewBroker(slog.Default())
	configureBrokerWithAPI(t, broker, apiServer.URL)
	ctx := context.Background()

	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "19:chan-1@thread.tacv2;messageid=100",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))
	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "a:dm-alice",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))
	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "19:public-chan@thread.tacv2",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))
	require.NoError(t, broker.store.CreateChannelLink(ctx, &ChannelLink{
		ConversationID:   "19:public-chan@thread.tacv2",
		ProjectID:        "p1",
		Active:           true,
		ShowAgentToAgent: true,
	}))
	require.NoError(t, broker.store.CreateUserMapping(ctx, &TeamsUserMapping{
		TeamsUserID: "aad-alice",
		ScionUserID: "user-alice",
		ScionEmail:  "alice@example.com",
	}))
	require.NoError(t, broker.store.SetConversationContext(ctx, &ConversationContext{
		TeamsUserID:        "aad-alice",
		ProjectID:          "p1",
		AgentSlug:          "coder",
		LastConversationID: "a:dm-alice",
		LastMessageAt:      time.Now(),
	}))

	projectID := "p1"
	threadKey, threadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "19:chan-1@thread.tacv2;messageid=100",
		ProjectID: projectID,
	})
	require.NoError(t, err)
	dmKey, dmKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      "11111111-1111-1111-1111-111111111111",
		RecipientKind: "user",
		RecipientID:   "22222222-2222-2222-2222-222222222222",
	})
	require.NoError(t, err)
	a2aKey, a2aKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    "agent",
		SenderID:      "11111111-1111-1111-1111-111111111111",
		RecipientKind: "agent",
		RecipientID:   "33333333-3333-3333-3333-333333333333",
	})
	require.NoError(t, err)

	convStore := &hubConvStore{
		convs: map[string]*store.Conversation{
			"conv-teams-thread": {
				ID:          "conv-teams-thread",
				ProjectID:   &projectID,
				Kind:        threadKind,
				Surface:     "teams",
				ExternalRef: threadKey,
				ParentRef:   "19:chan-1@thread.tacv2",
				DisplayName: "General",
			},
			"conv-dm-alice": {
				ID:          "conv-dm-alice",
				ProjectID:   &projectID,
				Kind:        dmKind,
				Surface:     "native",
				ExternalRef: dmKey,
				DisplayName: "Direct Message",
			},
			"conv-native-group": {
				ID:          "conv-native-group",
				ProjectID:   &projectID,
				Kind:        "group",
				Surface:     "native",
				ExternalRef: "thread:p1:native-topic-1",
				DisplayName: "Native Topic",
			},
			"conv-dm-a2a": {
				ID:          "conv-dm-a2a",
				ProjectID:   &projectID,
				Kind:        a2aKind,
				Surface:     "native",
				ExternalRef: a2aKey,
			},
		},
	}

	proxy := hub.NewMessageBrokerProxy(&teamsForwardingBus{broker: broker}, convStore, nil, nil, slog.Default())

	// 1. Outbound Teams thread reply enriched by MessageBrokerProxy (empty ThreadID).
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-alice", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "reply on teams thread",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ConversationID: "conv-teams-thread",
	}))

	// 2. Outbound DM reply enriched by MessageBrokerProxy: Conversation is
	// {Surface:"native", Kind:"direct"} with empty ExternalRef. Bogus ThreadID
	// pointing at a real conversation ref must be ignored; routes via Recipient.
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-alice", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "dm reply via teams affinity",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ThreadID:       "19:chan-1@thread.tacv2;messageid=100",
		ConversationID: "conv-dm-alice",
	}))

	// 3. Outbound DM reply to an unmapped user must not broadcast to public channel links.
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-bob", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:bob@example.com",
		Msg:            "private dm for unmapped user",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ConversationID: "conv-dm-alice",
	}))

	// 4. Native-surface user-addressed group conversation for an unmapped user
	// must never broadcast to public project channel links (O1).
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-bob", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:bob@example.com",
		Msg:            "native group message for unmapped user",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ConversationID: "conv-native-group",
	}))

	// 5. Outbound agent-to-agent observer DM still delivers to channel links.
	require.NoError(t, proxy.PublishMessage(ctx, projectID, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "agent-to-agent observer dm",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-dm-a2a",
	}))

	// 6. Defense-in-depth: Kind:"direct" with unexpected non-empty ExternalRef
	// ignores ExternalRef and routes via Recipient.
	require.NoError(t, broker.Publish(ctx, "scion.project.p1.user.user-alice.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "direct with unexpected external_ref",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ConversationID: "conv-direct-guard",
		Conversation: &messages.Conversation{
			Surface:     "teams",
			Kind:        "direct",
			ExternalRef: "19:chan-1@thread.tacv2;messageid=100",
			ParentRef:   "19:chan-1@thread.tacv2",
		},
	}))

	// 7. Version-skew guard: pre-P5a hub (Conversation == nil) with dm: ThreadID
	// for an unmapped user must not broadcast to public channel links.
	require.NoError(t, broker.Publish(ctx, "scion.project.p1.user.user-bob.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:bob@example.com",
		Msg:       "pre-p5a dm for unmapped user",
		Type:      messages.TypeInstruction,
		Channel:   "teams",
		ThreadID:  dmKey,
	}))

	broker.mu.Lock()
	sq := broker.sendQueue
	broker.mu.Unlock()
	sq.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, delivered, 4)
	assert.Equal(t, 1, countPath(delivered, "/v3/conversations/19:chan-1@thread.tacv2;messageid=100/activities"))
	assert.Equal(t, 2, countPath(delivered, "/v3/conversations/a:dm-alice/activities"))
	assert.Equal(t, 1, countPath(delivered, "/v3/conversations/19:public-chan@thread.tacv2/activities"))
}

func countPath(paths []string, target string) int {
	n := 0
	for _, p := range paths {
		if p == target {
			n++
		}
	}
	return n
}

// TestTeamsBroker_Publish_PreUpgradeReplyToIDNeverPostsToGuessedTarget (R-FV1)
// verifies that a conv:<id> reply into a pre-upgrade Teams thread:p:<replyToId>
// row (translated to ExternalRef="<replyToId>" with empty ThreadID) never posts
// to "<replyToId>", instead falling through to the project's linked channel.
func TestTeamsBroker_Publish_PreUpgradeReplyToIDNeverPostsToGuessedTarget(t *testing.T) {
	var mu sync.Mutex
	var delivered []string

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delivered = append(delivered, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ActivityResponse{ID: "resp-1"})
	}))
	defer apiServer.Close()

	broker := NewBroker(slog.Default())
	configureBrokerWithAPI(t, broker, apiServer.URL)
	ctx := context.Background()

	require.NoError(t, broker.store.UpsertConversationReference(ctx, &ConversationReference{
		ConversationID: "19:linked-chan@thread.tacv2",
		ServiceURL:     apiServer.URL,
		UpdatedAt:      time.Now(),
	}))
	require.NoError(t, broker.store.CreateChannelLink(ctx, &ChannelLink{
		ConversationID: "19:linked-chan@thread.tacv2",
		ProjectID:      "p1",
		Active:         true,
	}))

	require.NoError(t, broker.Publish(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "conv:<id> reply into pre-upgrade teams replyToId row",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ConversationID: "conv-legacy-teams",
		Conversation: &messages.Conversation{
			Surface:     "teams",
			ExternalRef: "1700000000100",
		},
	}))

	broker.mu.Lock()
	sq := broker.sendQueue
	broker.mu.Unlock()
	sq.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, delivered, 1)
	assert.Equal(t, "/v3/conversations/19:linked-chan@thread.tacv2/activities", delivered[0])
}
