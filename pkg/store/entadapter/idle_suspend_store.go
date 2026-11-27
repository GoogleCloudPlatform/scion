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

package entadapter

import (
	"context"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// defaultIdleSuspendClaimTTL bounds how long an un-cleared idle-suspend claim
// blocks delivery/auto-wake if a hub replica crashes mid-stop.
const defaultIdleSuspendClaimTTL = 45 * time.Second

const (
	maxIdleSuspendTxRetries  = 32
	idleSuspendRetryBaseWait = time.Millisecond
	idleSuspendRetryMaxWait  = 15 * time.Millisecond
)

// withIdleSuspendLockedAgent wraps withLockedAgent with bounded retries on
// transient database conflicts (SQLite SQLITE_BUSY / SQLITE_LOCKED when two
// separate database handles upgrade a DEFERRED read transaction to a write on
// the same file, or Postgres 40001 / 40P01). Exhausted transient errors are
// marked with store.ErrTransient.
func (s *AgentStore) withIdleSuspendLockedAgent(ctx context.Context, agentID string, fn lockedAgentFn) error {
	wait := idleSuspendRetryBaseWait
	for attempt := 0; ; attempt++ {
		err := s.withLockedAgent(ctx, agentID, fn)
		if err == nil {
			return nil
		}
		if !isTransientDBError(err) || attempt >= maxIdleSuspendTxRetries {
			return markTransient(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if wait < idleSuspendRetryMaxWait {
			wait *= 2
			if wait > idleSuspendRetryMaxWait {
				wait = idleSuspendRetryMaxWait
			}
		}
	}
}

func optTimeAttr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func optStringAttr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// checkIdleSuspendRow evaluates cond against a locked ent.Agent row at
// store-clock now for ClaimIdleSuspend. An already-active
// idle_suspend_claim_until rejects so two sweeps never double-claim.
func checkIdleSuspendRow(row *ent.Agent, cond store.IdleSuspendCondition, now time.Time) *store.IdleSuspendPreconditionError {
	if row == nil {
		return &store.IdleSuspendPreconditionError{Predicate: "row_nil"}
	}
	if row.DeletedAt != nil || store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, now) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "deleted_or_deleting",
			Attrs: []any{
				"deleted_at", optTimeAttr(row.DeletedAt),
				"deletion_state", row.DeletionState,
				"deletion_lease_at", optTimeAttr(row.DeletionLeaseAt),
				"store_now", now.UTC(),
			},
		}
	}
	if !claimEligible(row) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "reincarnation_in_flight",
			Attrs: []any{
				"reincarnation_state", row.ReincarnationState,
			},
		}
	}
	if heldClaimError(row) != nil {
		return &store.IdleSuspendPreconditionError{
			Predicate: "start_claim_held",
			Attrs: []any{
				"start_claim_id", optStringAttr(row.StartClaimID),
				"start_claim_kind", row.StartClaimKind,
				"start_claim_state", row.StartClaimState,
				"start_claim_owner", row.StartClaimOwner,
			},
		}
	}
	if row.IdleSuspendClaimUntil != nil && now.Before(*row.IdleSuspendClaimUntil) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "idle_claim_active",
			Attrs: []any{
				"idle_suspend_claim_until", row.IdleSuspendClaimUntil.UTC(),
				"store_now", now.UTC(),
			},
		}
	}
	if row.IdleSuspendClaimUntil != nil && cond.ClaimBackoff > 0 && now.Before(row.IdleSuspendClaimUntil.Add(cond.ClaimBackoff)) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "idle_claim_backoff",
			Attrs: []any{
				"idle_suspend_claim_until", row.IdleSuspendClaimUntil.UTC(),
				"claim_backoff", cond.ClaimBackoff,
				"store_now", now.UTC(),
			},
		}
	}
	if row.Phase != string(state.PhaseRunning) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "phase_not_running",
			Attrs: []any{
				"phase", row.Phase,
			},
		}
	}
	if cond.RequireSuspendPolicyAuto && row.SuspendPolicy == agent.SuspendPolicyNever {
		return &store.IdleSuspendPreconditionError{
			Predicate: "suspend_policy_never",
			Attrs: []any{
				"suspend_policy", string(row.SuspendPolicy),
			},
		}
	}
	if row.RunIntent != nil && *row.RunIntent != string(store.RunIntentRunning) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "run_intent_not_running",
			Attrs: []any{
				"run_intent", *row.RunIntent,
			},
		}
	}
	if row.RunID != cond.RunID {
		return &store.IdleSuspendPreconditionError{
			Predicate: "run_id_mismatch",
			Attrs: []any{
				"row_run_id", row.RunID,
				"cond_run_id", cond.RunID,
			},
		}
	}
	if row.Activity != cond.Activity || !state.Activity(row.Activity).IsSuspendEligible() {
		return &store.IdleSuspendPreconditionError{
			Predicate: "activity_changed_or_ineligible",
			Attrs: []any{
				"row_activity", row.Activity,
				"cond_activity", cond.Activity,
			},
		}
	}
	if row.ActivitySince == nil || !claimTime(*row.ActivitySince).Equal(claimTime(cond.ActivitySince)) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "activity_since_changed",
			Attrs: []any{
				"row_activity_since", optTimeAttr(row.ActivitySince),
				"cond_activity_since", cond.ActivitySince.UTC(),
			},
		}
	}
	if cond.IdleSuspendAfter > 0 && !row.ActivitySince.Before(now.Add(-cond.IdleSuspendAfter)) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "idle_grace_not_elapsed",
			Attrs: []any{
				"row_activity_since", row.ActivitySince.UTC(),
				"idle_suspend_after", cond.IdleSuspendAfter,
				"store_now", now.UTC(),
			},
		}
	}
	if row.LastSeen == nil {
		return &store.IdleSuspendPreconditionError{Predicate: "last_seen_nil"}
	}
	if cond.HeartbeatRecency > 0 && row.LastSeen.Before(now.Add(-cond.HeartbeatRecency)) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "heartbeat_stale",
			Attrs: []any{
				"last_seen", row.LastSeen.UTC(),
				"heartbeat_recency", cond.HeartbeatRecency,
				"store_now", now.UTC(),
			},
		}
	}
	if (&store.Agent{LastLiveProcessAt: row.LastLiveProcessAt}).HasRecentLiveWork(now) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "recent_live_work",
			Attrs: []any{
				"last_live_process_at", optTimeAttr(row.LastLiveProcessAt),
				"live_work_freshness", store.LiveWorkFreshness,
				"store_now", now.UTC(),
			},
		}
	}
	if row.LastDeliveryAt != nil && !claimTime(*row.LastDeliveryAt).Before(claimTime(*row.ActivitySince)) {
		return &store.IdleSuspendPreconditionError{
			Predicate: "delivery_since_activity",
			Attrs: []any{
				"last_delivery_at", row.LastDeliveryAt.UTC(),
				"row_activity_since", row.ActivitySince.UTC(),
				"store_now", now.UTC(),
			},
		}
	}
	return nil
}

