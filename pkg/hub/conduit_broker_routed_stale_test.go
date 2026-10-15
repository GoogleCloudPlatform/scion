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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestRoutedBrokerRPC_StaleHeaderInRpcResponseIsBrokerAnswer: the
// stale-route header is honoured only on the owner relay's internal API
// response. A broker answer that carries it, with a 409 and a value of the
// closed set, is the broker's own answer, on a local and on a remote
// session alike: it is returned to the caller, the request is not
// re-resolved to the broker's other session, and nothing falls back.
func TestRoutedBrokerRPC_StaleHeaderInRpcResponseIsBrokerAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{"local session", false},
		{"remote session", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			a := w.StartNode("relay-a", nil)
			holder := a
			if tc.remote {
				holder = w.StartNode("relay-b", nil)
			}
			const brokerID = "broker-s"
			w.SetPrincipal("bs", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "proc-s"})
			hello := &conduitv1.Hello{
				PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
				PrincipalId:   brokerID,
				Capabilities:  &conduitv1.Capabilities{Rpc: conduit.BrokerRPCs(), EndpointIncarnation: "proc-s"},
			}
			var served atomic.Int32
			stale := conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				served.Add(1)
				env := envelope.RPCToRequest(req)
				return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusConflict,
					map[string]string{relay.HeaderStaleReason: string(registry.ReasonDraining)},
					[]byte(`{"error":{"code":"conflict","message":"broker says conflict"}}`)))
			})
			// Two sessions: if the header were honoured, the request would
			// be re-resolved to the second one.
			holder.MustDial("bs", hello, conduit.Config{RPCHandler: stale})
			holder.MustDial("bs", hello, conduit.Config{RPCHandler: stale})

			rt, err := router.New(router.Config{Relay: a.Relay, Registry: w.Registry, Store: w.Store, Peers: a.Peers, Now: w.Now, Brokers: noBrokerControlChannel{}})
			require.NoError(t, err)
			legacy := &mockControlChannelTunnel{connected: true}
			c := &HybridBrokerClient{
				controlChannel: &ControlChannelBrokerClient{manager: legacy, signer: &mockBrokerSigner{}},
				httpClient:     &mockRuntimeBrokerClient{},
			}
			c.SetConduitRouting(&routedBrokerTunnel{
				router:    func() brokerRPCRouter { return rt },
				enabled:   func() bool { return true },
				execScope: func(context.Context, string) (string, error) { return "", nil },
			})

			_, err = c.GetAgentLogs(context.Background(), brokerID, "", "agent-a", "p1", 0)
			var se *brokerStatusError
			require.ErrorAs(t, err, &se, "the broker's 409 is returned as its answer")
			assert.Equal(t, http.StatusConflict, se.StatusCode)
			assert.Equal(t, int32(1), served.Load(), "not re-resolved to the other session")
			assert.Equal(t, 0, legacy.calls, "no fallback")
		})
	}
}

// TestRoutedBrokerRPC_StaleHeaderNeverForwarded: the stale-route header
// in a broker's answer never reaches an API caller, on a success (logs)
// and on a failure (exec).
func TestRoutedBrokerRPC_StaleHeaderNeverForwarded(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	agent := f.seedRoutedAgent(t, nil)
	b := f.dialRoutedBroker(t, routedBrokerOpts{answer: func(ctx context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
		resp := defaultBrokerAnswer(ctx, op, env)
		if op == conduit.RPCAgentExec {
			resp = wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusConflict, nil,
				[]byte(`{"error":{"code":"conflict","message":"busy"}}`))
		}
		resp.Headers = map[string]string{relay.HeaderStaleReason: string(registry.ReasonDraining)}
		return resp
	}})

	for _, tc := range []struct {
		name, method, suffix string
		body                 interface{}
	}{
		{"logs", http.MethodGet, "/logs", nil},
		{"exec", http.MethodPost, "/exec", map[string]interface{}{"command": []string{"true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, f.srv, tc.method, "/api/v1/agents/"+agent.ID+tc.suffix, tc.body)
			for k := range rec.Header() {
				assert.False(t, strings.EqualFold(k, relay.HeaderStaleReason), "%s forwarded to the API caller (status %d)", k, rec.Code)
			}
			assert.NotContains(t, rec.Body.String(), relay.HeaderStaleReason)
		})
	}
	assert.Equal(t, 1, b.count(conduit.RPCAgentLogs))
	assert.Equal(t, 1, b.count(conduit.RPCAgentExec))
	assert.Equal(t, 0, f.legacy.calls)
}
