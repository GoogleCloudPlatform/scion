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
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// reconcileMinReservationAge is the minimum age a reservation must reach
// before ReconcileStaleBrokerQuotaReservations will release it on the basis
// of the agent's stored phase. It is a secondary safeguard. The primary one
// is that every start-type dispatch (HTTP start and restart, create of an
// existing agent, DM wake) goes through beginStartDispatch, which writes the
// counted phase "starting" before dispatching, so the reconcile sees a
// counted phase for the whole dispatch leg however old the reservation is or
// however long the dispatch takes (ptone/scion#2014). The age gate still
// covers what that does not: a reservation taken by a path that does not
// write a counted phase first, a heartbeat from another hub replica that
// moves the row off "starting" mid-dispatch (heartbeatPhaseGuarded is a
// per-replica hint), and the window between the reserve and the starting
// write (ptone/scion#2011). 15 minutes clears the worst-case dispatch
// latency, including a cold image pull, with margin, and is not meant to be
// tuned per deployment. This grace period does not apply to reservations
// whose agent is missing or soft-deleted: those are always released
// regardless of age.
const reconcileMinReservationAge = 15 * time.Minute

// isBrokerQuotaCountedPhase reports whether phase currently counts toward an
// agent's runtime broker's max_agents_per_broker ceiling (ptone/scion#1963).
// Only stopped, suspended, error and halted are excluded: a container in any
// other phase (created/provisioning/cloning/starting/running) either
// occupies, or is actively moving toward occupying, broker capacity. Stopped
// and suspended agents have no running container; error means the container
// already exited or crashed; halted means the platform ended it. The rule
// lives in the store (store.BrokerQuotaCountedPhase), which evaluates it on a
// locked row for the conditional release (store.AgentReleaseCond.NotCounted).
func isBrokerQuotaCountedPhase(phase string) bool {
	return store.BrokerQuotaCountedPhase(phase)
}

// agentHoldsBrokerCapacity reports whether agent's reservation must be kept:
// it is in a counted phase, or its stop is queued for an offline broker (the
// container may still be running until the stop is applied or the container
// is confirmed gone). See store.AgentHoldsBrokerCapacity.
func agentHoldsBrokerCapacity(agent *store.Agent) bool {
	return store.AgentHoldsBrokerCapacity(agent)
}

// Sites of scion.hub.quota.backstop_reassert: a backstop re-assert that
// actually wrote a reservation, which means a release slipped through an
// unguarded path (design §4.4).
const (
	// backstopSiteRestart: reassertBrokerReservation, the restart's
	// re-assert after its stop leg and after its final write.
	backstopSiteRestart = "restart"
	// backstopSiteSettle: settleQueuedStops' re-assert after a start
	// superseded the queued stop during the release.
	backstopSiteSettle = "settle"
)

// OTelQuotaCapacityMetrics counts capacity repairs:
// scion.hub.quota.backstop_reassert (attribute site: restart or settle), each
// backstop re-assert that wrote a reservation, and
// scion.hub.quota.deregistered_scope_released, each reservation the sweep
// released on a Runtime Broker scope confirmed deregistered. The agent and
// broker IDs are in the log line and audit event, not in the attributes.
type OTelQuotaCapacityMetrics struct {
	backstop     metric.Int64Counter
	deregistered metric.Int64Counter
}

// NewOTelQuotaCapacityMetrics creates the counters.
func NewOTelQuotaCapacityMetrics(mp metric.MeterProvider) (*OTelQuotaCapacityMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel quota capacity metrics: nil MeterProvider")
	}
	m := mp.Meter(instrumentationScope)
	backstop, err := m.Int64Counter("scion.hub.quota.backstop_reassert", metric.WithUnit("{reservation}"))
	if err != nil {
		return nil, err
	}
	deregistered, err := m.Int64Counter("scion.hub.quota.deregistered_scope_released", metric.WithUnit("{reservation}"))
	if err != nil {
		return nil, err
	}
	return &OTelQuotaCapacityMetrics{backstop: backstop, deregistered: deregistered}, nil
}

// SetQuotaCapacityMetrics wires the capacity counters. Until it is set, the
// events are logged and audited but not counted.
func (s *Server) SetQuotaCapacityMetrics(m *OTelQuotaCapacityMetrics) {
	if s.quotaService != nil {
		s.quotaService.capacityMetrics.Store(m)
	}
}

// recordBackstopReassert counts one backstop re-assert write at site.
func (qs *QuotaService) recordBackstopReassert(site string) {
	if m := qs.capacityMetrics.Load(); m != nil {
		m.backstop.Add(context.Background(), 1, metric.WithAttributes(attribute.String("site", site)))
	}
}

// recordDeregisteredScopeReleased counts one reservation released on a
// deregistered Runtime Broker scope.
func (qs *QuotaService) recordDeregisteredScopeReleased() {
	if m := qs.capacityMetrics.Load(); m != nil {
		m.deregistered.Add(context.Background(), 1)
	}
}

// Mutation audit types of the capacity repairs (target: the agent).
const (
	auditQuotaBackstopReassert         = "quota_backstop_reassert"
	auditQuotaDeregisteredScopeRelease = "quota_deregistered_scope_released"
	auditStartClaimBrokerDeregistered  = "start_claim_released_broker_deregistered"
)

