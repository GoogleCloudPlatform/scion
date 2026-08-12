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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

var (
	ErrAgentNotFound  = errors.New("agent not found")
	ErrContextUnknown = errors.New("unknown context ID")
	ErrTaskTerminal   = errors.New("task is in a terminal state")
	ErrTimeout        = errors.New("timeout waiting for agent response")
)

// Bridge is the core bridge logic that ties together state management,
// hub client operations, and message translation.
type Bridge struct {
	store     state.Store
	hubClient hubclient.Client
	minter    *identity.TokenMinter
	config    *Config
	snapshot  *SnapshotHolder // atomic snapshot of effective config (hot-apply)
	broker    *BrokerServer
	streams   *StreamManager
	push      *PushDispatcher
	metrics   *Metrics
	log       *slog.Logger

	// sdkRequestHandler holds the SDK RequestHandler for multi-transport use (gRPC, REST).
	sdkRequestHandler a2asrv.RequestHandler

	// activeTasks is a best-effort local cache mapping taskID to routing/lifecycle
	// metadata. On cache miss, the DB is queried via FindActiveTaskForAgent.
	tasksMu     sync.RWMutex
	activeTasks map[string]activeTaskEntry

	// agentTasks maps agentKey (projectID:agentSlug) to active task IDs,
	// used as a local cache for reverse lookup when broker messages arrive.
	agentTasks map[string][]string

	// lastSweptAt is a unix timestamp used by maybeOpportunisticSweep to
	// throttle opportunistic sweeps to at most once per interval per instance.
	lastSweptAt atomic.Int64

	// wg tracks background goroutines to drain on shutdown.
	wg sync.WaitGroup

	// notifier accelerates event delivery via PostgreSQL NOTIFY/LISTEN.
	// nil in plugin/SQLite mode; polling remains the correctness floor.
	notifier *Notifier

	// oidcCache caches proxied OIDC discovery and JWKS responses from the hub.
	oidcCache *oidcProxyCache

	// oidcClientOnce ensures the shared OIDC HTTP client is created exactly once.
	oidcClientOnce sync.Once
	// oidcClient is the cached HTTP client for reaching the hub's OIDC endpoints.
	oidcClient *http.Client

	// agentCache caches lookupAgent results to avoid listing all agents per call.
	agentCacheMu sync.RWMutex
	agentCache   map[string]*agentCacheEntry

	// transportSrc and transportMode hold the resolved transport-layer OIDC
	// auth (IAP / Cloud Run invoker). Stored so callerHubClient can compose
	// transport auth into per-caller hub clients.
	transportSrc  transportauth.TokenSource
	transportMode transportauth.HeaderMode

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

// BridgeOption configures optional Bridge behaviour.
type BridgeOption func(*Bridge)

// WithTransportAuth stores the resolved transport-layer OIDC auth so that
// per-caller hub clients include it in their HTTP transport chain.
func WithTransportAuth(src transportauth.TokenSource, mode transportauth.HeaderMode) BridgeOption {
	return func(b *Bridge) {
		b.transportSrc = src
		b.transportMode = mode
	}
}

// New creates a new Bridge instance. Options (e.g. WithTransportAuth) are
// applied after construction.
func New(store state.Store, hubClient hubclient.Client, minter *identity.TokenMinter, cfg *Config, metrics *Metrics, log *slog.Logger, opts ...BridgeOption) *Bridge {
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
		activeTasks:    make(map[string]activeTaskEntry),
		agentTasks:     make(map[string][]string),
		oidcCache:      newOIDCProxyCache(oidcProxyCacheTTL),
		agentCache:     make(map[string]*agentCacheEntry),
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
	}
	for _, opt := range opts {
		opt(b)
	}
	b.wg.Add(1)
	go b.janitor()
	return b
}

// janitor periodically reaps stale active tasks from the database and
// evicts stale agent cache entries.
func (b *Bridge) janitor() {
	defer b.wg.Done()

	maxAge := 2 * b.effectiveConfig().Timeouts.SendMessage
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
			b.reapStaleTasks(b.shutdownCtx, maxAge)
			b.evictStaleAgentCache()
		}
	}
}

// reapStaleTasks queries the DB for stale active tasks and CAS-transitions
// them to failed. Each instance runs this independently; CAS ensures exactly
// one instance wins per task.
func (b *Bridge) reapStaleTasks(ctx context.Context, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	tasks, err := b.store.ListStaleActiveTasks(ctx, cutoff, 100)
	if err != nil {
		b.log.Error("janitor: listing stale tasks", "error", err)
		return
	}
	for _, task := range tasks {
		changed, err := b.store.UpdateTaskState(ctx, task.ID, TaskStateFailed)
		if err != nil {
			b.log.Error("janitor: failing stale task", "taskID", task.ID, "error", err)
			continue
		}
		if changed {
			// We won the CAS — emit the failure event and push.
			b.log.Warn("janitor: reaping stale task", "task_id", task.ID, "age_cutoff", maxAge)
			failPayload, _ := json.Marshal(TaskStatusUpdate{
				TaskID: task.ID,
				Status: TaskStatus{State: TaskStateFailed},
			})
			if _, err := b.store.AppendTaskEvent(ctx, &state.TaskEvent{
				TaskID:  task.ID,
				Kind:    "status",
				Payload: failPayload,
				Final:   true,
			}); err != nil {
				b.log.Error("failed to append task event", "task_id", task.ID, "kind", "status", "error", err)
			}
			failEvent := StreamEvent{
				StatusUpdate: &TaskStatusUpdate{
					TaskID: task.ID,
					Status: TaskStatus{State: TaskStateFailed},
					Final:  true,
				},
			}
			b.push.Dispatch(ctx, task.ID, failEvent)
			if b.metrics != nil {
				b.metrics.TasksCompleted.WithLabelValues(TaskStateFailed).Inc()
			}
		}
		// Unregister from local cache regardless of who won the CAS.
		aKey := agentKey(task.ProjectID, task.AgentSlug)
		b.unregisterActiveTask(task.ID, aKey)
	}
}

