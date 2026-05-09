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

package agent

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// deliveryRecord captures a single call to the delivery function.
type deliveryRecord struct {
	agentID   string
	projectID   string
	message   string
	interrupt bool
}

func TestMessageBuffer_SingleMessage(t *testing.T) {
	// A single message should be delivered after the debounce delay.
	var mu sync.Mutex
	var deliveries []deliveryRecord
	done := make(chan struct{}, 1)

	buf := NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		deliveries = append(deliveries, deliveryRecord{agentID, projectID, message, interrupt})
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer buf.Close()

	buf.Send("agent-1", "grove-a", "hello")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}
	if deliveries[0].agentID != "agent-1" {
		t.Errorf("expected agent-1, got %s", deliveries[0].agentID)
	}
	if deliveries[0].projectID != "grove-a" {
		t.Errorf("expected grove-a, got %s", deliveries[0].projectID)
	}
	if deliveries[0].message != "hello" {
		t.Errorf("expected 'hello', got %q", deliveries[0].message)
	}
}

func TestMessageBuffer_CoalescesRapidMessages(t *testing.T) {
	// Multiple messages sent within the debounce window should be
	// concatenated and delivered as a single combined message.
	var mu sync.Mutex
	var deliveries []deliveryRecord
	done := make(chan struct{}, 1)

	buf := NewMessageBuffer(200*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		deliveries = append(deliveries, deliveryRecord{agentID, projectID, message, interrupt})
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer buf.Close()

	// Send three messages in rapid succession — all within the 200ms window.
	buf.Send("agent-1", "grove-a", "msg-1")
	time.Sleep(50 * time.Millisecond)
	buf.Send("agent-1", "grove-a", "msg-2")
	time.Sleep(50 * time.Millisecond)
	buf.Send("agent-1", "grove-a", "msg-3")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery (coalesced), got %d", len(deliveries))
	}
	// All three messages should be joined with double-newline separators.
	expected := "msg-1\n\nmsg-2\n\nmsg-3"
	if deliveries[0].message != expected {
		t.Errorf("expected %q, got %q", expected, deliveries[0].message)
	}
}

func TestMessageBuffer_SeparateAgents(t *testing.T) {
	// Messages to different agents should be buffered independently.
	var mu sync.Mutex
	var deliveries []deliveryRecord
	done := make(chan struct{}, 2)

	buf := NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		deliveries = append(deliveries, deliveryRecord{agentID, projectID, message, interrupt})
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer buf.Close()

	buf.Send("agent-1", "grove-a", "for-agent-1")
	buf.Send("agent-2", "grove-a", "for-agent-2")

	// Wait for both deliveries.
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for delivery")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(deliveries))
	}

	// Check both agents received their messages (order not guaranteed).
	got := map[string]string{}
	for _, d := range deliveries {
		got[d.agentID] = d.message
	}
	if got["agent-1"] != "for-agent-1" {
		t.Errorf("agent-1 got %q", got["agent-1"])
	}
	if got["agent-2"] != "for-agent-2" {
		t.Errorf("agent-2 got %q", got["agent-2"])
	}
}

func TestMessageBuffer_SameAgentDifferentProjects(t *testing.T) {
	// Same agent slug in different groves should be buffered independently.
	var mu sync.Mutex
	var deliveries []deliveryRecord
	done := make(chan struct{}, 2)

	buf := NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		deliveries = append(deliveries, deliveryRecord{agentID, projectID, message, interrupt})
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer buf.Close()

	buf.Send("manager", "grove-a", "for-grove-a")
	buf.Send("manager", "grove-b", "for-grove-b")

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for delivery")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(deliveries))
	}

	got := map[string]string{}
	for _, d := range deliveries {
		got[d.projectID] = d.message
	}
	if got["grove-a"] != "for-grove-a" {
		t.Errorf("grove-a got %q", got["grove-a"])
	}
	if got["grove-b"] != "for-grove-b" {
		t.Errorf("grove-b got %q", got["grove-b"])
	}
}

func TestMessageBuffer_DebounceResetsTimer(t *testing.T) {
	// Verify that each new message resets the debounce timer, so delivery
	// happens bufferDelay after the LAST message, not the first.
	var mu sync.Mutex
	var deliveries []deliveryRecord
	done := make(chan struct{}, 1)

	buf := NewMessageBuffer(150*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		deliveries = append(deliveries, deliveryRecord{agentID, projectID, message, interrupt})
		mu.Unlock()
		done <- struct{}{}
		return nil
	})
	defer buf.Close()

	buf.Send("agent-1", "", "first")
	time.Sleep(100 * time.Millisecond) // 100ms in — timer should NOT have fired yet

	// Verify no delivery has happened yet (debounce window is 150ms).
	mu.Lock()
	count := len(deliveries)
	mu.Unlock()
	if count != 0 {
		t.Fatal("message was delivered too early (before debounce window)")
	}

	// Send another message — this resets the 150ms timer.
	buf.Send("agent-1", "", "second")

	// Wait 100ms more (200ms total since first, 100ms since second).
	// Timer should still NOT have fired (needs 150ms from second message).
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	count = len(deliveries)
	mu.Unlock()
	if count != 0 {
		t.Fatal("message was delivered before debounce expired after reset")
	}

	// Now wait for the delivery to happen.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliveries))
	}
	if !strings.Contains(deliveries[0].message, "first") || !strings.Contains(deliveries[0].message, "second") {
		t.Errorf("expected both messages, got %q", deliveries[0].message)
	}
}

func TestMessageBuffer_Close(t *testing.T) {
	// Close should flush all pending messages immediately.
	var mu sync.Mutex
	var deliveries []deliveryRecord

	buf := NewMessageBuffer(10*time.Second, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		deliveries = append(deliveries, deliveryRecord{agentID, projectID, message, interrupt})
		mu.Unlock()
		return nil
	})

	// Send messages with a very long delay (10s) so they won't auto-flush.
	buf.Send("agent-1", "grove-a", "pending-1")
	buf.Send("agent-2", "grove-a", "pending-2")

	// Close should flush everything immediately.
	buf.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("expected 2 deliveries after Close, got %d", len(deliveries))
	}
}
