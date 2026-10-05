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
	"log/slog"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The start-claim reaper (a singleton, every minute) moves start claims on
// when their holder cannot: it demotes a live claim whose lease expired to
// unconfirmed, and releases an unconfirmed claim once the runtime shows the
// start's outcome. It never releases on time alone: an offline broker, or
// one with no fresh complete inventory of the agent's target, keeps the
// claim. Agents with an active launch are left to the launch.

// unconfirmedStopInterval rate-limits the reaper's stop of a container that
// an unconfirmed start left running past its hold.
const unconfirmedStopInterval = 2 * time.Minute

// unconfirmedObservationLag is how long after a claim became unconfirmed an
// inventory must have been received to show the start's outcome: one
// heartbeat interval, which covers a heartbeat the broker had already
// assembled before the start was cancelled. A variable for tests.
var unconfirmedObservationLag = observationFreshness

// startClaimReaperHandler is the scheduler entry point.
func (s *Server) startClaimReaperHandler() func(ctx context.Context) {
	return func(ctx context.Context) { s.reapStartClaims(ctx) }
}

// reaperBrokerView is what the reaper reads once per broker per tick.
type reaperBrokerView struct {
	broker *store.RuntimeBroker
	inv    map[string]time.Time
}

func (s *Server) reapStartClaims(ctx context.Context) {
	agents, err := s.store.ListAgentsWithStartClaim(ctx)
	if err != nil {
		slog.Warn("Start claim reaper: listing claims failed", "error", err)
		return
	}
	if len(agents) == 0 {
		return
	}
	now, err := s.store.StoreClock(ctx)
	if err != nil {
		slog.Warn("Start claim reaper: reading the store clock failed", "error", err)
		return
	}
	holds := s.startClaimSettings().Holds()
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		ids = append(ids, a.ID)
	}
	obs, err := s.store.GetRecoveryObservations(ctx, ids)
	if err != nil {
		slog.Warn("Start claim reaper: reading observations failed", "error", err)
		return
	}
	views := map[string]*reaperBrokerView{}
	for _, a := range agents {
		view, ok := views[a.RuntimeBrokerID]
		if !ok {
			view = s.reaperBrokerView(ctx, a.RuntimeBrokerID)
			views[a.RuntimeBrokerID] = view
		}
		s.reapStartClaim(ctx, a, now, holds, view, obs[a.ID])
	}
}

func (s *Server) reaperBrokerView(ctx context.Context, brokerID string) *reaperBrokerView {
	if brokerID == "" {
		return nil
	}
	b, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		return nil
	}
	rows, err := s.store.ListBrokerTargetInventory(ctx, brokerID)
	if err != nil {
		return nil
	}
	return &reaperBrokerView{
		broker: b,
		inv:    targetInventoryTimes(rows),
	}
}

// reapStartClaim applies the reaper rules to one claim.
func (s *Server) reapStartClaim(ctx context.Context, a *store.Agent, now time.Time, holds store.StartClaimHolds, view *reaperBrokerView, obs store.RecoveryObservationRecord) {
	if a.LaunchState == store.LaunchStateActive {
		return // the launch owns liveness and settles the claim when it ends
	}
	if a.StartClaimKind == store.StartClaimCreate && a.StartClaimLaunchID != "" && a.StartClaimLaunchID == a.LaunchID {
		if changed, err := s.store.SettleEndedLaunchClaim(ctx, a.ID, a.StartClaimID); err != nil || changed {
			return
		}
	}
	switch a.StartClaimState {
	case store.StartClaimLive:
		if a.StartClaimLeaseUntil != nil && a.StartClaimLeaseUntil.Before(now) {
			if _, err := s.store.DemoteExpiredStartClaim(ctx, a.ID, a.StartClaimID, holds); err != nil {
				slog.Warn("Start claim reaper: demote failed", "agent_id", a.ID, "error", err)
			}
		}
	case store.StartClaimUnconfirmed:
		s.reapUnconfirmedStartClaim(ctx, a, now, holds, view, obs)
	}
}

// startReportedRunning reports whether the agent reported running after its
// claim became unconfirmed: the start succeeded after all. last_seen is
// written on the hub process clock, so on Postgres this compares across
// clocks; the error is the clock skew between hub and database.
func startReportedRunning(a *store.Agent) bool {
	return a.Phase == string(state.PhaseRunning) && a.StartClaimUnconfirmedAt != nil &&
		!a.LastSeen.Before(*a.StartClaimUnconfirmedAt)
}

