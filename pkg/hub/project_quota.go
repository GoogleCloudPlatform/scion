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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The per-project agent limit (max_agents_per_project) follows the same
// lifecycle as the per-broker one (ptone/scion#6033): an agent holds its
// project reservation only while it holds capacity
// (store.AgentHoldsBrokerCapacity: a counted phase, or a stop queued for an
// offline broker). Stop, suspend and an observed exit release it; every
// start-type admission (start, restart, resume, DM wake, reincarnate)
// reserves it again with the cap check. The cap compared against the
// project's shared total is the one resolved for the agent's creator
// (agent.CreatedBy) on every path, so it does not depend on who starts the
// agent.
//
// The helpers below sit next to the broker helpers in broker_quota.go and
// are called beside them; they never run inside them. Unlike the broker
// helpers they do not return early for an agent with no Runtime Broker:
// hub-direct agents hold a project reservation too.

// auditQuotaProjectBackstopReassert is the mutation audit type of a project
// backstop re-assert that wrote a reservation.
const auditQuotaProjectBackstopReassert = "quota_project_backstop_reassert"

// projectBackfillPageSize is the page size of the project backfill's agent
// list. A variable for tests.
var projectBackfillPageSize = brokerScopedListLimit

// checkAndReserveProjectQuota reserves agent's max_agents_per_project slot
// with the cap check, the subject being the agent's creator. It returns nil
// if the reservation succeeded, was already held, or no limit is defined;
// store.ErrQuotaExceeded (a *QuotaExceededError) or ErrQuotaLockContention
// otherwise. created reports whether this call made a new reservation; pass
// it to rollbackProjectQuota if the admission then fails.
func (s *Server) checkAndReserveProjectQuota(ctx context.Context, agent *store.Agent) (created bool, err error) {
	if s.quotaService == nil || agent == nil || agent.ProjectID == "" {
		return false, nil
	}
	return s.quotaService.Reserve(ctx, store.LimitMaxAgentsPerProject, agent.CreatedBy, store.QuotaScopeProject, agent.ProjectID, agent.ID)
}

// releaseProjectQuota releases agent's max_agents_per_project reservation,
// if any. Best-effort; a no-op when the agent holds none.
func (s *Server) releaseProjectQuota(ctx context.Context, agent *store.Agent) {
	if s.quotaService == nil || agent == nil {
		return
	}
	s.quotaService.Release(ctx, store.LimitMaxAgentsPerProject, agent.ID)
}

// rollbackProjectQuota undoes a checkAndReserveProjectQuota reservation
// after a failed admission, but only if that call created it.
func (s *Server) rollbackProjectQuota(ctx context.Context, agent *store.Agent, created bool) {
	if !created {
		return
	}
	s.releaseProjectQuota(ctx, agent)
}

// releaseProjectQuotaIf releases agent's max_agents_per_project reservation
// only if cond holds on the stored agent row, in one conditional store write
// (store.ReleaseReservationIfAgent), as releaseBrokerQuotaIf does for the
// broker limit. released reports whether a reservation was released; err is
// a store failure (logged here).
func (s *Server) releaseProjectQuotaIf(ctx context.Context, agent *store.Agent, cond store.AgentReleaseCond) (released bool, err error) {
	if s.quotaService == nil || agent == nil {
		return false, nil
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		s.agentLifecycleLog.Warn("quota: conditional project release: limit lookup failed",
			"agent_id", agent.ID, "error", err)
		return false, err
	}
	released, err = s.store.ReleaseReservationIfAgent(ctx, def.ID, agent.ID, cond)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		s.agentLifecycleLog.Warn("quota: conditional project release failed",
			"agent_id", agent.ID, "error", err)
		return false, err
	}
	return released, nil
}

// releaseProjectSlotIfIdle releases agentID's active max_agents_per_project
// reservation if the agent, as stored at the write, holds no capacity and no
// start-kind claim, keyed on the run ID and the reservation it reads here. It
// is called beside each releaseSlotIfIdle / releaseSlotIfIdleOn call site
// (the launch report, the reaper, claim finish) and after a reincarnation
// failure that left the agent idle. It reads the agent row itself.
// Best-effort.
func (s *Server) releaseProjectSlotIfIdle(ctx context.Context, agentID string) {
	row := s.readAgentForIdleRelease(ctx, agentID)
	if row == nil {
		return
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: idle project release: limit lookup failed",
				"agent_id", row.ID, "error", err)
		}
		return
	}
	res, err := s.store.GetActiveReservation(ctx, def.ID, row.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: idle project release: reading the reservation failed",
				"agent_id", row.ID, "error", err)
		}
		return
	}
	if res.ScopeType != store.QuotaScopeProject || res.ScopeID != row.ProjectID {
		return
	}
	released, err := s.store.ReleaseReservationIfAgent(ctx, def.ID, row.ID, store.AgentReleaseCond{
		ExpectedRunID:    row.RunID,
		NoStartKindClaim: true,
		NotCounted:       true,
		ReservationID:    res.ID,
	})
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: idle project release failed",
				"agent_id", row.ID, "reservation_id", res.ID, "error", err)
		}
		return
	}
	if released {
		s.agentLifecycleLog.Info("quota: released the project slot of an idle agent",
			"agent_id", row.ID, "project_id", row.ProjectID)
	}
}

