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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test helpers ---

// fakeHubClient implements HubClient for testing.
type fakeHubClient struct {
	mu       sync.Mutex
	projects []ProjectOption
	agents   map[string][]string // projectID → agent slugs
}

func newFakeHubClient() *fakeHubClient {
	return &fakeHubClient{
		agents: make(map[string][]string),
	}
}

func (f *fakeHubClient) ListProjects(_ context.Context) ([]ProjectOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects, nil
}

func (f *fakeHubClient) ListAgents(_ context.Context, projectID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[projectID], nil
}

// fakeTGServerV2 extends fakeTelegramServer with v2 endpoint support.
type fakeTGServerV2 struct {
	srv *httptest.Server

	mu                 sync.Mutex
	sentMessages       []sendMessageWithKeyboardRequest
	editedTexts        []editMessageTextRequest
	editedMarkups      []editMessageReplyMarkupRequest
	answeredCallbacks  []answerCallbackQueryRequest
	nextSendMessageID  int64
}

func newFakeTGServerV2(t *testing.T) *fakeTGServerV2 {
	t.Helper()
	f := &fakeTGServerV2{nextSendMessageID: 100}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/bottest-token/getMe":
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, BotUser{
					ID: 100, IsBot: true, FirstName: "TestBot", Username: "test_bot",
				}),
			})

		case "/bottest-token/sendMessage":
			var req sendMessageWithKeyboardRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.sentMessages = append(f.sentMessages, req)
			msgID := f.nextSendMessageID
			f.nextSendMessageID++
			f.mu.Unlock()

			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, TGMessage{
					MessageID: msgID,
					Chat:      TGChat{ID: req.ChatID, Type: "group"},
					Text:      req.Text,
				}),
			})

		case "/bottest-token/editMessageText":
			var req editMessageTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.editedTexts = append(f.editedTexts, req)
			f.mu.Unlock()

			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, TGMessage{
					MessageID: req.MessageID,
					Chat:      TGChat{ID: req.ChatID},
					Text:      req.Text,
				}),
			})

		case "/bottest-token/editMessageReplyMarkup":
			var req editMessageReplyMarkupRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.editedMarkups = append(f.editedMarkups, req)
			f.mu.Unlock()

			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, TGMessage{
					MessageID: req.MessageID,
					Chat:      TGChat{ID: req.ChatID},
				}),
			})

		case "/bottest-token/answerCallbackQuery":
			var req answerCallbackQueryRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.answeredCallbacks = append(f.answeredCallbacks, req)
			f.mu.Unlock()

			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, true)})

		case "/bottest-token/getUpdates":
			json.NewEncoder(w).Encode(apiResponse{
				OK:     true,
				Result: mustJSONRawV2(t, []Update{}),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(func() { f.srv.Close() })
	return f
}

func (f *fakeTGServerV2) getSentMessages() []sendMessageWithKeyboardRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]sendMessageWithKeyboardRequest, len(f.sentMessages))
	copy(result, f.sentMessages)
	return result
}

func (f *fakeTGServerV2) getEditedTexts() []editMessageTextRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]editMessageTextRequest, len(f.editedTexts))
	copy(result, f.editedTexts)
	return result
}

func (f *fakeTGServerV2) getAnsweredCallbacks() []answerCallbackQueryRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]answerCallbackQueryRequest, len(f.answeredCallbacks))
	copy(result, f.answeredCallbacks)
	return result
}

func mustJSONRawV2(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// newTestBrokerV2 creates a fully wired TelegramBrokerV2 for testing.
// It uses a real SQLite store (temp file) and a fake Telegram API server.
func newTestBrokerV2(t *testing.T, tgSrv *fakeTGServerV2) *TelegramBrokerV2 {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	t.Cleanup(func() { b.Close() })

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      "http://hub.test",
		"broker_id":    "broker-test",
		"db_path":      dbPath,
	})
	require.NoError(t, err)

	return b
}

// newTestBrokerV2WithHub creates a v2 broker with a custom hub client injected.
func newTestBrokerV2WithHub(t *testing.T, tgSrv *fakeTGServerV2, hub *fakeHubClient) *TelegramBrokerV2 {
	t.Helper()
	b := newTestBrokerV2(t, tgSrv)
	b.hubClient = hub
	b.commands = NewCommandHandler(b.store, b.api, hub, b.botInfo.Username, b.log)
	b.callbacks = NewCallbackHandler(b.store, b.api, hub, b.log)
	return b
}

// --- Configure tests ---

