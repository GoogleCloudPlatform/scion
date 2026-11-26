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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrMessageDeferred signals that broker dispatch failed transiently and should
// be retried. Consumed by dispatchWithBrokerRetry.
var ErrMessageDeferred = errors.New("message deferred: broker not locally reachable")

// ErrBrokerTimeout is returned by dispatchWithBrokerRetry when the broker
// remains unreachable after the context deadline. Callers map this to 504.
var ErrBrokerTimeout = errors.New("broker unreachable after deadline")

// ErrLifecycleDeferred is returned by HybridBrokerClient.StartAgent/StopAgent/
// RestartAgent when the broker is not locally connected and has no HTTP
// endpoint. The caller should serialize resolved params into a broker_dispatch
// row, signal the owning node via the command bus, and wait for the resulting
// agent status transition (design §5.4, §6.2).
var ErrLifecycleDeferred = errors.New("lifecycle deferred: broker not locally reachable")

// routeDecision is the outcome of HybridBrokerClient.route — how a dispatch for a
// broker should be delivered when this node does not hold the broker's socket.
type routeDecision int

const (
	// routeLocal: this node holds the broker's control-channel socket — tunnel
	// directly (the unchanged, zero-added-latency fast path).
	routeLocal routeDecision = iota
	// routeForward: some other node is believed to own the broker (affinity hint
	// is alive) — write durable intent + NOTIFY and let the owner self-select.
	routeForward
	// routeHTTP: no live owner, but the broker exposes a direct HTTP endpoint
	// (direct-mode broker; existing fallback — rare under NAT'd deployments).
	routeHTTP
	// routeUndeliverable: no owner and no endpoint — write durable pending intent
	// and return a retryable status; reconciled on the broker's next reconnect.
	routeUndeliverable
)

func (d routeDecision) String() string {
	switch d {
	case routeLocal:
		return "local"
	case routeForward:
		return "forward"
	case routeHTTP:
		return "http"
	default:
		return "undeliverable"
	}
}

// defaultAffinityFreshness bounds how long a broker's last_heartbeat is trusted
// as "owner alive" for routing. Generous (a multiple of the heartbeat interval);
// a stale hint only costs one dispatch timeout before falling through, and the
// reaper (B5-1) clears dead owners.
const defaultAffinityFreshness = 90 * time.Second

// route decides how to deliver a dispatch for brokerID. Stateless local broker
// identities represent a shared, replica-independent runtime capability and are
// always routed through this replica's local broker HTTP endpoint when present.
// Stateful brokers keep the existing control-channel and affinity behavior.
func (c *HybridBrokerClient) route(ctx context.Context, brokerID, brokerEndpoint string) routeDecision {
	if c.isStatelessLocalBroker(brokerID) {
		if brokerEndpoint != "" {
			return routeHTTP
		}
		return routeUndeliverable
	}
	if c.controlChannel.manager.IsConnected(brokerID) {
		return routeLocal
	}
	var owner string
	var alive bool
	if c.affinity != nil {
		owner, alive = c.affinity(ctx, brokerID)
	}
	switch {
	case owner != "" && alive:
		return routeForward
	case brokerEndpoint != "":
		return routeHTTP
	default:
		return routeUndeliverable
	}
}

// SetAffinityLookup injects the affinity hint used by route(). Wired by the
// server to a store-backed lookup (StoreAffinityLookup).
func (c *HybridBrokerClient) SetAffinityLookup(fn func(ctx context.Context, brokerID string) (owner string, alive bool)) {
	c.affinity = fn
}

// SetStatelessLocalBrokers marks co-located broker IDs whose runtime operations
// are replica-independent. Remote/stateful brokers must not be listed here.
func (c *HybridBrokerClient) SetStatelessLocalBrokers(ids []string) {
	if len(ids) == 0 {
		c.statelessLocalBrokers = nil
		return
	}
	c.statelessLocalBrokers = make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		c.statelessLocalBrokers[id] = struct{}{}
	}
}

func (c *HybridBrokerClient) isStatelessLocalBroker(brokerID string) bool {
	if c == nil || brokerID == "" || len(c.statelessLocalBrokers) == 0 {
		return false
	}
	_, ok := c.statelessLocalBrokers[brokerID]
	return ok
}

const (
	brokerRetryInitialBackoff = 500 * time.Millisecond
	brokerRetryMaxBackoff     = 5 * time.Second
)

// agentDeliveryMarker is the minimal store seam dispatchMarkedAgentMessage
// uses to stamp last_delivery_at and observe any active idle-suspend claim
// under the agent row's lock before DispatchAgentMessage runs.
type agentDeliveryMarker interface {
	MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error)
}

