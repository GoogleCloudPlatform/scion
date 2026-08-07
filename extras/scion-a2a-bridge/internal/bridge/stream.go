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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

// ErrTooManySubscribers is returned when the SSE connection limit is reached.
var ErrTooManySubscribers = errors.New("too many active SSE subscribers")

// StreamEvent represents an SSE event sent to streaming clients.
type StreamEvent struct {
	Task           *TaskResult         `json:"task,omitempty"`
	StatusUpdate   *TaskStatusUpdate   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdate `json:"artifactUpdate,omitempty"`
}

// TaskStatusUpdate represents a task state change event.
type TaskStatusUpdate struct {
	TaskID string     `json:"taskId"`
	Status TaskStatus `json:"status"`
	Final  bool       `json:"final"`
}

// TaskArtifactUpdate represents a task artifact delivery event.
type TaskArtifactUpdate struct {
	TaskID   string   `json:"taskId"`
	Artifact Artifact `json:"artifact"`
}

// StreamManager tracks active SSE stream sessions per task, each polling the
// event log with its own cursor. This replaces the old in-memory channel
// fan-out with cursor-based reads from the durable event log.
type StreamManager struct {
	mu             sync.RWMutex
	sessions       map[string]int // taskID -> count of active sessions
	maxSubscribers int
	totalActive    int64 // guarded by mu — no atomic needed
}

// NewStreamManager creates a new stream manager with the given subscriber limit.
func NewStreamManager(maxSubscribers int) *StreamManager {
	if maxSubscribers <= 0 {
		maxSubscribers = 100
	}
	return &StreamManager{
		sessions:       make(map[string]int),
		maxSubscribers: maxSubscribers,
	}
}

// AcquireSession registers a new SSE stream session for a task. Returns a
// cleanup function that must be called when the session ends. Enforces the
// global subscriber cap.
func (sm *StreamManager) AcquireSession(taskID string) (func(), error) {
	sm.mu.Lock()
	if int(sm.totalActive) >= sm.maxSubscribers {
		sm.mu.Unlock()
		return nil, ErrTooManySubscribers
	}
	sm.sessions[taskID]++
	sm.totalActive++
	sm.mu.Unlock()

	cleanup := func() {
		sm.mu.Lock()
		sm.sessions[taskID]--
		if sm.sessions[taskID] <= 0 {
			delete(sm.sessions, taskID)
		}
		sm.totalActive--
		sm.mu.Unlock()
	}
	return cleanup, nil
}

// HasSubscribers returns true if any SSE sessions are active for the task.
func (sm *StreamManager) HasSubscribers(taskID string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[taskID] > 0
}

// StreamSession represents a single SSE client reading from the event log.
type StreamSession struct {
	taskID string
	cursor int64
	store  state.Store
}

// NewStreamSession creates a session that reads events from the given cursor.
// Use cursor=0 to replay all events, or a specific ID to resume.
func NewStreamSession(taskID string, cursor int64, store state.Store) *StreamSession {
	return &StreamSession{
		taskID: taskID,
		cursor: cursor,
		store:  store,
	}
}

// ReadNext reads the next batch of events after the session's cursor.
func (ss *StreamSession) ReadNext(ctx context.Context, batchLimit int) ([]state.TaskEvent, error) {
	events, err := ss.store.ReadTaskEvents(ctx, ss.taskID, ss.cursor, batchLimit)
	if err != nil {
		return nil, err
	}
	if len(events) > 0 {
		ss.cursor = events[len(events)-1].ID
	}
	return events, nil
}

// Cursor returns the current cursor position.
func (ss *StreamSession) Cursor() int64 {
	return ss.cursor
}

