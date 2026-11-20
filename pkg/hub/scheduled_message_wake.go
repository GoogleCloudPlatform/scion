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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Scheduled message wake (ptone/scion#7734): a message event whose payload
// sets wake resumes a suspended target before delivery, through the shared
// direct-message wake path (wakeAgentForDM). Chat scheduled sends
// (chat_scheduled_send.go) have their own opt-in wake on the chat send path.

// scheduledWakePermission is the permission a scheduled wake exercises: the
// lifecycle permission a direct-message wake needs (wakeResumeDenial).
const scheduledWakePermission = "agent.lifecycle"

// scheduledWakeBudget bounds a scheduled wake (the resume dispatch and the
// readiness wait, or the wait on a start already in progress). It replaces
// the fire handler's deadline for the wake step, which is too short for a
// resume; the dispatch that follows gets its own retry bound. It matches the
// chat wake's resume budget (chatWakeResumeBudget).
//
// It is a variable only as a test seam. Tests that change it must not use
// t.Parallel and must restore the original value (t.Cleanup).
var scheduledWakeBudget = 90 * time.Second

// errScheduledWakeNotReady is recorded on a wake fire that found another
// start (or a stop) of the target in progress, and the target did not become
// ready within the wake budget. There is no sender to retry, so the fire
// fails rather than reporting a delivery that did not happen.
var errScheduledWakeNotReady = errors.New("target agent was already starting or stopping and did not become ready; message not delivered")

// scheduledPayloadWakes reports whether a stored message payload requests a
// wake. A payload that does not decode does not.
func scheduledPayloadWakes(raw string) bool {
	var p MessageEventPayload
	if raw == "" || json.Unmarshal([]byte(raw), &p) != nil {
		return false
	}
	return p.Wake
}

// scheduledWakeNeedsResume reports whether a wake of agent resumes it, the
// case the lifecycle permission is required for: its effective resting
// phase is suspended.
func scheduledWakeNeedsResume(agent *store.Agent) bool {
	return state.Phase(store.EffectiveRestingPhase(agent)) == state.PhaseSuspended
}

// authorizeScheduledWake decides a scheduled wake under the event's
// authorization revision, after the send itself was authorized
// (authorizeScheduledMessageFire). A target that the wake would resume needs
// both:
//   - the revision's frozen ceiling allowing agent.lifecycle, for user and
//     agent principals alike. An agent principal's identity is rebuilt from
//     the agent's current role, so only the frozen ceiling keeps a revision
//     authored without lifecycle authority from waking anyone after that
//     role widens. (agent.message has no such check for agents because no
//     agent scope covers it; agent.lifecycle is covered by
//     ScopeAgentLifecycle and so is recorded in agent ceilings.)
//   - the permission a direct-message wake needs (agentLifecycleAllowed)
//     for the revision's identity now.
//
// A target the wake would not resume needs nothing more. A denial returns an
// error naming the cause, for the log.
func (s *Server) authorizeScheduledWake(ctx context.Context, auth FireAuthority, identity Identity, agent *store.Agent) error {
	if !scheduledWakeNeedsResume(agent) {
		return nil
	}
	if !EffectCeilingAllows(auth.Ceiling, scheduledWakePermission, false) {
		return fmt.Errorf("%w: revision ceiling does not allow %s", errScheduledAuthorityDenied, scheduledWakePermission)
	}
	if !s.agentLifecycleAllowed(ctx, identity, agent) {
		return fmt.Errorf("scheduled_message_denied: not permitted to resume agent %s", agent.Slug)
	}
	return nil
}

// wakeFireTarget runs the wake of a deferred message fire whose authorization,
// standing, and wake permission have passed. It returns nil when the target is
// ready for delivery (resumed, already running, or brought up by a start that
// was already in progress), or the error the fire records. On an error nothing
// is dispatched.
//
// The wake runs on a budget of its own (scheduledWakeBudget), detached from
// the fire handler's deadline but keeping its values.
func (s *Server) wakeFireTarget(ctx context.Context, fireID, scheduleID string, agent *store.Agent) error {
	detached, cancelDetached := detachKeepFireAbort(ctx)
	defer cancelDetached()
	wakeCtx, cancel := context.WithTimeout(detached, scheduledWakeBudget)
	defer cancel()

	if state.Phase(agent.Phase) == state.PhaseStarting {
		slog.Info("Scheduler: scheduled message target is already starting; waiting for it to become ready",
			"eventID", fireID, "agent_id", agent.ID)
	}
	dmErr, awaitErr := s.wakeOrAwaitStart(wakeCtx, agent)
	if dmErr != nil {
		slog.Warn("Scheduler: scheduled message wake failed",
			"eventID", fireID, "scheduleID", scheduleID, "agent_id", agent.ID,
			"code", dmErr.Code, "error", dmErr.Message)
		return fmt.Errorf("failed to wake agent %s: %s: %s", agent.Slug, dmErr.Code, dmErr.Message)
	}
	if awaitErr != nil {
		slog.Warn("Scheduler: scheduled message target did not become ready after a start in progress",
			"eventID", fireID, "agent_id", agent.ID, "error", awaitErr)
		if errors.Is(awaitErr, errStartInProgressNotReady) || errors.Is(awaitErr, errIdleSuspendInProgressNotSettled) {
			return errScheduledWakeNotReady
		}
		return awaitErr
	}
	return nil
}

// errStartInProgressNotReady is awaitStartInProgress's answer when a start
// of the target that someone else began did not bring it up in time.
var errStartInProgressNotReady = errors.New("target agent was already starting and did not become ready")

// errIdleSuspendInProgressNotSettled is waitForIdleSuspendToSettle's answer
// when an in-flight idle auto-suspend did not finish stopping within the
// settle budget (or the caller's request/wake deadline).
var errIdleSuspendInProgressNotSettled = errors.New("target agent was suspending and did not finish stopping in time")

// idleSuspendSettleTimeoutOverride is a test-only seam overriding
// idleSuspendSettleBudget when positive. Tests that set it must not use
// t.Parallel and must restore it in t.Cleanup.
var idleSuspendSettleTimeoutOverride time.Duration

// idleSuspendClaimLease is the store lease for an idle auto-suspend claim:
// one stopWriteBudget plus 10s slack so the claim covers the full broker stop
// dispatch, status write, and credential revocation.
func idleSuspendClaimLease() time.Duration {
	return stopWriteBudget() + 10*time.Second
}

// idleSuspendSettleBudget bounds waitForIdleSuspendToSettle to the full
// idle-suspend claim lease (stopWriteBudget() + 10s); context.WithTimeout also
// caps the wait at any tighter caller/request deadline on ctx.
func idleSuspendSettleBudget() time.Duration {
	if idleSuspendSettleTimeoutOverride > 0 {
		return idleSuspendSettleTimeoutOverride
	}
	return idleSuspendClaimLease()
}

// wakeOrAwaitStart is wakeAgentForDM for a caller that must deliver after
// the wake rather than keep its item deferred: a target another start is
// already bringing up (phase starting, which wakeAgentForDM refuses, or a
// held start claim, WakeDeferred) is waited for with awaitStartInProgress
// instead. On success agent describes the ready run, as after
// wakeAgentForDM. dmErr is wakeAgentForDM's refusal or failure; awaitErr is
// awaitStartInProgress's (errStartInProgressNotReady, or a failed re-read).
// Nothing is started by the wait; the caller's delivery authorization
// covers delivering to the run another start brought up.
func (s *Server) wakeOrAwaitStart(ctx context.Context, agent *store.Agent) (dmErr *AgentDMError, awaitErr error) {
	if agent != nil && (agent.IdleSuspendClaimActive || agent.IdleSuspendClaimUntil != nil) {
		if err := s.waitForIdleSuspendToSettle(ctx, agent); err != nil {
			return nil, err
		}
		if state.Phase(agent.Phase) == state.PhaseRunning {
			return nil, nil
		}
		if s.autoWakeModeOf(agent) == autoWakeNone {
			return s.validateAgentDeliverable(agent), nil
		}
	}
	if state.Phase(agent.Phase) == state.PhaseStarting {
		return nil, s.awaitStartInProgress(ctx, agent)
	}
	result, dmErr := s.wakeAgentForDM(ctx, agent)
	if dmErr != nil {
		return dmErr, nil
	}
	if result == nil || result.Outcome != WakeDeferred {
		return nil, nil
	}
	s.messageLog.Info("wake: another start of the target is in progress; waiting for it",
		"agent_id", agent.ID, "holder", string(agent.StartClaimKind))
	return nil, s.awaitStartInProgress(ctx, agent)
}

// waitForIdleSuspendToSettle waits, up to idleSuspendSettleBudget (the claim
// lease, bounded by any tighter caller deadline on ctx), for an active
// idle-suspend claim on agent to clear (evaluated against the store clock via
// MarkAgentDelivery), refreshing *agent to the settled row.
func (s *Server) waitForIdleSuspendToSettle(ctx context.Context, agent *store.Agent) error {
	ctx, cancel := context.WithTimeout(ctx, idleSuspendSettleBudget())
	defer cancel()
	ticker := time.NewTicker(scheduledWakePollInterval)
	defer ticker.Stop()
	for {
		fresh, claimActive, err := s.store.MarkAgentDelivery(ctx, agent.ID)
		if err != nil {
			return fmt.Errorf("failed to check agent status during idle suspend: %w", err)
		}
		if fresh == nil {
			return store.ErrNotFound
		}
		*agent = *fresh
		if !claimActive {
			return nil
		}
		s.recoverExpiredIdleClaimOnDemand(ctx, fresh)
		if !fresh.IdleSuspendClaimLeaseActive && state.Phase(fresh.Phase) == state.PhaseRunning {
			// The lease expired and the row still runs: the stop never
			// landed, so waiting cannot help. Fail fast with the suspending
			// refusal; the sweep abandons the suspend after
			// idleSuspendAbandonAfter and the agent is deliverable again.
			return errIdleSuspendInProgressNotSettled
		}
		select {
		case <-ctx.Done():
			return errIdleSuspendInProgressNotSettled
		case <-ticker.C:
		}
	}
}

// startInProgressDMError is the direct-message answer to a wait on a start
// (or an in-flight idle suspend) already in progress that ended without a
// ready target (awaitErr from wakeOrAwaitStart).
func startInProgressDMError(agent *store.Agent, awaitErr error) *AgentDMError {
	if errors.Is(awaitErr, errIdleSuspendInProgressNotSettled) {
		return &AgentDMError{
			Code:       ErrCodeUnavailable,
			Message:    fmt.Sprintf("agent %q is suspending and did not finish stopping in time: %v", agent.Slug, awaitErr),
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	return &AgentDMError{
		Code:       ErrCodeUnavailable,
		Message:    fmt.Sprintf("agent %q was already starting and did not become ready: %v", agent.Slug, awaitErr),
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

// awaitStartInProgress waits, within ctx and wakeReadyTimeout for each
// step, for a start of the target that someone else began to bring it up:
// first for that start to leave the suspended phase, then for readiness. On
// success agent is refreshed to the row the start brought up, so the
// dispatch goes to the new run. A target that does not become ready returns
// errStartInProgressNotReady; one that cannot be re-read afterwards returns
// the read error, so nothing is dispatched on the row read before that
// start (its broker or phase may be stale).
func (s *Server) awaitStartInProgress(ctx context.Context, agent *store.Agent) error {
	err := s.waitForStartToBegin(ctx, agent.ID, wakeReadyTimeout)
	var activity state.Activity
	if err == nil {
		activity, err = s.waitForAgentReady(ctx, agent.ID, wakeReadyTimeout)
	}
	if err != nil {
		s.messageLog.Warn("wake: the start in progress did not bring the target up",
			"agent_id", agent.ID, "error", err)
		return errStartInProgressNotReady
	}
	fresh, gerr := s.store.GetAgent(ctx, agent.ID)
	if gerr == nil && fresh == nil {
		gerr = store.ErrNotFound
	}
	if gerr != nil {
		return fmt.Errorf("failed to refresh agent %s after the start in progress: %w", agent.Slug, gerr)
	}
	*agent = *fresh
	agent.Activity = string(activity)
	return nil
}

// scheduledWakePollInterval is how often waitForStartToBegin re-reads the
// target.
const scheduledWakePollInterval = 100 * time.Millisecond

// waitForStartToBegin waits, up to timeout, while agentID is still at rest
// in the suspended phase: a start that holds the start claim may not have
// written starting yet, and waitForAgentReady treats a suspended row as a
// failed start.
func (s *Server) waitForStartToBegin(ctx context.Context, agentID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		a, err := s.store.GetAgent(ctx, agentID)
		if err != nil {
			return fmt.Errorf("failed to check agent status: %w", err)
		}
		if !scheduledWakeNeedsResume(a) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the start in progress did not begin")
		case <-time.After(scheduledWakePollInterval):
		}
	}
}

// errScheduledWakeShutdown is recorded on a wake fire (one-shot or recurring)
// that the hub's shutdown cut short: the fire was aborted
// (scheduledDetachedFires), or it was still running when the shutdown grace ended.
var errScheduledWakeShutdown = errors.New("hub shut down during the wake; message not delivered")

// errScheduledWakeAbandoned is recorded when recovery finalizes a wake event
// left in provisional status "firing" because a prior hub stopped before its
// outcome could be written.
var errScheduledWakeAbandoned = errors.New("hub stopped before the wake fire completed")

// scheduledFireAbortKey carries a recurring wake fire's abort context: it
// ends when the hub's shutdown gives up waiting for the fire.
type scheduledFireAbortKey struct{}

// withScheduledFireAbort returns ctx carrying abort as the fire's abort
// context.
func withScheduledFireAbort(ctx, abort context.Context) context.Context {
	return context.WithValue(ctx, scheduledFireAbortKey{}, abort)
}

// scheduledFireAborted reports whether ctx carries a fire abort context that
// has ended.
func scheduledFireAborted(ctx context.Context) bool {
	abort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context)
	return abort != nil && abort.Err() != nil
}

// detachKeepFireAbort returns ctx without its cancellation and deadline, as
// context.WithoutCancel does, but still cancelled when the fire's abort
// context (if any) ends: the wake and its dispatch outlive the fire
// handler's deadline, not the hub's shutdown. The caller must call the
// returned cancel func.
func detachKeepFireAbort(ctx context.Context) (context.Context, context.CancelFunc) {
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	abort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context)
	if abort == nil {
		return detached, cancel
	}
	stop := context.AfterFunc(abort, cancel)
	return detached, func() {
		stop()
		cancel()
	}
}

// detachedFireRun is one scheduled wake fire in flight: recurring (scheduleID
// set) or one-shot (scheduleID empty). Its outcome is claimed and recorded
// once: either by the fire when it ends (runScheduleFire / fireEvent via
// tryRecord), or by the hub's shutdown for a fire still running when the
// shutdown stops waiting (recordAbandonedDetachedFires via recordOrWait).
type detachedFireRun struct {
	eventID    string
	scheduleID string
	ranAt      time.Time
	isExec     bool

	mu      sync.Mutex
	claimed bool
	done    chan struct{}
}

// tryRecord claims the run's outcome and executes write when unclaimed, or
// returns false immediately without blocking when another caller already
// claimed the outcome.
func (r *detachedFireRun) tryRecord(write func()) bool {
	r.mu.Lock()
	if r.claimed {
		r.mu.Unlock()
		return false
	}
	r.claimed = true
	done := make(chan struct{})
	r.done = done
	r.mu.Unlock()
	defer close(done)
	write()
	return true
}

// recordOrWait claims the run's outcome and executes write when unclaimed.
// When the fire's own completion write is already in flight, it waits up to
// ctx's deadline for that write to finish instead of blocking unboundedly,
// returning whether the outcome write completed in time.
func (r *detachedFireRun) recordOrWait(ctx context.Context, write func()) bool {
	r.mu.Lock()
	if !r.claimed {
		r.claimed = true
		done := make(chan struct{})
		r.done = done
		r.mu.Unlock()
		defer close(done)
		write()
		return true
	}
	done := r.done
	r.mu.Unlock()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

type oneShotFireScopeKey struct{}

// oneShotFireScope extends a one-shot wake fire's shutdown tracking across
// Scheduler.fireEvent's final status persistence and shares its detachedFireRun
// claim with fireEvent without altering messageEventHandler's direct-call
// cleanup contract.
type oneShotFireScope struct {
	mu       sync.Mutex
	run      *detachedFireRun
	abortCtx context.Context
	cleanup  func()
}

func withOneShotFireScope(ctx context.Context) (context.Context, *oneShotFireScope) {
	scope := &oneShotFireScope{}
	return context.WithValue(ctx, oneShotFireScopeKey{}, scope), scope
}

func oneShotFireScopeFromContext(ctx context.Context) *oneShotFireScope {
	scope, _ := ctx.Value(oneShotFireScopeKey{}).(*oneShotFireScope)
	return scope
}

func (sc *oneShotFireScope) attach(run *detachedFireRun, abortCtx context.Context, cleanup func()) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.run = run
	sc.abortCtx = abortCtx
	sc.cleanup = cleanup
}

func (sc *oneShotFireScope) state() (*detachedFireRun, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	aborted := sc.abortCtx != nil && sc.abortCtx.Err() != nil
	return sc.run, aborted
}

func (sc *oneShotFireScope) finish() {
	sc.mu.Lock()
	cleanup := sc.cleanup
	sc.cleanup = nil
	sc.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

// detachedFireRuns is the set of wake fires (one-shot and recurring) in flight.
type detachedFireRuns struct {
	mu   sync.Mutex
	runs map[*detachedFireRun]struct{}
}

func (w *detachedFireRuns) add(r *detachedFireRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runs == nil {
		w.runs = make(map[*detachedFireRun]struct{})
	}
	w.runs[r] = struct{}{}
}

func (w *detachedFireRuns) remove(r *detachedFireRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.runs, r)
}

func (w *detachedFireRuns) snapshot() []*detachedFireRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*detachedFireRun, 0, len(w.runs))
	for r := range w.runs {
		out = append(out, r)
	}
	return out
}

