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

package chatapp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// recordingMessenger records all SendMessage calls for test assertions.
type recordingMessenger struct {
	mu       sync.Mutex
	calls    []SendMessageRequest
	delay    time.Duration // optional per-send delay
	failNext bool          // if set, next send returns an error
}

func (m *recordingMessenger) SendMessage(_ context.Context, req SendMessageRequest) (string, error) {
	if m.delay > 0 {
		time.Sleep(m.delay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return "", fmt.Errorf("simulated send failure")
	}
	m.calls = append(m.calls, req)
	return fmt.Sprintf("msg-%d", len(m.calls)), nil
}

func (m *recordingMessenger) SendCard(_ context.Context, spaceID string, card Card) (string, error) {
	return m.SendMessage(context.Background(), SendMessageRequest{SpaceID: spaceID, Card: &card})
}
func (m *recordingMessenger) UpdateMessage(context.Context, string, SendMessageRequest) error {
	return nil
}
func (m *recordingMessenger) OpenDialog(context.Context, string, Dialog) error   { return nil }
func (m *recordingMessenger) UpdateDialog(context.Context, string, Dialog) error { return nil }
func (m *recordingMessenger) GetUser(context.Context, string) (*ChatUser, error) {
	return nil, nil
}
func (m *recordingMessenger) SetAgentIdentity(context.Context, AgentIdentity) error { return nil }

func (m *recordingMessenger) getCalls() []SendMessageRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SendMessageRequest, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestSendQueue_SendAndClose(t *testing.T) {
	rm := &recordingMessenger{}
	log := slog.Default()
	sq := NewSendQueue(rm, log, 10, 1*time.Millisecond)

	ctx := context.Background()

	// Send three messages to the same space.
	for i := 0; i < 3; i++ {
		msgID, err := sq.Send(ctx, SendMessageRequest{
			SpaceID: "spaces/test1",
			Text:    fmt.Sprintf("message-%d", i),
		})
		if err != nil {
			t.Fatalf("Send(%d) failed: %v", i, err)
		}
		if msgID == "" {
			t.Errorf("Send(%d) returned empty message ID", i)
		}
	}

	// Send to a different space to verify per-space workers.
	msgID, err := sq.Send(ctx, SendMessageRequest{
		SpaceID: "spaces/test2",
		Text:    "other-space-message",
	})
	if err != nil {
		t.Fatalf("Send to second space failed: %v", err)
	}
	if msgID == "" {
		t.Error("Send to second space returned empty message ID")
	}

	// Close the queue — should drain cleanly.
	sq.Close()

	calls := rm.getCalls()
	if len(calls) != 4 {
		t.Fatalf("expected 4 sends, got %d", len(calls))
	}

	// Verify messages were sent to the correct spaces.
	space1Count := 0
	space2Count := 0
	for _, c := range calls {
		switch c.SpaceID {
		case "spaces/test1":
			space1Count++
		case "spaces/test2":
			space2Count++
		}
	}
	if space1Count != 3 {
		t.Errorf("expected 3 messages to spaces/test1, got %d", space1Count)
	}
	if space2Count != 1 {
		t.Errorf("expected 1 message to spaces/test2, got %d", space2Count)
	}

	// Sending after close should return an error.
	_, err = sq.Send(ctx, SendMessageRequest{SpaceID: "spaces/test1", Text: "after-close"})
	if err == nil {
		t.Error("expected error sending after Close(), got nil")
	}
}

func TestSendQueue_Overflow(t *testing.T) {
	// Use a slow messenger to fill up the queue.
	rm := &recordingMessenger{delay: 50 * time.Millisecond}
	log := slog.Default()
	// Buffer size of 2 so we can overflow.
	sq := NewSendQueue(rm, log, 2, 1*time.Millisecond)

	ctx := context.Background()

	// Send first message — it will be picked up by the worker and block.
	go sq.Send(ctx, SendMessageRequest{SpaceID: "spaces/overflow", Text: "msg-0"})
	// Brief wait for the worker to pick up msg-0.
	time.Sleep(10 * time.Millisecond)

	// Fill the buffer (2 slots).
	go sq.Send(ctx, SendMessageRequest{SpaceID: "spaces/overflow", Text: "msg-1"})
	go sq.Send(ctx, SendMessageRequest{SpaceID: "spaces/overflow", Text: "msg-2"})
	time.Sleep(10 * time.Millisecond)

	// This should trigger overflow — drops oldest queued message.
	go sq.Send(ctx, SendMessageRequest{SpaceID: "spaces/overflow", Text: "msg-3"})
	time.Sleep(10 * time.Millisecond)

	// Wait for all messages to drain.
	sq.Close()

	calls := rm.getCalls()
	// At least the first message and the overflow replacement should have been sent.
	// The exact count depends on timing, but we should have at least 3 delivered
	// (msg-0 was already being sent, one of msg-1/msg-2 was dropped, msg-3 was enqueued).
	if len(calls) < 3 {
		t.Errorf("expected at least 3 delivered messages after overflow, got %d", len(calls))
	}
}

func TestSendQueue_ContextCancellation(t *testing.T) {
	// Slow messenger so the message stays queued.
	rm := &recordingMessenger{delay: 500 * time.Millisecond}
	log := slog.Default()
	sq := NewSendQueue(rm, log, 10, 1*time.Millisecond)
	defer sq.Close()

	// Block the worker with a first message.
	go sq.Send(context.Background(), SendMessageRequest{SpaceID: "spaces/cancel", Text: "blocking"})
	time.Sleep(10 * time.Millisecond)

	// Send with a short-lived context.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := sq.Send(ctx, SendMessageRequest{SpaceID: "spaces/cancel", Text: "should-timeout"})
	if err == nil {
		t.Error("expected context deadline error, got nil")
	}
}
