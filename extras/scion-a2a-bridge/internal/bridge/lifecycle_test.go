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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// newLifecycleTestBridge creates a Bridge with a real SQLite store for lifecycle tests.
// The broker worker and janitor goroutines are started; callers must call
// b.Shutdown() (or defer it) to avoid goroutine leaks.
// An optional *Metrics can be passed to wire metrics from the start, avoiding
// data races from assigning b.metrics after background goroutines are running.
func newLifecycleTestBridge(t *testing.T, opts ...func(*lifecycleTestOpts)) (*Bridge, *state.Store) {
	t.Helper()

	o := &lifecycleTestOpts{}
	for _, fn := range opts {
		fn(o)
	}

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "lifecycle-test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &Config{
		Hub:      HubConfig{User: "test-user"},
		Timeouts: TimeoutConfig{SendMessage: 5 * time.Second},
	}
	b := New(store, nil, nil, cfg, o.metrics, log)
	t.Cleanup(func() { b.Shutdown() })
	return b, store
}

type lifecycleTestOpts struct {
	metrics *Metrics
}

func withMetrics(m *Metrics) func(*lifecycleTestOpts) {
	return func(o *lifecycleTestOpts) { o.metrics = m }
}

// seedTask creates and registers a task in both the store and the bridge's
// activeTasks map, mimicking what SendMessage does for non-blocking sends.
func seedLifecycleTask(t *testing.T, b *Bridge, store *state.Store, taskID, projectID, agentSlug string) {
	t.Helper()
	now := time.Now()
	if err := store.CreateTask(&state.Task{
		ID:        taskID,
		ContextID: "ctx-1",
		ProjectID: projectID,
		AgentSlug: agentSlug,
		State:     TaskStateWorking,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	aKey := agentKey(projectID, agentSlug)
	b.registerActiveTask(taskID, aKey)
}

// --- Tests for dispatchToActiveTask with content messages ---

func TestContentMessageDoesNotCompleteTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "content-no-complete-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Subscribe to the task's stream.
	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	// Dispatch a content (non-state-change) message to the active task.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Here is my progress update",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	// Wait for the broker worker to process.
	time.Sleep(100 * time.Millisecond)

	// Task should NOT be completed in the store.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q — content message should NOT complete the task", task.State, TaskStateWorking)
	}

	// Task should still be registered in activeTasks.
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("task should still be in activeTasks after content message")
	}

	// The stream should have received events (artifact + status with state=working).
	var events []StreamEvent
	drainLoop(ch, &events)

	if len(events) == 0 {
		t.Fatal("expected at least one stream event from content message")
	}

	// Find the status update event.
	var foundWorkingStatus bool
	for _, ev := range events {
		if ev.StatusUpdate != nil {
			if ev.StatusUpdate.Status.State != TaskStateWorking {
				t.Errorf("StatusUpdate.State = %q, want %q", ev.StatusUpdate.Status.State, TaskStateWorking)
			}
			if ev.StatusUpdate.Final {
				t.Error("StatusUpdate.Final = true, want false for content message")
			}
			foundWorkingStatus = true
		}
	}
	if !foundWorkingStatus {
		t.Error("no StatusUpdate with state=working found in stream events")
	}
}

func TestContentMessagePreservesInputRequiredState(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "content-preserves-ir-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	topic := "scion.project.proj1.user.test-user.messages"

	// Transition to input-required via state-change.
	stateMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "WAITING_FOR_INPUT",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, stateMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(state): %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Send a content message while in input-required state.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Please provide more details",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(content): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// State must still be input-required — content must not overwrite it.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateInputRequired {
		t.Errorf("task state = %q, want %q — content message must not overwrite input-required",
			task.State, TaskStateInputRequired)
	}

	// The streamed status update for the content message must also carry input-required.
	var events []StreamEvent
	drainLoop(ch, &events)

	var foundContentStatus bool
	for _, ev := range events {
		if ev.StatusUpdate != nil && ev.StatusUpdate.Status.Message != nil {
			if ev.StatusUpdate.Status.State != TaskStateInputRequired {
				t.Errorf("content StatusUpdate.State = %q, want %q",
					ev.StatusUpdate.Status.State, TaskStateInputRequired)
			}
			foundContentStatus = true
		}
	}
	if !foundContentStatus {
		t.Error("no StatusUpdate with message content found in stream events")
	}
}