func TestV2_Configure(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      "http://localhost:8080",
		"hmac_key":     "secret",
		"broker_id":    "broker-1",
		"plugin_name":  "tg-v2",
		"db_path":      dbPath,
	})
	require.NoError(t, err)

	assert.Equal(t, "http://localhost:8080", b.hubURL)
	assert.Equal(t, "secret", b.hmacKey)
	assert.Equal(t, "broker-1", b.brokerID)
	assert.Equal(t, "tg-v2", b.pluginName)
	assert.NotNil(t, b.botInfo)
	assert.Equal(t, "test_bot", b.botInfo.Username)
	assert.NotNil(t, b.store)
	assert.NotNil(t, b.commands)
	assert.NotNil(t, b.callbacks)
	assert.NotNil(t, b.registration)
}

func TestV2_Configure_MissingBotToken(t *testing.T) {
	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{"hub_url": "http://localhost:8080"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bot_token is required")
}

func TestV2_Configure_WithV1ChatRoutes(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	routes := `{"123": "scion.grove.proj1.agent.coder.messages", "-456": "scion.grove.proj1.broadcast"}`
	err := b.Configure(map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"db_path":        dbPath,
		"v1_chat_routes": routes,
	})
	require.NoError(t, err)

	ctx := context.Background()
	link, err := b.store.GetGroupLink(ctx, 123)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, "proj1", link.ProjectID)
	assert.Equal(t, "coder", link.DefaultAgent)

	link2, err := b.store.GetGroupLink(ctx, -456)
	require.NoError(t, err)
	require.NotNil(t, link2)
	assert.Equal(t, "proj1", link2.ProjectID)
}

// --- parseTopicComponents tests ---

func TestParseTopicComponents(t *testing.T) {
	tests := []struct {
		topic       string
		wantProject string
		wantAgent   string
	}{
		{"scion.grove.myproj.agent.coder.messages", "myproj", "coder"},
		{"scion.grove.proj1.broadcast", "proj1", ""},
		{"scion.project.proj2.agent.reviewer.messages", "proj2", "reviewer"},
		{"scion.grove.proj1.agent.coder.agent.reviewer.messages", "proj1", "reviewer"},
		{"unknown-topic-format", "unknown-topic-format", ""},
	}

	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			projID, agentSlug := parseTopicComponents(tt.topic)
			assert.Equal(t, tt.wantProject, projID)
			assert.Equal(t, tt.wantAgent, agentSlug)
		})
	}
}

// --- handleIncomingMessageV2 tests ---

func TestV2_HandleIncoming_BotEchoFiltering(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	// Message from the bot itself (ID 100 matches test bot).
	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 100, Username: "test_bot", IsBot: true},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "echo from bot",
	})

	assert.Equal(t, int32(0), atomic.LoadInt32(&received))
}

func TestV2_HandleIncoming_EmptyText(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Text:      "",
	})

	assert.Equal(t, int32(0), atomic.LoadInt32(&received))
}

func TestV2_HandleIncoming_DMHelp(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	// DMs have positive chat IDs. Non-command DMs get a help message.
	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 456, Type: "private"},
		Date:      time.Now().Unix(),
		Text:      "hello",
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(456), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "/register")
	assert.Contains(t, sent[0].Text, "/help")
}

func TestV2_HandleIncoming_CommandDispatch(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	// /help command in DM.
	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 456, Type: "private"},
		Date:      time.Now().Unix(),
		Text:      "/help",
	})

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "/status")
}

func TestV2_HandleIncoming_UnlinkedGroupIgnored(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	// Group message with bot mention but no group link.
	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@test_bot hello",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 9},
		},
	})

	assert.Equal(t, int32(0), atomic.LoadInt32(&received))
	// No messages sent to Telegram (silently ignored).
	assert.Empty(t, tgSrv.getSentMessages())
}

// --- handleGroupMessage tests ---

