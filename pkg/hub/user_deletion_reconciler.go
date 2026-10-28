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
	"sort"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Stop work and completion of forced user deletions (ptone/scion#2143).
//
// reconcileUserDeletion brings one deletion in state stopping forward:
//
//   - it refreshes each agent's outcome from the agent row: soft-deleted is
//     stopped; a delete in flight is stopping; a failed delete is failed;
//     a row that is gone is removed unless a non-force engine run already
//     recorded the agent stopped; anything else is pending;
//   - it claims the agent delete engine for each pending or failed agent
//     whose backoff has passed, deepest ancestry first, as a plain
//     (non-force) delete requested by the deletion's requester;
//   - once every agent is stopped or removed, it runs the completion
//     transaction (completeUserDeletion).
//
// It runs after the request commits (kickUserDeletion), when the delete
// engine finishes an agent of a stopping deletion, and on every tick of the
// user-deletion-reconcile handler, on every replica. Correctness across
// replicas rests on the engine's per-agent lease (a second claim joins or
// affects nothing) and on the completion's compare-and-set under the user
// row lock. In one process, a per-deletion mutex keeps one reconcile, or
// one engine-finish record, at a time.
//
// A deletion whose agent cannot be torn down (its Runtime Broker is gone)
// stays stopping with the agent failed, visible on GET
// /api/v1/users/{id}/deletion, until an operator removes the agent with
// the agent delete's force; that agent is then reported removed (teardown
// not confirmed). There is no timeout.

// Timing knobs. Variables so tests can shrink them.
var (
	// userDeletionRetryBase is the wait before the first retry of an
	// agent whose delete failed; each later retry doubles it, up to
	// userDeletionRetryMax.
	userDeletionRetryBase = 30 * time.Second
	userDeletionRetryMax  = time.Hour
	// userDeletionReconcileTimeout bounds one reconcile of one deletion.
	userDeletionReconcileTimeout = 2 * time.Minute
	// userDeletionReconcileBatch is how many stopping deletions one tick
	// reconciles.
	userDeletionReconcileBatch = 100
)

// userDeletionBrokerUnavailable is an agent's last error when its Runtime
// Broker is not connected, so no delete was attempted.
const userDeletionBrokerUnavailable = "runtime broker unavailable"

// userDeletionLock returns the in-process mutex of deletion id.
func (s *Server) userDeletionLock(id string) *sync.Mutex {
	m, _ := s.userDeletionLocks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// kickUserDeletion reconciles deletion id in the background, on a context
// detached from any request. retryNow claims failed agents again without
// waiting out their backoff (a repeated force request asks for that).
func (s *Server) kickUserDeletion(id string, retryNow bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), userDeletionReconcileTimeout)
		defer cancel()
		s.reconcileUserDeletion(ctx, id, retryNow)
	}()
}

// userDeletionReconcileHandler is the recurring handler that reconciles
// every stopping deletion.
//
// It runs whether or not hub.user_delete_force_cascade is on. The
// experiment governs new force requests (and the deletion resource); a
// deletion accepted while it was on runs to completion after it is turned
// off. By then the request has revoked the user's authority and the user
// is deleting; what is left is stopping the agents the requester was
// authorized to delete and removing the user. Pausing that work would
// leave the user deleting with no way to see or retry the deletion (the
// resource answers 404 and force is ignored while the experiment is off).
func (s *Server) userDeletionReconcileHandler() func(ctx context.Context) {
	return s.reconcileUserDeletions
}

// reconcileUserDeletions reconciles up to userDeletionReconcileBatch
// stopping deletions, least recently reconciled first. Each reconcile
// records itself on the deletion, so successive ticks work through every
// deletion in turn however many are stuck.
func (s *Server) reconcileUserDeletions(ctx context.Context) {
	ds, err := s.store.ListUserDeletionsByState(ctx, store.UserDeletionStateStopping, userDeletionReconcileBatch)
	if err != nil {
		slog.WarnContext(ctx, "user deletion reconcile: list failed", "error", err)
		return
	}
	for i := range ds {
		if ctx.Err() != nil {
			return
		}
		rctx, cancel := context.WithTimeout(ctx, userDeletionReconcileTimeout)
		s.reconcileUserDeletion(rctx, ds[i].ID, false)
		cancel()
	}
}

