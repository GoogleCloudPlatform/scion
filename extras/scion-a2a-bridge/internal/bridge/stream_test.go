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
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

func TestStreamManagerAcquireAndHasSubscribers(t *testing.T) {
	sm := NewStreamManager(0)

	cleanup, err := sm.AcquireSession("task-1")
	if err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	defer cleanup()

	if !sm.HasSubscribers("task-1") {
		t.Error("expected subscribers for task-1")
	}
	if sm.HasSubscribers("task-2") {
		t.Error("expected no subscribers for task-2")
	}
}

func TestStreamManagerMultipleSessions(t *testing.T) {
	sm := NewStreamManager(0)

	cleanup1, err := sm.AcquireSession("task-1")
	if err != nil {
		t.Fatalf("AcquireSession 1: %v", err)
	}
	cleanup2, err := sm.AcquireSession("task-1")
	if err != nil {
		t.Fatalf("AcquireSession 2: %v", err)
	}

	if !sm.HasSubscribers("task-1") {
		t.Error("expected subscribers for task-1")
	}

	// Releasing one session should keep the subscriber count > 0.
	cleanup1()
	if !sm.HasSubscribers("task-1") {
		t.Error("expected subscribers after releasing one of two")
	}

	// Releasing the second should drop to zero.
	cleanup2()
	if sm.HasSubscribers("task-1") {
		t.Error("expected no subscribers after releasing all")
	}
}

func TestStreamManagerCleanup(t *testing.T) {
	sm := NewStreamManager(0)

	cleanup, err := sm.AcquireSession("task-1")
	if err != nil {
		t.Fatalf("AcquireSession: %v", err)
	}
	if !sm.HasSubscribers("task-1") {
		t.Fatal("expected subscribers after acquire")
	}

	cleanup()
	if sm.HasSubscribers("task-1") {
		t.Error("expected no subscribers after cleanup")
	}
}

func TestStreamManagerMaxSubscribers(t *testing.T) {
	sm := NewStreamManager(2) // max 2

	c1, err := sm.AcquireSession("task-1")
	if err != nil {
		t.Fatalf("AcquireSession 1: %v", err)
	}
	c2, err := sm.AcquireSession("task-2")
	if err != nil {
		t.Fatalf("AcquireSession 2: %v", err)
	}

	// Third should fail.
	_, err = sm.AcquireSession("task-3")
	if err != ErrTooManySubscribers {
		t.Errorf("expected ErrTooManySubscribers, got %v", err)
	}

	// Release one, then acquire should succeed.
	c1()
	c3, err := sm.AcquireSession("task-3")
	if err != nil {
		t.Fatalf("AcquireSession 3 after release: %v", err)
	}
	c2()
	c3()
}

func TestStreamManagerConcurrentAccess(t *testing.T) {
	sm := NewStreamManager(0)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cleanup, err := sm.AcquireSession("task-1")
			if err != nil {
				return
			}
			defer cleanup()
			// Hold the session briefly.
			time.Sleep(5 * time.Millisecond)
		}()
	}
	wg.Wait()

	if sm.HasSubscribers("task-1") {
		t.Error("expected no subscribers after all goroutines completed")
	}
}

func TestStreamEventTypes(t *testing.T) {
	t.Run("task event", func(t *testing.T) {
		event := StreamEvent{
			Task: &TaskResult{
				ID:     "task-1",
				Status: TaskStatus{State: TaskStateSubmitted},
			},
		}
		if event.Task == nil {
			t.Fatal("expected task field")
		}
		if event.StatusUpdate != nil || event.ArtifactUpdate != nil {
			t.Error("expected only task field set")
		}
	})

	t.Run("status update event", func(t *testing.T) {
		event := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: "task-1",
				Status: TaskStatus{State: TaskStateCompleted},
				Final:  true,
			},
		}
		if event.StatusUpdate == nil {
			t.Fatal("expected status update field")
		}
		if !event.StatusUpdate.Final {
			t.Error("expected Final = true")
		}
	})

	t.Run("artifact update event", func(t *testing.T) {
		event := StreamEvent{
			ArtifactUpdate: &TaskArtifactUpdate{
				TaskID: "task-1",
				Artifact: Artifact{
					ArtifactID: "art-1",
					Parts:      []Part{{Text: "hello"}},
					LastChunk:  true,
				},
			},
		}
		if event.ArtifactUpdate == nil {
			t.Fatal("expected artifact update field")
		}
		if event.ArtifactUpdate.Artifact.ArtifactID != "art-1" {
			t.Errorf("ArtifactID = %q, want %q", event.ArtifactUpdate.Artifact.ArtifactID, "art-1")
		}
	})
}

