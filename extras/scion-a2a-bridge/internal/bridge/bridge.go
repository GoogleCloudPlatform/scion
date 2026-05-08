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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

var (
	ErrAgentNotFound  = errors.New("agent not found")
	ErrContextUnknown = errors.New("unknown context ID")
)

// waiter tracks a blocking response channel with agent routing info.
type waiter struct {
	ch        chan *messages.StructuredMessage
	agentSlug string
	groveID   string
}

// Bridge is the core bridge logic that ties together state management,
// hub client operations, and message translation.
type Bridge struct {
	store     *state.Store
	hubClient hubclient.Client
	minter    *identity.TokenMinter
	config    *Config
	broker    *BrokerServer
	streams   *StreamManager
	push      *PushDispatcher
	metrics   *Metrics
	log       *slog.Logger

	// waiters tracks channels waiting for agent responses, keyed by taskID.
	mu      sync.RWMutex
	waiters map[string]*waiter

	// activeTasks maps taskID to routing/lifecycle metadata for broker messages.
	tasksMu     sync.RWMutex
	activeTasks map[string]activeTaskEntry

	// agentTasks maps agentKey (groveID:agentSlug) to active task IDs,
	// used for reverse lookup when broker messages arrive.
	agentTasks map[string][]string

	// wg tracks background goroutines to drain on shutdown.
	wg sync.WaitGroup

	// brokerMsgs decouples HandleBrokerMessage (called synchronously by the
	// broker RPC) from dispatch work. Publish enqueues; a worker drains.
	brokerMsgs chan brokerMessage

	// agentCache caches lookupAgent results to avoid listing all agents per call.
	agentCacheMu sync.RWMutex
	agentCache   map[string]*agentCacheEntry

	// shutdownCtx is cancelled during graceful shutdown.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

type agentCacheEntry struct {
	agent    *hubclient.Agent
	cachedAt time.Time
}

const agentCacheTTL = 3 * time.Minute

type activeTaskEntry struct {
	aKey      string
	createdAt time.Time
}

type brokerMessage struct {
	topic string
	msg   *messages.StructuredMessage
}

// New creates a new Bridge instance.
func New(store *state.Store, hubClient hubclient.Client, minter *identity.TokenMinter, cfg *Config, metrics *Metrics, log *slog.Logger) *Bridge {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bridge{
		store:          store,
		hubClient:      hubClient,
		minter:         minter,
		config:         cfg,
		metrics:        metrics,
		log:            log,
		streams:        NewStreamManager(cfg.Bridge.MaxSubscribers),
		push:           NewPushDispatcher(store, cfg, log, ctx),
		waiters:        make(map[string]*waiter),
		activeTasks:    make(map[string]activeTaskEntry),
		agentTasks:     make(map[string][]string),
		brokerMsgs:     make(chan brokerMessage, 256),
		agentCache:     make(map[string]*agentCacheEntry),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
	b.wg.Add(2)
	go b.janitor()
	go b.brokerWorker()
	return b
}

// janitor periodically reaps active tasks that have exceeded the maximum age,
// preventing unbounded growth of activeTasks/agentTasks/waiters maps when
// agents crash or broker connections are lost.
func (b *Bridge) janitor() {
	defer b.wg.Done()

	maxAge := 2 * b.config.Timeouts.SendMessage
	if maxAge == 0 {
		maxAge = 4 * time.Minute
	}
	interval := maxAge / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.shutdownCtx.Done():
			return
		case <-ticker.C:
			b.reapStaleTasks(maxAge)
			b.evictStaleAgentCache()
		}
	}
}

