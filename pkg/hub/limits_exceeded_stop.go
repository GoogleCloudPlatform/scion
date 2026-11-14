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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// limitsExceededStopDue reports whether a status write the hub accepted
// moves the current run of before (the row as read ahead of the write) to
// activity limits_exceeded, so the hub has to stop that run on its runtime
// (ptone/scion#7928): the container's own exit does not end the run on a
// runtime that restarts it. upd is the update as written, after the
// guards. Not due: a row already at limits_exceeded, a report naming no run
// or another run, an agent with no broker, one past the running lifecycle
// (stopping, stopped, suspended, error, halted), and one being deleted or
// reincarnated.
func limitsExceededStopDue(before *store.Agent, upd store.AgentStatusUpdate) bool {
	if before == nil || upd.Activity != string(state.ActivityLimitsExceeded) || before.Activity == upd.Activity {
		return false
	}
	if before.RunID == "" || upd.ReportRunID != before.RunID || before.RuntimeBrokerID == "" {
		return false
	}
	return state.Phase(before.Phase).IsActivePhase() && before.DeletedAt.IsZero() &&
		!deletionActive(before) && !reincarnationInFlight(before)
}

// stopRunOnLimitsExceeded stops before's run in the background when
// limitsExceededStopDue says so, once per run on this hub instance. A stop
// that did not happen forgets the run, so a later limits_exceeded report
// of it can try again.
func (s *Server) stopRunOnLimitsExceeded(ctx context.Context, before *store.Agent, upd store.AgentStatusUpdate) {
	if !limitsExceededStopDue(before, upd) {
		return
	}
	agentID, runID := before.ID, before.RunID
	if prev, loaded := s.limitsStops.Swap(agentID, runID); loaded && prev == runID {
		return // this run's stop was already dispatched
	}
	s.agentLifecycleLog.Info("Agent reported limits_exceeded; stopping its run", "agent_id", agentID, "run_id", runID)
	s.limitsStopsInFlight.Add(1)
	go func() {
		defer s.limitsStopsInFlight.Done()
		defer func() {
			if rec := recover(); rec != nil {
				s.agentLifecycleLog.Error("limits_exceeded stop panicked", "agent_id", agentID, "panic", fmt.Sprint(rec))
				s.limitsStops.CompareAndDelete(agentID, runID)
			}
		}()
		if !s.stopCurrentRun(context.WithoutCancel(ctx), agentID, runID, "limits_exceeded stop") {
			s.limitsStops.CompareAndDelete(agentID, runID)
		}
	}()
}

// stopCurrentRun is a system-initiated stop of run runID of agent agentID:
// it dispatches the stop to the broker and, unless the row already says how
// the run ended, writes phase stopped (container status stopped) while the
// row still holds runID and the phase it was read with. The run intent is
// left as it is, as when a container exits on its own, so a later platform
// disruption of the run can still halt it. Activity, exit code, exit reason
// and message are left as they are, so the row still says why the run
// ended. It reports whether the stop was dispatched, and does nothing when
// the row has moved to another run, is suspended, has no broker, or is
// being deleted or reincarnated. action names the stop in logs.
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
	if agent.RunID != runID || agent.RuntimeBrokerID == "" || agent.Phase == string(state.PhaseSuspended) ||
		!agent.DeletedAt.IsZero() || deletionActive(agent) || reincarnationInFlight(agent) {
		s.agentLifecycleLog.Info("Agent "+action+": skipped; the run changed or the agent is suspended or being deleted or reincarnated",
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
	// recorded the end of the run meanwhile: that record is kept.
	cur, err := s.store.GetAgent(ctx, agentID)
	if err != nil || cur.RunID != runID || !state.Phase(cur.Phase).IsActivePhase() {
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
	// project reservations, as for a user stop.
	s.releaseBrokerQuota(ctx, cur)
	s.releaseProjectQuota(ctx, cur)
	if updated, err := s.store.GetAgent(ctx, agentID); err == nil && updated.DeletedAt.IsZero() {
		s.events.PublishAgentStatus(ctx, updated)
	}
	return true
}
