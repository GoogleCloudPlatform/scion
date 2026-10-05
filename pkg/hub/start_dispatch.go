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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// startDispatchRollbackTimeout bounds startDispatch.rollback's store writes,
// which run detached from the caller's (possibly canceled) context.
const startDispatchRollbackTimeout = 5 * time.Second

// startDispatch is a start-type dispatch's hold on its agent's
// max_agents_per_broker slot, returned by beginStartDispatch.
type startDispatch struct {
	s     *Server
	agent *store.Agent
	// priorPhase and priorMessage are the agent's phase and message when
	// beginStartDispatch was called.
	priorPhase   string
	priorMessage string
	// reserved reports whether beginStartDispatch created the reservation
	// (as opposed to finding one the agent already held).
	reserved bool
	// marked reports whether beginStartDispatch wrote PhaseStarting.
	marked bool
	// settled is set once rollback or settle has run.
	settled bool
}

// errStartingWrite marks beginStartDispatch's failure to write PhaseStarting,
// as opposed to a quota reservation error.
var errStartingWrite = errors.New("record starting phase before dispatch")

// beginStartDispatch prepares a start-type dispatch (start, restart, resume,
// DM wake) of agent so that its max_agents_per_broker reservation is held for
// the whole dispatch leg (ptone/scion#2014). Contract:
//
//  1. It reserves agent's broker slot with the cap check
//     (checkAndReserveBrokerQuota; idempotent, so an agent that already holds
//     a reservation keeps it, however old). On a reservation error it returns
//     that error (store.ErrQuotaExceeded, ErrQuotaLockContention, ...) and
//     changes nothing.
//  2. If the agent's stored phase is not counted toward the cap
//     (isBrokerQuotaCountedPhase: stopped, suspended, error), it writes
//     PhaseStarting with a plain UpdateAgentStatus, so the quota reconcile
//     (ReconcileStaleBrokerQuotaReservations) sees a counted phase for as
//     long as the dispatch runs and does not release the slot, whatever the
//     reservation's age. The write does not go through
//     reconcileBrokerQuotaOnPhaseChange, which would reserve a second time.
//     An agent already in a counted phase is left as it is. If the write
//     fails, a reservation this call created is released and an error
//     wrapping errStartingWrite is returned.
//  3. It does NOT change agent.Phase in memory. DispatchAgentStart reads its
//     revoke-on-failure decision (isConfirmedNonRunningPhase) from the
//     in-memory phase, so that decision stays the one the pre-dispatch phase
//     gives. Contrast reincarnate_worker.go, which persists and passes
//     "starting" and so gives up revoke-on-failure.
//
// The caller then dispatches and must, exactly once, either call rollback on
// a dispatch failure (or any early return after this call), or write its own
// final state and call settle. rollback restores priorPhase (if the row still
// reads PhaseStarting) and releases a reservation this call created.
//
// The caller must hold a lifecycle op (beginLifecycleOp) for agent from
// before this call until its final status write, or until rollback.
// heartbeatPhaseGuarded uses it to keep a heartbeat that reports the old,
// exited container from overwriting PhaseStarting with stopped/error and
// releasing the slot mid-dispatch. That guard is a best-effort, per-replica
// hint: the age gate in the reconcile (reconcileMinReservationAge) is the
// backstop.
func (s *Server) beginStartDispatch(ctx context.Context, agent *store.Agent) (*startDispatch, error) {
	reserved, err := s.checkAndReserveBrokerQuota(ctx, agent)
	if err != nil {
		return nil, err
	}
	d := &startDispatch{
		s:            s,
		agent:        agent,
		priorPhase:   agent.Phase,
		priorMessage: agent.Message,
		reserved:     reserved,
	}
	if !isBrokerQuotaCountedPhase(agent.Phase) {
		if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}); err != nil {
			s.rollbackBrokerQuota(ctx, agent, reserved)
			return nil, fmt.Errorf("%w: %w", errStartingWrite, err)
		}
		d.marked = true
	}
	return d, nil
}