func (b *Bridge) reapStaleTasks(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)

	b.tasksMu.RLock()
	var candidates []struct {
		taskID string
		entry  activeTaskEntry
	}
	for taskID, entry := range b.activeTasks {
		// Skip tasks that were created recently — they can't be stale yet.
		if entry.createdAt.After(cutoff) {
			continue
		}
		candidates = append(candidates, struct {
			taskID string
			entry  activeTaskEntry
		}{taskID, entry})
	}
	b.tasksMu.RUnlock()

	for _, c := range candidates {
		task, err := b.store.GetTask(c.taskID)
		if err != nil {
			b.log.Error("janitor: failed to get task", "task_id", c.taskID, "error", err)
			continue
		}
		if task != nil && IsTerminalState(task.State) {
			b.unregisterActiveTask(c.taskID, c.entry.aKey)
			b.streams.CloseAll(c.taskID)
			continue
		}
		if task == nil || task.UpdatedAt.Before(cutoff) {
			b.log.Warn("janitor: reaping stale task", "task_id", c.taskID, "age_cutoff", maxAge)
			if err := b.store.UpdateTaskState(c.taskID, TaskStateFailed); err != nil {
				b.log.Error("janitor: failed to update task state", "task_id", c.taskID, "error", err)
			}
			if b.metrics != nil {
				b.metrics.TasksCompleted.WithLabelValues(TaskStateFailed).Inc()
			}
			failEvent := StreamEvent{
				StatusUpdate: &TaskStatusUpdate{
					TaskID: c.taskID,
					Status: TaskStatus{State: TaskStateFailed},
					Final:  true,
				},
			}
			b.streams.Broadcast(c.taskID, failEvent)
			b.push.Dispatch(b.shutdownCtx, c.taskID, failEvent)
			b.unregisterActiveTask(c.taskID, c.entry.aKey)
			b.streams.CloseAll(c.taskID)
		}
	}
}

// Shutdown gracefully drains background work.
// Order: (1) close broker channel so brokerWorker drains buffered messages,
// (2) cancel shutdownCtx to stop the janitor,
// (3) wait for both goroutines to exit,
// (4) wait for push goroutines spawned during drain to finish.
func (b *Bridge) Shutdown() {
	close(b.brokerMsgs)
	b.shutdownCancel()
	b.wg.Wait()
	b.push.Wait()
}

// SetBroker wires the broker server for subscription management.
func (b *Bridge) SetBroker(broker *BrokerServer) {
	b.broker = broker
}

// agentKey returns a composite key for grove-scoped agent isolation.
func agentKey(groveID, agentSlug string) string {
	return groveID + ":" + agentSlug
}