// scheduledDetachedFireAbortGrace bounds how long shutdown waits, after it
// aborts the recurring wake fires that outlived scheduledDetachedFireStopGrace,
// for them to record their own outcome.
var scheduledDetachedFireAbortGrace = 2 * time.Second

// errScheduledWakeShuttingDown is recorded on a scheduled wake that did
// not start because the hub is shutting down.
var errScheduledWakeShuttingDown = errors.New("hub is shutting down; the wake fire did not run")

// scheduledDetachedRecordBudget bounds the teardown's outcome writes for all
// the wake fires it records (recordAbandonedDetachedFires) together.
var scheduledDetachedRecordBudget = 5 * time.Second

// stopScheduledDetachedFires is the background teardown's step for scheduled
// wake fires, one-shot and recurring: admission closes, running fires get
// scheduledDetachedFireStopGrace to finish, then they are aborted and get
// scheduledDetachedFireAbortGrace to record their outcome
// (errScheduledWakeShutdown). A fire still running after that (for example
// inside a broker launch the shared wake path keeps detached) has the same
// failure recorded for it, so its event does not stay fired with nothing
// delivered. It runs before the store closes.
func (s *Server) stopScheduledDetachedFires(ctx context.Context) {
	n := s.scheduledDetachedFires.stop(ctx, scheduledDetachedFireStopGrace)
	if n == 0 {
		return
	}
	n = s.scheduledDetachedFires.wait(ctx, scheduledDetachedFireAbortGrace)
	if n == 0 {
		return
	}
	slog.Warn("Scheduled wake fires still running after the shutdown grace; recording them as not delivered",
		"running", n, "grace", scheduledDetachedFireStopGrace+scheduledDetachedFireAbortGrace)
	s.recordAbandonedDetachedFires(ctx)
}