// RunSweep performs a full sweep pass: reaps stale tasks and purges old events.
// Safe to call from any goroutine; CAS in the store ensures exactly one
// instance wins per task even under concurrent sweeps.
func (b *Bridge) RunSweep(ctx context.Context) {
	maxAge := 2 * b.effectiveConfig().Timeouts.SendMessage
	if maxAge < 2*time.Minute {
		maxAge = 4 * time.Minute
	}
	b.reapStaleTasks(ctx, maxAge)

	// Purge events older than 1 hour (D8).
	purged, err := b.store.PurgeTaskEvents(ctx, time.Now().Add(-1*time.Hour))
	if err != nil {
		b.log.Error("failed to purge task events", "error", err)
	} else if purged > 0 {
		b.log.Info("purged old task events", "count", purged)
	}
}

// maybeOpportunisticSweep fires a best-effort sweep if the throttle interval
// has elapsed. The CAS on lastSweptAt ensures at most one goroutine per
// interval actually runs the sweep. The goroutine is fire-and-forget: if
// Cloud Run throttles it after the request completes, the partial sweep
// leaves the remainder for the next trigger.
func (b *Bridge) maybeOpportunisticSweep(ctx context.Context) {
	const interval int64 = 120 // 2 minutes
	now := time.Now().Unix()
	last := b.lastSweptAt.Load()
	if now-last < interval {
		return
	}
	if !b.lastSweptAt.CompareAndSwap(last, now) {
		return // another goroutine won the CAS
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		b.RunSweep(b.shutdownCtx)
	}()
}

// Shutdown gracefully drains background work.
func (b *Bridge) Shutdown() {
	b.shutdownCancel()
	b.wg.Wait()
	b.push.Wait()
}

// SetBroker wires the broker server for subscription management.
func (b *Bridge) SetBroker(broker *BrokerServer) {
	b.broker = broker
}

// SetNotifier wires the NOTIFY accelerator. When set, waitForTaskEvent
// and streamTaskEvents wake immediately on notification instead of waiting
// for the next poll timer. Pass nil to disable (SQLite / plugin mode).
func (b *Bridge) SetNotifier(n *Notifier) {
	b.notifier = n
}

// SetSnapshot wires the atomic config snapshot for hot-apply support.
func (b *Bridge) SetSnapshot(snap *SnapshotHolder) {
	b.snapshot = snap
}

// effectiveConfig returns the effective config from the snapshot if available,
// or the static config as fallback.
func (b *Bridge) effectiveConfig() *Config {
	if b.snapshot != nil {
		snap := b.snapshot.Load()
		return &snap.Config
	}
	return b.config
}

// SetSDKRequestHandler stores the SDK RequestHandler for multi-transport access.
func (b *Bridge) SetSDKRequestHandler(h a2asrv.RequestHandler) {
	b.sdkRequestHandler = h
}

// agentKey returns a composite key for project-scoped agent isolation.
func agentKey(projectID, agentSlug string) string {
	return projectID + ":" + agentSlug
}

