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
	if _, running := s.httpDrains.LoadOrStore(brokerID, struct{}{}); running {
		return
	}
	go func() {
		dctx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
		defer cancel()
		defer s.httpDrains.Delete(brokerID)
		s.drainBrokerDispatch(dctx, brokerID, func(op string) bool { return op == "stop" })
	}()
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
		if t := agentRuntimeTarget(a); t == "" || !complete[t] {
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
		s.releaseBrokerQuota(ctx, a)
		// A start can take the agent between the re-read and the release;
		// its own reserve was a no-op on the reservation just released.
		// Read once more: if the queued stop is no longer the intent, put
		// the reservation back without the cap check. The slot was this
		// agent's when the release ran; as with a restart, a start that
		// took it in between can leave the broker one over its cap until a
		// slot frees.
		after, err := s.store.GetAgent(ctx, a.ID)
		if err != nil {
			// Unknown: put the reservation back (idempotent). If no start
			// took the agent, the next settle releases it again.
			s.reassertBrokerReservation(ctx, a)
			s.agentLifecycleLog.Warn("Queued stop: re-reading the agent after the release failed; reservation put back", "agent_id", a.ID, "error", err)
			continue
		}
		if !queuedStopStillIntended(after) || !sameTime(after.RunIntentAt, a.RunIntentAt) {
			s.reassertBrokerReservation(ctx, after)
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
		s.agentLifecycleLog.Info("Queued stop: container confirmed gone; capacity released", "agent_id", a.ID)
	}
}

// queuedStopStillIntended reports whether a's queued stop is still what the
// agent should do: container status stop_queued, run intent stopped, no
// start claim, not deleted.
func queuedStopStillIntended(a *store.Agent) bool {
	return a.ContainerStatus == containerStatusStopQueued && a.RunIntent == store.RunIntentStopped &&
		a.StartClaimID == "" && a.DeletedAt.IsZero()
}

// sameTime reports whether two optional times are both unset or equal.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