// recordAbandonedDetachedFires records errScheduledWakeShutdown for every
// scheduled wake fire still in flight whose outcome is not recorded yet:
// on the event, and on the schedule for a recurring fire. All the writes
// share one deadline (scheduledDetachedRecordBudget, or ctx's if sooner); once
// it passes the rest are skipped.
func (s *Server) recordAbandonedDetachedFires(ctx context.Context) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledDetachedRecordBudget)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		var cancelEarlier context.CancelFunc
		recordCtx, cancelEarlier = context.WithDeadline(recordCtx, deadline)
		defer cancelEarlier()
	}
	for _, r := range s.runningDetachedFires.snapshot() {
		if recordCtx.Err() != nil {
			slog.Warn("Scheduled wake fire outcome not recorded: the shutdown record budget ran out",
				"event_id", r.eventID, "schedule_id", r.scheduleID)
			continue
		}
		if !r.recordOrWait(recordCtx, func() {
			if r.isExec {
				if _, err := s.store.CompleteUnadmittedExecEvent(
					recordCtx, r.eventID, store.ScheduledEventFailed, execReasonShuttingDown, r.scheduleID, r.ranAt,
				); err != nil {
					slog.Warn("Scheduled exec fire outcome write failed during shutdown",
						"event_id", r.eventID, "schedule_id", r.scheduleID, "error", err)
				}
				return
			}
			msg := errScheduledWakeShutdown.Error()
			if _, err := s.store.CompleteFiringScheduledEvent(
				recordCtx, r.eventID, store.ScheduledEventFailed, time.Now(), msg, r.scheduleID, r.ranAt,
			); err != nil {
				slog.Warn("Scheduled wake fire outcome write failed during shutdown",
					"event_id", r.eventID, "schedule_id", r.scheduleID, "error", err)
			}
		}) {
			slog.Warn("Scheduled wake fire outcome write still in flight when the shutdown record budget ran out",
				"event_id", r.eventID, "schedule_id", r.scheduleID)
		}
	}
}

