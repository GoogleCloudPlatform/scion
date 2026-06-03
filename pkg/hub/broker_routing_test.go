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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHybridBrokerClient_Route(t *testing.T) {
	ctx := context.Background()
	const localBroker = "broker-local"

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	// Seed a live local socket for localBroker only.
	mgr.mu.Lock()
	mgr.connections[localBroker] = &BrokerConnection{brokerID: localBroker, sessionID: "s1"}
	mgr.mu.Unlock()

	c := NewHybridBrokerClient(mgr, nil, nil, false)

	cases := []struct {
		name     string
		brokerID string
		endpoint string
		affOwner string
		affAlive bool
		want     routeDecision
	}{
		{"local socket wins", localBroker, "", "", false, routeLocal},
		{"local wins even over alive affinity", localBroker, "http://x", "hubA", true, routeLocal},
		{"alive owner -> forward", "b1", "", "hubA", true, routeForward},
		{"alive owner -> forward (endpoint ignored)", "b1", "http://x", "hubA", true, routeForward},
		{"no owner, endpoint set -> http", "b2", "http://x", "", false, routeHTTP},
		{"stale owner, endpoint set -> http", "b3", "http://x", "hubA", false, routeHTTP},
		{"stale owner, no endpoint -> undeliverable", "b4", "", "hubA", false, routeUndeliverable},
		{"no owner, no endpoint -> undeliverable", "b5", "", "", false, routeUndeliverable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.SetAffinityLookup(func(context.Context, string) (string, bool) { return tc.affOwner, tc.affAlive })
			got := c.route(ctx, tc.brokerID, tc.endpoint)
			assert.Equal(t, tc.want, got, "route(%s, endpoint=%q, owner=%q alive=%v)", tc.brokerID, tc.endpoint, tc.affOwner, tc.affAlive)
		})
	}
}

func TestHybridBrokerClient_Route_NilAffinityIsSafe(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, nil, nil, false)
	// No affinity lookup set: a non-local broker with no endpoint is undeliverable.
	assert.Equal(t, routeUndeliverable, c.route(context.Background(), "b-none", ""))
	assert.Equal(t, routeHTTP, c.route(context.Background(), "b-ep", "http://x"))
}