// idleSuspendGateChecker is an optional predicate on agentDeliveryMarker
// reporting whether lifecycle.idle_suspend is enabled. When false and the
// target carries no idle-suspend claim, markAgentForDispatch skips the
// row-locked write and claim check altogether so off-state behavior is
// identical to base (AC 8).
type idleSuspendGateChecker interface {
	IdleSuspendGateActive() bool
}

// autoSuspendChecker is an optional predicate on agentDeliveryMarker reporting
// whether this hub replica currently has an in-flight auto-suspend (stalled or
// idle) for agentID. Checked before MarkAgentDelivery so an in-flight stalled
// auto-suspend is intercepted even when lifecycle.idle_suspend is off.
type autoSuspendChecker interface {
	AutoSuspendingReason(agentID string) (state.SuspendReason, bool)
}

type gatedDeliveryMarker struct {
	store          agentDeliveryMarker
	gate           func() bool
	autoSuspending func(agentID string) (state.SuspendReason, bool)
}

func (m gatedDeliveryMarker) IdleSuspendGateActive() bool {
	return m.gate != nil && m.gate()
}

func (m gatedDeliveryMarker) AutoSuspendingReason(agentID string) (state.SuspendReason, bool) {
	if m.autoSuspending == nil {
		return "", false
	}
	return m.autoSuspending(agentID)
}

func (m gatedDeliveryMarker) MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error) {
	if m.store == nil {
		return nil, false, errors.New("delivery marker store is nil")
	}
	return m.store.MarkAgentDelivery(ctx, agentID)
}

// errDeliveryMarkFailed wraps a MarkAgentDelivery error (or nil marker/agent)
// so every message dispatch path fails closed instead of sending unmarked when
// the store cannot check or record the delivery handshake.
type errDeliveryMarkFailed struct {
	err error
}

func (e *errDeliveryMarkFailed) Error() string {
	return fmt.Sprintf("mark agent delivery before dispatch: %v", e.err)
}

func (e *errDeliveryMarkFailed) Unwrap() error { return e.err }

// errAgentIdleSuspending is returned by dispatchMarkedAgentMessage when
// MarkAgentDelivery observes an active idle-suspend claim on the locked row,
// observes a hub-suspended row, or observes that an agent the caller read as
// running/starting is no longer in running/starting. No broker dispatch ran.
type errAgentIdleSuspending struct {
	agent         *store.Agent
	claimActive   bool
	suspendReason state.SuspendReason
}

func (e *errAgentIdleSuspending) Error() string {
	if e != nil && e.agent != nil {
		if dmErr := validateAgentDeliverable(e.asSuspendedAgent()); dmErr != nil {
			return dmErr.Message
		}
		return fmt.Sprintf("agent %q is suspended", e.agent.Slug)
	}
	return "agent is suspended"
}

// asSuspendedAgent returns a copy of e.agent normalized to phase=suspended
// while an auto-suspend is still stopping the container, so non-auto-wake
// callers produce the exact refusal/failure message they emit for a suspended
// target.
func (e *errAgentIdleSuspending) asSuspendedAgent() *store.Agent {
	if e == nil || e.agent == nil {
		return nil
	}
	cp := *e.agent
	if e.claimActive {
		cp.Phase = string(state.PhaseSuspended)
		if e.suspendReason != "" {
			cp.SuspendReason = string(e.suspendReason)
		} else {
			cp.SuspendReason = string(state.SuspendReasonIdle)
		}
	}
	return &cp
}

// errNoWakeRecovery is an internal sentinel returned by a syncAutoWake hook
// when it re-reads the agent and finds it running and deliverable (no wake
// recovery applies). dispatchWithBrokerRetry handles it internally — retrying
// once when invoked from markAgentForDispatch (nothing dispatched yet), or
// returning the original broker error when invoked after a container-not-running
// broker error — so this sentinel never surfaces to callers or message rows.
var errNoWakeRecovery = errors.New("no wake recovery applicable")

// isContainerNotRunningDispatchError reports whether a synchronous broker
// DispatchAgentMessage error definitively indicates the target container was
// stopping or not running (so the message was NOT accepted by the broker).
// Ambiguous errors (timeouts, cancellations, 5xx) return false and are never
// retried.
func isContainerNotRunningDispatchError(err error) bool {
	if err == nil || isPreDispatchDeliveryError(err) {
		return false
	}
	if isBrokerAgentNotFound(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "container") && strings.Contains(msg, "is not running")
}

