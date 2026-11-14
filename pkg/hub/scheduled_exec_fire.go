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
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Fire flow of exec events (ptone/scion#7883, design "Fire flow"):
//
//	execEventHandler (one-shot: fireEvent, after the pending→fired claim)
//	  flag off → skipped; payload invalid → failed; > 10 min late → skipped;
//	  authorizeScheduledExecFire denied → failed "exec refused";
//	  target not running / reincarnating / managed → skipped;
//	  tracked-run bound full → skipped "capacity";
//	  INSERT scheduled_exec_runs (starting); exec-start, one attempt
//	  (StartAgentExecRun: launch-pinned, gated on both hub flags);
//	  then a detached tracker polls exec-status until the run finishes or
//	  its lease ends, and records the outcome with a compare-and-set.
//	the run sweeper (singleton, every minute) polls the unfinished runs no
//	  tracker on this replica owns, so a hub restart still records the real
//	  outcome, and records runs past their lease as abandoned.
//
// The hub never resends exec-start: a run whose start is ambiguous (timeout
// or transport error) is only ever polled.

// Run tracking bounds. Variables only as test seams: tests that change them
// must not use t.Parallel and must restore them (t.Cleanup).
var (
	// scheduledExecMaxTracked bounds the runs one replica tracks at once
	// (inline fires and trackers). A fire that finds it full is skipped
	// (capacity); it never waits.
	scheduledExecMaxTracked = 500
	// scheduledExecPollInitial and scheduledExecPollMax are a tracker's
	// first and longest exec-status interval.
	scheduledExecPollInitial = 5 * time.Second
	scheduledExecPollMax     = 15 * time.Second
	// scheduledExecSweepBatch, scheduledExecSweepMaxPages and
	// scheduledExecSweepInFlight bound one sweep: pages of unfinished runs,
	// pages per sweep, and concurrent exec-status calls.
	scheduledExecSweepBatch    = 100
	scheduledExecSweepMaxPages = 10
	scheduledExecSweepInFlight = 32
	// scheduledExecStopGrace bounds how long shutdown waits for exec fires
	// still at exec-start before it cancels them and the trackers.
	scheduledExecStopGrace = 2 * time.Second
)

// scheduledExecStartUnknownAfter is how long after a run's insert a 404
// from exec-status is definitive for a run never seen running: the start
// can no longer be admitted. The hub sets not_after_unix_ms to its send
// time plus ExecNotAfter, the send follows the insert within the call
// timeout, and sciontool spawns only while now <= not_after + tolerance;
// the design's extra 10s follows. A variable only as a test seam.
var scheduledExecStartUnknownAfter = agentcontrol.HubExecCallTimeout + agentcontrol.ExecNotAfter +
	agentcontrol.ExecClockTolerance + 10*time.Second

// scheduledExecRunTracker is the replica's in-memory state of exec runs.
// The zero value is ready to use.
type scheduledExecRunTracker struct {
	// work admits the inline fires and the trackers and joins them at
	// shutdown (closeAdmission, then wait cancels what is left).
	work brokerWorkTracker

	mu    sync.Mutex
	owned map[string]struct{} // event IDs a fire or tracker of this replica owns
	used  int                 // tracked-run slots in use
}

// acquire takes a tracked-run slot for eventID and marks it owned. It never
// waits: false when the bound is full or the run is already owned.
func (t *scheduledExecRunTracker) acquire(eventID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.used >= scheduledExecMaxTracked {
		return false
	}
	if _, ok := t.owned[eventID]; ok {
		return false
	}
	if t.owned == nil {
		t.owned = make(map[string]struct{})
	}
	t.owned[eventID] = struct{}{}
	t.used++
	return true
}

// release frees eventID's slot and ownership.
func (t *scheduledExecRunTracker) release(eventID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.owned[eventID]; !ok {
		return
	}
	delete(t.owned, eventID)
	t.used--
}

// owns reports whether a fire or tracker of this replica owns eventID.
func (t *scheduledExecRunTracker) owns(eventID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.owned[eventID]
	return ok
}

// stopScheduledExecRuns is the background teardown's step for exec runs:
// admission is already closed (haltDispatchProducers); fires still at
// exec-start get scheduledExecStopGrace, then everything left, the trackers
// included, is cancelled. No outcome is recorded for an admitted run: its
// row stays unfinished and its event fired, and the sweeper records the
// real outcome after the restart (design D5, D9 review).
func (s *Server) stopScheduledExecRuns(ctx context.Context) {
	if n := s.execRuns.work.stop(ctx, scheduledExecStopGrace); n > 0 {
		slog.Info("Scheduled exec runs still tracked at shutdown; the sweeper records their outcome after the restart",
			"running", n)
	}
}

// execEventHandler returns the EventHandler of exec events.
func (s *Server) execEventHandler() EventHandler {
	return func(ctx context.Context, evt store.ScheduledEvent) error {
		return s.fireScheduledExec(ctx, evt)
	}
}

// fireScheduledExec runs one exec event fire. A returned scheduledSkip
// records the event skipped; another error records it failed with the
// error's (generic) text; nil records it fired, and a tracker started after
// that write records the run's outcome.
func (s *Server) fireScheduledExec(ctx context.Context, evt store.ScheduledEvent) error {
	log := slog.With("subsystem", "scheduler", "eventID", evt.ID, "projectID", evt.ProjectID)
	if evt.ScheduleID != "" {
		// Recurring exec schedules are Phase 2 (ptone/scion#7884) and
		// cannot be authored; a stored one never runs.
		log.Warn("Scheduler: recurring exec event refused", "scheduleID", evt.ScheduleID)
		return errors.New(execReasonRecurring)
	}

	p, err := decodeScheduledExecPayload(evt.Payload)
	if err == nil && p.AgentID == "" {
		err = &scheduledExecPayloadError{field: "agentId"}
	}
	if err != nil {
		// Fail closed on a stored row that no longer validates.
		log.Warn("Scheduler: exec event payload invalid", "error", err)
		return errScheduledExecPayloadInvalid
	}

	skip := func(reason, detail string) error {
		a := newScheduledExecFireAudit(ctx, ScheduledExecAuditSkipped, evt, p)
		a.Reason = detail
		s.auditScheduledExec(ctx, a)
		log.Info("Scheduler: exec event skipped", "agent_id", p.AgentID, "reason", detail)
		return skipScheduledFire(reason)
	}

	if !s.execSchedulesEnabled() {
		return skip(execReasonDisabled, "experiment_off")
	}
	if !evt.FireAt.IsZero() && time.Since(evt.FireAt) > scheduledExecMaxWindow {
		return skip(execReasonMissedWindow, "missed_window")
	}

	auth, _, target, err := s.authorizeScheduledExecFire(ctx, evt, p)
	if err != nil {
		a := newScheduledExecFireAudit(ctx, ScheduledExecAuditRefused, evt, p)
		a.Reason = err.Error()
		s.auditScheduledExec(ctx, a)
		log.Warn("Scheduler: exec event refused at fire time",
			"agent_id", p.AgentID,
			"principal_kind", auth.PrincipalKind, "principal_id", auth.PrincipalID,
			"authorization_revision", evt.AuthorizationRevision, "error", err)
		return errors.New(execReasonRefused)
	}

	// Preflight (D4): running targets only. The phase reasons are the
	// shared deliverability rule's (validateAgentDeliverable,
	// ptone/scion#7825); the event records the generic text.
	if isManagedAgentRuntime(target.Runtime) {
		return skip(execReasonTargetNotRunning, "managed_runtime")
	}
	if phaseErr := validateAgentDeliverable(target); phaseErr != nil {
		return skip(execReasonTargetNotRunning, "phase_"+store.EffectiveRestingPhase(target))
	}
	if reincarnationInFlight(target) {
		return skip(execReasonTargetNotRunning, "reincarnating")
	}

	// Admission: the replica's tracked-run bound (never waits) and the
	// shutdown gate. The slot and ownership pass to the tracker, or are
	// released when no tracker starts.
	if !s.execRuns.acquire(evt.ID) {
		return skip(execReasonCapacity, "tracked_run_capacity")
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.execRuns.release(evt.ID)
		}
	}()
	workCtx, endWork, admitted := s.execRuns.work.begin()
	if !admitted {
		log.Warn("Scheduler: exec event not started: the hub is shutting down")
		return errors.New(execReasonShuttingDown)
	}
	defer endWork()

	executor, _ := ExecutorContextFromContext(ctx)
	now := time.Now()
	timeout := p.timeout()
	run := store.ScheduledExecRun{
		EventID:                 evt.ID,
		ProjectID:               evt.ProjectID,
		TargetAgentID:           target.ID,
		InitiatorPrincipalKind:  evt.InitiatorPrincipalKind,
		InitiatorPrincipalID:    evt.InitiatorPrincipalID,
		InitiatorCredentialKind: evt.InitiatorCredentialKind,
		InitiatorCredentialID:   evt.InitiatorCredentialID,
		AuthorizationRevision:   evt.AuthorizationRevision,
		ExecutorKind:            executor.Kind,
		ExecutorID:              executor.ID,
		CommandSHA256:           agentcontrol.ExecArgvDigest(p.Command),
		Argc:                    len(p.Command),
		TimeoutSeconds:          timeout,
		StartedAt:               now,
		LeaseUntil:              now.Add(time.Duration(timeout)*time.Second + scheduledExecLeaseGrace),
		Outcome:                 store.ScheduledExecOutcomeStarting,
		Runtime:                 target.Runtime,
		LaunchID:                target.RunID,
	}
	if err := s.store.CreateScheduledExecRun(ctx, &run); err != nil {
		// Nothing was sent. An existing run means this event was admitted
		// before: it is never started again.
		log.Error("Scheduler: exec run record not written; nothing started", "error", err)
		return errors.New(execReasonDispatchFailed)
	}
	s.auditScheduledExec(ctx, newScheduledExecFireAudit(ctx, ScheduledExecAuditAdmit, evt, p))

	// One exec-start attempt. It runs past the fire handler's deadline on
	// the call's own timeout, and is cancelled only when the shutdown gives
	// up waiting (workCtx); an ambiguous result is only ever polled.
	startCtx, cancelStart := context.WithCancel(context.WithoutCancel(ctx))
	stopStart := context.AfterFunc(workCtx, cancelStart)
	res, startErr := s.StartAgentExecRun(startCtx, target.ID, target.RunID, agentcontrol.ExecStartRequest{
		RunID:    evt.ID,
		Argv:     p.Command,
		TimeoutS: timeout,
	})
	stopStart()
	cancelStart()

	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
	defer cancelRecord()
	finish := func(outcome string, res *agentcontrol.ExecResult) {
		s.finishScheduledExecRun(recordCtx, run, outcome, res, "", "")
	}

	if startErr != nil {
		switch {
		case errors.Is(startErr, ErrExecSchedulesDisabled):
			finish(store.ScheduledExecOutcomeSkipped, nil)
			return skip(execReasonDisabled, "experiment_off_at_start")
		case errors.Is(startErr, router.ErrNoSession), errors.Is(startErr, ErrAgentControlNotAdvertised),
			errors.Is(startErr, ErrAgentExecLaunchChanged), errors.Is(startErr, store.ErrNotFound):
			finish(store.ScheduledExecOutcomeSkipped, nil)
			return skip(execReasonUnavailable, "executor_unavailable")
		case isExecFieldError(startErr):
			finish(store.ScheduledExecOutcomeError, nil)
			return errScheduledExecPayloadInvalid
		case errors.Is(startErr, agentcontrol.ErrExecSchema):
			log.Warn("Scheduler: exec-start answer outside the schema", "error", startErr)
			finish(store.ScheduledExecOutcomeError, nil)
			return errors.New(execReasonDispatchFailed)
		default:
			// Timeout or transport error after the send: the run may have
			// started. Never resend; the tracker resolves it.
			log.Warn("Scheduler: exec-start result unknown; tracking the run", "error", startErr)
			handedOff = true
			afterScheduledRecord(ctx, func() { s.trackScheduledExecRun(run) })
			return nil
		}
	}

	switch res.Status {
	case http.StatusAccepted, http.StatusOK:
		if res.Finished() {
			// start_failed at 202, or a replay of a finished run.
			outcome, status, reason := scheduledExecFinalOutcome(res, timeout)
			finish(outcome, &res)
			if status == store.ScheduledEventFailed {
				return errors.New(reason)
			}
			return nil
		}
		if _, err := s.store.MarkScheduledExecRunRunning(recordCtx, evt.ID); err == nil {
			run.Outcome = store.ScheduledExecOutcomeRunning
		}
		handedOff = true
		afterScheduledRecord(ctx, func() { s.trackScheduledExecRun(run) })
		return nil
	case http.StatusConflict:
		switch res.Outcome {
		case agentcontrol.ExecOutcomeOverlap:
			finish(store.ScheduledExecOutcomeSkipped, nil)
			return skip(execReasonOverlap, "agent_overlap")
		case agentcontrol.ExecOutcomeBusy:
			finish(store.ScheduledExecOutcomeSkipped, nil)
			return skip(execReasonCapacity, "agent_busy")
		default: // mismatch
			finish(store.ScheduledExecOutcomeError, nil)
			return errors.New(execReasonDispatchFailed)
		}
	case http.StatusRequestTimeout:
		finish(store.ScheduledExecOutcomeError, nil)
		return errors.New(execReasonStartExpired)
	default: // 400
		finish(store.ScheduledExecOutcomeError, nil)
		return errScheduledExecPayloadInvalid
	}
}