// SendMessage handles an A2A SendMessage. When blocking is true (the default),
// it waits for the agent response. When blocking is false, it returns immediately
// after submitting the message and the client can poll via GetTask or subscribe.
func (b *Bridge) SendMessage(ctx context.Context, groveSlug, agentSlug, contextID string, parts []Part, blocking bool) (*TaskResult, error) {
	agentCtx, err := b.resolveContext(ctx, groveSlug, agentSlug, contextID)
	if err != nil {
		return nil, fmt.Errorf("resolve context: %w", err)
	}

	taskID := uuid.New().String()
	now := time.Now()
	task := &state.Task{
		ID:        taskID,
		ContextID: agentCtx.ContextID,
		GroveID:   agentCtx.GroveID,
		AgentSlug: agentCtx.AgentSlug,
		AgentID:   agentCtx.AgentID,
		State:     TaskStateSubmitted,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  "{}",
	}
	if err := b.store.CreateTask(task); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	if b.metrics != nil {
		b.metrics.TasksCreated.WithLabelValues(agentCtx.GroveID).Inc()
	}

	scionMsg := TranslateA2AToScion(parts)
	scionMsg.Sender = fmt.Sprintf("user:%s", b.config.Hub.User)
	scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)
	scionMsg.Metadata = map[string]string{"a2aTaskId": taskID}

	if b.broker != nil {
		pattern := fmt.Sprintf("scion.grove.%s.user.%s.messages", agentCtx.GroveID, b.config.Hub.User)
		if err := b.broker.RequestSubscription(pattern); err != nil {
			b.log.Warn("failed to request subscription", "pattern", pattern, "error", err)
		}
	}

	if !blocking {
		aKey := agentKey(agentCtx.GroveID, agentCtx.AgentSlug)
		b.registerActiveTask(taskID, aKey)
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			sendCtx, cancel := context.WithTimeout(b.shutdownCtx, 30*time.Second)
			defer cancel()
			if err := b.hubClient.Agents().SendStructuredMessage(sendCtx, agentCtx.AgentID, scionMsg, false, false); err != nil {
				b.log.Error("non-blocking send failed", "error", err, "task_id", taskID)
				if err := b.store.UpdateTaskState(taskID, TaskStateFailed); err != nil {
					b.log.Error("failed to update task state", "error", err, "task_id", taskID)
				}
				b.unregisterActiveTask(taskID, aKey)
				return
			}
			if err := b.store.UpdateTaskState(taskID, TaskStateWorking); err != nil {
				b.log.Error("failed to update task state", "error", err, "task_id", taskID)
			}
		}()

		return &TaskResult{
			ID:        taskID,
			ContextID: agentCtx.ContextID,
			Status:    TaskStatus{State: TaskStateSubmitted},
		}, nil
	}

	// Blocking mode: set up per-task waiter.
	// Also register in agentTasks so the slug-based fallback correlation works
	// when broker messages arrive without a2aTaskId metadata.
	aKey := agentKey(agentCtx.GroveID, agentCtx.AgentSlug)
	b.registerActiveTask(taskID, aKey)
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: agentCtx.AgentSlug,
		groveID:   agentCtx.GroveID,
	})
	defer b.removeWaiter(taskID)
	defer b.unregisterActiveTask(taskID, aKey)

	if err := b.hubClient.Agents().SendStructuredMessage(ctx, agentCtx.AgentID, scionMsg, false, false); err != nil {
		if err := b.store.UpdateTaskState(taskID, TaskStateFailed); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}
		return nil, fmt.Errorf("send message to agent: %w", err)
	}

	if err := b.store.UpdateTaskState(taskID, TaskStateWorking); err != nil {
		b.log.Error("failed to update task state", "error", err, "task_id", taskID)
	}

	timeout := b.config.Timeouts.SendMessage
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case response := <-responseCh:
		msg, artifacts := TranslateScionToA2A(response)
		if err := b.store.UpdateTaskState(taskID, TaskStateCompleted); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}

		return &TaskResult{
			ID:        taskID,
			ContextID: agentCtx.ContextID,
			Status: TaskStatus{
				State:   TaskStateCompleted,
				Message: &msg,
			},
			Artifacts: artifacts,
		}, nil

	case <-timer.C:
		if err := b.store.UpdateTaskState(taskID, TaskStateFailed); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}
		return nil, fmt.Errorf("timeout waiting for agent response after %v", timeout)

	case <-ctx.Done():
		if err := b.store.UpdateTaskState(taskID, TaskStateFailed); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}
		return nil, ctx.Err()
	}
}

// GetTask retrieves a task by ID.
func (b *Bridge) GetTask(ctx context.Context, taskID string) (*TaskResult, error) {
	task, err := b.store.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	return &TaskResult{
		ID:        task.ID,
		ContextID: task.ContextID,
		Status: TaskStatus{
			State: task.State,
		},
	}, nil
}

// ListTasks returns tasks for a given context.
func (b *Bridge) ListTasks(ctx context.Context, contextID string) ([]TaskResult, error) {
	tasks, err := b.store.ListTasksByContext(ctx, contextID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}

	results := make([]TaskResult, len(tasks))
	for i, t := range tasks {
		results[i] = TaskResult{
			ID:        t.ID,
			ContextID: t.ContextID,
			Status:    TaskStatus{State: t.State},
		}
	}
	return results, nil
}

