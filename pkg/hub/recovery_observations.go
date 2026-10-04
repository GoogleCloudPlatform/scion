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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file records, once per heartbeat, what a complete inventory showed
// for the agents whose runtime state matters to start claims: agents that
// should be running but are not, agents holding an unconfirmed start claim,
// and agents running although their run intent is stopped. The start-claim
// reaper reads these observations, only when they are fresh (see
// observationFresh), to decide whether a start whose outcome is unknown
// left anything running.

// observedAgent is one agent the heartbeat listed.
type observedAgent struct {
	target string
	state  store.RecoveryObservedState
}

// heartbeatObservedState classifies a listed agent: present and terminal
// when it has an exit code, a terminal phase or a terminal container
// status; otherwise present and running. A pending or starting container is
// running, and so is one still running out its termination grace (for
// example a Kubernetes pod with a deletion timestamp, which may already
// carry an exit reason such as evicted).
func heartbeatObservedState(hb brokerAgentHeartbeat) store.RecoveryObservedState {
	if hb.ExitCode != nil {
		return store.ObservedPresentTerminal
	}
	switch state.Phase(hb.Phase) {
	case state.PhaseStopped, state.PhaseError, state.PhaseSuspended:
		return store.ObservedPresentTerminal
	}
	cs := strings.ToLower(strings.TrimSpace(hb.ContainerStatus))
	for _, p := range []string{"exited", "dead", "stopped", "completed", "succeeded", "failed", "terminated"} {
		if strings.HasPrefix(cs, p) {
			return store.ObservedPresentTerminal
		}
	}
	return store.ObservedPresentRunning
}

// startsInFlightKeys returns the (project, slug) keys of the starts the
// heartbeat listed in flight.
func (hb *brokerHeartbeatRequest) startsInFlightKeys() map[[2]string]bool {
	out := make(map[[2]string]bool, len(hb.StartsInFlight))
	for _, s := range hb.StartsInFlight {
		out[[2]string{s.ProjectID, s.Slug}] = true
	}
	return out
}

// needsRecoveryObservation reports whether a's runtime state is recorded:
// it should be running but is not; it holds an unconfirmed start claim; or
// it is running with run intent stopped and no claim.
func needsRecoveryObservation(a *store.Agent) bool {
	if !a.DeletedAt.IsZero() {
		return false
	}
	running := a.Phase == string(state.PhaseRunning)
	switch {
	case a.RunIntent == store.RunIntentRunning && !running:
		return true
	case a.StartClaimID != "" && a.StartClaimState == store.StartClaimUnconfirmed:
		return true
	case a.RunIntent == store.RunIntentStopped && a.StartClaimID == "" && running:
		return true
	}
	return false
}

// observationTarget is the target an agent's observation is keyed on: its
// recorded runtime target, or, for an agent with none yet, the target its
// start claim expects.
func observationTarget(a *store.Agent) string {
	if t := agentRuntimeTarget(a); t != "" {
		return t
	}
	return a.StartClaimTarget
}

// recordRecoveryObservations runs once per heartbeat, after the per-agent
// status loop, and only when the heartbeat is a complete inventory from a
// broker that was online a moment ago (inventoryAllowsReconcile). It writes
// every observation and each complete target's inventory time in one store
// transaction.
func (s *Server) recordRecoveryObservations(ctx context.Context, brokerID string, prev *store.RuntimeBroker, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	if !inventoryAllowsReconcile(prev, hb, s.missingAgents.now(), s.missingAgentGrace()) {
		return
	}
	complete := hb.completeTargets()
	targets := make([]string, 0, len(complete))
	for t := range complete {
		targets = append(targets, t)
	}

	agents, err := report.brokerAgents(ctx, s, brokerID)
	if err != nil {
		slog.Warn("Recovery observations: listing broker agents failed", "broker_id", brokerID, "error", err)
		return
	}
	// A queued create, start or restart dispatch (for a broker reached
	// through another hub node) may create a container at any moment:
	// record it as a start in flight.
	pending, err := s.pendingLifecycleAgents(ctx, brokerID)
	if err != nil {
		slog.Warn("Recovery observations: listing pending dispatches failed", "broker_id", brokerID, "error", err)
		return
	}
	inFlight := hb.startsInFlightKeys()
	var obs []store.RecoveryObservation
	for i := range agents {
		a := &agents[i]
		if !needsRecoveryObservation(a) {
			continue
		}
		o := store.RecoveryObservation{AgentID: a.ID, InFlight: inFlight[[2]string{a.ProjectID, a.Slug}] || pending[a.ID]}
		if seen, ok := report.observed[a.ID]; ok {
			o.Target, o.State = seen.target, seen.state
		} else {
			if report.unresolvedSlugs[a.Slug] {
				continue // a same-slug entry could not be matched: leave it alone
			}
			o.Target, o.State = observationTarget(a), store.ObservedAbsent
		}
		if o.Target == "" || !complete[o.Target] {
			continue
		}
		obs = append(obs, o)
	}
	if _, err := s.store.RecordRecoveryObservations(ctx, brokerID, targets, obs); err != nil {
		slog.Warn("Recovery observations: write failed", "broker_id", brokerID, "error", err)
	}
}

// listBrokerAgents returns every non-deleted agent assigned to brokerID,
// following pagination.
func (s *Server) listBrokerAgents(ctx context.Context, brokerID string) ([]store.Agent, error) {
	var out []store.Agent
	opts := store.ListOptions{SkipTotalCount: true}
	for {
		res, err := s.store.ListAgents(ctx, store.AgentFilter{RuntimeBrokerID: brokerID}, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.NextCursor == "" {
			return out, nil
		}
		opts.Cursor = res.NextCursor
	}
}

// observationFreshness is the heartbeat interval the freshness rule is
// measured in: brokers heartbeat every 30s by default.
const observationFreshness = 30 * time.Second

// observationFresh reports whether obs may be read as current, using the
// complete-inventory time of obs's target on its broker (inv):
//  1. obs was written by that inventory: observations and inventory times
//     are written together in one transaction with one store-clock value,
//     so an older row (an agent that has since left the observed set, or
//     an observation of an earlier inventory) does not match;
//  2. that inventory is at most two heartbeat intervals old at now.
//
// Reconnects are covered by rule 2 together with the complete-inventory
// gate (inventoryAllowsReconcile): the first heartbeat after a gap is not
// used, so no inventory time is refreshed until a complete inventory in the
// new period. now must be the store clock.
func observationFresh(obs store.RecoveryObservationRecord, inv map[string]time.Time, now time.Time) bool {
	if obs.Target == "" || obs.ObservedAt.IsZero() {
		return false
	}
	t, ok := inv[obs.Target]
	if !ok || !obs.ObservedAt.Equal(t) {
		return false
	}
	return now.Sub(t) <= 2*observationFreshness
}

// targetInventoryTimes maps a broker's targets to their last complete
// inventory time.
func targetInventoryTimes(rows []store.BrokerTargetInventory) map[string]time.Time {
	out := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		out[r.Target] = r.LastCompleteInventoryAt
	}
	return out
}