// reapUnconfirmedStartClaim releases an unconfirmed claim when the agent
// reported running, or when a fresh complete inventory received at least
// one heartbeat interval after the claim became unconfirmed shows nothing
// running and (for a broker that reports them) no start in flight. Past
// the hold, a container still present is stopped with normal grace; the
// claim is released once a later inventory shows it gone.
func (s *Server) reapUnconfirmedStartClaim(ctx context.Context, a *store.Agent, now time.Time, holds store.StartClaimHolds, view *reaperBrokerView, obs store.RecoveryObservationRecord) {
	release := func(why string) {
		if ok, err := s.store.ReleaseUnconfirmedStart(ctx, a.ID, a.StartClaimID); err != nil {
			slog.Warn("Start claim reaper: release failed", "agent_id", a.ID, "error", err)
		} else if ok {
			s.claimStops.Delete(a.ID)
			slog.Info("Start claim reaper: released an unconfirmed start claim", "agent_id", a.ID, "kind", a.StartClaimKind, "reason", why)
		}
	}
	if view == nil || a.StartClaimUnconfirmedAt == nil {
		return
	}
	if obs.Target == "" || obs.Target != reaperTarget(a, view) ||
		!observationFresh(obs, view.inv, now) {
		return // no fresh inventory of the target: keep the claim
	}
	// The start succeeded after all: the agent reported running since the
	// claim became unconfirmed, and a fresh inventory taken after that
	// lists its container running.
	if startReportedRunning(a) && obs.State == store.ObservedPresentRunning && !obs.InFlight &&
		!obs.ObservedAt.Before(a.StartClaimUnconfirmedAt.Add(unconfirmedObservationLag)) {
		release("agent reported running")
		return
	}
	// In flight: the broker lists the start (trusted only from a broker
	// that reports starts in flight; an older broker never sends the list)
	// or the hub has a create, start or restart queued for it (always
	// trusted). Both are recorded in observed_in_flight; a broker without
	// the capability can only have set it through the hub's queue.
	inFlight := obs.InFlight
	gone := obs.State == store.ObservedAbsent || obs.State == store.ObservedPresentTerminal
	if gone && !inFlight && !obs.ObservedAt.Before(a.StartClaimUnconfirmedAt.Add(unconfirmedObservationLag)) {
		release("runtime shows nothing running")
		return
	}
	if exp, ok := holds.HoldExpiry(a); ok && !now.Before(exp) && (obs.State == store.ObservedPresentRunning || inFlight) {
		s.stopUnconfirmedStart(ctx, a.ID, a.StartClaimID)
	}
}

// reaperTarget is the target the reaper reads an agent's observation on:
// its recorded or expected target, or the broker's only complete target.
func reaperTarget(a *store.Agent, view *reaperBrokerView) string {
	if t := observationTarget(a); t != "" {
		return t
	}
	if len(view.inv) == 1 {
		for t := range view.inv {
			return t
		}
	}
	return ""
}

// stopUnconfirmedStart stops, with normal grace, a container an unconfirmed
// start left running past its hold. It does not change run intent. The stop
// is sent from its own goroutine, so one slow broker does not age the rest
// of the tick, and only after re-reading the agent: the same unconfirmed
// claim (claimID) must still be held, so a stop never reaches a newer
// start's container once that claim was released.
func (s *Server) stopUnconfirmedStart(ctx context.Context, agentID, claimID string) {
	if last, ok := s.claimStops.Load(agentID); ok && time.Since(last.(time.Time)) < unconfirmedStopInterval {
		return
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	s.claimStops.Store(agentID, time.Now())
	go func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		cur, err := s.store.GetAgent(stopCtx, agentID)
		if err != nil || cur.StartClaimID != claimID || cur.StartClaimState != store.StartClaimUnconfirmed {
			return
		}
		slog.Info("Start claim reaper: stopping a container an unconfirmed start left running past its hold", "agent_id", agentID)
		if err := dispatcher.DispatchAgentStop(stopCtx, cur); err != nil {
			slog.Warn("Start claim reaper: stop failed", "agent_id", agentID, "error", err)
		}
	}()
}

// demoteOwnClaimsOnRestart runs once at startup: when this replica runs as
// a pod (POD_NAME set), claims owned by a previous process of the same pod
// can no longer be renewed, so they become unconfirmed at once rather than
// after their lease expires.
func (s *Server) demoteOwnClaimsOnRestart(ctx context.Context) {
	pod := os.Getenv("POD_NAME")
	if pod == "" {
		return
	}
	n, err := s.store.DemoteOwnerStartClaims(ctx, pod+"-", s.instanceID, s.startClaimSettings().Holds())
	if err != nil {
		slog.Warn("Start claims of a previous process could not be demoted", "error", err)
		return
	}
	if n > 0 {
		slog.Info("Demoted start claims of a previous process of this pod", "count", n)
	}
}
