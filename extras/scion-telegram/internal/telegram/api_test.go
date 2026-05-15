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
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestSetMyCommands_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/setMyCommands", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var reqBody setMyCommandsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Len(t, reqBody.Commands, 2)
		assert.Equal(t, "help", reqBody.Commands[0].Command)
		assert.Equal(t, "Show help", reqBody.Commands[0].Description)
		require.NotNil(t, reqBody.Scope)
		assert.Equal(t, "all_private_chats", reqBody.Scope.Type)

		resp := apiResponse{OK: true, Result: mustJSON(t, true)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.SetMyCommands(context.Background(), []BotCommand{
		{Command: "help", Description: "Show help"},
		{Command: "status", Description: "Show status"},
	}, &BotCommandScope{Type: "all_private_chats"})
	require.NoError(t, err)
}

func TestSetMyCommands_NoScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody setMyCommandsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Nil(t, reqBody.Scope)

		resp := apiResponse{OK: true, Result: mustJSON(t, true)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.SetMyCommands(context.Background(), []BotCommand{
		{Command: "help", Description: "Show help"},
	}, nil)
	require.NoError(t, err)
}

func TestSetMyCommands_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Bad Request: invalid command",
			ErrorCode:   400,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.SetMyCommands(context.Background(), []BotCommand{
		{Command: "bad!", Description: "invalid"},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid command")
}

// mustJSON marshals v to json.RawMessage, failing the test on error.
func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func TestSetWebhook_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/setWebhook", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var reqBody setWebhookRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Equal(t, "https://example.com/telegram/webhook", reqBody.URL)
		assert.Equal(t, "my-secret", reqBody.SecretToken)
		assert.Contains(t, reqBody.AllowedUpdates, "message")
		assert.Contains(t, reqBody.AllowedUpdates, "callback_query")

		resp := apiResponse{OK: true, Result: mustJSON(t, true)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.SetWebhook(context.Background(), "https://example.com/telegram/webhook", "my-secret")
	require.NoError(t, err)
}

func TestSetWebhook_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Bad Request: bad webhook",
			ErrorCode:   400,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.SetWebhook(context.Background(), "https://example.com/webhook", "secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad webhook")
}

func TestSetWebhook_NoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody setWebhookRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Empty(t, reqBody.SecretToken)

		resp := apiResponse{OK: true, Result: mustJSON(t, true)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.SetWebhook(context.Background(), "https://example.com/webhook", "")
	require.NoError(t, err)
}

func TestDeleteWebhook_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/deleteWebhook", r.URL.Path)
		assert.Equal(t, "POST", r.Method)

		resp := apiResponse{OK: true, Result: mustJSON(t, true)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.DeleteWebhook(context.Background())
	require.NoError(t, err)
}

func TestDeleteWebhook_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Internal Server Error",
			ErrorCode:   500,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	err := client.DeleteWebhook(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Internal Server Error")
}

func TestSendMessageWithForceReply_NoKeyboard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/sendMessage", r.URL.Path)

		var reqBody sendMessageForceReplyRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
		assert.Equal(t, int64(111), reqBody.ChatID)
		assert.Equal(t, "HTML", reqBody.ParseMode)

		var markup ForceReply
		require.NoError(t, json.Unmarshal(reqBody.ReplyMarkup, &markup))
		assert.True(t, markup.ForceReply)
		assert.True(t, markup.Selective)

		result := TGMessage{
			MessageID: 55,
			Chat:      TGChat{ID: 111, Type: "private"},
		}
		resp := apiResponse{OK: true, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	msg, err := client.SendMessageWithForceReply(context.Background(), 111, "What next?", "HTML", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(55), msg.MessageID)
}

func TestSendMessageWithForceReply_WithKeyboard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody sendMessageForceReplyRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))

		var markup InlineKeyboardMarkup
		require.NoError(t, json.Unmarshal(reqBody.ReplyMarkup, &markup))
		require.Len(t, markup.InlineKeyboard, 1)
		assert.Equal(t, "Yes", markup.InlineKeyboard[0][0].Text)

		result := TGMessage{MessageID: 56, Chat: TGChat{ID: 111, Type: "private"}}
		resp := apiResponse{OK: true, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Yes", CallbackData: "y"}, {Text: "No", CallbackData: "n"}},
		},
	}
	client := newTestAPIClient(t, srv)
	msg, err := client.SendMessageWithForceReply(context.Background(), 111, "Confirm?", "HTML", kb)
	require.NoError(t, err)
	assert.Equal(t, int64(56), msg.MessageID)
}