// errAutoWakeRefused wraps a synchronous auto-wake refusal or readiness timeout
// encountered inside dispatchWithBrokerRetry before any broker message dispatch.
type errAutoWakeRefused struct {
	dmErr    *AgentDMError
	awaitErr error
}

func (e *errAutoWakeRefused) Error() string {
	if e.dmErr != nil {
		return e.dmErr.Message
	}
	if e.awaitErr != nil {
		return e.awaitErr.Error()
	}
	return "auto-wake failed"
}

func (e *errAutoWakeRefused) Unwrap() error { return e.awaitErr }

// errAgentNotMessageable is returned by dispatchMarkedAgentMessage when the
// target agent is a shell execution container (ptone/scion#8812 §3.4 S5).
// No broker dispatch runs, and dispatchWithBrokerRetry never retries it.
type errAgentNotMessageable struct {
	agent *store.Agent
}

func (e *errAgentNotMessageable) Error() string {
	if e != nil && e.agent != nil {
		return shellAgentNotMessageableError(e.agent).Message
	}
	return "agent is an execution container and cannot receive messages"
}

// isPreDispatchDeliveryError reports whether err failed in the pre-dispatch
// delivery mark, claim gate, shell-agent gate, or auto-wake step before
// DispatchAgentMessage was called (so no broker delivery could be in flight).
func isPreDispatchDeliveryError(err error) bool {
	var (
		markErr   *errDeliveryMarkFailed
		idleErr   *errAgentIdleSuspending
		wakeErr   *errAutoWakeRefused
		notMsgErr *errAgentNotMessageable
	)
	return errors.As(err, &markErr) || errors.As(err, &idleErr) || errors.As(err, &wakeErr) || errors.As(err, &notMsgErr)
}

// markAgentForDispatch first checks any in-memory hub auto-suspend tracker on
// marker (for every dispatch caller, regardless of experiment state) and
// rejects with *errAgentIdleSuspending when active. Otherwise, whenever
// lifecycle.idle_suspend is enabled OR the target snapshot already carries an
// idle-suspend claim, it runs MarkAgentDelivery under the row lock, updates
// *agent with the fresh store snapshot (including store-clock
// IdleSuspendClaimActive), fails closed on any store error, and rejects
// dispatch with *errAgentIdleSuspending if an idle-suspend claim is active or
// a previously running/starting target moved out of running/starting since the
// caller's read. When no local auto-suspend is tracked, the experiment is off,
// and the row carries no claim, it returns nil immediately without touching the
// store (AC 8).
func markAgentForDispatch(ctx context.Context, marker agentDeliveryMarker, agent *store.Agent) error {
	if marker == nil {
		return &errDeliveryMarkFailed{err: errors.New("delivery marker store is nil")}
	}
	if agent == nil || agent.ID == "" {
		return &errDeliveryMarkFailed{err: errors.New("target agent is nil")}
	}
	if checker, ok := marker.(autoSuspendChecker); ok {
		if reason, active := checker.AutoSuspendingReason(agent.ID); active {
			return &errAgentIdleSuspending{agent: agent, claimActive: true, suspendReason: reason}
		}
	}
	hasRowClaim := agent.IdleSuspendClaimActive || agent.IdleSuspendClaimUntil != nil
	if gate, ok := marker.(idleSuspendGateChecker); ok && !gate.IdleSuspendGateActive() && !hasRowClaim {
		return nil
	}
	wasDeliverable := state.Phase(agent.Phase) == state.PhaseRunning || state.Phase(agent.Phase) == state.PhaseStarting
	wasRunning := state.Phase(agent.Phase) == state.PhaseRunning
	fresh, claimActive, err := marker.MarkAgentDelivery(ctx, agent.ID)
	if err != nil {
		return &errDeliveryMarkFailed{err: err}
	}
	if fresh == nil {
		return &errDeliveryMarkFailed{err: store.ErrNotFound}
	}
	*agent = *fresh
	freshDeliverable := state.Phase(fresh.Phase) == state.PhaseRunning || state.Phase(fresh.Phase) == state.PhaseStarting
	justIdleSuspended := wasRunning && state.Phase(fresh.Phase) == state.PhaseSuspended && state.SuspendReason(fresh.SuspendReason) == state.SuspendReasonIdle
	if claimActive || justIdleSuspended || (wasDeliverable && !freshDeliverable) {
		return &errAgentIdleSuspending{agent: fresh, claimActive: claimActive}
	}
	return nil
}

