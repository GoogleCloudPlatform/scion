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
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

	// 0. L2: With no chatRoutes or topicChats configured, Priority 0
	// conversation-chat posting is default-denied.
	err := b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "v1 default-deny without routes",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "-100111222",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	assert.Empty(t, tgSrv.sentMessages, "v1 must default-deny conversation chat when no routes are configured")
	tgSrv.mu.Unlock()

	// 1. Once a route is configured for -100111222, Conversation routes to it,
	// while an unconfigured chat is still denied.
	b.chatRoutes = map[int64]string{
		-100111222: "scion.project.other.>",
	}
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
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

	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice",
		Msg:       "v1 unrouted chat denied",
		Type:      messages.TypeInstruction,
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "-100999000",
		},
	})
	require.NoError(t, err)
	tgSrv.mu.Lock()
	assert.Len(t, tgSrv.sentMessages, 1, "unrouted conversation chat must be denied")
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

	// 1. Without hub_inbound_conversation flag (older hub): even threaded
	// deliveries must omit surface, external_ref, parent_ref, display_name.
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

	mu.Lock()
	require.Len(t, gotInbound, 2)
	for i := 0; i < 2; i++ {
		for _, field := range []string{"surface", "external_ref", "parent_ref", "display_name", "legacy_ref"} {
			_, hasField := gotInbound[i][field]
			assert.False(t, hasField, "older hub (no flag) must omit %q", field)
		}
	}
	gotInbound = nil
	mu.Unlock()

	// 2. With hub_inbound_conversation enabled (P4+/P5a hub):
	// V2 threaded inbound (deliverInbound and deliverInboundOutcome).
	b2.hubInboundConversation = true
	b2.deliverInbound("scion.project.p1.agent.coder.messages", threadedMsg)
	status, errMsg, answered = b2.deliverInboundOutcome(context.Background(), "scion.project.p1.agent.coder.messages", threadedMsg, nil)
	assert.Equal(t, http.StatusOK, status)
	assert.Empty(t, errMsg)
	assert.True(t, answered)

	// V1 non-threaded inbound (deliverInbound) with flag enabled still sends
	// surface:"telegram" (P5 always-surface rule) while omitting external_ref,
	// parent_ref, and legacy_ref so 1:1 private chats resolve via Phase 5 DM.
	b1 := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b1.hubURL = hubSrv.URL
	b1.hubInboundConversation = true

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

	// Both V2 threaded deliveries include surface, external_ref "<thread>"
	// (matching msg.ThreadID so pre-P4 rows and outbound --thread-id share one
	// conversation), parent_ref "<chat>", and legacy_ref "<thread>".
	for i := 0; i < 2; i++ {
		assert.Equal(t, "telegram", gotInbound[i]["surface"])
		assert.Equal(t, "42", gotInbound[i]["external_ref"])
		assert.Equal(t, "-100123456", gotInbound[i]["parent_ref"])
		assert.Equal(t, "42", gotInbound[i]["legacy_ref"])
	}

	// Non-threaded / private-chat delivery always sends surface:"telegram" and
	// omits external_ref, parent_ref, and legacy_ref so the hub resolves it
	// through Phase 5 as a direct dm: conversation (matching `scion message user:<x>`).
	assert.Equal(t, "telegram", gotInbound[2]["surface"])
	_, hasExtRef := gotInbound[2]["external_ref"]
	_, hasParent := gotInbound[2]["parent_ref"]
	_, hasLegacy := gotInbound[2]["legacy_ref"]
	assert.False(t, hasExtRef, "non-threaded private chat should omit external_ref")
	assert.False(t, hasParent, "non-threaded private chat should omit parent_ref")
	assert.False(t, hasLegacy, "non-threaded private chat should omit legacy_ref")
}