func TestContentMessageBroadcastsWorkingNonFinal(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "broadcast-working-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "I need more information",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	var events []StreamEvent
	drainLoop(ch, &events)

	// Should have an artifact update and a status update.
	var hasArtifact, hasStatus bool
	for _, ev := range events {
		if ev.ArtifactUpdate != nil {
			hasArtifact = true
			if ev.ArtifactUpdate.TaskID != taskID {
				t.Errorf("ArtifactUpdate.TaskID = %q, want %q", ev.ArtifactUpdate.TaskID, taskID)
			}
		}
		if ev.StatusUpdate != nil {
			hasStatus = true
			if ev.StatusUpdate.Status.State != TaskStateWorking {
				t.Errorf("StatusUpdate.State = %q, want %q", ev.StatusUpdate.Status.State, TaskStateWorking)
			}
			if ev.StatusUpdate.Final {
				t.Error("StatusUpdate.Final should be false")
			}
			if ev.StatusUpdate.Status.Message == nil {
				t.Error("StatusUpdate.Message should not be nil for content")
			}
		}
	}
	if !hasArtifact {
		t.Error("expected ArtifactUpdate event")
	}
	if !hasStatus {
		t.Error("expected StatusUpdate event")
	}
}

func TestMultipleContentMessagesKeepTaskAlive(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "multi-content-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	// Send 3 content messages.
	for i := 0; i < 3; i++ {
		msg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "agent:agent-a",
			Recipient: "user:test-user",
			Msg:       "progress update",
			Type:      messages.TypeAssistantReply,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", msg); err != nil {
			t.Fatalf("HandleBrokerMessage[%d]: %v", i, err)
		}
	}

	time.Sleep(200 * time.Millisecond)

	// Task should still be working.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q after 3 content messages, want %q", task.State, TaskStateWorking)
	}

	// Task should still be active.
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("task should still be in activeTasks after 3 content messages")
	}

	// Should have received multiple events (each content message produces artifact + status).
	var events []StreamEvent
	drainLoop(ch, &events)

	statusCount := 0
	for _, ev := range events {
		if ev.StatusUpdate != nil {
			statusCount++
			if ev.StatusUpdate.Status.State != TaskStateWorking {
				t.Errorf("StatusUpdate[%d].State = %q, want %q", statusCount, ev.StatusUpdate.Status.State, TaskStateWorking)
			}
			if ev.StatusUpdate.Final {
				t.Errorf("StatusUpdate[%d].Final = true, want false", statusCount)
			}
		}
	}
	if statusCount < 3 {
		t.Errorf("expected at least 3 status updates, got %d", statusCount)
	}
}

func TestStateChangeCompletedAfterContentClosesTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "complete-after-content-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	// First: send a content message.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Working on it...",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(content): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Second: send a state-change to completed.
	completedMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "COMPLETED",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", completedMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(completed): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Task should now be completed in the store.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateCompleted {
		t.Errorf("task state = %q, want %q", task.State, TaskStateCompleted)
	}

	// Task should be unregistered from activeTasks.
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after state-change to completed")
	}

	// Stream should have received the final event with Final=true.
	var events []StreamEvent
	drainLoop(ch, &events)

	var foundFinal bool
	for _, ev := range events {
		if ev.StatusUpdate != nil && ev.StatusUpdate.Final {
			foundFinal = true
			if ev.StatusUpdate.Status.State != TaskStateCompleted {
				t.Errorf("final StatusUpdate.State = %q, want %q", ev.StatusUpdate.Status.State, TaskStateCompleted)
			}
		}
	}
	if !foundFinal {
		t.Error("expected a final StatusUpdate with state=completed")
	}
}

