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

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// testDatabaseURL returns the TEST_DATABASE_URL or skips the test.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres-dependent test")
	}
	return url
}

// TestNotifyAcceleratesDelivery verifies that NOTIFY reduces latency
// for event delivery compared to pure polling. Requires a real Postgres.
func TestNotifyAcceleratesDelivery(t *testing.T) {
	dbURL := testDatabaseURL(t)

	store, err := state.NewPostgres(dbURL)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	defer store.Close()

	log := slog.Default()
	notifier := NewNotifier(dbURL, log)

	ctx := context.Background()
	taskID := "notify-accel-1"

	// Create a task so AppendTaskEvent has a valid task_id to reference.
	now := time.Now()
	if err := store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "p1", AgentSlug: "agent1",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	defer func() {
		// Clean up test data.
		store.PurgeTaskEvents(ctx, time.Now().Add(time.Hour))
	}()

	// Register a waiter (this starts the LISTEN connection lazily).
	ch, cleanup := notifier.Register(taskID)
	defer cleanup()
	defer notifier.Stop()

	// Give the LISTEN connection a moment to establish.
	time.Sleep(100 * time.Millisecond)

	// In a goroutine, wait 50ms then append an event for the task.
	go func() {
		time.Sleep(50 * time.Millisecond)
		payload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateWorking},
		})
		store.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:  taskID,
			Kind:    "message",
			Payload: payload,
		})
	}()

	// Measure time until the waiter channel receives a signal.
	start := time.Now()
	select {
	case <-ch:
		elapsed := time.Since(start)
		t.Logf("NOTIFY delivered in %v", elapsed)
		// Should be significantly faster than the 2s poll cap.
		if elapsed > 1*time.Second {
			t.Errorf("NOTIFY delivery took %v, expected < 1s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for NOTIFY delivery")
	}
}

// TestDegradationToPurePolling verifies that all cross-instance tests pass
// with the notifier set to nil — proving polling is the correctness floor.
func TestDegradationToPurePolling(t *testing.T) {
	// This test runs the cross-instance correlation test with notifier=nil.
	// streamTaskEvents and waitForTaskEvent both accept nil notifier and
	// fall back to pure polling. The cross-instance tests in
	// crossinstance_test.go already pass nil, so this validates the contract.

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "degrade.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	taskID := "degrade-poll-1"

	if err := store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "p1", AgentSlug: "agent1",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Create a bridge with notifier=nil (pure polling mode).
	b := &Bridge{
		store:       store,
		activeTasks: make(map[string]activeTaskEntry),
		agentTasks:  make(map[string][]string),
		notifier:    nil, // explicitly nil
	}

	type result struct {
		ev  *state.TaskEvent
		err error
	}
	resultCh := make(chan result, 1)

	// Start blocking wait (uses polling only).
	go func() {
		ev, err := b.waitForTaskEvent(ctx, taskID, 5*time.Second)
		resultCh <- result{ev, err}
	}()

	// Write a response event after a short delay.
	time.Sleep(100 * time.Millisecond)
	responsePayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{
			State: TaskStateWorking,
			Message: &Message{
				MessageID: "poll-msg-1",
				Role:      RoleAgent,
				Parts:     []Part{{Text: "Response via polling"}},
			},
		},
	})
	store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "message",
		Payload: responsePayload,
	})

	// Verify delivery via polling.
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("waitForTaskEvent: %v", r.err)
		}
		if r.ev.Kind != "message" {
			t.Errorf("event kind = %q, want %q", r.ev.Kind, "message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for polling delivery")
	}

	// Also verify streamTaskEvents works with nil notifier.
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	ch := streamTaskEvents(pollCtx, store, taskID, 0, 10, nil)

	select {
	case ev := <-ch:
		if ev.StatusUpdate == nil {
			t.Fatal("expected StatusUpdate event from stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream event via polling")
	}
}