// TestTelegramBroker_Publish_HubEnrichedOptionAShapes drives Telegram v2 with
// the exact outbound shapes emitted by MessageBrokerProxy.enrichConversation
// under Option A: (1) forum topic reply with translated "<chat>:<thread>",
// (2) outbound `--channel telegram --thread-id 42` with translated bare "42",
// and (3) DM reply with Conversation omitted (nil).
func TestTelegramBroker_Publish_HubEnrichedOptionAShapes(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	defer tgSrv.srv.Close()

	b := NewV2(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.api = NewAPIClient("test-token", tgSrv.srv.URL)
	topic := "scion.project.p1.user.u1.messages"

	// 1. Outbound `--channel telegram --thread-id -100123456:42` after Option A translation.
	err := b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "reply on telegram forum topic",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ThreadID:       "-100123456:42",
		ConversationID: "conv-tg-topic",
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "-100123456:42",
			ParentRef:   "-100123456",
		},
	})
	require.NoError(t, err)

	// 2. Outbound `--channel telegram --thread-id 42` (bare topic ID) and
	// conv:<id> reply into a pre-upgrade thread:p:42 row (empty ThreadID, empty
	// ParentRef): never posts to chat ID 42; resolves chat via metadata/links
	// with MessageThreadID=42.
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "conv:<id> reply into pre-upgrade bare topic id",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ConversationID: "conv-tg-bare-topic",
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "42",
		},
		Metadata: map[string]string{
			"telegram_chat_id": "-100123456",
		},
	})
	require.NoError(t, err)

	// 2b. Composed (ParentRef, ExternalRef) form from new inbound forum topics.
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "reply composed from parent_ref + external_ref",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ConversationID: "conv-tg-parent-topic",
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			ExternalRef: "42",
			ParentRef:   "-100123456",
		},
	})
	require.NoError(t, err)

	// 3. Outbound DM reply with Conversation omitted (nil) and affinity Channel:"telegram".
	err = b.Publish(context.Background(), topic, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "dm reply via telegram",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ThreadID:       "dm:agent:a1:user:u1",
		ConversationID: "conv-dm-1",
		Conversation:   nil,
		Metadata: map[string]string{
			"telegram_chat_id": "998877",
		},
	})
	require.NoError(t, err)

	tgSrv.mu.Lock()
	defer tgSrv.mu.Unlock()
	require.Len(t, tgSrv.sentMessages, 4)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[0].ChatID)
	assert.Equal(t, int64(42), tgSrv.sentMessages[0].MessageThreadID)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[1].ChatID)
	assert.Equal(t, int64(42), tgSrv.sentMessages[1].MessageThreadID)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[2].ChatID)
	assert.Equal(t, int64(42), tgSrv.sentMessages[2].MessageThreadID)
	assert.Equal(t, int64(998877), tgSrv.sentMessages[3].ChatID)
	assert.Equal(t, int64(0), tgSrv.sentMessages[3].MessageThreadID)
}

// TestTelegramBrokerV2_Publish_Priority0RequiresActiveGroupLink verifies that
// Priority 0 only posts to the conversation's ParentRef chat when that chat is
// an active GroupLink for the message's project; an unlinked, inactive, or
// other-project chat receives no post.
func TestTelegramBrokerV2_Publish_Priority0RequiresActiveGroupLink(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	defer tgSrv.srv.Close()

	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "tg-link-guard.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	b := NewV2(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.api = NewAPIClient("test-token", tgSrv.srv.URL)
	b.store = st

	ctx := context.Background()
	topic := "scion.project.p1.user.u1.messages"
	convReply := func(text string) *messages.StructuredMessage {
		return &messages.StructuredMessage{
			Version:        messages.Version,
			Sender:         "agent:coder",
			Recipient:      "user:alice",
			Msg:            text,
			Type:           messages.TypeInstruction,
			Channel:        "telegram",
			ConversationID: "conv-forum-42",
			Conversation: &messages.Conversation{
				Surface:     "telegram",
				ExternalRef: "42",
				ParentRef:   "-100123456",
			},
		}
	}

	// 1. Unlinked chat: no post.
	require.NoError(t, b.Publish(ctx, topic, convReply("to unlinked chat")))
	tgSrv.mu.Lock()
	assert.Empty(t, tgSrv.sentMessages, "unlinked chat must not receive Priority 0 post")
	tgSrv.mu.Unlock()

	// 2. Inactive GroupLink for project p1: no post.
	require.NoError(t, st.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100123456,
		ProjectID: "p1",
		Active:    false,
	}))
	require.NoError(t, b.Publish(ctx, topic, convReply("to inactive chat")))
	tgSrv.mu.Lock()
	assert.Empty(t, tgSrv.sentMessages, "inactive GroupLink must not receive Priority 0 post")
	tgSrv.mu.Unlock()

	// 3. Active GroupLink for a different project: no post.
	require.NoError(t, st.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100123456,
		ProjectID: "other-project",
		Active:    true,
	}))
	require.NoError(t, b.Publish(ctx, topic, convReply("to other project chat")))
	tgSrv.mu.Lock()
	assert.Empty(t, tgSrv.sentMessages, "GroupLink for another project must not receive Priority 0 post")
	tgSrv.mu.Unlock()

	// 4. Active GroupLink for project p1: delivered to chat -100123456 topic 42.
	require.NoError(t, st.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100123456,
		ProjectID: "p1",
		Active:    true,
	}))
	require.NoError(t, b.Publish(ctx, topic, convReply("to active linked chat")))
	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 1)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[0].ChatID)
	assert.Equal(t, int64(42), tgSrv.sentMessages[0].MessageThreadID)
	tgSrv.mu.Unlock()
}

