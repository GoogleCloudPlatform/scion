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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// v1Triggers is the set of authoritative phase transitions that fire lifecycle
// hooks in v1. Only these phases are considered as triggers.
var v1Triggers = map[state.Phase]string{
	state.PhaseRunning:   store.LifecycleHookTriggerRunning,
	state.PhaseSuspended: store.LifecycleHookTriggerSuspended,
	state.PhaseStopped:   store.LifecycleHookTriggerStopped,
	state.PhaseError:     store.LifecycleHookTriggerError,
}

// LifecycleHookExecutor is the interface that M5 will implement for executing
// the HTTP/webhook action of a lifecycle hook. M4 provides a no-op/logging
// default; tests and M5 can inject their own implementation.
type LifecycleHookExecutor interface {
	// Execute performs the action defined in the hook for the given agent and
	// trigger. Implementations MUST NOT panic; panics will be recovered by the
	// evaluator. Errors are logged but never propagated to the transition path.
	Execute(ctx context.Context, hook *store.LifecycleHook, agent *store.Agent, trigger string) error
}

// LoggingExecutor is a no-op executor that logs hook executions. It serves as
// the default executor for M4 and is replaced by the real HTTP executor in M5.
type LoggingExecutor struct {
	Log *slog.Logger
}

// Execute logs the hook execution without performing any real action.
func (e *LoggingExecutor) Execute(_ context.Context, hook *store.LifecycleHook, agent *store.Agent, trigger string) error {
	log := e.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("lifecycle hook fired (no-op executor)",
		"hook_id", hook.ID,
		"hook_name", hook.Name,
		"trigger", trigger,
		"agent_id", agent.ID,
		"agent_project_id", agent.ProjectID,
		"agent_template", agent.Template,
	)
	return nil
}

// =============================================================================
// TransitionDeduper — backend-aware phase transition de-duplication
// =============================================================================

// TransitionDeduper detects whether a phase change for an agent constitutes a
// genuine transition (i.e. the phase actually changed) rather than a
// re-publication of the same phase (e.g. heartbeats). Two implementations
// exist:
//
//   - storeDeduper: durable, backed by an atomic compare-and-set in the store.
//     Safe for multi-instance / HA deployments (Postgres) because exactly one
//     instance's CAS succeeds per logical transition.
//   - memoryDeduper: in-process map, seeded from the store on start. Used for
//     single-instance / sqlite / dev deployments where durability adds overhead
//     without benefit.
type TransitionDeduper interface {
	// IsTransition returns true if newPhase differs from the last phase
	// recorded for this agent (or no phase is recorded yet). On true, the
	// new phase is recorded atomically. Implementations must be goroutine-safe.
	IsTransition(ctx context.Context, agentID, newPhase string) (bool, error)

	// Forget removes any recorded phase for the agent. Called on terminal
	// phases and agent deletion to prevent unbounded state growth.
	Forget(ctx context.Context, agentID string) error
}

// storeDeduper delegates to the store's atomic CompareAndSetHookPhase /
// DeleteHookPhase. Durable across restarts and HA-safe (exactly one CAS
// winner per transition). No cold-start seeding is needed because the CAS
// state is persisted.
type storeDeduper struct {
	store store.Store
	log   *slog.Logger
}

func (d *storeDeduper) IsTransition(ctx context.Context, agentID, newPhase string) (bool, error) {
	changed, err := d.store.CompareAndSetHookPhase(ctx, agentID, newPhase)
	if err != nil {
		return false, fmt.Errorf("store CAS hook phase: %w", err)
	}
	return changed, nil
}

func (d *storeDeduper) Forget(ctx context.Context, agentID string) error {
	return d.store.DeleteHookPhase(ctx, agentID)
}

// memoryDeduper is an in-process previous-phase map with the same semantics as
// the original evaluator implementation: seeded from the store on construction,
// pruned on terminal phases / deletion. Suitable for single-instance deployments.
type memoryDeduper struct {
	mu            sync.Mutex
	previousPhase map[string]string
}

func newMemoryDeduper() *memoryDeduper {
	return &memoryDeduper{
		previousPhase: make(map[string]string),
	}
}

func (d *memoryDeduper) IsTransition(_ context.Context, agentID, newPhase string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.previousPhase[agentID]
	if prev == newPhase {
		return false, nil
	}
	d.previousPhase[agentID] = newPhase
	return true, nil
}

