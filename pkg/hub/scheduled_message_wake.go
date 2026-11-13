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
	"sync"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Scheduled message wake (ptone/scion#7734): a message event whose payload
// sets wake resumes a suspended target before delivery, through the shared
// direct-message wake path (wakeAgentForDM). Chat scheduled sends
// (chat_scheduled_send.go) never wake.

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
func (s *Server) authorizeScheduledWake(ctx context.Context, auth ScheduledAuthority, identity Identity, agent *store.Agent) error {
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

// wakeScheduledTarget runs the wake of a scheduled message fire whose
// authorization, standing and wake permission have passed. It returns nil
// when the target is ready for delivery (resumed, already running, or
// brought up by a start that was already in progress), or the error the
// event records. On an error nothing is dispatched.
//
// The wake runs on a budget of its own (scheduledWakeBudget), detached from
// the fire handler's deadline but keeping its values.
func (s *Server) wakeScheduledTarget(ctx context.Context, evt store.ScheduledEvent, agent *store.Agent) error {
	detached, cancelDetached := detachKeepFireAbort(ctx)
	defer cancelDetached()
	wakeCtx, cancel := context.WithTimeout(detached, scheduledWakeBudget)
	defer cancel()

	// A target already starting is not resumed (the shared wake path
	// refuses a starting agent): another start is bringing it up, so wait
	// for that start like a deferred wake. Delivering once it is ready is
	// a delivery to a running agent, which the message authorization
	// already covers; nothing is started here.
	if state.Phase(agent.Phase) == state.PhaseStarting {
		slog.Info("Scheduler: scheduled message target is already starting; waiting for it to become ready",
			"eventID", evt.ID, "agent_id", agent.ID)
		return s.awaitStartInProgress(wakeCtx, evt, agent)
	}

	result, dmErr := s.wakeAgentForDM(wakeCtx, agent)
	if dmErr != nil {
		slog.Warn("Scheduler: scheduled message wake failed",
			"eventID", evt.ID, "scheduleID", evt.ScheduleID, "agent_id", agent.ID,
			"code", dmErr.Code, "error", dmErr.Message)
		return fmt.Errorf("failed to wake agent %s: %s: %s", agent.Slug, dmErr.Code, dmErr.Message)
	}
	if result == nil || result.Outcome != WakeDeferred {
		return nil
	}

	// Another start (or a stop) holds the target's start claim. A
	// scheduled message has no stored row to keep and no sender to retry,
	// so wait for that start to bring the target up, then deliver; a target
	// that does not become ready fails the fire.
	slog.Info("Scheduler: scheduled message wake deferred; waiting for the start in progress",
		"eventID", evt.ID, "agent_id", agent.ID, "holder", string(agent.StartClaimKind))
	return s.awaitStartInProgress(wakeCtx, evt, agent)
}

// awaitStartInProgress waits, within ctx and wakeReadyTimeout for each
// step, for a start of the target that someone else began to bring it up:
// first for that start to leave the suspended phase, then for readiness. On
// success agent is refreshed to the row the start brought up, so the
// dispatch goes to the new run. A target that does not become ready returns
// errScheduledWakeNotReady.
func (s *Server) awaitStartInProgress(ctx context.Context, evt store.ScheduledEvent, agent *store.Agent) error {
	err := s.waitForStartToBegin(ctx, agent.ID, wakeReadyTimeout)
	var activity state.Activity
	if err == nil {
		activity, err = s.waitForAgentReady(ctx, agent.ID, wakeReadyTimeout)
	}
	if err != nil {
		slog.Warn("Scheduler: scheduled message target did not become ready after a start in progress",
			"eventID", evt.ID, "agent_id", agent.ID, "error", err)
		return errScheduledWakeNotReady
	}
	// Deliver to the run the other start brought up: a target that cannot
	// be re-read fails the fire rather than dispatching on the row read
	// before that start (its broker or phase may be stale).
	fresh, gerr := s.store.GetAgent(ctx, agent.ID)
	if gerr == nil && fresh == nil {
		gerr = store.ErrNotFound
	}
	if gerr != nil {
		slog.Warn("Scheduler: scheduled message target could not be re-read after a start in progress",
			"eventID", evt.ID, "agent_id", agent.ID, "error", gerr)
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
// (scheduledWakeFires), or it was still running when the shutdown grace ended.
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

// wakeFireRun is one scheduled wake fire in flight: recurring (scheduleID
// set) or one-shot (scheduleID empty). Its outcome is claimed and recorded
// once: either by the fire when it ends (runScheduleFire / fireEvent via
// tryRecord), or by the hub's shutdown for a fire still running when the
// shutdown stops waiting (recordAbandonedWakeFires via recordOrWait).
type wakeFireRun struct {
	eventID    string
	scheduleID string
	ranAt      time.Time

	mu      sync.Mutex
	claimed bool
	done    chan struct{}
}

// tryRecord claims the run's outcome and executes write when unclaimed, or
// returns false immediately without blocking when another caller already
// claimed the outcome.
func (r *wakeFireRun) tryRecord(write func()) bool {
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
func (r *wakeFireRun) recordOrWait(ctx context.Context, write func()) bool {
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
// Scheduler.fireEvent's final status persistence and shares its wakeFireRun
// claim with fireEvent without altering messageEventHandler's direct-call
// cleanup contract.
type oneShotFireScope struct {
	mu       sync.Mutex
	run      *wakeFireRun
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

func (sc *oneShotFireScope) attach(run *wakeFireRun, abortCtx context.Context, cleanup func()) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.run = run
	sc.abortCtx = abortCtx
	sc.cleanup = cleanup
}

func (sc *oneShotFireScope) state() (*wakeFireRun, bool) {
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

// wakeFireRuns is the set of wake fires (one-shot and recurring) in flight.
type wakeFireRuns struct {
	mu   sync.Mutex
	runs map[*wakeFireRun]struct{}
}

func (w *wakeFireRuns) add(r *wakeFireRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runs == nil {
		w.runs = make(map[*wakeFireRun]struct{})
	}
	w.runs[r] = struct{}{}
}

func (w *wakeFireRuns) remove(r *wakeFireRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.runs, r)
}

func (w *wakeFireRuns) snapshot() []*wakeFireRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*wakeFireRun, 0, len(w.runs))
	for r := range w.runs {
		out = append(out, r)
	}
	return out
}

// scheduledWakeFireAbortGrace bounds how long shutdown waits, after it
// aborts the recurring wake fires that outlived scheduledWakeFireStopGrace,
// for them to record their own outcome.
var scheduledWakeFireAbortGrace = 2 * time.Second

// trackScheduledWake joins a scheduled wake (and the dispatch after it) to
// the hub's shutdown tracking, the same mechanism for one-shot and
// recurring fires. A recurring wake fire is already tracked from
// executeSchedule (its ctx carries the fire abort): ctx is returned as is.
// A one-shot wake is admitted to scheduledWakeFires and registered, and its
// returned ctx ends, and carries the abort, when the shutdown aborts the
// tracked work. ok is false once the hub is shutting down: the wake must
// not start. The caller must call done when the wake and dispatch end.
//
// A one-shot fire records its own outcome (fireEvent, on a context that
// survives the scheduler's halt); the shutdown records the failure for one
// still running when it stops waiting.
func (s *Server) trackScheduledWake(ctx context.Context, evt store.ScheduledEvent) (context.Context, func(), bool) {
	if abort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context); abort != nil {
		return ctx, func() {}, true
	}
	trackCtx, endWork, ok := s.scheduledWakeFires.begin()
	if !ok {
		return ctx, func() {}, false
	}
	run := &wakeFireRun{eventID: evt.ID, ranAt: time.Now()}
	s.runningWakeFires.add(run)
	fireCtx, cancelFire := context.WithCancel(ctx)
	stop := context.AfterFunc(trackCtx, cancelFire)
	releaseRun := func() {
		s.runningWakeFires.remove(run)
		endWork()
	}
	if scope := oneShotFireScopeFromContext(ctx); scope != nil {
		scope.attach(run, trackCtx, releaseRun)
		return withScheduledFireAbort(fireCtx, trackCtx), func() {
			stop()
			cancelFire()
		}, true
	}
	return withScheduledFireAbort(fireCtx, trackCtx), func() {
		stop()
		cancelFire()
		releaseRun()
	}, true
}

// errScheduledWakeShuttingDown is recorded on a scheduled wake that did
// not start because the hub is shutting down.
var errScheduledWakeShuttingDown = errors.New("hub is shutting down; the wake fire did not run")

// scheduledWakeRecordBudget bounds the teardown's outcome writes for all
// the wake fires it records (recordAbandonedWakeFires) together.
var scheduledWakeRecordBudget = 5 * time.Second

// stopScheduledWakeFires is the background teardown's step for scheduled
// wake fires, one-shot and recurring: admission closes, running fires get
// scheduledWakeFireStopGrace to finish, then they are aborted and get
// scheduledWakeFireAbortGrace to record their outcome
// (errScheduledWakeShutdown). A fire still running after that (for example
// inside a broker launch the shared wake path keeps detached) has the same
// failure recorded for it, so its event does not stay fired with nothing
// delivered. It runs before the store closes.
func (s *Server) stopScheduledWakeFires(ctx context.Context) {
	n := s.scheduledWakeFires.stop(ctx, scheduledWakeFireStopGrace)
	if n == 0 {
		return
	}
	n = s.scheduledWakeFires.wait(ctx, scheduledWakeFireAbortGrace)
	if n == 0 {
		return
	}
	slog.Warn("Scheduled wake fires still running after the shutdown grace; recording them as not delivered",
		"running", n, "grace", scheduledWakeFireStopGrace+scheduledWakeFireAbortGrace)
	s.recordAbandonedWakeFires(ctx)
}

// recordAbandonedWakeFires records errScheduledWakeShutdown for every
// scheduled wake fire still in flight whose outcome is not recorded yet:
// on the event, and on the schedule for a recurring fire. All the writes
// share one deadline (scheduledWakeRecordBudget, or ctx's if sooner); once
// it passes the rest are skipped.
func (s *Server) recordAbandonedWakeFires(ctx context.Context) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledWakeRecordBudget)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		var cancelEarlier context.CancelFunc
		recordCtx, cancelEarlier = context.WithDeadline(recordCtx, deadline)
		defer cancelEarlier()
	}
	for _, r := range s.runningWakeFires.snapshot() {
		if recordCtx.Err() != nil {
			slog.Warn("Scheduled wake fire outcome not recorded: the shutdown record budget ran out",
				"event_id", r.eventID, "schedule_id", r.scheduleID)
			continue
		}
		if !r.recordOrWait(recordCtx, func() {
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

// scheduledWakeRecoveryBatchSize bounds each page read when recovering
// abandoned "firing" wake events.
const scheduledWakeRecoveryBatchSize = 200

// Timeouts shared between scheduled event execution, message dispatch, outcome
// persistence, and scheduledWakeStuckAfter so the shared-store recovery cutoff
// cannot drift below an active wake fire's budget.
const (
	scheduledEventHandlerTimeout    = 30 * time.Second
	scheduledMessageDispatchTimeout = 30 * time.Second
	scheduledEventRecordTimeout     = 10 * time.Second
	scheduledWakeStuckSafetyMargin  = time.Minute
)

// scheduledWakeStuckAfter returns the maximum legitimate lifetime of an active
// wake fire (pre-wake handler budget + wake budget + dispatch retry budget +
// completion write budget + safety margin). On a shared multi-replica store,
// only "firing" events older than this threshold are swept so another live
// replica's active wake fire is never clobbered.
func scheduledWakeStuckAfter() time.Duration {
	return scheduledEventHandlerTimeout + scheduledWakeBudget + scheduledMessageDispatchTimeout + scheduledEventRecordTimeout + scheduledWakeStuckSafetyMargin
}

// recoverAbandonedWakeFiresOnStart runs startup recovery before the scheduler
// starts. On SQLite (single hub instance) any "firing" row in the database
// belongs to a prior process that stopped, so all such rows are finalized
// immediately; on shared backends only rows older than scheduledWakeStuckAfter
// are finalized.
func (s *Server) recoverAbandonedWakeFiresOnStart(ctx context.Context) {
	_, dbDialect := s.storeDB()
	s.recoverAbandonedWakeFires(ctx, time.Now().UTC(), dbDialect == dialect.SQLite)
}

// recoverAbandonedWakeFires transitions wake events stranded in status
// "firing" to "failed" with errScheduledWakeAbandoned and, for recurring
// schedule fires, records the failed run on the parent schedule without
// rewinding next_run_at.
func (s *Server) recoverAbandonedWakeFires(ctx context.Context, now time.Time, immediate bool) {
	if s.store == nil {
		return
	}
	cutoff := now
	if !immediate {
		cutoff = now.Add(-scheduledWakeStuckAfter())
	}
	msg := errScheduledWakeAbandoned.Error()
	for {
		events, err := s.store.ListFiringScheduledEvents(ctx, cutoff, scheduledWakeRecoveryBatchSize)
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
		if len(events) < scheduledWakeRecoveryBatchSize || !progressed {
			return
		}
	}
}
