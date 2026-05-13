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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

func TestGenerateToken(t *testing.T) {
	t1 := generateToken()
	t2 := generateToken()
	assert.NotEmpty(t, t1)
	assert.NotEqual(t, t1, t2)
	assert.Len(t, strings.Split(t1, "-"), 5, "token should be UUID-like with 5 segments")
}

func TestRegistrationServer_CreateAndConsumeToken(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	rs := newRegistrationServer(b)

	token := rs.createToken("456", "alice", 789)
	assert.NotEmpty(t, token)

	// Peek should return the registration without consuming
	reg := rs.peekToken(token)
	require.NotNil(t, reg)
	assert.Equal(t, "456", reg.TelegramUserID)
	assert.Equal(t, "alice", reg.TelegramUsername)
	assert.Equal(t, int64(789), reg.ChatID)

	// Peek again — still there
	assert.NotNil(t, rs.peekToken(token))

	// Consume should return and remove
	reg = rs.consumeToken(token)
	require.NotNil(t, reg)
	assert.Equal(t, "456", reg.TelegramUserID)

	// Second consume returns nil
	assert.Nil(t, rs.consumeToken(token))
}

func TestRegistrationServer_TokenExpiry(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	rs := newRegistrationServer(b)

	token := rs.createToken("456", "alice", 789)

	// Manually expire the token
	rs.mu.Lock()
	rs.pending[token].ExpiresAt = time.Now().Add(-1 * time.Second)
	rs.mu.Unlock()

	assert.Nil(t, rs.peekToken(token))
	assert.Nil(t, rs.consumeToken(token))
}

func TestRegistrationServer_CleanExpired(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	rs := newRegistrationServer(b)

	// Create two tokens, expire one
	t1 := rs.createToken("1", "user1", 100)
	rs.createToken("2", "user2", 200)

	rs.mu.Lock()
	rs.pending[t1].ExpiresAt = time.Now().Add(-1 * time.Second)
	rs.mu.Unlock()

	// Creating a new token triggers cleanup of expired tokens
	rs.createToken("3", "user3", 300)

	rs.mu.Lock()
	_, exists := rs.pending[t1]
	total := len(rs.pending)
	rs.mu.Unlock()

	assert.False(t, exists, "expired token should be cleaned")
	assert.Equal(t, 2, total, "should have 2 non-expired tokens (t2 + new t3); t1 was cleaned")
}

func newTestBrokerWithRegistration(t *testing.T, tgSrv *fakeTelegramServer) *TelegramBroker {
	t.Helper()
	b := New(slog.Default())
	t.Cleanup(func() { b.Close() })

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"plugin_name":   "test-telegram",
		"register_addr": "127.0.0.1:0",
	})
	require.NoError(t, err)
	require.NotNil(t, b.regServer)
	require.NotEmpty(t, b.registerURL)

	return b
}

func TestRegistrationHTTP_GetForm(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	token := b.regServer.createToken("456", "alice", 789)

	resp, err := http.Get(b.registerURL + "/register?token=" + token)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "alice")
	assert.Contains(t, bodyStr, token)
	assert.Contains(t, bodyStr, "Link Telegram to Scion")
}

func TestRegistrationHTTP_GetMissingToken(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	resp, err := http.Get(b.registerURL + "/register")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestRegistrationHTTP_GetInvalidToken(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	resp, err := http.Get(b.registerURL + "/register?token=bogus")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "Invalid or expired")
}

