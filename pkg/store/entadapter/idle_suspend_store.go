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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// defaultIdleSuspendClaimTTL bounds how long an un-cleared idle-suspend claim
// blocks delivery/auto-wake if a hub replica crashes mid-stop.
const defaultIdleSuspendClaimTTL = 45 * time.Second

// checkIdleSuspendRow evaluates cond against a locked ent.Agent row at
// store-clock now for ClaimIdleSuspend. An already-active
// idle_suspend_claim_until rejects so two sweeps never double-claim.
func checkIdleSuspendRow(row *ent.Agent, cond store.IdleSuspendCondition, now time.Time) bool {
	if row == nil {
		return false
	}
	if row.DeletedAt != nil || store.DeletionHoldsRow(row.DeletionState, row.DeletionLeaseAt, now) {
		return false
	}
	if !claimEligible(row) || heldClaimError(row) != nil {
		return false
	}
	if row.IdleSuspendClaimUntil != nil && now.Before(*row.IdleSuspendClaimUntil) {
		return false
	}
	if row.Phase != string(state.PhaseRunning) {
		return false
	}
	if row.RunID != cond.RunID {
		return false
	}
	if row.Activity != cond.Activity || !state.Activity(row.Activity).IsSuspendEligible() {
		return false
	}
	if row.ActivitySince == nil || !claimTime(*row.ActivitySince).Equal(claimTime(cond.ActivitySince)) {
		return false
	}
	if cond.IdleSuspendAfter > 0 && !row.ActivitySince.Before(now.Add(-cond.IdleSuspendAfter)) {
		return false
	}
	if row.LastSeen == nil {
		return false
	}
	if cond.HeartbeatRecency > 0 && row.LastSeen.Before(now.Add(-cond.HeartbeatRecency)) {
		return false
	}
	if (&store.Agent{LastLiveProcessAt: row.LastLiveProcessAt}).HasRecentLiveWork(now) {
		return false
	}
	if row.LastDeliveryAt != nil && !claimTime(*row.LastDeliveryAt).Before(claimTime(*row.ActivitySince)) {
		return false
	}
	return true
}

// checkIdleSuspendCommit evaluates AgentStatusUpdate.IfIdleSuspendCommit on the
// locked row at store-clock now after ClaimIdleSuspend committed and
// DispatchAgentStop succeeded. Phase may be running, stopping or stopped
// (because the container reports its own shutdown before DispatchAgentStop
// returns), and activity, live-work, or delivery timestamps are not re-checked.
func checkIdleSuspendCommit(row *ent.Agent, claimUntil, now time.Time) bool {
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
	switch state.Phase(row.Phase) {
	case state.PhaseRunning, state.PhaseStopping, state.PhaseStopped:
		return true
	default:
		return false
	}
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
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		if !checkIdleSuspendRow(row, cond, now) {
			return false, store.ErrIdleSuspendPrecondition
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
	return s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, _ time.Time) (bool, error) {
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
	err := s.withLockedAgent(ctx, agentID, func(ctx context.Context, c *ent.Client, row *ent.Agent, now time.Time) (bool, error) {
		claimActive = row.IdleSuspendClaimUntil != nil && now.Before(*row.IdleSuspendClaimUntil)
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
		out = entAgentToStore(row)
		out.IdleSuspendClaimActive = claimActive
		return false, nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, claimActive, nil
}