// waitForTaskEvent polls the event log for a response event on the given task,
// with adaptive backoff. Returns the first response (content/message) event or
// a final event, whichever comes first.
func (b *Bridge) waitForTaskEvent(ctx context.Context, taskID string, timeout time.Duration) (*state.TaskEvent, error) {
	var cursor int64
	interval := 100 * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	pollTimer := time.NewTimer(interval)
	defer pollTimer.Stop()

	// Register for NOTIFY acceleration (no-op if notifier is nil).
	var notifyCh <-chan struct{}
	if b.notifier != nil {
		var cleanup func()
		notifyCh, cleanup = b.notifier.Register(taskID)
		defer cleanup()
	}

	for {
		events, err := b.store.ReadTaskEvents(ctx, taskID, cursor, 10)
		if err != nil {
			return nil, fmt.Errorf("reading task events: %w", err)
		}
		for _, ev := range events {
			cursor = ev.ID
			if isResponseEvent(ev) {
				return &ev, nil
			}
			if ev.Final {
				return &ev, nil
			}
		}

		// Reset backoff when we got events (but none were response/final)
		if len(events) > 0 {
			interval = 100 * time.Millisecond
			if !pollTimer.Stop() {
				select {
				case <-pollTimer.C:
				default:
				}
			}
			pollTimer.Reset(interval)
			continue
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
		case <-timer.C:
			return nil, ErrTimeout
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// isResponseEvent returns true if the event is a content/message event
// (as opposed to a status-only event). These are the events that carry
// the agent's response content back to the blocking caller.
func isResponseEvent(ev state.TaskEvent) bool {
	return ev.Kind == "message" || ev.Kind == "artifact"
}

// taskEventToTaskResult converts a stored TaskEvent to a TaskResult for
// blocking SendMessage callers.
func (b *Bridge) taskEventToTaskResult(taskID, contextID string, ev *state.TaskEvent) (*TaskResult, error) {
	result := &TaskResult{
		ID:        taskID,
		ContextID: contextID,
	}

	switch ev.Kind {
	case "message":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal message event: %w", err)
		}
		result.Status = su.Status
		return result, nil
	case "status":
		var su TaskStatusUpdate
		if err := json.Unmarshal(ev.Payload, &su); err != nil {
			return nil, fmt.Errorf("unmarshal status event: %w", err)
		}
		result.Status = su.Status
		return result, nil
	case "artifact":
		var au TaskArtifactUpdate
		if err := json.Unmarshal(ev.Payload, &au); err != nil {
			return nil, fmt.Errorf("unmarshal artifact event: %w", err)
		}
		result.Status = TaskStatus{State: TaskStateWorking}
		result.Artifacts = []Artifact{au.Artifact}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown event kind: %s", ev.Kind)
	}
}

// SendMessage handles an A2A SendMessage. When taskID is non-empty, the message
// is routed as a follow-up to an existing task (continuing the conversation).
// When blocking is true (the default), it waits for the agent response.
func (b *Bridge) SendMessage(ctx context.Context, projectSlug, agentSlug, contextID, existingTaskID string, parts []Part, blocking bool) (*TaskResult, error) {
	// Follow-up on an existing task
	if existingTaskID != "" {
		return b.sendFollowUp(ctx, projectSlug, agentSlug, existingTaskID, parts, blocking)
	}

	agentCtx, err := b.resolveContext(ctx, projectSlug, agentSlug, contextID)
	if err != nil {
		return nil, fmt.Errorf("resolve context: %w", err)
	}

	caller := callerIdentityFromContext(ctx) // nil in legacy mode

	// writeClient: used for Hub write operations (send message, cancel interrupt).
	// In per-user mode, this is a short-lived client authenticated as the caller.
	// In legacy mode, it's the bridge admin client.
	writeClient := b.hubClient
	senderLabel := fmt.Sprintf("user:%s", b.config.Hub.User)

	if caller != nil {
		senderLabel = caller.SenderLabel()
		var clientErr error
		writeClient, clientErr = b.callerHubClient(caller)
		if clientErr != nil {
			return nil, fmt.Errorf("creating per-caller hub client: %w", clientErr)
		}
	}

	taskID := uuid.New().String()
	now := time.Now()
	task := &state.Task{
		ID:           taskID,
		ContextID:    agentCtx.ContextID,
		ProjectID:    agentCtx.ProjectID,
		AgentSlug:    agentCtx.AgentSlug,
		AgentID:      agentCtx.AgentID,
		State:        TaskStateSubmitted,
		CallerUserID: "", // default for legacy
		CreatedAt:    now,
		UpdatedAt:    now,
		Metadata:     "{}",
	}
	if caller != nil {
		task.CallerUserID = caller.CallerKey()
	}
	if err := b.store.CreateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	if b.metrics != nil {
		b.metrics.TasksCreated.WithLabelValues(agentCtx.ProjectID).Inc()
	}

	scionMsg := TranslateA2AToScion(parts)
	scionMsg.Sender = senderLabel
	scionMsg.Recipient = fmt.Sprintf("agent:%s", agentCtx.AgentSlug)
	scionMsg.Metadata = map[string]string{"a2aTaskId": taskID}

	if b.broker != nil {
		if caller != nil && !caller.IsAgent() {
			b.subscribeAllUserTopics(agentCtx.ProjectID)
		} else {
			// Legacy and federation callers use admin subscriptions.
			// Federation callers (agents) don't have personal user topics.
			b.subscribeAdminUserTopics(agentCtx.ProjectID)
		}
	}

	if !blocking {
		aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
		b.registerActiveTask(taskID, aKey)
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			sendCtx, cancel := context.WithTimeout(b.shutdownCtx, 30*time.Second)
			defer cancel()
			if _, err := writeClient.Agents().SendStructuredMessage(sendCtx, agentCtx.AgentID, scionMsg, false, false, false); err != nil {
				b.log.Error("non-blocking send failed", "error", err, "task_id", taskID)
				if _, err := b.store.UpdateTaskState(sendCtx, taskID, TaskStateFailed); err != nil {
					b.log.Error("failed to update task state", "error", err, "task_id", taskID)
				}
				b.unregisterActiveTask(taskID, aKey)
				return
			}
			if _, err := b.store.UpdateTaskState(sendCtx, taskID, TaskStateWorking); err != nil {
				b.log.Error("failed to update task state", "error", err, "task_id", taskID)
			}
		}()

		return &TaskResult{
			ID:        taskID,
			ContextID: agentCtx.ContextID,
			Status:    TaskStatus{State: TaskStateSubmitted},
		}, nil
	}

	// Blocking mode: register in activeTasks cache for correlation, then poll
	// the event log for the agent's response.
	aKey := agentKey(agentCtx.ProjectID, agentCtx.AgentSlug)
	b.registerActiveTask(taskID, aKey)

	if _, err := writeClient.Agents().SendStructuredMessage(ctx, agentCtx.AgentID, scionMsg, false, false, false); err != nil {
		if _, err := b.store.UpdateTaskState(ctx, taskID, TaskStateFailed); err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}
		b.unregisterActiveTask(taskID, aKey)
		return nil, fmt.Errorf("send message to agent: %w", err)
	}

	if _, err := b.store.UpdateTaskState(ctx, taskID, TaskStateWorking); err != nil {
		b.log.Error("failed to update task state", "error", err, "task_id", taskID)
	}

	timeout := b.effectiveConfig().Timeouts.SendMessage
	if timeout == 0 {
		timeout = 120 * time.Second
	}

	ev, err := b.waitForTaskEvent(ctx, taskID, timeout)
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, updateErr := b.store.UpdateTaskState(cleanupCtx, taskID, TaskStateFailed); updateErr != nil {
			b.log.Error("failed to update task state", "error", updateErr, "task_id", taskID)
		}
		b.unregisterActiveTask(taskID, aKey)
		if errors.Is(err, ErrTimeout) {
			return nil, fmt.Errorf("timeout waiting for agent response after %v", timeout)
		}
		return nil, err
	}

	return b.taskEventToTaskResult(taskID, agentCtx.ContextID, ev)
}

