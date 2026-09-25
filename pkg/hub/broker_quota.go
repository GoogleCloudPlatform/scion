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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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
// duplicate reservation.
func (s *Server) checkAndReserveBrokerQuotaHTTP(ctx context.Context, w http.ResponseWriter, agent *store.Agent) bool {
	if agent.RuntimeBrokerID == "" {
		return true
	}
	return s.checkAndReserveQuota(ctx, w, store.LimitMaxAgentsPerBroker, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.ID)
}

// checkAndReserveBrokerQuota is the non-HTTP counterpart of
// checkAndReserveBrokerQuotaHTTP, for paths that cannot write an HTTP
// response directly (e.g. agent DM wake). Returns nil if the reservation
// succeeded, was already held (idempotent), or no limit is configured;
// returns store.ErrQuotaExceeded or ErrQuotaLockContention otherwise.
func (s *Server) checkAndReserveBrokerQuota(ctx context.Context, agent *store.Agent) error {
	if s.quotaService == nil || agent.RuntimeBrokerID == "" {
		return nil
	}
	return s.quotaService.CheckAndReserve(ctx, store.LimitMaxAgentsPerBroker, agent.RuntimeBrokerID, store.QuotaScopeBroker, agent.RuntimeBrokerID, agent.ID)
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
		if err := s.checkAndReserveBrokerQuota(ctx, agent); err != nil {
			s.agentLifecycleLog.Warn("quota: best-effort re-reserve on observed phase change failed",
				"agent_id", agent.ID, "old_phase", oldPhase, "new_phase", newPhase, "error", err)
		}
		return
	}
	s.releaseBrokerQuota(ctx, agent)
}

// ReconcileStaleBrokerQuotaReservations releases max_agents_per_broker
// reservations that are active (released_at IS NULL) for an agent that is no
// longer in a counted phase — or no longer exists — fixing rows left behind
// from before stop/suspend/crash release the reservation (ptone/scion#1963).
// Idempotent: safe to call on every hub startup. Best-effort throughout —
// this is bookkeeping cleanup, not on any request's critical path, so
// individual lookup/release failures are logged and skipped rather than
// aborting the whole pass.
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

	var reconciled, checked int
	for _, broker := range brokers.Items {
		reservations, err := s.store.ListActiveReservations(ctx, limitDef.ID, store.QuotaScopeBroker, broker.ID)
		if err != nil {
			s.agentLifecycleLog.Warn("quota reconcile: failed to list active reservations",
				"broker_id", broker.ID, "error", err)
			continue
		}
		for _, res := range reservations {
			checked++
			agent, err := s.store.GetAgent(ctx, res.ResourceID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					// The agent no longer exists (hard-deleted); its
					// reservation should have been released at delete time
					// but release the stale row now regardless.
					s.quotaService.Release(ctx, store.LimitMaxAgentsPerBroker, res.ResourceID)
					reconciled++
					continue
				}
				s.agentLifecycleLog.Warn("quota reconcile: failed to look up agent for reservation",
					"agent_id", res.ResourceID, "error", err)
				continue
			}
			if !isBrokerQuotaCountedPhase(agent.Phase) {
				s.quotaService.Release(ctx, store.LimitMaxAgentsPerBroker, agent.ID)
				reconciled++
			}
		}
	}

	if reconciled > 0 {
		s.agentLifecycleLog.Info("quota reconcile: released stale max_agents_per_broker reservations",
			"checked", checked, "released", reconciled)
	}
}
