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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reconcileMinReservationAge is the minimum age a reservation must reach
// before ReconcileStaleBrokerQuotaReservations will release it on the basis
// of the agent's stored phase. Start (HTTP) and DM wake both reserve the
// broker slot before dispatch but only write the counted phase (e.g.
// "starting") after dispatch returns, so a reconcile pass — the periodic
// tick or a startup sweep — that lands in that window would otherwise see an
// uncounted stored phase and release a reservation for a dispatch that is
// still in flight (ptone/scion#2011). The threshold must clear the
// worst-case dispatch latency, including a cold image pull, with margin;
// 15 minutes is chosen for that reason and is not meant to be tuned per
// deployment. This grace period does not apply to reservations whose agent
// is missing or soft-deleted — those are always released regardless of age.
const reconcileMinReservationAge = 15 * time.Minute

// isBrokerQuotaCountedPhase reports whether phase currently counts toward an
// agent's runtime broker's max_agents_per_broker ceiling (ptone/scion#1963).
// Only stopped, suspended, and error are excluded: a container in any other
// phase (created/provisioning/cloning/starting/running) either occupies, or
// is actively moving toward occupying, broker capacity. Stopped and suspended
// agents have no running container; error means the container already
// exited or crashed.
func isBrokerQuotaCountedPhase(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseSuspended, state.PhaseError:
		return false
	default:
		return true
	}
}

// releaseBrokerQuota releases agent's max_agents_per_broker reservation, if
// any. Best-effort and safe to call unconditionally (e.g. on every stop or
// suspend) — a no-op when the agent has no runtime broker assigned, and
// QuotaService.Release itself is a no-op when no reservation exists.
func (s *Server) releaseBrokerQuota(ctx context.Context, agent *store.Agent) {
	if s.quotaService == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return
	}
	s.quotaService.Release(ctx, store.LimitMaxAgentsPerBroker, agent.ID)
}

// rollbackBrokerQuota undoes a checkAndReserveBrokerQuota(HTTP) reservation
// after a failed dispatch, but only if that call created it
// (ptone/scion#1978). When the reservation already existed (for example,
// start called on an agent that is already running), the agent is still
// counted and releasing it would let the broker exceed its cap.
func (s *Server) rollbackBrokerQuota(ctx context.Context, agent *store.Agent, created bool) {
	if !created {
		return
	}
	s.releaseBrokerQuota(ctx, agent)
}

// checkAndReserveBrokerQuotaHTTP re-reserves agent's max_agents_per_broker
// slot before an HTTP-triggered start/resume/restart dispatch, using the same
// helper — and therefore the same error/status shape — createAgentInProject
// uses. Returns true if the caller may proceed with dispatch; on false it has
// already written the error response to w.
//
// Safe to call unconditionally regardless of the agent's current phase:
// QuotaService.CheckAndReserve is idempotent per resource, so calling this on
// an agent that already holds an active reservation (e.g. a fresh create, or
// "start" called again on an already-running agent) is a no-op rather than a
// duplicate reservation. created reports whether this call made a new
// reservation; pass it to rollbackBrokerQuota if dispatch then fails.
func (s *Server) checkAndReserveBrokerQuotaHTTP(ctx context.Context, w http.ResponseWriter, agent *store.Agent) (ok, created bool) {
	if agent == nil || agent.RuntimeBrokerID == "" {
		return true, false
	}
	return s.reserveQuotaHTTP(ctx, w, store.LimitMaxAgentsPerBroker, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.ID)
}

// checkAndReserveBrokerQuota is the non-HTTP counterpart of
// checkAndReserveBrokerQuotaHTTP, for paths that cannot write an HTTP
// response directly (e.g. agent DM wake). Returns nil if the reservation
// succeeded, was already held (idempotent), or no limit is configured;
// returns store.ErrQuotaExceeded or ErrQuotaLockContention otherwise.
// created reports whether this call made a new reservation; pass it to
// rollbackBrokerQuota if dispatch then fails.
func (s *Server) checkAndReserveBrokerQuota(ctx context.Context, agent *store.Agent) (created bool, err error) {
	if s.quotaService == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return false, nil
	}
	return s.quotaService.Reserve(ctx, store.LimitMaxAgentsPerBroker, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.ID)
}

// reconcileBrokerQuotaOnPhaseChange updates agent's max_agents_per_broker
// reservation to match an *observed* phase transition (heartbeat or agent
// self-report status update) rather than an explicit lifecycle action. It is
// the counterpart, for the "hub observes reality" paths, of the explicit
// release/reserve calls in the lifecycle handlers (ptone/scion#1963).
//
//   - counted → not counted (e.g. the broker reports the container exited or
//     crashed): release.
//   - not counted → counted (e.g. an operator or the runtime restarted the
//     container out of band, and the hub only learns about it via heartbeat):
//     best-effort re-reserve. The container is already running by the time
//     the hub observes this, so exceeding the cap cannot be prevented here —
//     only accounted for. Failure is logged, not propagated: the status
//     update itself must still succeed.
//
// No-ops when oldPhase and newPhase agree on countedness (the overwhelmingly
// common case — most heartbeats/status updates don't cross a counted
// boundary), or when newPhase is empty (no phase change in this update).
func (s *Server) reconcileBrokerQuotaOnPhaseChange(ctx context.Context, agent *store.Agent, oldPhase, newPhase string) {
	if newPhase == "" || newPhase == oldPhase {
		return
	}
	wasCounted := isBrokerQuotaCountedPhase(oldPhase)
	nowCounted := isBrokerQuotaCountedPhase(newPhase)
	if wasCounted == nowCounted {
		return
	}
	if nowCounted {
		if _, err := s.checkAndReserveBrokerQuota(ctx, agent); err != nil {
			s.agentLifecycleLog.Warn("quota: best-effort re-reserve on observed phase change failed",
				"agent_id", agent.ID, "old_phase", oldPhase, "new_phase", newPhase, "error", err)
		}
		return
	}
	s.releaseBrokerQuota(ctx, agent)
}