// userDeletionRetryDue reports whether an agent with this record may be
// claimed again at time ts.
func userDeletionRetryDue(rec *store.UserDeletionAgent, ts time.Time) bool {
	if rec.Attempts == 0 {
		return true
	}
	wait := userDeletionRetryBase
	for i := 1; i < rec.Attempts && wait < userDeletionRetryMax; i++ {
		wait *= 2
	}
	if wait > userDeletionRetryMax {
		wait = userDeletionRetryMax
	}
	return !ts.Before(rec.UpdatedAt.Add(wait))
}

// reconcileUserDeletion brings deletion id forward (see the file comment).
// retryNow claims pending and failed agents whatever their backoff. Errors
// are recorded on the deletion (last_error) and logged.
func (s *Server) reconcileUserDeletion(ctx context.Context, id string, retryNow bool) {
	mu := s.userDeletionLock(id)
	mu.Lock()
	defer mu.Unlock()

	d, err := s.store.GetUserDeletion(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.userDeletionLocks.Delete(id)
		} else {
			slog.WarnContext(ctx, "user deletion reconcile: read failed", "deletion_id", id, "error", err)
		}
		return
	}
	if d.State != store.UserDeletionStateStopping {
		// Completed, here or by another replica. Dropping the entry while
		// this pass holds the mutex is harmless: a pass that creates a
		// fresh mutex reads the same completed state and returns.
		s.userDeletionLocks.Delete(id)
		return
	}
	recs, err := s.store.ListUserDeletionAgents(ctx, d.ID)
	if err != nil {
		s.touchUserDeletion(ctx, d, fmt.Sprintf("list agents: %v", err))
		return
	}

	ts := time.Now()
	type candidate struct {
		rec   *store.UserDeletionAgent
		agent *store.Agent
	}
	var claims []candidate
	var lastErr string
	for i := range recs {
		rec := &recs[i]
		a, err := s.refreshUserDeletionAgent(ctx, d, rec, ts)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if a == nil {
			continue
		}
		retryable := rec.Outcome == store.UserDeletionOutcomePending || rec.Outcome == store.UserDeletionOutcomeFailed
		// A delete in flight whose lease lapsed (its engine died) is
		// claimed again, as an abandoned delete is, after the same
		// backoff as a failed one, so a delete that keeps dying is not
		// claimed in a loop.
		abandoned := rec.Outcome == store.UserDeletionOutcomeStopping &&
			a.DeletionLeaseAt != nil && a.DeletionLeaseAt.Before(ts)
		if (retryable || abandoned) && (retryNow || userDeletionRetryDue(rec, ts)) {
			claims = append(claims, candidate{rec: rec, agent: a})
		}
	}

	// Deepest ancestry first, so an agent's children are claimed before it.
	sort.SliceStable(claims, func(i, j int) bool {
		return len(claims[i].agent.Ancestry) > len(claims[j].agent.Ancestry)
	})
	for _, c := range claims {
		if err := s.claimUserDeletionAgent(ctx, d, c.rec, c.agent, ts); err != nil {
			lastErr = err.Error()
		}
	}

	done := true
	for i := range recs {
		if o := recs[i].Outcome; o != store.UserDeletionOutcomeStopped && o != store.UserDeletionOutcomeRemoved {
			done = false
			if recs[i].Outcome == store.UserDeletionOutcomeFailed && lastErr == "" {
				lastErr = fmt.Sprintf("agent %s: %s", recs[i].AgentID, recs[i].LastError)
			}
		}
	}
	if done {
		completed, err := s.completeUserDeletion(ctx, d)
		switch {
		case errors.Is(err, errUserDeletionNotDone):
			// A record this pass read as final is not final in the
			// completion's transaction: nothing to do until the next
			// reconcile, which reads it again.
		case err != nil:
			lastErr = "complete: " + err.Error()
		case completed:
			return
		default:
			// The completion found agents blocking again and recorded them
			// pending (or another process completed it): reconcile again
			// once this pass releases the deletion's lock, so they are
			// claimed without waiting for the next tick.
			s.kickUserDeletion(d.ID, false)
		}
	}
	s.touchUserDeletion(ctx, d, lastErr)
}

