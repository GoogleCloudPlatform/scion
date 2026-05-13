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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebhookServer_ValidUpdate(t *testing.T) {
	var received []Update
	var mu sync.Mutex

	ws := NewWebhookServer(":0", "test-secret", func(u Update) {
		mu.Lock()
		received = append(received, u)
		mu.Unlock()
	}, slog.Default())

	addr, err := ws.Start()
	require.NoError(t, err)
	defer ws.Stop(context.Background())

	update := Update{
		UpdateID: 42,
		Message: &TGMessage{
			MessageID: 1,
			From:      &TGUser{ID: 456, Username: "alice"},
			Chat:      TGChat{ID: -200, Type: "group"},
			Text:      "hello webhook",
		},
	}
	body, err := json.Marshal(update)
	require.NoError(t, err)

	req, err := http.NewRequest("POST", "http://"+addr+webhookPath, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(secretTokenHeader, "test-secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Give a moment for async processing.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 1)
	assert.Equal(t, int64(42), received[0].UpdateID)
	assert.Equal(t, "hello webhook", received[0].Message.Text)
}

func TestWebhookServer_InvalidSecret(t *testing.T) {
	ws := NewWebhookServer(":0", "correct-secret", func(u Update) {
		t.Fatal("handler should not be called")
	}, slog.Default())

	addr, err := ws.Start()
	require.NoError(t, err)
	defer ws.Stop(context.Background())

	update := Update{UpdateID: 1}
	body, _ := json.Marshal(update)

	req, err := http.NewRequest("POST", "http://"+addr+webhookPath, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(secretTokenHeader, "wrong-secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWebhookServer_MalformedBody(t *testing.T) {
	ws := NewWebhookServer(":0", "test-secret", func(u Update) {
		t.Fatal("handler should not be called")
	}, slog.Default())

	addr, err := ws.Start()
	require.NoError(t, err)
	defer ws.Stop(context.Background())

	req, err := http.NewRequest("POST", "http://"+addr+webhookPath, bytes.NewReader([]byte("not json {")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(secretTokenHeader, "test-secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestWebhookServer_MethodNotAllowed(t *testing.T) {
	ws := NewWebhookServer(":0", "", func(u Update) {
		t.Fatal("handler should not be called")
	}, slog.Default())

	addr, err := ws.Start()
	require.NoError(t, err)
	defer ws.Stop(context.Background())

	resp, err := http.Get("http://" + addr + webhookPath)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestWebhookServer_NoSecretAllowsAll(t *testing.T) {
	var received []Update
	var mu sync.Mutex

	ws := NewWebhookServer(":0", "", func(u Update) {
		mu.Lock()
		received = append(received, u)
		mu.Unlock()
	}, slog.Default())

	addr, err := ws.Start()
	require.NoError(t, err)
	defer ws.Stop(context.Background())

	update := Update{UpdateID: 99}
	body, _ := json.Marshal(update)

	req, err := http.NewRequest("POST", "http://"+addr+webhookPath, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	// No secret header set

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 1)
	assert.Equal(t, int64(99), received[0].UpdateID)
}

func TestWebhookServer_CallbackQuery(t *testing.T) {
	var received []Update
	var mu sync.Mutex

	ws := NewWebhookServer(":0", "secret", func(u Update) {
		mu.Lock()
		received = append(received, u)
		mu.Unlock()
	}, slog.Default())

	addr, err := ws.Start()
	require.NoError(t, err)
	defer ws.Stop(context.Background())

	update := Update{
		UpdateID: 55,
		CallbackQuery: &CallbackQuery{
			ID:   "cb-1",
			From: &TGUser{ID: 456, Username: "alice"},
			Data: "ask:yes:req-123",
		},
	}
	body, _ := json.Marshal(update)

	req, err := http.NewRequest("POST", "http://"+addr+webhookPath, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set(secretTokenHeader, "secret")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 1)
	assert.NotNil(t, received[0].CallbackQuery)
	assert.Equal(t, "ask:yes:req-123", received[0].CallbackQuery.Data)
}

func TestWebhookServer_GracefulStop(t *testing.T) {
	ws := NewWebhookServer(":0", "", func(u Update) {}, slog.Default())

	_, err := ws.Start()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = ws.Stop(ctx)
	require.NoError(t, err)
}

// TestWebhookHandler_Direct tests the handler via httptest without starting a real server.
func TestWebhookHandler_Direct(t *testing.T) {
	var received Update
	done := make(chan struct{}, 1)

	ws := NewWebhookServer(":0", "mysecret", func(u Update) {
		received = u
		select {
		case done <- struct{}{}:
		default:
		}
	}, slog.Default())

	update := Update{
		UpdateID: 10,
		Message: &TGMessage{
			MessageID: 5,
			Text:      "direct test",
			Chat:      TGChat{ID: 100, Type: "private"},
		},
	}
	body, _ := json.Marshal(update)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", webhookPath, bytes.NewReader(body))
	req.Header.Set(secretTokenHeader, "mysecret")

	ws.handleWebhook(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}

	assert.Equal(t, int64(10), received.UpdateID)
	assert.Equal(t, "direct test", received.Message.Text)
}