func (d *memoryDeduper) Forget(_ context.Context, agentID string) error {
	d.mu.Lock()
	delete(d.previousPhase, agentID)
	d.mu.Unlock()
	return nil
}

// seed populates the in-memory map from the store so that steady-state status
// events after a restart are not misinterpreted as transitions.
func (d *memoryDeduper) seed(s store.Store, log *slog.Logger) {
	ctx := context.Background()
	result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		log.Error("Failed to seed previousPhase from store (continuing without seed)", "error", err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range result.Items {
		d.previousPhase[a.ID] = a.Phase
	}
	log.Info("Seeded lifecycle hook evaluator previousPhase", "agents", len(result.Items))
}

// previousPhaseLen returns the number of entries (test helper).
func (d *memoryDeduper) previousPhaseLen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.previousPhase)
}

// previousPhaseHas returns true if the agent has an entry (test helper).
func (d *memoryDeduper) previousPhaseHas(agentID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.previousPhase[agentID]
	return ok
}

// DBDriverPostgres is the sentinel value for a Postgres-backed hub. When the
// evaluator is constructed with this driver, it uses the durable storeDeduper.
const DBDriverPostgres = "postgres"

// deduperDriverForPublisher returns the DB-driver sentinel that selects the
// transition-deduper backend from the event publisher's broadcast semantics.
// *PostgresEventPublisher broadcasts every event to ALL hub instances (multi-
// instance HA), so it requires the durable store-backed CAS deduper
// (DBDriverPostgres) to guarantee exactly-once firing. Purely in-process
// publishers (ChannelEventPublisher) are single-instance and need only the
// in-memory deduper.
func deduperDriverForPublisher(ep EventPublisher) string {
	if _, ok := ep.(*PostgresEventPublisher); ok {
		return DBDriverPostgres
	}
	return ""
}

// NewTransitionDeduper selects and returns the appropriate deduper for the
// given database driver. Postgres uses the durable store-backed CAS;
// everything else (sqlite, "", etc.) uses the in-memory map.
func NewTransitionDeduper(dbDriver string, s store.Store, log *slog.Logger) TransitionDeduper {
	if dbDriver == DBDriverPostgres {
		return &storeDeduper{store: s, log: log}
	}
	md := newMemoryDeduper()
	md.seed(s, log)
	return md
}

// =============================================================================
// LifecycleHookEvaluator
// =============================================================================