// ReconcileStaleBrokerQuotaReservations reconciles max_agents_per_broker
// reservations against observed reality, per runtime broker (ptone/scion#1963):
//
//   - Release: an active reservation (released_at IS NULL) whose agent no
//     longer exists, is soft-deleted, or is no longer in a counted phase —
//     fixing rows left behind from before stop/suspend/crash released the
//     reservation, or from a delete path that missed the release call. The
//     phase-based case is skipped for a reservation younger than
//     reconcileMinReservationAge, since dispatch reserves before it writes
//     the counted phase (ptone/scion#2011); the missing/soft-deleted case is
//     never subject to that grace period.
//   - Backfill: an agent in a counted phase on this broker with no active
//     reservation gets one recorded directly (no cap check — this is
//     accounting for an agent that already exists and is already running,
//     not a new admission decision; see checkAndReserveBrokerQuota for the
//     cap-enforced path used at start/resume). Covers legacy agents created
//     before max_agents_per_broker existed, which hold no reservation at all.
//
// Idempotent both ways: releasing an already-released reservation and
// re-running the backfill after a prior successful pass are no-ops (the
// latter enforced by the DB's unique-active-reservation-per-resource index,
// not just the in-memory reservedIDs check below). Safe to call on every hub
// startup. Best-effort throughout — this is bookkeeping, not on any
// request's critical path, so individual failures are logged and skipped
// rather than aborting the whole pass.
func (s *Server) ReconcileStaleBrokerQuotaReservations(ctx context.Context) {
	if s.quotaService == nil {
		return
	}
	limitDef, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota reconcile: failed to look up max_agents_per_broker limit definition", "error", err)
		}
		return
	}

	brokers, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		s.agentLifecycleLog.Warn("quota reconcile: failed to list runtime brokers", "error", err)
		return
	}

	var checked, released, backfilled int
	for _, broker := range brokers.Items {
		reservations, err := s.store.ListActiveReservations(ctx, limitDef.ID, store.QuotaScopeBroker, broker.ID)
		if err != nil {
			s.agentLifecycleLog.Warn("quota reconcile: failed to list active reservations",
				"broker_id", broker.ID, "error", err)
			continue
		}

		// Batch-fetch every reserved agent in one query instead of one
		// GetAgent per reservation (N+1). GetAgentsByIDs excludes
		// soft-deleted agents, so a soft-deleted agent's reservation is
		// released via the same "missing from the map" path as a
		// hard-deleted one — its stored phase (which may still read e.g.
		// "running") never enters into it.
		resourceIDs := make([]string, len(reservations))
		reservedIDs := make(map[string]bool, len(reservations))
		for i, res := range reservations {
			resourceIDs[i] = res.ResourceID
			reservedIDs[res.ResourceID] = true
		}
		agentsByID, err := s.store.GetAgentsByIDs(ctx, resourceIDs)
		if err != nil {
			s.agentLifecycleLog.Warn("quota reconcile: failed to batch-fetch reserved agents",
				"broker_id", broker.ID, "error", err)
			continue
		}

		checked += len(reservations)
		for _, res := range reservations {
			agent, ok := agentsByID[res.ResourceID]
			if !ok {
				// No longer exists or soft-deleted: release the stale row.
				// It should have been released at delete time; release it
				// now regardless.
				s.quotaService.Release(ctx, store.LimitMaxAgentsPerBroker, res.ResourceID)
				released++
				continue
			}
			if !isBrokerQuotaCountedPhase(agent.Phase) && time.Since(res.CreatedAt) >= reconcileMinReservationAge {
				s.quotaService.Release(ctx, store.LimitMaxAgentsPerBroker, agent.ID)
				released++
			}
		}

		// Backfill: agents on this broker in a counted phase with no active
		// reservation (legacy agents predating the cap, or any other gap).
		agents, err := s.store.ListAgents(ctx, store.AgentFilter{RuntimeBrokerID: broker.ID}, store.ListOptions{Limit: 10000})
		if err != nil {
			s.agentLifecycleLog.Warn("quota reconcile: failed to list agents for broker",
				"broker_id", broker.ID, "error", err)
			continue
		}
		for i := range agents.Items {
			agent := &agents.Items[i]
			if reservedIDs[agent.ID] || !isBrokerQuotaCountedPhase(agent.Phase) {
				continue
			}
			if _, err := s.store.CreateUsageReservation(ctx, &store.UsageReservation{
				LimitDefinitionID: limitDef.ID,
				SubjectID:         broker.ID,
				ScopeType:         store.QuotaScopeBroker,
				ScopeID:           broker.ID,
				ResourceID:        agent.ID,
				Reserved:          1,
			}); err != nil {
				s.agentLifecycleLog.Warn("quota reconcile: failed to backfill reservation",
					"agent_id", agent.ID, "broker_id", broker.ID, "error", err)
				continue
			}
			backfilled++
		}
	}

	if released > 0 || backfilled > 0 {
		s.agentLifecycleLog.Info("quota reconcile: reconciled max_agents_per_broker reservations",
			"checked", checked, "released", released, "backfilled", backfilled)
	}
}
