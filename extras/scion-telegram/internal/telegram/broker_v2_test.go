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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test helpers ---

// fakeHubClient implements HubClient for testing.
type fakeHubClient struct {
	mu       sync.Mutex
	projects []ProjectOption
	agents   map[string][]AgentInfo // projectID → agents
}

func newFakeHubClient() *fakeHubClient {
	return &fakeHubClient{
		agents: make(map[string][]AgentInfo),
	}
}

func (f *fakeHubClient) ListProjects(_ context.Context) ([]ProjectOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects, nil
}

func (f *fakeHubClient) ListProjectsForUser(_ context.Context, _ string) ([]ProjectOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects, nil
}

func (f *fakeHubClient) ListProjectsFresh(_ context.Context) ([]ProjectOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projects, nil
}

func (f *fakeHubClient) ListAgents(_ context.Context, projectID string) ([]AgentInfo, error) {
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
	webhookURL         string
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

		case "/bottest-token/setWebhook":
			var req setWebhookRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.webhookURL = req.URL
			f.mu.Unlock()
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, true)})

		case "/bottest-token/deleteWebhook":
			f.mu.Lock()
			f.webhookURL = ""
			f.mu.Unlock()
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, true)})

		case "/bottest-token/setMyCommands":
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, true)})

		case "/bottest-token/getFile":
			fileID := r.URL.Query().Get("file_id")
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, TGFile{
					FileID:   fileID,
					FileSize: 1024,
					FilePath: "photos/" + fileID + ".jpg",
				}),
			})

		default:
			if strings.HasPrefix(r.URL.Path, "/file/bottest-token/") {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("fake-file-content"))
				return
			}
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
		{"scion.project.myproj.agent.coder.messages", "myproj", "coder"},
		{"scion.grove.myproj.agent.coder.messages", "myproj", "coder"},
		{"scion.project.proj1.broadcast", "proj1", ""},
		{"scion.project.proj2.agent.reviewer.messages", "proj2", "reviewer"},
		{"scion.project.proj1.agent.coder.agent.reviewer.messages", "proj1", "reviewer"},
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
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
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
		Agents:      []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}},
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

	assert.Equal(t, "scion.project.proj-1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "hello there", deliveredMsg.Msg)
	assert.Equal(t, "user:alice@example.com", deliveredMsg.Sender)
	assert.Equal(t, "456", deliveredMsg.SenderID)
	assert.Equal(t, "agent:coder", deliveredMsg.Recipient)
	assert.Equal(t, "-200", deliveredMsg.Metadata["telegram_chat_id"])
}

func TestV2_HandleGroupMessage_DirectAgentMention(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}},
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
	assert.Equal(t, "scion.project.proj-1.agent.reviewer.messages", deliveredTopics[0])
}

func TestV2_HandleGroupMessage_AllMention(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}},
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
	assert.Contains(t, deliveredTopics, "scion.project.proj-1.agent.coder.messages")
	assert.Contains(t, deliveredTopics, "scion.project.proj-1.agent.reviewer.messages")
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
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
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
		Agents:      []AgentInfo{{Slug: "coder"}},
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
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}},
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

// --- Reply routing fallback tests ---