func TestActiveTaskTracking(t *testing.T) {
	b := &Bridge{
		activeTasks: make(map[string]activeTaskEntry),
		agentTasks:  make(map[string][]string),
	}

	b.registerActiveTask("task-1", "grove1:agent-a")
	b.registerActiveTask("task-2", "grove1:agent-a")
	b.registerActiveTask("task-3", "grove1:agent-b")

	// Check activeTasks maps taskID to agentKey.
	b.tasksMu.RLock()
	if b.activeTasks["task-1"].aKey != "grove1:agent-a" {
		t.Errorf("task-1 agent key = %q, want %q", b.activeTasks["task-1"].aKey, "grove1:agent-a")
	}
	agentATaskCount := len(b.agentTasks["grove1:agent-a"])
	agentBTaskCount := len(b.agentTasks["grove1:agent-b"])
	b.tasksMu.RUnlock()

	if agentATaskCount != 2 {
		t.Errorf("agent-a tasks = %d, want 2", agentATaskCount)
	}
	if agentBTaskCount != 1 {
		t.Errorf("agent-b tasks = %d, want 1", agentBTaskCount)
	}

	b.unregisterActiveTask("task-1", "grove1:agent-a")
	b.tasksMu.RLock()
	agentATaskCount = len(b.agentTasks["grove1:agent-a"])
	b.tasksMu.RUnlock()
	if agentATaskCount != 1 {
		t.Errorf("agent-a tasks after unregister = %d, want 1", agentATaskCount)
	}

	b.unregisterActiveTask("task-2", "grove1:agent-a")
	b.tasksMu.RLock()
	_, exists := b.agentTasks["grove1:agent-a"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected agent-a entry to be removed from agentTasks map")
	}
}

func TestBackoffInterval(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  time.Duration
	}{
		{100 * time.Millisecond, 200 * time.Millisecond},
		{250 * time.Millisecond, 500 * time.Millisecond},
		{500 * time.Millisecond, 1 * time.Second},
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 2 * time.Second}, // cap
		{3 * time.Second, 2 * time.Second}, // above cap
	}
	for _, tt := range tests {
		got := backoffInterval(tt.input)
		if got != tt.want {
			t.Errorf("backoffInterval(%v) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestStreamSession_ReadNext(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Write some events.
	for i := 0; i < 3; i++ {
		payload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateWorking},
		})
		_, err := store.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:  "task-1",
			Kind:    "status",
			Payload: payload,
			Final:   i == 2,
		})
		if err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	session := NewStreamSession("task-1", 0, store)

	// Read first batch.
	events, err := session.ReadNext(ctx, 2)
	if err != nil {
		t.Fatalf("ReadNext: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Cursor should advance.
	if session.Cursor() != events[1].ID {
		t.Errorf("cursor = %d, want %d", session.Cursor(), events[1].ID)
	}

	// Read remaining events.
	events, err = session.ReadNext(ctx, 10)
	if err != nil {
		t.Fatalf("ReadNext: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if !events[0].Final {
		t.Error("expected final event")
	}

	// No more events.
	events, err = session.ReadNext(ctx, 10)
	if err != nil {
		t.Fatalf("ReadNext: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestStreamTaskEvents(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Write a status event and a final message event.
	statusPayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: "task-1",
		Status: TaskStatus{State: TaskStateWorking},
	})
	store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  "task-1",
		Kind:    "status",
		Payload: statusPayload,
		Final:   false,
	})

	msgPayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: "task-1",
		Status: TaskStatus{
			State: TaskStateCompleted,
			Message: &Message{
				MessageID: "msg-1",
				Role:      RoleAgent,
				Parts:     []Part{{Text: "Done!"}},
			},
		},
	})
	store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:  "task-1",
		Kind:    "message",
		Payload: msgPayload,
		Final:   true,
	})

	ch := streamTaskEvents(ctx, store, "task-1", 0, 10, nil)

	// Should receive the status event.
	select {
	case ev := <-ch:
		if ev.StatusUpdate == nil {
			t.Fatal("expected status update event")
		}
		if ev.StatusUpdate.Status.State != TaskStateWorking {
			t.Errorf("state = %q, want %q", ev.StatusUpdate.Status.State, TaskStateWorking)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for status event")
	}

	// Should receive the final message event.
	select {
	case ev := <-ch:
		if ev.StatusUpdate == nil {
			t.Fatal("expected status update (message) event")
		}
		if !ev.StatusUpdate.Final {
			t.Error("expected final flag set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for final event")
	}

	// Channel should be closed after final event.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after final event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for channel close")
	}
}

func TestStreamTaskEvents_ContextCancel(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// Start streaming with no events — will poll indefinitely.
	ch := streamTaskEvents(ctx, store, "task-no-events", 0, 10, nil)

	// Cancel should cause the channel to close.
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed on cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for channel close on cancel")
	}
}

