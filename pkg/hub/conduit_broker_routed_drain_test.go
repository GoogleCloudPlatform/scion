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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestRoutedBrokerRPC_DrainingSessionReplacedOnSameRelay: while a broker
// session drains (it sent GoAway), the request in flight on it completes
// within the drain deadline, and a new request goes to the broker's
// replacement session on the same relay. The draining session's clock is
// fake and never advanced, so the drain deadline cannot pass during the
// test; nothing sleeps.
func TestRoutedBrokerRPC_DrainingSessionReplacedOnSameRelay(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	const brokerID = "broker-d"
	w.SetPrincipal("bd", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "proc-d"})
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: conduit.BrokerRPCs(), EndpointIncarnation: "proc-d"},
	}
	answer := func(served *atomic.Int32, body string, hold <-chan struct{}, entered chan<- struct{}) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			served.Add(1)
			if hold != nil {
				entered <- struct{}{}
				select {
				case <-hold:
				case <-ctx.Done():
					return nil
				}
			}
			env := envelope.RPCToRequest(req)
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, []byte(body)))
		})
	}

	// The first session holds the first request until released.
	var firstServed, secondServed atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	drainClock := clock.NewFake(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	first, _ := a.MustDial("bd", hello, conduit.Config{Clock: drainClock, RPCHandler: answer(&firstServed, "first", release, entered)})

	rt, err := router.New(router.Config{Relay: a.Relay, Registry: w.Registry, Store: w.Store, Peers: a.Peers, Now: w.Now, Brokers: noBrokerControlChannel{}})
	require.NoError(t, err)
	tun := &routedBrokerTunnel{
		router:    func() brokerRPCRouter { return rt },
		enabled:   func() bool { return true },
		execScope: func(context.Context, string) (string, error) { return "", nil },
	}
	call := func(id string) (*wsprotocol.ResponseEnvelope, error) {
		return tun.TunnelRequest(withBrokerRPC(context.Background(), conduit.RPCAgentLogs, true), brokerID,
			wsprotocol.NewRequestEnvelope(id, http.MethodGet, "/api/v1/agents/a/logs", "", nil, nil))
	}

	type result struct {
		resp *wsprotocol.ResponseEnvelope
		err  error
	}
	inFlight := make(chan result, 1)
	go func() {
		resp, err := call("in-flight")
		inFlight <- result{resp, err}
	}()
	waitSignal(t, entered, "the first request reached the first session")

	// The first session drains; its replacement connects to the same relay.
	require.NoError(t, first.GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute}))
	a.MustDial("bd", hello, conduit.Config{Clock: clock.Real(), RPCHandler: answer(&secondServed, "second", nil, nil)})

	resp, err := call("new")
	require.NoError(t, err)
	assert.Equal(t, "second", string(resp.Body), "the new request went to the replacement session")
	assert.Equal(t, int32(1), secondServed.Load())

	select {
	case <-first.Done():
		t.Fatalf("the draining session ended before its in-flight request completed: %v", first.Err())
	default:
	}
	close(release)
	r := <-inFlight
	require.NoError(t, r.err)
	assert.Equal(t, "first", string(r.resp.Body), "the in-flight request completed on the draining session")
	assert.Equal(t, int32(1), firstServed.Load(), "the draining session served only its in-flight request")
}
