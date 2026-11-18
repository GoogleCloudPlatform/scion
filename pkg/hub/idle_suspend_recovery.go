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

// stopOutcomeUnknown reports whether a failed stop dispatch may nonetheless
// have stopped the container: a timeout, a cancellation or a transport error.
// A broker's HTTP refusal, a request that was never sent and a run mismatch
// are definite and return false, as do errors raised before anything was sent.
func stopOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var se *brokerStatusError
	if errors.As(err, &se) || errors.Is(err, errStartRequestNotSent) || errors.Is(err, ErrStopRunNotFound) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// recordIdleSuspendCommit writes the idle suspend's status update under the
// claim stamp, retrying errors that are not guard rejections with backoff
// within idleSuspendCommitBudget, on a context detached from the stop's
// (which may be spent). It reports whether the suspension was recorded. A
// guard rejection after an earlier attempt failed ambiguously counts as
// recorded when the row already reads suspended(idle). On a final error the
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
					fresh.Phase == string(state.PhaseSuspended) && fresh.SuspendReason == upd.SuspendReason &&
					fresh.IdleSuspendClaimUntil != nil && fresh.IdleSuspendClaimUntil.Equal(claimUntil) {
					return true, nil
				}
			}
			return recorded, nil
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
// phase stopping, stopped or error. It runs under the idle sweep's gates and
// does nothing when either is off. The store guard (IfIdleSuspendRecover)
// re-checks everything on the locked row, so a row a user or hub lifecycle
// write has since touched, a new run, a start claim or a deletion is skipped.
func (s *Server) recoverIdleSuspendClaims(ctx context.Context) {
	if !s.experimentEnabled(experiments.IdleSuspend) {
		return
	}
	if enabled, _ := s.idleSuspendSettings(); !enabled {
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
	if agent.RuntimeBrokerID != "" && agent.RunIntentAt != nil {
		s.releaseSupersededClaim(rctx, agent.ID, agent.StartClaimID, *agent.RunIntentAt)
	}
	endLifecycleOp()
	s.events.PublishAgentStatus(rctx, agent)
}
