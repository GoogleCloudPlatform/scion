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
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestRoutedBrokerRPC_InFlightEndedByCredentialChange: a routed broker
// request in flight when the broker is deregistered, its secret rotated
// or revoked ends with the broker session's 4401 close: the exec gets
// outcome_unknown and the broker's late result is never delivered. The
// next request does not reach the broker over conduit. Without the close
// the in-flight exec would return the broker's result, and the test would
// fail.
func TestRoutedBrokerRPC_InFlightEndedByCredentialChange(t *testing.T) {
	for _, tc := range brokerChanges() {
		if tc.name == "re-join" {
			continue // the three changes of the 3.3 gate: deregister, rotate, revoke
		}
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerRevokeFixture(t, nil)
			f.srv.SetDispatcher(f.srv.CreateAuthenticatedDispatcher())
			client := f.srv.brokerClient
			require.NotNil(t, client)
			legacy := &mockControlChannelTunnel{connected: true, body: []byte(`{"output":"legacy","exitCode":0}`)}
			client.controlChannel = &ControlChannelBrokerClient{manager: legacy, signer: &mockBrokerSigner{}}

			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			var execs atomic.Int32
			handler := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				env := envelope.RPCToRequest(req)
				execs.Add(1)
				entered <- struct{}{}
				<-release // the late result, after the credential change
				return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil,
					[]byte(`{"output":"late result","exitCode":0}`)))
			})
			ls := dialRoutedBrokerSettled(t, f, handler)

			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, _, err := client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"touch", "x"}, 5)
				done <- result{out, err}
			}()
			waitSignal(t, entered, "the exec reached the broker over conduit")

			tc.viaHub(t, f.brokerConduitFixture, f.srv)
			assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls), "the broker session closes with 4401")

			var r result
			select {
			case r = <-done:
			case <-time.After(brokerRevokeCloseBound):
				close(release)
				t.Fatal("the in-flight exec did not end with the session")
			}
			close(release)
			var ou *brokerOutcomeUnknownError
			require.ErrorAs(t, r.err, &ou, "the in-flight exec ends as outcome unknown")
			assert.NotEqual(t, "late result", r.out)

			// The next request does not reach the broker over conduit.
			out, _, err := client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
			require.NoError(t, err)
			assert.Equal(t, "legacy", out)
			assert.Equal(t, 1, legacy.calls)
			assert.Equal(t, int32(1), execs.Load(), "no request reached the broker over conduit after the change")
		})
	}
}

// dialRoutedBrokerSettled opens the fixture broker's conduit session
// listing every broker RPC, served by handler, and returns once the hub
// has finished setting it up (dialSettled's rule).
func dialRoutedBrokerSettled(t *testing.T, f *brokerRevokeFixture, handler conduit.RPCHandler) conduit.LocalSession {
	t.Helper()
	before, _ := f.faults.secretReadsOf(f.brokerID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL:    f.wsURL("/api/v1/conduit"),
		Header: func(context.Context) (http.Header, error) { return f.signedHeaderAs(t, f.brokerID, f.secret), nil },
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   f.brokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: conduit.BrokerRPCs(), EndpointIncarnation: "proc-1"},
	}
	s, _, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real(), RPCHandler: handler}, hello)
	require.NoError(t, err)
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	deadline := time.After(10 * time.Second)
	for {
		n, changed := f.faults.secretReadsOf(f.brokerID)
		if n >= before+2 {
			return ls
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("broker session setup did not complete")
		}
	}
}