func TestRegistrationHTTP_PostSuccess(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	token := b.regServer.createToken("456", "alice", 789)

	resp, err := http.PostForm(b.registerURL+"/register", url.Values{
		"token": {token},
		"email": {"alice@example.com"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "Registration Complete")
	assert.Contains(t, string(body), "alice@example.com")

	// Verify the mapping was stored
	email, ok := b.getUserMapping("456")
	assert.True(t, ok)
	assert.Equal(t, "alice@example.com", email)

	// Token should be consumed
	assert.Nil(t, b.regServer.peekToken(token))

	// Verify a confirmation message was sent to Telegram
	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "Registration complete") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestRegistrationHTTP_PostExpiredToken(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	token := b.regServer.createToken("456", "alice", 789)

	// Expire the token
	b.regServer.mu.Lock()
	b.regServer.pending[token].ExpiresAt = time.Now().Add(-1 * time.Second)
	b.regServer.mu.Unlock()

	resp, err := http.PostForm(b.registerURL+"/register", url.Values{
		"token": {token},
		"email": {"alice@example.com"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	_, ok := b.getUserMapping("456")
	assert.False(t, ok)
}

func TestRegistrationHTTP_PostMissingFields(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	// Missing email
	token := b.regServer.createToken("456", "alice", 789)
	resp, err := http.PostForm(b.registerURL+"/register", url.Values{
		"token": {token},
	})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Missing token
	resp, err = http.PostForm(b.registerURL+"/register", url.Values{
		"email": {"alice@example.com"},
	})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestRegistrationHTTP_HealthEndpoint(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	resp, err := http.Get(b.registerURL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHandleRegisterCommand(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	// Simulate a /register message
	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "private"},
		Text:      "/register",
	}

	handled := b.handleBotCommand(tgMsg)
	assert.True(t, handled)

	// Should have sent a reply with a registration link
	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "/register?token=") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestHandleRegisterCommand_AlreadyRegistered(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	b.setUserMapping("456", "alice@example.com")

	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "private"},
		Text:      "/register",
	}

	b.handleBotCommand(tgMsg)

	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "already linked") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestHandleRegisterCommand_WithBotMention(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "group"},
		Text:      "/register@test_bot",
	}

	handled := b.handleBotCommand(tgMsg)
	assert.True(t, handled)

	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "/register?token=") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestHandleUnregisterCommand(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	b.setUserMapping("456", "alice@example.com")

	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "private"},
		Text:      "/unregister",
	}

	handled := b.handleBotCommand(tgMsg)
	assert.True(t, handled)

	// Mapping should be removed
	_, ok := b.getUserMapping("456")
	assert.False(t, ok)

	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "unlinked") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestHandleUnregisterCommand_NotRegistered(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "private"},
		Text:      "/unregister",
	}

	b.handleBotCommand(tgMsg)

	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if m.ChatID == 789 && strings.Contains(m.Text, "don't have a linked") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

func TestHandleBotCommand_UnrecognizedCommand(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "private"},
		Text:      "/help",
	}

	handled := b.handleBotCommand(tgMsg)
	assert.False(t, handled)
}

func TestRegisterCommand_InPollingLoop(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	// Queue a /register update
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "/register",
			},
		},
	})

	var received int32
	b.InboundHandler = func(_ string, _ *messages.StructuredMessage) {
		atomic.AddInt32(&received, 1)
	}

	require.NoError(t, b.Subscribe("scion.>"))

	// Should send a registration link but NOT forward to hub
	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if strings.Contains(m.Text, "/register?token=") {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)

	// Give some time to make sure no inbound delivery happens
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&received), "/register should not be forwarded to hub")
}

func TestUserMappingPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mappings.json")

	b := New(slog.Default())
	defer b.Close()

	b.setUserMapping("456", "alice@example.com")
	b.setUserMapping("789", "bob@example.com")

	require.NoError(t, b.saveUserMappings(path))

	// Read back
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var loaded map[string]string
	require.NoError(t, json.Unmarshal(data, &loaded))
	assert.Equal(t, "alice@example.com", loaded["456"])
	assert.Equal(t, "bob@example.com", loaded["789"])

	// Load into a new broker
	b2 := New(slog.Default())
	defer b2.Close()

	require.NoError(t, b2.loadUserMappings(path))
	email, ok := b2.getUserMapping("456")
	assert.True(t, ok)
	assert.Equal(t, "alice@example.com", email)
}

func TestUserMappingPersistence_LoadNonexistent(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	err := b.loadUserMappings("/nonexistent/path/mappings.json")
	assert.NoError(t, err, "loading nonexistent file should not error")
}

func TestUserMappingPersistence_MergePreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mappings.json")

	// Write a file with one mapping
	data := []byte(`{"456": "alice@example.com"}`)
	require.NoError(t, os.WriteFile(path, data, 0600))

	b := New(slog.Default())
	defer b.Close()

	// Set an existing mapping that should NOT be overwritten
	b.setUserMapping("456", "alice-override@example.com")

	require.NoError(t, b.loadUserMappings(path))

	email, ok := b.getUserMapping("456")
	assert.True(t, ok)
	assert.Equal(t, "alice-override@example.com", email, "existing mapping should not be overwritten by file")
}

func TestRegistrationHTTP_PostWithPersistence(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	dir := t.TempDir()
	mappingsPath := filepath.Join(dir, "mappings.json")

	b := New(slog.Default())
	t.Cleanup(func() { b.Close() })

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"plugin_name":   "test-telegram",
		"register_addr": "127.0.0.1:0",
		"mappings_file": mappingsPath,
	})
	require.NoError(t, err)

	token := b.regServer.createToken("456", "alice", 789)

	resp, err := http.PostForm(b.registerURL+"/register", url.Values{
		"token": {token},
		"email": {"alice@example.com"},
	})
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify file was written
	data, err := os.ReadFile(mappingsPath)
	require.NoError(t, err)

	var saved map[string]string
	require.NoError(t, json.Unmarshal(data, &saved))
	assert.Equal(t, "alice@example.com", saved["456"])
}

