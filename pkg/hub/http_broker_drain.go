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
// broker once its heartbeat shows it online again. At most one drain per
// broker runs at a time on this node; the dispatch claim keeps replicas from
// applying a row twice. Only stop rows are drained: they are the only rows
// queued for such a broker (any other op is delivered over HTTP directly).
func (s *Server) drainQueuedStopsFromHeartbeat(brokerID string, hb *brokerHeartbeatRequest) {
	if hb.Status != store.BrokerStatusOnline || s.GetDispatcher() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpDrainTimeout)
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil || !s.brokerHasNoControlChannel(broker) {
		cancel()
		return
	}
	if _, running := s.httpDrains.LoadOrStore(brokerID, struct{}{}); running {
		cancel()
		return
	}
	go func() {
		defer cancel()
		defer s.httpDrains.Delete(brokerID)
		s.drainBrokerDispatch(ctx, brokerID, func(op string) bool { return op == "stop" })
	}()
}

// settleQueuedStops confirms queued stops whose container is gone: an agent
// with a stop queued (container status stop_queued) that a fresh complete
// inventory of its target does not list has terminated, so its broker
// reservation is released and the queued-stop notice cleared. Capacity is
// released only on such confirmation or on the stop result itself (the
// drain), never because the stop was merely queued or drained by the
// message alone. The queued row is left for the drain, which then applies a
// harmless stop.
func (s *Server) settleQueuedStops(ctx context.Context, brokerID string, prev *store.RuntimeBroker, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	if !inventoryAllowsReconcile(prev, hb, s.missingAgents.now(), s.missingAgentGrace()) {
		return
	}
	complete := hb.completeTargets()
	agents, err := report.brokerAgents(ctx, s, brokerID)
	if err != nil {
		return
	}
	for i := range agents {
		a := &agents[i]
		if a.ContainerStatus != containerStatusStopQueued || report.present[a.ID] || report.unresolvedSlugs[a.Slug] {
			continue
		}
		if t := agentRuntimeTarget(a); t == "" || !complete[t] {
			continue
		}
		s.releaseBrokerQuota(ctx, a)
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