// dispatchMarkedAgentMessage is the hub's single choke point for
// AgentDispatcher.DispatchAgentMessage (enforced by
// TestMessageDispatchCallsMarkAgentDelivery). Every message dispatch stamps
// last_delivery_at and checks for an active idle-suspend claim through
// markAgentForDispatch before calling DispatchAgentMessage.
func dispatchMarkedAgentMessage(
	ctx context.Context,
	marker agentDeliveryMarker,
	recordLog *slog.Logger,
	beforeSend func(),
	dispatcher AgentDispatcher,
	agent *store.Agent,
	msg string,
	urgent bool,
	structuredMsg *messages.StructuredMessage,
) error {
	// Shell execution containers cannot receive messages from any path
	// (ptone/scion#8812 §3.4 S5). Enforced here at the single dispatch choke
	// point so notifications, inbound bridges, redrive, reconcile, and future
	// callers all fail closed before DispatchAgentMessage is reached.
	if isShellAgent(agent) {
		return &errAgentNotMessageable{agent: agent}
	}
	if err := markAgentForDispatch(ctx, marker, agent); err != nil {
		return err
	}
	if beforeSend != nil {
		beforeSend()
	}
	logMessageRecord(recordLog, agent, structuredMsg)
	return dispatcher.DispatchAgentMessage(ctx, agent, msg, effectiveInterrupt(agent, urgent), structuredMsg)
}

type dispatchRetryConfig struct {
	syncAutoWake          func(ctx context.Context, agent *store.Agent) (context.Context, context.CancelFunc, error)
	adjustUrgentAfterWake func(agent *store.Agent, urgent bool) bool
	beforeBrokerAttempt   func()
}

type dispatchRetryOption func(*dispatchRetryConfig)

func withSyncAutoWakeHook(fn func(ctx context.Context, agent *store.Agent) (context.Context, context.CancelFunc, error)) dispatchRetryOption {
	return func(cfg *dispatchRetryConfig) {
		cfg.syncAutoWake = fn
	}
}

func withAdjustUrgentAfterWake(fn func(agent *store.Agent, urgent bool) bool) dispatchRetryOption {
	return func(cfg *dispatchRetryConfig) {
		cfg.adjustUrgentAfterWake = fn
	}
}

func withBeforeBrokerAttempt(fn func()) dispatchRetryOption {
	return func(cfg *dispatchRetryConfig) {
		cfg.beforeBrokerAttempt = fn
	}
}

// dispatchWithBrokerRetry attempts to deliver a message to an agent via
// dispatchMarkedAgentMessage, retrying with exponential backoff when the
// broker is temporarily unreachable (ErrMessageDeferred). The caller must set
// a deadline on ctx (typically 30s). Returns nil on success, ErrBrokerTimeout
// if the deadline expires while still retrying, or the original error for
// non-transient failures (including *errAgentIdleSuspending on non-auto-wake
// paths and *errDeliveryMarkFailed when MarkAgentDelivery fails).
//
// recordLog is where the hub writes this message's record to the message
// log, once, right before the first broker attempt (see logMessageRecord).
// Callers that write their own record pass nil.
func dispatchWithBrokerRetry(
	ctx context.Context,
	marker agentDeliveryMarker,
	recordLog *slog.Logger,
	dispatcher AgentDispatcher,
	agent *store.Agent,
	msg string,
	urgent bool,
	structuredMsg *messages.StructuredMessage,
	opts ...dispatchRetryOption,
) error {
	var cfg dispatchRetryConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	attemptCtx := ctx
	backoff := brokerRetryInitialBackoff
	wokeOnce := false
	beforeAttemptRan := false
	nextLog := recordLog
	for {
		var hook func()
		if !beforeAttemptRan && cfg.beforeBrokerAttempt != nil {
			hook = func() {
				beforeAttemptRan = true
				cfg.beforeBrokerAttempt()
			}
		}
		err := dispatchMarkedAgentMessage(attemptCtx, marker, nextLog, hook, dispatcher, agent, msg, urgent, structuredMsg)
		if err == nil {
			return nil
		}
		var idleErr *errAgentIdleSuspending
		if errors.As(err, &idleErr) {
			if cfg.syncAutoWake != nil && !wokeOnce {
				wokeOnce = true
				newCtx, cancelNew, wakeErr := cfg.syncAutoWake(attemptCtx, agent)
				if cancelNew != nil {
					defer cancelNew()
				}
				if wakeErr != nil {
					if errors.Is(wakeErr, errNoWakeRecovery) {
						// The target is running and deliverable again (for example,
						// an in-flight auto-suspend aborted right after
						// markAgentForDispatch). Nothing has been dispatched yet,
						// so retry once instead of surfacing the internal sentinel.
						backoff = brokerRetryInitialBackoff
						continue
					}
					return wakeErr
				}
				if newCtx != nil {
					attemptCtx = newCtx
				}
				if cfg.adjustUrgentAfterWake != nil {
					urgent = cfg.adjustUrgentAfterWake(agent, urgent)
				}
				backoff = brokerRetryInitialBackoff
				continue
			}
			return err
		}
		if isContainerNotRunningDispatchError(err) && cfg.syncAutoWake != nil && !wokeOnce {
			wokeOnce = true
			nextLog = nil
			newCtx, cancelNew, wakeErr := cfg.syncAutoWake(attemptCtx, agent)
			if cancelNew != nil {
				defer cancelNew()
			}
			if wakeErr != nil {
				if errors.Is(wakeErr, errNoWakeRecovery) {
					return err
				}
				return wakeErr
			}
			if newCtx != nil {
				attemptCtx = newCtx
			}
			if cfg.adjustUrgentAfterWake != nil {
				urgent = cfg.adjustUrgentAfterWake(agent, urgent)
			}
			backoff = brokerRetryInitialBackoff
			continue
		}
		if isPreDispatchDeliveryError(err) {
			return err
		}
		nextLog = nil
		if !errors.Is(err, ErrMessageDeferred) {
			return err
		}
		select {
		case <-attemptCtx.Done():
			return ErrBrokerTimeout
		case <-time.After(backoff):
			backoff *= 2
			if backoff > brokerRetryMaxBackoff {
				backoff = brokerRetryMaxBackoff
			}
		}
	}
}