func TestV2_HandleGroupMessage_ReplyToBotMessage(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
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
		Agents:      []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}},
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

	// Simulate replying to a bot message from the "reviewer" agent.
	// No @-mention in the user's reply text — routing should use reply-to fallback.
	b.handleGroupMessage(&TGMessage{
		MessageID: 99,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "yes, looks good to me",
		ReplyToMessage: &TGMessage{
			MessageID: 50,
			From:      &TGUser{ID: b.botInfo.ID, IsBot: true, Username: b.botInfo.Username},
			Chat:      TGChat{ID: -200, Type: "group"},
			Text:      "🤖 reviewer\n\nPlease review the PR",
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	assert.Equal(t, "scion.project.proj-1.agent.reviewer.messages", deliveredTopic)
	assert.Equal(t, "yes, looks good to me", deliveredMsg.Msg)
	assert.Equal(t, "agent:reviewer", deliveredMsg.Recipient)
}

func TestV2_HandleGroupMessage_ReplyToBotMessage_MentionTakesPriority(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}},
		RefreshedAt: time.Now(),
	}))

	var deliveredTopic string
	done := make(chan struct{}, 1)
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		deliveredTopic = topic
		select {
		case done <- struct{}{}:
		default:
		}
	}

	// Reply to a reviewer message BUT explicitly mention @coder — mention wins.
	b.handleGroupMessage(&TGMessage{
		MessageID: 99,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "@coder handle this instead",
		ReplyToMessage: &TGMessage{
			MessageID: 50,
			From:      &TGUser{ID: b.botInfo.ID, IsBot: true, Username: b.botInfo.Username},
			Chat:      TGChat{ID: -200, Type: "group"},
			Text:      "🤖 reviewer\n\nSome reviewer message",
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	// @coder mention takes priority over reply-to-reviewer
	assert.Equal(t, "scion.project.proj-1.agent.coder.messages", deliveredTopic)
}

func TestV2_HandleGroupMessage_ReplyConversationContextFallback(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}, {Slug: "reviewer"}},
		RefreshedAt: time.Now(),
	}))

	// Save a conversation context so the fallback can find it.
	require.NoError(t, b.store.SaveConversationContext(ctx, &ConversationContext{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "reviewer",
		LastChatID:     -200,
		LastMessageAt:  time.Now().UTC(),
	}))

	var deliveredTopic string
	done := make(chan struct{}, 1)
	b.InboundHandler = func(topic string, msg *messages.StructuredMessage) {
		deliveredTopic = topic
		select {
		case done <- struct{}{}:
		default:
		}
	}

	// Reply to a bot message that has no parseable agent slug (e.g. a system message).
	// Should fall back to conversation context.
	b.handleGroupMessage(&TGMessage{
		MessageID: 99,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Text:      "continue with that",
		ReplyToMessage: &TGMessage{
			MessageID: 50,
			From:      &TGUser{ID: b.botInfo.ID, IsBot: true, Username: b.botInfo.Username},
			Chat:      TGChat{ID: -200, Type: "group"},
			Text:      "System notification: deployment complete",
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	assert.Equal(t, "scion.project.proj-1.agent.reviewer.messages", deliveredTopic)
}

// --- Publish tests ---

func TestV2_Publish_DirectChatID(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello from hub")
	msg.Metadata = map[string]string{
		"telegram_chat_id": "-300",
	}

	err := b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", msg)
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

	err := b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg)
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

	err := b.Publish(ctx, "scion.project.proj-1.broadcast", msg)
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

	err := b.Publish(context.Background(), "scion.project.unknown-proj.agent.coder.messages", msg)
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

	require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg))
	assert.Len(t, tgSrv.getSentMessages(), 1)

	require.NoError(t, b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg))
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

	err := b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", msg)
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

	err := b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", msg)
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

	assert.Equal(t, "scion.project.proj-1.agent.coder.messages", deliveredTopic)
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

// --- Publish: state-change DM routing ---

func TestV2_Publish_StateChange_RoutedToDM(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	// Also create a group link — state-change should NOT go here.
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -200,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice@example.com",
		Msg:       "Agent coder completed successfully",
		Type:      messages.TypeStateChange,
		Status:    "completed",
	}

	err := b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	// DM chat ID equals the user's Telegram user ID.
	assert.Equal(t, int64(456), sent[0].ChatID)
	assert.Contains(t, sent[0].Text, "completed")
}

func TestV2_Publish_StateChange_NotSentToGroup(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	// Group link exists but no user mapping — message should be dropped,
	// not broadcast to the group.
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -200,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:unknown@example.com",
		Msg:       "Agent coder errored",
		Type:      messages.TypeStateChange,
		Status:    "error",
	}

	err := b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	// No messages should be sent — dropped, not broadcast to group.
	assert.Empty(t, tgSrv.getSentMessages())
}

func TestV2_Publish_StateChange_RespectNotificationPref(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	// User explicitly disabled notifications for this agent.
	require.NoError(t, b.store.SaveNotificationPref(ctx, &NotificationPref{
		TelegramUserID: "456",
		ProjectID:      "proj-1",
		AgentSlug:      "coder",
		Enabled:        false,
	}))

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "user:alice@example.com",
		Msg:       "Agent coder completed",
		Type:      messages.TypeStateChange,
		Status:    "completed",
	}

	err := b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	// No messages should be sent — user disabled notifications.
	assert.Empty(t, tgSrv.getSentMessages())
}

