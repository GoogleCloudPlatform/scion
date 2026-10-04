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
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Backend-driven agent delete: the claim, the detached engine and the
// request-side wait (design ptone/scion#2483 §2.1, §2.3, §2.4).
//
// A DELETE claims a leased, sticky marker on the agent row
// (UpdateAgentDeletion), publishes status{deletion:deleting}, and starts
// runAgentDeletion on a context detached from the request. The handler then
// waits up to deleteSyncWait on the engine's in-process completion channel:
// a fast delete answers 204/502/409 as before (503 when the broker does not
// have the agent's runtime available), a slow one answers 202 and
// its outcome arrives as events. A concurrent DELETE joins the live one.

// Timing knobs. Variables, not constants, so tests can shrink them.
var (
	// deleteSyncWait is how long a DELETE waits for the outcome before
	// answering 202. "Prefer: wait=N" may only lower it.
	deleteSyncWait = 20 * time.Second
	// deleteDispatchBudget bounds the broker dispatch, including the
	// deferred (cross-node) wait, which gets the remainder as its budget.
	deleteDispatchBudget = 120 * time.Second
	// deleteLease is the lease the engine holds; it only has to outlive a
	// dead engine because the live one renews it.
	deleteLease = 60 * time.Second
	// deleteLeaseRenewInterval is how often the live engine renews.
	deleteLeaseRenewInterval = 20 * time.Second
	// deleteJoinPollInterval is the joiner's row-poll tick: events only wake
	// a joiner, the row decides.
	deleteJoinPollInterval = 1 * time.Second

	deleteRenewTimeout   = 5 * time.Second  // per renewal write
	deleteShortStep      = 5 * time.Second  // ports, finalizing CAS, classification
	deleteStepTimeout    = 10 * time.Second // revoke attempt, events, notifications, finish, topic
	deleteRevokeAttempts = 3
)

var (
	// errDeletionLost is the engine ctx cause when the engine no longer holds
	// its claim (a renewal or a CAS affected 0 rows).
	errDeletionLost = errors.New("delete engine lost its claim")
	// errDeletionLeaseLapsing is the engine ctx cause when renewals kept
	// failing and the lease would lapse before the next tick, so a re-claim
	// may already be legal.
	errDeletionLeaseLapsing = errors.New("delete engine could not renew its lease")
)

// deletionOutcomeKind is the terminal result an engine reports.
type deletionOutcomeKind int

const (
	deletionOutcomeDeleted deletionOutcomeKind = iota + 1
	deletionOutcomeFailed
	deletionOutcomeLost
)

// deletionOutcome is sent exactly once on the engine's completion channel.
type deletionOutcome struct {
	kind       deletionOutcomeKind
	code       string // failed: the deletion code
	message    string // failed: the stored error message
	retryAfter string // failed with runtime_unavailable: the Retry-After to send
}

// deletionRetryAfter remembers, per agent, the Retry-After of the latest
// runtime_unavailable delete failure classified by this hub process, so a
// request that joins that delete answers with the same value. A joiner on
// another hub process (or after a restart) has no entry and falls back to
// defaultBrokerRuntimeRetryAfter. One entry per agent, replaced on the next
// such failure and removed when a delete of the agent completes.
var deletionRetryAfter sync.Map // agent ID -> deletionRetryAfterEntry

type deletionRetryAfterEntry struct {
	claim int64
	value string
}

// deletionRetryAfterFor returns the remembered Retry-After for the agent's
// failed delete at claim, or "" if this process has none.
func deletionRetryAfterFor(agentID string, claim int64) string {
	if v, ok := deletionRetryAfter.Load(agentID); ok {
		if e := v.(deletionRetryAfterEntry); e.claim == claim {
			return e.value
		}
	}
	return ""
}

// agentDeletionPlan is what the engine needs from the claim.
type agentDeletionPlan struct {
	claim    int64
	snapshot *store.Agent // the row as read right after the claim
	req      store.DeletionRequestInfo
	prior    store.DeletionPriorState
	// skipDispatch: a re-claim of a lease-expired finalizing row. Teardown
	// already ran, so the engine goes straight to finalize.
	skipDispatch bool
}

// agentDeleteParams are the caller's request parameters.
type agentDeleteParams struct {
	deleteFiles  bool
	removeBranch bool
	force        bool
	requestedBy  string
}

// deleteClaimStopping reports whether a claim moves a's phase to stopping
// (design §2.1): an active phase, or an in-flight T1 launch (C1) — BeginLaunch
// leaves a created row created, and neither ApplyLaunchReport nor the reaper
// passes through Guard 0c, so the stopping write is what makes the launch's
// next report answer 409 stopped.
func deleteClaimStopping(a *store.Agent) bool {
	switch state.Phase(a.Phase) {
	case state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting, state.PhaseRunning:
		return true
	case state.PhaseCreated:
		return a.LaunchState == store.LaunchStateActive
	}
	return false
}

