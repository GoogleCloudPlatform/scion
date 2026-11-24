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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Fire flow and recovery of exec events (ptone/scion#7883, design "Fire flow";
// moved to execAction in action_exec.go in PR 2a, ptone/scion#8029):
//
//	execAction (action_exec.go, registered on Server.actions)
//	  ValidatePayload invalid -> failed; Preauthorize flag off / > 10 min late -> skipped;
//	  resolveRecordedAuthority + Authorize (authorizeExecFire) denied -> refused "exec refused";
//	  Authorize preflight (not running / reincarnating / managed) -> skipped;
//	  Fire: tracked-run bound full -> skipped "capacity";
//	  INSERT scheduled_exec_runs (starting); exec-start, one attempt
//	  (StartAgentExecRun: launch-pinned, gated on both hub flags);
//	  then a detached tracker started via FireOutcome.AfterRecord polls
//	  exec-status until the run finishes or its lease ends, and records the
//	  outcome with a compare-and-set.
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
	// scheduledExecCallTimeout bounds one exec-start or exec-status RPC.
	scheduledExecCallTimeout = agentcontrol.HubExecCallTimeout
)

// scheduledExecAdmitWindow bounds how long after its claim an exec event
// can still get its run row: the claim sets fired_at, and the run is
// inserted under the fire handler's deadline (scheduledEventHandlerTimeout),
// so a claimed event with no run past this window was cut off by a hub
// stop between the claim and the insert, and nothing was ever sent for it
// (exec-start follows the insert). A minute of margin covers replica clock
// differences. A variable only as a test seam.
var scheduledExecAdmitWindow = scheduledEventHandlerTimeout + scheduledEventRecordTimeout + time.Minute

// scheduledExecSeamMu guards the exec timing and bound variables above,
// which tests change while trackers of earlier fires may still run.
// Production code reads them through execSeamDuration and execSeamInt.
var scheduledExecSeamMu sync.RWMutex

func execSeamDuration(v *time.Duration) time.Duration {
	scheduledExecSeamMu.RLock()
	defer scheduledExecSeamMu.RUnlock()
	return *v
}

func execSeamInt(v *int) int {
	scheduledExecSeamMu.RLock()
	defer scheduledExecSeamMu.RUnlock()
	return *v
}

// scheduledExecNotAfter is how far after admission a run's start-admission
// deadline (not_after) is frozen. A variable only as a test seam.
var scheduledExecNotAfter = agentcontrol.ExecNotAfter

// scheduledExecStartAbsentGrace is how long after a run's frozen not_after
// a 404 from exec-status is definitive for a run never seen running:
// sciontool spawns only while now <= not_after + tolerance (at receipt and
// again before spawn), so no start can begin later; the design's extra 10s
// follows. A variable only as a test seam.
var scheduledExecStartAbsentGrace = agentcontrol.ExecClockTolerance + 10*time.Second

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
	if t.used >= execSeamInt(&scheduledExecMaxTracked) {
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

// ownedIDs returns the event IDs a fire or tracker of this replica owns.
func (t *scheduledExecRunTracker) ownedIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.owned))
	for id := range t.owned {
		out = append(out, id)
	}
	return out
}

// stopScheduledExecRuns is the background teardown's step for exec runs:
// admission is already closed (haltDispatchProducers); fires still at
// exec-start get scheduledExecStopGrace, then everything left, the trackers
// included, is cancelled. No outcome is recorded for an admitted run: its
// row stays unfinished and its event fired, and the sweeper records the
// real outcome after the restart (design D5, D9 review).
func (s *Server) stopScheduledExecRuns(ctx context.Context) {
	if n := s.execRuns.work.stop(ctx, execSeamDuration(&scheduledExecStopGrace)); n > 0 {
		slog.Info("Scheduled exec runs still tracked at shutdown; the sweeper records their outcome after the restart",
			"running", n)
	}
}