func TestV2_Publish_StateChange_NonUserRecipientDropped(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:    -200,
		ProjectID: "proj-1",
		LinkedAt:  time.Now().UTC(),
		Active:    true,
	}))

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Sender:    "agent:coder",
		Recipient: "agent:reviewer",
		Msg:       "state changed",
		Type:      messages.TypeStateChange,
	}

	err := b.Publish(ctx, "scion.project.proj-1.agent.coder.messages", msg)
	require.NoError(t, err)

	assert.Empty(t, tgSrv.getSentMessages())
}

// --- Subscribe/Unsubscribe/Close ---

func TestV2_SubscribeUnsubscribe(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))

	b.mu.RLock()
	assert.True(t, b.subs["scion.project.proj-1.>"])
	assert.NotNil(t, b.pollCancel)
	b.mu.RUnlock()

	require.NoError(t, b.Unsubscribe("scion.project.proj-1.>"))

	b.mu.RLock()
	assert.False(t, b.subs["scion.project.proj-1.>"])
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
			contains:  []string{"🤖 coder", "hello world"},
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
			contains:  []string{"[URGENT]", "[Broadcast]", "🤖 monitor", "[error]", "alert!"},
		},
		{
			name: "input needed",
			msg: &messages.StructuredMessage{
				Msg:  "proceed?",
				Type: messages.TypeInputNeeded,
			},
			agentSlug: "coder",
			contains:  []string{"🤖 coder", "proceed?"},
		},
		{
			name: "assistant reply",
			msg: &messages.StructuredMessage{
				Msg:  "here is the result",
				Type: messages.TypeAssistantReply,
			},
			agentSlug: "",
			contains:  []string{"here is the result"},
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

func TestFormatMessageV2_RecipientUsername(t *testing.T) {
	tests := []struct {
		name              string
		msg               *messages.StructuredMessage
		agentSlug         string
		recipientUsername string
		wantContains      string
		wantNotContains   string
	}{
		{
			name: "agent-to-user with username",
			msg: &messages.StructuredMessage{
				Sender:    "agent:coder",
				Recipient: "user:alice@example.com",
				Msg:       "hello",
			},
			agentSlug:         "coder",
			recipientUsername: "bob585",
			wantContains:      "🤖 coder → @bob585",
		},
		{
			name: "agent-to-user without username",
			msg: &messages.StructuredMessage{
				Sender:    "agent:coder",
				Recipient: "user:alice@example.com",
				Msg:       "hello",
			},
			agentSlug:         "coder",
			recipientUsername: "",
			wantContains:      "🤖 coder",
			wantNotContains:   "→",
		},
		{
			name: "agent-to-agent ignores username",
			msg: &messages.StructuredMessage{
				Sender:    "agent:coder",
				Recipient: "agent:reviewer",
				Msg:       "check this",
			},
			agentSlug:         "coder",
			recipientUsername: "bob585",
			wantContains:      "👀 🤖 coder → 🤖 reviewer 👀",
			wantNotContains:   "@bob585",
		},
		{
			name: "agent-to-user no agentSlug falls back to sender",
			msg: &messages.StructuredMessage{
				Sender:    "agent:deployer",
				Recipient: "user:alice@example.com",
				Msg:       "deployed",
			},
			agentSlug:         "",
			recipientUsername: "alice_tg",
			wantContains:      "🤖 deployer → @alice_tg",
		},
		{
			name: "non-agent sender ignores username",
			msg: &messages.StructuredMessage{
				Sender:    "user:alice",
				Recipient: "user:bob",
				Msg:       "hi",
			},
			agentSlug:         "",
			recipientUsername: "bob_tg",
			wantContains:      "user:alice",
			wantNotContains:   "@bob_tg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := FormatMessageV2(tt.msg, tt.agentSlug, tt.recipientUsername)
			if tt.wantContains != "" {
				assert.Contains(t, result, tt.wantContains)
			}
			if tt.wantNotContains != "" {
				assert.NotContains(t, result, tt.wantNotContains)
			}
		})
	}
}

