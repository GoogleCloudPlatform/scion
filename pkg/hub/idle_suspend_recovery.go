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
	"net"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Idle suspend commit and recovery (ptone/scion#8204). The record
// phase=suspended, suspend_reason=idle is written by the worker right after
// the stop (recordIdleSuspendCommit) or, when that did not land, by the idle
// sweep once the claim lease has expired (recoverIdleSuspendClaims).

var (
	// idleSuspendCommitBudget bounds the retries of the worker's record write.
	idleSuspendCommitBudget = 30 * time.Second
	// idleSuspendCommitBackoff is the first retry wait; it doubles up to
	// idleSuspendCommitBackoffMax. Variables so tests can shorten them.
	idleSuspendCommitBackoff    = 100 * time.Millisecond
	idleSuspendCommitBackoffMax = 2 * time.Second
	// idleSuspendRecoveryBatch bounds the rows one sweep tick recovers.
	idleSuspendRecoveryBatch = 64
)

// idleSuspendAbandonLeases is how many claim leases an idle suspend whose stop
// never landed (the row still runs, intent stopped, claim expired) may stay in
// progress before the sweep abandons it: the claim is cleared and the intent
// reverted to running, the state a failed stop leaves on main. Until then
// deliveries fail fast instead of waiting.
const idleSuspendAbandonLeases = 3

// idleSuspendAbandonAfter is how long a claim must have been expired for the
// sweep to abandon the idle suspend of a row that still runs.
func idleSuspendAbandonAfter() time.Duration {
	return idleSuspendAbandonLeases * idleSuspendClaimLease()
}

// idleSuspendBackoff is how long after its claim stamp an abandoned row is not
// claimed again by the idle sweep. The abandon runs idleSuspendAbandonAfter
// after the stamp and keeps the stamp (run_intent back to running) as the
// marker, so the next attempt comes idleSuspendAbandonAfter + one lease after
// the abandon at the earliest (about 15 minutes at the default lease): a
// persistent stop failure retries about every 30 minutes instead of every tick. Any user or hub lifecycle write clears the
// stamp and ends the backoff.
func idleSuspendBackoff() time.Duration {
	return 2*idleSuspendAbandonAfter() + idleSuspendClaimLease()
}

const idleSuspendAbandonedMessage = "Idle suspend failed: the container did not stop; the suspend was abandoned and the agent keeps running"

// stopDefinitelyRefused reports whether a failed idle-suspend stop dispatch
// provably did nothing to the container, so the worker may revert the run
// intent and clear the claim. Every other error leaves the outcome unknown
// (the claim is kept and the intent stays stopped): an owner node's timeout
// flattened across nodes, a non-zero exit of our own stop, a connection reset
// after the request was written, a 5xx. Unknown outcomes converge: a container
// that still runs keeps intent=stopped, the intent-stop backstop stops it, and
// the sweep then records suspended(idle).
//
// The allowlist, each entry with its reason:
//   - errStartRequestNotSent: the request could not be built, signed or routed;
//     nothing was sent.
//   - errStartBrokerNotConnected: no connection to the broker; nothing was sent.
//   - a dial failure: the connection was never established. Checked first: a
//     dial timeout also matches context.DeadlineExceeded.
//   - ErrStopRunNotFound: the broker answered that it holds no entry for the
//     requested run; nothing was stopped, and the run is already gone.
//   - a broker HTTP 400, 401 or 403: the request was rejected before the broker
//     acted on it. Other statuses (404 for a container that may be gone, 409,
//     408, 5xx) can follow a partial or completed stop and stay unknown.
func stopDefinitelyRefused(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	if errors.Is(err, errStartRequestNotSent) || errors.Is(err, errStartBrokerNotConnected) ||
		errors.Is(err, ErrStopRunNotFound) {
		return true
	}
	var se *brokerStatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
	}
	return false
}