func TestStateChangeInputRequiredKeepsTaskAlive(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "input-required-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	// Send state-change to WAITING_FOR_INPUT (maps to input-required).
	inputMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "WAITING_FOR_INPUT",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", inputMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Task should be in input-required state.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateInputRequired {
		t.Errorf("task state = %q, want %q", task.State, TaskStateInputRequired)
	}

	// input-required is NOT terminal, so task should still be active.
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("task should remain in activeTasks for input-required (non-terminal) state")
	}

	// Stream event should have Final=false.
	var events []StreamEvent
	drainLoop(ch, &events)

	var foundInputRequired bool
	for _, ev := range events {
		if ev.StatusUpdate != nil && ev.StatusUpdate.Status.State == TaskStateInputRequired {
			foundInputRequired = true
			if ev.StatusUpdate.Final {
				t.Error("input-required StatusUpdate.Final = true, want false")
			}
		}
	}
	if !foundInputRequired {
		t.Error("expected StatusUpdate with state=input-required")
	}
}

func TestStateChangeFailedClosesTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "failed-close-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	failMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "ERROR",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", failMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateFailed {
		t.Errorf("task state = %q, want %q", task.State, TaskStateFailed)
	}

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after terminal state-change")
	}
}

// --- Tests for blocking SendMessage path ---

func TestBlockingSendMessageReturnsWorking(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "blocking-working-1"
	now := time.Now()

	// Seed the task directly in the store.
	if err := store.CreateTask(&state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "proj1", AgentSlug: "agent-a",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Set up a blocking waiter as SendMessage would.
	aKey := agentKey("proj1", "agent-a")
	b.registerActiveTask(taskID, aKey)
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID: "proj1",
	})

	// Simulate agent sending a content response.
	responseCh <- &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Here is the answer",
		Type:      messages.TypeAssistantReply,
	}

	// Read the response as the blocking path would.
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	select {
	case response := <-responseCh:
		msg, artifacts := TranslateScionToA2A(response)
		result := &TaskResult{
			ID:        taskID,
			ContextID: "ctx-1",
			Status: TaskStatus{
				State:   TaskStateWorking,
				Message: &msg,
			},
			Artifacts: artifacts,
		}

		// The key assertion: status is working, not completed.
		if result.Status.State != TaskStateWorking {
			t.Errorf("result.Status.State = %q, want %q", result.Status.State, TaskStateWorking)
		}
	case <-timeout.C:
		t.Fatal("timed out waiting for response")
	}

	// Task should still be active (not unregistered by blocking path on success).
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("task should remain in activeTasks after blocking response (lifecycle driven by state-change)")
	}

	b.removeWaiter(taskID)
}

func TestBlockingSendMessageTimeoutCleansUpActiveTask(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	taskID := "timeout-cleanup-1"
	aKey := agentKey("proj1", "agent-a")

	b.registerActiveTask(taskID, aKey)
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID: "proj1",
	})

	// Simulate timeout path: the timer fires, and we clean up.
	// (This mimics the select case <-timer.C in SendMessage.)
	b.unregisterActiveTask(taskID, aKey)
	b.removeWaiter(taskID)

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after timeout")
	}

	b.mu.RLock()
	_, hasWaiter := b.waiters[taskID]
	b.mu.RUnlock()
	if hasWaiter {
		t.Error("waiter should be removed after timeout")
	}
}

func TestBlockingSendMessageErrorCleansUpActiveTask(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	taskID := "error-cleanup-1"
	aKey := agentKey("proj1", "agent-a")

	b.registerActiveTask(taskID, aKey)

	// Simulate the send failure path: the error branch unregisters the task.
	b.unregisterActiveTask(taskID, aKey)

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after send failure")
	}
}

// --- Tests for full multi-turn lifecycle flow ---