// --- Webhook mode tests ---

func TestV2_Configure_WebhookMode(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"hub_url":        "http://hub.test",
		"broker_id":      "broker-test",
		"db_path":        dbPath,
		"inbound_mode":   "webhook",
		"webhook_url":    "https://example.com/telegram/webhook",
		"webhook_listen": ":0",
		"webhook_secret": "test-secret",
	})
	require.NoError(t, err)

	assert.Equal(t, "webhook", b.inboundMode)
	assert.NotNil(t, b.webhookServer)

	tgSrv.mu.Lock()
	assert.Equal(t, "https://example.com/telegram/webhook", tgSrv.webhookURL)
	tgSrv.mu.Unlock()
}

func TestV2_Configure_WebhookMode_MissingURL(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"db_path":      dbPath,
		"inbound_mode": "webhook",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhook_url is required")
}

func TestV2_Configure_InvalidInboundMode(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"db_path":      dbPath,
		"inbound_mode": "invalid",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid inbound_mode")
}

func TestV2_Configure_DefaultPollMode(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"db_path":      dbPath,
	})
	require.NoError(t, err)

	assert.Equal(t, "poll", b.inboundMode)
	assert.Nil(t, b.webhookServer)
}

func TestV2_WebhookMode_PollingDoesNotStart(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"db_path":        dbPath,
		"inbound_mode":   "webhook",
		"webhook_url":    "https://example.com/telegram/webhook",
		"webhook_listen": ":0",
	})
	require.NoError(t, err)

	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))

	b.mu.RLock()
	assert.Nil(t, b.pollCancel, "polling should not start in webhook mode")
	b.mu.RUnlock()
}

func TestV2_WebhookMode_InboundMessageDelivery(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"hub_url":        "http://hub.test",
		"broker_id":      "broker-test",
		"db_path":        dbPath,
		"inbound_mode":   "webhook",
		"webhook_url":    "https://example.com/telegram/webhook",
		"webhook_listen": ":0",
		"webhook_secret": "webhook-secret",
	})
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		ProjectSlug:  "my-project",
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}},
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

	update := Update{
		UpdateID: 42,
		Message: &TGMessage{
			MessageID: 10,
			From:      &TGUser{ID: 456, Username: "alice"},
			Chat:      TGChat{ID: -200, Type: "group"},
			Date:      time.Now().Unix(),
			Text:      "@test_bot hello webhook",
			Entities: []MessageEntity{
				{Type: "mention", Offset: 0, Length: 9},
			},
		},
	}
	body, err := json.Marshal(update)
	require.NoError(t, err)

	webhookAddr := b.webhookServer.actualAddr
	req, err := http.NewRequest("POST", "http://"+webhookAddr+webhookPath, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(secretTokenHeader, "webhook-secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for webhook message delivery")
	}

	assert.Equal(t, "scion.project.proj-1.agent.coder.messages", deliveredTopic)
	assert.Equal(t, "hello webhook", deliveredMsg.Msg)
	assert.Equal(t, "user:alice@example.com", deliveredMsg.Sender)
	assert.Equal(t, "agent:coder", deliveredMsg.Recipient)
}

func TestV2_WebhookMode_Close(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	dbPath := filepath.Join(t.TempDir(), "test.db")

	b := NewV2(slog.Default())

	err := b.Configure(map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"db_path":        dbPath,
		"inbound_mode":   "webhook",
		"webhook_url":    "https://example.com/telegram/webhook",
		"webhook_listen": ":0",
	})
	require.NoError(t, err)

	tgSrv.mu.Lock()
	assert.Equal(t, "https://example.com/telegram/webhook", tgSrv.webhookURL)
	tgSrv.mu.Unlock()

	require.NoError(t, b.Close())

	tgSrv.mu.Lock()
	assert.Empty(t, tgSrv.webhookURL, "webhook should be deleted on close")
	tgSrv.mu.Unlock()
}