// beginStartDispatchHTTP is beginStartDispatch for HTTP handlers. On false
// it has already written the error response: a reservation error with the
// shape reserveQuotaHTTP uses (429 at the cap), otherwise the store error.
func (s *Server) beginStartDispatchHTTP(ctx context.Context, w http.ResponseWriter, agent *store.Agent) (*startDispatch, bool) {
	d, err := s.beginStartDispatch(ctx, agent)
	if err == nil {
		return d, true
	}
	if errors.Is(err, errStartingWrite) {
		writeErrorFromErr(w, err, "")
	} else {
		writeQuotaReserveError(w, store.LimitMaxAgentsPerBroker, err)
	}
	return nil, false
}

// rollback undoes beginStartDispatch after a failed dispatch or an early
// return: it restores the prior phase, if this dispatch wrote PhaseStarting
// and the row still reads PhaseStarting (a newer phase written meanwhile,
// such as a delete or a launch reaper's, is kept), and releases a
// reservation beginStartDispatch created. The phase check and the restore
// are a read then a write, not one atomic step. Runs detached from ctx's
// cancellation. Only the first rollback or settle on d has any effect.
func (d *startDispatch) rollback(ctx context.Context) {
	if d == nil || d.settled {
		return
	}
	d.settled = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startDispatchRollbackTimeout)
	defer cancel()
	if d.marked {
		cur, err := d.s.store.GetAgent(ctx, d.agent.ID)
		if err == nil && cur.Phase == string(state.PhaseStarting) {
			if err := d.s.store.UpdateAgentStatus(ctx, d.agent.ID, store.AgentStatusUpdate{Phase: d.priorPhase}); err != nil {
				d.s.agentLifecycleLog.Warn("start dispatch: failed to restore prior phase after failed dispatch",
					"agent_id", d.agent.ID, "prior_phase", d.priorPhase, "error", err)
			}
		} else if err != nil {
			d.s.agentLifecycleLog.Warn("start dispatch: failed to re-read agent to restore prior phase",
				"agent_id", d.agent.ID, "error", err)
		}
	}
	d.s.rollbackBrokerQuota(ctx, d.agent, d.reserved)
}

// settle marks d as finished without undoing anything: the caller has
// written the agent's final state itself (the dispatch succeeded, or it
// recorded its own failure state).
func (d *startDispatch) settle() {
	if d != nil {
		d.settled = true
	}
}

// clearMessageIf is the AgentStatusUpdate.ClearMessageIf value for the
// caller's final running write. UpdateAgentStatus clears the stale stop or
// crash message on a write of running only when the stored phase is stopped
// or error; beginStartDispatch's PhaseStarting write hides that, so the final
// write clears the message the agent had before this dispatch, if it is
// still the stored one.
func (d *startDispatch) clearMessageIf() string {
	if d == nil || !d.marked {
		return ""
	}
	switch state.Phase(d.priorPhase) {
	case state.PhaseStopped, state.PhaseError:
		return d.priorMessage
	}
	return ""
}

// heartbeatPhaseGuarded reports whether a broker heartbeat must not write
// hbPhase over agent's stored phase. It does so when hbPhase is not counted
// toward the broker cap (stopped, suspended, error), the stored phase is an
// active one (for example "starting", written by beginStartDispatch, or
// "running" between the stop and start legs of a restart), and a lifecycle
// op for the agent is in flight on this replica. Such a report describes the
// old container the dispatch is replacing; applying it would also release
// the broker slot mid-dispatch through reconcileBrokerQuotaOnPhaseChange.
// The lifecycle path writes the final phase itself, on success and on
// failure. The caller drops only the phase (and with it that heartbeat's
// quota reconcile); exit code, exit reason and message still apply.
//
// lifecycleOps is a per-replica hint (see lifecycleOpTracker), so this guard
// only covers dispatches made by the replica that handles the heartbeat; the
// quota reconcile's age gate is the backstop.
func (s *Server) heartbeatPhaseGuarded(agent *store.Agent, hbPhase string) bool {
	if hbPhase == "" || isBrokerQuotaCountedPhase(hbPhase) {
		return false
	}
	if !state.Phase(agent.Phase).IsActivePhase() {
		return false
	}
	return s.lifecycleOps.active(agent.ID)
}