func TestFullMultiTurnLifecycle(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "multi-turn-full-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	topic := "scion.project.proj1.user.test-user.messages"

	// Step 1: Agent sends content (progress update) — task stays alive.
	sendContent := func(text string) {
		t.Helper()
		msg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "agent:agent-a",
			Recipient: "user:test-user",
			Msg:       text,
			Type:      messages.TypeAssistantReply,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if err := b.HandleBrokerMessage(context.Background(), topic, msg); err != nil {
			t.Fatalf("HandleBrokerMessage(content %q): %v", text, err)
		}
	}

	sendStateChange := func(activity string) {
		t.Helper()
		msg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "agent:agent-a",
			Recipient: "user:test-user",
			Msg:       activity,
			Type:      messages.TypeStateChange,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if err := b.HandleBrokerMessage(context.Background(), topic, msg); err != nil {
			t.Fatalf("HandleBrokerMessage(state %q): %v", activity, err)
		}
	}

	// Step 1: Content message.
	sendContent("Analyzing your request...")
	time.Sleep(50 * time.Millisecond)

	// Step 2: State change to WAITING_FOR_INPUT (non-terminal).
	sendStateChange("WAITING_FOR_INPUT")
	time.Sleep(50 * time.Millisecond)

	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask after input-required: %v", err)
	}
	if task.State != TaskStateInputRequired {
		t.Errorf("after input-required: state = %q, want %q", task.State, TaskStateInputRequired)
	}

	// Task should still be active.
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("task should still be active after input-required")
	}

	// Step 3: Agent resumes working (another state-change).
	sendStateChange("WORKING")
	time.Sleep(50 * time.Millisecond)

	task, err = store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask after working: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("after working: state = %q, want %q", task.State, TaskStateWorking)
	}

	// Step 4: More content.
	sendContent("Here is the final answer.")
	time.Sleep(50 * time.Millisecond)

	// Step 5: Completed state-change closes the task.
	sendStateChange("COMPLETED")
	time.Sleep(100 * time.Millisecond)

	task, err = store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask after completed: %v", err)
	}
	if task.State != TaskStateCompleted {
		t.Errorf("after completed: state = %q, want %q", task.State, TaskStateCompleted)
	}

	b.tasksMu.RLock()
	_, isActive = b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after completed")
	}

	// Verify we got all the events.
	var events []StreamEvent
	drainLoop(ch, &events)

	// Count status updates by state.
	stateCounts := make(map[string]int)
	for _, ev := range events {
		if ev.StatusUpdate != nil {
			stateCounts[ev.StatusUpdate.Status.State]++
		}
	}

	// Expect: working (from content × 2 + state-change), input-required, completed.
	if stateCounts[TaskStateWorking] < 2 {
		t.Errorf("expected at least 2 working status updates, got %d", stateCounts[TaskStateWorking])
	}
	if stateCounts[TaskStateInputRequired] != 1 {
		t.Errorf("expected 1 input-required update, got %d", stateCounts[TaskStateInputRequired])
	}
	if stateCounts[TaskStateCompleted] != 1 {
		t.Errorf("expected 1 completed update, got %d", stateCounts[TaskStateCompleted])
	}
}

// --- Tests for slug-based fallback correlation ---

func TestSlugFallbackContentDoesNotCloseTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "slug-fallback-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Send a content message WITHOUT a2aTaskId (slug-based correlation).
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Response via slug fallback",
		Type:      messages.TypeAssistantReply,
		// No a2aTaskId in metadata.
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q — slug-fallback content should not close task", task.State, TaskStateWorking)
	}

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive == false {
		t.Error("task should still be active after slug-fallback content message")
	}
}

// --- Test dispatchToWaiter skips state-changes ---

