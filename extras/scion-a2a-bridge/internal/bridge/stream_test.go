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
	"sync"
	"testing"
	"time"
)

func TestStreamManagerSubscribeAndBroadcast(t *testing.T) {
	sm := NewStreamManager()

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
	sm := NewStreamManager()

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
	sm := NewStreamManager()

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
	sm := NewStreamManager()

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
	sm := NewStreamManager()

	// Should not panic.
	sm.Broadcast("nonexistent-task", StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: "nonexistent-task",
			Status: TaskStatus{State: TaskStateWorking},
		},
	})
}

func TestStreamManagerConcurrentAccess(t *testing.T) {
	sm := NewStreamManager()

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
	sm := NewStreamManager()
	_ = sm // StreamManager tested above; this tests Bridge active task methods.

	b := &Bridge{
		activeTasks: make(map[string][]string),
	}

	b.registerActiveTask("task-1", "agent-a")
	b.registerActiveTask("task-2", "agent-a")
	b.registerActiveTask("task-3", "agent-b")

	tasksA := b.getActiveTaskIDs("agent-a")
	if len(tasksA) != 2 {
		t.Errorf("agent-a tasks = %d, want 2", len(tasksA))
	}

	tasksB := b.getActiveTaskIDs("agent-b")
	if len(tasksB) != 1 {
		t.Errorf("agent-b tasks = %d, want 1", len(tasksB))
	}

	b.unregisterActiveTask("task-1", "agent-a")
	tasksA = b.getActiveTaskIDs("agent-a")
	if len(tasksA) != 1 {
		t.Errorf("agent-a tasks after unregister = %d, want 1", len(tasksA))
	}

	b.unregisterActiveTask("task-2", "agent-a")
	tasksA = b.getActiveTaskIDs("agent-a")
	if len(tasksA) != 0 {
		t.Errorf("agent-a tasks after full unregister = %d, want 0", len(tasksA))
	}

	// Verify the map entry is cleaned up.
	b.tasksMu.RLock()
	_, exists := b.activeTasks["agent-a"]
	b.tasksMu.RUnlock()
	if exists {
		t.Error("expected agent-a entry to be removed from map")
	}
}