// touchUserDeletion records a reconcile and its error, best effort.
func (s *Server) touchUserDeletion(ctx context.Context, d *store.UserDeletion, lastErr string) {
	if lastErr != "" {
		slog.WarnContext(ctx, "user deletion reconcile", "deletion_id", d.ID, "user_id", d.UserID, "error", lastErr)
	}
	if err := s.store.TouchUserDeletionReconciled(ctx, d.ID, time.Now(), lastErr); err != nil {
		slog.WarnContext(ctx, "user deletion reconcile: record failed", "deletion_id", d.ID, "error", err)
	}
}

// refreshUserDeletionAgent sets rec's outcome from the agent row and saves
// it when it changed. It returns the row, or nil when the row is gone or
// soft-deleted (nothing left to claim).
func (s *Server) refreshUserDeletionAgent(ctx context.Context, d *store.UserDeletion, rec *store.UserDeletionAgent, ts time.Time) (*store.Agent, error) {
	prev := *rec
	a, err := s.store.GetAgent(ctx, rec.AgentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if rec.Outcome != store.UserDeletionOutcomeStopped {
			// Re-read the record: an engine run may have recorded the
			// agent stopped since the list was read.
			if fresh, ferr := s.userDeletionAgentRecord(ctx, d.ID, rec.AgentID); ferr == nil && fresh != nil &&
				fresh.Outcome == store.UserDeletionOutcomeStopped {
				*rec = *fresh
				return nil, nil
			}
			rec.Outcome = store.UserDeletionOutcomeRemoved
		}
		a = nil
	case err != nil:
		return nil, fmt.Errorf("agent %s: %w", rec.AgentID, err)
	case !a.DeletedAt.IsZero():
		rec.Outcome = store.UserDeletionOutcomeStopped
		rec.LastError = ""
		a = nil
	case a.DeletionState == store.DeletionStateFinalizing && a.DeletionFailedAt != nil:
		// The finalize failed and rolled back (for example a hook of the
		// finalize transaction refused). Only a new claim clears the
		// failure time, so the agent is failed, with the engine's error,
		// and is claimed again only after its backoff.
		rec.Outcome = store.UserDeletionOutcomeFailed
		rec.LastError = a.DeletionError
	case a.DeletionState == store.DeletionStateDeleting || a.DeletionState == store.DeletionStateFinalizing:
		rec.Outcome = store.UserDeletionOutcomeStopping
	case a.DeletionState == store.DeletionStateFailed:
		rec.Outcome = store.UserDeletionOutcomeFailed
		rec.LastError = a.DeletionError
	default:
		// No delete in flight. A failure this cascade recorded without
		// claiming (the broker was not connected) stays failed.
		if rec.Outcome != store.UserDeletionOutcomeFailed {
			rec.Outcome = store.UserDeletionOutcomePending
		}
	}
	if rec.Outcome != prev.Outcome || rec.LastError != prev.LastError {
		rec.UpdatedAt = ts
		if err := s.store.UpsertUserDeletionAgent(ctx, rec); err != nil {
			// Keep the record as it was read, so this pass does not
			// treat an outcome it could not save as final.
			*rec = prev
			return a, fmt.Errorf("record agent %s: %w", rec.AgentID, err)
		}
	}
	return a, nil
}

// userDeletionAgentRecord returns the current record of agentID in
// deletion id, or nil.
func (s *Server) userDeletionAgentRecord(ctx context.Context, id, agentID string) (*store.UserDeletionAgent, error) {
	recs, err := s.store.ListUserDeletionAgents(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].AgentID == agentID {
			return &recs[i], nil
		}
	}
	return nil, nil
}