// CancelTask cancels an in-progress task, notifying stream and push subscribers,
// and sending an interrupt to the Hub to stop the agent.
func (b *Bridge) CancelTask(ctx context.Context, taskID string) (*TaskResult, error) {
	task, err := b.store.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, nil
	}
	if IsTerminalState(task.State) {
		return nil, fmt.Errorf("task %s is already in terminal state: %s", taskID, task.State)
	}

	// Unregister before updating state so concurrent broker messages cannot
	// overwrite the canceled state via dispatchToActiveTask.
	aKey := agentKey(task.GroveID, task.AgentSlug)
	b.unregisterActiveTask(taskID, aKey)

	// Send interrupt to the agent via Hub, re-resolving if the stored AgentID is stale.
	if b.hubClient != nil && task.AgentID != "" {
		targetAgentID := task.AgentID
		if agent := b.lookupAgent(ctx, task.GroveID, task.AgentSlug); agent != nil {
			targetAgentID = agent.ID
		}
		interruptMsg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    fmt.Sprintf("user:%s", b.config.Hub.User),
			Recipient: fmt.Sprintf("agent:%s", task.AgentSlug),
			Msg:       "Task cancelled by A2A client.",
			Type:      messages.TypeInstruction,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if err := b.hubClient.Agents().SendStructuredMessage(ctx, targetAgentID, interruptMsg, true, false); err != nil {
			b.log.Error("failed to send cancel interrupt to agent", "error", err, "task_id", taskID, "agent_id", targetAgentID)
		}
	}

	if err := b.store.UpdateTaskState(taskID, TaskStateCanceled); err != nil {
		b.log.Error("failed to update task state", "error", err, "task_id", taskID)
	}
	if b.metrics != nil {
		b.metrics.TasksCompleted.WithLabelValues(TaskStateCanceled).Inc()
	}

	cancelEvent := StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateCanceled},
			Final:  true,
		},
	}
	b.streams.Broadcast(taskID, cancelEvent)
	b.push.Dispatch(ctx, taskID, cancelEvent)

	b.streams.CloseAll(taskID)

	return &TaskResult{
		ID:        task.ID,
		ContextID: task.ContextID,
		Status:    TaskStatus{State: TaskStateCanceled},
	}, nil
}

// HandleBrokerMessage enqueues a broker message for async dispatch, keeping
// the broker's Publish RPC non-blocking. Returns an error only if the queue
// is full or shutdown has begun, signalling backpressure to the Hub.
func (b *Bridge) HandleBrokerMessage(ctx context.Context, topic string, msg *messages.StructuredMessage) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("broker shutting down")
		}
	}()
	select {
	case b.brokerMsgs <- brokerMessage{topic: topic, msg: msg}:
		return nil
	default:
		b.log.Error("broker message queue full, dropping message", "topic", topic)
		return fmt.Errorf("broker message queue full")
	}
}

// brokerWorker drains the brokerMsgs channel, dispatching each message.
// Shutdown closes the channel; the range loop drains buffered messages
// with shutdownCtx still live so push.Dispatch works during drain.
func (b *Bridge) brokerWorker() {
	defer b.wg.Done()
	for bm := range b.brokerMsgs {
		b.dispatchBrokerMessage(bm.topic, bm.msg)
	}
}

