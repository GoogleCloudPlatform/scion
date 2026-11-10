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

package relay

import (
	"context"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
)

// The Cloud Run singleton relay (design §3.9 "Cloud Run singleton",
// v2.21.54 and v2.21.55): a non-HA relay whose internal endpoint is a
// platform service URL. It registers the relay feature singleton. Its
// startup self-check is retried, since during a rollout the service URL
// still routes to the old instance. Registration never refuses because of
// another row; instead, every heartbeat counts the other live rows that
// share its internal endpoint (the gauge scion.relay.singleton_endpoint_peers)
// and logs WARN when that count goes from 0 to non-zero. Correctness rests
// on 409 relay_target_mismatch.

// SingletonSelfCheckBound bounds a singleton's startup self-check retries.
const SingletonSelfCheckBound = 60 * time.Second

// singletonSelfCheckRetry is the pause between a singleton's self-check
// attempts.
const singletonSelfCheckRetry = 2 * time.Second

// relayFeatures is the relay's relay_instances.features.
func (r *Relay) relayFeatures() []string {
	if r.cfg.Singleton {
		return []string{registry.RelayFeatureSingleton}
	}
	return nil
}

// selfCheck runs the startup self-check of endpoint. A singleton (already
// listening) repeats it on the relay clock until it answers itself, for
// at most SingletonSelfCheckBound, and returns the last failure when the
// bound expires; any other relay checks once.
func (r *Relay) selfCheck(ctx context.Context, endpoint string) error {
	err := registry.SelfCheck(ctx, endpoint, r.cfg.InstanceID, r.probe)
	if err == nil || !r.cfg.Singleton {
		return err
	}
	deadline := r.clk.Now().Add(SingletonSelfCheckBound)
	for attempt := 2; ; attempt++ {
		if !r.clk.Now().Before(deadline) {
			return fmt.Errorf("singleton self-check did not answer this relay within %s: %w", SingletonSelfCheckBound, err)
		}
		r.log.Info("Conduit relay: singleton self-check not answered by this relay yet; retrying", "attempt", attempt, "error", err)
		wait, stop := clock.After(r.clk, min(singletonSelfCheckRetry, deadline.Sub(r.clk.Now())))
		select {
		case <-wait:
		case <-ctx.Done():
			stop()
			return ctx.Err()
		}
		if err = registry.SelfCheck(ctx, endpoint, r.cfg.InstanceID, r.probe); err == nil {
			return nil
		}
	}
}

// checkSingletonPeers counts the other rows with this relay's internal
// endpoint that are live and not draining, stores the count
// (SingletonEndpointPeers), and logs WARN with their instance ids when the
// count goes from 0 to non-zero. A failed read keeps the last count.
func (r *Relay) checkSingletonPeers(ctx context.Context) {
	r.mu.Lock()
	endpoint := r.endpoint
	r.mu.Unlock()
	if endpoint == "" {
		return
	}
	live, err := r.cfg.Registry.LiveRelays(ctx, r.cfg.Registry.Now())
	if err != nil {
		r.log.Warn("Conduit relay: reading relays for the singleton shared-endpoint check failed", "error", err)
		return
	}
	var peers []string
	for _, ri := range live {
		if ri.InstanceID != r.cfg.InstanceID && ri.InternalEndpoint == endpoint && !ri.Draining {
			peers = append(peers, ri.InstanceID)
		}
	}
	if prev := r.singletonPeers.Swap(int64(len(peers))); prev == 0 && len(peers) > 0 {
		r.log.Warn("Conduit relay: another live relay shares this singleton's internal endpoint; calls that reach it get 409 relay_target_mismatch and re-resolve",
			"internal_endpoint", endpoint, "instance_id", r.cfg.InstanceID, "other_instance_ids", peers)
	}
}

// SingletonEndpointPeers returns how many other live, non-draining relays
// shared this singleton's internal endpoint at the last check (0 for a
// relay that is not a singleton).
func (r *Relay) SingletonEndpointPeers() int64 { return r.singletonPeers.Load() }