// scheduledDetachedRecoveryBatchSize bounds each page read when recovering
// abandoned "firing" wake events.
const scheduledDetachedRecoveryBatchSize = 200

// Timeouts shared between scheduled event execution, message dispatch, outcome
// persistence, and scheduledDetachedStuckAfter so the shared-store recovery cutoff
// cannot drift below an active wake fire's budget.
const (
	scheduledEventHandlerTimeout       = 30 * time.Second
	scheduledMessageDispatchTimeout    = 30 * time.Second
	scheduledEventRecordTimeout        = 10 * time.Second
	scheduledDetachedStuckSafetyMargin = time.Minute
)

// scheduledDetachedStuckAfter returns the maximum legitimate lifetime of an active
// wake fire (pre-wake handler budget + wake budget + dispatch retry budget +
// completion write budget + safety margin). On a shared multi-replica store,
// only "firing" events older than this threshold are swept so another live
// replica's active wake fire is never clobbered.
func scheduledDetachedStuckAfter() time.Duration {
	return scheduledEventHandlerTimeout + scheduledWakeBudget + scheduledMessageDispatchTimeout + scheduledEventRecordTimeout + scheduledDetachedStuckSafetyMargin
}

// recoverAbandonedDetachedFiresOnStart runs startup recovery before the scheduler
// starts. On SQLite (single hub instance) any "firing" row in the database
// belongs to a prior process that stopped, so all such rows are finalized
// immediately; on shared backends only rows older than scheduledDetachedStuckAfter
// are finalized.
func (s *Server) recoverAbandonedDetachedFiresOnStart(ctx context.Context) {
	_, dbDialect := s.storeDB()
	s.recoverAbandonedDetachedFires(ctx, time.Now().UTC(), dbDialect == dialect.SQLite)
}