// sendFollowUp routes a user message to an existing task's agent, continuing
// the conversation. Returns ErrTaskTerminal if the task has already completed.
func (b *Bridge) sendFollowUp(ctx context.Context, projectSlug, agentSlug, taskID string, parts []Part, blocking bool) (*TaskResult, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, taskID)
	}
	if task.ProjectID != projectSlug || task.AgentSlug != agentSlug {
		return nil, fmt.Errorf("%w: task does not belong to %s/%s", ErrAgentNotFound, projectSlug, agentSlug)
	}
	if IsTerminalState(task.State) {
		return nil, fmt.Errorf("%w: state is %s", ErrTaskTerminal, task.State)
	}

	caller := callerIdentityFromContext(ctx)

	// Per-user/agent isolation: the follow-up caller must match the task's owner.
	if caller != nil && task.CallerUserID != "" && caller.CallerKey() != task.CallerUserID {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, taskID)
	}

	// Use per-caller client if caller identity is present.
	writeClient := b.hubClient
	senderLabel := fmt.Sprintf("user:%s", b.config.Hub.User)
	if caller != nil {
		senderLabel = caller.SenderLabel()
		var clientErr error
		writeClient, clientErr = b.callerHubClient(caller)
		if clientErr != nil {
			return nil, fmt.Errorf("creating per-caller hub client: %w", clientErr)
		}
	}

	agentID := task.AgentID
	if agent := b.lookupAgent(ctx, task.ProjectID, task.AgentSlug); agent != nil {
		agentID = agent.ID
	}

	scionMsg := TranslateA2AToScion(parts)
	scionMsg.Sender = senderLabel
	scionMsg.Recipient = fmt.Sprintf("agent:%s", task.AgentSlug)
	scionMsg.Metadata = map[string]string{"a2aTaskId": taskID}

	// Re-request broker subscriptions in case the broker reconnected since
	// the original task was created (subscriptions may have been lost).
	if b.broker != nil {
		if caller != nil && !caller.IsAgent() {
			b.subscribeAllUserTopics(task.ProjectID)
		} else {
			b.subscribeAdminUserTopics(task.ProjectID)
		}
	}

	if _, err := b.store.UpdateTaskState(ctx, taskID, TaskStateWorking); err != nil {
		b.log.Error("failed to update task state for follow-up", "error", err, "task_id", taskID)
	}

	if blocking {
		aKey := agentKey(task.ProjectID, task.AgentSlug)
		b.registerActiveTask(taskID, aKey)

		if _, err := writeClient.Agents().SendStructuredMessage(ctx, agentID, scionMsg, false, false, false); err != nil {
			b.failFollowUpTask(taskID)
			b.unregisterActiveTask(taskID, aKey)
			return nil, fmt.Errorf("send follow-up to agent: %w", err)
		}

		timeout := b.effectiveConfig().Timeouts.SendMessage
		if timeout == 0 {
			timeout = 120 * time.Second
		}

		ev, err := b.waitForTaskEvent(ctx, taskID, timeout)
		if err != nil {
			if errors.Is(err, ErrTimeout) {
				b.failFollowUpTask(taskID)
				b.unregisterActiveTask(taskID, aKey)
				return nil, fmt.Errorf("timeout waiting for agent response after %v", timeout)
			}
			b.failFollowUpTask(taskID)
			b.unregisterActiveTask(taskID, aKey)
			return nil, err
		}

		return b.taskEventToTaskResult(taskID, task.ContextID, ev)
	}

	// Non-blocking follow-up
	aKey := agentKey(task.ProjectID, task.AgentSlug)
	b.registerActiveTask(taskID, aKey)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		sendCtx, cancel := context.WithTimeout(b.shutdownCtx, 30*time.Second)
		defer cancel()
		if _, err := writeClient.Agents().SendStructuredMessage(sendCtx, agentID, scionMsg, false, false, false); err != nil {
			b.log.Error("non-blocking follow-up send failed", "error", err, "task_id", taskID)
			b.failFollowUpTask(taskID)
			b.unregisterActiveTask(taskID, aKey)
		}
	}()

	return &TaskResult{
		ID:        taskID,
		ContextID: task.ContextID,
		Status:    TaskStatus{State: TaskStateWorking},
	}, nil
}

// GetTask retrieves a task by ID. Per-user isolation: if CallerIdentity is
// present and the task has a CallerUserID, only the task's owner can read it.
func (b *Bridge) GetTask(ctx context.Context, taskID string) (*TaskResult, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	// Per-user/agent isolation: if the request has a caller identity AND the
	// task was created by a per-user/agent caller, deny access to other callers.
	// Legacy tasks (CallerUserID == "") are visible to all legacy callers
	// but NOT to per-user/agent callers (prevents information leakage across modes).
	caller := callerIdentityFromContext(ctx)
	if caller != nil && task.CallerUserID != "" && caller.CallerKey() != task.CallerUserID {
		return nil, nil // not found (avoids leaking existence)
	}
	if caller != nil && task.CallerUserID == "" {
		// Per-user/agent caller cannot see legacy tasks — prevents mode mixing.
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

// ListTasks returns tasks for a given context. Per-user isolation:
// if CallerIdentity is present, only the caller's tasks are returned.
func (b *Bridge) ListTasks(ctx context.Context, contextID string) ([]TaskResult, error) {
	caller := callerIdentityFromContext(ctx)

	var tasks []state.Task
	var err error
	if caller != nil {
		// Per-user/agent mode: only list tasks created by this caller.
		tasks, err = b.store.ListTasksByContextAndCaller(ctx, contextID, caller.CallerKey())
	} else {
		tasks, err = b.store.ListTasksByContext(ctx, contextID)
	}
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
// and sending an interrupt to the Hub to stop the agent. Per-user isolation:
// if CallerIdentity is present and the task has a CallerUserID, only the
// task's owner can cancel it.
func (b *Bridge) CancelTask(ctx context.Context, taskID string) (*TaskResult, error) {
	task, err := b.store.GetTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	// Per-user/agent isolation (same logic as GetTask).
	caller := callerIdentityFromContext(ctx)
	if caller != nil && task.CallerUserID != "" && caller.CallerKey() != task.CallerUserID {
		return nil, nil // not found
	}
	if caller != nil && task.CallerUserID == "" {
		return nil, nil // per-user/agent caller cannot cancel legacy tasks
	}

	if IsTerminalState(task.State) {
		return nil, fmt.Errorf("task %s is already in terminal state: %s", taskID, task.State)
	}

	// Unregister from local cache before CAS so concurrent broker messages
	// cannot overwrite the canceled state.
	aKey := agentKey(task.ProjectID, task.AgentSlug)
	b.unregisterActiveTask(taskID, aKey)

	// Send interrupt to the agent via Hub, re-resolving if the stored AgentID is stale.
	// Use per-user client when CallerIdentity is present (M1).
	if b.hubClient != nil && task.AgentID != "" {
		targetAgentID := task.AgentID
		if agent := b.lookupAgent(ctx, task.ProjectID, task.AgentSlug); agent != nil {
			targetAgentID = agent.ID
		}
		cancelClient := b.hubClient
		senderLabel := fmt.Sprintf("user:%s", b.config.Hub.User)
		if caller != nil {
			senderLabel = caller.SenderLabel()
			if cc, err := b.callerHubClient(caller); err == nil {
				cancelClient = cc
			} else {
				b.log.Warn("CancelTask: failed to create per-caller client, falling back to admin",
					"error", err, "task_id", taskID)
			}
		}
		interruptMsg := &messages.StructuredMessage{
			Version:   1,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    senderLabel,
			Recipient: fmt.Sprintf("agent:%s", task.AgentSlug),
			Msg:       "Task cancelled by A2A client.",
			Type:      messages.TypeInstruction,
			Metadata:  map[string]string{"a2aTaskId": taskID},
		}
		if _, err := cancelClient.Agents().SendStructuredMessage(ctx, targetAgentID, interruptMsg, true, false, false); err != nil {
			b.log.Error("failed to send cancel interrupt to agent", "error", err, "task_id", taskID, "agent_id", targetAgentID)
		}
	}

	changed, err := b.store.UpdateTaskState(ctx, taskID, TaskStateCanceled)
	if err != nil {
		b.log.Error("failed to update task state", "error", err, "task_id", taskID)
	}

	if changed {
		if b.metrics != nil {
			b.metrics.TasksCompleted.WithLabelValues(TaskStateCanceled).Inc()
		}
		cancelPayload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateCanceled},
		})
		if _, err := b.store.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:  taskID,
			Kind:    "status",
			Payload: cancelPayload,
			Final:   true,
		}); err != nil {
			b.log.Error("failed to append task event", "task_id", taskID, "kind", "status", "error", err)
		}
		cancelEvent := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: taskID,
				Status: TaskStatus{State: TaskStateCanceled},
				Final:  true,
			},
		}
		b.push.Dispatch(ctx, taskID, cancelEvent)
	}

	return &TaskResult{
		ID:        task.ID,
		ContextID: task.ContextID,
		Status:    TaskStatus{State: TaskStateCanceled},
	}, nil
}