// checkIdleSuspendCommit evaluates AgentStatusUpdate.IfIdleSuspendCommit on the
// locked row at store-clock now after ClaimIdleSuspend committed and
// DispatchAgentStop succeeded. It requires this exact unexpired claim lease to
// still be held at now (any user or hub-authored lifecycle write — stop,
// stop-all, suspend, restart, hold, start, reincarnation, or delete — clears
// idle_suspend_claim_until in the same write) and run_intent to remain stopped
// (never stop-queued). While that claim is held, running, stopping, stopped,
// and error (including non-zero exits or forced post-stop broker heartbeats on
// another replica) are all container/runtime outcomes of our own stop dispatch
// and are accepted.
func checkIdleSuspendCommit(row *ent.Agent, claimUntil, now time.Time) bool {
	return checkIdleSuspendRecord(row, claimUntil, now, false)
}

// checkIdleSuspendRecover evaluates AgentStatusUpdate.IfIdleSuspendRecover on
// the locked row at store-clock now: the same row conditions as
// checkIdleSuspendCommit, but the claim lease must have expired. A surviving
// stamp means no user or hub lifecycle write has touched the row since the
// claim (each clears it), so the stop is the idle suspend's own.
func checkIdleSuspendRecover(row *ent.Agent, claimUntil, now time.Time) bool {
	return checkIdleSuspendRecord(row, claimUntil, now, true)
}