// claimAgentDeletion runs the claim (design §2.1, without phase 2b's
// live-force branch):
//
//	deleted_at IS NULL AND (
//	   state IN ('', 'failed')                               -- fresh attempt
//	OR (state IN ('deleting','finalizing') AND lease_at < now)) -- abandoned
//
// It returns the plan when this request now holds the claim, or nil when the
// claim affected no row (the caller re-reads to decide).
func (s *Server) claimAgentDeletion(ctx context.Context, agentID string, p agentDeleteParams) (*agentDeletionPlan, error) {
	now := time.Now()
	lease := now.Add(deleteLease)
	deleting := store.DeletionStateDeleting
	empty := ""

	var (
		claim    int64
		oldState string
		req      store.DeletionRequestInfo
	)
	set := store.DeletionFields{
		State:         &deleting,
		BumpClaim:     true,
		LeaseAt:       &lease,
		StartedAt:     &now,
		Code:          &empty,
		Error:         &empty,
		ClearFailedAt: true,
		Derive: func(cur *store.Agent, f *store.DeletionFields) {
			claim = cur.DeletionClaim + 1
			oldState = cur.DeletionState
			// Capture prior once per delete attempt: a re-claim of an
			// abandoned row keeps the original prior.
			if cur.DeletionState == store.DeletionStateNone || cur.DeletionState == store.DeletionStateFailed {
				prior := store.DeletionPriorState{Phase: cur.Phase, Activity: cur.Activity}
				if cur.LaunchState == store.LaunchStateActive {
					prior.LaunchID = cur.LaunchID
				}
				if b, err := json.Marshal(prior); err == nil {
					ps := string(b)
					f.Prior = &ps
				}
			}
			post := *cur
			post.DeletionState = store.DeletionStateDeleting
			if deleteClaimStopping(cur) {
				stopping := string(state.PhaseStopping)
				f.Phase = &stopping
				post.Phase = stopping
			}
			// Soft unless force, no retention, or an incomplete async create
			// (T1 §0c, evaluated on the post-claim row): those are always
			// hard-deleted so the name can be reused.
			req = store.DeletionRequestInfo{
				DeleteFiles:  p.deleteFiles,
				RemoveBranch: p.removeBranch,
				Force:        p.force,
				RequestedBy:  p.requestedBy,
				Soft:         s.config.SoftDeleteRetention > 0 && !p.force && !post.IsIncompleteCreate(),
			}
			if req.Soft && s.config.SoftDeleteRetainFiles {
				req.DeleteFiles = false
			}
			if b, err := json.Marshal(req); err == nil {
				rs := string(b)
				f.Request = &rs
			}
		},
	}

	preds := []store.DeletionPredicate{
		{States: []string{store.DeletionStateNone, store.DeletionStateFailed}, DeletedAtNull: true},
		{States: []string{store.DeletionStateDeleting, store.DeletionStateFinalizing}, DeletedAtNull: true, LeaseExpiredBefore: &now},
	}
	for _, pred := range preds {
		n, err := s.store.UpdateAgentDeletion(ctx, agentID, pred, set)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		row, err := s.store.GetAgent(ctx, agentID)
		if err != nil {
			return nil, err
		}
		if row.DeletionClaim != claim {
			// Re-claimed by someone else already (only possible after our
			// lease expired); treat as not claimed.
			return nil, nil
		}
		return &agentDeletionPlan{
			claim:        claim,
			snapshot:     row,
			req:          req,
			prior:        row.ParseDeletionPrior(),
			skipDispatch: oldState == store.DeletionStateFinalizing,
		}, nil
	}
	return nil, nil
}

// runAgentDeletion starts the engine for plan on a context detached from
// the request (design §2.3) and returns its completion channel. Exactly one
// outcome is sent, from a defer, on every exit including a panic.
func (s *Server) runAgentDeletion(reqCtx context.Context, plan *agentDeletionPlan) <-chan deletionOutcome {
	done := make(chan deletionOutcome, 1)
	base := context.WithoutCancel(reqCtx)
	engineCtx, cancel := context.WithCancelCause(base)
	e := &deletionEngine{s: s, plan: plan, base: base, ctx: engineCtx, cancel: cancel}
	go func() {
		var out deletionOutcome
		defer func() {
			if rec := recover(); rec != nil {
				s.agentLifecycleLog.Error("delete engine panicked",
					"agent_id", plan.snapshot.ID, "claim", plan.claim, "finished", e.finished, "panic", fmt.Sprint(rec))
				if e.finished {
					// The delete committed: report it, never "retry".
					out = deletionOutcome{kind: deletionOutcomeDeleted}
				} else {
					out = e.abandonOutcome()
				}
			}
			if out.kind == deletionOutcomeDeleted {
				// The agent is gone: drop any remembered Retry-After.
				deletionRetryAfter.Delete(plan.snapshot.ID)
			}
			e.stopRenewal()
			cancel(nil)
			done <- out
		}()
		out = e.run()
	}()
	return done
}