// capacityAuditSummary is the audit summary of a capacity repair.
type capacityAuditSummary struct {
	Reason          string `json:"reason"`
	RuntimeBrokerID string `json:"runtimeBrokerId"`
	Site            string `json:"site,omitempty"`
	ReservationID   string `json:"reservationId,omitempty"`
	ClaimID         string `json:"claimId,omitempty"`
	ClaimKind       string `json:"claimKind,omitempty"`
}

// writeCapacityAudit writes a mutation audit record of a capacity repair on
// agentID, attributed to the hub. Best-effort: a failure is logged.
func (s *Server) writeCapacityAudit(ctx context.Context, mutationType, agentID string, summary capacityAuditSummary) {
	record, err := lifecycleAudit(mutationType, agentID, AuditActor{}, time.Now(), summary)
	if err == nil {
		err = s.store.CreateMutationAudit(ctx, record)
	}
	if err != nil {
		s.agentLifecycleLog.Warn("quota: writing the capacity audit event failed",
			"mutation_type", mutationType, "agent_id", agentID,
			"runtime_broker_id", summary.RuntimeBrokerID, "error", err)
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

// rollbackBrokerQuota undoes a checkAndReserveBrokerQuota reservation
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

// releaseBrokerQuotaIf releases agent's max_agents_per_broker reservation
// only if cond holds on the stored agent row, in one conditional store write
// (store.ReleaseReservationIfAgent): a start that claimed the agent, or
// minted a run, since the caller read it keeps its slot. released reports
// whether a reservation was released; err is a store failure (logged here),
// distinct from a condition that did not hold or no active reservation
// (released false, err nil). A missing agent row releases nothing and is not
// an error here; the sweep releases a missing agent's row itself.
func (s *Server) releaseBrokerQuotaIf(ctx context.Context, agent *store.Agent, cond store.AgentReleaseCond) (released bool, err error) {
	if s.quotaService == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return false, nil
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		s.agentLifecycleLog.Warn("quota: conditional release: limit lookup failed",
			"agent_id", agent.ID, "error", err)
		return false, err
	}
	released, err = s.store.ReleaseReservationIfAgent(ctx, def.ID, agent.ID, cond)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		s.agentLifecycleLog.Warn("quota: conditional release failed",
			"agent_id", agent.ID, "error", err)
		return false, err
	}
	return released, nil
}

// releaseSlotIfIdle releases agentID's max_agents_per_broker reservation on
// the agent row's own Runtime Broker if the agent, as stored at the write,
// holds no capacity (store.AgentHoldsBrokerCapacity is false) and no
// start-kind claim, keyed on the run ID it reads (releaseSlotIfIdleOn with
// the row's RuntimeBrokerID). It runs where a claim or a launch ends with no
// claim run in hand: the launch-report hook, the reaper's unconfirmed release
// and SettleEndedLaunchClaim. A reservation on another broker is never
// released here; the sweep's rules decide it.
func (s *Server) releaseSlotIfIdle(ctx context.Context, agentID string) {
	row := s.readAgentForIdleRelease(ctx, agentID)
	if row == nil {
		return
	}
	s.releaseIdleSlotOn(ctx, row, row.RuntimeBrokerID)
}

// releaseSlotIfIdleOn is releaseSlotIfIdle for a start claim's holder: broker
// is the Runtime Broker the claim's start targeted, recorded when the claim
// was taken. It runs right after the claim is released without a hand-off,
// so a release that was skipped while the claim was held does not wait for
// the hourly sweep (design §4.4, k8s merge condition 2, R6-B1). The slot it
// may free is the one that start held: a reservation on broker, which must
// also be the agent row's broker. If the broker recorded at the claim ever
// differs from the row's or from the reservation's scope, or is empty, the
// release is skipped and the reservation is left to the sweep: the safe
// side. No caller relies on an unconditional release here. On a running
// agent it is a no-op. Best-effort.
func (s *Server) releaseSlotIfIdleOn(ctx context.Context, agentID, broker string) {
	row := s.readAgentForIdleRelease(ctx, agentID)
	if row == nil {
		return
	}
	s.releaseIdleSlotOn(ctx, row, broker)
}

// readAgentForIdleRelease reads agentID's row for an idle release; nil when
// there is nothing to do (no quota service, no row, or a read error, which
// is logged).
func (s *Server) readAgentForIdleRelease(ctx context.Context, agentID string) *store.Agent {
	if s.quotaService == nil || agentID == "" {
		return nil
	}
	row, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: release after the claim cleared: reading the agent failed",
				"agent_id", agentID, "error", err)
		}
		return nil
	}
	return row
}

