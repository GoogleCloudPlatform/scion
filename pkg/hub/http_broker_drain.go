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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A stop for an agent whose broker is offline is queued as a broker_dispatch
// row and applied by the drain when the broker reconnects. The drain runs on
// the node that holds the broker's control channel. A broker without a
// control channel (reached over HTTP only) never reconnects one, so its
// queued stops are applied from its heartbeat instead.

// httpDrainTimeout bounds one heartbeat-triggered drain.
const httpDrainTimeout = 2 * time.Minute

// brokerHasNoControlChannel reports whether broker is reached over HTTP
// only: no hub node holds a control channel to it, and it has an endpoint.
func (s *Server) brokerHasNoControlChannel(broker *store.RuntimeBroker) bool {
	if broker == nil || broker.Endpoint == "" || (broker.ConnectedHubID != nil && *broker.ConnectedHubID != "") {
		return false
	}
	return s.controlChannel == nil || !s.controlChannel.IsConnected(broker.ID)
}

// drainQueuedStopsFromHeartbeat applies the queued stops of an HTTP-only
// broker once its heartbeat shows it online again. broker is the broker row
// the heartbeat already read; nothing runs unless this heartbeat's pending
// dispatch read shows a queued stop. At most one drain per broker runs at a
// time on this node; the dispatch claim keeps replicas from applying a row
// twice. Only stop rows are drained: they are the only rows queued for such
// a broker (any other op is delivered over HTTP directly).
func (s *Server) drainQueuedStopsFromHeartbeat(ctx context.Context, brokerID string, broker *store.RuntimeBroker, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	if hb.Status != store.BrokerStatusOnline || s.GetDispatcher() == nil || !s.brokerHasNoControlChannel(broker) {
		return
	}
	if stops, err := report.pendingStops(ctx, s, brokerID); err != nil || len(stops) == 0 {
		return
	}
	s.startHTTPStopDrain(brokerID)
}

// startHTTPStopDrain starts the stop-only drain for an HTTP-only broker,
// unless one is already running for it on this node. The drain is tracked
// broker work (brokerWorkTracker): shutdown joins it before the store
// closes, and once shutdown has begun it is not started, leaving the rows
// pending for a later heartbeat (ptone/scion#4869).
func (s *Server) startHTTPStopDrain(brokerID string) {
	if _, running := s.httpDrains.LoadOrStore(brokerID, struct{}{}); running {
		return
	}
	started := s.brokerWork.goTracked(func(workCtx context.Context) {
		dctx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
		defer cancel()
		stop := context.AfterFunc(workCtx, cancel)
		defer stop()
		defer s.httpDrains.Delete(brokerID)
		s.drainBrokerDispatch(dctx, brokerID, func(op string) bool { return op == "stop" })
	})
	if !started {
		s.httpDrains.Delete(brokerID)
	}
}