func TestTaskEventToStreamEvent(t *testing.T) {
	t.Run("status event", func(t *testing.T) {
		payload, _ := json.Marshal(TaskStatusUpdate{
			Status: TaskStatus{State: TaskStateWorking},
		})
		ev := state.TaskEvent{
			TaskID:  "task-1",
			Kind:    "status",
			Payload: payload,
			Final:   false,
		}
		se, err := taskEventToStreamEvent(ev)
		if err != nil {
			t.Fatalf("taskEventToStreamEvent: %v", err)
		}
		if se.StatusUpdate == nil {
			t.Fatal("expected status update")
		}
		if se.StatusUpdate.TaskID != "task-1" {
			t.Errorf("TaskID = %q, want %q", se.StatusUpdate.TaskID, "task-1")
		}
		if se.StatusUpdate.Status.State != TaskStateWorking {
			t.Errorf("State = %q, want %q", se.StatusUpdate.Status.State, TaskStateWorking)
		}
	})

	t.Run("artifact event", func(t *testing.T) {
		payload, _ := json.Marshal(TaskArtifactUpdate{
			Artifact: Artifact{ArtifactID: "art-1", Parts: []Part{{Text: "data"}}},
		})
		ev := state.TaskEvent{
			TaskID:  "task-1",
			Kind:    "artifact",
			Payload: payload,
			Final:   false,
		}
		se, err := taskEventToStreamEvent(ev)
		if err != nil {
			t.Fatalf("taskEventToStreamEvent: %v", err)
		}
		if se.ArtifactUpdate == nil {
			t.Fatal("expected artifact update")
		}
		if se.ArtifactUpdate.TaskID != "task-1" {
			t.Errorf("TaskID = %q, want %q", se.ArtifactUpdate.TaskID, "task-1")
		}
	})

	t.Run("message event", func(t *testing.T) {
		payload, _ := json.Marshal(TaskStatusUpdate{
			Status: TaskStatus{
				State: TaskStateCompleted,
				Message: &Message{
					MessageID: "msg-1",
					Role:      RoleAgent,
					Parts:     []Part{{Text: "result"}},
				},
			},
		})
		ev := state.TaskEvent{
			TaskID:  "task-1",
			Kind:    "message",
			Payload: payload,
			Final:   true,
		}
		se, err := taskEventToStreamEvent(ev)
		if err != nil {
			t.Fatalf("taskEventToStreamEvent: %v", err)
		}
		if se.StatusUpdate == nil {
			t.Fatal("expected status update (from message event)")
		}
		if !se.StatusUpdate.Final {
			t.Error("expected final flag")
		}
		if se.StatusUpdate.Status.Message == nil {
			t.Fatal("expected message in status update")
		}
		if se.StatusUpdate.Status.Message.Parts[0].Text != "result" {
			t.Errorf("message text = %q, want %q", se.StatusUpdate.Status.Message.Parts[0].Text, "result")
		}
	})

	t.Run("unknown event kind", func(t *testing.T) {
		ev := state.TaskEvent{
			TaskID:  "task-1",
			Kind:    "unknown",
			Payload: []byte("{}"),
		}
		_, err := taskEventToStreamEvent(ev)
		if err == nil {
			t.Fatal("expected error for unknown kind")
		}
	})
}

// TestHandleBrokerMessage_WritesToEventLog verifies that HandleBrokerMessage
// writes events to the event log (replacing the old in-memory dispatch).
func TestHandleBrokerMessage_WritesToEventLog(t *testing.T) {
	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "broker-test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &Config{
		Hub: HubConfig{User: "test-user"},
	}
	b := New(store, nil, nil, cfg, nil, log)
	defer b.Shutdown()

	ctx := context.Background()
	now := time.Now()
	taskID := "task-broker-1"
	store.CreateTask(ctx, &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "grove1", AgentSlug: "agent-a",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Register the active task so correlation works via local cache.
	b.registerActiveTask(taskID, "grove1:agent-a")

	// Simulate a message arriving from the broker.
	msg := &state.TaskEvent{
		TaskID: taskID,
		Kind:   "message",
		Payload: mustJSON(t, TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{
				State: TaskStateWorking,
				Message: &Message{
					MessageID: "resp-1",
					Role:      RoleAgent,
					Parts:     []Part{{Text: "response text"}},
				},
			},
		}),
		Final: false,
	}
	_, err = store.AppendTaskEvent(ctx, msg)
	if err != nil {
		t.Fatalf("AppendTaskEvent: %v", err)
	}

	// Verify the event is readable from the log.
	events, err := store.ReadTaskEvents(ctx, taskID, 0, 10)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != "message" {
		t.Errorf("event kind = %q, want %q", events[0].Kind, "message")
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}