// recoverAbandonedDetachedFires transitions wake events stranded in status
// "firing" to "failed" with errScheduledWakeAbandoned and, for recurring
// schedule fires, records the failed run on the parent schedule without
// rewinding next_run_at.
func (s *Server) recoverAbandonedDetachedFires(ctx context.Context, now time.Time, immediate bool) {
	if s.store == nil {
		return
	}
	cutoff := now
	if !immediate {
		cutoff = now.Add(-scheduledDetachedStuckAfter())
	}
	msg := errScheduledWakeAbandoned.Error()
	for {
		events, err := s.store.ListFiringScheduledEvents(ctx, cutoff, scheduledDetachedRecoveryBatchSize)
		if err != nil {
			slog.Warn("Scheduler: failed to list firing scheduled events for recovery", "error", err)
			return
		}
		if len(events) == 0 {
			return
		}
		progressed := false
		for _, evt := range events {
			ranAt := evt.FireAt
			if evt.FiredAt != nil {
				ranAt = *evt.FiredAt
			}
			updated, err := s.store.CompleteFiringScheduledEvent(
				ctx, evt.ID, store.ScheduledEventFailed, now, msg, evt.ScheduleID, ranAt,
			)
			if err != nil {
				slog.Warn("Scheduler: failed to recover abandoned wake fire",
					"event_id", evt.ID, "schedule_id", evt.ScheduleID, "error", err)
				continue
			}
			if updated {
				progressed = true
				slog.Warn("Scheduler: recovered abandoned wake fire",
					"event_id", evt.ID, "schedule_id", evt.ScheduleID)
			}
		}
		if len(events) < scheduledDetachedRecoveryBatchSize || !progressed {
			return
		}
	}
}