// releaseIdleSlotOn releases row's active max_agents_per_broker reservation
// only if broker is set, is the row's RuntimeBrokerID, and is the
// reservation's broker scope, and only that reservation: its ID is part of
// the condition (store.ReleaseReservationIfAgent evaluates it with the rest
// on the locked rows), with the run ID read here, no start-kind claim and
// the agent holding no capacity (NotCounted). With the caller's agent read,
// it costs a limit-definition read, a reservation read and one conditional
// write.
func (s *Server) releaseIdleSlotOn(ctx context.Context, row *store.Agent, broker string) {
	if broker == "" || row.RuntimeBrokerID != broker {
		return
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: release after the claim cleared: limit lookup failed",
				"agent_id", row.ID, "error", err)
		}
		return
	}
	res, err := s.store.GetActiveReservation(ctx, def.ID, row.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: release after the claim cleared: reading the reservation failed",
				"agent_id", row.ID, "error", err)
		}
		return
	}
	if res.ScopeType != store.QuotaScopeBroker || res.ScopeID != broker {
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
			s.agentLifecycleLog.Warn("quota: release after the claim cleared failed",
				"agent_id", row.ID, "reservation_id", res.ID, "error", err)
		}
		return
	}
	if released {
		s.agentLifecycleLog.Info("quota: released the slot of an agent left idle when its claim cleared",
			"agent_id", row.ID, "runtime_broker_id", broker)
	}
}

// checkAndReserveBrokerQuota reserves agent's max_agents_per_broker slot with
// the cap check. Start-type dispatch sites call it through
// beginStartDispatch. Returns nil if the reservation
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

// reassertBrokerReservation records agent's max_agents_per_broker
// reservation if it holds none, without the cap check, the way
// ReconcileStaleBrokerQuotaReservations backfills one: it is accounting for
// a slot the caller already held, not a new admission decision. A restart
// calls it after its stop leg and after its final write. The restart holds
// its restart claim across both legs, and the observed-phase release skips
// an agent holding a start-kind claim (reconcileBrokerQuotaOnPhaseChange),
// so the dying container's own stopped report no longer frees the slot. The
// re-assert is therefore a backstop: it writes only if a release slipped
// through an unguarded path, such as a report handled by another replica
// between the restart's reserve and its claim. Because it skips the cap
// check, a racing start that took the freed slot in between can leave the
// broker one over its cap per in-flight restart until a slot frees; that is
// accepted, rather than refusing a restart whose container is already
// stopped. Every write is counted (scion.hub.quota.backstop_reassert, site
// "restart"), logged and audited with the agent and broker IDs. Reports
// whether it created a reservation. An active reservation on another broker
// is never overwritten or moved: it is reported as a scope conflict (log and
// metric, site "reassert"). Best-effort: failures are logged.
func (s *Server) reassertBrokerReservation(ctx context.Context, agent *store.Agent) bool {
	return s.reassertBrokerReservationAt(ctx, agent, backstopSiteRestart)
}

// restoreBrokerReservation records agent's max_agents_per_broker reservation
// if it holds none, without the cap check and without the backstop metric,
// log or audit: for a caller that released the slot itself just before (the
// reincarnation move worker's rollback of its own quota move), so the write
// is expected and is not a slipped release. Otherwise as
// reassertBrokerReservation.
func (s *Server) restoreBrokerReservation(ctx context.Context, agent *store.Agent) bool {
	return s.reassertBrokerReservationAt(ctx, agent, "")
}

// reassertBrokerReservationAt is reassertBrokerReservation with the backstop
// site that a write is counted and audited under; an empty site records
// nothing (restoreBrokerReservation).
func (s *Server) reassertBrokerReservationAt(ctx context.Context, agent *store.Agent, site string) bool {
	if s.quotaService == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return false
	}
	def, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota: backstop re-assert: limit lookup failed",
				"site", site, "agent_id", agent.ID, "error", err)
		}
		return false
	}
	held, err := s.store.GetActiveReservation(ctx, def.ID, agent.ID)
	switch {
	case err == nil:
		if held.ScopeType != store.QuotaScopeBroker || held.ScopeID != agent.RuntimeBrokerID {
			s.reportBrokerScopeConflict(scopeConflictSiteReassert, agent, held.ScopeID)
		}
		return false
	case !errors.Is(err, store.ErrNotFound):
		s.agentLifecycleLog.Warn("quota: backstop re-assert: reservation check failed",
			"site", site, "agent_id", agent.ID, "error", err)
		return false
	}
	created, err := s.recordBrokerReservationUnchecked(ctx, def.ID, agent.RuntimeBrokerID, agent.ID)
	if err != nil {
		var conflict *ReservationScopeConflictError
		if errors.As(err, &conflict) {
			s.reportBrokerScopeConflict(scopeConflictSiteReassert, agent, conflict.HeldScopeID)
			return false
		}
		s.agentLifecycleLog.Warn("quota: backstop re-assert failed",
			"site", site, "agent_id", agent.ID, "error", err)
		return false
	}
	if created && site != "" {
		s.recordBackstopWrite(ctx, agent, site)
	}
	return created
}

// recordBackstopWrite counts, logs and audits a backstop re-assert that
// wrote agent's reservation at site: a release slipped through an unguarded
// path, and the broker may be one over its cap until a slot frees.
func (s *Server) recordBackstopWrite(ctx context.Context, agent *store.Agent, site string) {
	s.quotaService.recordBackstopReassert(site)
	s.agentLifecycleLog.Warn("quota: backstop re-asserted a released reservation without the cap check",
		"site", site, "agent_id", agent.ID, "runtime_broker_id", agent.RuntimeBrokerID)
	s.writeCapacityAudit(ctx, auditQuotaBackstopReassert, agent.ID, capacityAuditSummary{
		Reason:          "backstop re-assert: the reservation was released while the agent held its slot",
		RuntimeBrokerID: agent.RuntimeBrokerID,
		Site:            site,
	})
}