// isExecFieldError reports whether err is an exec request field error (the
// hub refused to send an out-of-bounds request).
func isExecFieldError(err error) bool {
	var fe *agentcontrol.ExecFieldError
	return errors.As(err, &fe)
}

// scheduledExecFinalOutcome maps a finished run's answer to the run outcome,
// the event status and the event's generic text (D6): ok leaves the event
// fired; nonzero, timeout and start_failed fail it.
func scheduledExecFinalOutcome(res agentcontrol.ExecResult, timeoutSeconds int) (outcome, eventStatus, reason string) {
	switch res.Outcome {
	case agentcontrol.ExecOutcomeOK:
		return store.ScheduledExecOutcomeOK, "", ""
	case agentcontrol.ExecOutcomeNonzero:
		return store.ScheduledExecOutcomeNonzero, store.ScheduledEventFailed, execReasonExitStatus(res.ExitCode)
	case agentcontrol.ExecOutcomeTimeout:
		return store.ScheduledExecOutcomeTimeout, store.ScheduledEventFailed, execReasonTimedOut(timeoutSeconds)
	default: // start_failed
		return store.ScheduledExecOutcomeError, store.ScheduledEventFailed, execReasonStartFailed
	}
}

// finishScheduledExecRun records run's final outcome (first writer wins)
// and, when eventStatus is set, the event's status in the same write; the
// winner emits the outcome audit record. res carries the numbers of a
// finished run, nil otherwise.
func (s *Server) finishScheduledExecRun(ctx context.Context, run store.ScheduledExecRun, outcome string, res *agentcontrol.ExecResult, eventStatus, eventError string) bool {
	fin := store.ScheduledExecRunFinish{
		EventID:     run.EventID,
		FinishedAt:  time.Now(),
		Outcome:     outcome,
		EventStatus: eventStatus,
		EventError:  eventError,
	}
	if res != nil && res.Finished() && res.Outcome != agentcontrol.ExecOutcomeStartFailed {
		code, dur, out := res.ExitCode, res.DurationMS, res.OutputBytes
		fin.ExitCode, fin.DurationMS, fin.OutputBytes = &code, &dur, &out
	}
	won, err := s.store.FinishScheduledExecRun(ctx, fin)
	if err != nil {
		slog.Warn("Scheduler: exec run outcome not recorded", "eventID", run.EventID, "outcome", outcome, "error", err)
		return false
	}
	if !won {
		return false
	}
	a := newScheduledExecRunAudit(run)
	a.Outcome = outcome
	a.ExitCode, a.DurationMS, a.OutputBytes = fin.ExitCode, fin.DurationMS, fin.OutputBytes
	a.Reason = eventError
	s.auditScheduledExec(ctx, a)
	slog.Info("Scheduler: exec run finished", "eventID", run.EventID, "agent_id", run.TargetAgentID,
		"outcome", outcome, "event_status", eventStatus)
	return true
}