// agentStillInitializing reports whether a stored agent is running but its
// harness has not shown it reads input yet: the one case where
// state.AgentState.AcceptsInput is false for a running agent that reported
// an activity. A running agent with no activity at all is treated as before
// (images and brokers that predate the initializing activity).
func agentStillInitializing(agent *store.Agent) bool {
	return agent != nil && state.Phase(agent.Phase) == state.PhaseRunning &&
		state.Activity(agent.Activity) == state.ActivityInitializing
}

// effectiveInterrupt returns whether an interrupt/urgent message may be
// pasted at once. The broker delivers an interrupt immediately, bypassing
// its message buffer and that buffer's harness-readiness wait. An agent that
// is still initializing gets an ordinary buffered delivery instead, so the
// message waits for the harness rather than racing its startup. The message
// is still delivered; only the interrupt is dropped. Images that predate the
// initializing activity report working at once and keep their interrupt.
func effectiveInterrupt(agent *store.Agent, urgent bool) bool {
	if !urgent || !agentStillInitializing(agent) {
		return urgent
	}
	slog.Info("Delivering interrupt as a buffered message: agent is still initializing",
		"agent_id", agent.ID, "phase", agent.Phase, "activity", agent.Activity)
	return false
}

// StoreAffinityLookup returns an affinity lookup backed by runtime_brokers: the
// owner is connected_hub_id, and "alive" means last_heartbeat is within
// freshness. (Liveness is inferred from heartbeat freshness because there is no
// hub-to-hub addressability to ping a peer — design §5.3.)
func StoreAffinityLookup(st store.Store, freshness time.Duration) func(ctx context.Context, brokerID string) (string, bool) {
	if freshness <= 0 {
		freshness = defaultAffinityFreshness
	}
	return func(ctx context.Context, brokerID string) (string, bool) {
		b, err := st.GetRuntimeBroker(ctx, brokerID)
		if err != nil || b == nil || b.ConnectedHubID == nil || *b.ConnectedHubID == "" {
			return "", false
		}
		alive := !b.LastHeartbeat.IsZero() && time.Since(b.LastHeartbeat) < freshness
		return *b.ConnectedHubID, alive
	}
}

// logMessageRecord writes the hub's message record for structuredMsg to the
// message log. The rule: each message gets one record, written when the hub
// hands the message to delivery and before the outcome is known, so
// deferred and failed deliveries keep theirs. Outcome lines are tagged
// log_kind=event. A nil logger, agent or message writes nothing.
func logMessageRecord(l *slog.Logger, agent *store.Agent, structuredMsg *messages.StructuredMessage) {
	if l == nil || agent == nil || structuredMsg == nil {
		return
	}
	attrs := []any{"agent_id", agent.ID, "project_id", agent.ProjectID}
	attrs = append(attrs, structuredMsg.LogAttrs()...)
	l.Info("message handed to delivery", attrs...)
}