// releaseOwnedReservationIfIdle releases the active reservation
// reservationID of limitName held by agentID, only if the stored agent row
// holds no capacity and no start-kind claim, keyed on the run ID read here
// (releaseReservationIfIdleAt). It is the release of a row a reincarnation
// created itself; an empty reservationID is a no-op. Best-effort.
func (s *Server) releaseOwnedReservationIfIdle(ctx context.Context, limitName, agentID, reservationID string) {
	if reservationID == "" {
		return
	}
	row := s.readAgentForIdleRelease(ctx, agentID)
	if row == nil {
		return
	}
	s.releaseReservationIfIdleAt(ctx, limitName, agentID, row.RunID, reservationID)
}

// activeReservationID returns the ID of agentID's active reservation of
// limitName in (scopeType, scopeID), or "" when there is none, it is in
// another scope, or the read fails (logged). Used right after a reservation
// was created, to know which row a caller owns.
func (s *Server) activeReservationID(ctx context.Context, limitName, agentID, scopeType, scopeID string) string {
	def, err := s.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: reading the limit for a created reservation failed",
				"limit", limitName, "agent_id", agentID, "error", err)
		}
		return ""
	}
	res, err := s.store.GetActiveReservation(ctx, def.ID, agentID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: reading a created reservation failed",
				"limit", limitName, "agent_id", agentID, "error", err)
		}
		return ""
	}
	if res.ScopeType != scopeType || res.ScopeID != scopeID {
		return ""
	}
	return res.ID
}

// reassertProjectReservation records agent's max_agents_per_project
// reservation, without the cap check, if it holds none: a backstop beside
// the broker backstops (the restart's two re-asserts, the queued-stop settle
// and the reincarnation worker after its stopping write) for a release that
// slipped through while the agent was being started. Like the broker
// backstop it can leave the project one over its cap until a slot frees.
// Every write is logged and audited at site. It returns the ID of the row
// it created, or "" when it created none. Best-effort.
func (s *Server) reassertProjectReservation(ctx context.Context, agent *store.Agent, site string) string {
	if s.quotaService == nil || agent == nil || agent.ProjectID == "" {
		return ""
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: project backstop re-assert: limit lookup failed",
				"site", site, "agent_id", agent.ID, "error", err)
		}
		return ""
	}
	created, err := s.recordProjectReservationUnchecked(ctx, def.ID, agent)
	if err != nil {
		s.agentLifecycleLog.Warn("quota: project backstop re-assert failed",
			"site", site, "agent_id", agent.ID, "error", err)
		return ""
	}
	if !created {
		return ""
	}
	s.agentLifecycleLog.Warn("quota: backstop re-asserted a released project reservation without the cap check",
		"site", site, "agent_id", agent.ID, "project_id", agent.ProjectID)
	s.writeCapacityAudit(ctx, auditQuotaProjectBackstopReassert, agent.ID, capacityAuditSummary{
		Reason:          "project reservation re-asserted without the cap check",
		RuntimeBrokerID: agent.RuntimeBrokerID,
		Site:            site,
	})
	return s.activeReservationID(ctx, store.LimitMaxAgentsPerProject, agent.ID, store.QuotaScopeProject, agent.ProjectID)
}

