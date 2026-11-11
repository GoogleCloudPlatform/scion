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

// noBrokerControlChannel is a router.BrokerResolver for a node that holds
// no broker control channel.
type noBrokerControlChannel struct{}

func (noBrokerControlChannel) ResolveBroker(context.Context, router.Request) (router.Resolved, error) {
	return router.Resolved{}, router.ErrNoSession
}

// TestRoutedBrokerRPC_CrossNode (criterion 2, broker path): a hub node
// whose relay does not hold the broker's session reaches it through the
// owner relay, and the request runs once on the broker.
func TestRoutedBrokerRPC_CrossNode(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	const brokerID = "broker-x"
	w.SetPrincipal("bx", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "proc-x"})
	var served atomic.Int32
	b.MustDial("bx", &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: conduit.BrokerRPCs(), EndpointIncarnation: "proc-x"},
	}, conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		served.Add(1)
		env := envelope.RPCToRequest(req)
		return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, []byte(`{"output":"from relay-b","exitCode":0}`)))
	})})

	rt, err := router.New(router.Config{Relay: a.Relay, Registry: w.Registry, Store: w.Store, Peers: a.Peers, Now: w.Now, Brokers: noBrokerControlChannel{}})
	require.NoError(t, err)
	tun := &routedBrokerTunnel{
		router:    func() brokerRPCRouter { return rt },
		enabled:   func() bool { return true },
		execScope: func(context.Context, string) (string, error) { return "", nil },
	}
	c := &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: &mockControlChannelTunnel{connected: false}, signer: &mockBrokerSigner{}},
		httpClient:     &mockRuntimeBrokerClient{},
	}
	c.SetConduitRouting(tun)

	out, code, err := c.ExecAgent(context.Background(), brokerID, "", "agent-a", "p1", []string{"true"}, 5)
	require.NoError(t, err)
	assert.Equal(t, "from relay-b", out)
	assert.Equal(t, 0, code)
	assert.Equal(t, int32(1), served.Load())
}
