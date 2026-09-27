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
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deliveryRecord captures a single call to the delivery function.
type deliveryRecord struct {
	agentID   string
	projectID string
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

	buf.Send("agent-1", "project-a", "hello")

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
	if deliveries[0].projectID != "project-a" {
		t.Errorf("expected project-a, got %s", deliveries[0].projectID)
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
	buf.Send("agent-1", "project-a", "msg-1")
	time.Sleep(50 * time.Millisecond)
	buf.Send("agent-1", "project-a", "msg-2")
	time.Sleep(50 * time.Millisecond)
	buf.Send("agent-1", "project-a", "msg-3")

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

	buf.Send("agent-1", "project-a", "for-agent-1")
	buf.Send("agent-2", "project-a", "for-agent-2")

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
	// Same agent slug in different projects should be buffered independently.
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

	buf.Send("manager", "project-a", "for-project-a")
	buf.Send("manager", "project-b", "for-project-b")

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
	if got["project-a"] != "for-project-a" {
		t.Errorf("project-a got %q", got["project-a"])
	}
	if got["project-b"] != "for-project-b" {
		t.Errorf("project-b got %q", got["project-b"])
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
	buf.Send("agent-1", "project-a", "pending-1")
	buf.Send("agent-2", "project-a", "pending-2")

	// Close should flush everything immediately.
	buf.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("expected 2 deliveries after Close, got %d", len(deliveries))
	}
}

// TestMessageBuffer_FailureHandlersInvokedOnFlushFailure covers #1820: when a
// coalesced delivery fails, every sender that was told "accepted" is told it
// failed; a successful flush invokes no handlers.
func TestMessageBuffer_FailureHandlersInvokedOnFlushFailure(t *testing.T) {
	var fail bool
	var mu sync.Mutex
	flushed := make(chan struct{}, 4)
	buf := NewMessageBuffer(20*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		mu.Lock()
		defer mu.Unlock()
		defer func() { flushed <- struct{}{} }()
		if fail {
			return errors.New("container gone")
		}
		return nil
	})
	defer buf.Close()

	var calls []string
	var cmu sync.Mutex
	// Failure handlers run after deliverFunc returns, so completion is
	// signalled from the handlers themselves rather than from deliverFunc.
	var handled sync.WaitGroup
	handler := func(id string) DeliveryFailureHandler {
		return func(err error) {
			defer handled.Done()
			cmu.Lock()
			defer cmu.Unlock()
			calls = append(calls, id+":"+err.Error())
		}
	}

	// Successful flush: no handler calls.
	buf.SendWithFailureHandler("a", "p", "ok", handler("ok"))
	<-flushed
	cmu.Lock()
	if len(calls) != 0 {
		t.Fatalf("expected no failure calls on success, got %v", calls)
	}
	cmu.Unlock()

	// Failing flush with two coalesced messages, one without a handler.
	mu.Lock()
	fail = true
	mu.Unlock()
	handled.Add(2)
	buf.SendWithFailureHandler("a", "p", "m1", handler("m1"))
	buf.Send("a", "p", "no-handler")
	buf.SendWithFailureHandler("a", "p", "m2", handler("m2"))
	done := make(chan struct{})
	go func() { handled.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for failure handlers")
	}

	cmu.Lock()
	defer cmu.Unlock()
	if len(calls) != 2 || calls[0] != "m1:container gone" || calls[1] != "m2:container gone" {
		t.Fatalf("expected both handlers invoked with the error, got %v", calls)
	}
}

// tuneFlushRetry overrides buf's flush retry bounds so retry tests don't
// have to wait on the production backoff. Each test constructs its own
// MessageBuffer, so there is no shared state to restore afterward.
func tuneFlushRetry(buf *MessageBuffer, attempts int, backoff time.Duration) {
	buf.maxFlushAttempts = attempts
	buf.flushRetryBackoff = backoff
}

// TestMessageBuffer_RetriesTransientFailureBeforeSucceeding covers #1866:
// a transient deliverFunc failure (not a PartialDeliveryError) is retried
// within flush, so a message doesn't need to be marked failed just because
// the first delivery attempt hit a blip.
func TestMessageBuffer_RetriesTransientFailureBeforeSucceeding(t *testing.T) {
	var attempts int32
	done := make(chan struct{}, 1)
	buf := NewMessageBuffer(10*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			return errors.New("transient: container briefly unreachable")
		}
		done <- struct{}{}
		return nil
	})
	tuneFlushRetry(buf, 3, 5*time.Millisecond)
	defer buf.Close()

	var handlerCalled bool
	buf.SendWithFailureHandler("a", "p", "hello", func(error) { handlerCalled = true })

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivery to eventually succeed")
	}

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected 3 delivery attempts, got %d", got)
	}
	if handlerCalled {
		t.Fatal("failure handler must not be called once a retry succeeds")
	}
}

// TestMessageBuffer_GivesUpAfterMaxAttempts covers #1866: retries are
// bounded — a failure that never clears is reported after maxFlushAttempts,
// not retried forever.
func TestMessageBuffer_GivesUpAfterMaxAttempts(t *testing.T) {
	var attempts int32
	handled := make(chan error, 1)
	buf := NewMessageBuffer(10*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		atomic.AddInt32(&attempts, 1)
		return errors.New("permanently gone")
	})
	tuneFlushRetry(buf, 3, 5*time.Millisecond)
	defer buf.Close()

	buf.SendWithFailureHandler("a", "p", "hello", func(err error) { handled <- err })

	select {
	case err := <-handled:
		if err == nil || err.Error() != "permanently gone" {
			t.Fatalf("expected the final attempt's error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failure handler")
	}

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected exactly 3 delivery attempts (maxFlushAttempts), got %d", got)
	}
}

// TestMessageBuffer_NoRetryForPartialDeliveryError covers #1866: once
// deliverFunc reports that some content already reached the agent's
// terminal, flush must not retry — a retry would re-run the whole delivery
// (including the paste) and the agent would see the text twice.
func TestMessageBuffer_NoRetryForPartialDeliveryError(t *testing.T) {
	var attempts int32
	handled := make(chan error, 1)
	wrapped := errors.New("enter keypress failed after paste")
	buf := NewMessageBuffer(10*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		atomic.AddInt32(&attempts, 1)
		return &PartialDeliveryError{Err: wrapped}
	})
	tuneFlushRetry(buf, 3, 5*time.Millisecond)
	defer buf.Close()

	buf.SendWithFailureHandler("a", "p", "hello", func(err error) { handled <- err })

	select {
	case err := <-handled:
		if !errors.Is(err, wrapped) {
			t.Fatalf("expected the wrapped error to be reported, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failure handler")
	}

	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected exactly 1 delivery attempt for a PartialDeliveryError, got %d", got)
	}
}

func TestDeliveryFailureHandlerContext(t *testing.T) {
	ctx := context.Background()
	if DeliveryFailureHandlerFromContext(ctx) != nil {
		t.Fatal("expected nil handler on bare context")
	}
	if WithDeliveryFailureHandler(ctx, nil) != ctx {
		t.Fatal("nil handler must not wrap the context")
	}
	called := false
	ctx = WithDeliveryFailureHandler(ctx, func(error) { called = true })
	DeliveryFailureHandlerFromContext(ctx)(errors.New("x"))
	if !called {
		t.Fatal("handler from context was not the one stored")
	}
}
