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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTelegramServer creates an httptest server that mimics the Telegram Bot API.
// It returns a getMe response and handles sendMessage and getUpdates.
type fakeTelegramServer struct {
	srv          *httptest.Server
	mu           sync.Mutex
	sentMessages []sendMessageRequest
	updates      []Update
}

func newFakeTelegramServer(t *testing.T) *fakeTelegramServer {
	t.Helper()
	f := &fakeTelegramServer{}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/bottest-token/getMe":
			resp := apiResponse{
				OK: true,
				Result: mustJSONRaw(t, BotUser{
					ID:        100,
					IsBot:     true,
					FirstName: "TestBot",
					Username:  "test_bot",
				}),
			}
			json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/bottest-token/sendMessage":
			var req sendMessageRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.sentMessages = append(f.sentMessages, req)
			f.mu.Unlock()

			resp := apiResponse{
				OK: true,
				Result: mustJSONRaw(t, TGMessage{
					MessageID: 1,
					Chat:      TGChat{ID: req.ChatID, Type: "private"},
					Text:      req.Text,
				}),
			}
			json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/bottest-token/getUpdates":
			f.mu.Lock()
			updates := f.updates
			f.updates = nil
			f.mu.Unlock()

			resp := apiResponse{
				OK:     true,
				Result: mustJSONRaw(t, updates),
			}
			json.NewEncoder(w).Encode(resp)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeTelegramServer) getSentMessages() []sendMessageRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]sendMessageRequest, len(f.sentMessages))
	copy(result, f.sentMessages)
	return result
}

func (f *fakeTelegramServer) setUpdates(updates []Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = updates
}

func mustJSONRaw(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func newTestBroker(t *testing.T, tgSrv *fakeTelegramServer) *TelegramBroker {
	t.Helper()
	b := New(slog.Default())
	t.Cleanup(func() { b.Close() })

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"plugin_name":  "test-telegram",
	})
	require.NoError(t, err)

	return b
}

func TestConfigure(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      "http://localhost:8080",
		"hmac_key":     "secret",
		"broker_id":    "broker-1",
		"plugin_name":  "my-telegram",
	})
	require.NoError(t, err)

	assert.Equal(t, "http://localhost:8080", b.hubURL)
	assert.Equal(t, "secret", b.hmacKey)
	assert.Equal(t, "broker-1", b.brokerID)
	assert.Equal(t, "my-telegram", b.pluginName)
	assert.NotNil(t, b.botInfo)
	assert.Equal(t, "test_bot", b.botInfo.Username)
}

func TestConfigure_MissingBotToken(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"hub_url": "http://localhost:8080",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bot_token is required")
}

func TestConfigure_InvalidBotToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := apiResponse{OK: false, Description: "Unauthorized", ErrorCode: 401}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "bad-token",
		"api_base_url": srv.URL,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to validate bot token")
}

func TestConfigure_WithChatRoutes(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	routes := `{"123": "scion.project.p1.agent.coder.messages", "-456": "scion.project.p1.broadcast"}`

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"chat_routes":  routes,
	})
	require.NoError(t, err)

	assert.Equal(t, "scion.project.p1.agent.coder.messages", b.chatRoutes[123])
	assert.Equal(t, "scion.project.p1.broadcast", b.chatRoutes[-456])
	assert.Contains(t, b.topicChats["scion.project.p1.agent.coder.messages"], int64(123))
}

func TestConfigure_WithOutboundRoutes(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":       "test-token",
		"api_base_url":    tgSrv.srv.URL,
		"chat_routes":     `{"-100": "scion.project.p1.agent.coder.messages"}`,
		"outbound_routes": `{"scion.project.p1.user.*.messages": "-100"}`,
	})
	require.NoError(t, err)

	// chat_routes populates both chatRoutes and topicChats
	assert.Equal(t, "scion.project.p1.agent.coder.messages", b.chatRoutes[-100])
	// outbound_routes adds to topicChats only
	assert.Contains(t, b.topicChats["scion.project.p1.user.*.messages"], int64(-100))
}