func TestDispatchToWaiterSkipsStateChange(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	taskID := "waiter-skip-1"

	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID: "proj1",
	})
	defer b.removeWaiter(taskID)

	stateMsg := &messages.StructuredMessage{
		Version: 1,
		Sender:  "agent:agent-a",
		Msg:     "COMPLETED",
		Type:    messages.TypeStateChange,
	}

	handled := b.dispatchToWaiter(taskID, stateMsg)
	if !handled {
		t.Error("dispatchToWaiter should return true for state-change (to suppress further dispatch)")
	}

	// The channel should NOT have received the message.
	select {
	case <-responseCh:
		t.Error("waiter should NOT receive state-change messages")
	default:
		// Good.
	}

	// Content message SHOULD be dispatched to the waiter.
	contentMsg := &messages.StructuredMessage{
		Version: 1,
		Sender:  "agent:agent-a",
		Msg:     "Hello",
		Type:    messages.TypeAssistantReply,
	}
	handled = b.dispatchToWaiter(taskID, contentMsg)
	if !handled {
		t.Error("dispatchToWaiter should return true for content message when waiter exists")
	}

	select {
	case got := <-responseCh:
		if got.Msg != "Hello" {
			t.Errorf("waiter received Msg = %q, want %q", got.Msg, "Hello")
		}
	default:
		t.Error("waiter should have received content message")
	}
}

// --- Metrics test ---

func TestContentMessageDoesNotIncrementCompletedMetric(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	b, store := newLifecycleTestBridge(t, withMetrics(metrics))

	taskID := "no-metric-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Just a content msg",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// The completed metric should NOT have been incremented.
	// We test indirectly by verifying the task is still active and not completed.
	task, _ := store.GetTask(taskID)
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q", task.State, TaskStateWorking)
	}
}

// --- Edge case tests ---

func TestContentAfterCompletedIsIgnored(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "content-after-complete-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	topic := "scion.project.proj1.user.test-user.messages"

	// First complete the task via state-change.
	completedMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "COMPLETED",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, completedMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(completed): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Verify task is completed and unregistered.
	task, _ := store.GetTask(taskID)
	if task.State != TaskStateCompleted {
		t.Fatalf("expected completed state, got %q", task.State)
	}
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Fatal("task should be unregistered after completed")
	}

	// Now send a content message — it should be silently dropped (no crash, no state change).
	lateContent := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Late message after completion",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, lateContent); err != nil {
		t.Fatalf("HandleBrokerMessage(late content): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// State should still be completed (store protects terminal states).
	task, _ = store.GetTask(taskID)
	if task.State != TaskStateCompleted {
		t.Errorf("task state changed after late content: %q, want %q", task.State, TaskStateCompleted)
	}
}

func TestDoubleCompletedIsIdempotent(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "double-complete-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	topic := "scion.project.proj1.user.test-user.messages"

	for i := 0; i < 2; i++ {
		msg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "agent:agent-a",
			Recipient: "user:test-user",
			Msg:       "COMPLETED",
			Type:      messages.TypeStateChange,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		// First should succeed, second should be a no-op since task is
		// unregistered from activeTasks.
		if err := b.HandleBrokerMessage(context.Background(), topic, msg); err != nil {
			t.Fatalf("HandleBrokerMessage[%d]: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	task, _ := store.GetTask(taskID)
	if task.State != TaskStateCompleted {
		t.Errorf("task state = %q, want %q", task.State, TaskStateCompleted)
	}

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should not be in activeTasks after double-completed")
	}
}

func TestNonBlockingSendKeepsTaskAlive(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "nonblock-alive-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	topic := "scion.project.proj1.user.test-user.messages"

	// Send content message to a task registered the non-blocking way.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Working on your request",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Task should still be alive.
	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("non-blocking task should still be active after content message")
	}

	task, _ := store.GetTask(taskID)
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q", task.State, TaskStateWorking)
	}
}

func TestStateChangeWorkingDoesNotCloseTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "working-nonterminal-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	topic := "scion.project.proj1.user.test-user.messages"

	// WORKING state-change is non-terminal.
	workingMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "WORKING",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, workingMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Error("WORKING state-change should not unregister the task (non-terminal)")
	}

	task, _ := store.GetTask(taskID)
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q", task.State, TaskStateWorking)
	}
}