// recordBrokerReservationUnchecked records agentID's max_agents_per_broker
// reservation on brokerID directly, with no cap check. It is the shared write
// of the reconcile backfill and the restart's re-assert
// (reassertBrokerReservation): accounting for a slot that is already in
// use, not an admission decision.
//
// A racing insert is classified as in QuotaService.Reserve: when the agent
// already holds an active reservation, created is false and err is nil if it
// is on brokerID, or a *ReservationScopeConflictError if it is on another
// broker (left as it is). When the winner was released before it could be
// read, the insert is retried once.
func (s *Server) recordBrokerReservationUnchecked(ctx context.Context, limitDefID, brokerID, agentID string) (bool, error) {
	for attempt := 0; attempt < maxBrokerReservationInserts; attempt++ {
		_, err := s.store.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: limitDefID,
			SubjectID:         brokerID,
			ScopeType:         store.QuotaScopeBroker,
			ScopeID:           brokerID,
			ResourceID:        agentID,
			Reserved:          1,
		})
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrActiveReservationExists) {
			return false, err
		}
		held, err := s.store.GetActiveReservation(ctx, limitDefID, agentID)
		if err == nil {
			if held.ScopeType == store.QuotaScopeBroker && held.ScopeID == brokerID {
				return false, nil
			}
			return false, &ReservationScopeConflictError{
				Limit:            store.LimitMaxAgentsPerBroker,
				ResourceID:       agentID,
				HeldScopeID:      held.ScopeID,
				RequestedScopeID: brokerID,
			}
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	return false, ErrQuotaLockContention
}

// reportBrokerScopeConflict logs and counts, at site, an agent whose active
// max_agents_per_broker reservation is on heldBrokerID rather than its
// Runtime Broker. The reservation is left as it is: never moved, released or
// replaced.
func (s *Server) reportBrokerScopeConflict(site string, agent *store.Agent, heldBrokerID string) {
	s.quotaService.recordScopeConflict(site)
	s.agentLifecycleLog.Warn("quota: agent's broker reservation is on another Runtime Broker; left unchanged",
		"site", site, "agent_id", agent.ID, "pinned", agent.IsPinned(),
		"runtime_broker_id", agent.RuntimeBrokerID, "reservation_broker_id", heldBrokerID)
}

// reconcileBrokerQuotaOnPhaseChange updates agent's max_agents_per_broker
// reservation to match an *observed* phase transition (heartbeat or agent
// self-report status update) rather than an explicit lifecycle action. It is
// the counterpart, for the "hub observes reality" paths, of the explicit
// release/reserve calls in the lifecycle handlers (ptone/scion#1963).
//
//   - counted → not counted (e.g. the broker reports the container exited or
//     crashed): release, unless a start owns the slot. The release is one
//     conditional store write keyed on the run ID read with agent and on no
//     start-kind claim being held (store.ReleaseReservationIfAgent), so a
//     restart, or any start that claimed the agent or minted a run since the
//     read, keeps its reservation; a stop-kind claim does not block it (a stop
//     claim blocks every start). It is also skipped while a lifecycle
//     operation for the agent runs on this replica. Countedness comes from
//     the observed phase, not the stored row: two callers run before the
//     phase is persisted, so the condition does not include NotCounted. A
//     release skipped because a claim was held is completed by
//     releaseSlotIfIdle when the claim clears, or by the sweep.
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
	if agent == nil || s.lifecycleOps.active(agent.ID) {
		return
	}
	_, _ = s.releaseBrokerQuotaIf(ctx, agent, store.AgentReleaseCond{
		ExpectedRunID:    agent.RunID,
		NoStartKindClaim: true,
	})
}

// reconcileBrokerListLimit bounds the sweep's Runtime Broker list (one page,
// brokerScopedListLimit). A variable for tests.
var reconcileBrokerListLimit = brokerScopedListLimit

