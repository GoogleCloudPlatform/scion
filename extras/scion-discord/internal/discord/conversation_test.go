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

package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestDiscordBroker_Publish_ConversationRoutingAndFallback(t *testing.T) {
	f := newDMRouteFixture(t)
	topic := "scion.project.p1.user.u1.messages"

	// 1. Conversation field (Phase 11) with empty legacy Channel/ThreadID routes to Discord.
	err := f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:" + dmTestEmail,
		Msg:            "phase 11 message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "discord",
			ExternalRef: "555555555555555555",
			ParentRef:   "200000000000000001",
			DisplayName: "#eng-thread",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"555555555555555555"}, sentChannelIDs(f.transport))

	// 2. Conversation targeting another surface ("slack") is ignored.
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:" + dmTestEmail,
		Msg:       "slack message",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "slack",
			ExternalRef: "C0123ABC:1700000000.000100",
		},
	})
	require.NoError(t, err)
	assert.Len(t, sentChannelIDs(f.transport), 1)

	// 3. Defeat scenario (a): reply affinity — Channel:"discord" + native DM
	// conversation + Recipient:user must deliver to the user's Discord channel
	// (not be dropped by Surface:"native" and not try to post to "dm:...").
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:" + dmTestEmail,
		Msg:            "reply affinity DM",
		Type:           messages.TypeInstruction,
		Channel:        "discord",
		ConversationID: "conv-dm-affinity",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:user:u1",
		},
	})
	require.NoError(t, err)
	assert.Len(t, sentChannelIDs(f.transport), 2)

	// 3b. Foreign non-empty Channel ("web") is skipped by Discord.
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:" + dmTestEmail,
		Msg:       "webchat message",
		Type:      messages.TypeInstruction,
		Channel:   "web",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "thread:p1:topic-1",
		},
	})
	require.NoError(t, err)
	assert.Len(t, sentChannelIDs(f.transport), 2)

	// 4. Legacy fallback (older hub sends Channel/ThreadID without Conversation).
	err = f.broker.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:" + dmTestEmail,
		Msg:       "legacy hub message",
		Type:      messages.TypeInstruction,
		Channel:   "discord",
		ThreadID:  "666666666666666666",
	})
	require.NoError(t, err)
	got := sentChannelIDs(f.transport)
	require.Len(t, got, 3)
	assert.Equal(t, "555555555555555555", got[0])
	assert.Equal(t, "666666666666666666", got[2])
}

func TestDiscordBroker_Inbound_SendsSurfaceExternalRefParentRef(t *testing.T) {
	var mu sync.Mutex
	var gotInbound []map[string]any
	var gotRouted []map[string]any

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		switch r.URL.Path {
		case "/api/v1/broker/inbound":
			gotInbound = append(gotInbound, body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"delivered":true}`))
		case "/api/v1/broker/inbound/routed":
			gotRouted = append(gotRouted, body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"delivered":true,"primary_agent":"coder"}`))
		default:
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hubSrv.Close()

	b := NewBroker(discardLogger())
	b.hubURL = hubSrv.URL
	b.threadParents = map[string]string{
		"777777777777777777": "200000000000000001",
	}

	// Threaded channel: includes surface, external_ref, and parent_ref.
	threadMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello from thread",
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"discord_channel_id": "777777777777777777",
		},
	}

	require.Nil(t, b.deliverInbound("scion.project.p1.agent.coder.messages", threadMsg))
	res, he := b.deliverRoutedInbound("p1", "coder", threadMsg)
	require.Nil(t, he)
	require.NotNil(t, res)
	assert.True(t, res.Delivered)

	// Top-level channel (non-threaded): includes surface and external_ref, omits parent_ref.
	channelMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello from channel",
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"discord_channel_id": "200000000000000001",
		},
	}
	require.Nil(t, b.deliverInbound("scion.project.p1.agent.coder.messages", channelMsg))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotInbound, 2)
	assert.Equal(t, "discord", gotInbound[0]["surface"])
	assert.Equal(t, "777777777777777777", gotInbound[0]["external_ref"])
	assert.Equal(t, "200000000000000001", gotInbound[0]["parent_ref"])

	assert.Equal(t, "discord", gotInbound[1]["surface"])
	assert.Equal(t, "200000000000000001", gotInbound[1]["external_ref"])
	_, hasParent := gotInbound[1]["parent_ref"]
	assert.False(t, hasParent, "non-threaded channel should omit parent_ref")

	require.Len(t, gotRouted, 1)
	assert.Equal(t, "discord", gotRouted[0]["surface"])
	assert.Equal(t, "777777777777777777", gotRouted[0]["external_ref"])
	assert.Equal(t, "200000000000000001", gotRouted[0]["parent_ref"])
}