// TestNotifierReconnectsOnConnectionDeath verifies that when the LISTEN
// connection dies (via Stop), polling still delivers events and the
// notifier degrades gracefully.
func TestNotifierReconnectsOnConnectionDeath(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "reconnect.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	taskID := "reconnect-1"

	if err := store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "p1", AgentSlug: "agent1",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Create a notifier with a bogus URL — it won't connect, simulating
	// a dead LISTEN connection.
	log := slog.Default()
	notifier := NewNotifier("postgres://invalid:5432/nonexistent", log)

	// Create a bridge with the broken notifier.
	b := &Bridge{
		store:       store,
		activeTasks: make(map[string]activeTaskEntry),
		agentTasks:  make(map[string][]string),
		notifier:    notifier,
	}

	type result struct {
		ev  *state.TaskEvent
		err error
	}
	resultCh := make(chan result, 1)

	// Start blocking wait — notifier.Register will try to start the broken
	// connection, fail, and degrade to polling.
	go func() {
		ev, err := b.waitForTaskEvent(ctx, taskID, 5*time.Second)
		resultCh <- result{ev, err}
	}()

	// Write event after short delay.
	time.Sleep(200 * time.Millisecond)
	payload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{
			State: TaskStateWorking,
			Message: &Message{
				MessageID: "recon-msg-1",
				Role:      RoleAgent,
				Parts:     []Part{{Text: "Delivered via polling after LISTEN death"}},
			},
		},
	})
	store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "message",
		Payload: payload,
	})

	// Should still deliver via polling despite broken notifier.
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("waitForTaskEvent: %v", r.err)
		}
		if r.ev.Kind != "message" {
			t.Errorf("event kind = %q, want %q", r.ev.Kind, "message")
		}
		t.Log("event delivered via polling after LISTEN connection death — degradation works")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out — degradation to polling failed")
	}
}

// TestNotifierStopUnblocksWaiters verifies that Stop() unblocks all
// registered waiters.
func TestNotifierStopUnblocksWaiters(t *testing.T) {
	log := slog.Default()
	// Use a bogus URL — we only need Register/Stop, not a real connection.
	notifier := NewNotifier("postgres://invalid:5432/nonexistent", log)

	var wg sync.WaitGroup
	const numWaiters = 5
	unblocked := make(chan string, numWaiters)

	for i := 0; i < numWaiters; i++ {
		taskID := fmt.Sprintf("stop-test-%d", i)
		ch, _ := notifier.Register(taskID)
		wg.Add(1)
		go func(tid string) {
			defer wg.Done()
			<-ch
			unblocked <- tid
		}(taskID)
	}

	// Give goroutines a moment to block on channels.
	time.Sleep(50 * time.Millisecond)

	// Stop should unblock all waiters.
	notifier.Stop()

	wg.Wait()
	close(unblocked)

	var got []string
	for tid := range unblocked {
		got = append(got, tid)
	}
	if len(got) != numWaiters {
		t.Errorf("unblocked %d waiters, want %d", len(got), numWaiters)
	}
}

// TestNotifierFanOut verifies that multiple waiters on the same taskID
// all receive the signal.
func TestNotifierFanOut(t *testing.T) {
	log := slog.Default()
	notifier := NewNotifier("postgres://invalid:5432/nonexistent", log)

	taskID := "fanout-1"
	const numWaiters = 3
	channels := make([]<-chan struct{}, numWaiters)
	cleanups := make([]func(), numWaiters)

	for i := 0; i < numWaiters; i++ {
		channels[i], cleanups[i] = notifier.Register(taskID)
	}
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	// Simulate a notification by directly calling fanOut.
	notifier.fanOut(taskID)

	// All channels should receive the signal.
	for i, ch := range channels {
		select {
		case <-ch:
			// Good.
		case <-time.After(time.Second):
			t.Errorf("waiter %d did not receive fanOut signal", i)
		}
	}
}

// TestNotifierCleanupRemovesWaiter verifies that calling the cleanup
// function removes the waiter so it no longer receives signals.
func TestNotifierCleanupRemovesWaiter(t *testing.T) {
	log := slog.Default()
	notifier := NewNotifier("postgres://invalid:5432/nonexistent", log)

	taskID := "cleanup-1"
	ch, cleanup := notifier.Register(taskID)

	// Clean up immediately.
	cleanup()

	// fanOut should not deliver to the removed waiter.
	notifier.fanOut(taskID)

	select {
	case <-ch:
		t.Error("cleaned-up waiter should not receive signal")
	case <-time.After(100 * time.Millisecond):
		// Good — no signal received.
	}

	// Verify the task has no waiters left.
	notifier.mu.Lock()
	remaining := len(notifier.waiters[taskID])
	notifier.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected 0 waiters after cleanup, got %d", remaining)
	}
}