// streamTaskEvents polls the event log for a task and sends StreamEvents on
// the returned channel. The channel is closed when a final event is read,
// the context is cancelled, or an error occurs. If a Notifier is provided
// (non-nil), notifications accelerate the poll loop by waking it immediately.
func streamTaskEvents(ctx context.Context, store state.Store, taskID string, cursor int64, batchLimit int, notifier *Notifier) <-chan StreamEvent {
	ch := make(chan StreamEvent, 16)
	go func() {
		defer close(ch)
		session := NewStreamSession(taskID, cursor, store)
		interval := 100 * time.Millisecond
		pollTimer := time.NewTimer(interval)
		defer pollTimer.Stop()

		// Register for NOTIFY acceleration (no-op if notifier is nil).
		var notifyCh <-chan struct{}
		var cleanup func()
		if notifier != nil {
			notifyCh, cleanup = notifier.Register(taskID)
			defer cleanup()
		}

		for {
			events, err := session.ReadNext(ctx, batchLimit)
			if err != nil {
				slog.Error("stream: error reading task events", "task_id", taskID, "error", err)
				return
			}

			for _, ev := range events {
				streamEv, err := taskEventToStreamEvent(ev)
				if err != nil {
					slog.Error("stream: error converting event", "task_id", taskID, "event_id", ev.ID, "error", err)
					continue
				}
				select {
				case ch <- streamEv:
				case <-ctx.Done():
					return
				}
				if ev.Final {
					return
				}
			}

			// Reset backoff when we got events
			if len(events) > 0 {
				interval = 100 * time.Millisecond
				if !pollTimer.Stop() {
					select {
					case <-pollTimer.C:
					default:
					}
				}
				pollTimer.Reset(interval)
				continue // immediately check for more
			}

			select {
			case <-notifyCh:
				// NOTIFY woke us — immediately re-read (reset backoff).
				interval = 100 * time.Millisecond
				if !pollTimer.Stop() {
					select {
					case <-pollTimer.C:
					default:
					}
				}
				pollTimer.Reset(interval)
			case <-pollTimer.C:
				interval = backoffInterval(interval)
				pollTimer.Reset(interval)
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// backoffInterval increases the polling interval up to a 2s cap.
func backoffInterval(current time.Duration) time.Duration {
	next := current * 2
	if next > 2*time.Second {
		return 2 * time.Second
	}
	return next
}

// taskEventToStreamEvent converts a stored TaskEvent to a StreamEvent.
func taskEventToStreamEvent(ev state.TaskEvent) (StreamEvent, error) {
	switch ev.Kind {
	case "status":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return StreamEvent{}, fmt.Errorf("unmarshal status event: %w", err)
		}
		su.TaskID = ev.TaskID
		su.Final = ev.Final
		return StreamEvent{StatusUpdate: &su}, nil
	case "artifact":
		var au TaskArtifactUpdate
		if err := json.Unmarshal(ev.Payload, &au); err != nil {
			return StreamEvent{}, fmt.Errorf("unmarshal artifact event: %w", err)
		}
		au.TaskID = ev.TaskID
		return StreamEvent{ArtifactUpdate: &au}, nil
	case "message":
		// Message events carry a status update with a message payload
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return StreamEvent{}, fmt.Errorf("unmarshal message event: %w", err)
		}
		su.TaskID = ev.TaskID
		su.Final = ev.Final
		return StreamEvent{StatusUpdate: &su}, nil
	default:
		return StreamEvent{}, fmt.Errorf("unknown event kind: %s", ev.Kind)
	}
}

// SendStreamingMessage creates a task, sends the message to the agent, and
// returns a channel that will receive SSE events as the agent processes the request.
func (b *Bridge) SendStreamingMessage(ctx context.Context, projectSlug, agentSlug, contextID string, parts []Part) (string, <-chan StreamEvent, func(), error) {
	agentCtx, err := b.resolveContext(ctx, projectSlug, agentSlug, contextID)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve context: %w", err)
	}

	taskID := uuid.New().String()
	now := time.Now()
	task := &state.Task{
		ID:        taskID,
		ContextID: agentCtx.ContextID,
		ProjectID: agentCtx.ProjectID,
		AgentSlug: agentCtx.AgentSlug,
		AgentID:   agentCtx.AgentID,
		State:     TaskStateSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}
	if err := b.store.CreateTask(ctx, task); err != nil {
		return "", nil, nil, fmt.Errorf("create task: %w", err)
	}
	if b.metrics != nil {
		b.metrics.TasksCreated.WithLabelValues(agentCtx.ProjectID).Inc()
	}

	aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
	b.registerActiveTask(taskID, aKey)

	sessionCleanup, err := b.streams.AcquireSession(taskID)
	if err != nil {
		b.unregisterActiveTask(taskID, aKey)
		return "", nil, nil, fmt.Errorf("subscribe: %w", err)
	}

	// Write the initial "submitted" event to the event log so streaming
	// clients see it.
	submittedPayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{State: TaskStateSubmitted},
	})
	if _, err := b.store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "status",
		Payload:  submittedPayload,
		Final:    false,
		DedupKey: taskID + ":submitted",
	}); err != nil {
		b.log.Error("failed to append task event", "task_id", taskID, "kind", "status", "error", err)
	}

	// Start polling the event log for this task. Derive from the request
	// context so cancellation propagates when the client disconnects.
	pollCtx, pollCancel := context.WithCancel(ctx)
	events := streamTaskEvents(pollCtx, b.store, taskID, 0, 10, b.notifier)

	// NOTE: SendStreamingMessage intentionally uses the bridge admin identity
	// rather than propagating per-user CallerIdentity. Streaming is an admin-only
	// transport; per-user routing is handled by the blocking SendMessage path and
	// the SDK executor (ScionExecutor.Execute). If per-user streaming is needed
	// in the future, CallerIdentity propagation should be added here (similar to
	// Bridge.SendMessage).
	scionMsg := TranslateA2AToScion(parts)
	scionMsg.Sender = fmt.Sprintf("user:%s", b.config.Hub.User)
	scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)
	scionMsg.Metadata = map[string]string{"a2aTaskId": taskID}

	if b.broker != nil {
		pattern := projectcompat.UserTopic(agentCtx.ProjectID, b.config.Hub.User)
		if err := b.broker.RequestSubscription(pattern); err != nil {
			b.log.Warn("failed to request subscription", "pattern", pattern, "error", err)
		}
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		sendCtx, cancel := context.WithTimeout(b.shutdownCtx, 30*time.Second)
		defer cancel()

		if _, err := b.hubClient.Agents().SendStructuredMessage(sendCtx, agentCtx.AgentID, scionMsg, false, false, false); err != nil {
			b.log.Error("streaming send failed", "error", err, "task_id", taskID)
			changed, updateErr := b.store.UpdateTaskState(sendCtx, taskID, TaskStateFailed)
			if updateErr != nil {
				b.log.Error("failed to update task state", "error", updateErr, "task_id", taskID)
			}
			if changed {
				failPayload, _ := json.Marshal(TaskStatusUpdate{
					TaskID: taskID,
					Status: TaskStatus{State: TaskStateFailed},
				})
				if _, err := b.store.AppendTaskEvent(sendCtx, &state.TaskEvent{
					TaskID:  taskID,
					Kind:    "status",
					Payload: failPayload,
					Final:   true,
				}); err != nil {
					b.log.Error("failed to append task event", "task_id", taskID, "kind", "status", "error", err)
				}
			}
			b.unregisterActiveTask(taskID, aKey)
			return
		}

		changed, updateErr := b.store.UpdateTaskState(sendCtx, taskID, TaskStateWorking)
		if updateErr != nil {
			b.log.Error("failed to update task state", "error", updateErr, "task_id", taskID)
		}
		if changed {
			workingPayload, _ := json.Marshal(TaskStatusUpdate{
				TaskID: taskID,
				Status: TaskStatus{State: TaskStateWorking},
			})
			if _, err := b.store.AppendTaskEvent(sendCtx, &state.TaskEvent{
				TaskID:   taskID,
				Kind:     "status",
				Payload:  workingPayload,
				Final:    false,
				DedupKey: taskID + ":working",
			}); err != nil {
				b.log.Error("failed to append task event", "task_id", taskID, "kind", "status", "error", err)
			}
		}
	}()

	returnedCleanup := func() {
		pollCancel()
		sessionCleanup()
		b.unregisterActiveTask(taskID, aKey)
	}
	return taskID, events, returnedCleanup, nil
}

