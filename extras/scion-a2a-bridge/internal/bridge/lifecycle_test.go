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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// newLifecycleTestBridge creates a Bridge with a real SQLite store for lifecycle tests.
// The janitor goroutine is started; callers must call b.Shutdown() (or defer it)
// to avoid goroutine leaks.
// An optional *Metrics can be passed to wire metrics from the start, avoiding
// data races from assigning b.metrics after background goroutines are running.
func newLifecycleTestBridge(t *testing.T, opts ...func(*lifecycleTestOpts)) (*Bridge, state.Store) {
	t.Helper()

	o := &lifecycleTestOpts{}
	for _, fn := range opts {
		fn(o)
	}

	dir := t.TempDir()
	store, err := state.NewSQLite(filepath.Join(dir, "lifecycle-test.db"))
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

// seedLifecycleTask creates and registers a task in both the store and the bridge's
// activeTasks map, mimicking what SendMessage does for non-blocking sends.
func seedLifecycleTask(t *testing.T, b *Bridge, store state.Store, taskID, projectID, agentSlug string) {
	t.Helper()
	now := time.Now()
	if err := store.CreateTask(context.Background(), &state.Task{
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

// readStreamEvents reads events from the store's event log and converts them
// to StreamEvents, replacing the old Subscribe+drainLoop pattern.
func readStreamEvents(t *testing.T, store state.Store, taskID string) []StreamEvent {
	t.Helper()
	rawEvents, err := store.ReadTaskEvents(context.Background(), taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	var events []StreamEvent
	for _, raw := range rawEvents {
		se, err := taskEventToStreamEvent(raw)
		if err != nil {
			t.Logf("skipping event %d: %v", raw.ID, err)
			continue
		}
		events = append(events, se)
	}
	return events
}

// --- Tests for HandleBrokerMessage with content messages ---

func TestContentMessageDoesNotCompleteTask(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "content-no-complete-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Dispatch a content (non-state-change) message to the active task.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Here is my progress update",
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Task should NOT be completed in the store.
	task, err := store.GetTask(context.Background(), taskID)
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

	// Verify events were written to the event log.
	events := readStreamEvents(t, store, taskID)
	if len(events) == 0 {
		t.Fatal("expected at least one event in the event log from content message")
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
		t.Error("no StatusUpdate with state=working found in event log")
	}
}

func TestContentMessagePreservesInputRequiredState(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "content-preserves-ir-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

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
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(content): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// State must still be input-required — content must not overwrite it.
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != TaskStateInputRequired {
		t.Errorf("task state = %q, want %q — content message must not overwrite input-required",
			task.State, TaskStateInputRequired)
	}

	// Verify event log has events with content.
	events := readStreamEvents(t, store, taskID)
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
		t.Error("no StatusUpdate with message content found in event log")
	}
}

func TestContentMessageBroadcastsWorkingNonFinal(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "broadcast-working-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "I need more information",
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	events := readStreamEvents(t, store, taskID)

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
		t.Error("expected ArtifactUpdate event in event log")
	}
	if !hasStatus {
		t.Error("expected StatusUpdate event in event log")
	}
}

func TestMultipleContentMessagesKeepTaskAlive(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "multi-content-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Send 3 content messages with distinct text to avoid dedup.
	for i := 0; i < 3; i++ {
		msg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Sender:    "agent:agent-a",
			Recipient: "user:test-user",
			Msg:       fmt.Sprintf("progress update %d", i),
			Type:      messages.TypeInstruction,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", msg); err != nil {
			t.Fatalf("HandleBrokerMessage[%d]: %v", i, err)
		}
	}

	time.Sleep(200 * time.Millisecond)

	// Task should still be working.
	task, err := store.GetTask(context.Background(), taskID)
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
	events := readStreamEvents(t, store, taskID)

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

	topic := "scion.project.proj1.user.test-user.messages"

	// First: send a content message.
	contentMsg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       "Working on it...",
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, contentMsg); err != nil {
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
	if err := b.HandleBrokerMessage(context.Background(), topic, completedMsg); err != nil {
		t.Fatalf("HandleBrokerMessage(completed): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Task should now be completed in the store.
	task, err := store.GetTask(context.Background(), taskID)
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

	// Event log should have the final event with Final=true.
	events := readStreamEvents(t, store, taskID)
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
		t.Error("expected a final StatusUpdate with state=completed in event log")
	}
}

func TestStateChangeInputRequiredKeepsTaskAlive(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "input-required-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

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
	task, err := store.GetTask(context.Background(), taskID)
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

	// Event log should have Final=false.
	events := readStreamEvents(t, store, taskID)
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
		t.Error("expected StatusUpdate with state=input-required in event log")
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

	task, err := store.GetTask(context.Background(), taskID)
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

// --- Tests for blocking wait via event log ---

func TestBlockingWaitForTaskEvent_ReturnsOnEvent(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "blocking-event-1"
	now := time.Now()

	// Seed the task directly in the store.
	if err := store.CreateTask(context.Background(), &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "proj1", AgentSlug: "agent-a",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	aKey := agentKey("proj1", "agent-a")
	b.registerActiveTask(taskID, aKey)

	// Inject a response event into the event log in a goroutine.
	go func() {
		time.Sleep(50 * time.Millisecond)
		injectResponseEvent(t, store, taskID, "Here is the answer")
	}()

	// Wait for the event (as the blocking SendMessage path would).
	ev, err := b.waitForTaskEvent(context.Background(), taskID, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForTaskEvent: %v", err)
	}

	if ev.Kind != "message" {
		t.Errorf("event kind = %q, want %q", ev.Kind, "message")
	}
}

func TestBlockingWaitForTaskEvent_TimeoutCleansUp(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "timeout-cleanup-1"
	now := time.Now()

	if err := store.CreateTask(context.Background(), &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "proj1", AgentSlug: "agent-a",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	aKey := agentKey("proj1", "agent-a")
	b.registerActiveTask(taskID, aKey)

	// Wait with a very short timeout — no events will arrive.
	_, err := b.waitForTaskEvent(context.Background(), taskID, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if err != ErrTimeout {
		t.Errorf("error = %v, want ErrTimeout", err)
	}
}

func TestBlockingWaitForTaskEvent_ContextCancel(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "cancel-cleanup-1"
	now := time.Now()

	if err := store.CreateTask(context.Background(), &state.Task{
		ID: taskID, ContextID: "ctx-1", ProjectID: "proj1", AgentSlug: "agent-a",
		State: TaskStateWorking, CreatedAt: now, UpdatedAt: now, Metadata: "{}",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	aKey := agentKey("proj1", "agent-a")
	b.registerActiveTask(taskID, aKey)

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after a short delay.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := b.waitForTaskEvent(ctx, taskID, 5*time.Second)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
}

func TestActiveTaskCleanup(t *testing.T) {
	b, _ := newLifecycleTestBridge(t)
	taskID := "cleanup-1"
	aKey := agentKey("proj1", "agent-a")

	b.registerActiveTask(taskID, aKey)

	b.tasksMu.RLock()
	_, isActive := b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if !isActive {
		t.Fatal("task should be active after register")
	}

	b.unregisterActiveTask(taskID, aKey)

	b.tasksMu.RLock()
	_, isActive = b.activeTasks[taskID]
	b.tasksMu.RUnlock()
	if isActive {
		t.Error("task should be removed from activeTasks after unregister")
	}
}

// --- Tests for full multi-turn lifecycle flow ---

func TestFullMultiTurnLifecycle(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "multi-turn-full-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

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
			Type:      messages.TypeInstruction,
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

	task, err := store.GetTask(context.Background(), taskID)
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

	task, err = store.GetTask(context.Background(), taskID)
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

	task, err = store.GetTask(context.Background(), taskID)
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

	// Verify we got all the events from the event log.
	events := readStreamEvents(t, store, taskID)

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
		Type:      messages.TypeInstruction,
		// No a2aTaskId in metadata.
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	task, err := store.GetTask(context.Background(), taskID)
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
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// The completed metric should NOT have been incremented.
	// We test indirectly by verifying the task is still active and not completed.
	task, _ := store.GetTask(context.Background(), taskID)
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
	task, _ := store.GetTask(context.Background(), taskID)
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
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), topic, lateContent); err != nil {
		t.Fatalf("HandleBrokerMessage(late content): %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// State should still be completed (store protects terminal states).
	task, _ = store.GetTask(context.Background(), taskID)
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

	task, _ := store.GetTask(context.Background(), taskID)
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
		Type:      messages.TypeInstruction,
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

	task, _ := store.GetTask(context.Background(), taskID)
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

	task, _ := store.GetTask(context.Background(), taskID)
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
		Type:      messages.TypeInstruction,
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
		task, _ := store.GetTask(context.Background(), tid)
		if task.State != TaskStateWorking {
			t.Errorf("task %s state = %q, want %q", tid, task.State, TaskStateWorking)
		}
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

			task, err := store.GetTask(context.Background(), taskID)
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

			// Check event log Final flag.
			events := readStreamEvents(t, store, taskID)
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

// --- Stream close via event log ---

func TestTerminalStateWritesFinalEvent(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "stream-close-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Start polling the event log.
	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	ch := streamTaskEvents(pollCtx, store, taskID, 0, 10, nil)

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

	// The channel should close after the final event is read.
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
	case <-time.After(5 * time.Second):
		t.Fatal("stream channel was not closed after terminal state-change")
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

func TestTerminalStateFailedWritesFinalEvent(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "stream-close-fail-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	pollCtx, pollCancel := context.WithCancel(context.Background())
	defer pollCancel()
	ch := streamTaskEvents(pollCtx, store, taskID, 0, 10, nil)

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
	case <-time.After(5 * time.Second):
		t.Fatal("stream channel was not closed after ERROR state-change")
	}
}

// --- Timestamp refresh test ---

func TestContentMessageRefreshesTimestamp(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "timestamp-refresh-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	// Record the initial timestamp.
	taskBefore, err := store.GetTask(context.Background(), taskID)
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
		Type:      messages.TypeInstruction,
		Metadata:  map[string]string{"a2aTaskId": taskID},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", contentMsg); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// The task's UpdatedAt should have been refreshed.
	taskAfter, err := store.GetTask(context.Background(), taskID)
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

// --- Explicit agent replies form the task response ---

// TestExplicitReplyBecomesTaskResponse covers the harness-independent
// response path: the agent runs `scion message user:<caller> ...`, and the
// hub publishes it on the caller's user topic as an "instruction" message
// with Sender agent:<slug> and no a2aTaskId. That reply must become the A2A
// task artifact and the response returned to the caller.
func TestExplicitReplyBecomesTaskResponse(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "explicit-reply-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")

	reply := &messages.StructuredMessage{
		Version:     1,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Sender:      "agent:agent-a",
		Recipient:   "user:test-user",
		Msg:         "Here is the answer",
		Type:        messages.TypeInstruction,
		Attachments: []string{"https://example.com/answer.txt"},
	}
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", reply); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	var artifacts []Artifact
	var replyMessages int
	for _, ev := range readStreamEvents(t, store, taskID) {
		if ev.ArtifactUpdate != nil {
			artifacts = append(artifacts, ev.ArtifactUpdate.Artifact)
		}
		if ev.StatusUpdate != nil && ev.StatusUpdate.Status.Message != nil {
			replyMessages++
		}
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1 from the explicit reply", len(artifacts))
	}
	parts := artifacts[0].Parts
	if len(parts) != 2 || parts[0].Text != "Here is the answer" || parts[1].URL != "https://example.com/answer.txt" {
		t.Errorf("artifact parts = %+v, want reply text and attachment", parts)
	}
	if replyMessages != 1 {
		t.Errorf("status messages = %d, want 1 carrying the reply", replyMessages)
	}

	// The blocking caller is answered by the explicit reply.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := b.waitForTaskEvent(ctx, taskID, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForTaskEvent: %v", err)
	}
	if ev.Kind != "artifact" {
		t.Fatalf("first response event kind = %q, want artifact", ev.Kind)
	}
	result, err := b.taskEventToTaskResult(taskID, "ctx-1", ev)
	if err != nil {
		t.Fatalf("taskEventToTaskResult: %v", err)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("result artifacts = %d, want 1 from the explicit reply", len(result.Artifacts))
	}
	if len(result.Artifacts[0].Parts) == 0 {
		t.Fatalf("result artifact has no parts: %+v", result.Artifacts[0])
	}
	if result.Artifacts[0].Parts[0].Text != "Here is the answer" {
		t.Errorf("result artifacts = %+v, want the explicit reply", result.Artifacts)
	}

	// The SDK executor maps the artifact event to COMPLETED with the reply
	// as its status message (taskEventToSDKEvent, "artifact" case).
	sdkEv, err := taskEventToSDKEvent(&a2asrv.ExecutorContext{TaskID: a2a.TaskID(taskID)}, ev)
	if err != nil {
		t.Fatalf("taskEventToSDKEvent: %v", err)
	}
	statusEv, ok := sdkEv.(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("SDK event = %T, want *a2a.TaskStatusUpdateEvent", sdkEv)
	}
	if statusEv.Status.State != a2a.TaskStateCompleted {
		t.Errorf("SDK state = %v, want %v", statusEv.Status.State, a2a.TaskStateCompleted)
	}
	if statusEv.Status.Message == nil || len(statusEv.Status.Message.Parts) == 0 ||
		statusEv.Status.Message.Parts[0].Text() != "Here is the answer" {
		t.Errorf("SDK status message = %+v, want the explicit reply", statusEv.Status.Message)
	}
}

// inputNeededMessage builds an input-needed message from agent-a. When
// taskID is empty the message carries no a2aTaskId metadata.
func inputNeededMessage(taskID, question string) *messages.StructuredMessage {
	msg := &messages.StructuredMessage{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    "agent:agent-a",
		Recipient: "user:test-user",
		Msg:       question,
		Type:      messages.TypeInputNeeded,
	}
	if taskID != "" {
		msg.Metadata = map[string]string{"a2aTaskId": taskID}
	}
	return msg
}

// sdkStatusEvent converts a bridge event with taskEventToSDKEvent and
// requires a status update event.
func sdkStatusEvent(t *testing.T, taskID string, ev *state.TaskEvent) *a2a.TaskStatusUpdateEvent {
	t.Helper()
	sdkEv, err := taskEventToSDKEvent(&a2asrv.ExecutorContext{TaskID: a2a.TaskID(taskID)}, ev)
	if err != nil {
		t.Fatalf("taskEventToSDKEvent(%s): %v", ev.Kind, err)
	}
	statusEv, ok := sdkEv.(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("SDK event for %s = %T, want *a2a.TaskStatusUpdateEvent", ev.Kind, sdkEv)
	}
	return statusEv
}

// captureWebhooks registers a push config for taskID that points at a test
// webhook server and returns a function that waits for in-flight dispatches
// and returns the decoded events in arrival order.
func captureWebhooks(t *testing.T, b *Bridge, store state.Store, taskID string) func() []StreamEvent {
	t.Helper()
	var mu sync.Mutex
	var got []StreamEvent
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var se StreamEvent
		if err := json.NewDecoder(r.Body).Decode(&se); err != nil {
			t.Errorf("decode webhook body: %v", err)
		}
		mu.Lock()
		got = append(got, se)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	b.push.client = testPushClient()
	if err := store.SetPushConfig(context.Background(), &state.PushNotificationConfig{
		ID: "push-" + taskID, TaskID: taskID, URL: ts.URL, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SetPushConfig: %v", err)
	}
	return func() []StreamEvent {
		b.push.Wait()
		mu.Lock()
		defer mu.Unlock()
		return append([]StreamEvent(nil), got...)
	}
}

// TestInputNeededReplyReturnsInputRequiredArtifact covers ptone/scion#3377
// on the legacy SendMessage path (the task has a legacy store row): an
// input-needed message becomes an artifact with the question, moves the
// legacy task to input-required, and is surfaced to blocking callers as
// input-required rather than as the final answer.
func TestInputNeededReplyReturnsInputRequiredArtifact(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "input-needed-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	webhooks := captureWebhooks(t, b, store, taskID)

	question := inputNeededMessage(taskID, "Which region should I deploy to?")
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", question); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	task, err := store.GetTask(context.Background(), taskID)
	if err != nil || task == nil {
		t.Fatalf("GetTask: %v (task=%v)", err, task)
	}
	if task.State != TaskStateInputRequired {
		t.Errorf("task state = %q, want %q", task.State, TaskStateInputRequired)
	}

	var artifacts []TaskArtifactUpdate
	var msgStates []string
	for _, ev := range readStreamEvents(t, store, taskID) {
		if ev.ArtifactUpdate != nil {
			artifacts = append(artifacts, *ev.ArtifactUpdate)
		}
		if ev.StatusUpdate != nil && ev.StatusUpdate.Status.Message != nil {
			msgStates = append(msgStates, ev.StatusUpdate.Status.State)
			if ev.StatusUpdate.Final {
				t.Error("input-needed status message must not be final")
			}
		}
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1 from the input-needed message", len(artifacts))
	}
	if len(artifacts[0].Artifact.Parts) == 0 {
		t.Fatalf("artifact has no parts: %+v", artifacts[0])
	}
	if artifacts[0].Artifact.Parts[0].Text != question.Msg {
		t.Errorf("artifact text = %q, want the question", artifacts[0].Artifact.Parts[0].Text)
	}
	if artifacts[0].State != TaskStateInputRequired {
		t.Errorf("artifact state = %q, want %q", artifacts[0].State, TaskStateInputRequired)
	}
	if len(msgStates) != 1 || msgStates[0] != TaskStateInputRequired {
		t.Errorf("status message states = %v, want [%s]", msgStates, TaskStateInputRequired)
	}

	// Push subscribers must see the same states as the stored events: the
	// question artifact labelled input-required and an input-required status.
	// Dispatch is per event, so arrival order is not guaranteed.
	var pushArtStates, pushMsgStates []string
	for _, ev := range webhooks() {
		if ev.ArtifactUpdate != nil {
			pushArtStates = append(pushArtStates, ev.ArtifactUpdate.State)
		}
		if ev.StatusUpdate != nil && ev.StatusUpdate.Status.Message != nil {
			pushMsgStates = append(pushMsgStates, ev.StatusUpdate.Status.State)
		}
	}
	if len(pushArtStates) != 1 || pushArtStates[0] != TaskStateInputRequired {
		t.Errorf("pushed artifact states = %v, want [%s]", pushArtStates, TaskStateInputRequired)
	}
	if len(pushMsgStates) != 1 || pushMsgStates[0] != TaskStateInputRequired {
		t.Errorf("pushed status message states = %v, want [%s]", pushMsgStates, TaskStateInputRequired)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := b.waitForTaskEvent(ctx, taskID, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForTaskEvent: %v", err)
	}
	if ev.Kind != "artifact" {
		t.Fatalf("first response event kind = %q, want artifact", ev.Kind)
	}
	result, err := b.taskEventToTaskResult(taskID, "ctx-1", ev)
	if err != nil {
		t.Fatalf("taskEventToTaskResult: %v", err)
	}
	if result.Status.State != TaskStateInputRequired {
		t.Errorf("result state = %q, want %q", result.Status.State, TaskStateInputRequired)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("result artifacts = %d, want 1 with the question", len(result.Artifacts))
	}
	if len(result.Artifacts[0].Parts) == 0 {
		t.Fatalf("result artifact has no parts: %+v", result.Artifacts[0])
	}
	if result.Artifacts[0].Parts[0].Text != question.Msg {
		t.Errorf("result artifacts = %+v, want the question", result.Artifacts)
	}
}

// TestInputNeededWithoutLegacyRowIsInputRequired covers ptone/scion#3377 on
// the SDK executor path. ScionExecutor.Execute only registers the task in
// the local cache; there is no legacy store row, so the legacy CAS updates
// nothing. The response must still be input-required, for both the
// artifact event and the message event, live and on replay.
func TestInputNeededWithoutLegacyRowIsInputRequired(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "input-needed-sdk-1"
	b.registerActiveTask(taskID, agentKey("proj1", "agent-a"))

	question := inputNeededMessage("", "Which region should I deploy to?")
	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages", question); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := b.waitForTaskEvent(ctx, taskID, 2*time.Second)
	if err != nil {
		t.Fatalf("waitForTaskEvent: %v", err)
	}
	if ev.Kind != "artifact" {
		t.Fatalf("first response event kind = %q, want artifact", ev.Kind)
	}
	artEv := sdkStatusEvent(t, taskID, ev)
	if artEv.Status.State != a2a.TaskStateInputRequired {
		t.Errorf("SDK artifact state = %v, want %v", artEv.Status.State, a2a.TaskStateInputRequired)
	}
	if artEv.Status.Message == nil || len(artEv.Status.Message.Parts) == 0 {
		t.Fatalf("SDK artifact status message has no parts: %+v", artEv.Status.Message)
	}
	if got := artEv.Status.Message.Parts[0].Text(); got != question.Msg {
		t.Errorf("SDK artifact text = %q, want the question", got)
	}

	events, err := store.ReadTaskEvents(context.Background(), taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	var kinds []string
	for i := range events {
		raw := &events[i]
		kinds = append(kinds, raw.Kind)
		if st := sdkStatusEvent(t, taskID, raw).Status.State; st != a2a.TaskStateInputRequired {
			t.Errorf("SDK %s state = %v, want %v", raw.Kind, st, a2a.TaskStateInputRequired)
		}
		if raw.Kind != "message" {
			continue
		}
		replay, err := taskEventToSDKResubscribeEvent(a2a.TaskID(taskID), raw)
		if err != nil {
			t.Fatalf("taskEventToSDKResubscribeEvent(message): %v", err)
		}
		replayEv, ok := replay.(*a2a.TaskStatusUpdateEvent)
		if !ok {
			t.Fatalf("replayed message = %T, want *a2a.TaskStatusUpdateEvent", replay)
		}
		if replayEv.Status.State != a2a.TaskStateInputRequired {
			t.Errorf("replayed message state = %v, want %v", replayEv.Status.State, a2a.TaskStateInputRequired)
		}
	}
	if len(kinds) != 2 || kinds[0] != "artifact" || kinds[1] != "message" {
		t.Errorf("event kinds = %v, want [artifact message]", kinds)
	}
}

// TestInputNeededKeepsTerminalLegacyState pins that a late input-needed
// message does not move a terminal legacy task back to input-required.
func TestInputNeededKeepsTerminalLegacyState(t *testing.T) {
	b, store := newLifecycleTestBridge(t)
	taskID := "input-needed-terminal-1"
	seedLifecycleTask(t, b, store, taskID, "proj1", "agent-a")
	if _, err := store.UpdateTaskState(context.Background(), taskID, TaskStateCompleted); err != nil {
		t.Fatalf("UpdateTaskState: %v", err)
	}

	if err := b.HandleBrokerMessage(context.Background(), "scion.project.proj1.user.test-user.messages",
		inputNeededMessage(taskID, "One more question?")); err != nil {
		t.Fatalf("HandleBrokerMessage: %v", err)
	}

	task, err := store.GetTask(context.Background(), taskID)
	if err != nil || task == nil {
		t.Fatalf("GetTask: %v (task=%v)", err, task)
	}
	// Sanity check only: the store CAS already refuses transitions out of a
	// terminal state, so this holds even without the bridge guard.
	if task.State != TaskStateCompleted {
		t.Errorf("task state = %q, want %q", task.State, TaskStateCompleted)
	}

	// The guard's own effect: the response events of a terminal legacy task
	// are not labelled input-required. Require that both events exist so a
	// dropped message cannot pass vacuously.
	var artifacts, msgs int
	for _, ev := range readStreamEvents(t, store, taskID) {
		switch {
		case ev.ArtifactUpdate != nil:
			artifacts++
			if ev.ArtifactUpdate.State != "" {
				t.Errorf("artifact state = %q, want none on a terminal task", ev.ArtifactUpdate.State)
			}
		case ev.StatusUpdate != nil:
			msgs++
			if ev.StatusUpdate.Status.State != TaskStateCompleted {
				t.Errorf("message event state = %q, want %q", ev.StatusUpdate.Status.State, TaskStateCompleted)
			}
		}
	}
	if artifacts != 1 || msgs != 1 {
		t.Errorf("got %d artifact and %d message events, want 1 and 1", artifacts, msgs)
	}
}

// TestResubscribeReplayResponseState covers the durable subscribe replay
// converter: an input-needed response replays as input-required, and any
// other response still replays as completed.
func TestResubscribeReplayResponseState(t *testing.T) {
	taskID := a2a.TaskID("replay-1")
	msgPayload := func(st string) []byte {
		p, _ := json.Marshal(TaskStatusUpdate{
			TaskID: string(taskID),
			Status: TaskStatus{State: st, Message: &Message{Role: "agent", Parts: []Part{{Text: "q?"}}}},
		})
		return p
	}
	artPayload := func(st string) []byte {
		p, _ := json.Marshal(TaskArtifactUpdate{TaskID: string(taskID), State: st})
		return p
	}
	tests := []struct {
		name string
		ev   state.TaskEvent
		want a2a.TaskState
	}{
		{"input-needed message", state.TaskEvent{Kind: "message", Payload: msgPayload(TaskStateInputRequired)}, a2a.TaskStateInputRequired},
		{"plain message", state.TaskEvent{Kind: "message", Payload: msgPayload(TaskStateWorking)}, a2a.TaskStateCompleted},
		{"input-needed empty artifact", state.TaskEvent{Kind: "artifact", Payload: artPayload(TaskStateInputRequired)}, a2a.TaskStateInputRequired},
		{"plain empty artifact", state.TaskEvent{Kind: "artifact", Payload: artPayload("")}, a2a.TaskStateCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := taskEventToSDKResubscribeEvent(taskID, &tt.ev)
			if err != nil {
				t.Fatalf("taskEventToSDKResubscribeEvent: %v", err)
			}
			statusEv, ok := got.(*a2a.TaskStatusUpdateEvent)
			if !ok {
				t.Fatalf("event = %T, want *a2a.TaskStatusUpdateEvent", got)
			}
			if statusEv.Status.State != tt.want {
				t.Errorf("state = %v, want %v", statusEv.Status.State, tt.want)
			}
		})
	}
}

// TestLiveSDKEventResponseState pins the response state on the live SDK
// executor path (taskEventToSDKEvent), including the empty-artifact branch,
// which is reached by an input-needed message with no text or attachments.
func TestLiveSDKEventResponseState(t *testing.T) {
	taskID := "live-1"
	msgPayload := func(st string) []byte {
		p, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: st, Message: &Message{Role: "agent", Parts: []Part{{Text: "q?"}}}},
		})
		return p
	}
	artPayload := func(st string, parts ...Part) []byte {
		p, _ := json.Marshal(TaskArtifactUpdate{TaskID: taskID, State: st, Artifact: Artifact{ArtifactID: "a1", Parts: parts}})
		return p
	}
	tests := []struct {
		name    string
		ev      state.TaskEvent
		want    a2a.TaskState
		wantMsg bool
	}{
		{"input-needed message", state.TaskEvent{Kind: "message", Payload: msgPayload(TaskStateInputRequired)}, a2a.TaskStateInputRequired, true},
		{"plain message", state.TaskEvent{Kind: "message", Payload: msgPayload(TaskStateWorking)}, a2a.TaskStateCompleted, true},
		{"input-needed artifact", state.TaskEvent{Kind: "artifact", Payload: artPayload(TaskStateInputRequired, Part{Text: "q?"})}, a2a.TaskStateInputRequired, true},
		{"plain artifact", state.TaskEvent{Kind: "artifact", Payload: artPayload("", Part{Text: "done"})}, a2a.TaskStateCompleted, true},
		{"input-needed empty artifact", state.TaskEvent{Kind: "artifact", Payload: artPayload(TaskStateInputRequired)}, a2a.TaskStateInputRequired, false},
		{"plain empty artifact", state.TaskEvent{Kind: "artifact", Payload: artPayload("")}, a2a.TaskStateCompleted, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.ev
			got := sdkStatusEvent(t, taskID, &ev)
			if got.Status.State != tt.want {
				t.Errorf("state = %v, want %v", got.Status.State, tt.want)
			}
			if (got.Status.Message != nil) != tt.wantMsg {
				t.Errorf("status message present = %v, want %v", got.Status.Message != nil, tt.wantMsg)
			}
		})
	}
}