// deletionEngine is one run of runAgentDeletion.
type deletionEngine struct {
	s    *Server
	plan *agentDeletionPlan
	base context.Context // detached from the request, never cancelled
	ctx  context.Context // base, cancelled with a cause on lost claim
	// cancel cancels ctx with a cause.
	cancel context.CancelCauseFunc

	renewOnce sync.Once
	renewStop chan struct{}
	renewDone chan struct{}

	// finished is set once finish() has committed the soft or hard delete.
	// Only the engine goroutine touches it.
	finished bool
}

// tailStep runs one best-effort step after the delete committed, recovering
// a panic so the remaining steps (quota release, topic clear) still run and
// the outcome stays deleted.
func (e *deletionEngine) tailStep(name string, fn func()) {
	defer func() {
		if rec := recover(); rec != nil {
			e.s.agentLifecycleLog.Error("delete engine: post-finish step panicked",
				"agent_id", e.agentID(), "step", name, "panic", fmt.Sprint(rec))
		}
	}()
	fn()
}

func (e *deletionEngine) agentID() string { return e.plan.snapshot.ID }

// claimPred pins a write to this engine's claim.
func (e *deletionEngine) claimPred(states ...string) store.DeletionPredicate {
	claim := e.plan.claim
	return store.DeletionPredicate{Claim: &claim, States: states}
}

// step returns a context for one step with its own budget.
func (e *deletionEngine) step(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.ctx, d)
}

// lost returns the outcome for an engine that no longer holds its claim. It
// publishes nothing and rolls nothing back (design §2.1 "Losing engine").
func (e *deletionEngine) lost() deletionOutcome {
	e.s.agentLifecycleLog.Info("delete engine lost its claim",
		"agent_id", e.agentID(), "claim", e.plan.claim, "cause", context.Cause(e.ctx))
	return deletionOutcome{kind: deletionOutcomeLost}
}

// isLost reports whether the engine ctx was cancelled because the claim is
// gone (or about to be).
func (e *deletionEngine) isLost() bool {
	c := context.Cause(e.ctx)
	return errors.Is(c, errDeletionLost) || errors.Is(c, errDeletionLeaseLapsing)
}

// startRenewal renews the lease every deleteLeaseRenewInterval (design §2.1
// "Lease"). A renewal that affects 0 rows means the claim is gone: the engine
// stops. A renewal that errors is retried on the next tick; two consecutive
// misses abort, because the lease would lapse before the next tick.
func (e *deletionEngine) startRenewal() {
	e.renewStop = make(chan struct{})
	e.renewDone = make(chan struct{})
	go func() {
		defer close(e.renewDone)
		defer func() {
			if rec := recover(); rec != nil {
				// Without renewal the lease lapses; stop the engine rather
				// than let it act on a claim it may no longer hold.
				e.s.agentLifecycleLog.Error("delete engine: lease renewal panicked",
					"agent_id", e.agentID(), "claim", e.plan.claim, "panic", fmt.Sprint(rec))
				e.cancel(errDeletionLeaseLapsing)
			}
		}()
		ticker := time.NewTicker(deleteLeaseRenewInterval)
		defer ticker.Stop()
		misses := 0
		for {
			select {
			case <-e.renewStop:
				return
			case <-ticker.C:
			}
			lease := time.Now().Add(deleteLease)
			ctx, cancel := context.WithTimeout(e.base, deleteRenewTimeout)
			pred := e.claimPred(store.DeletionStateDeleting, store.DeletionStateFinalizing)
			pred.DeletedAtNull = true
			n, err := e.s.store.UpdateAgentDeletion(ctx, e.agentID(), pred,
				store.DeletionFields{LeaseAt: &lease, KeepUpdated: true})
			switch {
			case err != nil:
				misses++
				e.s.agentLifecycleLog.Warn("delete engine: lease renewal failed",
					"agent_id", e.agentID(), "claim", e.plan.claim, "misses", misses, "error", err)
				if misses >= 2 {
					cancel()
					e.cancel(errDeletionLeaseLapsing)
					return
				}
			case n == 0:
				cancel()
				e.cancel(errDeletionLost)
				return
			default:
				misses = 0
				e.publishStatus(ctx)
			}
			cancel()
		}
	}()
}

// stopRenewal stops the renewal goroutine and waits for it, so no renewal
// can land after a terminal write. Safe to call more than once.
func (e *deletionEngine) stopRenewal() {
	e.renewOnce.Do(func() {
		if e.renewStop == nil {
			return
		}
		close(e.renewStop)
		<-e.renewDone
	})
}