func TestConfigure_InvalidOutboundRoutes(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":       "test-token",
		"api_base_url":    tgSrv.srv.URL,
		"outbound_routes": `not-json`,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid outbound_routes")
}

func TestPublishViaOutboundRoute(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":       "test-token",
		"api_base_url":    tgSrv.srv.URL,
		"outbound_routes": `{"scion.project.p1.user.*.messages": "789"}`,
	})
	require.NoError(t, err)

	msg := messages.NewInstruction("agent:coder", "user:alice", "reply to user")
	err = b.Publish(context.Background(), "scion.project.p1.user.alice.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(789), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "reply to user")
}

func TestPublishToChat(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	// Add a chat route
	b.chatRoutes[789] = "scion.project.p1.agent.coder.messages"
	b.topicChats["scion.project.p1.agent.coder.messages"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello")
	err := b.Publish(context.Background(), "scion.project.p1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(789), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "hello")
	assert.Contains(t, sent[0].Text, "user:alice")
}

func TestPublishDedup(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	b.topicChats["test.topic"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello")

	// First publish should succeed
	require.NoError(t, b.Publish(context.Background(), "test.topic", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1)

	// Second publish of the same message should be deduplicated
	require.NoError(t, b.Publish(context.Background(), "test.topic", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1, "duplicate message should be skipped")

	// Third publish of the same message should also be deduplicated
	require.NoError(t, b.Publish(context.Background(), "test.topic", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1, "duplicate message should still be skipped")

	// A different message should go through
	msg2 := messages.NewInstruction("user:alice", "agent:coder", "different content")
	require.NoError(t, b.Publish(context.Background(), "test.topic", msg2))
	assert.Len(t, tgSrv.getSentMessages(), 2, "different message should be sent")
}

func TestPublishDedupExpiry(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	b.topicChats["test.topic"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello")

	// First publish
	require.NoError(t, b.Publish(context.Background(), "test.topic", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1)

	// Manually expire the dedup entry
	b.sentIDsMu.Lock()
	for k := range b.sentIDs {
		b.sentIDs[k] = time.Now().Add(-dedupTTL - time.Second)
	}
	b.sentIDsMu.Unlock()

	// Same message should now go through again
	require.NoError(t, b.Publish(context.Background(), "test.topic", msg))
	assert.Len(t, tgSrv.getSentMessages(), 2, "expired dedup entry should allow resend")
}

func TestMsgDedupKey(t *testing.T) {
	msg1 := messages.NewInstruction("user:alice", "agent:coder", "hello")
	msg2 := messages.NewInstruction("user:alice", "agent:coder", "hello")
	msg2.Timestamp = msg1.Timestamp // same timestamp

	// Same content → same key
	assert.Equal(t, msgDedupKey(msg1), msgDedupKey(msg2))

	// Different content → different key
	msg3 := messages.NewInstruction("user:alice", "agent:coder", "world")
	msg3.Timestamp = msg1.Timestamp
	assert.NotEqual(t, msgDedupKey(msg1), msgDedupKey(msg3))

	// Nil message → empty key
	assert.Equal(t, "", msgDedupKey(nil))

	// Empty body → empty key
	msg4 := &messages.StructuredMessage{}
	assert.Equal(t, "", msgDedupKey(msg4))
}

func TestPublishNoRoute(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	msg := messages.NewInstruction("user:alice", "agent:coder", "no route")
	err := b.Publish(context.Background(), "scion.project.unknown.agent.coder.messages", msg)
	require.NoError(t, err) // should not error, just drop

	sent := tgSrv.getSentMessages()
	assert.Empty(t, sent)
}

func TestPublishWithMetadataChatID(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	msg := messages.NewInstruction("user:alice", "agent:coder", "direct route")
	msg.Metadata = map[string]string{
		"telegram_chat_id": "999",
	}

	err := b.Publish(context.Background(), "any.topic", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(999), sent[0].ChatID)
}

func TestPublishPatternMatch(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	// Configure a wildcard route pattern
	b.chatRoutes[789] = "scion.project.p1.agent.*.messages"
	b.topicChats["scion.project.p1.agent.*.messages"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "pattern match")
	err := b.Publish(context.Background(), "scion.project.p1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(789), sent[0].ChatID)
}

func TestPublishClosed(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
	})
	require.NoError(t, err)

	require.NoError(t, b.Close())

	err = b.Publish(context.Background(), "test.topic", messages.NewInstruction("a", "b", "c"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestSubscribeUnsubscribe(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	require.NoError(t, b.Subscribe("scion.project.p1.agent.*.messages"))

	b.mu.RLock()
	assert.True(t, b.subs["scion.project.p1.agent.*.messages"])
	assert.NotNil(t, b.pollCancel, "polling should be started")
	b.mu.RUnlock()

	require.NoError(t, b.Unsubscribe("scion.project.p1.agent.*.messages"))

	b.mu.RLock()
	assert.False(t, b.subs["scion.project.p1.agent.*.messages"])
	b.mu.RUnlock()
}

func TestDoubleSubscribe(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	require.NoError(t, b.Subscribe("test.>"))
	require.NoError(t, b.Subscribe("test.>")) // idempotent

	b.mu.RLock()
	assert.Len(t, b.subs, 1)
	b.mu.RUnlock()
}

func TestUnsubscribeNonexistent(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)
	require.NoError(t, b.Unsubscribe("nonexistent.pattern"))
}

func TestClose(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
	})
	require.NoError(t, err)

	require.NoError(t, b.Subscribe("test.>"))
	require.NoError(t, b.Close())

	// Operations after close should fail
	err = b.Publish(context.Background(), "test.topic", messages.NewInstruction("a", "b", "c"))
	assert.Error(t, err)

	err = b.Subscribe("test.new")
	assert.Error(t, err)

	// Double close is safe
	require.NoError(t, b.Close())
}

func TestGetInfo(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	info, err := b.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "telegram", info.Name)
	assert.Equal(t, "0.1.0", info.Version)
	assert.Contains(t, info.Capabilities, "echo-filter")
	assert.Contains(t, info.Capabilities, "long-polling")
	assert.Contains(t, info.Capabilities, "telegram-bot-api")
}

func TestHealthCheck_Healthy(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "healthy", status.Status)
	assert.Equal(t, "@test_bot", status.Details["bot_username"])
}

func TestHealthCheck_Degraded(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "degraded", status.Status)
}

func TestHealthCheck_Unhealthy(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
	})
	require.NoError(t, err)

	require.NoError(t, b.Close())

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "unhealthy", status.Status)
}

func TestEchoFiltering_BotMessage(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	b := newTestBroker(t, tgSrv)

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	// Queue an update from the bot itself (ID 100 matches our test bot)
	// and a real user message, using the non-blocking update queue.
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 100, Username: "test_bot", IsBot: true},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "echo from bot",
			},
		},
		{
			UpdateID: 2,
			Message: &TGMessage{
				MessageID: 2,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "real message",
			},
		},
	})

	require.NoError(t, b.Subscribe("test.>"))

	// Wait for the real message to be delivered, proving the echo was filtered
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&received) == 1
	}, 5*time.Second, 50*time.Millisecond)

	// Exactly 1 message should be delivered (the real one, not the echo)
	assert.Equal(t, int32(1), atomic.LoadInt32(&received), "bot's own message should be filtered")
}