func (b *Bridge) dispatchBrokerMessage(topic string, msg *messages.StructuredMessage) {
	b.log.Debug("handling broker message",
		"topic", topic,
		"sender", msg.Sender,
		"type", msg.Type,
		"msg_preview", truncate(msg.Msg, 100),
	)

	agentSlug := extractAgentIDFromSender(msg.Sender)
	if agentSlug == "" {
		b.log.Warn("ignoring message: sender does not use agent:<slug> format, dropping", "topic", topic, "sender", msg.Sender)
		return
	}

	groveID := extractGroveIDFromTopic(topic)
	if groveID == "" {
		b.log.Warn("dropping message with unparseable grove ID", "topic", topic)
		return
	}

	ctx := b.shutdownCtx

	// If the message carries a task correlation ID, dispatch only to that task
	// after verifying the message's agent matches the task's owner.
	if taskID := msg.Metadata["a2aTaskId"]; taskID != "" {
		task, err := b.store.GetTask(taskID)
		if err != nil || task == nil {
			b.log.Debug("ignoring message for unknown task", "task_id", taskID)
			return
		}
		if task.AgentSlug != agentSlug {
			b.log.Warn("dropping cross-agent a2aTaskId injection",
				"task_agent", task.AgentSlug, "msg_agent", agentSlug, "task_id", taskID)
			return
		}

		if b.dispatchToWaiter(taskID, msg) {
			return
		}
		b.tasksMu.RLock()
		_, isActive := b.activeTasks[taskID]
		b.tasksMu.RUnlock()
		if isActive {
			b.dispatchToActiveTask(ctx, taskID, agentSlug, msg)
		}
		return
	}

	// No a2aTaskId — the outbound message path (sciontool stop hook →
	// SendOutboundMessage) does not carry metadata from the original inbound
	// message, so a2aTaskId is lost in the round-trip. Fall back to agent-slug
	// correlation using the agentTasks reverse map.
	aKey := agentKey(groveID, agentSlug)
	b.tasksMu.RLock()
	taskIDs := append([]string(nil), b.agentTasks[aKey]...)
	b.tasksMu.RUnlock()

	if len(taskIDs) == 0 {
		b.log.Warn("dropping broker message: no a2aTaskId and no active tasks for agent",
			"topic", topic, "sender", msg.Sender, "grove", groveID, "agent", agentSlug)
		return
	}

	b.log.Debug("correlating broker message by agent slug (no a2aTaskId)",
		"agent", agentSlug, "grove", groveID, "active_tasks", len(taskIDs))
	for _, taskID := range taskIDs {
		if b.dispatchToWaiter(taskID, msg) {
			continue
		}
		b.tasksMu.RLock()
		_, isActive := b.activeTasks[taskID]
		b.tasksMu.RUnlock()
		if isActive {
			b.dispatchToActiveTask(ctx, taskID, agentSlug, msg)
		}
	}
}

// dispatchToWaiter sends a message to a blocking waiter for the given taskID.
// Returns true if a waiter exists and handled the message (callers should skip
// further dispatch). State-change messages are skipped so the actual reply
// lands in the buffer.
func (b *Bridge) dispatchToWaiter(taskID string, msg *messages.StructuredMessage) bool {
	b.mu.RLock()
	w, ok := b.waiters[taskID]
	b.mu.RUnlock()
	if !ok {
		return false
	}
	if msg.Type == messages.TypeStateChange {
		return true
	}
	select {
	case w.ch <- msg:
	default:
		b.log.Debug("dropping duplicate response for blocking waiter", "task_id", taskID)
	}
	return true
}

// dispatchToActiveTask routes a broker message to streaming/push subscribers for a task.
func (b *Bridge) dispatchToActiveTask(ctx context.Context, taskID, agentSlug string, msg *messages.StructuredMessage) {
	a2aMsg, artifacts := TranslateScionToA2A(msg)

	if msg.Type == messages.TypeStateChange {
		taskState := MapActivityToTaskState(msg.Msg)
		if err := b.store.UpdateTaskState(taskID, taskState); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}

		event := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: taskID,
				Status: TaskStatus{State: taskState},
				Final:  IsTerminalState(taskState),
			},
		}
		b.streams.Broadcast(taskID, event)
		b.push.Dispatch(ctx, taskID, event)

		if IsTerminalState(taskState) {
			if b.metrics != nil {
				b.metrics.TasksCompleted.WithLabelValues(taskState).Inc()
			}
			b.tasksMu.RLock()
			aKey := b.activeTasks[taskID].aKey
			b.tasksMu.RUnlock()
			b.unregisterActiveTask(taskID, aKey)
		}
	} else {
		// TODO(multi-turn): MVP limitation — treats any non-state-change message as
		// a terminal response. Multi-turn agents that emit interim content (e.g.
		// clarifying questions, progress updates) will have their task closed
		// prematurely on the first content message. This breaks agents that use
		// input-required → completed flows. Must be fixed before exposing
		// non-trivial agent types.
		b.log.Debug("treating content message as task completion (MVP)", "task_id", taskID)
		if err := b.store.UpdateTaskState(taskID, TaskStateCompleted); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}

		for _, art := range artifacts {
			artEvent := StreamEvent{
				ArtifactUpdate: &TaskArtifactUpdate{
					TaskID:   taskID,
					Artifact: art,
				},
			}
			b.streams.Broadcast(taskID, artEvent)
			b.push.Dispatch(ctx, taskID, artEvent)
		}

		statusEvent := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: taskID,
				Status: TaskStatus{
					State:   TaskStateCompleted,
					Message: &a2aMsg,
				},
				Final: true,
			},
		}
		b.streams.Broadcast(taskID, statusEvent)
		b.push.Dispatch(ctx, taskID, statusEvent)

		if b.metrics != nil {
			b.metrics.TasksCompleted.WithLabelValues(TaskStateCompleted).Inc()
		}
		b.tasksMu.RLock()
		aKey := b.activeTasks[taskID].aKey
		b.tasksMu.RUnlock()
		b.unregisterActiveTask(taskID, aKey)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// GenerateAgentCard builds an agent card for the given grove and agent,