// publishStatus re-reads the row and publishes its status (with the current
// deletion view). Nothing is published for a row that is gone or
// soft-deleted.
func (e *deletionEngine) publishStatus(ctx context.Context) {
	row, err := e.s.store.GetAgent(ctx, e.agentID())
	if err != nil || !row.DeletedAt.IsZero() {
		return
	}
	e.s.events.PublishAgentStatus(ctx, row)
}

// abandon marks the claim abandoned at once (lease_at = now), so the row
// reads failed/abandoned without waiting out the lease. Best effort: if this
// write fails too, the lease still lapses on its own.
func (e *deletionEngine) abandon() {
	ctx, cancel := context.WithTimeout(e.base, deleteShortStep)
	defer cancel()
	now := time.Now()
	n, err := e.s.store.UpdateAgentDeletion(ctx, e.agentID(),
		e.claimPred(store.DeletionStateDeleting, store.DeletionStateFinalizing),
		store.DeletionFields{LeaseAt: &now})
	if err != nil || n == 0 {
		return
	}
	e.publishStatus(ctx)
}

// abandonOutcome abandons the claim (see abandon) and returns the outcome a
// requester sees for it: failed{abandoned}, the code the view shows for a
// lease-expired row with no stored code. Used after a panic and when a
// terminal write (finalizing, rollback, in_doubt) errors, so the row does not
// keep blocking start until the lease lapses.
func (e *deletionEngine) abandonOutcome() deletionOutcome {
	e.stopRenewal()
	e.abandon()
	return deletionOutcome{kind: deletionOutcomeFailed, code: store.DeletionCodeAbandoned,
		message: "the delete engine stopped unexpectedly; retry the delete"}
}

// run executes the engine steps (design §2.3 table).
func (e *deletionEngine) run() deletionOutcome {
	s := e.s
	agent := e.plan.snapshot
	e.startRenewal()

	// clearExposedPorts: the agent is being deleted, its ports are unreachable.
	func() {
		ctx, cancel := e.step(deleteShortStep)
		defer cancel()
		s.clearExposedPortsForAgent(ctx, agent.ID)
	}()

	if !e.plan.skipDispatch {
		if out, ok := e.dispatch(); !ok {
			return out
		}
	}

	// CAS deleting → finalizing.
	{
		ctx, cancel := e.step(deleteShortStep)
		finalizing := store.DeletionStateFinalizing
		n, err := s.store.UpdateAgentDeletion(ctx, agent.ID,
			e.claimPred(store.DeletionStateDeleting, store.DeletionStateFinalizing),
			store.DeletionFields{State: &finalizing})
		cancel()
		if err != nil {
			s.agentLifecycleLog.Error("delete engine: finalizing write failed",
				"agent_id", agent.ID, "error", err)
			// Without the finalizing write we cannot safely continue. Mark
			// the claim abandoned now so the row reads failed at once and a
			// retry can re-claim it without waiting out the lease.
			return e.abandonOutcome()
		}
		if n == 0 {
			return e.lost()
		}
	}

	// Revoke credentials: does not finalize on failure (security).
	if err := e.revoke(); err != nil {
		if e.isLost() {
			return e.lost()
		}
		msg := "Failed to revoke agent credentials: " + err.Error()
		return e.failFinalizing(store.DeletionCodeRevokeFailed, msg)
	}
	func() {
		ctx, cancel := e.step(deleteStepTimeout)
		defer cancel()
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType: "agent_credential_revoke",
			TargetType:   "agent_credential",
			TargetID:     agent.ID,
		})
	}()

	// Cancel scheduled events targeting the agent.
	func() {
		ctx, cancel := e.step(deleteStepTimeout)
		defer cancel()
		s.cancelScheduledEventsForAgent(ctx, agent)
	}()

	// Resolve DELETED notifications from the snapshot (DB reads only).
	var pending []pendingDeletedNotification
	if nd := s.notificationDispatcher; nd != nil {
		ctx, cancel := e.step(deleteStepTimeout)
		pending = nd.ResolveDeletedNotifications(ctx, agent)
		cancel()
	}

	if out, ok := e.finish(); !ok {
		return out
	}

	// Persist and deliver only after the row change succeeded. Each tail
	// step recovers on its own: the delete has committed.
	if nd := s.notificationDispatcher; nd != nil && len(pending) > 0 {
		e.tailStep("deliver notifications", func() { nd.DeliverDeletedNotifications(e.base, pending) })
	}

	// Release quotas and the topic default binding (each bounded).
	e.tailStep("release quotas", func() { s.releaseAgentQuotas(e.base, agent.ID, agent.RuntimeBrokerID) })
	e.tailStep("clear topic default", func() {
		ctx, cancel := context.WithTimeout(e.base, deleteStepTimeout)
		defer cancel()
		s.ClearTopicDefaultAgent(ctx, agent.ID, agent.Slug, agent.ProjectID)
	})

	return deletionOutcome{kind: deletionOutcomeDeleted}
}