func TestV2_HandleGroupMessage_BotMentionDefaultAgent(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []string{"coder", "reviewer"}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		ProjectSlug:  "my-project",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		AgentSlugs:  []string{"coder", "reviewer"},
		RefreshedAt: time.Now(),
	}))

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

	b.handleGroupMessage(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@test_bot hello there",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 9},
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	assert.Equal(t, "scion.grove.proj-1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "hello there", deliveredMsg.Msg)
	assert.Equal(t, "telegram:alice", deliveredMsg.Sender)
	assert.Equal(t, "456", deliveredMsg.SenderID)
	assert.Equal(t, "agent:coder", deliveredMsg.Recipient)
	assert.Equal(t, "-200", deliveredMsg.Metadata["telegram_chat_id"])
}

func TestV2_HandleGroupMessage_DirectAgentMention(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []string{"coder", "reviewer"}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		AgentSlugs:  []string{"coder", "reviewer"},
		RefreshedAt: time.Now(),
	}))

	var deliveredTopics []string
	var mu sync.Mutex
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		deliveredTopics = append(deliveredTopics, topic)
		mu.Unlock()
	}

	b.handleGroupMessage(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@reviewer please review this",
	})

	// Give a moment for async delivery.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, deliveredTopics, 1)
	assert.Equal(t, "scion.grove.proj-1.agent.reviewer.messages", deliveredTopics[0])
}

func TestV2_HandleGroupMessage_AllMention(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []string{"coder", "reviewer"}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		AgentSlugs:  []string{"coder", "reviewer"},
		RefreshedAt: time.Now(),
	}))

	var deliveredTopics []string
	var mu sync.Mutex
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		mu.Lock()
		deliveredTopics = append(deliveredTopics, topic)
		mu.Unlock()
	}

	b.handleGroupMessage(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@all update status please",
	})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, deliveredTopics, 2)
	assert.Contains(t, deliveredTopics, "scion.grove.proj-1.agent.coder.messages")
	assert.Contains(t, deliveredTopics, "scion.grove.proj-1.agent.reviewer.messages")
}

func TestV2_HandleGroupMessage_NoMention(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	b.handleGroupMessage(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "just chatting, no mentions",
	})

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&received))
}

func TestV2_HandleGroupMessage_UserMappingResolution(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []string{"coder"}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		AgentSlugs:  []string{"coder"},
		RefreshedAt: time.Now(),
	}))
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{}, 1)
	b.InboundHandler = func(_ string, msg *messages.StructuredMessage) {
		deliveredMsg = msg
		select {
		case done <- struct{}{}:
		default:
		}
	}

	b.handleGroupMessage(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@test_bot hi",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 9},
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	assert.Equal(t, "user:alice@example.com", deliveredMsg.Sender)
	assert.Equal(t, "456", deliveredMsg.SenderID)
}

func TestV2_HandleGroupMessage_ConversationContextSaved(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []string{"coder"}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		AgentSlugs:  []string{"coder"},
		RefreshedAt: time.Now(),
	}))

	done := make(chan struct{}, 1)
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		select {
		case done <- struct{}{}:
		default:
		}
	}

	b.handleGroupMessage(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@test_bot check this",
		Entities: []MessageEntity{
			{Type: "mention", Offset: 0, Length: 9},
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	cc, err := b.store.GetConversationContext(ctx, "456", "proj-1", "coder")
	require.NoError(t, err)
	require.NotNil(t, cc)
	assert.Equal(t, int64(-200), cc.LastChatID)
}

// --- Publish tests ---

func TestV2_Publish_DirectChatID(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello from hub")
	msg.Metadata = map[string]string{
		"telegram_chat_id": "-300",
	}

	err := b.Publish(context.Background(), "scion.grove.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(-300), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "hello from hub")
}

func TestV2_Publish_ConversationContextRouting(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveConversationContext(ctx, &ConversationContext{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		LastChatID:     -200,
		LastMessageAt:  time.Now().UTC(),
	}))

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice@example.com",
		Msg:       "reply to alice",
		Type:      messages.TypeAssistantReply,
	}

	err := b.Publish(ctx, "scion.grove.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(-200), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "reply to alice")
}

func TestV2_Publish_BroadcastToGroupLinks(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	for _, chatID := range []int64{-100, -200} {
		require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
			ChatID:    chatID,
			ProjectID: "proj-1",
			LinkedAt:  time.Now().UTC(),
			Active:    true,
		}))
	}
	// Inactive link should be skipped.
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -300,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    false,
	}))

	msg := messages.NewInstruction("system", "broadcast", "system update")

	err := b.Publish(ctx, "scion.grove.proj-1.broadcast", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	assert.Len(t, sent, 2)

	chatIDs := []int64{sent[0].ChatID, sent[1].ChatID}
	assert.Contains(t, chatIDs, int64(-100))
	assert.Contains(t, chatIDs, int64(-200))
}

func TestV2_Publish_NoRouteDropsMessage(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	msg := messages.NewInstruction("agent:coder", "user:nobody", "lost message")

	err := b.Publish(context.Background(), "scion.grove.unknown-proj.agent.coder.messages", msg)
	require.NoError(t, err)

	assert.Empty(t, tgSrv.getSentMessages())
}