// trackScheduledExecRun starts the detached tracker of an admitted run. The
// caller handed it the run's slot and ownership; once the hub is shutting
// down no tracker starts and the sweeper records the outcome after the
// restart.
func (s *Server) trackScheduledExecRun(run store.ScheduledExecRun) {
	if !s.execRuns.work.goTracked(func(ctx context.Context) {
		defer s.execRuns.release(run.EventID)
		s.pollScheduledExecRun(ctx, run)
	}) {
		s.execRuns.release(run.EventID)
	}
}

// pollScheduledExecRun polls exec-status for run until it records an
// outcome or ctx ends (shutdown: nothing is recorded).
func (s *Server) pollScheduledExecRun(ctx context.Context, run store.ScheduledExecRun) {
	interval := scheduledExecPollInitial
	var unavailable bool
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if s.pollScheduledExecRunOnce(ctx, &run, &unavailable) {
			return
		}
		interval = min(interval*2, scheduledExecPollMax)
	}
}

// pollScheduledExecRunOnce polls run once and records its outcome when it is
// known. It reports whether the run needs no further polling. unavailable
// carries, across a tracker's polls, whether the agent's session was last
// unavailable (the lease-expiry text).
func (s *Server) pollScheduledExecRunOnce(ctx context.Context, run *store.ScheduledExecRun, unavailable *bool) bool {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
	defer cancel()
	failed := func(outcome, reason string) bool {
		s.finishScheduledExecRun(recordCtx, *run, outcome, nil, store.ScheduledEventFailed, reason)
		return true
	}
	if time.Now().After(run.LeaseUntil) {
		reason := execReasonOutcomeUnknown
		if *unavailable {
			reason = execReasonAgentStopped
		}
		return failed(store.ScheduledExecOutcomeAbandoned, reason)
	}

	res, err := s.AgentExecRunStatus(ctx, run.TargetAgentID, run.LaunchID, run.EventID)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return true // shutdown: leave the run to the sweeper
		case errors.Is(err, ErrAgentExecLaunchChanged):
			// D9-S3: never query a replacement incarnation.
			return failed(store.ScheduledExecOutcomeAbandoned, execReasonAgentRestarted)
		case errors.Is(err, store.ErrNotFound):
			return failed(store.ScheduledExecOutcomeAbandoned, execReasonAgentStopped)
		case errors.Is(err, router.ErrNoSession), errors.Is(err, ErrAgentControlNotAdvertised):
			// Stopped or suspended mid-run: keep polling until the lease.
			*unavailable = true
			return false
		case errors.Is(err, agentcontrol.ErrExecSchema):
			slog.Warn("Scheduler: exec-status answer outside the schema", "eventID", run.EventID, "error", err)
			return failed(store.ScheduledExecOutcomeError, execReasonOutcomeUnknown)
		default:
			*unavailable = false
			return false
		}
	}
	*unavailable = false
	switch res.State {
	case agentcontrol.ExecStateFinished:
		outcome, status, reason := scheduledExecFinalOutcome(res, run.TimeoutSeconds)
		s.finishScheduledExecRun(recordCtx, *run, outcome, &res, status, reason)
		return true
	case agentcontrol.ExecStateRunning:
		if run.Outcome == store.ScheduledExecOutcomeStarting {
			if _, err := s.store.MarkScheduledExecRunRunning(recordCtx, run.EventID); err == nil {
				run.Outcome = store.ScheduledExecOutcomeRunning
			}
		}
		return false
	default: // unknown (404)
		if run.Outcome == store.ScheduledExecOutcomeRunning {
			// The agent knew the run and has forgotten it: sciontool (PID
			// 1), so the container, restarted.
			return failed(store.ScheduledExecOutcomeAbandoned, execReasonAgentRestarted)
		}
		if time.Since(run.StartedAt) > scheduledExecStartUnknownAfter {
			// The admission window has closed: the start never arrived.
			return failed(store.ScheduledExecOutcomeError, execReasonDispatchFailed)
		}
		return false
	}
}