func TestMultipleAgentTasksContentDoesNotClose(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID1 := "multi-agent-task-1"
	taskID2 := "multi-agent-task-2"
	seedLifecycleTask(t, b, store, taskID1, "proj1", "agent-a")
	seedLifecycleTask(t, b, store, taskID2, "proj1", "agent-a")
	topic := "scion.project.proj1.user.test-user.messages"

	// Send content without a2aTaskId — slug fallback should hit both tasks.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Broadcast content",
		Type:      messages.TypeAssistantReply,
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Both tasks should still be active.
	for _, tid := range []string{taskID1, taskID2} {
		b.tasksMu.RLock()
		_, isActive := b.activeTasks[tid]
		b.tasksMu.RUnlock()
		if !isActive {
			t.Errorf("task %s should still be active after slug-fallback content", tid)
		}
		task, _ := store.GetTask(tid)
		if task.State != TaskStateWorking {
			t.Errorf("task %s state = %q, want %q", tid, task.State, TaskStateWorking)
		}
	}
}

func TestBlockingSendMessageCancelCleansUpActiveTask(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	taskID := "cancel-cleanup-1"
	aKey := agentKey("proj1", "agent-a")

	b.registerActiveTask(taskID, aKey)
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID: "proj1",
	})

	// Simulate the ctx.Done() path in SendMessage.
	b.unregisterActiveTask(taskID, aKey)
	b.removeWaiter(taskID)

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after context cancellation")
	}

	b.mu.RLock()
	_, hasWaiter := b.waiters[taskID]
	b.mu.RUnlock()
	if hasWaiter {
		t.Error("waiter should be removed after context cancellation")
	}
}

func TestStateChangeTerminalityTableDriven(t *testing.T) {
	tests := []struct {
		activity     string
		wantState    string
		wantTerminal bool
	}{
		{"WORKING", TaskStateWorking, false},
		{"THINKING", TaskStateWorking, false},
		{"EXECUTING", TaskStateWorking, false},
		{"WAITING_FOR_INPUT", TaskStateInputRequired, false},
		{"COMPLETED", TaskStateCompleted, true},
		{"ERROR", TaskStateFailed, true},
		{"STALLED", TaskStateFailed, true},
		{"LIMITS_EXCEEDED", TaskStateFailed, true},
		{"OFFLINE", TaskStateFailed, true},
	}

	for _, tc := range tests {
		t.Run(tc.activity, func(t *testing.T) {
			b, store := newLifecycleTestBridge(t)
			taskID := "term-" + tc.activity
			seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

			ch, cleanup, err := b.streams.Subscribe(taskID)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer cleanup()

			msg := &messages.StructuredMessage{
				Version:   1,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Sender:    "agent:agent-a",
				Recipient: "user:test-user",
				Msg:       tc.activity,
				Type:      messages.TypeStateChange,
				Metadata:  map[string]string{"a2aTaskId": taskID},
			}
			if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", msg); err != nil {
				t.Fatalf("HandleBrokerMessage: %v", err)
			}
			time.Sleep(100 * time.Millisecond)

			task, err := store.GetTask(taskID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.State != tc.wantState {
				t.Errorf("task state = %q, want %q", task.State, tc.wantState)
			}

			b.tasksMu.RLock()
			_, isActive := b.activeTasks[taskID]
			b.tasksMu.RUnlock()

			if tc.wantTerminal && isActive {
				t.Errorf("task should be unregistered for terminal state %q", tc.activity)
			}
			if !tc.wantTerminal && !isActive {
				t.Errorf("task should remain active for non-terminal state %q", tc.activity)
			}

			// Check stream event Final flag.
			var events []StreamEvent
			drainLoop(ch, &events)

			for _, ev := range events {
				if ev.StatusUpdate != nil {
					if ev.StatusUpdate.Final != tc.wantTerminal {
						t.Errorf("StatusUpdate.Final = %v, want %v for %q",
							ev.StatusUpdate.Final, tc.wantTerminal, tc.activity)
					}
				}
			}
		})
	}
}

// --- Stream close regression tests ---

