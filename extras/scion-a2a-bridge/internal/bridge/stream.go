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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
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

// StreamManager tracks active SSE streams per task and fans out events.
type StreamManager struct {
	mu             sync.RWMutex
	streams        map[string][]chan StreamEvent
	maxSubscribers int
	droppedEvents  atomic.Int64
}

// NewStreamManager creates a new stream manager with the given subscriber limit.
func NewStreamManager(maxSubscribers int) *StreamManager {
	if maxSubscribers <= 0 {
		maxSubscribers = 100
	}
	return &StreamManager{
		streams:        make(map[string][]chan StreamEvent),
		maxSubscribers: maxSubscribers,
	}
}

// Subscribe registers a new SSE stream for a task. Returns a receive channel
// and a cleanup function that must be called when the stream is no longer needed.
// The global cap is enforced with an O(N_tasks) scan on every Subscribe. At the
// default maxSubscribers=100 this is trivial; if the cap becomes config-driven
// and raised significantly, switch to an atomic counter.
func (sm *StreamManager) Subscribe(taskID string) (<-chan StreamEvent, func(), error) {
	ch := make(chan StreamEvent, 16)
	sm.mu.Lock()
	total := 0
	for _, subs := range sm.streams {
		total += len(subs)
	}
	if total >= sm.maxSubscribers {
		sm.mu.Unlock()
		return nil, nil, ErrTooManySubscribers
	}
	sm.streams[taskID] = append(sm.streams[taskID], ch)
	sm.mu.Unlock()

	cleanup := func() {
		sm.mu.Lock()
		defer sm.mu.Unlock()
		streams := sm.streams[taskID]
		for i, s := range streams {
			if s == ch {
				sm.streams[taskID] = append(streams[:i], streams[i+1:]...)
				break
			}
		}
		if len(sm.streams[taskID]) == 0 {
			delete(sm.streams, taskID)
		}
	}

	return ch, cleanup, nil
}

// Broadcast sends an event to all active streams for a task.
// Holds RLock for the entire send loop to prevent concurrent CloseAll
// from closing channels mid-send (which would panic on send-to-closed-channel).
func (sm *StreamManager) Broadcast(taskID string, event StreamEvent) {
	sm.mu.RLock()
	streams := sm.streams[taskID]

	var slowChans []chan StreamEvent
	for _, ch := range streams {
		select {
		case ch <- event:
		default:
			dropped := sm.droppedEvents.Add(1)
			slog.Warn("SSE subscriber too slow, will remove",
				"task_id", taskID,
				"total_dropped", dropped,
			)
			slowChans = append(slowChans, ch)
		}
	}
	sm.mu.RUnlock()

	// Safe: Subscribe's cleanup removes by channel identity, so if Broadcast already
	// removed and closed a slow channel, the cleanup is a no-op — no double-close.
	if len(slowChans) > 0 {
		sm.mu.Lock()
		for _, slow := range slowChans {
			subs := sm.streams[taskID]
			for i, s := range subs {
				if s == slow {
					sm.streams[taskID] = append(subs[:i], subs[i+1:]...)
					close(slow)
					break
				}
			}
		}
		if len(sm.streams[taskID]) == 0 {
			delete(sm.streams, taskID)
		}
		sm.mu.Unlock()
	}
}

// HasSubscribers returns true if any SSE streams are active for the task.
func (sm *StreamManager) HasSubscribers(taskID string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.streams[taskID]) > 0
}

// CloseAll closes all streams for a task (used on task completion).
func (sm *StreamManager) CloseAll(taskID string) {
	sm.mu.Lock()
	channels := sm.streams[taskID]
	delete(sm.streams, taskID)
	sm.mu.Unlock()

	for _, ch := range channels {
		close(ch)
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
		ProjectID:   agentCtx.ProjectID,
		AgentSlug: agentCtx.AgentSlug,
		AgentID:   agentCtx.AgentID,
		State:     TaskStateSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}
	if err := b.store.CreateTask(task); err != nil {
		return "", nil, nil, fmt.Errorf("create task: %w", err)
	}
	if b.metrics != nil {
		b.metrics.TasksCreated.WithLabelValues(agentCtx.ProjectID).Inc()
	}

	aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
	b.registerActiveTask(taskID, aKey)

	events, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		b.unregisterActiveTask(taskID, aKey)
		return "", nil, nil, fmt.Errorf("subscribe: %w", err)
	}

	b.streams.Broadcast(taskID, StreamEvent{
		Task: &TaskResult{
			ID:        taskID,
			ContextID: agentCtx.ContextID,
			Status:    TaskStatus{State: TaskStateSubmitted},
		},
	})

	scionMsg := TranslateA2AToScion(parts)
	scionMsg.Sender = fmt.Sprintf("user:%s", b.config.Hub.User)
	scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)
	scionMsg.Metadata = map[string]string{"a2aTaskId": taskID}

	if b.broker != nil {
		pattern := fmt.Sprintf("scion.project.%s.user.%s.messages", agentCtx.ProjectID, b.config.Hub.User)
		if err := b.broker.RequestSubscription(pattern); err != nil {
			b.log.Warn("failed to request subscription", "pattern", pattern, "error", err)
		}
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		sendCtx, cancel := context.WithTimeout(b.shutdownCtx, 30*time.Second)
		defer cancel()

		if err := b.hubClient.Agents().SendStructuredMessage(sendCtx, agentCtx.AgentID, scionMsg, false, false); err != nil {
			b.log.Error("streaming send failed", "error", err, "task_id", taskID)
			if err := b.store.UpdateTaskState(taskID, TaskStateFailed); err != nil {
				b.log.Error("failed to update task state", "error", err, "task_id", taskID)
			}
			b.streams.Broadcast(taskID, StreamEvent{
				StatusUpdate: &TaskStatusUpdate{
					TaskID: taskID,
					Status: TaskStatus{State: TaskStateFailed},
					Final:  true,
				},
			})
			b.unregisterActiveTask(taskID, aKey)
			return
		}

		if err := b.store.UpdateTaskState(taskID, TaskStateWorking); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}
		b.streams.Broadcast(taskID, StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: taskID,
				Status: TaskStatus{State: TaskStateWorking},
			},
		})
	}()

	returnedCleanup := func() {
		cleanup()
		b.unregisterActiveTask(taskID, aKey)
	}
	return taskID, events, returnedCleanup, nil
}

// SubscribeToTask opens an SSE stream for an existing in-progress task.
func (b *Bridge) SubscribeToTask(ctx context.Context, taskID string) (<-chan StreamEvent, func(), error) {
	task, err := b.store.GetTask(taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, nil, fmt.Errorf("task not found: %s", taskID)
	}
	if IsTerminalState(task.State) {
		return nil, nil, fmt.Errorf("task %s is in terminal state: %s", taskID, task.State)
	}

	events, cleanup, err := b.streams.Subscribe(taskID)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe: %w", err)
	}

	b.streams.Broadcast(taskID, StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: task.State},
		},
	})

	return events, cleanup, nil
}