// ReconcileStaleBrokerQuotaReservations reconciles max_agents_per_broker
// reservations against observed reality, per runtime broker (ptone/scion#1963):
//
//   - Release: an active reservation (released_at IS NULL) whose agent no
//     longer exists, is soft-deleted, or is no longer in a counted phase —
//     fixing rows left behind from before stop/suspend/crash released the
//     reservation, or from a delete path that missed the release call. The
//     phase-based case is skipped for a reservation younger than
//     reconcileMinReservationAge, a secondary safeguard behind
//     beginStartDispatch's pre-dispatch "starting" write (ptone/scion#2011,
//     ptone/scion#2014); the missing/soft-deleted case is never subject to
//     that grace period.
//   - Backfill: an agent on this broker that holds capacity with no active
//     reservation gets one recorded directly (no cap check — this is
//     accounting for an agent that already exists and is already running,
//     not a new admission decision; see checkAndReserveBrokerQuota for the
//     cap-enforced path used at start/resume). Covers legacy agents created
//     before max_agents_per_broker existed, which hold no reservation at all,
//     and an agent whose stop is queued and still intended that lost its
//     reservation (brokerBackfillHolds).
//   - Scope conflicts: a pinned agent whose active reservation is on a broker
//     other than its Runtime Broker is never moved or backfilled, and is not
//     released while the agent holds capacity (agentHoldsBrokerCapacity);
//     once it holds none, the release rule above applies (contract erratum
//     E-FC11-1). While it is kept it is reported (log and metric, sites
//     "sweep" and "backfill") in each pass that reads it, and shown with
//     scopeConflict on the admin usage view. A pass that cannot load a
//     listed broker's agents skips that broker, so its conflicts are
//     reported on a later pass. An unpinned agent's row on another broker is
//     released by the phase rule above as before, and its backfill is
//     refused and reported the same way.
//   - Deregistered scopes: a reservation whose broker is not in the list is
//     released only when a point lookup confirms the broker is gone
//     (releaseDeregisteredScopeReservations).
//
// Every release is conditional on what the pass read. A missing agent's row
// is released by its reservation ID, so a newer reservation of the same
// agent (a restore and a start after the read) is left alone. The
// phase-based release is one conditional store write on the agent row: the
// reservation the pass read, the run ID it read, no start-kind claim, and
// the stored row holding no capacity (store.ReleaseReservationIfAgent). A
// start that claimed the agent, minted a run or re-reserved since the read
// therefore keeps its slot, including a same-broker start that reuses the
// row as an idempotent retry once it holds its claim. A start takes its
// claim before it reserves, except a restart, which reserves first; in that
// reserve-to-claim window the restart is covered, as before, only by the
// grace period and beginStartDispatch's "starting" write.
//
// Idempotent both ways: releasing an already-released reservation and
// re-running the backfill after a prior successful pass are no-ops (the
// latter enforced by the DB's unique-active-reservation-per-resource index,
// not just the in-memory check below). Safe to call on every hub startup.
// Best-effort throughout — this is bookkeeping, not on any request's
// critical path, so individual failures are logged and skipped rather than
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

	brokers, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: reconcileBrokerListLimit})
	if err != nil {
		s.agentLifecycleLog.Warn("quota reconcile: failed to list runtime brokers", "error", err)
		return
	}

	// One query for every broker's active reservations (ptone/scion#2314),
	// grouped by broker below. If this query fails the whole pass is
	// skipped; the next scheduled pass retries.
	//
	// The rows are one snapshot taken at the start of the pass, not a fresh
	// read per broker, so a later broker's rows can be older than the work
	// already done for earlier brokers. That is acceptable: every release is
	// conditional on what the pass read (see above), releasing an
	// already-released row is a no-op, and the backfill is idempotent through
	// the unique active-reservation index.
	allReservations, err := s.store.ListActiveReservationsByScopeType(ctx, limitDef.ID, store.QuotaScopeBroker)
	if err != nil {
		s.agentLifecycleLog.Warn("quota reconcile: failed to list active reservations", "error", err)
		return
	}

	listedBrokers := make(map[string]bool, len(brokers.Items))
	for _, broker := range brokers.Items {
		listedBrokers[broker.ID] = true
	}

	// Rows on unlisted scopes: released only on a confirmed-deleted broker.
	// Released rows are dropped from the snapshot, so the backfill below
	// can record a running agent on its real broker in this pass.
	deregistered := s.releaseDeregisteredScopeReservations(ctx, allReservations, listedBrokers)
	if len(deregistered) > 0 {
		kept := make([]*store.UsageReservation, 0, len(allReservations))
		for _, res := range allReservations {
			if !deregistered[res.ID] {
				kept = append(kept, res)
			}
		}
		allReservations = kept
	}

	reservationsByBroker := groupReservationsByScopeID(allReservations)
	// heldBroker maps each reserved agent to the broker its active row is on,
	// so a backfill can see a row held on this or another broker.
	heldBroker := make(map[string]string, len(allReservations))
	for _, res := range allReservations {
		heldBroker[res.ResourceID] = res.ScopeID
	}

	var checked, released, backfilled, conflicts int
	for _, broker := range brokers.Items {
		reservations := reservationsByBroker[broker.ID]

		// Batch-fetch every reserved agent in one query instead of one
		// GetAgent per reservation (N+1). GetAgentsByIDs excludes
		// soft-deleted agents, so a soft-deleted agent's reservation is
		// released via the same "missing from the map" path as a
		// hard-deleted one — its stored phase (which may still read e.g.
		// "running") never enters into it.
		resourceIDs := make([]string, len(reservations))
		for i, res := range reservations {
			resourceIDs[i] = res.ResourceID
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
				if s.releaseObservedReservation(ctx, res) {
					released++
				}
				continue
			}
			if !agentHoldsBrokerCapacity(agent) && time.Since(res.CreatedAt) >= reconcileMinReservationAge {
				// No container to protect, wherever the row is: release.
				// For a pinned agent's row on another broker this is not a
				// move; its next start reserves on its own broker.
				if s.releaseObservedIdleReservation(ctx, limitDef.ID, agent, res) {
					released++
				}
				continue
			}
			if agent.IsPinned() && agent.RuntimeBrokerID != res.ScopeID {
				// A pinned agent whose row on another broker is not released
				// above (it holds capacity, or the row is within the grace):
				// report it, never release or move it.
				s.reportBrokerScopeConflict(scopeConflictSiteSweep, agent, res.ScopeID)
				conflicts++
			}
		}

		// Backfill: agents on this broker that hold capacity with no active
		// reservation (legacy agents predating the cap, or any other gap). A
		// pinned agent's row on another broker is reported here only when
		// that broker is not listed; the release pass reports it otherwise.
		n, c, _, err := s.backfillBrokerAgents(ctx, limitDef.ID, broker.ID, func(agentID string) (string, bool, error) {
			held, ok := heldBroker[agentID]
			return held, ok, nil
		}, func(held string) bool { return !listedBrokers[held] }, false)
		backfilled += n
		conflicts += c
		if err != nil {
			s.agentLifecycleLog.Warn("quota reconcile: failed to list agents for broker",
				"broker_id", broker.ID, "error", err)
			continue
		}
	}

	if released > 0 || backfilled > 0 || conflicts > 0 || len(deregistered) > 0 {
		s.agentLifecycleLog.Info("quota reconcile: reconciled max_agents_per_broker reservations",
			"checked", checked, "released", released, "backfilled", backfilled, "scope_conflicts", conflicts,
			"deregistered_released", len(deregistered))
	}
}