func TestResolveOutboundMentions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID:   "100",
		TelegramUsername: "ptone805",
		ScionEmail:       "ptone@google.com",
		LinkedAt:         time.Now().UTC(),
	}))
	require.NoError(t, store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "200",
		ScionEmail:     "nousername@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "user:email replaced",
			text: "Hey user:ptone@google.com check this",
			want: "Hey @ptone805 check this",
		},
		{
			name: "standalone email replaced",
			text: "Hey ptone@google.com check this",
			want: "Hey @ptone805 check this",
		},
		{
			name: "no username leaves as-is",
			text: "Contact nousername@example.com please",
			want: "Contact nousername@example.com please",
		},
		{
			name: "unknown email leaves as-is",
			text: "Contact unknown@example.com please",
			want: "Contact unknown@example.com please",
		},
		{
			name: "email in URL skipped",
			text: "See https://ptone@google.com/path",
			want: "See https://ptone@google.com/path",
		},
		{
			name: "mailto skipped",
			text: "Send to mailto:ptone@google.com",
			want: "Send to mailto:ptone@google.com",
		},
		{
			name: "multiple emails",
			text: "user:ptone@google.com and nousername@example.com",
			want: "@ptone805 and nousername@example.com",
		},
		{
			name: "email at start of text",
			text: "ptone@google.com said hello",
			want: "@ptone805 said hello",
		},
		{
			name: "email at end of text",
			text: "message from ptone@google.com",
			want: "message from @ptone805",
		},
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "no emails",
			text: "just a regular message",
			want: "just a regular message",
		},
		{
			name: "email followed by slash skipped",
			text: "http://ptone@google.com/foo",
			want: "http://ptone@google.com/foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveOutboundMentions(ctx, store, tt.text)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("nil store returns text unchanged", func(t *testing.T) {
		got := resolveOutboundMentions(ctx, nil, "ptone@google.com")
		assert.Equal(t, "ptone@google.com", got)
	})
}

// --- File attachment tests ---

func TestV2_HandleIncoming_PhotoMessageNotDropped(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	tmpDir := t.TempDir()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		ProjectSlug:  filepath.Base(tmpDir),
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}},
		RefreshedAt: time.Now(),
	}))

	// Override the project path to use temp dir.
	origHome := os.Getenv("HOME")
	_ = origHome // keep for reference

	var deliveredMsg *messages.StructuredMessage
	done := make(chan struct{}, 1)
	b.InboundHandler = func(_ string, msg *messages.StructuredMessage) {
		deliveredMsg = msg
		select {
		case done <- struct{}{}:
		default:
		}
	}

	// Photo-only message (no text, no caption).
	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 42,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Photo: []PhotoSize{
			{FileID: "small", FileUniqueID: "uniq1", Width: 90, Height: 90, FileSize: 100},
			{FileID: "large", FileUniqueID: "uniq2", Width: 800, Height: 600, FileSize: 5000},
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	require.NotNil(t, deliveredMsg)
	assert.Contains(t, deliveredMsg.Msg, "📎")
	assert.Contains(t, deliveredMsg.Msg, "Photo attached")
	assert.Len(t, deliveredMsg.Attachments, 1)
	assert.Contains(t, deliveredMsg.Attachments[0], "/workspace/downloads/")
}

