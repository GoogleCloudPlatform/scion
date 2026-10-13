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
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// failingSigner always fails, for testing that a signing failure — proven to
// occur before anything is sent — classifies as agentkeys.ErrNotDispatched.
type failingSigner struct{}

// pendingDialKeysClient returns an HTTPRuntimeBrokerClient whose keys
// transport dials through a stub that never connects: each dial blocks until
// the test ends. This reproduces a broker endpoint that is not reachable
// (for example a broker that only connects out over the control channel)
// without depending on the sandbox's network behaviour. The returned counter
// reports how many dials were started.
func pendingDialKeysClient(t *testing.T, clientTimeout time.Duration) (*HTTPRuntimeBrokerClient, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var dials atomic.Int32
	rt := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("stub dial abandoned")
		},
	}
	t.Cleanup(func() {
		close(release)
		rt.CloseIdleConnections()
	})
	client := NewHTTPRuntimeBrokerClient()
	// Wrap the stub the same way newBrokerHTTPTransport wraps
	// http.DefaultTransport, so the trace context goes through otelhttp
	// exactly as it does in production.
	client.transport.keysClient.Transport = otelhttp.NewTransport(rt)
	client.transport.keysClient.Timeout = clientTimeout
	return client, &dials
}

func (failingSigner) Sign(context.Context, *http.Request, string) error {
	return errors.New("boom: no broker secret")
}
