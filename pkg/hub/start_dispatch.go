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
	// priorPhase is the agent's phase when beginStartDispatch was called.
	priorPhase string
	// reserved reports whether beginStartDispatch created the reservation
	// (as opposed to finding one the agent already held).
	reserved bool
	// marked reports whether beginStartDispatch wrote PhaseStarting.
	marked bool
	// settled is set once rollback or settle has run.
	settled bool
}

// errStartingWrite marks beginStartDispatch's failure to write PhaseStarting,
// as opposed to a quota reservation error. When the stored phase no longer
// matches the caller's agent.Phase it also wraps store.ErrPhaseMismatch (a
// store.ErrVersionConflict, answered 409 over HTTP).
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
//  2. If agent.Phase (the caller's copy) is not counted toward the cap
//     (isBrokerQuotaCountedPhase: stopped, suspended, error), it writes
//     PhaseStarting with UpdateAgentStatus conditional on the stored phase
//     still being agent.Phase (IfPhase), so the quota reconcile
//     (ReconcileStaleBrokerQuotaReservations) sees a counted phase for as
//     long as the dispatch runs and does not release the slot, whatever the
//     reservation's age. The write does not go through
//     reconcileBrokerQuotaOnPhaseChange, which would reserve a second time.
//     An agent already in a counted phase is left as it is. If the write
//     fails, including when the stored phase has moved on from the caller's
//     stale copy (store.ErrPhaseMismatch), a reservation this call created is
//     released, nothing is dispatched, and an error wrapping
//     errStartingWrite is returned.
//  3. It does NOT change agent.Phase in memory. DispatchAgentStart reads its
//     revoke-on-failure decision (isConfirmedNonRunningPhase) from the
//     in-memory phase, so that decision stays the one the pre-dispatch phase
//     gives. Contrast reincarnate_worker.go, which persists and passes
//     "starting" and so gives up revoke-on-failure.
//
// The caller then dispatches and must, exactly once, either call rollback on
// a dispatch failure (or any early return after this call), or call settle
// once the dispatch has succeeded or the caller has recorded its own failure
// state. Further:
//
//   - The caller's final write must move the row off PhaseStarting and should
//     use AgentStatusUpdate.ClearTerminalRemnants (or, for a full-row write,
//     clear the message, stalled marker and exit fields in memory): the
//     store's stopped/error -> running clear no longer fires once the row
//     reads starting. settle only marks the handle. If the final write
//     fails, the row stays starting with the reservation held, and only a
//     heartbeat corrects it.
//   - rollback restores priorPhase with one conditional write (IfPhase
//     starting), so a phase written meanwhile (a heartbeat from another
//     replica, a launch reaper) is kept. Under a live delete claim the
//     restore is dropped by the store's delete guard; the claim moved the
//     row to stopping, and the delete engine's rollback restores stopped for
//     a prior of starting with no start in flight.
//   - rollback releases the reservation by agent ID when this call created
//     it, which also drops it for a concurrent start of the same agent that
//     reused it (pre-existing behaviour of rollbackBrokerQuota).
//
// The caller must hold a lifecycle op (beginLifecycleOp) for agent from
// before this call until its final status write, or until rollback.
// heartbeatPhaseGuarded uses it to keep a heartbeat that reports the old,
// exited container (stopped, suspended or error) from overwriting
// PhaseStarting and releasing the slot mid-dispatch. That guard is a
// best-effort hint on this replica only: the age gate in the reconcile
// (reconcileMinReservationAge) is the backstop.
func (s *Server) beginStartDispatch(ctx context.Context, agent *store.Agent) (*startDispatch, error) {
	reserved, err := s.checkAndReserveBrokerQuota(ctx, agent)
	if err != nil {
		return nil, err
	}
	d := &startDispatch{
		s:          s,
		agent:      agent,
		priorPhase: agent.Phase,
		reserved:   reserved,
	}
	if !isBrokerQuotaCountedPhase(agent.Phase) {
		if err := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseStarting),
			IfPhase: agent.Phase,
		}); err != nil {
			s.rollbackBrokerQuota(ctx, agent, reserved)
			return nil, fmt.Errorf("%w: %w", errStartingWrite, err)
		}
		d.marked = true
	}
	return d, nil
}

// beginStartDispatchHTTP is beginStartDispatch for HTTP handlers. On false
// it has already written the error response: a reservation error with the
// shape reserveQuotaHTTP uses (429 at the cap), otherwise the store error
// (409 when the stored phase moved on).
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
// return: if this dispatch wrote PhaseStarting it restores the prior phase,
// in one write conditional on the row still reading PhaseStarting, and it
// releases a reservation beginStartDispatch created. Runs detached from
// ctx's cancellation. Only the first rollback or settle on d has any effect.
func (d *startDispatch) rollback(ctx context.Context) {
	if d == nil || d.settled {
		return
	}
	d.settled = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startDispatchRollbackTimeout)
	defer cancel()
	if d.marked {
		err := d.s.store.UpdateAgentStatus(ctx, d.agent.ID, store.AgentStatusUpdate{
			Phase:   d.priorPhase,
			IfPhase: string(state.PhaseStarting),
		})
		if err != nil && !errors.Is(err, store.ErrPhaseMismatch) {
			d.s.agentLifecycleLog.Warn("start dispatch: failed to restore prior phase after failed dispatch",
				"agent_id", d.agent.ID, "prior_phase", d.priorPhase, "error", err)
		}
	}
	d.s.rollbackBrokerQuota(ctx, d.agent, d.reserved)
}

// settle marks d as finished without undoing anything: the dispatch
// succeeded, or the caller recorded its own failure state.
func (d *startDispatch) settle() {
	if d != nil {
		d.settled = true
	}
}

// heartbeatPhaseGuarded reports whether a broker heartbeat must not write
// hbPhase over agent's stored phase. It does so when hbPhase is not counted
// toward the broker cap (stopped, suspended, error), the stored phase is an
// active one (for example "starting", written by beginStartDispatch, or
// "running" between the stop and start legs of a restart), and a lifecycle
// op for the agent is in flight on this replica. Such a report usually
// describes the old container a start is replacing; applying it would also
// release the broker slot mid-dispatch through
// reconcileBrokerQuotaOnPhaseChange. The caller drops only the phase (and
// with it that heartbeat's quota reconcile); exit code, exit reason, message
// and container status still apply, so a guarded report can leave e.g.
// "Exited (137)" as the container status until the lifecycle path's final
// write or the next heartbeat replaces it.
//
// lifecycleOps is held by every lifecycle op, not only starts: the HTTP
// lifecycle handler (start, stop, restart, suspend), auto-suspend, the queued
// broker dispatches (execDispatchStart/Stop/Restart), the DM wake's dispatch
// leg and the create-of-an-existing-agent resume branches. The start-type
// sites (beginStartDispatch callers) and HTTP stop/suspend write the final
// phase themselves; the others (a queued dispatch, or the HTTP handler
// returning on a dispatch error) do not, and a guarded phase is then
// applied by the next heartbeat after the op ends. That deferral is
// transient and accepted.
//
// lifecycleOps is a per-replica hint (see lifecycleOpTracker), so this guard
// only covers ops run by the replica that handles the heartbeat; the quota
// reconcile's age gate is the backstop.
func (s *Server) heartbeatPhaseGuarded(agent *store.Agent, hbPhase string) bool {
	if hbPhase == "" || isBrokerQuotaCountedPhase(hbPhase) {
		return false
	}
	if !state.Phase(agent.Phase).IsActivePhase() {
		return false
	}
	return s.lifecycleOps.active(agent.ID)
}