func TestV2_Publish_Dedup(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -100,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))

	msg := messages.NewInstruction("agent:coder", "user:alice", "hello")

	require.NoError(t, b.Publish(ctx, "scion.grove.proj-1.agent.coder.messages", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1)

	require.NoError(t, b.Publish(ctx, "scion.grove.proj-1.agent.coder.messages", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1, "duplicate should be skipped")
}

func TestV2_Publish_InputNeeded(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:coder",
		Msg:     "Do you want to proceed?",
		Type:    messages.TypeInputNeeded,
		Metadata: map[string]string{
			"telegram_chat_id": "-200",
			"choices":          `["Yes","No","Skip"]`,
		},
	}

	err := b.Publish(context.Background(), "scion.grove.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(-200), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "Do you want to proceed?")
	require.NotNil(t, sent[0].ReplyMarkup)
	// Should have buttons for Yes, No, Skip.
	buttons := sent[0].ReplyMarkup.InlineKeyboard
	require.NotEmpty(t, buttons)
}

func TestV2_Publish_Closed(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)
	require.NoError(t, b.Close())

	err := b.Publish(context.Background(), "test.topic", messages.NewInstruction("a", "b", "c"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestV2_Publish_ReplyToMessageID(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	msg := &messages.StructuredMessage{
		Version: messages.Version,
		Sender:  "agent:coder",
		Msg:     "reply message",
		Type:    messages.TypeAssistantReply,
		Metadata: map[string]string{
			"telegram_chat_id":    "-200",
			"telegram_message_id": "42",
		},
	}

	err := b.Publish(context.Background(), "scion.grove.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(42), sent[0].ReplyToMessageID)
}

// --- handleCallbackQuery tests ---

func TestV2_HandleCallback_AskUserResponse(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "req-123",
		MessageID: 50,
		ChatID:    -200,
		AgentSlug: "coder",
		ProjectID: "proj-1",
		Choices:   []string{"Yes", "No"},
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))

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

	b.handleCallbackQuery(ctx, &CallbackQuery{
		ID:   "cb-1",
		From: &TGUser{ID: 456, Username: "alice"},
		Message: &TGMessage{
			MessageID: 50,
			Chat:      TGChat{ID: -200, Type: "group"},
		},
		Data: "ask:yes:req-123",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	assert.Equal(t, "scion.grove.proj-1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "Yes", deliveredMsg.Msg)
	assert.Equal(t, "telegram:alice", deliveredMsg.Sender)
	assert.Equal(t, "req-123", deliveredMsg.Metadata["ask_request_id"])

	// Verify the callback was answered.
	callbacks := tgSrv.getAnsweredCallbacks()
	require.Len(t, callbacks, 1)
	assert.Equal(t, "cb-1", callbacks[0].CallbackQueryID)
	assert.Contains(t, callbacks[0].Text, "Yes")

	// Verify pending is marked as responded.
	pending, err := b.store.GetPendingAskUser(ctx, "req-123")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.True(t, pending.Responded)
}

func TestV2_HandleCallback_AskUserWithMapping(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "req-456",
		MessageID: 60,
		ChatID:    -200,
		AgentSlug: "coder",
		ProjectID: "proj-1",
		Choices:   []string{},
		ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}))
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{}, 1)
	b.InboundHandler = func(_ string, msg *messages.StructuredMessage) {
		deliveredMsg = msg
		select {
		case done <- struct{}{}:
		default:
		}
	}

	b.handleCallbackQuery(ctx, &CallbackQuery{
		ID:   "cb-2",
		From: &TGUser{ID: 456, Username: "alice"},
		Message: &TGMessage{
			MessageID: 60,
			Chat:      TGChat{ID: -200, Type: "group"},
		},
		Data: "ask:no:req-456",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	assert.Equal(t, "user:alice@example.com", deliveredMsg.Sender)
}

func TestV2_HandleCallback_ExpiredRequest(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SavePendingAskUser(ctx, &PendingAskUser{
		RequestID: "req-expired",
		MessageID: 70,
		ChatID:    -200,
		AgentSlug: "coder",
		ProjectID: "proj-1",
		ExpiresAt: time.Now().Add(-time.Hour).UTC(),
	}))

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	b.handleCallbackQuery(ctx, &CallbackQuery{
		ID:   "cb-3",
		From: &TGUser{ID: 456, Username: "alice"},
		Message: &TGMessage{
			MessageID: 70,
			Chat:      TGChat{ID: -200, Type: "group"},
		},
		Data: "ask:yes:req-expired",
	})

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&received))

	callbacks := tgSrv.getAnsweredCallbacks()
	require.Len(t, callbacks, 1)
	assert.Contains(t, callbacks[0].Text, "expired")
}

