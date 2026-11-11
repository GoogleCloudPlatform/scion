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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
)

// TestRoutedBrokerRPC_SignedAttemptPassesBrokerGuard: a routed request,
// signed by the hub for its session (signForSession with the broker's
// stored secret), is served by the broker's production conduit RPC
// handler and its guard; the same request unsigned is refused there.
func TestRoutedBrokerRPC_SignedAttemptPassesBrokerGuard(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	var mu sync.Mutex
	var hits []string
	tree := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("guarded logs"))
	})
	secret := append([]byte(nil), f.secret...)
	f.dialRoutedBroker(t, routedBrokerOpts{
		handler: runtimebroker.NewConduitBrokerRPCHandler(tree, "hub-a", f.brokerID, func() []byte { return secret }, nil),
	})

	out, err := f.client.GetAgentLogs(context.Background(), f.brokerID, "", "agent-a", "p1", 0)
	require.NoError(t, err)
	assert.Equal(t, "guarded logs", out)
	mu.Lock()
	assert.Equal(t, []string{"GET /api/v1/agents/agent-a/logs"}, hits, "served through the broker guard")
	mu.Unlock()
	assert.Equal(t, 0, f.legacy.calls)

	unsigned := f.srv.newRoutedBrokerTunnel()
	unsigned.signer = nil
	resp, err := unsigned.TunnelRequest(withBrokerRPC(context.Background(), conduit.RPCAgentLogs, true), f.brokerID,
		wsprotocol.NewRequestEnvelope("u-1", http.MethodGet, "/api/v1/agents/agent-a/logs", "", nil, nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the broker guard refuses an unsigned request")
	mu.Lock()
	assert.Len(t, hits, 1, "the unsigned request reached no handler")
	mu.Unlock()
}
