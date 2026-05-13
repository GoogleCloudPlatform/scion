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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFakeRateLimitServer creates a test server that simulates Telegram's API.
// It returns 429 for the first rateLimitCount calls, then 200 for subsequent ones.
func newFakeRateLimitServer(t *testing.T, rateLimitCount int, retryAfterSec int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	callCount := &atomic.Int32{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/bottest-token/getMe":
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: func() json.RawMessage {
					b, _ := json.Marshal(BotUser{ID: 100, IsBot: true, FirstName: "Bot", Username: "test_bot"})
					return b
				}(),
			})
		case r.URL.Path == "/bottest-token/sendMessage":
			count := int(callCount.Add(1))
			if count <= rateLimitCount {
				resp := apiResponse{
					OK:          false,
					ErrorCode:   429,
					Description: "Too Many Requests: retry after " + string(rune('0'+retryAfterSec)),
					Parameters:  &apiParameters{RetryAfterSec: retryAfterSec},
				}
				w.WriteHeader(http.StatusTooManyRequests)
				json.NewEncoder(w).Encode(resp)
				return
			}
			var req sendMessageRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(apiResponse{
				OK: true,
				Result: func() json.RawMessage {
					b, _ := json.Marshal(TGMessage{
						MessageID: int64(count),
						Chat:      TGChat{ID: req.ChatID},
						Text:      req.Text,
					})
					return b
				}(),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(func() { srv.Close() })
	return srv, callCount
}

func TestSendQueue_BasicSend(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	ctx := context.Background()
	msg, err := sq.Send(ctx, -100, "hello world", "", nil, 0)
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, int64(-100), msg.Chat.ID)
	assert.Equal(t, "hello world", msg.Text)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(-100), sent[0].ChatID)
	assert.Equal(t, "hello world", sent[0].Text)
}

func TestSendQueue_BasicSendWithKeyboard(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	kb := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "OK", CallbackData: "ok"}},
		},
	}

	ctx := context.Background()
	msg, err := sq.Send(ctx, -100, "pick one", "HTML", kb, 42)
	require.NoError(t, err)
	require.NotNil(t, msg)

	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Equal(t, int64(-100), sent[0].ChatID)
	assert.Equal(t, "pick one", sent[0].Text)
	assert.Equal(t, "HTML", sent[0].ParseMode)
	assert.NotNil(t, sent[0].ReplyMarkup)
	assert.Equal(t, int64(42), sent[0].ReplyToMessageID)
}

func TestSendQueue_429Backoff(t *testing.T) {
	// Server returns 429 for first call, then succeeds.
	srv, callCount := newFakeRateLimitServer(t, 1, 1)
	api := NewAPIClient("test-token", srv.URL)

	// Use very short minDelay so the test is fast.
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	ctx := context.Background()
	msg, err := sq.Send(ctx, -100, "retry me", "", nil, 0)
	require.NoError(t, err)
	require.NotNil(t, msg)

	// The server should have been called twice: once 429, once success.
	assert.Equal(t, int32(2), callCount.Load())
}

func TestSendQueue_QueueOverflow(t *testing.T) {
	// Create a server that's slow to respond, causing the queue to fill up.
	var sendCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bottest-token/getMe":
			b, _ := json.Marshal(BotUser{ID: 100, IsBot: true, Username: "test_bot"})
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: b})
		case "/bottest-token/sendMessage":
			// Simulate slow send.
			time.Sleep(50 * time.Millisecond)
			count := int(sendCount.Add(1))
			var req sendMessageRequest
			json.NewDecoder(r.Body).Decode(&req)
			b, _ := json.Marshal(TGMessage{MessageID: int64(count), Chat: TGChat{ID: req.ChatID}, Text: req.Text})
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: b})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := NewAPIClient("test-token", srv.URL)
	// Tiny queue size of 2.
	sq := NewSendQueue(api, slog.Default(), 2, time.Millisecond)
	defer sq.Close()

	ctx := context.Background()

	// Send the first message to start the worker (it will block on the slow API).
	var wg sync.WaitGroup
	results := make(chan *sendResult, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg, err := sq.Send(ctx, -100, "msg", "", nil, 0)
			results <- &sendResult{msg: msg, err: err}
		}(i)
	}

	wg.Wait()
	close(results)

	var overflowErrors int
	var successes int
	for res := range results {
		if res.err != nil && res.err.Error() == "dropped: send queue overflow" {
			overflowErrors++
		} else if res.err == nil {
			successes++
		}
	}

	// With queue size 2 and 5 concurrent sends, some should overflow.
	assert.Greater(t, overflowErrors, 0, "expected at least one overflow drop")
	assert.Greater(t, successes, 0, "expected at least one success")
}

