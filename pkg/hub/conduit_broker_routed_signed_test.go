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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestRoutedBrokerRPC_SignedForTheSession: each routed attempt carries the
// hub's signature and names, in a signed header, the session it is sent
// on; a retry on another session is signed again for that session. The
// two sessions are on two relays, each current on its own, so the retry
// reaches the other session as soon as the first loses its result.
func TestRoutedBrokerRPC_SignedForTheSession(t *testing.T) {
	f, hubB := newTwoRelayRoutedFixture(t)
	var mu sync.Mutex
	seen := map[*routedTestBroker][]map[string]string{}
	record := func(b **routedTestBroker) func(context.Context, string, *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
		return func(ctx context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
			mu.Lock()
			seen[*b] = append(seen[*b], env.Headers)
			mu.Unlock()
			return defaultBrokerAnswer(ctx, op, env)
		}
	}
	var lost atomic.Bool
	var p1, p2 atomic.Pointer[conduit.LocalSession]
	dropOnce := func(p *atomic.Pointer[conduit.LocalSession]) conduit.Interceptor {
		drop := dropResponseThenClose(p)
		return func(dir conduit.Direction, fr *conduitv1.Frame) []*conduitv1.Frame {
			if dir == conduit.Outbound && fr.GetRpcResponse() != nil && lost.CompareAndSwap(false, true) {
				return drop(dir, fr)
			}
			return []*conduitv1.Frame{fr}
		}
	}
	var b1, b2 *routedTestBroker
	b1 = f.dialRoutedBroker(t, routedBrokerOpts{answer: record(&b1), interceptor: dropOnce(&p1)})
	p1.Store(&b1.ls)
	b2 = f.dialRoutedBrokerOn(t, hubB, routedBrokerOpts{answer: record(&b2), interceptor: dropOnce(&p2)})
	p2.Store(&b2.ls)

	require.NoError(t, f.client.CleanupProject(context.Background(), f.brokerID, "", "proj-a", "p1"))
	mu.Lock()
	defer mu.Unlock()
	for _, b := range []*routedTestBroker{b1, b2} {
		require.Len(t, seen[b], 1, "one attempt per session")
		h := http.Header{}
		for k, v := range seen[b][0] {
			h.Set(k, v)
		}
		assert.Equal(t, b.sessionID, h.Get(conduit.BrokerRPCSessionHeader), "signed for the receiving session")
		assert.True(t, strings.EqualFold(h.Get(apiclient.HeaderSignedHeaders), conduit.BrokerRPCSessionHeader), "the session header is signed")
		assert.Equal(t, f.brokerID, h.Get(apiclient.HeaderBrokerID))
		assert.NotEmpty(t, h.Get(apiclient.HeaderSignature))
		assert.NotEmpty(t, h.Get(apiclient.HeaderNonce))
	}
	assert.NotEqual(t, seen[b1][0][apiclient.HeaderSignature], seen[b2][0][apiclient.HeaderSignature], "the retry is signed afresh")
}
