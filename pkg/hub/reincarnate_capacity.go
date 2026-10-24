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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A reincarnation starts the agent, also one that was stopped, so it is an
// admission like any start (ptone/scion#6033). The handler reserves the
// agent's project slot, and for a reincarnation that stays on its broker its
// broker slot, with the cap check before the reincarnation is claimed. A
// move's target broker slot is checked by the worker when the reservation
// moves (moveBrokerQuota).

// reincarnationCapacity records the reservations a reincarnation created
// itself, by ID ("" when it created none), so that only those are released
// when it fails before its agent could hold capacity.
type reincarnationCapacity struct {
	brokerReservationID  string
	projectReservationID string
}

// owned returns a copy of c, the zero value for a nil c.
func (c *reincarnationCapacity) owned() reincarnationCapacity {
	if c == nil {
		return reincarnationCapacity{}
	}
	return *c
}

// reserveReincarnationCapacity reserves agent's broker slot (only when move
// is nil) and its project slot, each with the cap check and idempotent for a
// slot the agent already holds. On a refusal it releases a broker slot this
// call created and returns the error (store.ErrQuotaExceeded,
// ErrQuotaLockContention, a *ReservationScopeConflictError, ...).
func (s *Server) reserveReincarnationCapacity(ctx context.Context, agent *store.Agent, move *reincarnationMove) (*reincarnationCapacity, error) {
	c := &reincarnationCapacity{}
	brokerCreated := false
	if move == nil {
		created, err := s.checkAndReserveBrokerQuota(ctx, agent)
		if err != nil {
			return nil, err
		}
		if created {
			brokerCreated = true
			c.brokerReservationID = s.activeReservationID(ctx, store.LimitMaxAgentsPerBroker, agent.ID, store.QuotaScopeBroker, agent.RuntimeBrokerID)
		}
	}
	created, err := s.checkAndReserveProjectQuota(ctx, agent)
	if err != nil {
		rctx, cancel := detachedCleanupContext(ctx, quotaReleaseTimeout)
		s.rollbackBrokerQuota(rctx, agent, brokerCreated)
		cancel()
		return nil, err
	}
	if created {
		c.projectReservationID = s.activeReservationID(ctx, store.LimitMaxAgentsPerProject, agent.ID, store.QuotaScopeProject, agent.ProjectID)
	}
	return c, nil
}

// rollbackReincarnationCapacity releases the reservations c records after
// the reincarnation could not be claimed. Each release is conditional on the
// agent row as the handler read it (agent.RunID), no start-kind claim, the
// agent holding no capacity, and that reservation: when the claim failed
// because a start holds the agent, or because a concurrent reincarnation won
// and has since begun, the row stays with them.
func (s *Server) rollbackReincarnationCapacity(ctx context.Context, agent *store.Agent, c *reincarnationCapacity) {
	if c == nil || agent == nil {
		return
	}
	ctx, cancel := detachedCleanupContext(ctx, quotaReleaseTimeout)
	defer cancel()
	s.releaseReservationIfIdleAt(ctx, store.LimitMaxAgentsPerBroker, agent.ID, agent.RunID, c.brokerReservationID)
	s.releaseReservationIfIdleAt(ctx, store.LimitMaxAgentsPerProject, agent.ID, agent.RunID, c.projectReservationID)
}

// releaseReservationIfIdleAt releases agentID's active reservation
// reservationID of limitName if the stored agent row has run runID, no
// start-kind claim and holds no capacity, in one conditional store write. An
// empty reservationID is a no-op. Best-effort.
func (s *Server) releaseReservationIfIdleAt(ctx context.Context, limitName, agentID, runID, reservationID string) {
	if reservationID == "" || s.quotaService == nil {
		return
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: reincarnation capacity release: limit lookup failed",
				"limit", limitName, "agent_id", agentID, "error", err)
		}
		return
	}
	if _, err := s.store.ReleaseReservationIfAgent(ctx, def.ID, agentID, store.AgentReleaseCond{
		ExpectedRunID:    runID,
		NoStartKindClaim: true,
		NotCounted:       true,
		ReservationID:    reservationID,
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.agentLifecycleLog.Warn("quota: reincarnation capacity release failed",
			"limit", limitName, "agent_id", agentID, "reservation_id", reservationID, "error", err)
	}
}

// releaseFailedReincarnationCapacity runs when a reincarnation failed after
// its stop step succeeded and no container can exist (any failure other than
// an ambiguous start), once the failure is recorded (phase error). A move
// releases the agent's idle broker and project slots: the move's quota
// handling may have restored a source broker slot the agent did not hold
// before. A reincarnation that stays on its broker releases only the rows
// it created (owned). Every release is conditional on the agent, as read at
// release time, holding no capacity and no start-kind claim, so a start
// that claimed it since keeps them.
func (s *Server) releaseFailedReincarnationCapacity(ctx context.Context, agentID string, isMove bool, owned reincarnationCapacity) {
	ctx, cancel := detachedCleanupContext(ctx, quotaReleaseTimeout)
	defer cancel()
	if isMove {
		s.releaseSlotIfIdle(ctx, agentID)
		s.releaseProjectSlotIfIdle(ctx, agentID)
		return
	}
	s.releaseOwnedReservationIfIdle(ctx, store.LimitMaxAgentsPerBroker, agentID, owned.brokerReservationID)
	s.releaseOwnedReservationIfIdle(ctx, store.LimitMaxAgentsPerProject, agentID, owned.projectReservationID)
}
