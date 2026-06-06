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

// ErrMessageDeferred is returned by HybridBrokerClient.MessageAgent when the
// broker is not locally connected and has no HTTP endpoint: the message row
// (already persisted with dispatch_state=pending) is the durable intent, and
// the caller should emit a NOTIFY wakeup and return 202 Accepted.
var ErrMessageDeferred = errors.New("message deferred: broker not locally reachable")

// ErrLifecycleDeferred is returned by HybridBrokerClient.StartAgent/StopAgent/
// RestartAgent when the broker is not locally connected and has no HTTP
// endpoint. The caller should serialize resolved params into a broker_dispatch
// row, signal the owning node via the command bus, and wait for the resulting
// agent status transition (design §5.4, §6.2).
var ErrLifecycleDeferred = errors.New("lifecycle deferred: broker not locally reachable")

// routeDecision is the outcome of HybridBrokerClient.route — how a dispatch for a
// broker should be delivered when this node does not hold the broker's socket.
type routeDecision int

const (
	// routeLocal: this node holds the broker's control-channel socket — tunnel
	// directly (the unchanged, zero-added-latency fast path).
	routeLocal routeDecision = iota
	// routeForward: some other node is believed to own the broker (affinity hint
	// is alive) — write durable intent + NOTIFY and let the owner self-select.
	routeForward
	// routeHTTP: no live owner, but the broker exposes a direct HTTP endpoint
	// (direct-mode broker; existing fallback — rare under NAT'd deployments).
	routeHTTP
	// routeUndeliverable: no owner and no endpoint — write durable pending intent
	// and return a retryable status; reconciled on the broker's next reconnect.
	routeUndeliverable
)

func (d routeDecision) String() string {
	switch d {
	case routeLocal:
		return "local"
	case routeForward:
		return "forward"
	case routeHTTP:
		return "http"
	default:
		return "undeliverable"
	}
}

// defaultAffinityFreshness bounds how long a broker's last_heartbeat is trusted
// as "owner alive" for routing. Generous (a multiple of the heartbeat interval);
// a stale hint only costs one dispatch timeout before falling through, and the
// reaper (B5-1) clears dead owners.
const defaultAffinityFreshness = 90 * time.Second

// route decides how to deliver a dispatch for brokerID. The local fast path is
// checked first and unchanged; affinity is consulted only to choose between
// forwarding (durable intent + signal) and fast-failing (design §5.3). The
// affinity lookup is a hint — a wrong "alive" costs one timeout (intent stays
// durable and reconciles later); a wrong "dead" is reaped by §7.1.
func (c *HybridBrokerClient) route(ctx context.Context, brokerID, brokerEndpoint string) routeDecision {
	if c.controlChannel.manager.IsConnected(brokerID) {
		return routeLocal
	}
	var owner string
	var alive bool
	if c.affinity != nil {
		owner, alive = c.affinity(ctx, brokerID)
	}
	switch {
	case owner != "" && alive:
		return routeForward
	case brokerEndpoint != "":
		return routeHTTP
	default:
		return routeUndeliverable
	}
}

// SetAffinityLookup injects the affinity hint used by route(). Wired by the
// server to a store-backed lookup (StoreAffinityLookup).
func (c *HybridBrokerClient) SetAffinityLookup(fn func(ctx context.Context, brokerID string) (owner string, alive bool)) {
	c.affinity = fn
}

// StoreAffinityLookup returns an affinity lookup backed by runtime_brokers: the
// owner is connected_hub_id, and "alive" means last_heartbeat is within
// freshness. (Liveness is inferred from heartbeat freshness because there is no
// hub-to-hub addressability to ping a peer — design §5.3.)
func StoreAffinityLookup(st store.Store, freshness time.Duration) func(ctx context.Context, brokerID string) (string, bool) {
	if freshness <= 0 {
		freshness = defaultAffinityFreshness
	}
	return func(ctx context.Context, brokerID string) (string, bool) {
		b, err := st.GetRuntimeBroker(ctx, brokerID)
		if err != nil || b == nil || b.ConnectedHubID == nil || *b.ConnectedHubID == "" {
			return "", false
		}
		alive := !b.LastHeartbeat.IsZero() && time.Since(b.LastHeartbeat) < freshness
		return *b.ConnectedHubID, alive
	}
}