// enriching it with metadata from the Hub API when available.
func (b *Bridge) GenerateAgentCard(ctx context.Context, groveSlug, agentSlug string) map[string]interface{} {
	baseURL := strings.TrimRight(b.config.Bridge.ExternalURL, "/")
	agentURL := fmt.Sprintf("%s/groves/%s/agents/%s", baseURL, groveSlug, agentSlug)

	name := agentSlug
	description := fmt.Sprintf("Scion agent %s in grove %s", agentSlug, groveSlug)
	var skills []map[string]interface{}

	if agent := b.lookupAgent(ctx, groveSlug, agentSlug); agent != nil {
		if agent.Name != "" {
			name = agent.Name
		}
		if desc, ok := agent.Annotations["description"]; ok && desc != "" {
			description = desc
		} else if agent.TaskSummary != "" {
			description = agent.TaskSummary
		}
		if agent.Labels != nil {
			for k, v := range agent.Labels {
				if strings.HasPrefix(k, "skill/") {
					skills = append(skills, map[string]interface{}{
						"id":          strings.TrimPrefix(k, "skill/"),
						"name":        strings.TrimPrefix(k, "skill/"),
						"description": v,
					})
				}
			}
		}
	}

	if len(skills) == 0 {
		skills = []map[string]interface{}{
			{
				"id":          agentSlug,
				"name":        name,
				"description": fmt.Sprintf("Interact with agent %s", name),
			},
		}
	}

	card := map[string]interface{}{
		"name":        name,
		"description": description,
		"url":         agentURL,
		"version":     "1.0.0",
		"capabilities": map[string]bool{
			"streaming":         false,
			"pushNotifications": false,
		},
		"defaultInputModes":  []string{"text/plain", "application/json"},
		"defaultOutputModes": []string{"text/plain", "application/json"},
		"skills":             skills,
	}

	if b.config.Bridge.Provider.Organization != "" {
		card["provider"] = map[string]string{
			"organization": b.config.Bridge.Provider.Organization,
			"url":          b.config.Bridge.Provider.URL,
		}
	}

	return card
}

// lookupAgent fetches agent metadata from the Hub API, returning nil on failure.
// Results are cached for agentCacheTTL to avoid listing all agents on every call.
func (b *Bridge) lookupAgent(ctx context.Context, groveSlug, agentSlug string) *hubclient.Agent {
	cacheKey := groveSlug + ":" + agentSlug

	b.agentCacheMu.RLock()
	if entry, ok := b.agentCache[cacheKey]; ok && time.Since(entry.cachedAt) < agentCacheTTL {
		b.agentCacheMu.RUnlock()
		return entry.agent
	}
	b.agentCacheMu.RUnlock()

	if b.hubClient == nil {
		return nil
	}
	agentSvc := b.hubClient.Agents()
	if agentSvc == nil {
		return nil
	}
	agents, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{GroveID: groveSlug})
	if err != nil {
		b.log.Debug("failed to list agents for card enrichment", "error", err)
		return nil
	}

	var result *hubclient.Agent
	for _, a := range agents.Agents {
		if a.Name == agentSlug || a.Slug == agentSlug {
			agentCopy := a
			result = &agentCopy
			break
		}
	}

	b.agentCacheMu.Lock()
	b.agentCache[cacheKey] = &agentCacheEntry{agent: result, cachedAt: time.Now()}
	b.agentCacheMu.Unlock()

	return result
}