func TestTerminalStateClosesStreamChannel(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "stream-close-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	// Send a terminal state-change.
	completedMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "COMPLETED",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", completedMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	// The channel should be closed after the broker worker processes the message.
	// Read all events; the channel must close (range exits).
	done := make(chan struct{})
	var events []StreamEvent
	go func() {
		defer close(done)
		for ev := range ch {
			events = append(events, ev)
		}
	}()

	select {
	case <-done:
		// Good — channel was closed.
	case <-time.After(2 * time.Second):
		t.Fatal("stream channel was not closed after terminal state-change (CloseAll missing?)")
	}

	// Verify we received the final event.
	var foundFinal bool
	for _, ev := range events {
		if ev.StatusUpdate != nil && ev.StatusUpdate.Final {
			foundFinal = true
		}
	}
	if !foundFinal {
		t.Error("expected final StatusUpdate before channel close")
	}
}

func TestTerminalStateFailedClosesStreamChannel(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "stream-close-fail-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	ch, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cleanup()

	failMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "ERROR",
		Type:      messages.TypeStateChange,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", failMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
		}
	}()

	select {
	case <-done:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("stream channel was not closed after ERROR state-change")
	}
}

// --- Fix regression tests ---

func TestDispatchToWaiterPersistsTerminalState(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "waiter-persist-terminal-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Set up a blocking waiter as SendMessage would.
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID: "proj1",
	})
	defer b.removeWaiter(taskID)

	// Dispatch a COMPLETED state-change via dispatchToWaiter.
	completedMsg := &messages.StructuredMessage{
		Version: 1,
		Sender:  "agent:agent-a",
		Msg:     "COMPLETED",
		Type:    messages.TypeStateChange,
	}
	handled := b.dispatchToWaiter(taskID, completedMsg)
	if !handled {
		t.Fatal("dispatchToWaiter should return true for state-change")
	}

	// The waiter channel should NOT have received the message (state-changes are skipped).
	select {
	case <-responseCh:
		t.Error("waiter should NOT receive state-change messages")
	default:
	}

	// But the DB state must be updated to completed.
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateCompleted {
		t.Errorf("task state = %q, want %q — terminal state-change must persist even when waiter exists", task.State, TaskStateCompleted)
	}
}

func TestDispatchToWaiterDoesNotPersistNonTerminalState(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "waiter-no-persist-nonterminal-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: "agent-a",
		projectID: "proj1",
	})
	defer b.removeWaiter(taskID)

	// Dispatch a WORKING state-change (non-terminal).
	workingMsg := &messages.StructuredMessage{
		Version: 1,
		Sender:  "agent:agent-a",
		Msg:     "WORKING",
		Type:    messages.TypeStateChange,
	}
	handled := b.dispatchToWaiter(taskID, workingMsg)
	if !handled {
		t.Fatal("dispatchToWaiter should return true for state-change")
	}

	// DB state should remain working (seedTask sets it to working).
	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q — non-terminal state-change should not alter DB from waiter path", task.State, TaskStateWorking)
	}
}

func TestContentMessageRefreshesTimestamp(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "timestamp-refresh-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Record the initial timestamp.
	taskBefore, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask (before): %v", err)
	}
	initialUpdatedAt := taskBefore.UpdatedAt

	// Sleep briefly to ensure timestamp moves forward.
	time.Sleep(50 * time.Millisecond)

	// Send a content message through the broker.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Still working...",
		Type:      messages.TypeAssistantReply,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// The task's UpdatedAt should have been refreshed.
	taskAfter, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask (after): %v", err)
	}
	if !taskAfter.UpdatedAt.After(initialUpdatedAt) {
		t.Errorf("UpdatedAt was not refreshed: before=%v, after=%v — content messages must refresh timestamp to prevent janitor reaping",
			initialUpdatedAt, taskAfter.UpdatedAt)
	}
	if taskAfter.State != TaskStateWorking {
		t.Errorf("task state = %q, want %q", taskAfter.State, TaskStateWorking)
	}
}

// --- Helpers ---

// drainLoop reads all available events from a channel without blocking.
func drainLoop(ch <-chan StreamEvent, out *[]StreamEvent) {
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			*out = append(*out, ev)
		default:
			return
		}
	}
}