// LifecycleHookEvaluator listens for authoritative agent phase transitions and
// evaluates matching lifecycle hooks. It follows the same event-subscriber
// pattern as NotificationDispatcher: it subscribes to the EventPublisher
// and fires asynchronously after the transition is committed, guaranteeing that
// hook evaluation never blocks or fails the authoritative transition.
//
// The evaluator accepts the EventPublisher interface (not the concrete
// *ChannelEventPublisher) so it works with both ChannelEventPublisher (dev/
// sqlite) and PostgresEventPublisher (HA/production). When using Postgres,
// the PostgresEventPublisher broadcasts each event to ALL hub instances via
// NOTIFY, so the store-backed CAS deduper is mandatory for exactly-once firing.
//
// Transition de-duplication is backend-aware: Postgres deployments use a
// durable store-backed atomic CAS (safe for multi-instance HA); sqlite/dev
// deployments use an in-memory map (seeded from the store on Start).
type LifecycleHookEvaluator struct {
	store    store.Store
	events   EventPublisher
	executor LifecycleHookExecutor
	log      *slog.Logger

	// deduper detects actual phase transitions vs. heartbeat re-publications.
	// Selected at construction based on the configured DB backend.
	deduper TransitionDeduper

	// dbDriver is preserved for test introspection (backend-selection tests).
	dbDriver string

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewLifecycleHookEvaluator creates a new evaluator. The executor is injectable;
// pass nil to use the default LoggingExecutor. The events parameter accepts the
// EventPublisher interface so the evaluator works with both ChannelEventPublisher
// (dev) and PostgresEventPublisher (HA). The dbDriver option selects the
// transition de-duplication strategy: "postgres" uses the durable store-backed
// CAS (HA-safe); any other value uses the in-memory map.
func NewLifecycleHookEvaluator(s store.Store, events EventPublisher, executor LifecycleHookExecutor, log *slog.Logger, opts ...EvaluatorOption) *LifecycleHookEvaluator {
	if executor == nil {
		executor = &LoggingExecutor{Log: log}
	}
	if log == nil {
		log = slog.Default()
	}

	cfg := evaluatorConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	deduper := NewTransitionDeduper(cfg.dbDriver, s, log)

	return &LifecycleHookEvaluator{
		store:    s,
		events:   events,
		executor: executor,
		log:      log,
		deduper:  deduper,
		dbDriver: cfg.dbDriver,
		stopCh:   make(chan struct{}),
	}
}

// evaluatorConfig holds optional configuration for the evaluator.
type evaluatorConfig struct {
	dbDriver string
}

// EvaluatorOption configures the LifecycleHookEvaluator.
type EvaluatorOption func(*evaluatorConfig)

// WithDBDriver sets the database driver used for backend-aware de-duplication
// selection. Pass "postgres" for durable store-backed CAS; any other value
// (including "") uses the in-memory map.
func WithDBDriver(driver string) EvaluatorOption {
	return func(c *evaluatorConfig) {
		c.dbDriver = driver
	}
}

// Start subscribes to agent status events and spawns a goroutine to process them.
// It is safe to call multiple times; only the first call has an effect.
func (e *LifecycleHookEvaluator) Start() {
	e.startOnce.Do(func() {
		// Use "*" (single-token wildcard) rather than ">" (multi-token) to
		// avoid cross-matching: "project.>.agent.status" would also match
		// "project.X.agent.deleted" subjects, causing handleDeletedEvent to
		// spuriously prune entries on status events.
		statusCh, unsubStatus := e.events.Subscribe("project.*.agent.status")
		deletedCh, unsubDeleted := e.events.Subscribe("project.*.agent.deleted")

		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer unsubStatus()
			defer unsubDeleted()
			for {
				select {
				case evt, ok := <-statusCh:
					if !ok {
						return
					}
					e.handleEvent(evt)
				case evt, ok := <-deletedCh:
					if !ok {
						return
					}
					e.handleDeletedEvent(evt)
				case <-e.stopCh:
					return
				}
			}
		}()

		e.log.Info("Lifecycle hook evaluator started")
	})
}

// Stop signals the evaluator goroutine to exit and waits for it to finish.
// Safe to call multiple times.
func (e *LifecycleHookEvaluator) Stop() {
	e.stopOnce.Do(func() {
		close(e.stopCh)
		e.wg.Wait()
		e.log.Info("Lifecycle hook evaluator stopped")
	})
}

// handleEvent processes a single agent status event. It checks whether the
// phase is a v1 trigger and whether it represents an actual transition, then
// evaluates matching hooks.
func (e *LifecycleHookEvaluator) handleEvent(evt Event) {
	var statusEvt AgentStatusEvent
	if err := json.Unmarshal(evt.Data, &statusEvt); err != nil {
		e.log.Error("Failed to unmarshal agent status event for lifecycle hooks", "error", err)
		return
	}

	// Defensive: an empty AgentID would fail downstream deduper/store queries
	// with validation errors. Skip such malformed events.
	if statusEvt.AgentID == "" {
		e.log.Error("Received agent status event with empty AgentID for lifecycle hooks")
		return
	}

	// Only process v1 triggers.
	trigger, ok := v1Triggers[state.Phase(statusEvt.Phase)]
	if !ok {
		return
	}

	// Check for actual transition via the deduper.
	ctx := context.Background()

	changed, err := e.deduper.IsTransition(ctx, statusEvt.AgentID, statusEvt.Phase)
	if err != nil {
		// DEFENSIVE: log and skip — never abort/block the transition.
		e.log.Error("Failed to check transition dedup",
			"agent_id", statusEvt.AgentID, "phase", statusEvt.Phase, "error", err)
		return
	}

	// NOTE: we intentionally do NOT prune the deduper entry on terminal phases
	// (stopped/error). Pruning here re-arms the transition check, so a
	// redelivered terminal event (pub/sub redelivery under HA, retries, or
	// heartbeats while the agent stays terminal) would be seen as a fresh
	// transition and fire the hook again. The entry is pruned only on agent
	// deletion (handleDeletedEvent). One entry per agent is negligible overhead
	// (bounded by the agents table) and buys robust exactly-once firing.

	if !changed {
		return // same phase re-published (e.g., heartbeat), not a transition
	}

	// Fetch the full agent record so we have project_id and template for matching.
	agent, err := e.store.GetAgent(ctx, statusEvt.AgentID)
	if err != nil {
		e.log.Error("Failed to fetch agent for lifecycle hook evaluation",
			"agent_id", statusEvt.AgentID, "error", err)
		return
	}

	e.evaluateAndExecute(ctx, agent, trigger)
}