func TestSendDocument_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/sendDocument", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Contains(t, r.Header.Get("Content-Type"), "multipart/form-data")

		err := r.ParseMultipartForm(10 << 20)
		require.NoError(t, err)

		assert.Equal(t, "789", r.FormValue("chat_id"))
		assert.Equal(t, "Here is the report", r.FormValue("caption"))
		assert.Empty(t, r.FormValue("parse_mode"))

		file, header, err := r.FormFile("document")
		require.NoError(t, err)
		defer file.Close()
		assert.Equal(t, "report.pdf", header.Filename)

		data, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Equal(t, "fake-pdf-content", string(data))

		result := TGMessage{
			MessageID: 99,
			Chat:      TGChat{ID: 789, Type: "private"},
		}
		resp := apiResponse{OK: true, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	doc := strings.NewReader("fake-pdf-content")
	msg, err := client.SendDocument(context.Background(), 789, "report.pdf", doc, "Here is the report", "")
	require.NoError(t, err)
	assert.Equal(t, int64(99), msg.MessageID)
}

func TestSendDocument_WithParseMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseMultipartForm(10 << 20)
		require.NoError(t, err)
		assert.Equal(t, "HTML", r.FormValue("parse_mode"))

		result := TGMessage{MessageID: 100, Chat: TGChat{ID: 789}}
		resp := apiResponse{OK: true, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	doc := strings.NewReader("content")
	msg, err := client.SendDocument(context.Background(), 789, "file.txt", doc, "caption", "HTML")
	require.NoError(t, err)
	assert.Equal(t, int64(100), msg.MessageID)
}

func TestSendDocument_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Bad Request: file is too big",
			ErrorCode:   400,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	doc := strings.NewReader("big-content")
	_, err := client.SendDocument(context.Background(), 789, "huge.zip", doc, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file is too big")
}

func TestSendDocument_EmptyCaption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseMultipartForm(10 << 20)
		require.NoError(t, err)
		assert.Empty(t, r.FormValue("caption"))

		result := TGMessage{MessageID: 101, Chat: TGChat{ID: 789}}
		resp := apiResponse{OK: true, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	doc := strings.NewReader("content")
	msg, err := client.SendDocument(context.Background(), 789, "data.csv", doc, "", "")
	require.NoError(t, err)
	assert.Equal(t, int64(101), msg.MessageID)
}

func TestGetFile_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/bottest-token/getFile", r.URL.Path)
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "file-abc-123", r.URL.Query().Get("file_id"))

		result := TGFile{
			FileID:   "file-abc-123",
			FileSize: 12345,
			FilePath: "photos/file_0.jpg",
		}
		resp := apiResponse{OK: true, Result: mustJSON(t, result)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	file, err := client.GetFile(context.Background(), "file-abc-123")
	require.NoError(t, err)
	assert.Equal(t, "file-abc-123", file.FileID)
	assert.Equal(t, int64(12345), file.FileSize)
	assert.Equal(t, "photos/file_0.jpg", file.FilePath)
}

func TestGetFile_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := apiResponse{
			OK:          false,
			Description: "Bad Request: invalid file_id",
			ErrorCode:   400,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	_, err := client.GetFile(context.Background(), "bad-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid file_id")
}

func TestDownloadFile_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/file/bottest-token/photos/file_0.jpg", r.URL.Path)
		assert.Equal(t, "GET", r.Method)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("fake-image-data"))
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	rc, err := client.DownloadFile(context.Background(), "photos/file_0.jpg")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "fake-image-data", string(data))
}

func TestDownloadFile_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestAPIClient(t, srv)
	_, err := client.DownloadFile(context.Background(), "photos/missing.jpg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 404")
}