// recordIdleSuspendCommit writes the idle suspend's status update under the
// claim stamp, retrying errors that are not guard rejections with backoff
// within idleSuspendCommitBudget, on a context detached from the stop's
// (which may be spent). It reports whether the suspension was recorded. A
// guard rejection after an earlier attempt failed ambiguously counts as
// recorded when the row already reads suspended for the same run under the same
// claim stamp, whatever the reason (a membership hold records it as NULL). On a final error the
// claim is left in place for sweep recovery.
func (s *Server) recordIdleSuspendCommit(ctx context.Context, agentID, runID string, claimUntil time.Time, upd store.AgentStatusUpdate) (bool, error) {
	rctx, cancel := detachedContext(ctx, idleSuspendCommitBudget)
	defer cancel()
	wait := idleSuspendCommitBackoff
	for attempt := 0; ; attempt++ {
		actx, acancel := context.WithTimeout(rctx, finalizationTimeout)
		recorded, err := s.recordStopStatus(actx, agentID, runID, "auto-suspend", upd)
		acancel()
		if err == nil {
			if !recorded && attempt > 0 {
				if fresh, gerr := s.store.GetAgent(rctx, agentID); gerr == nil && fresh != nil &&
					fresh.Phase == string(state.PhaseSuspended) && fresh.RunID == runID &&
					fresh.IdleSuspendClaimUntil != nil && fresh.IdleSuspendClaimUntil.Equal(claimUntil) {
					return true, nil
				}
			}
			return recorded, nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return false, err // the row is gone: nothing to record
		}
		slog.Warn("Scheduler: idle auto-suspend record write failed; retrying",
			"agent_id", agentID, "attempt", attempt+1, "error", err)
		select {
		case <-rctx.Done():
			return false, err
		case <-time.After(wait):
		}
		if wait *= 2; wait > idleSuspendCommitBackoffMax {
			wait = idleSuspendCommitBackoffMax
		}
	}
}

// recoverIdleSuspendClaims records suspended(idle) for idle suspends whose
// stop landed but whose record did not (record write failure, hub restart,
// unknown stop outcome): rows with an expired claim, run_intent=stopped and
// phase stopping, stopped or error. It runs whenever the experiment is on,
// whether or not auto_suspend_idle is: a stamp exists only because the sweep
// ran, and finishing a stop that already landed is not a new idle suspend.
// Gating it on the setting would leave such a row "in progress" forever once
// the operator turned the setting off. The store guard (IfIdleSuspendRecover)
// re-checks everything on the locked row, so a row a user or hub lifecycle
// write has since touched, a new run, a start claim or a deletion is skipped.
func (s *Server) recoverIdleSuspendClaims(ctx context.Context) {
	if !s.experimentEnabled(experiments.IdleSuspend) {
		return
	}
	rows, err := s.store.ListExpiredIdleSuspendClaims(ctx, idleSuspendRecoveryBatch)
	if err != nil {
		slog.Error("Scheduler: idle auto-suspend recovery listing failed", "error", err)
		return
	}
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		s.recoverIdleSuspendClaim(ctx, &rows[i])
	}
	s.abandonStuckIdleSuspends(ctx)
}