func TestEndToEnd_RegisterThenMessage(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	b.mu.Lock()
	b.chatRoutes[789] = "scion.project.p1.agent.coder.messages"
	b.mu.Unlock()

	// Step 1: User sends /register → get link
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 1,
			Message: &TGMessage{
				MessageID: 1,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "/register",
			},
		},
	})

	var inboundMsgs []*messages.StructuredMessage
	var inboundMu sync.Mutex
	b.InboundHandler = func(_ string, msg *messages.StructuredMessage) {
		inboundMu.Lock()
		inboundMsgs = append(inboundMsgs, msg)
		inboundMu.Unlock()
	}

	require.NoError(t, b.Subscribe("scion.>"))

	// Wait for the registration link to be sent
	var regLink string
	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if strings.Contains(m.Text, "/register?token=") {
				regLink = m.Text
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)

	// Step 2: Extract token and complete registration via HTTP
	idx := strings.Index(regLink, "/register?token=")
	require.Greater(t, idx, 0)
	tokenURL := regLink[idx:]
	// Find end of URL (newline)
	if nlIdx := strings.Index(tokenURL, "\n"); nlIdx > 0 {
		tokenURL = tokenURL[:nlIdx]
	}

	resp, err := http.PostForm(b.registerURL+tokenURL, url.Values{
		"email": {"alice@scion.dev"},
	})
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Step 3: Send a normal message — should be attributed to scion user
	tgSrv.setUpdates([]Update{
		{
			UpdateID: 2,
			Message: &TGMessage{
				MessageID: 2,
				From:      &TGUser{ID: 456, Username: "alice"},
				Chat:      TGChat{ID: 789, Type: "private"},
				Date:      time.Now().Unix(),
				Text:      "hello after registration",
			},
		},
	})

	require.Eventually(t, func() bool {
		inboundMu.Lock()
		defer inboundMu.Unlock()
		return len(inboundMsgs) > 0
	}, 5*time.Second, 50*time.Millisecond)

	inboundMu.Lock()
	lastMsg := inboundMsgs[len(inboundMsgs)-1]
	inboundMu.Unlock()

	assert.Equal(t, "user:alice@scion.dev", lastMsg.Sender,
		"after registration, sender should be the mapped scion user")
	assert.Equal(t, "hello after registration", lastMsg.Msg)
}

func TestConfigureWithRegistration(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)

	b := New(slog.Default())
	defer b.Close()

	err := b.Configure(map[string]string{
		"bot_token":     "test-token",
		"api_base_url":  tgSrv.srv.URL,
		"register_addr": "127.0.0.1:0",
		"register_url":  "https://register.example.com",
	})
	require.NoError(t, err)

	assert.NotNil(t, b.regServer)
	assert.Equal(t, "https://register.example.com", b.registerURL)
}

func TestConfigureWithoutRegistration(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	// Without register_addr, no registration server
	assert.Nil(t, b.regServer)
}

func TestRegisterCommand_NoServer(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBroker(t, tgSrv)

	tgMsg := &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 456, Username: "alice"},
		Chat:      TGChat{ID: 789, Type: "private"},
		Text:      "/register",
	}

	b.handleBotCommand(tgMsg)

	require.Eventually(t, func() bool {
		msgs := tgSrv.getSentMessages()
		for _, m := range msgs {
			if strings.Contains(m.Text, "not available") || strings.Contains(m.Text, "not configured") {
				return true
			}
		}
		return false
	}, 2*time.Second, 50*time.Millisecond)
}

// Import needed for the test that uses messages.StructuredMessage via InboundHandler.
// The import is done via the existing telegram_test.go file which already imports it.
// This file shares the package so it has access.

func TestGetInfo_IncludesRegistration(t *testing.T) {
	b := New(slog.Default())
	defer b.Close()

	info, err := b.GetInfo()
	require.NoError(t, err)
	assert.Contains(t, info.Capabilities, "user-registration")
}

func TestRegistrationHTTP_MethodNotAllowed(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	b := newTestBrokerWithRegistration(t, tgSrv)

	req, _ := http.NewRequest(http.MethodDelete, b.registerURL+"/register?token=abc", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

var _ = fmt.Sprintf
