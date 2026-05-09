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
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestStreamManagerSubscribeAndBroadcast(t *testing.T) {
	sm := NewStreamManager(0)

	ch, cleanup, err := sm.Subscribe("task-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	if !sm.HasSubscribers("task-1") {
		t.Error("expected subscribers for task-1")
	}
	if sm.HasSubscribers("task-2") {
		t.Error("expected no subscribers for task-2")
	}

	event := StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateWorking},
		},
	}
	sm.Broadcast("task-1", event)

	select {
	case got := <-ch:
		if got.StatusUpdate == nil {
			t.Fatal("expected status update event")
		}
		if got.StatusUpdate.TaskID != "task-1" {
			t.Errorf("TaskID = %q, want %q", got.StatusUpdate.TaskID, "task-1")
		}
		if got.StatusUpdate.Status.State != TaskStateWorking {
			t.Errorf("State = %q, want %q", got.StatusUpdate.Status.State, TaskStateWorking)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestStreamManagerMultipleSubscribers(t *testing.T) {
	sm := NewStreamManager(0)

	ch1, cleanup1, err := sm.Subscribe("task-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup1()
	ch2, cleanup2, err := sm.Subscribe("task-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup2()

	event := StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "task-1",
			Status: TaskStatus{State: TaskStateCompleted},
			Final:  true,
		},
	}
	sm.Broadcast("task-1", event)

	for i, ch := range []<-chan StreamEvent{ch1, ch2} {
		select {
		case got := <-ch:
			if got.StatusUpdate == nil || got.StatusUpdate.Status.State != TaskStateCompleted {
				t.Errorf("subscriber %d: expected completed status", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out", i)
		}
	}
}

func TestStreamManagerCleanup(t *testing.T) {
	sm := NewStreamManager(0)

	_, cleanup, err := sm.Subscribe("task-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if !sm.HasSubscribers("task-1") {
		t.Fatal("expected subscribers after subscribe")
	}

	cleanup()
	if sm.HasSubscribers("task-1") {
		t.Error("expected no subscribers after cleanup")
	}
}

func TestStreamManagerCloseAll(t *testing.T) {
	sm := NewStreamManager(0)

	ch1, _, err := sm.Subscribe("task-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ch2, _, err := sm.Subscribe("task-1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	sm.CloseAll("task-1")

	// Channels should be closed.
	if _, ok := <-ch1; ok {
		t.Error("expected ch1 to be closed")
	}
	if _, ok := <-ch2; ok {
		t.Error("expected ch2 to be closed")
	}

	if sm.HasSubscribers("task-1") {
		t.Error("expected no subscribers after CloseAll")
	}
}

func TestStreamManagerBroadcastNoSubscribers(t *testing.T) {
	sm := NewStreamManager(0)

	// Should not panic.
	sm.Broadcast("nonexistent-task", StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "nonexistent-task",
			Status: TaskStatus{State: TaskStateWorking},
		},
	})
}

func TestStreamManagerConcurrentAccess(t *testing.T) {
	sm := NewStreamManager(0)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cleanup, err := sm.Subscribe("task-1")
			if err != nil {
				return
			}
			defer cleanup()

			sm.Broadcast("task-1", StreamEvent{
				StatusUpdate: &TaskStatusUpdate{
					TaskID: "task-1",
					Status: TaskStatus{State: TaskStateWorking},
				},
			})

			select {
			case <-ch:
			case <-time.After(time.Second):
			}
		}()
	}
	wg.Wait()
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
	sm := NewStreamManager(0)
	_ = sm // StreamManager tested above; this tests Bridge active task methods.

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

// TestBlockingTaskIgnoresActiveDispatch is a regression test for C3: ensures
// HandleBrokerMessage does not dispatch to dispatchToActiveTask for blocking
// tasks (which are tracked as waiters, not activeTasks).
func TestBlockingTaskIgnoresActiveDispatch(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "c3-test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	defer store.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &Config{
		Hub: HubConfig{User: "test-user"},
	}
	b := New(store, nil, nil, cfg, nil, log)

	now := time.Now()
	taskID := "blocking-task-1"
	store.CreateTask(&state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "grove1", AgentSlug: "agent-a",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	})

	// Register a blocking waiter (as SendMessage would in blocking mode).
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID:   "grove1",
	})
	defer b.removeWaiter(taskID)

	// Simulate a state-change arriving for the blocking task.
	stateChangeMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "IDLE",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}

	err = b.HandleBrokerMessage(context.Background(), "scion.grove.grove1.agent.agent-a.messages", stateChangeMsg)
	if err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	// The waiter should NOT receive a state-change (dispatchToWaiter skips TypeStateChange).
	select {
	case <-responseCh:
		t.Fatal("blocking waiter should not receive state-change messages")
	default:
	}

	// The task should NOT have been updated in the DB by dispatchToActiveTask
	// because the task is not registered in activeTasks.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q (state-change should not have been dispatched to active task path)", task.State, TaskStateWorking)
	}
}