// abandonStuckIdleSuspends gives up idle suspends whose stop never landed: rows
// still running, intent stopped, with a claim expired for longer than
// idleSuspendAbandonAfter. The store write clears the claim and reverts the
// intent to running under the exact stamp, run and no-start-claim guards, so
// it wins or loses atomically against the intent-stop backstop and a user
// action. The agent stays deliverable, as after a failed stop on main.
func (s *Server) abandonStuckIdleSuspends(ctx context.Context) {
	after := idleSuspendAbandonAfter()
	rows, err := s.store.ListAbandonedIdleSuspendClaims(ctx, after, idleSuspendRecoveryBatch)
	if err != nil {
		slog.Error("Scheduler: idle auto-suspend abandon listing failed", "error", err)
		return
	}
	for i := range rows {
		if ctx.Err() != nil {
			return
		}
		agent := &rows[i]
		if agent.IdleSuspendClaimUntil == nil || s.isAutoSuspendQueued(agent.ID) {
			continue
		}
		endLifecycleOp, ok := s.tryBeginLifecycleOp(agent.ID)
		if !ok {
			continue
		}
		rctx, cancel := detachedContext(ctx, finalizationTimeout)
		if agent.RunIntentAt == nil {
			endLifecycleOp()
			cancel()
			continue
		}
		err := s.store.AbandonIdleSuspend(rctx, agent.ID, *agent.IdleSuspendClaimUntil, agent.RunID, *agent.RunIntentAt, after, idleSuspendAbandonedMessage)
		cancel()
		endLifecycleOp()
		switch {
		case err == nil:
			slog.Warn("Scheduler: idle auto-suspend abandoned: the stop never landed; the agent keeps running",
				"agent_id", agent.ID, "agent_name", agent.Name, "claim_expired_at", *agent.IdleSuspendClaimUntil)
			if fresh, gerr := s.store.GetAgent(ctx, agent.ID); gerr == nil && fresh != nil {
				s.events.PublishAgentStatus(ctx, fresh)
			}
		case errors.Is(err, store.ErrIdleSuspendPrecondition):
			// The row moved on (a stop landed, a user acted): nothing to do.
		default:
			slog.Error("Scheduler: idle auto-suspend abandon failed; will retry on the next sweep",
				"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
		}
	}
}

func (s *Server) recoverIdleSuspendClaim(ctx context.Context, agent *store.Agent) {
	if s.isAutoSuspendQueued(agent.ID) || agent.IdleSuspendClaimUntil == nil {
		return
	}
	endLifecycleOp, ok := s.tryBeginLifecycleOp(agent.ID)
	if !ok {
		return
	}
	defer endLifecycleOp()
	rctx, cancel := detachedContext(ctx, stopWriteBudget())
	defer cancel()

	stamp := *agent.IdleSuspendClaimUntil
	upd := store.AgentStatusUpdate{
		Phase:                string(state.PhaseSuspended),
		ContainerStatus:      "stopped",
		Activity:             "",
		RetainedActivity:     string(state.RetainedSuspendActivity(state.Activity(agent.Activity), state.Activity(agent.StalledFromActivity))),
		SuspendReason:        string(state.SuspendReasonIdle),
		IfRunID:              agent.RunID,
		IfIdleSuspendRecover: &stamp,
	}
	recorded, err := s.recordStopStatus(rctx, agent.ID, agent.RunID, "auto-suspend recovery", upd)
	if err != nil {
		slog.Error("Scheduler: idle auto-suspend recovery write failed; will retry on the next sweep",
			"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
		return
	}
	if !recorded {
		return
	}
	slog.Info("Scheduler: recovered idle auto-suspend", "agent_id", agent.ID, "agent_name", agent.Name)
	s.completeAutoSuspendRecord(rctx, agent, agent.RunID, state.SuspendReasonIdle)
	endLifecycleOp()
	s.events.PublishAgentStatus(rctx, agent)
}

// recoverExpiredIdleClaimOnDemand is called by a delivery wait that finds an
// idle suspend in progress whose lease has expired (fresh read from
// MarkAgentDelivery). When the row already reports stopping, stopped or error
// it tries the recovery write for that one row, instead of leaving the wait to
// run into the next sweep tick. The store guard re-checks everything, so a
// row that is not recoverable is left untouched.
func (s *Server) recoverExpiredIdleClaimOnDemand(ctx context.Context, agent *store.Agent) {
	if agent == nil || agent.IdleSuspendClaimLeaseActive || !agent.IdleSuspendClaimActive {
		return
	}
	switch state.Phase(agent.Phase) {
	case state.PhaseStopping, state.PhaseStopped, state.PhaseError:
	default:
		return
	}
	if !s.experimentEnabled(experiments.IdleSuspend) {
		return
	}
	s.recoverIdleSuspendClaim(ctx, agent)
}