// releaseObservedReservation releases the reservation row res that the
// reconcile read, and only that row, in one conditional write: a start (or
// a restore and a start) that re-reserved since the read holds a new row,
// which is left alone. Reports whether it released res. Best-effort: a
// failure is logged.
func (s *Server) releaseObservedReservation(ctx context.Context, res *store.UsageReservation) bool {
	if err := s.store.ReleaseReservationByID(ctx, res.ID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("quota reconcile: failed to release reservation",
				"agent_id", res.ResourceID, "reservation_id", res.ID, "error", err)
		}
		return false
	}
	return true
}

// releaseObservedIdleReservation releases res, the row the reconcile read for
// agent, only if the stored agent row still has the run ID the reconcile
// read, holds no start-kind claim and holds no capacity, in one conditional
// store write (store.ReleaseReservationIfAgent). Reports whether it released
// res. Best-effort: a failure is logged.
func (s *Server) releaseObservedIdleReservation(ctx context.Context, limitDefID string, agent *store.Agent, res *store.UsageReservation) bool {
	released, err := s.store.ReleaseReservationIfAgent(ctx, limitDefID, agent.ID, store.AgentReleaseCond{
		ExpectedRunID:    agent.RunID,
		NoStartKindClaim: true,
		NotCounted:       true,
		ReservationID:    res.ID,
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.agentLifecycleLog.Warn("quota reconcile: failed to release reservation",
			"agent_id", agent.ID, "reservation_id", res.ID, "error", err)
	}
	return released
}

// releaseDeregisteredScopeReservations releases the active reservations, of
// reservations, whose Runtime Broker scope is not listed and is confirmed
// deregistered, and returns the IDs it released. Not being listed is only a
// candidate: the list is capped (the oldest brokers beyond the cap are not
// in it), and it is read before the reservations, so a broker registered in
// between is not in it either. Each candidate scope is confirmed with a
// point lookup, once per pass: store.ErrNotFound confirms it; a found broker
// is live and its rows are not touched; any other error skips the scope
// this pass. Each row of a confirmed scope is released with one conditional
// write that also requires that no broker has the scope's ID
// (store.ReleaseReservationIfScopeBrokerAbsent), so a broker that
// re-registers with the same ID after the lookup keeps its rows. Each
// release is counted (scion.hub.quota.deregistered_scope_released), logged
// and audited. A pinned agent's row is released here too: the broker it is
// on no longer exists, so the row counts against no live instance.
func (s *Server) releaseDeregisteredScopeReservations(ctx context.Context, reservations []*store.UsageReservation, listed map[string]bool) map[string]bool {
	const (
		scopeLive = iota
		scopeDeleted
		scopeUnknown
	)
	lookups := map[string]int{}
	var released map[string]bool
	for _, res := range reservations {
		if listed[res.ScopeID] {
			continue
		}
		st, seen := lookups[res.ScopeID]
		if !seen {
			_, err := s.store.GetRuntimeBroker(ctx, res.ScopeID)
			switch {
			case err == nil:
				st = scopeLive
			case errors.Is(err, store.ErrNotFound):
				st = scopeDeleted
			default:
				st = scopeUnknown
				s.agentLifecycleLog.Warn("quota reconcile: looking up an unlisted Runtime Broker scope failed; its reservations are kept this pass",
					"broker_id", res.ScopeID, "error", err)
			}
			lookups[res.ScopeID] = st
		}
		if st != scopeDeleted {
			continue
		}
		ok, err := s.store.ReleaseReservationIfScopeBrokerAbsent(ctx, res.ID, res.ScopeID)
		if err != nil {
			s.agentLifecycleLog.Warn("quota reconcile: failed to release a reservation on a deregistered Runtime Broker",
				"agent_id", res.ResourceID, "reservation_id", res.ID, "broker_id", res.ScopeID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		if released == nil {
			released = map[string]bool{}
		}
		released[res.ID] = true
		s.quotaService.recordDeregisteredScopeReleased()
		s.agentLifecycleLog.Warn("quota reconcile: capacity released: Runtime Broker deregistered; runtime state unknown",
			"agent_id", res.ResourceID, "runtime_broker_id", res.ScopeID, "reservation_id", res.ID)
		s.writeCapacityAudit(ctx, auditQuotaDeregisteredScopeRelease, res.ResourceID, capacityAuditSummary{
			Reason:          "capacity released: Runtime Broker " + res.ScopeID + " deregistered; runtime state unknown",
			RuntimeBrokerID: res.ScopeID,
			ReservationID:   res.ID,
		})
	}
	return released
}

// releaseDeletedBrokerReservations releases the active max_agents_per_broker
// reservations scoped to brokerID once its row is deleted, so no active
// reservation is left pointing at a Runtime Broker that no longer exists
// (ptone/scion#3994). It is the sweep's deregistered-scope pass
// (releaseDeregisteredScopeReservations) run on that one scope: each row is
// released only after a point lookup confirms the broker is gone and with a
// conditional write that also requires it, and each release is counted,
// logged and audited exactly as the sweep does. Best-effort: a row it misses
// is released by the next sweep.
func (s *Server) releaseDeletedBrokerReservations(ctx context.Context, brokerID string) {
	if s.quotaService == nil {
		return
	}
	limitDef, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.agentLifecycleLog.Warn("broker delete: failed to look up max_agents_per_broker limit definition; the sweep releases the broker's reservations",
				"broker_id", brokerID, "error", err)
		}
		return
	}
	reservations, err := s.store.ListActiveReservations(ctx, limitDef.ID, store.QuotaScopeBroker, brokerID)
	if err != nil {
		s.agentLifecycleLog.Warn("broker delete: failed to list the broker's reservations; the sweep releases them",
			"broker_id", brokerID, "error", err)
		return
	}
	s.releaseDeregisteredScopeReservations(ctx, reservations, nil)
}

// brokerBackfillHolds reports whether the backfill records a reservation for
// agent when it holds none: it is in a counted phase, or its stop is queued
// and still intended (the container may still be running; container status
// alone is not enough).
func brokerBackfillHolds(agent *store.Agent) bool {
	return isBrokerQuotaCountedPhase(agent.Phase) || (agentHoldsBrokerCapacity(agent) && queuedStopStillIntended(agent))
}

// backfillBrokerAgents records, without the cap check, the reservation of
// every agent on brokerID that holds capacity (brokerBackfillHolds) and has
// no active reservation. heldBroker reports, for such an agent, the broker
// its active row is on (ok false when it holds none; err when that cannot be
// read, and the agent is skipped this time): an agent with a row on brokerID
// is skipped, and a pinned agent with a row on another broker is never
// backfilled; it is reported as a scope conflict when reportHeld returns true
// for that broker. An unpinned agent's backfill onto a row held elsewhere is
// refused by the unique index and reported. Returns the number of
// reservations recorded, of conflicts reported and of agents skipped
// because their reservation could not be read or written; err is the agent
// list's error.
func (s *Server) backfillBrokerAgents(ctx context.Context, limitDefID, brokerID string, heldBroker func(agentID string) (held string, ok bool, err error), reportHeld func(heldBrokerID string) bool, quiet bool) (backfilled, conflicts, skipped int, err error) {
	skipLog := s.agentLifecycleLog.Warn
	if quiet {
		skipLog = s.agentLifecycleLog.Debug
	}
	agents, err := s.store.ListAgents(ctx, store.AgentFilter{RuntimeBrokerID: brokerID}, store.ListOptions{Limit: 10000})
	if err != nil {
		return 0, 0, 0, err
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		if !brokerBackfillHolds(agent) {
			continue
		}
		held, hasHeld, herr := heldBroker(agent.ID)
		if herr != nil {
			skipLog("quota backfill: reading the agent's reservation failed",
				"agent_id", agent.ID, "broker_id", brokerID, "error", herr)
			skipped++
			continue
		}
		if hasHeld && held == brokerID {
			continue
		}
		if hasHeld && agent.IsPinned() {
			if reportHeld(held) {
				s.reportBrokerScopeConflict(scopeConflictSiteBackfill, agent, held)
				conflicts++
			}
			continue
		}
		created, err := s.recordBrokerReservationUnchecked(ctx, limitDefID, brokerID, agent.ID)
		if err != nil {
			var conflict *ReservationScopeConflictError
			if errors.As(err, &conflict) {
				s.reportBrokerScopeConflict(scopeConflictSiteBackfill, agent, conflict.HeldScopeID)
				conflicts++
				continue
			}
			skipLog("quota reconcile: failed to backfill reservation",
				"agent_id", agent.ID, "broker_id", brokerID, "error", err)
			skipped++
			continue
		}
		if created {
			backfilled++
		}
	}
	return backfilled, conflicts, skipped, nil
}

// backfillBrokerReservations runs the reconcile's backfill for one broker
// (backfillBrokerAgents), outside the hourly pass. It reads only the active
// reservation of each of that broker's agents that holds capacity (one
// point read per such agent), not every reservation on the hub. Reports
// whether it ran to the end with every agent read (true also when no
// max_agents_per_broker limit is defined).
func (s *Server) backfillBrokerReservations(ctx context.Context, brokerID string) bool {
	if s.quotaService == nil {
		return true
	}
	limitDef, err := s.store.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true
		}
		s.agentLifecycleLog.Debug("quota backfill: failed to look up max_agents_per_broker limit definition",
			"broker_id", brokerID, "error", err)
		return false
	}
	heldBroker := func(agentID string) (string, bool, error) {
		res, err := s.store.GetActiveReservation(ctx, limitDef.ID, agentID)
		switch {
		case err == nil:
			return res.ScopeID, true, nil
		case errors.Is(err, store.ErrNotFound):
			return "", false, nil
		default:
			return "", false, err
		}
	}
	backfilled, conflicts, skipped, err := s.backfillBrokerAgents(ctx, limitDef.ID, brokerID, heldBroker, func(string) bool { return true }, true)
	if err != nil {
		s.agentLifecycleLog.Debug("quota backfill: failed to list agents for broker", "broker_id", brokerID, "error", err)
		return false
	}
	if backfilled > 0 {
		s.agentLifecycleLog.Info("quota backfill: recorded reservations of a Runtime Broker's agents",
			"broker_id", brokerID, "backfilled", backfilled, "scope_conflicts", conflicts, "skipped", skipped)
	} else if conflicts > 0 || skipped > 0 {
		s.agentLifecycleLog.Debug("quota backfill: no reservation recorded for a Runtime Broker's agents",
			"broker_id", brokerID, "scope_conflicts", conflicts, "skipped", skipped)
	}
	// A skipped agent is retried (the backfill is idempotent): the caller
	// keeps the broker pending (runBrokerBackfill).
	return skipped == 0
}