// Stages of an exec fire at which scheduledExecFireHook runs.
const (
	// scheduledExecStageAdmitted: the run row is written; exec-start is
	// not sent yet.
	scheduledExecStageAdmitted = "admitted"
	// scheduledExecStageChecked: the dispatch guard passed; exec-start is
	// about to be sent.
	scheduledExecStageChecked = "checked"
	// scheduledExecStageStarted: exec-start has answered (or failed);
	// nothing is recorded yet.
	scheduledExecStageStarted = "started"
)

// scheduledExecFireHook, when set, runs at each stage of an exec fire. It is
// a test seam for interleaving tests only; production never sets it.
var scheduledExecFireHook func(stage, eventID string)

func scheduledExecFireStage(stage, eventID string) {
	if hook := scheduledExecFireHook; hook != nil {
		hook(stage, eventID)
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
//
// It returns whether this call recorded the outcome; false with a nil error
// means the run was already finished (another writer won) or is gone
// (purged), and needs nothing more. A non-nil error means the write failed:
// the run is still unfinished and must be retried.
func (s *Server) finishScheduledExecRun(ctx context.Context, run store.ScheduledExecRun, outcome string, res *agentcontrol.ExecResult, eventStatus, eventError string) (bool, error) {
	return s.finishScheduledExecRunWithOutputStatus(ctx, run, outcome, res, "", eventStatus, eventError)
}

func (s *Server) finishScheduledExecRunWithOutputStatus(ctx context.Context, run store.ScheduledExecRun, outcome string, res *agentcontrol.ExecResult, fallbackOutputStatus, eventStatus, eventError string) (bool, error) {
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
		if run.CaptureMode != "" {
			if err := agentcontrol.ValidateCapturedOutput(run.CaptureMode, res.OutputStatus, res.Output); err != nil {
				slog.Warn("Scheduler: captured exec output failed validation; recording lost",
					"eventID", run.EventID, "capture_mode", run.CaptureMode, "output_status", res.OutputStatus, "error", err)
				fin.OutputStatus = agentcontrol.ExecOutputLost
			} else {
				fin.OutputStatus = res.OutputStatus
				fin.Output = res.Output
			}
		}
	} else if run.CaptureMode != "" && fallbackOutputStatus != "" {
		fin.OutputStatus = fallbackOutputStatus
	}
	won, err := s.store.FinishScheduledExecRun(ctx, fin)
	if err != nil {
		slog.Warn("Scheduler: exec run outcome not recorded", "eventID", run.EventID, "outcome", outcome, "error", err)
		return false, err
	}
	if !won {
		return false, nil
	}
	a := newScheduledExecRunAudit(run)
	a.Outcome = outcome
	a.ExitCode, a.DurationMS, a.OutputBytes = fin.ExitCode, fin.DurationMS, fin.OutputBytes
	a.OutputStatus = fin.OutputStatus
	a.Reason = eventError
	s.auditScheduledExec(ctx, a)
	slog.Info("Scheduler: exec run finished", "eventID", run.EventID, "agent_id", run.TargetAgentID,
		"outcome", outcome, "event_status", eventStatus, "capture_mode", run.CaptureMode, "output_status", fin.OutputStatus)
	return true, nil
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
	interval, longest := execSeamDuration(&scheduledExecPollInitial), execSeamDuration(&scheduledExecPollMax)
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
		interval = min(interval*2, longest)
	}
}