// settleQueuedStops confirms queued stops whose container is gone: an agent
// with a stop queued (container status stop_queued) that a fresh complete
// inventory of its target does not list, or lists as terminal, has
// terminated, so its broker reservation is released and the queued-stop
// notice cleared. It acts only while the queued stop is still the agent's
// intent: run intent stopped, no start claim, no lifecycle operation on this
// node, and no start in flight on the broker or queued for it; the row is
// re-read just before the release. Capacity is released only on such
// confirmation or on the stop result itself (the drain), never because the
// stop was merely queued. A queued row left pending is applied later by the
// drain (on the broker's reconnect, or from its heartbeat when it has no
// control channel) as a stop of an agent already stopped.
func (s *Server) settleQueuedStops(ctx context.Context, brokerID string, prev *store.RuntimeBroker, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	if !inventoryAllowsReconcile(prev, hb, s.missingAgents.now(), s.missingAgentGrace()) {
		return
	}
	complete := hb.completeTargets()
	agents, err := report.brokerAgents(ctx, s, brokerID)
	if err != nil {
		return
	}
	var inFlight map[[2]string]bool
	var pending map[string]bool
	for i := range agents {
		a := &agents[i]
		if !queuedStopStillIntended(a) || report.unresolvedSlugs[a.Slug] {
			continue
		}
		if report.present[a.ID] && report.observed[a.ID].state != store.ObservedPresentTerminal {
			continue // listed, and not terminal: the stop has not taken effect
		}
		if t := settleTarget(a, brokerID, hb); t == "" || !complete[t] {
			continue
		}
		if s.lifecycleOps.active(a.ID) {
			continue
		}
		if inFlight == nil {
			inFlight = hb.startsInFlightKeys()
			if pending, err = report.pendingStarts(ctx, s, brokerID); err != nil {
				return
			}
		}
		if inFlight[[2]string{a.ProjectID, a.Slug}] || pending[a.ID] {
			continue
		}
		cur, err := s.store.GetAgent(ctx, a.ID)
		if err != nil || !queuedStopStillIntended(cur) || !sameTime(cur.RunIntentAt, a.RunIntentAt) {
			continue
		}
		// One conditional store write: the reservation is released only if
		// the run is the one just read and the queued stop is still the
		// intent at the same RunIntentAt, so a start that claimed the agent
		// after the re-read keeps the slot.
		released, err := s.releaseBrokerQuotaIf(ctx, cur, store.AgentReleaseCond{
			ExpectedRunID:         cur.RunID,
			QueuedStopIntent:      true,
			QueuedStopRunIntentAt: a.RunIntentAt,
		})
		if err != nil {
			// A store failure, not a condition that did not hold: nothing
			// was released. Leave the stop queued (no confirmation), so the
			// next heartbeat settles it again.
			continue
		}
		// Read once more: if the queued stop is no longer the intent, a
		// start superseded it. Its slot was not freed by the conditional
		// release above, so the re-assert below is a backstop that writes
		// only if a release slipped through an unguarded path (counted,
		// logged and audited, site "settle"). Without the cap check, as
		// with a restart, a start that took a slipped slot can leave the
		// broker one over its cap until a slot frees.
		after, err := s.store.GetAgent(ctx, a.ID)
		if err != nil {
			// Unknown: put the reservation back (idempotent). If no start
			// took the agent, the next settle releases it again.
			s.reassertBrokerReservationAt(ctx, a, backstopSiteSettle)
			s.agentLifecycleLog.Warn("Queued stop: re-reading the agent after the release failed; reservation put back", "agent_id", a.ID, "error", err)
			continue
		}
		if !queuedStopStillIntended(after) || !sameTime(after.RunIntentAt, a.RunIntentAt) {
			if queuedStopAppliedElsewhere(after, a.RunIntentAt) {
				// The drain is applying, or has applied, this same queued
				// stop: the stop result releases (or released) the slot.
				// A re-assert would count a container that is gone.
				s.agentLifecycleLog.Info("Queued stop: the stop result settled it during the release; nothing re-asserted", "agent_id", a.ID)
				continue
			}
			if released && after.RunIntent == store.RunIntentRunning {
				// The release was proven safe on the locked row (no start
				// held the slot), and a start superseded the stop after it.
				// That start reserves with the cap check; a re-assert here
				// would hand it an unchecked slot at a full instance.
				s.agentLifecycleLog.Info("Queued stop: a start superseded it after the release; the start's own reserve decides", "agent_id", a.ID)
				continue
			}
			s.reassertBrokerReservationAt(ctx, after, backstopSiteSettle)
			s.agentLifecycleLog.Info("Queued stop: a start superseded it during the release; reservation kept", "agent_id", a.ID)
			continue
		}
		if err := s.store.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			ContainerStatus: "stopped",
			ClearMessageIf:  offlineStopMessage,
		}); err != nil {
			s.agentLifecycleLog.Warn("Queued stop: confirming termination failed", "agent_id", a.ID, "error", err)
			continue
		}
		s.agentLifecycleLog.Info("Queued stop: container confirmed gone", "agent_id", a.ID, "capacity_released", released)
	}
}

// queuedStopStillIntended reports whether a's queued stop is still what the
// agent should do: container status stop_queued, run intent stopped, no
// start claim, not deleted. See store.QueuedStopStillIntended, which the
// conditional release evaluates on the locked row.
func queuedStopStillIntended(a *store.Agent) bool {
	return store.QueuedStopStillIntended(a)
}

// queuedStopAppliedElsewhere reports whether after shows the queued stop
// recorded at intentAt being applied, or applied, by the drain rather than
// superseded: run intent is still stopped at exactly intentAt, no delete
// holds or removed the row, and either the drain's stop-kind claim is held
// or the container status is already stopped. The same intent with no claim
// and a container not recorded stopped keeps the backstop re-assert, so a
// container that may still run is not left uncounted.
func queuedStopAppliedElsewhere(after *store.Agent, intentAt *time.Time) bool {
	return after.RunIntent == store.RunIntentStopped && sameTime(after.RunIntentAt, intentAt) &&
		after.DeletedAt.IsZero() && !deletionActive(after) &&
		((after.StartClaimID != "" && after.StartClaimKind == store.StartClaimStop) || after.ContainerStatus == "stopped")
}

// settleTarget is the inventory target a queued stop of a is settled on:
// its recorded runtime target. A pinned agent whose target is not recorded
// yet (it is recorded once two heartbeats confirm it) uses the single
// target of a report from its own Runtime Broker: a flat instance reports
// exactly one target. The pin's target ID is the registration descriptor's
// ID and the inventory's target keys are runtime target keys (for example
// "docker" or "kubernetes|context=...|namespace=..."), so the two are not
// compared: the report's broker ID is the agent's own, which is what ties
// the target to the agent. Otherwise "" (not settled).
func settleTarget(a *store.Agent, brokerID string, hb *brokerHeartbeatRequest) string {
	if t := agentRuntimeTarget(a); t != "" {
		return t
	}
	if !a.IsPinned() || a.RuntimeBrokerID != brokerID || hb.Inventory == nil {
		return ""
	}
	only := ""
	for _, t := range hb.Inventory.Targets {
		if t.ID == "" || t.ID == only {
			continue
		}
		if only != "" {
			return "" // more than one target
		}
		only = t.ID
	}
	return only
}

// sameTime reports whether two optional times are both unset or equal.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