// dispatch runs the broker (or managed-runtime) teardown. ok=false means the
// engine is done and out is its outcome.
func (e *deletionEngine) dispatch() (out deletionOutcome, ok bool) {
	s := e.s
	agent := e.plan.snapshot
	req := e.plan.req

	ctx, cancel := context.WithTimeout(e.ctx, deleteDispatchBudget)
	defer cancel()

	// Managed agent: clean up cloud resources directly, skip the broker.
	// Errors are logged, as before (follow-up 4).
	if isManagedAgentRuntime(agent.Runtime) {
		if err := s.managedAgentDelete(ctx, agent); err != nil {
			s.agentLifecycleLog.Warn("Failed to delete managed agent cloud resources",
				"agent_id", agent.ID, "error", err)
		}
		if e.isLost() {
			return e.lost(), false
		}
		return deletionOutcome{}, true
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil || agent.RuntimeBrokerID == "" {
		return deletionOutcome{}, true
	}

	// A created row with no launch in flight at claim (provision-only, or a
	// sync start whose request timed out): the dispatch is best-effort —
	// only when the broker looks reachable, and an error does not block
	// removing the record (ptone/scion#2635). A created row WITH an active
	// launch (T1, C1) takes the mandatory path below.
	bestEffort := e.plan.prior.Phase == string(state.PhaseCreated) && e.plan.prior.LaunchID == ""
	if bestEffort && !s.brokerReachable(ctx, agent) {
		return deletionOutcome{}, true
	}

	if deadline, has := ctx.Deadline(); has {
		ctx = withDeleteWaitBudget(ctx, time.Until(deadline))
	}
	startedAt := time.Now()
	if agent.DeletionStartedAt != nil {
		startedAt = *agent.DeletionStartedAt
	}
	err := dispatcher.DispatchAgentDelete(ctx, agent, req.DeleteFiles, req.RemoveBranch, req.Soft, startedAt)
	if err == nil {
		return deletionOutcome{}, true
	}
	if e.isLost() {
		return e.lost(), false
	}
	switch {
	case bestEffort:
		s.agentLifecycleLog.Warn("Failed to dispatch created-phase agent delete to broker (continuing)",
			"agent_id", agent.ID, "error", err)
		return deletionOutcome{}, true
	case req.Force:
		s.agentLifecycleLog.Warn("Failed to dispatch agent delete to broker (force=true, continuing)",
			"agent_id", agent.ID, "error", err)
		return deletionOutcome{}, true
	}

	s.agentLifecycleLog.Error("Failed to dispatch agent delete to broker", "agent_id", agent.ID, "error", err)
	return e.classifyDispatchFailure(err, startedAt)
}

// classifyDispatchFailure decides a non-force dispatch error from the
// dispatch table, whichever wait exit fired (design §2.3.1):
//   - a delete intent still outstanding → failed/in_doubt, phase not restored;
//   - an intent that reached done at or after DeletionStartedAt → success;
//   - neither → rollback with runtime_error (conflict for a broker 409);
//   - the check itself errors → in_doubt (the safe side).
func (e *deletionEngine) classifyDispatchFailure(dispatchErr error, since time.Time) (deletionOutcome, bool) {
	s := e.s
	agentID := e.agentID()

	ctx, cancel := context.WithTimeout(e.base, deleteShortStep)
	outstanding, oErr := s.store.HasOutstandingBrokerDispatch(ctx, agentID, brokerDispatchOpDelete)
	var completed bool
	var cErr error
	if oErr == nil && !outstanding {
		completed, cErr = s.store.HasCompletedBrokerDispatchSince(ctx, agentID, brokerDispatchOpDelete, since)
	}
	cancel()

	switch {
	case oErr != nil || cErr != nil || outstanding:
		if oErr != nil || cErr != nil {
			s.agentLifecycleLog.Error("delete engine: dispatch-table check failed; treating as in doubt",
				"agent_id", agentID, "outstanding_err", oErr, "completed_err", cErr)
		}
		return e.failInDoubt(), false
	case completed:
		s.agentLifecycleLog.Info("delete engine: deferred delete completed after the wait exited",
			"agent_id", agentID, "dispatch_error", dispatchErr)
		return deletionOutcome{}, true
	}

	code := store.DeletionCodeRuntimeError
	msg := "Failed to delete agent on runtime broker: " + dispatchErr.Error()
	var se *brokerStatusError
	if errors.As(dispatchErr, &se) && se.StatusCode == http.StatusConflict {
		// The broker returns 409 for more than one reason: the target is
		// ambiguous (several agents match in the project), or, for a
		// record-less substrate actor, its identity could not be verified
		// (agent_identity_unknown, the fail-closed RecordlessActorProber
		// case). Either way this is a conflict for the caller to resolve,
		// not a gateway failure.
		code = store.DeletionCodeConflict
		msg = "Failed to delete agent on runtime broker: " + se.brokerErrorMessage()
	} else if isBrokerRuntimeUnavailable(dispatchErr) {
		// The broker does not have the agent's recorded runtime available
		// (ptone/scion#2748): nothing ran there and the agent may still be
		// running, so restore it; the caller may retry, and force=true
		// still removes the record.
		code = store.DeletionCodeRuntimeUnavailable
		msg = brokerRuntimeUnavailableMessage(e.plan.snapshot.Runtime)
		retryAfter := brokerRuntimeRetryAfter(dispatchErr)
		// Stored before the rollback publishes, so a joiner woken by that
		// event already finds it.
		deletionRetryAfter.Store(agentID, deletionRetryAfterEntry{claim: e.plan.claim, value: retryAfter})
		out := e.rollback(code, msg)
		out.retryAfter = retryAfter
		return out, false
	}
	return e.rollback(code, msg), false
}

// rollback fails the delete and restores the prior phase and activity
// (design §2.3.1). Exception (T1, C2): if the prior carried a launch that has
// since ended (or been replaced), restore stopped with the activity cleared —
// the launch's report path already answered stopped and the broker removed
// the pod, and restoring provisioning would strand the row.
func (e *deletionEngine) rollback(code, msg string) deletionOutcome {
	e.stopRenewal()
	ctx, cancel := context.WithTimeout(e.base, deleteStepTimeout)
	defer cancel()
	now := time.Now()
	failed := store.DeletionStateFailed
	prior := e.plan.prior
	set := store.DeletionFields{
		State:    &failed,
		FailedAt: &now,
		Code:     &code,
		Error:    &msg,
		Derive: func(cur *store.Agent, f *store.DeletionFields) {
			if prior.LaunchID != "" && (cur.LaunchState != store.LaunchStateActive || cur.LaunchID != prior.LaunchID) {
				stopped := string(state.PhaseStopped)
				noActivity := ""
				f.Phase = &stopped
				f.Activity = &noActivity
				return
			}
			if prior.Phase != "" {
				phase, activity := prior.Phase, prior.Activity
				f.Phase = &phase
				f.Activity = &activity
			}
		},
	}
	n, err := e.s.store.UpdateAgentDeletion(ctx, e.agentID(), e.claimPred(store.DeletionStateDeleting), set)
	if err != nil {
		e.s.agentLifecycleLog.Error("delete engine: rollback write failed",
			"agent_id", e.agentID(), "code", code, "error", err)
		return e.abandonOutcome()
	}
	if n == 0 {
		return e.lost()
	}
	e.publishStatus(ctx)
	return deletionOutcome{kind: deletionOutcomeFailed, code: code, message: msg}
}

// deleteInDoubtMessage is the in_doubt banner text.
const deleteInDoubtMessage = "teardown may still complete on the broker; retry or force is safe"

// failInDoubt fails the delete with in_doubt without restoring a live phase:
// the hub does not know whether the container still exists. Start stays
// blocked while the intent is outstanding (deleteBlocksStart).
func (e *deletionEngine) failInDoubt() deletionOutcome {
	e.stopRenewal()
	ctx, cancel := context.WithTimeout(e.base, deleteStepTimeout)
	defer cancel()
	now := time.Now()
	failed := store.DeletionStateFailed
	code := store.DeletionCodeInDoubt
	msg := deleteInDoubtMessage
	n, err := e.s.store.UpdateAgentDeletion(ctx, e.agentID(), e.claimPred(store.DeletionStateDeleting),
		store.DeletionFields{State: &failed, FailedAt: &now, Code: &code, Error: &msg})
	if err != nil {
		e.s.agentLifecycleLog.Error("delete engine: in_doubt write failed",
			"agent_id", e.agentID(), "error", err)
		return e.abandonOutcome()
	}
	if n == 0 {
		return e.lost()
	}
	e.publishStatus(ctx)
	return deletionOutcome{kind: deletionOutcomeFailed, code: code, message: msg}
}

// failFinalizing records a failure after teardown (revoke_failed,
// finalize_failed). The row stays finalizing, with lease_at = now so it reads
// failed at once and never expires from view; only a retry or force lifts it.
func (e *deletionEngine) failFinalizing(code, msg string) deletionOutcome {
	e.stopRenewal()
	ctx, cancel := context.WithTimeout(e.base, deleteStepTimeout)
	defer cancel()
	now := time.Now()
	n, err := e.s.store.UpdateAgentDeletion(ctx, e.agentID(), e.claimPred(store.DeletionStateFinalizing),
		store.DeletionFields{LeaseAt: &now, FailedAt: &now, Code: &code, Error: &msg})
	if err != nil {
		e.s.agentLifecycleLog.Error("delete engine: failure write failed",
			"agent_id", e.agentID(), "code", code, "error", err)
		return e.abandonOutcome()
	}
	if n == 0 {
		return e.lost()
	}
	e.publishStatus(ctx)
	return deletionOutcome{kind: deletionOutcomeFailed, code: code, message: msg}
}

// revoke revokes every credential for the agent, each attempt with its own
// budget.
func (e *deletionEngine) revoke() error {
	var err error
	for attempt := 0; attempt < deleteRevokeAttempts; attempt++ {
		ctx, cancel := e.step(deleteStepTimeout)
		err = revokeAgentCredentials(ctx, e.s.store, e.agentID(), agentCredentialRevokeReasonDeleted)
		cancel()
		if err == nil || e.isLost() {
			return err
		}
	}
	return err
}

// finish removes the agent: soft (a predicate write that sets DeletedAt) or
// hard (publish deleted, then DeleteAgent). R1: the soft finish publishes only
// deleted, never a stopped status. ok=false means the engine is done.
func (e *deletionEngine) finish() (deletionOutcome, bool) {
	s := e.s
	agent := e.plan.snapshot
	pred := e.claimPred(store.DeletionStateFinalizing)
	pred.DeletedAtNull = true

	if e.plan.req.Soft {
		ctx, cancel := e.step(deleteStepTimeout)
		defer cancel()
		now := time.Now()
		stopped := string(state.PhaseStopped)
		none := store.DeletionStateNone
		empty := ""
		// The soft finish clears the whole marker (the claim epoch is kept),
		// so a restored agent carries no stale banner.
		set := store.DeletionFields{
			State: &none, Phase: &stopped, DeletedAt: &now,
			Code: &empty, Error: &empty, Prior: &empty, Request: &empty,
			ClearLeaseAt: true, ClearStartedAt: true, ClearFailedAt: true,
		}
		e.stopRenewal()
		n, err := s.finalizeAgentDeletion(ctx, agent.ID, pred, store.DeletionFinalizeSoft, set)
		if err != nil {
			if e.isLost() {
				return e.lost(), false
			}
			s.agentLifecycleLog.Error("delete engine: soft finish failed", "agent_id", agent.ID, "error", err)
			return e.failFinalizing(store.DeletionCodeFinalizeFailed, "Failed to finalize agent delete: "+err.Error()), false
		}
		if n == 0 {
			return e.lost(), false
		}
		e.finished = true
		e.tailStep("publish deleted", func() { s.events.PublishAgentDeleted(e.base, agent.ID, agent.ProjectID) })
		return deletionOutcome{}, true
	}

	// Hard delete: publish deleted BEFORE removing the record (the
	// documented order), then delete, retried once.
	e.stopRenewal()
	s.events.PublishAgentDeleted(e.base, agent.ID, agent.ProjectID)
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := e.step(deleteStepTimeout)
		var n int
		n, err = s.finalizeAgentDeletion(ctx, agent.ID, pred, store.DeletionFinalizeHard, store.DeletionFields{})
		cancel()
		if err == nil {
			if n == 1 || e.rowGone() {
				e.finished = true
				return deletionOutcome{}, true
			}
			return e.lost(), false
		}
		if e.isLost() {
			return e.lost(), false
		}
	}
	s.agentLifecycleLog.Error("delete engine: hard delete failed", "agent_id", agent.ID, "error", err)
	return e.failFinalizing(store.DeletionCodeFinalizeFailed, "Failed to finalize agent delete: "+err.Error()), false
}