func TestEchoFiltering_OriginMarker(t *testing.T) {
	assert.True(t, isEcho(messages.NewInstruction(
		OriginMarkerKey+":"+OriginMarkerValue+":hub",
		"agent:coder",
		"echo",
	)))
	assert.False(t, isEcho(messages.NewInstruction(
		"user:alice",
		"agent:coder",
		"not echo",
	)))
	assert.False(t, isEcho(nil))
}

func TestInboundDelivery(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	b := newTestBroker(t, tgSrv)

	// Set up a chat route
	b.mu.Lock()
	b.chatRoutes[789] = "scion.project.p1.agent.coder.messages"
	b.mu.Unlock()

	var deliveredTopic string
	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{}, 1)
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		deliveredTopic = topic
		deliveredMsg = msg
		select {
		case done <- struct{}{}:
		default:
		}
	}

	// Queue an update from a user
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 42,
				From:      &TGUser{ID: 456, Username: "alice", FirstName: "Alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      1700000000,
				Text:      "hello agent",
			},
		},
	})

	require.NoError(t, b.Subscribe("scion.project.p1.>"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inbound delivery")
	}

	assert.Equal(t, "scion.project.p1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "hello agent", deliveredMsg.Msg)
	assert.Equal(t, "telegram:alice", deliveredMsg.Sender)
	assert.Equal(t, "456", deliveredMsg.SenderID)
	assert.Equal(t, "agent:coder", deliveredMsg.Recipient)
	assert.Equal(t, messages.TypeInstruction, deliveredMsg.Type)
	assert.Equal(t, "789", deliveredMsg.Metadata["telegram_chat_id"])
	assert.Equal(t, "42", deliveredMsg.Metadata["telegram_message_id"])
}

func TestInboundDelivery_DefaultTopic(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	b := newTestBroker(t, tgSrv)

	// No chat routes configured — should use default topic

	var deliveredTopic string
	done := make(chan struct{}, 1)
	b.InboundHandler = func(topic string, _ *messages.StructuredMessage) {
		deliveredTopic = topic
		select {
		case done <- struct{}{}:
		default:
		}
	}

	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "no route",
			},
		},
	})

	require.NoError(t, b.Subscribe("scion.>"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inbound delivery")
	}

	assert.Equal(t, "scion.telegram.chat.789.messages", deliveredTopic)
}