// claimUserDeletionAgent starts a plain delete of agent a for the
// deletion and records the attempt. A delete is not attempted while the
// agent's Runtime Broker is not connected (the agent is recorded failed),
// matching the agent delete route; managed runtimes, a created agent with
// no launch in flight and a finalizing row need no broker.
func (s *Server) claimUserDeletionAgent(ctx context.Context, d *store.UserDeletion, rec *store.UserDeletionAgent, a *store.Agent, ts time.Time) error {
	rec.Attempts++
	rec.UpdatedAt = ts
	save := func() error {
		if err := s.store.UpsertUserDeletionAgent(ctx, rec); err != nil {
			return fmt.Errorf("record agent %s: %w", rec.AgentID, err)
		}
		return nil
	}

	createdNoLaunch := a.Phase == string(state.PhaseCreated) && a.LaunchState != store.LaunchStateActive
	finalizing := a.DeletionState == store.DeletionStateFinalizing
	if !isManagedAgentRuntime(a.Runtime) && !createdNoLaunch && !finalizing && !s.brokerReachable(ctx, a) {
		rec.Outcome = store.UserDeletionOutcomeFailed
		rec.LastError = userDeletionBrokerUnavailable
		return save()
	}
	plan, err := s.claimAgentDeletion(ctx, a.ID, agentDeleteParams{
		deleteFiles:  true,
		removeBranch: true,
		requestedBy:  d.RequestedByID,
	})
	if refusal, refused := placementRefusalOf(err); refused {
		rec.Outcome = store.UserDeletionOutcomeFailed
		rec.LastError = "placement conflict: " + refusal.Error()
		return save()
	}
	if err != nil {
		rec.Outcome = store.UserDeletionOutcomeFailed
		rec.LastError = err.Error()
		return save()
	}
	if plan == nil {
		// Not claimed: another delete holds the row or just finished.
		// The next reconcile reads what it did.
		return save()
	}
	rec.Outcome = store.UserDeletionOutcomeStopping
	rec.LastError = ""
	if err := save(); err != nil {
		return err
	}
	s.events.PublishAgentStatus(ctx, plan.snapshot)
	// The engine runs detached and reports through
	// noteUserDeletionAgentFinished.
	s.runAgentDeletion(ctx, plan)
	return nil
}

// recordUserDeletionAgentStoppedTx runs inside the delete engine's
// finalize transaction (agentFinalizeHook), after the soft or hard delete
// work. When the delete was not a force delete, the engine got here only
// after the runtime teardown succeeded, so every stopping deletion that
// lists the agent records it as stopped, in the same transaction that
// soft-deletes or removes the row: no reconcile can see the row gone
// without this record. A force delete skips broker failures, so it is not
// recorded; its row is gone without confirmation, which the reconcile
// reports as removed. A stored request that cannot be read is treated as
// a force delete (not confirmed).
func recordUserDeletionAgentStoppedTx(ctx context.Context, tx store.Store, a *store.Agent) error {
	req, ok := storedDeletionRequest(a)
	if !ok || req.Force {
		return nil
	}
	recs, err := tx.ListUserDeletionAgentsForAgent(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("user deletion: list records of agent %s: %w", a.ID, err)
	}
	for i := range recs {
		rec := recs[i]
		rec.Outcome = store.UserDeletionOutcomeStopped
		rec.LastError = ""
		rec.UpdatedAt = time.Now()
		if err := tx.UpsertUserDeletionAgent(ctx, &rec); err != nil {
			return fmt.Errorf("user deletion: record agent %s stopped: %w", a.ID, err)
		}
	}
	return nil
}