// HandleBrokerMessage processes a broker message inline: correlates to a taskID,
// writes events to the durable event log, CAS-updates task state, and
// conditionally dispatches push notifications. No in-memory fan-out is used;
// all consumers read from the event log.
func (b *Bridge) HandleBrokerMessage(ctx context.Context, topic string, msg *messages.StructuredMessage) error {
	b.log.Debug("handling broker message",
		"topic", topic,
		"sender", msg.Sender,
		"type", msg.Type,
		"msg_preview", truncate(msg.Msg, 100),
	)

	agentSlug := extractAgentIDFromSender(msg.Sender)
	if agentSlug == "" {
		b.log.Warn("ignoring message: sender does not use agent:<slug> format, dropping", "topic", topic, "sender", msg.Sender)
		return nil
	}

	projectID := extractProjectIDFromTopic(topic)
	if projectID == "" {
		b.log.Warn("dropping message with unparseable project ID", "topic", topic)
		return nil
	}

	// Correlate to taskID.
	taskID, err := b.correlateToTask(ctx, projectID, agentSlug, msg)
	if err != nil {
		b.log.Debug("could not correlate broker message to task", "error", err, "topic", topic, "sender", msg.Sender)
		return nil
	}

	// Skip system messages — they don't produce events.
	if msg.Type == messages.TypeSystem {
		b.log.Debug("skipping system message in A2A bridge", "task_id", taskID, "sender", msg.Sender)
		return nil
	}

	// Process the message and write to the event log.
	b.processAndAppendEvent(ctx, taskID, agentSlug, msg)
	return nil
}

// correlateToTask determines the taskID for a broker message, using metadata
// or falling back to DB lookup.
func (b *Bridge) correlateToTask(ctx context.Context, projectID, agentSlug string, msg *messages.StructuredMessage) (string, error) {
	// If the message carries a task correlation ID, verify ownership.
	if taskID := msg.Metadata["a2aTaskId"]; taskID != "" {
		task, err := b.store.GetTask(ctx, taskID)
		if err != nil || task == nil {
			return "", fmt.Errorf("unknown task: %s", taskID)
		}
		if task.AgentSlug != agentSlug {
			b.log.Warn("dropping cross-agent a2aTaskId injection",
				"task_agent", task.AgentSlug, "msg_agent", agentSlug, "task_id", taskID)
			return "", fmt.Errorf("cross-agent injection for task %s", taskID)
		}
		return taskID, nil
	}

	// No a2aTaskId — fall back to local cache, then DB.
	aKey := agentKey(projectID, agentSlug)

	// Fast path: check local cache.
	b.tasksMu.RLock()
	taskIDs := append([]string(nil), b.agentTasks[aKey]...)
	b.tasksMu.RUnlock()

	if len(taskIDs) > 0 {
		b.log.Debug("correlating broker message by agent slug from local cache",
			"agent", agentSlug, "project", projectID, "active_tasks", len(taskIDs))
		// Use the most recently registered task.
		return taskIDs[len(taskIDs)-1], nil
	}

	// Slow path: query DB.
	task, err := b.store.FindActiveTaskForAgent(ctx, projectID, agentSlug)
	if err != nil {
		return "", fmt.Errorf("DB lookup failed: %w", err)
	}
	if task == nil {
		return "", fmt.Errorf("no active tasks for agent %s in project %s", agentSlug, projectID)
	}

	b.log.Debug("correlating broker message by agent slug from DB fallback",
		"agent", agentSlug, "project", projectID, "task_id", task.ID)
	return task.ID, nil
}