// pollScheduledExecRunOnce polls run once and records its outcome when it is
// known. It reports whether the run needs no further polling: its outcome
// is recorded, by this call or another writer, or it is gone. A failed
// write reports false, so the run stays tracked and is retried. unavailable
// carries, across a tracker's polls, whether the agent's session was last
// unavailable (the lease-expiry text).
//
// Each write gets its own bounded context, created after the status call
// returns (R3): a status answer that took most of the call budget can still
// be recorded.
func (s *Server) pollScheduledExecRunOnce(ctx context.Context, run *store.ScheduledExecRun, unavailable *bool) bool {
	record := func(outcome string, res *agentcontrol.ExecResult, fallbackOutputStatus, eventStatus, reason string) bool {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
		defer cancel()
		_, err := s.finishScheduledExecRunWithOutputStatus(recordCtx, *run, outcome, res, fallbackOutputStatus, eventStatus, reason)
		return err == nil
	}
	failed := func(outcome, reason string) bool {
		fallback := ""
		if run.CaptureMode != "" {
			fallback = agentcontrol.ExecOutputLost
		}
		return record(outcome, nil, fallback, store.ScheduledEventFailed, reason)
	}
	if time.Now().After(run.LeaseUntil) {
		reason := execReasonOutcomeUnknown
		if *unavailable {
			reason = execReasonAgentStopped
		}
		return failed(store.ScheduledExecOutcomeAbandoned, reason)
	}

	res, err := s.AgentExecRunStatus(ctx, run.TargetAgentID, run.LaunchID, run.EventID, run.CaptureMode)
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
		return record(outcome, &res, "", status, reason)
	case agentcontrol.ExecStateRunning:
		if run.Outcome == store.ScheduledExecOutcomeStarting {
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
			if _, err := s.store.MarkScheduledExecRunRunning(recordCtx, run.EventID); err == nil {
				run.Outcome = store.ScheduledExecOutcomeRunning
			}
			cancel()
		}
		return false
	default: // unknown (404)
		if run.Outcome == store.ScheduledExecOutcomeRunning {
			// The agent knew the run and has forgotten it: sciontool (PID
			// 1), so the container, restarted.
			return failed(store.ScheduledExecOutcomeAbandoned, execReasonAgentRestarted)
		}
		if time.Now().After(run.NotAfter.Add(execSeamDuration(&scheduledExecStartAbsentGrace))) {
			// The frozen start deadline plus tolerance has passed: the
			// start never arrived and can no longer be admitted.
			return failed(store.ScheduledExecOutcomeError, execReasonDispatchFailed)
		}
		return false
	}
}

// scheduledExecRunSweepHandler returns the run sweeper's tick handler.
func (s *Server) scheduledExecRunSweepHandler() func(ctx context.Context) {
	return s.sweepScheduledExecRuns
}