func (b *Bridge) evictStaleAgentCache() {
	cutoff := 2 * agentCacheTTL
	b.agentCacheMu.Lock()
	for key, entry := range b.agentCache {
		if time.Since(entry.cachedAt) >= cutoff {
			delete(b.agentCache, key)
		}
	}
	b.agentCacheMu.Unlock()
}

// GetGroveConfig returns the configuration for a grove slug, or nil if not configured.
// Returns a pointer to a copy to avoid aliasing the live config slice.
func (b *Bridge) GetGroveConfig(groveSlug string) *GroveConfig {
	for i := range b.config.Groves {
		if b.config.Groves[i].Slug == groveSlug {
			cfg := b.config.Groves[i]
			return &cfg
		}
	}
	return nil
}

// resolveContext maps an A2A context to a Scion agent, creating a new context if needed.
func (b *Bridge) resolveContext(ctx context.Context, groveSlug, agentSlug, contextID string) (*state.Context, error) {
	if contextID != "" {
		existing, err := b.store.GetContext(contextID)
		if err != nil {
			return nil, fmt.Errorf("get context: %w", err)
		}
		if existing != nil {
			if existing.GroveID != groveSlug || existing.AgentSlug != agentSlug {
				return nil, fmt.Errorf("%w: context does not belong to %s/%s", ErrContextUnknown, groveSlug, agentSlug)
			}
			if err := b.store.TouchContext(contextID); err != nil {
				b.log.Error("failed to touch context", "context_id", contextID, "error", err)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrContextUnknown, contextID)
	}

	agents, err := b.hubClient.Agents().List(ctx, &hubclient.ListAgentsOptions{GroveID: groveSlug})
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	var agentID, groveID string
	for _, a := range agents.Agents {
		if a.Name == agentSlug || a.Slug == agentSlug {
			agentID = a.ID
			groveID = a.GroveID
			break
		}
	}
	if agentID == "" {
		groveCfg := b.GetGroveConfig(groveSlug)
		if groveCfg == nil || !groveCfg.AutoProvision || groveCfg.DefaultTemplate == "" {
			return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, agentSlug)
		}

		b.log.Info("auto-provisioning agent", "slug", agentSlug, "grove", groveSlug, "template", groveCfg.DefaultTemplate)
		created, err := b.hubClient.Agents().Create(ctx, &hubclient.CreateAgentRequest{
			Name:     agentSlug,
			GroveID:  groveSlug,
			Template: groveCfg.DefaultTemplate,
			Labels:   map[string]string{"a2a-bridge/auto-provisioned": "true"},
		})
		if err != nil {
			// Concurrent create may have succeeded; re-list to find the agent.
			retryAgents, retryErr := b.hubClient.Agents().List(ctx, &hubclient.ListAgentsOptions{GroveID: groveSlug})
			if retryErr != nil {
				return nil, fmt.Errorf("auto-provision agent %q: %w", agentSlug, err)
			}
			found := false
			for _, a := range retryAgents.Agents {
				if a.Name == agentSlug || a.Slug == agentSlug {
					agentID = a.ID
					groveID = a.GroveID
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("auto-provision agent %q: %w", agentSlug, err)
			}
		} else {
			agentID = created.Agent.ID
			groveID = created.Agent.GroveID
		}
	}
	if groveID == "" {
		groveID = groveSlug
	}

	newContextID := uuid.New().String()
	now := time.Now()
	agentCtx := &state.Context{
		ContextID:  newContextID,
		GroveID:    groveID,
		AgentSlug:  agentSlug,
		AgentID:    agentID,
		CreatedAt:  now,
		LastActive: now,
	}
	if err := b.store.CreateContext(agentCtx); err != nil {
		return nil, fmt.Errorf("create context: %w", err)
	}

	return agentCtx, nil
}

func (b *Bridge) registerActiveTask(taskID, aKey string) {
	b.tasksMu.Lock()
	defer b.tasksMu.Unlock()
	b.activeTasks[taskID] = activeTaskEntry{aKey: aKey, createdAt: time.Now()}
	b.agentTasks[aKey] = append(b.agentTasks[aKey], taskID)
}

func (b *Bridge) unregisterActiveTask(taskID, aKey string) {
	b.tasksMu.Lock()
	defer b.tasksMu.Unlock()
	delete(b.activeTasks, taskID)
	tasks := b.agentTasks[aKey]
	for i, t := range tasks {
		if t == taskID {
			b.agentTasks[aKey] = append(tasks[:i], tasks[i+1:]...)
			break
		}
	}
	if len(b.agentTasks[aKey]) == 0 {
		delete(b.agentTasks, aKey)
	}
}

func (b *Bridge) addWaiter(taskID string, w *waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.waiters[taskID] = w
}

func (b *Bridge) removeWaiter(taskID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.waiters, taskID)
}