// processAndAppendEvent translates a broker message to an event, appends it to
// the event log, CAS-updates task state, and conditionally fires push notifications.
func (b *Bridge) processAndAppendEvent(ctx context.Context, taskID, agentSlug string, msg *messages.StructuredMessage) {
	// Determine the dedup key.
	dedupKey := ""
	if msgID, ok := msg.Metadata["msgId"]; ok && msgID != "" {
		dedupKey = msgID
	} else {
		// Generate a deterministic key from taskID + content hash.
		h := sha256.Sum256([]byte(taskID + msg.Msg + msg.Timestamp))
		dedupKey = fmt.Sprintf("%x", h[:16])
	}

	if msg.Type == messages.TypeStateChange {
		taskState := MapActivityToTaskState(msg.Msg)
		isFinal := IsTerminalState(taskState)

		statusPayload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: taskState},
		})

		if _, err := b.store.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:   taskID,
			Kind:     "status",
			Payload:  statusPayload,
			Final:    isFinal,
			DedupKey: dedupKey,
		}); err != nil {
			b.log.Error("failed to append task event", "task_id", taskID, "kind", "status", "error", err)
		}

		changed, err := b.store.UpdateTaskState(ctx, taskID, taskState)
		if err != nil {
			b.log.Error("failed to update task state", "error", err, "task_id", taskID)
		}

		if changed && isFinal {
			if b.metrics != nil {
				b.metrics.TasksCompleted.WithLabelValues(taskState).Inc()
			}
			// This instance won the CAS — dispatch push notifications.
			event := StreamEvent{
				StatusUpdate: &TaskStatusUpdate{
					TaskID: taskID,
					Status: TaskStatus{State: taskState},
					Final:  true,
				},
			}
			b.push.Dispatch(ctx, taskID, event)

			// Unregister from local cache.
			b.tasksMu.RLock()
			aKey := b.activeTasks[taskID].aKey
			b.tasksMu.RUnlock()
			if aKey == "" {
				aKey = agentKey("", agentSlug)
			}
			b.unregisterActiveTask(taskID, aKey)
		} else if changed {
			// Non-terminal state change — still dispatch push for subscribers.
			event := StreamEvent{
				StatusUpdate: &TaskStatusUpdate{
					TaskID: taskID,
					Status: TaskStatus{State: taskState},
					Final:  false,
				},
			}
			b.push.Dispatch(ctx, taskID, event)
		}
		return
	}

	// Content message — write to event log.
	// Touch the DB timestamp so the janitor doesn't reap active tasks
	// whose only recent activity is content messages.
	if err := b.store.TouchTask(ctx, taskID); err != nil {
		b.log.Error("failed to refresh task timestamp for content message",
			"task_id", taskID, "error", err)
	}

	a2aMsg, artifacts := TranslateScionToA2A(msg)

	currentState := TaskStateWorking
	if task, err := b.store.GetTask(ctx, taskID); err != nil {
		b.log.Error("failed to get task for content message",
			"task_id", taskID, "error", err)
	} else if task != nil {
		currentState = task.State
	}

	// Write artifact events.
	for _, art := range artifacts {
		artPayload, _ := json.Marshal(TaskArtifactUpdate{
			TaskID:   taskID,
			Artifact: art,
		})
		if _, err := b.store.AppendTaskEvent(ctx, &state.TaskEvent{
			TaskID:   taskID,
			Kind:     "artifact",
			Payload:  artPayload,
			Final:    false,
			DedupKey: dedupKey + ":artifact:" + art.ArtifactID,
		}); err != nil {
			b.log.Error("failed to append task event", "task_id", taskID, "kind", "artifact", "error", err)
		}
		artEvent := StreamEvent{
			ArtifactUpdate: &TaskArtifactUpdate{
				TaskID:   taskID,
				Artifact: art,
			},
		}
		b.push.Dispatch(ctx, taskID, artEvent)
	}

	// Write message event.
	msgPayload, _ := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{
			State:   currentState,
			Message: &a2aMsg,
		},
	})
	if _, err := b.store.AppendTaskEvent(ctx, &state.TaskEvent{
		TaskID:   taskID,
		Kind:     "message",
		Payload:  msgPayload,
		Final:    false,
		DedupKey: dedupKey + ":message",
	}); err != nil {
		b.log.Error("failed to append task event", "task_id", taskID, "kind", "message", "error", err)
	}

	statusEvent := StreamEvent{
		StatusUpdate: &TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{
				State:   currentState,
				Message: &a2aMsg,
			},
			Final: false,
		},
	}
	b.push.Dispatch(ctx, taskID, statusEvent)
}

// failFollowUpTask centralises the failure-notification pattern for follow-up
// messages: update DB state via CAS, increment metrics, write failure event
// to the log, and dispatch push notifications.
func (b *Bridge) failFollowUpTask(taskID string) {
	changed, err := b.store.UpdateTaskState(b.shutdownCtx, taskID, TaskStateFailed)
	if err != nil {
		b.log.Error("failed to update task state", "error", err, "task_id", taskID)
	}
	if changed {
		if b.metrics != nil {
			b.metrics.TasksCompleted.WithLabelValues(TaskStateFailed).Inc()
		}
		failPayload, _ := json.Marshal(TaskStatusUpdate{
			TaskID: taskID,
			Status: TaskStatus{State: TaskStateFailed},
		})
		if _, err := b.store.AppendTaskEvent(b.shutdownCtx, &state.TaskEvent{
			TaskID:  taskID,
			Kind:    "status",
			Payload: failPayload,
			Final:   true,
		}); err != nil {
			b.log.Error("failed to append task event", "task_id", taskID, "kind", "status", "error", err)
		}
		failEvent := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: taskID,
				Status: TaskStatus{State: TaskStateFailed},
				Final:  true,
			},
		}
		b.push.Dispatch(b.shutdownCtx, taskID, failEvent)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// callerHubClient creates a per-request Hub client authenticated as the caller.