// SubscribeToTask opens an SSE stream for an existing in-progress task.
// If cursor > 0, events are replayed from that cursor (tasks/resubscribe).
func (b *Bridge) SubscribeToTask(ctx context.Context, taskID string) (<-chan StreamEvent, func(), error) {
	return b.SubscribeToTaskFromCursor(ctx, taskID, 0)
}

// SubscribeToTaskFromCursor opens an SSE stream for a task starting from the
// given cursor. Use cursor=0 to replay all events, or a specific event ID
// to resume from that point (tasks/resubscribe).
func (b *Bridge) SubscribeToTaskFromCursor(ctx context.Context, taskID string, cursor int64) (<-chan StreamEvent, func(), error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, nil, fmt.Errorf("task not found: %s", taskID)
	}
	if IsTerminalState(task.State) {
		return nil, nil, fmt.Errorf("task %s is in terminal state: %s", taskID, task.State)
	}

	sessionCleanup, err := b.streams.AcquireSession(taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe: %w", err)
	}

	pollCtx, pollCancel := context.WithCancel(ctx)
	events := streamTaskEvents(pollCtx, b.store, taskID, cursor, 10, b.notifier)

	cleanup := func() {
		pollCancel()
		sessionCleanup()
	}

	return events, cleanup, nil
}