func TestV2_HandleIncoming_DocumentWithCaption(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
	b := newTestBrokerV2WithHub(t, tgSrv, hub)

	ctx := context.Background()
	require.NoError(t, b.store.SaveUserMapping(ctx, &TelegramUserMapping{
		TelegramUserID: "456",
		ScionEmail:     "alice@example.com",
		LinkedAt:       time.Now().UTC(),
	}))

	tmpDir := t.TempDir()
	require.NoError(t, b.store.SaveGroupLink(ctx, &GroupLink{
		ChatID:       -200,
		ProjectID:    "proj-1",
		ProjectSlug:  filepath.Base(tmpDir),
		DefaultAgent: "coder",
		LinkedAt:     time.Now().UTC(),
		Active:       true,
	}))
	require.NoError(t, b.store.SaveProjectAgents(ctx, &ProjectAgents{
		ProjectID:   "proj-1",
		Agents:      []AgentInfo{{Slug: "coder"}},
		RefreshedAt: time.Now(),
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

	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 43,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
		Date:      time.Now().Unix(),
		Caption:   "please review this file",
		Document: &TGDocument{
			FileID:       "doc-abc",
			FileUniqueID: "docuniq1",
			FileName:     "report.pdf",
			MimeType:     "application/pdf",
			FileSize:     8192,
		},
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	require.NotNil(t, deliveredMsg)
	assert.Contains(t, deliveredMsg.Msg, "please review this file")
	assert.Contains(t, deliveredMsg.Msg, "📎")
	assert.Contains(t, deliveredMsg.Msg, "Document attached: report.pdf")
	assert.Len(t, deliveredMsg.Attachments, 1)
	assert.Contains(t, deliveredMsg.Attachments[0], "report.pdf")
}

func TestV2_HandleIncoming_EmptyMessageDropped(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	// No text, no caption, no photo, no document.
	b.handleIncomingMessageV2(&TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: -200, Type: "group"},
	})

	assert.Equal(t, int32(0), atomic.LoadInt32(&received))
}

func TestV2_DownloadTelegramFile_PhotoPicksLargest(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	tmpDir := t.TempDir()
	slug := filepath.Base(tmpDir)

	// Point project path to temp dir.
	projectDir := filepath.Join(tmpDir, ".scion", "projects", slug)
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	// Patch HOME so /home/scion/.scion/projects/<slug> resolves to temp dir.
	// Instead, we'll test with the actual slug and just verify the function returns.
	ctx := context.Background()

	tgMsg := &TGMessage{
		Photo: []PhotoSize{
			{FileID: "small", FileUniqueID: "u1", Width: 90, Height: 90, FileSize: 100},
			{FileID: "medium", FileUniqueID: "u2", Width: 320, Height: 240, FileSize: 2000},
			{FileID: "large", FileUniqueID: "u3", Width: 1280, Height: 960, FileSize: 5000},
		},
	}

	agentPath, placeholder, err := b.downloadTelegramFile(ctx, tgMsg, slug)
	require.NoError(t, err)
	assert.Contains(t, agentPath, "/workspace/downloads/")
	assert.Contains(t, agentPath, "photo_u3.jpg")
	assert.Contains(t, placeholder, "Photo attached")
	assert.Contains(t, placeholder, "photo_u3.jpg")
}

func TestV2_DownloadTelegramFile_DocumentFallbackName(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	tmpDir := t.TempDir()
	slug := filepath.Base(tmpDir)

	ctx := context.Background()

	tgMsg := &TGMessage{
		Document: &TGDocument{
			FileID:       "doc-xyz",
			FileUniqueID: "docuniq99",
			FileName:     "", // empty filename
			MimeType:     "application/octet-stream",
			FileSize:     512,
		},
	}

	agentPath, placeholder, err := b.downloadTelegramFile(ctx, tgMsg, slug)
	require.NoError(t, err)
	assert.Contains(t, agentPath, "docuniq99")
	assert.Contains(t, placeholder, "Document attached")
}

func TestV2_DownloadTelegramFile_TooLarge(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()

	tgMsg := &TGMessage{
		Document: &TGDocument{
			FileID:       "big-file",
			FileUniqueID: "bigid",
			FileName:     "huge.zip",
			FileSize:     25 * 1024 * 1024, // 25 MB > 20 MB limit
		},
	}

	_, _, err := b.downloadTelegramFile(ctx, tgMsg, "test-slug")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file too large")
}

func TestV2_DownloadTelegramFile_NoAttachment(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newTestBrokerV2(t, tgSrv)

	ctx := context.Background()
	tgMsg := &TGMessage{Text: "just text"}

	_, _, err := b.downloadTelegramFile(ctx, tgMsg, "test-slug")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no photo or document")
}

func TestV2_DownloadTelegramFile_DownloadFailure(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: mustJSONRawV2(t, TGFile{
					FileID:   "file-id",
					FilePath: "photos/fail.jpg",
				}),
			})
		case strings.HasPrefix(r.URL.Path, "/file/"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer failSrv.Close()

	client := NewAPIClient("test-token", failSrv.URL)
	b := &TelegramBrokerV2{
		api: client,
		log: slog.Default(),
	}

	ctx := context.Background()
	tgMsg := &TGMessage{
		Photo: []PhotoSize{
			{FileID: "file-id", FileUniqueID: "u1", FileSize: 100},
		},
	}

	_, _, err := b.downloadTelegramFile(ctx, tgMsg, "test-slug")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "download file")
}