// For UAT callers, the original token is passed through to the Hub.
// For JWT callers, a fresh 5-minute JWT is minted for the caller's identity.
// For federation callers, the bridge's admin auth is used with the federation
// token passed via X-Scion-Federation-Token header.
//
// All cases compose transport auth (IAP / Cloud Run invoker) into the HTTP
// transport chain when configured, so per-caller clients can reach hubs behind
// identity-aware proxies.
func (b *Bridge) callerHubClient(caller *CallerIdentity) (hubclient.Client, error) {
	switch caller.TokenType {
	case "uat":
		opts := []hubclient.Option{hubclient.WithBearerToken(caller.RawToken)}
		if b.transportSrc != nil {
			opts = append(opts, hubclient.WithTransportAuth(b.transportSrc, b.transportMode))
		}
		return hubclient.New(b.config.Hub.Endpoint, opts...)
	case "jwt":
		// Re-mint a 5-minute JWT for the caller's identity using the bridge's
		// signing key. This is the same infrastructure used for the bridge's
		// own admin identity.
		mintAuth := identity.NewMintingAuth(b.minter,
			caller.UserID, caller.Email, caller.Role, 5*time.Minute)
		opts := []hubclient.Option{hubclient.WithAuthenticator(mintAuth)}
		if b.transportSrc != nil {
			opts = append(opts, hubclient.WithTransportAuth(b.transportSrc, b.transportMode))
		}
		return hubclient.New(b.config.Hub.Endpoint, opts...)
	case "federation":
		// For federation callers, use the bridge's admin auth for Hub API
		// access, but inject the X-Scion-Federation-Token header so the Hub
		// can identify the federated agent. The bridge acts as a proxy:
		// its admin identity authorizes the API call, the federation header
		// carries the agent's identity for the Hub to validate.
		hubUserID := b.config.Hub.UserID
		if hubUserID == "" {
			hubUserID = b.config.Hub.User
		}
		mintAuth := identity.NewMintingAuth(b.minter,
			hubUserID, b.config.Hub.User, "admin", 5*time.Minute)

		// Compose: transport auth (IAP/invoker) → federation header injection.
		base := http.DefaultTransport
		if b.transportSrc != nil {
			base = transportauth.Wrap(base, b.transportSrc, b.transportMode)
		}

		// WithHTTPClient must come before WithAuthenticator so that the
		// federation header transport is set as the base before auth wraps it.
		opts := []hubclient.Option{
			hubclient.WithHTTPClient(&http.Client{
				Transport: &federationHeaderTransport{
					base:  base,
					token: caller.RawToken,
				},
			}),
			hubclient.WithAuthenticator(mintAuth),
		}
		if b.transportSrc != nil {
			opts = append(opts, hubclient.WithTransportAuth(b.transportSrc, b.transportMode))
		}
		return hubclient.New(b.config.Hub.Endpoint, opts...)
	default:
		return nil, fmt.Errorf("unknown token type: %s", caller.TokenType)
	}
}

// subscribeAllUserTopics subscribes to the wildcard user topic for per-user
// mode. This captures replies to all users' messages in the project.
// The broker's eventbus supports NATS-style * wildcards (confirmed by
// TestInProcessEventBus_WildcardSubscribe).
func (b *Bridge) subscribeAllUserTopics(projectID string) {
	pattern := projectcompat.AllUserTopic(projectID)
	if err := b.broker.RequestSubscription(pattern); err != nil {
		b.log.Warn("failed to request wildcard subscription", "pattern", pattern, "error", err)
	}
}

// subscribeAdminUserTopics subscribes to the bridge admin's user topic (legacy mode).
func (b *Bridge) subscribeAdminUserTopics(projectID string) {
	pattern := projectcompat.UserTopic(projectID, b.config.Hub.User)
	if err := b.broker.RequestSubscription(pattern); err != nil {
		b.log.Warn("failed to request subscription", "pattern", pattern, "error", err)
	}
	legacyPattern := projectcompat.LegacyUserTopic(projectID, b.config.Hub.User)
	if err := b.broker.RequestSubscription(legacyPattern); err != nil {
		b.log.Warn("failed to request legacy subscription", "pattern", legacyPattern, "error", err)
	}
}

// GenerateAgentCard builds an agent card for the given project and agent,
// enriching it with metadata from the Hub API when available.
func (b *Bridge) GenerateAgentCard(ctx context.Context, projectSlug, agentSlug string) map[string]interface{} {
	cfg := b.effectiveConfig()
	baseURL := strings.TrimRight(cfg.Bridge.ExternalURL, "/")
	agentURL := fmt.Sprintf("%s/projects/%s/agents/%s", baseURL, projectSlug, agentSlug)

	name := agentSlug
	description := fmt.Sprintf("Scion agent %s in project %s", agentSlug, projectSlug)
	var skills []map[string]interface{}

	if agent := b.lookupAgent(ctx, projectSlug, agentSlug); agent != nil {
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

	jsonrpcURL := agentURL + "/jsonrpc"
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
		"supportedInterfaces": []map[string]interface{}{
			{
				"url":             jsonrpcURL,
				"protocolBinding": "JSONRPC",
				"protocolVersion": "1.0",
			},
		},
	}

	if cfg.Bridge.Provider.Organization != "" {
		card["provider"] = map[string]string{
			"organization": cfg.Bridge.Provider.Organization,
			"url":          cfg.Bridge.Provider.URL,
		}
	}

	return card
}

// lookupAgent fetches agent metadata from the Hub API, returning nil on failure.
// Results are cached for agentCacheTTL to avoid listing all agents on every call.
func (b *Bridge) lookupAgent(ctx context.Context, projectSlug, agentSlug string) *hubclient.Agent {
	cacheKey := projectSlug + ":" + agentSlug

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
	agents, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{ProjectID: projectSlug})
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

// GetProjectConfig returns the configuration for a project slug, or nil if not configured.
// Returns a pointer to a copy to avoid aliasing the live config slice.
func (b *Bridge) GetProjectConfig(projectSlug string) *ProjectConfig {
	cfg := b.effectiveConfig()
	for i := range cfg.Projects {
		if cfg.Projects[i].Slug == projectSlug {
			pc := cfg.Projects[i]
			return &pc
		}
	}
	return nil
}

