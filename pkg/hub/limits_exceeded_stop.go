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
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// limitsExceededStopGrace is how long the hub waits, after it first sees a
// run at limits_exceeded, before it stops the run (ptone/scion#7928).
// sciontool reports limits_exceeded first, then shuts down: SIGTERM and the
// child's grace (10s by default, then SIGKILL), the open-session report (up
// to 5s), the stopping report, sidecar shutdown (the grace again), the
// session-end hooks and the final exit-10 report (5s). Its bounded steps
// add up to under a minute; two minutes leaves the session-end hooks room.
const limitsExceededStopGrace = 2 * time.Minute

// limitsExceededStopWait waits out limitsExceededStopGrace. A variable for
// tests.
var limitsExceededStopWait = func() { time.Sleep(limitsExceededStopGrace) }

// limitsExceededStopDue reports whether a status write the hub accepted
// moves the current run of before (the row as read ahead of the write) to
// activity limits_exceeded, so the hub arms a stop of the run. upd is the
// update as written, after the guards. Not due: a row already at
// limits_exceeded (each run arms once), a report naming no run or another
// run, an agent with no broker, and one systemStopBlocked refuses. The
// agent's own report of phase stopped does not block it: sciontool's final
// report says stopped, and the platform may still restart the container.
func limitsExceededStopDue(before *store.Agent, upd store.AgentStatusUpdate) bool {
	if before == nil || upd.Activity != string(state.ActivityLimitsExceeded) || before.Activity == upd.Activity {
		return false
	}
	if before.RunID == "" || upd.ReportRunID != before.RunID || before.RuntimeBrokerID == "" {
		return false
	}
	return !systemStopBlocked(before)
}

// systemStopBlocked reports whether a system stop of a's run must not run:
// the hub or a user stopped (run intent stopped) or suspended it, the
// platform halted it, or it is being deleted or reincarnated.
func systemStopBlocked(a *store.Agent) bool {
	return a.RunIntent == store.RunIntentStopped || a.Phase == string(state.PhaseSuspended) ||
		a.Phase == string(state.PhaseHalted) || !a.DeletedAt.IsZero() ||
		deletionActive(a) || reincarnationInFlight(a)
}

// stopRunOnLimitsExceeded arms a stop of before's run when
// limitsExceededStopDue says so and the agent's runtime may restart an
// exited container (its broker lists the runtime in RestartingRuntimes).
// The stop is dispatched limitsExceededStopGrace later, so sciontool's own
// shutdown completes first. s.limitsStops holds the runs whose stop is
// armed or in flight, so reports arriving meanwhile do not arm it again;
// the entry is removed when the stop ends.
func (s *Server) stopRunOnLimitsExceeded(ctx context.Context, before *store.Agent, upd store.AgentStatusUpdate) {
	if !limitsExceededStopDue(before, upd) {
		return
	}
	agentID, runID := before.ID, before.RunID
	if prev, loaded := s.limitsStops.Swap(agentID, runID); loaded && prev == runID {
		return // this run's stop is armed or in flight
	}
	snapshot := *before
	s.limitsStopsInFlight.Add(1)
	go func() {
		defer s.limitsStopsInFlight.Done()
		defer s.limitsStops.CompareAndDelete(agentID, runID)
		defer func() {
			if rec := recover(); rec != nil {
				s.agentLifecycleLog.Error("limits_exceeded stop panicked", "agent_id", agentID, "panic", fmt.Sprint(rec))
			}
		}()
		bctx := context.WithoutCancel(ctx)
		broker, err := s.store.GetRuntimeBroker(bctx, snapshot.RuntimeBrokerID)
		if err != nil || broker.Capabilities == nil ||
			!slices.Contains(broker.Capabilities.RestartingRuntimes, effectiveAgentRuntime(&snapshot, broker)) {
			return // the exited container stays exited: nothing to stop
		}
		s.agentLifecycleLog.Info("Agent reported limits_exceeded; stopping its run after the grace",
			"agent_id", agentID, "run_id", runID, "grace", limitsExceededStopGrace)
		limitsExceededStopWait()
		s.stopCurrentRun(bctx, agentID, runID, "limits_exceeded stop")
	}()
}

// stopCurrentRun is a system stop of run runID of agent agentID: it
// dispatches the stop and, unless the row already records the run's end,
// writes phase stopped while the row holds runID and the phase it was read
// with. Run intent, activity, exit code, exit reason and message are kept
// (a later platform disruption can still halt the run). It reports whether
// the stop was dispatched; it does nothing when the run changed, there is
// no broker, or systemStopBlocked refuses. action names the stop in logs.
func (s *Server) stopCurrentRun(ctx context.Context, agentID, runID, action string) bool {
	ctx, cancel := detachStopFromClient(ctx, stopWriteBudget())
	defer cancel()

	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Error("Agent "+action+": could not read agent", "agent_id", agentID, "error", err)
		}
		return false
	}
	if agent.RunID != runID || agent.RuntimeBrokerID == "" || systemStopBlocked(agent) {
		s.agentLifecycleLog.Info("Agent "+action+": skipped; the run changed, or it was stopped, suspended, halted, deleted or reincarnated",
			"agent_id", agentID, "run_id", runID, "current_run_id", agent.RunID, "phase", agent.Phase)
		return false
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return false
	}

	// The container is stopped before phase=stopped is written; see
	// beginLifecycleOp.
	endLifecycleOp := s.beginLifecycleOp(agent.ID)
	defer endLifecycleOp()
	if err := syncDispatch(ctx, func(dctx context.Context) error {
		return dispatcher.DispatchAgentStop(dctx, agent)
	}); err != nil {
		s.logStopRunMismatch(agent, action, err)
		s.agentLifecycleLog.Error("Agent "+action+": dispatch failed", "agent_id", agentID, "error", err)
		return false
	}

	// The agent's own stopped report, a crash or a platform halt may have
	// recorded the end of the run: that record is kept.
	cur, err := s.store.GetAgent(ctx, agentID)
	if err != nil || cur.RunID != runID ||
		(!state.Phase(cur.Phase).IsActivePhase() && cur.Phase != string(state.PhaseStopping)) {
		return true
	}
	err = s.store.UpdateAgentStatus(ctx, agentID, store.AgentStatusUpdate{
		Phase:           string(state.PhaseStopped),
		ContainerStatus: "stopped",
		IfPhase:         cur.Phase,
		IfRunID:         runID,
	})
	endLifecycleOp()
	if err != nil {
		// ErrPhaseMismatch and ErrRunChanged (both ErrVersionConflict): another
		// writer recorded the run's end, or a newer run, first.
		if !errors.Is(err, store.ErrVersionConflict) {
			s.agentLifecycleLog.Error("Agent "+action+": status write failed", "agent_id", agentID, "error", err)
		}
		return true
	}
	// A stopped agent has no running container: release its broker and
	// project reservations, but only for the stopped run and not while a
	// start claim holds the agent, as for an observed phase change.
	cond := store.AgentReleaseCond{ExpectedRunID: runID, NoStartKindClaim: true}
	_, _ = s.releaseBrokerQuotaIf(ctx, cur, cond)
	_, _ = s.releaseProjectQuotaIf(ctx, cur, cond)
	if updated, err := s.store.GetAgent(ctx, agentID); err == nil && updated.DeletedAt.IsZero() {
		s.events.PublishAgentStatus(ctx, updated)
	}
	return true
}
