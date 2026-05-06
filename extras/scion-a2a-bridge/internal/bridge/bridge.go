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
	log       *slog.Logger

	// waiters tracks channels waiting for agent responses, keyed by taskID.
	mu      sync.RWMutex
	waiters map[string]*waiter

	// activeTasks maps taskID to the agentKey (groveID:agentSlug) for routing broker messages.
	tasksMu     sync.RWMutex
	activeTasks map[string]string

	// agentTasks maps agentKey (groveID:agentSlug) to active task IDs,
	// used for reverse lookup when broker messages arrive.
	agentTasks map[string][]string

	// wg tracks background goroutines to drain on shutdown.
	wg sync.WaitGroup

	// shutdownCtx is cancelled during graceful shutdown.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// New creates a new Bridge instance.
func New(store *state.Store, hubClient hubclient.Client, minter *identity.TokenMinter, cfg *Config, log *slog.Logger) *Bridge {
	ctx, cancel := context.WithCancel(context.Background())
	return &Bridge{
		store:          store,
		hubClient:      hubClient,
		minter:         minter,
		config:         cfg,
		log:            log,
		streams:        NewStreamManager(),
		push:           NewPushDispatcher(store, cfg, log, ctx),
		waiters:        make(map[string]*waiter),
		activeTasks:    make(map[string]string),
		agentTasks:     make(map[string][]string),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
}

// Shutdown signals background goroutines to stop and waits for them to drain.
func (b *Bridge) Shutdown() {
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
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(taskID, &waiter{
		ch:        responseCh,
		agentSlug: agentCtx.AgentSlug,
		groveID:   agentCtx.GroveID,
	})
	defer b.removeWaiter(taskID)

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

	case <-time.After(timeout):
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
	tasks, err := b.store.ListTasksByContext(contextID)
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

	// Send interrupt to the agent via Hub.
	if b.hubClient != nil && task.AgentID != "" {
		interruptMsg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    fmt.Sprintf("user:%s", b.config.Hub.User),
			Recipient: fmt.Sprintf("agent:%s", task.AgentSlug),
			Msg:       "Task cancelled by A2A client.",
			Type:      messages.TypeInstruction,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if err := b.hubClient.Agents().SendStructuredMessage(ctx, task.AgentID, interruptMsg, true, false); err != nil {
			b.log.Error("failed to send cancel interrupt to agent", "error", err, "task_id", taskID, "agent_id", task.AgentID)
		}
	}

	if err := b.store.UpdateTaskState(taskID, TaskStateCanceled); err != nil {
		b.log.Error("failed to update task state", "error", err, "task_id", taskID)
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

	aKey := agentKey(task.GroveID, task.AgentSlug)
	b.unregisterActiveTask(taskID, aKey)
	b.streams.CloseAll(taskID)

	return &TaskResult{
		ID:        task.ID,
		ContextID: task.ContextID,
		Status:    TaskStatus{State: TaskStateCanceled},
	}, nil
}

// HandleBrokerMessage processes an inbound message from the broker plugin.
func (b *Bridge) HandleBrokerMessage(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.log.Info("handling broker message",
		"topic", topic,
		"sender", msg.Sender,
		"type", msg.Type,
		"msg_preview", truncate(msg.Msg, 100),
	)

	agentSlug := extractAgentIDFromSender(msg.Sender)
	if agentSlug == "" {
		agentSlug = extractAgentIDFromTopic(topic)
	}
	if agentSlug == "" {
		b.log.Debug("ignoring message: could not determine agent slug", "topic", topic, "sender", msg.Sender)
		return nil
	}

	// If the message carries a task correlation ID, dispatch only to that task.
	if taskID := msg.Metadata["a2aTaskId"]; taskID != "" {
		b.dispatchToWaiter(taskID, msg)
		b.dispatchToActiveTask(ctx, taskID, agentSlug, msg)
		return nil
	}

	// Fallback: dispatch to blocking waiters that match this agent (best-effort without correlation).
	groveID := extractGroveIDFromTopic(topic)
	b.mu.RLock()
	var matchedTaskIDs []string
	for taskID, w := range b.waiters {
		if w.agentSlug == agentSlug && (groveID == "" || w.groveID == groveID) {
			matchedTaskIDs = append(matchedTaskIDs, taskID)
		}
	}
	b.mu.RUnlock()

	for _, taskID := range matchedTaskIDs {
		b.dispatchToWaiter(taskID, msg)
	}

	// Dispatch to all active (non-blocking/streaming) tasks for this agent
	// using the indexed reverse map for O(1) lookup.
	aKey := agentKey(groveID, agentSlug)
	b.tasksMu.RLock()
	activeTaskIDs := make([]string, len(b.agentTasks[aKey]))
	copy(activeTaskIDs, b.agentTasks[aKey])
	b.tasksMu.RUnlock()

	for _, taskID := range activeTaskIDs {
		b.dispatchToActiveTask(ctx, taskID, agentSlug, msg)
	}

	return nil
}

// dispatchToWaiter sends a message to a blocking waiter for the given taskID.
// State-change messages are skipped so the actual reply lands in the buffer.
func (b *Bridge) dispatchToWaiter(taskID string, msg *messages.StructuredMessage) {
	if msg.Type == messages.TypeStateChange {
		return
	}
	b.mu.RLock()
	w, ok := b.waiters[taskID]
	b.mu.RUnlock()
	if !ok {
		return
	}
	select {
	case w.ch <- msg:
	default:
	}
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
			// Find the agent key for this task.
			b.tasksMu.RLock()
			aKey := b.activeTasks[taskID]
			b.tasksMu.RUnlock()
			b.unregisterActiveTask(taskID, aKey)
		}
	} else {
		// MVP: treat any non-state-change message as a terminal response.
		// A multi-turn agent may send interim content that shouldn't complete the task.
		b.log.Debug("treating content message as task completion", "task_id", taskID)
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

		b.tasksMu.RLock()
		aKey := b.activeTasks[taskID]
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
			"streaming":         true,
			"pushNotifications": true,
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
// Uses GroveID filter to avoid listing all agents.
func (b *Bridge) lookupAgent(ctx context.Context, groveSlug, agentSlug string) *hubclient.Agent {
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
	for _, a := range agents.Agents {
		if a.Name == agentSlug || a.Slug == agentSlug {
			return &a
		}
	}
	return nil
}

// GetGroveConfig returns the configuration for a grove slug, or nil if not configured.
func (b *Bridge) GetGroveConfig(groveSlug string) *GroveConfig {
	for i := range b.config.Groves {
		if b.config.Groves[i].Slug == groveSlug {
			return &b.config.Groves[i]
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
			b.store.TouchContext(contextID)
			return existing, nil
		}
		return nil, fmt.Errorf("unknown context ID: %s", contextID)
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
			return nil, fmt.Errorf("agent %q not found", agentSlug)
		}

		b.log.Info("auto-provisioning agent", "slug", agentSlug, "grove", groveSlug, "template", groveCfg.DefaultTemplate)
		created, err := b.hubClient.Agents().Create(ctx, &hubclient.CreateAgentRequest{
			Name:     agentSlug,
			GroveID:  groveSlug,
			Template: groveCfg.DefaultTemplate,
			Labels:   map[string]string{"a2a-bridge/auto-provisioned": "true"},
		})
		if err != nil {
			return nil, fmt.Errorf("auto-provision agent %q: %w", agentSlug, err)
		}
		agentID = created.Agent.ID
		groveID = created.Agent.GroveID
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
	b.activeTasks[taskID] = aKey
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

// extractAgentIDFromTopic parses agent identity from broker topic strings.
func extractAgentIDFromTopic(topic string) string {
	parts := strings.Split(topic, ".")
	if len(parts) < 5 {
		return ""
	}
	if parts[0] == "scion" && parts[1] == "grove" && parts[3] == "agent" {
		return parts[4]
	}
	return ""
}

// extractGroveIDFromTopic parses grove identity from broker topic strings.
func extractGroveIDFromTopic(topic string) string {
	parts := strings.Split(topic, ".")
	if len(parts) >= 3 && parts[0] == "scion" && parts[1] == "grove" {
		return parts[2]
	}
	return ""
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
func (b *Bridge) AuthorizeContext(contextID, groveSlug, agentSlug string) bool {
	ctx, err := b.store.GetContext(contextID)
	if err != nil || ctx == nil {
		return false
	}
	return ctx.GroveID == groveSlug && ctx.AgentSlug == agentSlug
}

// extractAgentIDFromSender extracts agent identity from sender field.
func extractAgentIDFromSender(sender string) string {
	if strings.HasPrefix(sender, "agent:") {
		return strings.TrimPrefix(sender, "agent:")
	}
	return ""
}