// rowGone reports whether the agent row no longer exists (a concurrent
// hard delete got there first).
func (e *deletionEngine) rowGone() bool {
	ctx, cancel := e.step(deleteShortStep)
	defer cancel()
	_, err := e.s.store.GetAgent(ctx, e.agentID())
	return errors.Is(err, store.ErrNotFound)
}

// agentDeletionFinalizeSeam runs inside the finalize transaction (soft and
// hard, including the hard delete of an incomplete create), just before
// commit, with a transaction-scoped store. A non-nil error rolls the
// finalize back, and the engine fails with finalize_failed. It is the
// attachment point for lifecycle hooks and op-ID stamping
// (ptone/scion#2121); a no-op until then. Tests may replace it.
var agentDeletionFinalizeSeam store.DeletionFinalizeHook = func(context.Context, store.Store, *store.Agent, store.DeletionFinalizeMode) error {
	return nil
}

// finalizeAgentDeletion is the delete engine's single terminal write: one
// store transaction that re-checks the claim, applies the soft or hard
// delete, and runs agentDeletionFinalizeSeam before commit.
func (s *Server) finalizeAgentDeletion(ctx context.Context, agentID string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields) (int, error) {
	return s.store.FinalizeAgentDeletion(ctx, agentID, pred, mode, set, agentDeletionFinalizeSeam)
}