func checkIdleSuspendRecord(row *ent.Agent, claimUntil, now time.Time, expired bool) bool {
	if row == nil {
		return false
	}
	if row.DeletedAt != nil || store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, now) {
		return false
	}
	if !claimEligible(row) || heldClaimError(row) != nil {
		return false
	}
	if row.IdleSuspendClaimUntil == nil || !claimTime(*row.IdleSuspendClaimUntil).Equal(claimTime(claimUntil)) {
		return false
	}
	if expired == now.Before(*row.IdleSuspendClaimUntil) {
		return false
	}
	if row.RunIntent == nil || *row.RunIntent != string(store.RunIntentStopped) {
		return false
	}
	if !entAgentToStore(row).IdleSuspendIntentIsOwn() {
		return false // a newer system stop intent (hold, stalled): not the claim's
	}
	if row.ContainerStatus == store.ContainerStatusStopQueued {
		return false
	}
	switch state.Phase(row.Phase) {
	case state.PhaseRunning, state.PhaseStopping, state.PhaseStopped, state.PhaseError:
		// Recovery additionally never converts a row that is still running:
		// its stop may not have landed.
		return !expired || state.Phase(row.Phase) != state.PhaseRunning
	default:
		return false
	}
}

// populateIdleSuspendClaimActive sets sa.IdleSuspendClaimActive against the
// store clock when sa.IdleSuspendClaimUntil is non-nil. Rows without a claim
// timestamp incur zero additional database queries.
func (s *AgentStore) populateIdleSuspendClaimActive(ctx context.Context, sa *store.Agent) error {
	if sa == nil || sa.IdleSuspendClaimUntil == nil {
		return nil
	}
	now, err := s.driverStoreNow(ctx)
	if err != nil {
		return err
	}
	sa.SetIdleSuspendFlags(now, s.idleSuspendExtended())
	return nil
}

func (s *AgentStore) driverStoreNow(ctx context.Context) (time.Time, error) {
	if s.dialect() != dialect.Postgres {
		return time.Now(), nil
	}
	var rows entsql.Rows
	if err := s.client.Driver().Query(ctx, "SELECT now()", []any{}, &rows); err != nil {
		return time.Time{}, fmt.Errorf("agent store: SELECT now(): %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return time.Time{}, fmt.Errorf("agent store: SELECT now(): %w", err)
		}
		return time.Time{}, fmt.Errorf("agent store: SELECT now(): no row")
	}
	var now time.Time
	if err := rows.Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("agent store: scan now(): %w", err)
	}
	return now, nil
}