// TestTelegramBrokerV2_Publish_Priority0RefusedZerosConvThreadID (L1) verifies
// that when the Priority-0 conversation chat is refused (unlinked/inactive/
// other-project), convThreadID is zeroed so the refused chat's forum topic ID
// is never attached to a fallback chat.
func TestTelegramBrokerV2_Publish_Priority0RefusedZerosConvThreadID(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	defer tgSrv.srv.Close()

	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "tg-l1-guard.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// Active fallback GroupLink for project p1 (different chat from the refused one).
	require.NoError(t, st.SaveGroupLink(ctxOrBg(), &GroupLink{
		ChatID:    -100777888,
		ProjectID: "p1",
		Active:    true,
	}))

	b := NewV2(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.api = NewAPIClient("test-token", tgSrv.srv.URL)
	b.store = st

	err = b.Publish(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice",
		Msg:            "refused forum chat falls back without topic id",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ConversationID: "conv-unlinked-forum",
		Conversation: &messages.Conversation{
			Surface:     "telegram",
			Kind:        "group",
			ExternalRef: "42",
			ParentRef:   "-100123456", // unlinked chat -> Priority 0 refuses
		},
	})
	require.NoError(t, err)

	tgSrv.mu.Lock()
	defer tgSrv.mu.Unlock()
	require.Len(t, tgSrv.sentMessages, 1)
	assert.Equal(t, int64(-100777888), tgSrv.sentMessages[0].ChatID)
	assert.Equal(t, int64(0), tgSrv.sentMessages[0].MessageThreadID,
		"refused Priority-0 chat must zero convThreadID before fallback delivery")
}

func ctxOrBg() context.Context { return context.Background() }

type telegramHubConvStore struct {
	store.Store
	convs map[string]*store.Conversation
}

func (s *telegramHubConvStore) GetConversation(_ context.Context, id string) (*store.Conversation, error) {
	c, ok := s.convs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return c, nil
}

type telegramForwardingBus struct {
	broker *TelegramBrokerV2
}

func (f *telegramForwardingBus) Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	wire := grpcbroker.ProtoToStructuredMessage(grpcbroker.StructuredMessageToProto(msg))
	return f.broker.Publish(ctx, topic, wire)
}

func (f *telegramForwardingBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, nil
}

func (f *telegramForwardingBus) Close() error { return nil }

