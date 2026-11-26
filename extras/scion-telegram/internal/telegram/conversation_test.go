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

package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestTelegramBrokerV2_Publish_ConversationRoutingAndFallback(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	defer tgSrv.srv.Close()

	b := NewV2(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.api = NewAPIClient("test-token", tgSrv.srv.URL)

	topic := "scion.project.p1.user.u1.messages"

	// 1a. Phase 11 Conversation with threaded ExternalRef ("<chatID>:<threadID>"),
	// empty legacy Channel/ThreadID/Metadata.
	err := b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "phase 11 threaded message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-1",
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "-100123456:42",
			ParentRef:   "-100123456",
			DisplayName: "Eng Forum / Topic 42",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 1)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[0].ChatID)
	assert.Equal(t, int64(42), tgSrv.sentMessages[0].MessageThreadID)
	tgSrv.mu.Unlock()

	// 1b. Phase 11 Conversation with non-threaded ExternalRef ("<chatID>").
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "phase 11 flat chat message",
		Type:           messages.TypeInstruction,
		ConversationID: "conv-2",
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "-100555666",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 2)
	assert.Equal(t, int64(-100555666), tgSrv.sentMessages[1].ChatID)
	assert.Equal(t, int64(0), tgSrv.sentMessages[1].MessageThreadID)
	tgSrv.mu.Unlock()

	// 2. Defeat scenario (c): Channel:"web" user DM with native DM conversation
	// is relayed by Telegram v2 (matching pre-P4 no-filter behavior) via
	// metadata/recipient routing without parsing "dm:..." as a Telegram chat ID.
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "web user DM relayed by telegram",
		Type:           messages.TypeInstruction,
		Channel:        "web",
		ThreadID:       "dm:agent:a1:user:u1",
		ConversationID: "conv-web-dm",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:user:u1",
		},
		Metadata: map[string]string{
			"telegram_chat_id": "-100777888",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 3)
	assert.Equal(t, int64(-100777888), tgSrv.sentMessages[2].ChatID)
	assert.Equal(t, int64(0), tgSrv.sentMessages[2].MessageThreadID)
	tgSrv.mu.Unlock()

	// 3. Foreign-surface ExternalRef (e.g. Discord snowflake) is never parsed
	// as a Telegram chat ID.
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "discord message without telegram route",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "discord",
			ExternalRef: "123456789012345678",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	assert.Len(t, tgSrv.sentMessages, 3)
	tgSrv.mu.Unlock()

	// 4. Legacy fallback (older hub sends Channel/ThreadID/Metadata without Conversation).
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "legacy hub message",
		Type:      messages.TypeInstruction,
		Channel:   "telegram",
		ThreadID:  "77",
		Metadata: map[string]string{
			"telegram_chat_id": "-100999888",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 4)
	assert.Equal(t, int64(-100999888), tgSrv.sentMessages[3].ChatID)
	assert.Equal(t, int64(77), tgSrv.sentMessages[3].MessageThreadID)
	tgSrv.mu.Unlock()
}

func TestTelegramBrokerV1_Publish_ConversationRoutingAndFallback(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	defer tgSrv.srv.Close()

	b := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.api = NewAPIClient("test-token", tgSrv.srv.URL)

	topic := "scion.project.p1.user.u1.messages"

	// 1. Conversation only routes to chat ID from ExternalRef.
	err := b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "v1 phase 11 message",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "-100111222",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 1)
	assert.Equal(t, int64(-100111222), tgSrv.sentMessages[0].ChatID)
	tgSrv.mu.Unlock()

	// 2. Defeat scenario (c): Channel:"web" user DM with native conversation
	// is relayed by Telegram v1 (no channel filter, matching pre-P4 behavior).
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "v1 web DM relayed",
		Type:      messages.TypeInstruction,
		Channel:   "web",
		ThreadID:  "dm:agent:a1:user:u1",
		Conversation: &messages.Conversation{
			Surface:     "native",
			ExternalRef: "dm:agent:a1:user:u1",
		},
		Metadata: map[string]string{
			"telegram_chat_id": "-100333444",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	assert.Len(t, tgSrv.sentMessages, 2)
	assert.Equal(t, int64(-100333444), tgSrv.sentMessages[1].ChatID)
	tgSrv.mu.Unlock()

	// 3. Legacy fallback via metadata.
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "v1 legacy message",
		Type:      messages.TypeInstruction,
		Channel:   "telegram",
		Metadata: map[string]string{
			"telegram_chat_id": "-100333444",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 3)
	assert.Equal(t, int64(-100333444), tgSrv.sentMessages[2].ChatID)
	tgSrv.mu.Unlock()
}

func TestTelegramBroker_Inbound_SendsSurfaceExternalRefParentRef(t *testing.T) {
	var mu sync.Mutex
	var gotInbound []map[string]any

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		gotInbound = append(gotInbound, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"delivered":true}`))
	}))
	defer hubSrv.Close()

	// V2 threaded inbound (deliverInbound and deliverInboundOutcome).
	b2 := NewV2(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b2.hubURL = hubSrv.URL

	threadedMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello from forum topic",
		Type:      messages.TypeInstruction,
		ThreadID:  "42",
		Metadata: map[string]string{
			"telegram_chat_id": "-100123456",
		},
	}
	b2.deliverInbound("scion.project.p1.agent.coder.messages", threadedMsg)
	status, errMsg, answered := b2.deliverInboundOutcome(context.Background(), "scion.project.p1.agent.coder.messages", threadedMsg, nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, errMsg)
	assert.True(t, answered)

	// V1 non-threaded inbound (deliverInbound).
	b1 := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b1.hubURL = hubSrv.URL

	flatMsg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello from flat chat",
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"telegram_chat_id": "-100555666",
		},
	}
	require.Nil(t, b1.deliverInbound("scion.project.p1.agent.coder.messages", flatMsg))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, gotInbound, 3)

	// Both V2 threaded deliveries include surface, external_ref "<chat>:<thread>", and parent_ref "<chat>".
	for i := 0; i < 2; i++ {
		assert.Equal(t, "telegram", gotInbound[i]["surface"])
		assert.Equal(t, "-100123456:42", gotInbound[i]["external_ref"])
		assert.Equal(t, "-100123456", gotInbound[i]["parent_ref"])
	}

	// Non-threaded delivery includes surface and external_ref "<chat>", omits parent_ref.
	assert.Equal(t, "telegram", gotInbound[2]["surface"])
	assert.Equal(t, "-100555666", gotInbound[2]["external_ref"])
	_, hasParent := gotInbound[2]["parent_ref"]
	assert.False(t, hasParent, "non-threaded chat should omit parent_ref")
}