// --- Request side (design §2.4) ---

// agentDeleteAcceptedResponse is the 202 body.
type agentDeleteAcceptedResponse struct {
	AgentID  string              `json:"agentId"`
	Deletion *store.DeletionInfo `json:"deletion"`
}

// deleteSyncWaitFor returns the sync wait for r: deleteSyncWait, lowered
// (never raised) by "Prefer: wait=N" (seconds).
func deleteSyncWaitFor(r *http.Request) time.Duration {
	wait := deleteSyncWait
	for _, pref := range r.Header.Values("Prefer") {
		for _, part := range strings.Split(pref, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "wait") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				continue
			}
			if d := time.Duration(n) * time.Second; d < wait {
				wait = d
			}
		}
	}
	return wait
}

// writeDeletionFailure writes the HTTP answer for a failed delete: 409 for a
// broker conflict, a retryable 503 when the broker does not have the agent's
// runtime available, 502 otherwise (today's codes on the fast path).
// retryAfter is the 503's Retry-After; "" means defaultBrokerRuntimeRetryAfter.
func writeDeletionFailure(w http.ResponseWriter, agentID, code, message, retryAfter string) {
	if message == "" {
		message = "agent delete failed (" + code + ")"
	}
	details := map[string]interface{}{"agentId": agentID, "deletionCode": code}
	if code == store.DeletionCodeConflict {
		writeError(w, http.StatusConflict, ErrCodeConflict, message, details)
		return
	}
	if code == store.DeletionCodeRuntimeUnavailable {
		if retryAfter == "" {
			retryAfter = defaultBrokerRuntimeRetryAfter
		}
		w.Header().Set("Retry-After", retryAfter)
		writeError(w, http.StatusServiceUnavailable, brokerCodeRuntimeUnavailable, message, details)
		return
	}
	writeError(w, http.StatusBadGateway, ErrCodeRuntimeError, message, details)
}