// brokerBackfillWarnInterval bounds the WARN for a broker whose
// registration backfill keeps skipping agents: it is logged on the first
// skip, then at most once per interval per broker on this replica (each
// attempt also leaves a debug record with the cause).
const brokerBackfillWarnInterval = 15 * time.Minute

// brokerBackfillRetry is a broker whose registration backfill skipped an agent
// on this replica: its heartbeats retry the backfill until it runs to the
// end.
type brokerBackfillRetry struct {
	mu       sync.Mutex
	lastWarn time.Time
}

// BackfillBrokerOnRegistration records, when a Runtime Broker's row is
// written by a registration path, the reservations of the agents it still
// runs (backfillBrokerReservations), so a broker registered again under the
// same ID after a deregistration released its reservations gets them back
// before its first heartbeat and before the hourly sweep. Every registration
// path calls it after the row is written and before it reports success: the
// broker join (handleBrokerJoin); a flat registration that creates the row
// (registerFlatRuntimeBroker: the co-located flat registration, and the HTTP
// flat registration, whose join then repeats it idempotently); the embedded
// provider registration (handleProjectRegister's embedded path); and the
// co-located broker at server startup (cmd, on every start). It is idempotent
// and bounded to brokerID's agents. The writes go to the shared store, so
// every replica sees them. If an agent is skipped (a reservation read or
// write failed), the broker is kept pending on this replica and its
// heartbeats retry (retryBrokerBackfill); a new registration starts afresh.
// Accepted gap: if this replica goes away while the broker is pending, the
// hourly sweep's backfill is the backstop.
func (s *Server) BackfillBrokerOnRegistration(ctx context.Context, brokerID string) {
	if s.quotaService == nil || brokerID == "" {
		return
	}
	s.quotaService.brokerBackfillPending.Delete(brokerID)
	s.runBrokerBackfill(ctx, brokerID)
}