// --- Subscribe/Unsubscribe/Close ---

func TestV2_SubscribeUnsubscribe(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	require.NoError(t, b.Subscribe("scion.grove.proj-1.>"))

	b.mu.RLock()
	assert.True(t, b.subs["scion.grove.proj-1.>"])
	assert.NotNil(t, b.pollCancel)
	b.mu.RUnlock()

	require.NoError(t, b.Unsubscribe("scion.grove.proj-1.>"))

	b.mu.RLock()
	assert.False(t, b.subs["scion.grove.proj-1.>"])
	b.mu.RUnlock()
}

func TestV2_DoubleSubscribe(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	require.NoError(t, b.Subscribe("test.>"))
	require.NoError(t, b.Subscribe("test.>"))

	b.mu.RLock()
	assert.Len(t, b.subs, 1)
	b.mu.RUnlock()
}

func TestV2_Close(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"db_path":      filepath.Join(t.TempDir(), "test.db"),
	})
	require.NoError(t, err)

	require.NoError(t, b.Subscribe("test.>"))
	require.NoError(t, b.Close())

	err = b.Publish(context.Background(), "test.topic", messages.NewInstruction("a", "b", "c"))
	assert.Error(t, err)

	err = b.Subscribe("test.new")
	assert.Error(t, err)

	require.NoError(t, b.Close())
}

// --- GetInfo / HealthCheck ---

func TestV2_GetInfo(t *testing.T) {
	b := NewV2(slog.Default())
	defer b.Close()

	info, err := b.GetInfo()
	require.NoError(t, err)
	assert.Equal(t, "telegram", info.Name)
	assert.Equal(t, "2.0.0", info.Version)
	assert.Contains(t, info.Capabilities, "inline-keyboards")
	assert.Contains(t, info.Capabilities, "group-links")
	assert.Contains(t, info.Capabilities, "mention-routing")
}

func TestV2_HealthCheck_Healthy(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "healthy", status.Status)
	assert.Equal(t, "@test_bot", status.Details["bot_username"])
	assert.Equal(t, "v2", status.Details["version"])
}

func TestV2_HealthCheck_Degraded(t *testing.T) {
	b := NewV2(slog.Default())
	defer b.Close()

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "degraded", status.Status)
}

func TestV2_HealthCheck_Closed(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	require.NoError(t, b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"db_path":      filepath.Join(t.TempDir(), "test.db"),
	}))
	require.NoError(t, b.Close())

	status, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, "unhealthy", status.Status)
}

// --- FormatMessageV2 tests ---

func TestFormatMessageV2(t *testing.T) {
	tests := []struct {
		name      string
		msg       *messages.StructuredMessage
		agentSlug string
		contains  []string
	}{
		{
			name:      "nil message",
			msg:       nil,
			agentSlug: "coder",
			contains:  nil,
		},
		{
			name: "basic message",
			msg: &messages.StructuredMessage{
				Msg:  "hello world",
				Type: messages.TypeInstruction,
			},
			agentSlug: "coder",
			contains:  []string{"[coder]", "Message", "hello world"},
		},
		{
			name: "urgent broadcast",
			msg: &messages.StructuredMessage{
				Msg:         "alert!",
				Type:        messages.TypeStateChange,
				Urgent:      true,
				Broadcasted: true,
				Status:      "error",
			},
			agentSlug: "monitor",
			contains:  []string{"[URGENT]", "[Broadcast]", "[monitor]", "Status Update", "[error]", "alert!"},
		},
		{
			name: "input needed",
			msg: &messages.StructuredMessage{
				Msg:  "proceed?",
				Type: messages.TypeInputNeeded,
			},
			agentSlug: "coder",
			contains:  []string{"Input Needed", "proceed?"},
		},
		{
			name: "assistant reply",
			msg: &messages.StructuredMessage{
				Msg:  "here is the result",
				Type: messages.TypeAssistantReply,
			},
			agentSlug: "",
			contains:  []string{"Reply", "here is the result"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatMessageV2(tt.msg, tt.agentSlug)
			if tt.contains == nil {
				assert.Empty(t, result)
				return
			}
			for _, s := range tt.contains {
				assert.Contains(t, result, s)
			}
		})
	}
}