// recordProjectReservationUnchecked records agent's project reservation
// (subject: its creator) with no cap check. created is false when the agent
// already holds an active reservation for the limit.
func (s *Server) recordProjectReservationUnchecked(ctx context.Context, limitDefID string, agent *store.Agent) (bool, error) {
	_, err := s.store.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: limitDefID,
		SubjectID:         agent.CreatedBy,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           agent.ProjectID,
		ResourceID:        agent.ID,
		Reserved:          1,
	})
	if err != nil {
		if errors.Is(err, store.ErrActiveReservationExists) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// reconcileProjectQuotaOnPhaseChange keeps agent's max_agents_per_project
// reservation in step with a phase change observed from the agent or its
// broker, as reconcileBrokerQuotaOnPhaseChange does for the broker limit, and
// is called beside it. Counted to not counted releases the reservation,
// conditional on the run read and on no start-kind claim (a start in flight
// keeps its slot), and is skipped while a lifecycle op runs on this replica.
// Not counted to counted re-reserves best-effort with the cap check: the
// container already runs, so a refusal is only logged, and the backfill
// records it.
func (s *Server) reconcileProjectQuotaOnPhaseChange(ctx context.Context, agent *store.Agent, oldPhase, newPhase string) {
	if newPhase == "" || newPhase == oldPhase {
		return
	}
	wasCounted := isBrokerQuotaCountedPhase(oldPhase)
	nowCounted := isBrokerQuotaCountedPhase(newPhase)
	if wasCounted == nowCounted {
		return
	}
	if nowCounted {
		if _, err := s.checkAndReserveProjectQuota(ctx, agent); err != nil {
			s.agentLifecycleLog.Warn("quota: best-effort project re-reserve on observed phase change failed",
				"agent_id", agent.ID, "old_phase", oldPhase, "new_phase", newPhase, "error", err)
		}
		return
	}
	if agent == nil || s.lifecycleOps.active(agent.ID) {
		return
	}
	_, _ = s.releaseProjectQuotaIf(ctx, agent, store.AgentReleaseCond{
		ExpectedRunID:    agent.RunID,
		NoStartKindClaim: true,
	})
}

// reconcileQuotaReservations is the scheduled quota reconcile: the broker
// pass, then the project pass.
func (s *Server) reconcileQuotaReservations(ctx context.Context) {
	s.ReconcileStaleBrokerQuotaReservations(ctx)
	s.ReconcileStaleProjectQuotaReservations(ctx)
}

// ReconcileStaleProjectQuotaReservations reconciles max_agents_per_project
// reservations against the agents, as ReconcileStaleBrokerQuotaReservations
// does for the broker limit (ptone/scion#6033):
//
//   - Release: an active reservation whose agent no longer exists or is
//     soft-deleted (by reservation ID, at any age), or whose agent holds no
//     capacity and that is at least reconcileMinReservationAge old (one
//     conditional write on the run read, no start-kind claim, no capacity
//     held, and that reservation).
//   - Backfill: every agent, hub-wide and paged to the end, that holds
//     capacity with no active project reservation gets one recorded without
//     the cap check (accounting for an agent that already runs, not an
//     admission). It covers agents admitted before the limit followed the
//     lifecycle and any release that slipped through.
//
// Best-effort throughout: failures are logged and skipped, and a failed
// agent page ends the backfill for this pass.
func (s *Server) ReconcileStaleProjectQuotaReservations(ctx context.Context) {
	if s.quotaService == nil {
		return
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("project quota reconcile: limit lookup failed", "error", err)
		}
		return
	}
	reservations, err := s.store.ListActiveReservationsByScopeType(ctx, def.ID, store.QuotaScopeProject)
	if err != nil {
		s.agentLifecycleLog.Warn("project quota reconcile: listing active reservations failed", "error", err)
		return
	}
	ids := make([]string, len(reservations))
	for i, res := range reservations {
		ids[i] = res.ResourceID
	}
	agentsByID, err := s.store.GetAgentsByIDs(ctx, ids)
	if err != nil {
		s.agentLifecycleLog.Warn("project quota reconcile: loading reserved agents failed", "error", err)
		return
	}
	held := make(map[string]bool, len(reservations))
	var released, backfilled int
	for _, res := range reservations {
		agent, ok := agentsByID[res.ResourceID]
		if !ok {
			if s.releaseObservedReservation(ctx, res) {
				released++
			}
			continue
		}
		if !agentHoldsBrokerCapacity(agent) && time.Since(res.CreatedAt) >= reconcileMinReservationAge {
			if s.releaseObservedIdleReservation(ctx, def.ID, agent, res) {
				released++
				continue
			}
		}
		if res.ScopeID != agent.ProjectID {
			s.agentLifecycleLog.Warn("project quota reconcile: reservation scope differs from the agent's project; left as it is",
				"agent_id", agent.ID, "reservation_project_id", res.ScopeID, "project_id", agent.ProjectID)
		}
		held[agent.ID] = true
	}

	cursor := ""
	for {
		page, err := s.store.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{
			Limit:          projectBackfillPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			s.agentLifecycleLog.Warn("project quota reconcile: listing agents for the backfill failed", "error", err)
			break
		}
		for i := range page.Items {
			agent := &page.Items[i]
			if held[agent.ID] || agent.ProjectID == "" || !agentHoldsBrokerCapacity(agent) {
				continue
			}
			created, err := s.recordProjectReservationUnchecked(ctx, def.ID, agent)
			if err != nil {
				s.agentLifecycleLog.Warn("project quota reconcile: backfill failed",
					"agent_id", agent.ID, "error", err)
				continue
			}
			if created {
				backfilled++
			}
		}
		if page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextCursor
	}

	if released > 0 || backfilled > 0 {
		s.agentLifecycleLog.Info("project quota reconcile: reconciled max_agents_per_project reservations",
			"checked", len(reservations), "released", released, "backfilled", backfilled)
	}
}