// ClaimIdleSuspend implements store.AgentStore.ClaimIdleSuspend.
func (s *AgentStore) ClaimIdleSuspend(ctx context.Context, agentID string, cond store.IdleSuspendCondition) (store.RunIntent, time.Time, time.Time, error) {
	if cond.Activity == "" || cond.ActivitySince.IsZero() {
		return "", time.Time{}, time.Time{}, fmt.Errorf("%w: incomplete idle-suspend condition", store.ErrInvalidInput)
	}
	ttl := cond.ClaimTTL
	if ttl <= 0 {
		ttl = defaultIdleSuspendClaimTTL
	}

	var (
		prior      store.RunIntent
		at         time.Time
		claimUntil time.Time
	)
	err := s.withIdleSuspendLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		prior = ""
		at = time.Time{}
		claimUntil = time.Time{}
		if preErr := checkIdleSuspendRow(row, cond, now); preErr != nil {
			return false, preErr
		}
		if row.RunIntent != nil {
			prior = store.RunIntent(*row.RunIntent)
		}
		at = nextRunIntentAt(now, row.RunIntentAt)
		claimUntil = now.Add(ttl)
		if _, err := c.Agent.UpdateOneID(row.ID).
			SetUpdated(row.Updated).
			SetRunIntent(string(store.RunIntentStopped)).
			SetRunIntentAt(at).
			SetRunIntentMarkedAt(at).
			SetIdleSuspendClaimUntil(claimUntil).
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
	if err != nil {
		return "", time.Time{}, time.Time{}, err
	}
	return prior, at, claimUntil, nil
}

// ClearIdleSuspendClaim implements store.AgentStore.ClearIdleSuspendClaim.
func (s *AgentStore) ClearIdleSuspendClaim(ctx context.Context, agentID string) error {
	return s.withIdleSuspendLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, _ time.Time) (bool, error) {
		if row.IdleSuspendClaimUntil == nil {
			return false, nil
		}
		if _, err := c.Agent.UpdateOneID(row.ID).
			SetUpdated(row.Updated).
			ClearIdleSuspendClaimUntil().
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
}

// MarkAgentDelivery implements store.AgentStore.MarkAgentDelivery.
func (s *AgentStore) MarkAgentDelivery(ctx context.Context, agentID string) (*store.Agent, bool, error) {
	var (
		out         *store.Agent
		claimActive bool
	)
	err := s.withIdleSuspendLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		out = nil
		// claimActive is "an idle suspend is in progress" (store.Agent
		// SetIdleSuspendFlags): an unexpired lease, or a surviving stamp with
		// run_intent=stopped on a row not yet recorded suspended.
		probe := entAgentToStore(row)
		probe.SetIdleSuspendFlags(now, s.idleSuspendExtended())
		claimActive = probe.IdleSuspendClaimActive
		if !claimActive && (row.Phase == string(state.PhaseRunning) || row.Phase == string(state.PhaseStarting)) {
			updated, err := c.Agent.UpdateOneID(row.ID).
				SetUpdated(row.Updated).
				SetLastDeliveryAt(now).
				Save(ctx)
			if err != nil {
				return false, mapError(err)
			}
			out = entAgentToStore(updated)
			out.IdleSuspendClaimActive = false
			return true, nil
		}
		out = probe
		return false, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, claimActive, nil
}

// ListExpiredIdleSuspendClaims implements store.AgentStore.ListExpiredIdleSuspendClaims.
func (s *AgentStore) ListExpiredIdleSuspendClaims(ctx context.Context, limit int) ([]store.Agent, error) {
	now, err := s.driverStoreNow(ctx)
	if err != nil {
		return nil, err
	}
	q := s.client.Agent.Query().Where(
		agent.DeletedAtIsNil(),
		agent.IdleSuspendClaimUntilNotNil(),
		agent.IdleSuspendClaimUntilLTE(claimTime(now)),
		agent.RunIntentEQ(string(store.RunIntentStopped)),
		agent.PhaseIn(string(state.PhaseStopping), string(state.PhaseStopped), string(state.PhaseError)),
		ownIdleIntentPredicate(),
	).Order(ent.Asc(agent.FieldIdleSuspendClaimUntil), ent.Asc(agent.FieldID))
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.Agent, len(rows))
	for i, a := range rows {
		out[i] = *entAgentToStore(a)
	}
	return out, nil
}