// writeDeleteAccepted writes 202 with the agent's current deletion view.
func (s *Server) writeDeleteAccepted(w http.ResponseWriter, agentID string) {
	var view *store.DeletionInfo
	ctx, cancel := context.WithTimeout(context.Background(), deleteShortStep)
	defer cancel()
	if row, err := s.store.GetAgent(ctx, agentID); err == nil {
		view = store.ComputeAgentDeletion(row, time.Now())
	}
	writeJSON(w, http.StatusAccepted, agentDeleteAcceptedResponse{AgentID: agentID, Deletion: view})
}

// joinAgentDeletion waits for a delete owned by another request (or by an
// engine that reported lost) and writes its outcome (design §2.4). It
// subscribes first, then re-reads the row on every wake-up and on a 1s poll;
// events are only a wake-up signal, the row decides:
//   - row gone, or DeletedAt set → 204;
//   - failed (or lease expired) at a claim >= observedClaim → the answer for
//     the code the view shows: 409 for a conflict, a retryable 503 with
//     Retry-After for runtime_unavailable, 502 otherwise;
//   - deadline → 202.
func (s *Server) joinAgentDeletion(w http.ResponseWriter, r *http.Request, agentID string, observedClaim int64, deadline time.Time) {
	evCh, unsub := s.events.Subscribe("agent."+agentID+".deleted", "agent."+agentID+".status")
	defer unsub()

	ticker := time.NewTicker(deleteJoinPollInterval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	for first := true; ; first = false {
		if s.resolveJoinFromRow(w, r.Context(), agentID, observedClaim) {
			return
		}
		if first && joinAgentDeletionHook != nil {
			joinAgentDeletionHook(agentID)
		}
		select {
		case _, ok := <-evCh:
			if !ok {
				evCh = nil // closed: keep polling
			}
		case <-ticker.C:
		case <-timer.C:
			if s.resolveJoinFromRow(w, r.Context(), agentID, observedClaim) {
				return
			}
			s.writeDeleteAccepted(w, agentID)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// joinAgentDeletionHook, when set by a test, runs once per join after the
// joiner has subscribed and its first re-read left the outcome undecided.
var joinAgentDeletionHook func(agentID string)

// resolveJoinFromRow reads the row and, when it decides the outcome, writes
// the response and returns true.
func (s *Server) resolveJoinFromRow(w http.ResponseWriter, ctx context.Context, agentID string, observedClaim int64) bool {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteShortStep)
	defer cancel()
	row, err := s.store.GetAgent(rctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false // transient: try again on the next tick
	}
	if !row.DeletedAt.IsZero() {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if row.DeletionClaim < observedClaim {
		return false
	}
	now := time.Now()
	if row.DeletionActive(now) {
		return false
	}
	if code := row.DeletionEffectiveCode(now); code != "" {
		writeDeletionFailure(w, agentID, code, row.DeletionError, deletionRetryAfterFor(agentID, row.DeletionClaim))
		return true
	}
	if row.DeletionState == store.DeletionStateNone && row.DeletionClaim >= observedClaim {
		// The marker was cleared after a failure (a successful start or stop
		// clears it): the delete did not complete.
		writeDeletionFailure(w, agentID, store.DeletionCodeRuntimeError, "agent delete did not complete", "")
		return true
	}
	return false
}