// scheduledExecRunSweepHandler returns the run sweeper's tick handler.
func (s *Server) scheduledExecRunSweepHandler() func(ctx context.Context) {
	return s.sweepScheduledExecRuns
}

// sweepScheduledExecRuns polls, once each, the unfinished runs no tracker
// of this replica owns, and records runs past their lease as abandoned
// (design "Recovery bounds"): bounded pages, at most
// scheduledExecSweepInFlight concurrent calls, round-robin across target
// agents so unavailable agents cannot monopolize a sweep. It recovers the
// real outcome of runs whose tracker a hub restart lost while their
// tombstone and lease are live.
func (s *Server) sweepScheduledExecRuns(ctx context.Context) {
	after := ""
	for page := 0; page < scheduledExecSweepMaxPages && ctx.Err() == nil; page++ {
		runs, err := s.store.ListUnfinishedScheduledExecRuns(ctx, after, scheduledExecSweepBatch)
		if err != nil {
			slog.Warn("Scheduler: exec run sweep: list failed", "error", err)
			return
		}
		if len(runs) == 0 {
			return
		}
		after = runs[len(runs)-1].EventID
		candidates := make([]store.ScheduledExecRun, 0, len(runs))
		for _, r := range runs {
			if !s.execRuns.owns(r.EventID) {
				candidates = append(candidates, r)
			}
		}
		sem := make(chan struct{}, scheduledExecSweepInFlight)
		var wg sync.WaitGroup
		for _, r := range roundRobinByAgent(candidates) {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(r store.ScheduledExecRun) {
				defer wg.Done()
				defer func() { <-sem }()
				var unavailable bool
				s.pollScheduledExecRunOnce(ctx, &r, &unavailable)
			}(r)
		}
		wg.Wait()
		if len(runs) < scheduledExecSweepBatch {
			return
		}
	}
}

// roundRobinByAgent orders runs one per target agent at a time, keeping each
// agent's runs in their original order.
func roundRobinByAgent(runs []store.ScheduledExecRun) []store.ScheduledExecRun {
	var agents []string
	byAgent := make(map[string][]store.ScheduledExecRun)
	for _, r := range runs {
		if _, ok := byAgent[r.TargetAgentID]; !ok {
			agents = append(agents, r.TargetAgentID)
		}
		byAgent[r.TargetAgentID] = append(byAgent[r.TargetAgentID], r)
	}
	out := make([]store.ScheduledExecRun, 0, len(runs))
	for len(out) < len(runs) {
		for _, a := range agents {
			if q := byAgent[a]; len(q) > 0 {
				out = append(out, q[0])
				byAgent[a] = q[1:]
			}
		}
	}
	return out
}