// resolveContext maps an A2A context to a Scion agent, creating a new context if needed.
func (b *Bridge) resolveContext(ctx context.Context, projectSlug, agentSlug, contextID string) (*state.Context, error) {
	if contextID != "" {
		existing, err := b.store.GetContext(ctx, contextID)
		if err != nil {
			return nil, fmt.Errorf("get context: %w", err)
		}
		if existing != nil {
			if existing.ProjectID != projectSlug || existing.AgentSlug != agentSlug {
				return nil, fmt.Errorf("%w: context does not belong to %s/%s", ErrContextUnknown, projectSlug, agentSlug)
			}
			if err := b.store.TouchContext(ctx, contextID); err != nil {
				b.log.Error("failed to touch context", "context_id", contextID, "error", err)
			}
			return existing, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrContextUnknown, contextID)
	}

	agents, err := b.hubClient.Agents().List(ctx, &hubclient.ListAgentsOptions{ProjectID: projectSlug})
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	var agentID, projectID string
	for _, a := range agents.Agents {
		if a.Name == agentSlug || a.Slug == agentSlug {
			agentID = a.ID
			projectID = a.ProjectID
			break
		}
	}
	if agentID == "" {
		projectCfg := b.GetProjectConfig(projectSlug)
		if projectCfg == nil || !projectCfg.AutoProvision || projectCfg.DefaultTemplate == "" {
			return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, agentSlug)
		}

		b.log.Info("auto-provisioning agent", "slug", agentSlug, "project", projectSlug, "template", projectCfg.DefaultTemplate)
		created, err := b.hubClient.Agents().Create(ctx, &hubclient.CreateAgentRequest{
			Name:      agentSlug,
			ProjectID: projectSlug,
			Template:  projectCfg.DefaultTemplate,
			Labels:    map[string]string{"a2a-bridge/auto-provisioned": "true"},
		})
		if err != nil {
			// Concurrent create may have succeeded; re-list to find the agent.
			retryAgents, retryErr := b.hubClient.Agents().List(ctx, &hubclient.ListAgentsOptions{ProjectID: projectSlug})
			if retryErr != nil {
				return nil, fmt.Errorf("auto-provision agent %q: %w", agentSlug, err)
			}
			found := false
			for _, a := range retryAgents.Agents {
				if a.Name == agentSlug || a.Slug == agentSlug {
					agentID = a.ID
					projectID = a.ProjectID
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("auto-provision agent %q: %w", agentSlug, err)
			}
		} else {
			agentID = created.Agent.ID
			projectID = created.Agent.ProjectID
		}
	}
	if projectID == "" {
		projectID = projectSlug
	}

	newContextID := uuid.New().String()
	now := time.Now()
	agentCtx := &state.Context{
		ContextID:  newContextID,
		ProjectID:  projectID,
		AgentSlug:  agentSlug,
		AgentID:    agentID,
		CreatedAt:  now,
		LastActive: now,
	}
	if err := b.store.CreateContext(ctx, agentCtx); err != nil {
		return nil, fmt.Errorf("create context: %w", err)
	}

	return agentCtx, nil
}

func (b *Bridge) registerActiveTask(taskID, aKey string) {
	b.tasksMu.Lock()
	defer b.tasksMu.Unlock()
	// Only append to agentTasks if the task is not already registered,
	// preventing duplicate entries from concurrent follow-ups.
	if _, exists := b.activeTasks[taskID]; !exists {
		b.agentTasks[aKey] = append(b.agentTasks[aKey], taskID)
	}
	b.activeTasks[taskID] = activeTaskEntry{aKey: aKey, createdAt: time.Now()}
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

// parseTopic extracts project and agent identifiers from a broker topic string.
// Canonical scion.project topics and legacy scion.grove topics are accepted.
func parseTopic(topic string) (projectID, agentSlug string, err error) {
	parsed, err := projectcompat.ParseTopic(topic)
	if err != nil {
		return "", "", fmt.Errorf("malformed topic: %s", topic)
	}
	if parsed.Kind == projectcompat.TopicKindAgent {
		agentSlug = parsed.Actor
	}
	return parsed.ProjectID, agentSlug, nil
}

func extractProjectIDFromTopic(topic string) string {
	projectID, _, _ := parseTopic(topic)
	return projectID
}

// AuthorizeExposed returns nil if the project is configured and the agent
// is exposed (or no allowlist is set). Returns ErrAgentNotFound to avoid
// leaking project existence.
func (b *Bridge) AuthorizeExposed(projectSlug, agentSlug string) error {
	g := b.GetProjectConfig(projectSlug)
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

// AuthorizeTask verifies a task belongs to the given project and agent.
// Returns nil (not an error) if the task doesn't exist or doesn't match,
// so callers can return "not found" without leaking existence.
func (b *Bridge) AuthorizeTask(taskID, projectSlug, agentSlug string) (*state.Task, error) {
	task, err := b.store.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	if task == nil || task.ProjectID != projectSlug || task.AgentSlug != agentSlug {
		return nil, nil
	}
	return task, nil
}

// AuthorizeContext verifies a context belongs to the given project and agent.
// Returns (true, nil) on success, (false, nil) when the context doesn't exist
// or doesn't match, and (false, err) on database errors.
func (b *Bridge) AuthorizeContext(contextID, projectSlug, agentSlug string) (bool, error) {
	ctx, err := b.store.GetContext(context.Background(), contextID)
	if err != nil {
		return false, fmt.Errorf("get context: %w", err)
	}
	if ctx == nil {
		return false, nil
	}
	return ctx.ProjectID == projectSlug && ctx.AgentSlug == agentSlug, nil
}

// extractAgentIDFromSender extracts agent identity from sender field.
func extractAgentIDFromSender(sender string) string {
	if strings.HasPrefix(sender, "agent:") {
		return strings.TrimPrefix(sender, "agent:")
	}
	return ""
}