func TestHubAPIDelivery(t *testing.T) {
	var receivedPayloads []inboundPayload
	var mu sync.Mutex

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/broker/inbound", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "test-telegram", r.Header.Get("X-Scion-Plugin-Name"))

		body, _ := io.ReadAll(r.Body)
		var p inboundPayload
		require.NoError(t, json.Unmarshal(body, &p))

		mu.Lock()
		receivedPayloads = append(receivedPayloads, p)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer hubSrv.Close()

	tgSrv := newFakeTelegramServer(t)

	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      hubSrv.URL,
		"plugin_name":  "test-telegram",
	})
	require.NoError(t, err)

	// Queue an update using the non-blocking queue
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "via hub api",
			},
		},
	})

	require.NoError(t, b.Subscribe("scion.>"))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(receivedPayloads) == 1
	}, 5*time.Second, 50*time.Millisecond)

	mu.Lock()
	assert.Equal(t, "via hub api", receivedPayloads[0].Message.Msg)
	mu.Unlock()
}

func TestConfigure_WithUserMappings(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"user_mappings": `{"8663066556": "ptone@google.com", "123": "alice@example.com"}`,
	})
	require.NoError(t, err)
	assert.Equal(t, "ptone@google.com", b.userMappings["8663066556"])
	assert.Equal(t, "alice@example.com", b.userMappings["123"])
}

func TestConfigure_InvalidUserMappings(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"user_mappings": `not-json`,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid user_mappings")
}

func TestInboundDelivery_UserMapping(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"user_mappings": `{"456": "alice@example.com"}`,
	})
	require.NoError(t, err)

	b.mu.Lock()
	b.chatRoutes[789] = "scion.project.p1.agent.coder.messages"
	b.mu.Unlock()

	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{}, 1)
	b.InboundHandler = func(_ string, msg *messages.StructuredMessage) {
		deliveredMsg = msg
		select {
		case done <- struct{}{}:
		default:
		}
	}

	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 42,
				From:      &TGUser{ID: 456, Username: "alice", FirstName: "Alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      1700000000,
				Text:      "mapped user",
			},
		},
	})

	require.NoError(t, b.Subscribe("scion.project.p1.>"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inbound delivery")
	}

	assert.Equal(t, "user:alice@example.com", deliveredMsg.Sender)
	assert.Equal(t, "456", deliveredMsg.SenderID)
}

func TestInboundDelivery_UserMappingNoMatch(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"user_mappings": `{"999": "other@example.com"}`,
	})
	require.NoError(t, err)

	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{}, 1)
	b.InboundHandler = func(_ string, msg *messages.StructuredMessage) {
		deliveredMsg = msg
		select {
		case done <- struct{}{}:
		default:
		}
	}

	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "unmapped user",
			},
		},
	})

	require.NoError(t, b.Subscribe("scion.>"))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for inbound delivery")
	}

	assert.Equal(t, "telegram:alice", deliveredMsg.Sender)
}

