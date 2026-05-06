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

	// waiters tracks channels waiting for agent responses, keyed by agent slug.
	mu      sync.RWMutex
	waiters map[string][]chan *messages.StructuredMessage

	// activeTasks maps agent slugs to their active (non-terminal) task IDs,
	// used to route broker messages to streaming and push subscribers.
	tasksMu     sync.RWMutex
	activeTasks map[string][]string
}

// New creates a new Bridge instance.
func New(store *state.Store, hubClient hubclient.Client, minter *identity.TokenMinter, cfg *Config, log *slog.Logger) *Bridge {
	return &Bridge{
		store:       store,
		hubClient:   hubClient,
		minter:      minter,
		config:      cfg,
		log:         log,
		streams:     NewStreamManager(),
		push:        NewPushDispatcher(store, cfg, log),
		waiters:     make(map[string][]chan *messages.StructuredMessage),
		activeTasks: make(map[string][]string),
	}
}

// SetBroker wires the broker server for subscription management.
func (b *Bridge) SetBroker(broker *BrokerServer) {
	b.broker = broker
}

// SendMessage handles an A2A SendMessage. When blocking is true (the default),
// it waits for the agent response. When blocking is false, it returns immediately
// after submitting the message and the client can poll via GetTask or subscribe.
func (b *Bridge) SendMessage(ctx context.Context, groveSlug, agentSlug, contextID string, parts []Part, blocking bool) (*TaskResult, error) {
	// Resolve context to agent.
	agentCtx, err := b.resolveContext(ctx, groveSlug, agentSlug, contextID)
	if err != nil {
		return nil, fmt.Errorf("resolve context: %w", err)
	}

	// Create task record.
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

	// Translate A2A parts to Scion message.
	scionMsg := TranslateA2AToScion(parts)
	scionMsg.Sender = fmt.Sprintf("user:%s", b.config.Hub.User)
	scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)

	// Ensure subscription exists for this agent.
	if b.broker != nil {
		pattern := fmt.Sprintf("scion.grove.%s.user.>", agentCtx.GroveID)
		if err := b.broker.RequestSubscription(pattern); err != nil {
			b.log.Warn("failed to request subscription", "pattern", pattern, "error", err)
		}
	}

	// Non-blocking mode: submit and return immediately.
	if !blocking {
		b.registerActiveTask(taskID, agentCtx.AgentSlug)
		go func() {
			sendCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := b.hubClient.Agents().SendStructuredMessage(sendCtx, agentCtx.AgentID, scionMsg, false, false); err != nil {
				b.log.Error("non-blocking send failed", "error", err, "task_id", taskID)
				b.store.UpdateTaskState(taskID, TaskStateFailed)
				b.unregisterActiveTask(taskID, agentCtx.AgentSlug)
				return
			}
			b.store.UpdateTaskState(taskID, TaskStateWorking)
		}()

		return &TaskResult{
			ID:        taskID,
			ContextID: agentCtx.ContextID,
			Status:    TaskStatus{State: TaskStateSubmitted},
		}, nil
	}

	// Blocking mode: set up waiter and wait for response.
	responseCh := make(chan *messages.StructuredMessage, 1)
	b.addWaiter(agentCtx.AgentSlug, responseCh)
	defer b.removeWaiter(agentCtx.AgentSlug, responseCh)

	// Send message to agent via Hub API.
	if err := b.hubClient.Agents().SendStructuredMessage(ctx, agentCtx.AgentID, scionMsg, false, false); err != nil {
		b.store.UpdateTaskState(taskID, TaskStateFailed)
		return nil, fmt.Errorf("send message to agent: %w", err)
	}

	b.store.UpdateTaskState(taskID, TaskStateWorking)

	// Wait for response with timeout.
	timeout := b.config.Timeouts.SendMessage
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	select {
	case response := <-responseCh:
		msg, artifacts := TranslateScionToA2A(response)
		b.store.UpdateTaskState(taskID, TaskStateCompleted)

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
		b.store.UpdateTaskState(taskID, TaskStateFailed)
		return nil, fmt.Errorf("timeout waiting for agent response after %v", timeout)

	case <-ctx.Done():
		b.store.UpdateTaskState(taskID, TaskStateFailed)
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

// CancelTask cancels an in-progress task, notifying stream and push subscribers.
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

	b.store.UpdateTaskState(taskID, TaskStateCanceled)

	cancelEvent := StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateCanceled},
			Final:  true,
		},
	}
	b.streams.Broadcast(taskID, cancelEvent)
	b.push.Dispatch(ctx, taskID, cancelEvent)
	b.unregisterActiveTask(taskID, task.AgentSlug)
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

	// Extract agent slug from sender field (format: "agent:<slug>").
	// This is more reliable than parsing the topic, which may be a user-targeted
	// topic (scion.grove.<groveId>.user.<userId>.messages) without agent info.
	agentSlug := extractAgentIDFromSender(msg.Sender)
	if agentSlug == "" {
		agentSlug = extractAgentIDFromTopic(topic)
	}
	if agentSlug == "" {
		b.log.Debug("ignoring message: could not determine agent slug", "topic", topic, "sender", msg.Sender)
		return nil
	}

	// Dispatch to blocking waiters (SendMessage).
	b.mu.RLock()
	waiters := b.waiters[agentSlug]
	waiterCount := len(waiters)
	b.mu.RUnlock()

	b.log.Info("dispatching broker message", "agent_slug", agentSlug, "waiter_count", waiterCount)

	for _, ch := range waiters {
		select {
		case ch <- msg:
		default:
		}
	}

	// Dispatch to streaming and push subscribers for active tasks.
	taskIDs := b.getActiveTaskIDs(agentSlug)
	if len(taskIDs) == 0 {
		return nil
	}

	a2aMsg, artifacts := TranslateScionToA2A(msg)

	for _, taskID := range taskIDs {
		if msg.Type == messages.TypeStateChange {
			taskState := MapActivityToTaskState(msg.Msg)
			b.store.UpdateTaskState(taskID, taskState)

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
				b.unregisterActiveTask(taskID, agentSlug)
			}
		} else {
			b.store.UpdateTaskState(taskID, TaskStateCompleted)

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

			b.unregisterActiveTask(taskID, agentSlug)
		}
	}

	return nil
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

	// Fetch agent metadata from Hub for richer card content.
	if agent := b.lookupAgent(ctx, agentSlug); agent != nil {
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
func (b *Bridge) lookupAgent(ctx context.Context, agentSlug string) *hubclient.Agent {
	if b.hubClient == nil {
		return nil
	}
	agentSvc := b.hubClient.Agents()
	if agentSvc == nil {
		return nil
	}
	agents, err := agentSvc.List(ctx, nil)
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
	// If contextID provided, look up existing context.
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

	// No contextID — create a new context.
	// Look up the agent via Hub API.
	agents, err := b.hubClient.Agents().List(ctx, nil)
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
		// Agent not found — try auto-provisioning if grove config allows it.
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

func (b *Bridge) registerActiveTask(taskID, agentSlug string) {
	b.tasksMu.Lock()
	defer b.tasksMu.Unlock()
	b.activeTasks[agentSlug] = append(b.activeTasks[agentSlug], taskID)
}

func (b *Bridge) unregisterActiveTask(taskID, agentSlug string) {
	b.tasksMu.Lock()
	defer b.tasksMu.Unlock()
	tasks := b.activeTasks[agentSlug]
	for i, t := range tasks {
		if t == taskID {
			b.activeTasks[agentSlug] = append(tasks[:i], tasks[i+1:]...)
			break
		}
	}
	if len(b.activeTasks[agentSlug]) == 0 {
		delete(b.activeTasks, agentSlug)
	}
}

func (b *Bridge) getActiveTaskIDs(agentSlug string) []string {
	b.tasksMu.RLock()
	defer b.tasksMu.RUnlock()
	return append([]string(nil), b.activeTasks[agentSlug]...)
}

func (b *Bridge) addWaiter(agentID string, ch chan *messages.StructuredMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.waiters[agentID] = append(b.waiters[agentID], ch)
}

func (b *Bridge) removeWaiter(agentID string, ch chan *messages.StructuredMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	waiters := b.waiters[agentID]
	for i, w := range waiters {
		if w == ch {
			b.waiters[agentID] = append(waiters[:i], waiters[i+1:]...)
			break
		}
	}
	if len(b.waiters[agentID]) == 0 {
		delete(b.waiters, agentID)
	}
}

// extractAgentIDFromTopic parses agent identity from broker topic strings.
// Topic format: scion.grove.<groveId>.agent.<agentSlug>.messages
// Or: scion.grove.<groveId>.user.<userId>.messages
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

// extractAgentIDFromSender extracts agent identity from sender field.
// Sender format: "agent:<slug>" or "agent:<id>"
func extractAgentIDFromSender(sender string) string {
	if strings.HasPrefix(sender, "agent:") {
		return strings.TrimPrefix(sender, "agent:")
	}
	return ""
}