// TestTelegramBrokerV2_Publish_HubEnrichedP5Contract drives TelegramBrokerV2
// from REAL hub.MessageBrokerProxy enrichment (backed by real store.Conversation
// rows and round-tripped through grpcbroker proto conversion):
//  1. External Telegram forum topic reply (Kind:"group", translated ExternalRef, empty ThreadID)
//  2. Direct user DM (Kind:"direct", Surface:"native", empty ExternalRef) routed by
//     Recipient while ignoring a bogus/stale ThreadID when Conversation is present
//  3. Unmapped user DM does not broadcast to public project group links
//  4. Native-surface group addressed to an unmapped user does not broadcast to public links (O1)
//  5. Agent-to-agent observer DM (Kind:"direct", Surface:"native") still delivers to
//     ShowAgentToAgent group links only
//  6. Pre-P5a hub (Conversation == nil, dm: ThreadID) for unmapped user does not broadcast
func TestTelegramBrokerV2_Publish_HubEnrichedP5Contract(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	defer tgSrv.srv.Close()

	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "tg-p5.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	projectID := "p1"

	require.NoError(t, st.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "555001",
		ScionEmail:     "alice@example.com",
		ScionUserID:    "22222222-2222-2222-2222-222222222222",
	}))
	require.NoError(t, st.SaveConversationContext(ctx, &ConversationContext{
		TelegramUserID: "555001",
		ProjectID:      projectID,
		AgentSlug:      "coder",
		LastChatID:     555001,
	}))
	require.NoError(t, st.SaveGroupLink(ctx, &GroupLink{
		ChatID:           -100123456,
		ProjectID:        projectID,
		Active:           true,
		ShowAgentToAgent: true,
	}))
	require.NoError(t, st.SaveGroupLink(ctx, &GroupLink{
		ChatID:           -100999888,
		ProjectID:        projectID,
		Active:           true,
		ShowAgentToAgent: false,
	}))

	b := NewV2(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.api = NewAPIClient("test-token", tgSrv.srv.URL)
	b.store = st
	b.hubClient = &fakeHubClient{
		projects: []ProjectOption{{ID: projectID, Name: "Project 1"}},
	}

	threadKey, threadKind, _, err := messaging.DeriveConversationKey(messaging.KeyInputs{
		ThreadID:  "-100123456:42",
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

	convStore := &telegramHubConvStore{
		convs: map[string]*store.Conversation{
			"conv-tg-topic": {
				ID:          "conv-tg-topic",
				ProjectID:   &projectID,
				Kind:        threadKind,
				Surface:     "telegram",
				ExternalRef: threadKey,
				ParentRef:   "-100123456",
				DisplayName: "Eng Forum / Topic 42",
			},
			"conv-dm-alice": {
				ID:          "conv-dm-alice",
				ProjectID:   &projectID,
				Kind:        dmKind,
				Surface:     "native",
				ExternalRef: dmKey,
				DisplayName: "Direct Message",
			},
			"conv-dm-a2a": {
				ID:          "conv-dm-a2a",
				ProjectID:   &projectID,
				Kind:        a2aKind,
				Surface:     "native",
				ExternalRef: a2aKey,
			},
			"conv-native-group": {
				ID:          "conv-native-group",
				ProjectID:   &projectID,
				Kind:        "group",
				Surface:     "native",
				ExternalRef: "thread:" + projectID + ":native-topic",
				DisplayName: "Native Topic",
			},
		},
	}

	proxy := hub.NewMessageBrokerProxy(&telegramForwardingBus{broker: b}, convStore, nil, nil, slog.Default())

	// 1. External forum topic reply (no legacy ThreadID needed).
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-alice", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "reply on telegram forum topic",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ConversationID: "conv-tg-topic",
	}))

	// 2. Direct user DM with bogus/stale ThreadID: must ignore ThreadID and route to Alice's DM.
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-alice", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:alice@example.com",
		Msg:            "dm reply ignoring stale thread_id",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ThreadID:       "-100123456:42",
		ConversationID: "conv-dm-alice",
	}))

	// 3. Unmapped user DM must not broadcast to public project group links.
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-bob", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:bob@example.com",
		Msg:            "private dm for unmapped user",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ConversationID: "conv-dm-alice",
	}))

	// 4. Native-surface group addressed to unmapped user must not broadcast (O1).
	require.NoError(t, proxy.PublishUserMessage(ctx, projectID, "user-bob", &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "user:bob@example.com",
		Msg:            "native group for unmapped user",
		Type:           messages.TypeInstruction,
		Channel:        "telegram",
		ConversationID: "conv-native-group",
	}))

	// 5. Agent-to-agent observer DM delivers only to ShowAgentToAgent link (-100123456).
	require.NoError(t, proxy.PublishMessage(ctx, projectID, &messages.StructuredMessage{
		Version:        messages.Version,
		Sender:         "agent:coder",
		Recipient:      "agent:reviewer",
		Msg:            "agent-to-agent observer dm",
		Type:           messages.TypeInstruction,
		ObserverOnly:   true,
		ConversationID: "conv-dm-a2a",
	}))

	tgSrv.mu.Lock()
	require.Len(t, tgSrv.sentMessages, 3)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[0].ChatID)
	assert.Equal(t, int64(42), tgSrv.sentMessages[0].MessageThreadID)
	assert.Equal(t, int64(555001), tgSrv.sentMessages[1].ChatID)
	assert.Equal(t, int64(0), tgSrv.sentMessages[1].MessageThreadID)
	assert.Equal(t, int64(-100123456), tgSrv.sentMessages[2].ChatID)
	assert.Equal(t, int64(0), tgSrv.sentMessages[2].MessageThreadID)
	tgSrv.mu.Unlock()

	// 6. Pre-P5a hub (Conversation == nil, dm: ThreadID) for unmapped user must not broadcast.
	require.NoError(t, b.Publish(ctx, "scion.project.p1.user.u2.messages", &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:bob@example.com",
		Msg:       "pre-p5a dm for unmapped user",
		Type:      messages.TypeInstruction,
		Channel:   "telegram",
		ThreadID:  dmKey,
	}))
	tgSrv.mu.Lock()
	assert.Len(t, tgSrv.sentMessages, 3, "pre-P5a dm: ThreadID for unmapped user must not broadcast")
	tgSrv.mu.Unlock()
}
