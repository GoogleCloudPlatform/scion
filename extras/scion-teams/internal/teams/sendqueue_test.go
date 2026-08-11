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

package teams

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendQueue_EnqueueAndSend(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var sentCount int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sentCount, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "sent-act"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	sq := NewSendQueue(sender, 10, 10*time.Millisecond, slog.Default())
	defer sq.Close()

	activity := &Activity{Type: "message", Text: "test"}
	actID, err := sq.Enqueue(context.Background(), "conv-1", apiServer.URL, activity)

	require.NoError(t, err)
	assert.Equal(t, "sent-act", actID)
	assert.Equal(t, int32(1), atomic.LoadInt32(&sentCount))
}

func TestSendQueue_MultipleSendsSerialize(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var timestamps []time.Time
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timestamps = append(timestamps, time.Now())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "act"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	// Use a 50ms min delay to make timing measurable.
	sq := NewSendQueue(sender, 10, 50*time.Millisecond, slog.Default())
	defer sq.Close()

	// Send 3 messages to the same conversation.
	for i := 0; i < 3; i++ {
		activity := &Activity{Type: "message", Text: "test"}
		_, err := sq.Enqueue(context.Background(), "conv-1", apiServer.URL, activity)
		require.NoError(t, err)
	}

	// Verify messages were spaced by at least the min delay.
	require.Len(t, timestamps, 3)
	for i := 1; i < len(timestamps); i++ {
		gap := timestamps[i].Sub(timestamps[i-1])
		assert.GreaterOrEqual(t, gap.Milliseconds(), int64(40), // Allow 10ms tolerance
			"gap between sends %d should be >= 40ms, got %v", i, gap)
	}
}

func TestSendQueue_ClosedRejectsNew(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	sender := NewSender(tp, slog.Default())
	sq := NewSendQueue(sender, 10, 10*time.Millisecond, slog.Default())
	sq.Close()

	activity := &Activity{Type: "message", Text: "test"}
	_, err := sq.Enqueue(context.Background(), "conv-1", "http://example.com", activity)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestSendQueue_OverflowDropsOldest(t *testing.T) {
	// Test that when the queue is full, the oldest message is dropped.
	// We test the enqueue method directly without a running worker to
	// avoid timing-dependent blocking.

	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	sender := NewSender(tp, slog.Default())

	// Queue size of 2.
	sq := NewSendQueue(sender, 2, 10*time.Millisecond, slog.Default())
	defer sq.Close()

	// Manually create a queue entry so we can fill its buffer without a worker.
	sq.mu.Lock()
	cq := &conversationQueue{
		ch: make(chan *sendQueueRequest, 2),
	}
	sq.queues["conv-overflow"] = cq
	sq.mu.Unlock()

	// Fill the buffer.
	droppedResult := make(chan *sendQueueResult, 1)
	cq.ch <- &sendQueueRequest{
		conversationID: "conv-overflow",
		activity:       &Activity{Type: "message", Text: "oldest"},
		result:         droppedResult,
	}
	cq.ch <- &sendQueueRequest{
		conversationID: "conv-overflow",
		activity:       &Activity{Type: "message", Text: "second"},
	}

	// Next enqueue should drop the oldest.
	err := sq.enqueue("conv-overflow", &sendQueueRequest{
		conversationID: "conv-overflow",
		activity:       &Activity{Type: "message", Text: "overflow"},
	})
	assert.NoError(t, err)

	// The dropped request's result channel should receive an error.
	select {
	case result := <-droppedResult:
		assert.Error(t, result.err)
		assert.Contains(t, result.err.Error(), "overflow")
	case <-time.After(time.Second):
		t.Fatal("expected dropped result notification")
	}
}

func TestSendQueue_429RetrySuccess(t *testing.T) {
	// R2 + R6: Verify that a 429-rated message is retried (not dropped)
	// and eventually succeeds.

	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var requestCount int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requestCount, 1)
		if count <= 2 {
			// First two requests: rate limit.
			w.Header().Set("Retry-After", "0") // 0 seconds for fast test
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Third request: succeed.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "retry-success"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	sq := NewSendQueue(sender, 10, 1*time.Millisecond, slog.Default())
	defer sq.Close()

	activity := &Activity{Type: "message", Text: "rate limited"}
	actID, err := sq.Enqueue(context.Background(), "conv-1", apiServer.URL, activity)

	require.NoError(t, err)
	assert.Equal(t, "retry-success", actID)
	assert.Equal(t, int32(3), atomic.LoadInt32(&requestCount),
		"should have retried after 429s")
}

func TestSendQueue_429ExhaustsRetries(t *testing.T) {
	// R2: Verify that after maxSendRetries 429s the error is returned (not infinite loop).

	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	var requestCount int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	sq := NewSendQueue(sender, 10, 1*time.Millisecond, slog.Default())
	defer sq.Close()

	activity := &Activity{Type: "message", Text: "always limited"}
	_, err := sq.Enqueue(context.Background(), "conv-1", apiServer.URL, activity)

	require.Error(t, err)
	var retryErr *RetryAfterError
	assert.ErrorAs(t, err, &retryErr)
	// 1 initial + 3 retries = 4 total requests.
	assert.Equal(t, int32(4), atomic.LoadInt32(&requestCount))
}

func TestSendQueue_ContextCancellation(t *testing.T) {
	tp, tokenServer := newTestTokenProvider(t)
	defer tokenServer.Close()

	// Create a server that responds slowly but finishes.
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ActivityResponse{ID: "late"})
	}))
	defer apiServer.Close()

	sender := NewSender(tp, slog.Default())
	sender.httpClient = apiServer.Client()

	sq := NewSendQueue(sender, 10, 10*time.Millisecond, slog.Default())
	defer sq.Close()

	// Cancel the context before the server responds.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	activity := &Activity{Type: "message", Text: "test"}
	_, err := sq.Enqueue(ctx, "conv-1", apiServer.URL, activity)
	// The context timeout fires before the API responds,
	// so Enqueue returns a context error.
	assert.Error(t, err)
}
