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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

	// Threaded channel conversation includes surface, external_ref, and parent_ref.
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

	// Non-threaded conversation includes surface and external_ref without parent_ref.
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

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotBodies, 2)

	assert.Equal(t, "teams", gotBodies[0]["surface"])
	assert.Equal(t, "19:chan-1@thread.tacv2;messageid=1700000000100", gotBodies[0]["external_ref"])
	assert.Equal(t, "19:chan-1@thread.tacv2", gotBodies[0]["parent_ref"])

	assert.Equal(t, "teams", gotBodies[1]["surface"])
	assert.Equal(t, "19:chan-2@thread.tacv2", gotBodies[1]["external_ref"])
	_, hasParent := gotBodies[1]["parent_ref"]
	assert.False(t, hasParent)
}

// TestTeamsBroker_Publish_HubEnrichedOptionAShapes drives the Teams broker
// with the exact outbound shapes emitted by MessageBrokerProxy.enrichConversation
// under Option A: (1) an external thread reply with translated Teams
// conversation ID, and (2) a DM reply with Conversation omitted (nil).
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

	topic := "scion.project.p1.user.u1.messages"

	// 1. Outbound `--channel teams --thread-id 19:chan-1@thread.tacv2;messageid=100`
	// after Option A translation (thread:<proj>: stripped).
	require.NoError(t, broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "reply on teams thread",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ThreadID:       "19:chan-1@thread.tacv2;messageid=100",
		ConversationID: "conv-teams-thread",
		Conversation: &messages.Conversation{
			Surface:     "teams",
			ExternalRef: "19:chan-1@thread.tacv2;messageid=100",
			ParentRef:   "19:chan-1@thread.tacv2",
		},
	}))

	// 2. Outbound DM reply with Conversation omitted (nil) and affinity Channel:"teams".
	require.NoError(t, broker.Publish(ctx, topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "dm reply via teams",
		Type:           messages.TypeInstruction,
		Channel:        "teams",
		ConversationID: "conv-dm-1",
		Conversation:   nil,
		Metadata: map[string]string{
			"teams_conversation_id": "a:dm-alice",
		},
	}))

	broker.mu.Lock()
	sq := broker.sendQueue
	broker.mu.Unlock()
	sq.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, delivered, 2)
	assert.Contains(t, delivered, "/v3/conversations/19:chan-1@thread.tacv2;messageid=100/activities")
	assert.Contains(t, delivered, "/v3/conversations/a:dm-alice/activities")
}