// sweepScheduledExecRuns is one bounded sweep of unfinished exec runs
// (design "Recovery bounds", R2):
//
//  1. Runs past their lease are recorded abandoned first, in pages, with no
//     status call, so they never wait behind slow polls.
//  2. Live runs no tracker of this replica owns are claimed in chunks of at
//     most scheduledExecSweepInFlight, least recently polled first
//     (ClaimScheduledExecRunsForPoll records the claim), and polled once each,
//     concurrently and round-robin across target agents. The claim order
//     is persisted, so successive sweeps (after a deadline exit, or on
//     another replica) rotate through every unfinished run instead of
//     restarting at the same prefix.
//
// It recovers the real outcome of runs whose tracker a hub restart lost
// while their tombstone and lease are live.
func (s *Server) sweepScheduledExecRuns(ctx context.Context) {
	batch, inFlight := execSeamInt(&scheduledExecSweepBatch), execSeamInt(&scheduledExecSweepInFlight)
	budget := batch * execSeamInt(&scheduledExecSweepMaxPages)
	s.recoverUnadmittedExecEvents(ctx, batch, budget)
	for expired := 0; expired < budget && ctx.Err() == nil; {
		runs, err := s.store.ListExpiredScheduledExecRuns(ctx, time.Now(), batch)
		if err != nil {
			slog.Warn("Scheduler: exec run sweep: expired list failed", "error", err)
			break
		}
		progressed := false
		for i := range runs {
			var unavailable bool
			if s.pollScheduledExecRunOnce(ctx, &runs[i], &unavailable) {
				progressed = true
			}
		}
		expired += len(runs)
		if len(runs) < batch || !progressed {
			break
		}
	}

	// Live runs: claimed fairly across target agents, polled with at most
	// inFlight calls at once, and at most once per run per sweep
	// (ptone/scion#8096). Slots are acquired before claiming from the store
	// so cancelling a sweep while all slots are busy never advances the
	// persisted turn of runs this sweep never polled.
	if inFlight <= 0 || budget <= 0 {
		return
	}
	sem := make(chan struct{}, inFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	var claimedThisSweep []string
	for polled := 0; polled < budget && ctx.Err() == nil; {
		reserved := reserveSweepSlots(ctx, sem, min(inFlight, budget-polled))
		if reserved == 0 {
			return
		}
		owned := s.execRuns.ownedIDs()
		exclude := make([]string, 0, len(owned)+len(claimedThisSweep))
		exclude = append(exclude, owned...)
		exclude = append(exclude, claimedThisSweep...)

		runs, err := s.store.ClaimScheduledExecRunsForPoll(ctx, time.Now(), reserved, exclude)
		if err != nil {
			for i := 0; i < reserved; i++ {
				<-sem
			}
			slog.Warn("Scheduler: exec run sweep: claim failed", "error", err)
			return
		}
		for i := len(runs); i < reserved; i++ {
			<-sem
		}
		if len(runs) == 0 {
			return
		}
		for _, r := range runs {
			claimedThisSweep = append(claimedThisSweep, r.EventID)
			wg.Add(1)
			go func(r store.ScheduledExecRun) {
				defer wg.Done()
				defer func() { <-sem }()
				var unavailable bool
				s.pollScheduledExecRunOnce(ctx, &r, &unavailable)
			}(r)
		}
		polled += len(runs)
		if len(runs) < reserved {
			return
		}
	}
}

// recoverUnadmittedExecEvents records failed (execReasonNotDispatched) the
// exec events claimed fired or expired more than scheduledExecAdmitWindow
// ago that never got a run row: a hub that stopped between the claim and
// the run insert left them, and neither restart replay (they are no longer
// pending) nor the run sweep (they have no run) would otherwise finish them
// (ptone/scion#7883 P2-R1, AC10). Nothing was sent for such an event, and
// recovery never sends exec-start. The write is conditional on the event
// still having no run, in one transaction; for a recurring event (Phase 2)
// it also records the schedule's failed run.
func (s *Server) recoverUnadmittedExecEvents(ctx context.Context, batch, budget int) {
	for done := 0; done < budget && ctx.Err() == nil; {
		cutoff := time.Now().Add(-execSeamDuration(&scheduledExecAdmitWindow))
		events, err := s.store.ListUnadmittedScheduledEvents(ctx, scheduledEventTypeExec, cutoff, batch)
		if err != nil {
			slog.Warn("Scheduler: exec run sweep: unadmitted list failed", "error", err)
			return
		}
		progressed := false
		for _, evt := range events {
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledEventRecordTimeout)
			changed, err := s.store.FailUnadmittedScheduledEvent(recordCtx, evt.ID, scheduledEventTypeExec, cutoff, execReasonNotDispatched)
			cancel()
			if err != nil {
				slog.Warn("Scheduler: exec run sweep: unadmitted event not recorded", "eventID", evt.ID, "error", err)
				continue
			}
			if !changed {
				continue
			}
			progressed = true
			p, _ := decodeScheduledExecPayload(evt.Payload)
			a := newScheduledExecFireAudit(ContextWithExecutor(ctx, ExecutorContext{Kind: "scheduler", ID: "scheduled_event:" + evt.ID}),
				ScheduledExecAuditOutcome, evt, p)
			a.Outcome = store.ScheduledExecOutcomeError
			a.Reason = execReasonNotDispatched
			s.auditScheduledExec(ctx, a)
			slog.Warn("Scheduler: exec event claimed but never dispatched; recorded failed",
				"eventID", evt.ID, "scheduleID", evt.ScheduleID, "projectID", evt.ProjectID)
		}
		done += len(events)
		if len(events) < batch || !progressed {
			return
		}
	}
}

// reserveSweepSlots blocks for one in-flight slot (or ctx cancellation), then
// greedily acquires up to maxReserve-1 additional immediately-free slots.
func reserveSweepSlots(ctx context.Context, sem chan struct{}, maxReserve int) int {
	if maxReserve <= 0 {
		return 0
	}
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return 0
	}
	reserved := 1
	for reserved < maxReserve {
		select {
		case sem <- struct{}{}:
			reserved++
		default:
			return reserved
		}
	}
	return reserved
}
