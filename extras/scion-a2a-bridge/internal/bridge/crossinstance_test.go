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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// TestCrossInstanceCorrelation proves that events written by one "instance"
// (writer) are picked up by a polling reader on another "instance" via the
// shared durable event log, validating the HA uniform-delivery design.
func TestCrossInstanceCorrelation(t *testing.T) {
	// 1. Create a shared SQLite store (simulating shared Postgres).
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "cross-instance.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	taskID := "cross-t1"

	// 2. Create a task on "instance A".
	if err := store.CreateTask(ctx, &state.Task{
		ID:        taskID,
		ContextID: "ctx-1",
		ProjectID: "p1",
		AgentSlug: "agent1",
		State:     TaskStateSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// 3. Start a polling reader on "instance A" (simulating blocking read).
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	eventsCh := streamTaskEvents(pollCtx, store, taskID, 0, 10, nil)

	// 4. Simulate Publish on "instance B" — write event to the store directly.
	time.Sleep(50 * time.Millisecond) // ensure the reader is polling

	responsePayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{
			State: TaskStateWorking,
			Message: &Message{
				MessageID: "msg-from-instance-b",
				Role:      RoleAgent,
				Parts:     []Part{{Text: "Hello from instance B"}},
			},
		},
	})
	_, err = store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  responsePayload,
		Final:    false,
		DedupKey: "instance-b-msg-1",
	})
	if err != nil {
		t.Fatalf("AppendTaskEvent (instance B): %v", err)
	}

	// 5. Verify the polling reader on "instance A" picks up the event.
	select {
	case ev := <-eventsCh:
		if ev.StatusUpdate == nil {
			t.Fatal("expected StatusUpdate event")
		}
		if ev.StatusUpdate.Status.State != TaskStateWorking {
			t.Errorf("state = %q, want %q", ev.StatusUpdate.Status.State, TaskStateWorking)
		}
		if ev.StatusUpdate.Status.Message == nil {
			t.Fatal("expected message in status update")
		}
		if ev.StatusUpdate.Status.Message.Parts[0].Text != "Hello from instance B" {
			t.Errorf("message text = %q, want %q", ev.StatusUpdate.Status.Message.Parts[0].Text, "Hello from instance B")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-instance event delivery")
	}
}

// TestCrossInstanceSSEPath verifies that the SSE streaming path also works
// across instances: instance B writes events and instance A's SSE stream
// picks them up via cursor-based polling.
func TestCrossInstanceSSEPath(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "sse-cross.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	taskID := "sse-cross-1"

	store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "p1", AgentSlug: "agent1",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Start SSE stream on instance A with a StreamSession.
	session := NewStreamSession(taskID, 0, store)
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	ch := streamTaskEvents(pollCtx, store, taskID, session.Cursor(), 10, nil)

	// Instance B writes multiple events.
	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateWorking},
		})
		store.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:  taskID,
			Kind:    "status",
			Payload: payload,
			Final:   false,
		})
	}

	// Write final event.
	finalPayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{State: TaskStateCompleted},
	})
	store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "status",
		Payload: finalPayload,
		Final:   true,
	})

	// Read all events from the channel — it should close after the final event.
	var events []StreamEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			events = append(events, ev)
		}
	}()

	select {
	case <-done:
		// Good — channel closed after final event.
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SSE stream to complete")
	}

	if len(events) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(events))
	}

	// Last event should be the completion.
	last := events[len(events)-1]
	if last.StatusUpdate == nil {
		t.Fatal("expected final StatusUpdate")
	}
	if last.StatusUpdate.Status.State != TaskStateCompleted {
		t.Errorf("final state = %q, want %q", last.StatusUpdate.Status.State, TaskStateCompleted)
	}
	if !last.StatusUpdate.Final {
		t.Error("expected Final = true on last event")
	}
}

// TestCrossInstanceDedup verifies that the dedup_key unique constraint prevents
// duplicate events from being inserted by multiple instances.
func TestCrossInstanceDedup(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "dedup.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	taskID := "dedup-1"

	payload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{State: TaskStateWorking},
	})

	// Write the same event twice with the same dedup key.
	_, err1 := store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  payload,
		DedupKey: "same-key",
	})
	_, err2 := store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  payload,
		DedupKey: "same-key",
	})

	if err1 != nil {
		t.Fatalf("first append should succeed: %v", err1)
	}
	// Second append should silently succeed (ON CONFLICT DO NOTHING) or return 0 ID.
	if err2 != nil {
		t.Fatalf("second append should not error: %v", err2)
	}

	// Only one event should exist.
	events, err := store.ReadTaskEvents(ctx, taskID, 0, 10)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event after dedup, got %d", len(events))
	}
}

// TestCrossInstanceCAS verifies that concurrent state transitions via
// UpdateTaskState use CAS semantics — only one instance wins.
func TestCrossInstanceCAS(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "cas.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	taskID := "cas-1"

	store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "p1", AgentSlug: "agent1",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Simulate 5 instances trying to transition the task to completed concurrently.
	var wg sync.WaitGroup
	var mu sync.Mutex
	winnersCount := 0

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			changed, err := store.UpdateTaskState(ctx, taskID, TaskStateCompleted)
			if err != nil {
				t.Errorf("UpdateTaskState: %v", err)
				return
			}
			if changed {
				mu.Lock()
				winnersCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Exactly one instance should have won the CAS.
	if winnersCount != 1 {
		t.Errorf("CAS winners = %d, want exactly 1", winnersCount)
	}

	// Task should be in the completed state.
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateCompleted {
		t.Errorf("task state = %q, want %q", task.State, TaskStateCompleted)
	}
}

// TestCrossInstanceWaitForTaskEvent verifies the blocking waitForTaskEvent
// pattern works across instances: instance A calls waitForTaskEvent, instance B
// writes the response event.
func TestCrossInstanceWaitForTaskEvent(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "wait-cross.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now()
	taskID := "wait-cross-1"

	store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "p1", AgentSlug: "agent1",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Create a minimal bridge for waitForTaskEvent.
	b := &Bridge{
		store:       store,
		activeTasks: make(map[string]activeTaskEntry),
		agentTasks:  make(map[string][]string),
	}

	type result struct {
		ev  *state.TaskEvent
		err error
	}
	resultCh := make(chan result, 1)

	// Instance A: start blocking wait.
	go func() {
		ev, err := b.waitForTaskEvent(ctx, taskID, 5*time.Second)
		resultCh <- result{ev, err}
	}()

	// Instance B: write a response event after a short delay.
	time.Sleep(100 * time.Millisecond)
	responsePayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{
			State: TaskStateWorking,
			Message: &Message{
				MessageID: "cross-msg-1",
				Role:      RoleAgent,
				Parts:     []Part{{Text: "Response from instance B"}},
			},
		},
	})
	store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  taskID,
		Kind:    "message",
		Payload: responsePayload,
	})

	// Wait for instance A to receive it.
	select {
	case r := <-resultCh:
		if r.err != nil {
			t.Fatalf("waitForTaskEvent: %v", r.err)
		}
		if r.ev.Kind != "message" {
			t.Errorf("event kind = %q, want %q", r.ev.Kind, "message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-instance event delivery via waitForTaskEvent")
	}
}