// parseTopic extracts grove and agent identifiers from a broker topic string.
// Expected format: scion.grove.<groveID>.user.<user>.messages (6 segments).
// The 5-segment agent form (scion.grove.<g>.agent.<a>) is parsed but currently
// unused — the bridge only subscribes to user-scoped topics.
func parseTopic(topic string) (groveID, agentSlug string, err error) {
	parts := strings.Split(topic, ".")
	if len(parts) < 3 || parts[0] != "scion" || parts[1] != "grove" {
		return "", "", fmt.Errorf("malformed topic: %s", topic)
	}
	groveID = parts[2]
	if len(parts) >= 5 && parts[3] == "agent" {
		agentSlug = parts[4]
	}
	return groveID, agentSlug, nil
}

func extractGroveIDFromTopic(topic string) string {
	groveID, _, _ := parseTopic(topic)
	return groveID
}

// AuthorizeExposed returns nil if the grove is configured and the agent
// is exposed (or no allowlist is set). Returns ErrAgentNotFound to avoid
// leaking grove existence.
func (b *Bridge) AuthorizeExposed(groveSlug, agentSlug string) error {
	g := b.GetGroveConfig(groveSlug)
	if g == nil {
		return ErrAgentNotFound
	}
	if len(g.ExposedAgents) == 0 {
		return nil
	}
	for _, a := range g.ExposedAgents {
		if a == agentSlug {
			return nil
		}
	}
	return ErrAgentNotFound
}

// AuthorizeTask verifies a task belongs to the given grove and agent.
// Returns nil (not an error) if the task doesn't exist or doesn't match,
// so callers can return "not found" without leaking existence.
func (b *Bridge) AuthorizeTask(taskID, groveSlug, agentSlug string) (*state.Task, error) {
	task, err := b.store.GetTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil || task.GroveID != groveSlug || task.AgentSlug != agentSlug {
		return nil, nil
	}
	return task, nil
}

// AuthorizeContext verifies a context belongs to the given grove and agent.
// Returns (true, nil) on success, (false, nil) when the context doesn't exist
// or doesn't match, and (false, err) on database errors.
func (b *Bridge) AuthorizeContext(contextID, groveSlug, agentSlug string) (bool, error) {
	ctx, err := b.store.GetContext(contextID)
	if err != nil {
		return false, fmt.Errorf("get context: %w", err)
	}
	if ctx == nil {
		return false, nil
	}
	return ctx.GroveID == groveSlug && ctx.AgentSlug == agentSlug, nil
}

// extractAgentIDFromSender extracts agent identity from sender field.
func extractAgentIDFromSender(sender string) string {
	if strings.HasPrefix(sender, "agent:") {
		return strings.TrimPrefix(sender, "agent:")
	}
	return ""
}