// retryBrokerBackfill retries the registration backfill of brokerID from its
// heartbeat, only if that backfill is pending on this replica. Otherwise it
// returns without a store read: a heartbeat that needs nothing reads nothing.
func (s *Server) retryBrokerBackfill(ctx context.Context, brokerID string) {
	if s.quotaService == nil {
		return
	}
	if _, ok := s.quotaService.brokerBackfillPending.Load(brokerID); !ok {
		return
	}
	s.runBrokerBackfill(ctx, brokerID)
}

// runBrokerBackfill runs backfillBrokerReservations for brokerID, clears the
// pending entry when it ran to the end, and otherwise keeps the broker
// pending, with a rate-limited WARN. The backfill is idempotent: an agent
// already reserved on brokerID is not written again.
func (s *Server) runBrokerBackfill(ctx context.Context, brokerID string) {
	if s.backfillBrokerReservations(ctx, brokerID) {
		s.quotaService.brokerBackfillPending.Delete(brokerID)
		return
	}
	v, _ := s.quotaService.brokerBackfillPending.LoadOrStore(brokerID, &brokerBackfillRetry{})
	retry := v.(*brokerBackfillRetry)
	retry.mu.Lock()
	warn := retry.lastWarn.IsZero() || time.Since(retry.lastWarn) >= brokerBackfillWarnInterval
	if warn {
		retry.lastWarn = time.Now()
	}
	retry.mu.Unlock()
	if warn {
		s.agentLifecycleLog.Warn("quota backfill: could not record every reservation of a Runtime Broker's agents; retrying on its heartbeats on this replica (the hourly sweep is the backstop)",
			"broker_id", brokerID)
	}
}

// forgetBrokerBackfill drops brokerID's pending registration backfill on this
// replica, when the broker is deregistered here.
func (s *Server) forgetBrokerBackfill(brokerID string) {
	if s.quotaService != nil {
		s.quotaService.brokerBackfillPending.Delete(brokerID)
	}
}