// evaluateAndExecute loads matching hooks and invokes the executor for each.
// This method is safe to call directly (e.g. from tests) and recovers from
// panics in the executor.
func (e *LifecycleHookEvaluator) evaluateAndExecute(ctx context.Context, agent *store.Agent, trigger string) {
	hooks, err := e.findMatchingHooks(ctx, agent, trigger)
	if err != nil {
		e.log.Error("Failed to query lifecycle hooks",
			"trigger", trigger, "agent_id", agent.ID, "error", err)
		return
	}

	if len(hooks) == 0 {
		return
	}

	e.log.Info("Evaluating lifecycle hooks",
		"trigger", trigger,
		"agent_id", agent.ID,
		"matching_hooks", len(hooks),
	)

	for i := range hooks {
		hook := &hooks[i]
		e.executeHookSafe(ctx, hook, agent, trigger)
	}
}

// findMatchingHooks queries the store for enabled hooks matching the given
// trigger, then filters by selector (project_id, template). Empty/zero selector
// fields mean "match any".
func (e *LifecycleHookEvaluator) findMatchingHooks(ctx context.Context, agent *store.Agent, trigger string) ([]store.LifecycleHook, error) {
	enabled := true
	result, err := e.store.ListLifecycleHooks(ctx, store.LifecycleHookFilter{
		Trigger: trigger,
		Enabled: &enabled,
	}, store.ListOptions{Limit: 1000}) // generous limit; hooks are admin-managed
	if err != nil {
		return nil, fmt.Errorf("list lifecycle hooks: %w", err)
	}

	var matched []store.LifecycleHook
	for _, hook := range result.Items {
		if selectorMatches(&hook, agent) {
			matched = append(matched, hook)
		}
	}
	return matched, nil
}

// selectorMatches returns true if the hook's selector matches the given agent.
// An empty/nil selector matches all agents. When a selector field is non-empty,
// it must match the corresponding agent field exactly.
func selectorMatches(hook *store.LifecycleHook, agent *store.Agent) bool {
	sel := hook.Selector
	if sel == nil {
		return true // nil selector matches all agents
	}
	if sel.ProjectID != "" && sel.ProjectID != agent.ProjectID {
		return false
	}
	if sel.Template != "" && sel.Template != agent.Template {
		return false
	}
	return true
}

// handleDeletedEvent prunes the deduper entry for a deleted agent,
// mirroring the NotificationDispatcher's deletion subscription pattern.
func (e *LifecycleHookEvaluator) handleDeletedEvent(evt Event) {
	var deletedEvt AgentDeletedEvent
	if err := json.Unmarshal(evt.Data, &deletedEvt); err != nil {
		e.log.Error("Failed to unmarshal agent deleted event for lifecycle hooks", "error", err)
		return
	}
	ctx := context.Background()
	if err := e.deduper.Forget(ctx, deletedEvt.AgentID); err != nil {
		e.log.Error("Failed to prune deduper entry for deleted agent",
			"agent_id", deletedEvt.AgentID, "error", err)
	}
}

// executeHookSafe invokes the executor with panic recovery. Executor errors and
// panics are logged but never propagated — the transition path must succeed
// regardless.
func (e *LifecycleHookEvaluator) executeHookSafe(ctx context.Context, hook *store.LifecycleHook, agent *store.Agent, trigger string) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("Panic in lifecycle hook executor (recovered)",
				"hook_id", hook.ID,
				"hook_name", hook.Name,
				"trigger", trigger,
				"agent_id", agent.ID,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()

	if err := e.executor.Execute(ctx, hook, agent, trigger); err != nil {
		e.log.Error("Lifecycle hook execution failed",
			"hook_id", hook.ID,
			"hook_name", hook.Name,
			"trigger", trigger,
			"agent_id", agent.ID,
			"error", err,
		)
	}
}