// ListAbandonedIdleSuspendClaims implements store.AgentStore.ListAbandonedIdleSuspendClaims.
func (s *AgentStore) ListAbandonedIdleSuspendClaims(ctx context.Context, expiredFor time.Duration, limit int) ([]store.Agent, error) {
	now, err := s.driverStoreNow(ctx)
	if err != nil {
		return nil, err
	}
	q := s.client.Agent.Query().Where(
		agent.DeletedAtIsNil(),
		agent.IdleSuspendClaimUntilNotNil(),
		agent.IdleSuspendClaimUntilLTE(claimTime(now.Add(-expiredFor))),
		agent.RunIntentEQ(string(store.RunIntentStopped)),
		agent.PhaseEQ(string(state.PhaseRunning)),
		ownIdleIntentPredicate(),
		// A heartbeat after the lease expired proves the container is alive.
		heartbeatAfterIdleClaimPredicate(),
	).Order(ent.Asc(agent.FieldIdleSuspendClaimUntil), ent.Asc(agent.FieldID))
	if limit > 0 {
		q = q.Limit(limit)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.Agent, len(rows))
	for i, a := range rows {
		out[i] = *entAgentToStore(a)
	}
	return out, nil
}

// AbandonIdleSuspend implements store.AgentStore.AbandonIdleSuspend.
func (s *AgentStore) AbandonIdleSuspend(ctx context.Context, agentID string, claimUntil time.Time, runID string, intentAt time.Time, expiredFor time.Duration, message string) error {
	return s.withIdleSuspendLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if row.DeletedAt != nil || store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, now) ||
			!claimEligible(row) || heldClaimError(row) != nil {
			return false, store.ErrIdleSuspendPrecondition
		}
		if row.IdleSuspendClaimUntil == nil || !claimTime(*row.IdleSuspendClaimUntil).Equal(claimTime(claimUntil)) ||
			now.Before(row.IdleSuspendClaimUntil.Add(expiredFor)) {
			return false, store.ErrIdleSuspendPrecondition
		}
		if row.RunID != runID || row.Phase != string(state.PhaseRunning) ||
			!entAgentToStore(row).RunIntentMatches(store.RunIntentStopped, intentAt) ||
			!entAgentToStore(row).IdleSuspendIntentIsOwn() {
			return false, store.ErrIdleSuspendPrecondition
		}
		// A heartbeat after the lease expired proves the container is still
		// alive: phase=running alone may be a stop whose report lags, which must
		// not be reverted.
		if row.LastSeen == nil || !row.LastSeen.After(*row.IdleSuspendClaimUntil) {
			return false, store.ErrIdleSuspendPrecondition
		}
		if _, err := c.Agent.UpdateOneID(row.ID).
			SetUpdated(row.Updated).
			SetRunIntent(string(store.RunIntentRunning)).
			SetMessage(message).
			Save(ctx); err != nil {
			return false, mapError(err)
		}
		return true, nil
	})
}

// ownIdleIntentPredicate is the SQL form of store.Agent.IdleSuspendIntentIsOwn:
// run_intent_at <= idle_suspend_claim_until (the claim writes both in one
// write; a newer system stop intent is later than the stamp). Applied in the
// query so a limit cannot starve own-intent rows behind foreign ones.
func ownIdleIntentPredicate() predicate.Agent {
	return predicate.Agent(func(sel *entsql.Selector) {
		sel.Where(entsql.And(
			entsql.NotNull(sel.C(agent.FieldRunIntentAt)),
			entsql.ColumnsLTE(sel.C(agent.FieldRunIntentAt), sel.C(agent.FieldIdleSuspendClaimUntil)),
		))
	})
}

// heartbeatAfterIdleClaimPredicate selects rows with last_seen after the claim
// stamp (a heartbeat since the lease expired).
func heartbeatAfterIdleClaimPredicate() predicate.Agent {
	return predicate.Agent(func(sel *entsql.Selector) {
		sel.Where(entsql.And(
			entsql.NotNull(sel.C(agent.FieldLastSeen)),
			entsql.ColumnsGT(sel.C(agent.FieldLastSeen), sel.C(agent.FieldIdleSuspendClaimUntil)),
		))
	})
}