// noteUserDeletionAgentFinished is called by the delete engine when a run
// ends with a deleted or failed outcome. Every stopping deletion that lists
// the agent is reconciled (the outcome itself is recorded in the finalize
// transaction, recordUserDeletionAgentStoppedTx, or read from the agent
// row). It returns at once; the work runs in the background.
func (s *Server) noteUserDeletionAgentFinished(plan *agentDeletionPlan, out deletionOutcome) {
	if out.kind != deletionOutcomeDeleted && out.kind != deletionOutcomeFailed {
		return
	}
	agentID := plan.snapshot.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), userDeletionReconcileTimeout)
		defer cancel()
		recs, err := s.store.ListUserDeletionAgentsForAgent(ctx, agentID)
		if err != nil {
			slog.WarnContext(ctx, "user deletion: engine finish lookup failed", "agent_id", agentID, "error", err)
			return
		}
		for i := range recs {
			s.reconcileUserDeletion(ctx, recs[i].DeletionID, false)
		}
	}()
}

// completeUserDeletion is the completion transaction of a forced deletion.
// Under the user row lock it re-lists the blockers: an agent that blocks
// again (for example one restored, or created through a path that does not
// take the user lock) is recorded pending, its edges and assignments are
// deactivated under the deletion's ID, and the deletion stays stopping.
// Otherwise it deactivates every edge left with the user as the delegator
// (cause delegator_deleted), deletes the user row and what hangs off it
// (deleteUserRowCascadeModeTx), moves the deletion to completed
// (compare-and-set: of two concurrent completions one takes effect) and
// writes the user_delete audit record, which counts the agents stopped
// (teardown confirmed) apart from those removed (not confirmed). After the
// commit, afterUserDeleted runs as for any user delete. It reports whether
// this call completed the deletion, and returns errUserDeletionNotDone,
// having written nothing, when an agent record read in its transaction is
// neither stopped nor removed.
func (s *Server) completeUserDeletion(ctx context.Context, d *store.UserDeletion) (bool, error) {
	ts := time.Now()
	completed := false
	err := s.store.WithTx(ctx, func(tx store.Store) error {
		userGone := false
		if err := tx.LockUserRow(ctx, d.UserID, true); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			userGone = true
		}
		// The records are read in the transaction, under the user row
		// lock, so the audit counts match what it commits. Stopped and
		// removed are final (UpsertUserDeletionAgent never changes them;
		// only this transaction reopens a record), so a concurrent write
		// cannot change a record counted here.
		recs, err := tx.ListUserDeletionAgents(ctx, d.ID)
		if err != nil {
			return err
		}
		known := make(map[string]store.UserDeletionAgent, len(recs))
		var counts userDeletionCounts
		removed := []string{}
		for _, r := range recs {
			if r.Outcome != store.UserDeletionOutcomeStopped && r.Outcome != store.UserDeletionOutcomeRemoved {
				// The caller decided from an older read: not done.
				return errUserDeletionNotDone
			}
			known[r.AgentID] = r
			counts.add(r.Outcome)
			if r.Outcome == store.UserDeletionOutcomeRemoved {
				removed = append(removed, r.AgentID)
			}
		}

		var before string
		if !userGone {
			u, err := tx.GetUser(ctx, d.UserID)
			if err != nil {
				return err
			}
			cur, err := collectUserDeleteBlockers(ctx, tx, d.UserID)
			if err != nil {
				return err
			}
			if len(cur.agents) > 0 {
				if completeUserDeletionBeforeAgentLock != nil {
					completeUserDeletionBeforeAgentLock(ctx, tx, cur.agents)
				}
				// Lock order: user row (above), then the agent rows, then
				// the agents' records. A finalize takes the agent row and
				// then the records, so it either commits before this lock
				// (the re-read below sees the row gone or soft-deleted, and
				// its stopped record stays) or waits for this transaction.
				ids := make([]string, 0, len(cur.agents))
				for i := range cur.agents {
					ids = append(ids, cur.agents[i].ID)
				}
				if err := tx.LockAgentRows(ctx, ids); err != nil {
					return fmt.Errorf("lock blocking agents: %w", err)
				}
				deact := store.Deactivation{Cause: store.EdgeDeactivationUserDeleteCascade, OpID: d.ID}
				for i := range cur.agents {
					a := &cur.agents[i]
					live, err := tx.GetAgent(ctx, a.ID)
					if errors.Is(err, store.ErrNotFound) {
						continue
					}
					if err != nil {
						return fmt.Errorf("re-read blocking agent %s: %w", a.ID, err)
					}
					if !live.DeletedAt.IsZero() {
						continue
					}
					if _, err := tx.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, a.ID, deact); err != nil {
						return fmt.Errorf("deactivate delegation edges of agent %s: %w", a.ID, err)
					}
					if _, err := tx.DeactivateAgentServiceAccountAssignments(ctx, a.ID, deact); err != nil {
						return fmt.Errorf("deactivate service-account assignments of agent %s: %w", a.ID, err)
					}
					// The agent blocks again, so its record is reopened even
					// when it reads stopped or removed: the one move out of
					// a final outcome.
					rec := known[a.ID]
					rec.DeletionID, rec.AgentID, rec.Slug, rec.ProjectID = d.ID, a.ID, a.Slug, a.ProjectID
					rec.Outcome, rec.LastError, rec.UpdatedAt = store.UserDeletionOutcomePending, "", ts
					if err := tx.ReopenUserDeletionAgent(ctx, &rec); err != nil {
						return fmt.Errorf("record agent %s: %w", a.ID, err)
					}
				}
				return nil
			}
			// Edges with the user as the delegator left active (one a
			// concurrent restore reactivated after the request) end here,
			// under the user row lock.
			deact := store.Deactivation{Cause: store.EdgeDeactivationDelegatorDeleted, OpID: d.ID}
			if _, err := tx.DeactivateDelegationEdgesForDelegator(ctx, store.DelegationPrincipalUser, d.UserID, deact); err != nil {
				return fmt.Errorf("deactivate delegation edges of user: %w", err)
			}
			if err := deleteUserRowCascadeModeTx(ctx, tx, d.UserID, s.membershipNow(), true); err != nil {
				return err
			}
			b, err := json.Marshal(map[string]string{"email": u.Email, "role": u.Role, "status": u.Status})
			if err != nil {
				return err
			}
			before = string(b)
		}
		changed, err := tx.CompleteUserDeletion(ctx, d.ID, ts)
		if err != nil {
			return err
		}
		if !changed {
			// Completed by another reconcile: roll back this one.
			return errUserDeletionNotStopping
		}
		after, err := json.Marshal(map[string]interface{}{
			"deletionId":    d.ID,
			"stopped":       counts.Stopped,
			"removed":       counts.Removed,
			"removedAgents": removed,
			"completedBy":   "user_deletion_reconciler",
		})
		if err != nil {
			return err
		}
		record := &store.MutationAuditRecord{
			MutationType:       "user_delete",
			TargetType:         "user",
			TargetID:           d.UserID,
			BeforeSummary:      before,
			AfterSummary:       string(after),
			Timestamp:          ts,
			ActorPrincipalKind: d.RequestedByKind,
			ActorPrincipalID:   d.RequestedByID,
			ExecutorKind:       "user_deletion_reconciler",
			ExecutorID:         d.ID,
		}
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return fmt.Errorf("audit user delete: %w", err)
		}
		completed = true
		return nil
	})
	if errors.Is(err, errUserDeletionNotStopping) {
		return false, nil
	}
	if err != nil || !completed {
		return false, err
	}
	s.userDeletionLocks.Delete(d.ID)
	s.afterUserDeleted(ctx, d.UserID)
	return true, nil
}

// completeUserDeletionBeforeAgentLock is a test seam. When set, it runs in
// the completion transaction after the blockers are re-listed and before
// their agent rows are locked. Production code leaves it nil.
var completeUserDeletionBeforeAgentLock func(ctx context.Context, tx store.Store, agents []store.Agent)

// errUserDeletionNotStopping rolls back a completion that lost the
// compare-and-set to a concurrent one.
var errUserDeletionNotStopping = errors.New("user deletion is not stopping")

// errUserDeletionNotDone rolls back a completion that finds, in its
// transaction, an agent record that is neither stopped nor removed.
var errUserDeletionNotDone = errors.New("user deletion has agents left to stop")