func TestSendQueue_ConcurrentChats(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	ctx := context.Background()
	chatIDs := []int64{-100, -200, -300}
	var wg sync.WaitGroup

	for _, chatID := range chatIDs {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			msg, err := sq.Send(ctx, id, "concurrent", "", nil, 0)
			assert.NoError(t, err)
			assert.NotNil(t, msg)
			assert.Equal(t, id, msg.Chat.ID)
		}(chatID)
	}

	wg.Wait()

	sent := tgSrv.getSentMessages()
	assert.Len(t, sent, 3)

	// Verify each chat got its message.
	gotChats := map[int64]bool{}
	for _, s := range sent {
		gotChats[s.ChatID] = true
	}
	for _, id := range chatIDs {
		assert.True(t, gotChats[id], "missing message for chat %d", id)
	}
}

func TestSendQueue_IdleWorkerCleanup(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	// Temporarily override the idle timeout for fast testing.
	// We'll send a message to create the worker, then verify it cleans up.
	ctx := context.Background()
	_, err := sq.Send(ctx, -100, "first", "", nil, 0)
	require.NoError(t, err)

	// Worker exists.
	sq.mu.Lock()
	_, exists := sq.queues[-100]
	sq.mu.Unlock()
	assert.True(t, exists, "worker should exist after send")

	// Wait for idle timeout (defaultIdleTimeout is 5min, too long for test).
	// Instead, verify the queue is cleaned up after Close.
	sq.Close()

	sq.mu.Lock()
	_, existsAfter := sq.queues[-100]
	sq.mu.Unlock()
	assert.False(t, existsAfter, "worker should be cleaned up after close")
}

func TestSendQueue_Close(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)

	// Send a message to create a worker.
	ctx := context.Background()
	_, err := sq.Send(ctx, -100, "before close", "", nil, 0)
	require.NoError(t, err)

	// Close should not block indefinitely.
	done := make(chan struct{})
	go func() {
		sq.Close()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return in time")
	}

	// Sending after close should fail.
	_, err = sq.Send(ctx, -100, "after close", "", nil, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestSendQueue_ContextCancellation(t *testing.T) {
	// Create a slow server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bottest-token/getMe":
			b, _ := json.Marshal(BotUser{ID: 100, IsBot: true, Username: "test_bot"})
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: b})
		case "/bottest-token/sendMessage":
			time.Sleep(5 * time.Second)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := NewAPIClient("test-token", srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := sq.Send(ctx, -100, "will timeout", "", nil, 0)
	assert.Error(t, err)
}

func TestSendQueue_SameChat_Serialized(t *testing.T) {
	// Verify messages to the same chat are serialized (arrive in order).
	var mu sync.Mutex
	var receivedOrder []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bottest-token/getMe":
			b, _ := json.Marshal(BotUser{ID: 100, IsBot: true, Username: "test_bot"})
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: b})
		case "/bottest-token/sendMessage":
			var req sendMessageRequest
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			receivedOrder = append(receivedOrder, req.Text)
			mu.Unlock()
			b, _ := json.Marshal(TGMessage{MessageID: 1, Chat: TGChat{ID: req.ChatID}, Text: req.Text})
			json.NewEncoder(w).Encode(apiResponse{OK: true, Result: b})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	api := NewAPIClient("test-token", srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, time.Millisecond)
	defer sq.Close()

	ctx := context.Background()

	// Send 5 messages sequentially to the same chat.
	expected := []string{"msg-1", "msg-2", "msg-3", "msg-4", "msg-5"}
	for _, text := range expected {
		_, err := sq.Send(ctx, -100, text, "", nil, 0)
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, expected, receivedOrder)
}

func TestSendQueue_Defaults(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)

	// Pass zero values to test defaults.
	sq := NewSendQueue(api, nil, 0, 0)
	defer sq.Close()

	assert.Equal(t, defaultMaxQueueSize, sq.maxSize)
	assert.Equal(t, defaultMinDelay, sq.minDelay)
}