func TestRecipientFromTopic(t *testing.T) {
	tests := []struct {
		topic string
		want  string
	}{
		{"scion.project.p1.agent.coder.messages", "agent:coder"},
		{"scion.project.p1.user.alice.messages", "user:alice"},
		{"scion.project.p1.broadcast", "broker:topic"},
		{"scion.telegram.chat.123.messages", "broker:topic"},
	}

	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			assert.Equal(t, tt.want, recipientFromTopic(tt.topic))
		})
	}
}

func TestPublish_Swallows429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/bottest-token/getMe":
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRaw(t, BotUser{
					ID: 100, IsBot: true, FirstName: "TestBot", Username: "test_bot",
				}),
			})
		case r.URL.Path == "/bottest-token/sendMessage":
			json.NewEncoder(w).Encode(apiResponse{
				OK:          false,
				ErrorCode:   429,
				Description: "Too Many Requests: retry after 35",
				Parameters:  &apiParameters{RetryAfterSec: 35},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b := New(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": srv.URL,
	}))
	b.topicChats["test.topic"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "rate limited")
	err := b.Publish(context.Background(), "test.topic", msg)
	assert.NoError(t, err, "429 should be swallowed, not propagated")
}

func TestPublish_Swallows5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/bottest-token/getMe":
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRaw(t, BotUser{
					ID: 100, IsBot: true, FirstName: "TestBot", Username: "test_bot",
				}),
			})
		case r.URL.Path == "/bottest-token/sendMessage":
			json.NewEncoder(w).Encode(apiResponse{
				OK:          false,
				ErrorCode:   502,
				Description: "Bad Gateway",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b := New(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": srv.URL,
	}))
	b.topicChats["test.topic"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "server error")
	err := b.Publish(context.Background(), "test.topic", msg)
	assert.NoError(t, err, "5xx should be swallowed, not propagated")
}

func TestPublish_PropagatesPermanentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/bottest-token/getMe":
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRaw(t, BotUser{
					ID: 100, IsBot: true, FirstName: "TestBot", Username: "test_bot",
				}),
			})
		case r.URL.Path == "/bottest-token/sendMessage":
			json.NewEncoder(w).Encode(apiResponse{
				OK:          false,
				ErrorCode:   400,
				Description: "Bad Request: chat not found",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b := New(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": srv.URL,
	}))
	b.topicChats["test.topic"] = []int64{789}

	msg := messages.NewInstruction("user:alice", "agent:coder", "bad request")
	err := b.Publish(context.Background(), "test.topic", msg)
	assert.Error(t, err, "permanent errors (4xx non-429) should propagate")
	assert.Contains(t, err.Error(), "chat not found")
}

func TestAPIError_IsTransient(t *testing.T) {
	tests := []struct {
		name      string
		code      int
		transient bool
	}{
		{"429 rate limit", 429, true},
		{"500 internal", 500, true},
		{"502 bad gateway", 502, true},
		{"503 unavailable", 503, true},
		{"400 bad request", 400, false},
		{"401 unauthorized", 401, false},
		{"403 forbidden", 403, false},
		{"404 not found", 404, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &APIError{Code: tt.code, Description: "test"}
			assert.Equal(t, tt.transient, e.IsTransient())
		})
	}
}

func TestAPIError_ErrorString(t *testing.T) {
	e := &APIError{Code: 429, Description: "Too Many Requests", RetryAfterSec: 35}
	assert.Contains(t, e.Error(), "429")
	assert.Contains(t, e.Error(), "retry after 35s")

	e2 := &APIError{Code: 500, Description: "Internal Server Error"}
	assert.Contains(t, e2.Error(), "500")
	assert.NotContains(t, e2.Error(), "retry after")
}

