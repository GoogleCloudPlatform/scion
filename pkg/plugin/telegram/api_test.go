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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestAPIClient creates a TelegramAPIClient pointed at the given httptest server.
func newTestAPIClient(t *testing.T, srv *httptest.Server) *TelegramAPIClient {
	t.Helper()
	return NewAPIClient("test-token", srv.URL)
}

func TestGetMe_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/getMe", r.URL.Path)
		assert.Equal(t, "GET", r.Method)

		resp := apiResponse{
			OK: true,
			Result: mustJSON(t, BotUser{
				ID:        123,
				IsBot:     true,
				FirstName: "TestBot",
				Username:  "test_bot",
			}),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	bot, err := client.GetMe(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(123), bot.ID)
	assert.True(t, bot.IsBot)
	assert.Equal(t, "test_bot", bot.Username)
}

func TestGetMe_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Unauthorized",
			ErrorCode:   401,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	_, err := client.GetMe(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unauthorized")
}

func TestGetUpdates_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/getUpdates", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var reqBody getUpdatesRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Equal(t, int64(100), reqBody.Offset)
		assert.Equal(t, 30, reqBody.Timeout)
		assert.Contains(t, reqBody.AllowedUpdates, "message")

		updates := []Update{
			{
				UpdateID: 100,
				Message: &TGMessage{
					MessageID: 1,
					From: &TGUser{
						ID:        456,
						FirstName: "Alice",
						Username:  "alice",
					},
					Chat: TGChat{
						ID:   789,
						Type: "private",
					},
					Date: 1700000000,
					Text: "hello bot",
				},
			},
		}

		resp := apiResponse{
			OK:     true,
			Result: mustJSON(t, updates),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	updates, err := client.GetUpdates(context.Background(), 100, 30)
	require.NoError(t, err)
	require.Len(t, updates, 1)
	assert.Equal(t, int64(100), updates[0].UpdateID)
	assert.Equal(t, "hello bot", updates[0].Message.Text)
	assert.Equal(t, "alice", updates[0].Message.From.Username)
}

func TestGetUpdates_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:     true,
			Result: mustJSON(t, []Update{}),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	updates, err := client.GetUpdates(context.Background(), 1, 30)
	require.NoError(t, err)
	assert.Empty(t, updates)
}

func TestGetUpdates_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Bad Request: offset is too old",
			ErrorCode:   400,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	_, err := client.GetUpdates(context.Background(), 1, 30)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offset is too old")
}

func TestSendMessage_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/sendMessage", r.URL.Path)
		assert.Equal(t, "POST", r.Method)

		var reqBody sendMessageRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Equal(t, int64(789), reqBody.ChatID)
		assert.Equal(t, "hello world", reqBody.Text)
		assert.Empty(t, reqBody.ParseMode)

		result := TGMessage{
			MessageID: 42,
			Chat:      TGChat{ID: 789, Type: "private"},
			Date:      1700000000,
			Text:      "hello world",
		}
		resp := apiResponse{
			OK:     true,
			Result: mustJSON(t, result),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	msg, err := client.SendMessage(context.Background(), 789, "hello world", "")
	require.NoError(t, err)
	assert.Equal(t, int64(42), msg.MessageID)
}

func TestSendMessage_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Bad Request: chat not found",
			ErrorCode:   400,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	_, err := client.SendMessage(context.Background(), 999, "test", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chat not found")
}

func TestSendMessage_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// This handler should not be reached if context is canceled
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := client.SendMessage(ctx, 789, "test", "")
	require.Error(t, err)
}

// mustJSON marshals v to json.RawMessage, failing the test on error.
func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}