func TestSubjectMatchesPattern(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		want    bool
	}{
		{"foo.bar", "foo.bar", true},
		{"foo.bar", "foo.baz", false},
		{"foo.*", "foo.bar", true},
		{"foo.*", "foo.bar.baz", false},
		{"foo.>", "foo.bar", true},
		{"foo.>", "foo.bar.baz", true},
		{"foo.>", "foo", false},
		{"scion.project.*.agent.*.messages", "scion.project.p1.agent.coder.messages", true},
		{"scion.project.*.agent.*.messages", "scion.project.p1.broadcast", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.subject, func(t *testing.T) {
			assert.Equal(t, tt.want, subjectMatchesPattern(tt.pattern, tt.subject))
		})
	}
}

func TestParseHubError(t *testing.T) {
	t.Run("valid error response", func(t *testing.T) {
		body := `{"error":{"code":"agent_not_found","message":"Agent not found"}}`
		resp := &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		he := parseHubError(resp)
		require.NotNil(t, he)
		assert.Equal(t, 404, he.StatusCode)
		assert.Equal(t, "agent_not_found", he.Code)
		assert.Equal(t, "Agent not found", he.Message)
	})

	t.Run("empty body", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("")),
		}
		he := parseHubError(resp)
		assert.Equal(t, "unknown", he.Code)
		assert.Equal(t, "Internal Server Error", he.Message)
	})

	t.Run("non-JSON body", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 403,
			Body:       io.NopCloser(strings.NewReader("plain text")),
		}
		he := parseHubError(resp)
		assert.Equal(t, "unknown", he.Code)
		assert.Equal(t, "Forbidden", he.Message)
	})
}

func TestHubError_UserFacingMessage(t *testing.T) {
	tests := []struct {
		name     string
		err      hubError
		contains string
	}{
		{"agent_not_found", hubError{StatusCode: 404, Code: "agent_not_found"}, "Target agent not found"},
		{"forbidden", hubError{StatusCode: 403, Code: "forbidden"}, "permission"},
		{"unauthorized", hubError{StatusCode: 401, Code: "unauthorized"}, "Authentication error"},
		{"server_error", hubError{StatusCode: 500, Code: "internal_error"}, "try again or contact"},
		{"other", hubError{StatusCode: 400, Code: "invalid_request", Message: "bad topic"}, "try again or contact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Contains(t, tt.err.userFacingMessage(), tt.contains)
		})
	}
}

func TestDeliverInbound_ReturnsHubError(t *testing.T) {
	t.Run("returns error on 404", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "agent_not_found",
					"message": "Agent not found",
				},
			})
		}))
		defer hub.Close()

		b := &TelegramBroker{
			log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		msg := messages.NewInstruction("user:alice", "agent:coder", "hello")
		he := b.deliverInbound("scion.project.p1.agent.coder.messages", msg)
		require.NotNil(t, he)
		assert.Equal(t, 404, he.StatusCode)
		assert.Equal(t, "agent_not_found", he.Code)
	})

	t.Run("returns nil on success", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer hub.Close()

		b := &TelegramBroker{
			log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		msg := messages.NewInstruction("user:alice", "agent:coder", "hello")
		he := b.deliverInbound("scion.project.p1.agent.coder.messages", msg)
		assert.Nil(t, he)
	})
}

func TestInboundDelivery_ErrorFeedback(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	// Hub that rejects with 404 (deleted agent)
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "agent_not_found",
				"message": "Agent not found",
			},
		})
	}))
	defer hubSrv.Close()

	b := newTestBroker(t, tgSrv)

	// Configure hub URL to point to the rejecting hub
	b.mu.Lock()
	b.hubURL = hubSrv.URL
	b.chatRoutes[789] = "scion.project.p1.agent.deleted-agent.messages"
	b.topicChats["scion.project.p1.agent.deleted-agent.messages"] = []int64{789}
	b.InboundHandler = nil // Force HTTP delivery path
	b.mu.Unlock()

	// Queue a message from a user
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 42,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "hello deleted agent",
			},
		},
	})

	require.NoError(t, b.Subscribe("scion.project.p1.>"))

	// Wait for the error feedback message to be sent to the Telegram chat
	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "Target agent not found") {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "expected error feedback message in Telegram chat")
}
