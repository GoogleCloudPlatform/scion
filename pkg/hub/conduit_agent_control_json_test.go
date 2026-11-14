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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoControl answers every control RPC 207 with the request's
// Content-Type and body, so a test sees exactly what reached the agent.
func echoControl(conduit.RPCHandler) conduit.RPCHandler {
	return conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		return &conduitv1.RpcResponse{Status: 207, Body: []byte(req.GetHeaders()["Content-Type"] + "|" + string(req.GetBody()))}
	})
}

// TestCallAgentControlJSON_RoundTrip: the body goes out as
// application/json and the agent's status and body come back as they
// are, locally and across nodes, in one RPC.
func TestCallAgentControlJSON_RoundTrip(t *testing.T) {
	for _, crossNode := range []bool{false, true} {
		name := map[bool]string{false: "local", true: "cross-node"}[crossNode]
		t.Run(name, func(t *testing.T) {
			f := newPTYConduitFixture(t)
			hubURL := f.public.URL
			if crossNode {
				hubURL = f.startPeerRelay(t, "hub-b")
			}
			ca := f.startControlAgent(t, hubURL, true, echoControl)
			status, body, err := f.srv.callAgentControlJSON(context.Background(), f.launched.ID,
				agentControlRouteRotateToken, []byte(`{"v":1}`), time.Minute)
			require.NoError(t, err)
			assert.Equal(t, 207, status)
			assert.Equal(t, `application/json|{"v":1}`, string(body))
			assert.Equal(t, int32(1), ca.rpcs.Load())

			// No body: no Content-Type either.
			_, body, err = f.srv.callAgentControlJSON(context.Background(), f.launched.ID,
				agentControlRouteRotateToken, nil, time.Minute)
			require.NoError(t, err)
			assert.Equal(t, "|", string(body))
		})
	}
}

// TestCallAgentControlJSON_Timeout: the per-route timeout bounds the call;
// it ends with a deadline error after one RPC, never a retry.
func TestCallAgentControlJSON_Timeout(t *testing.T) {
	f := newPTYConduitFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ca := f.startControlAgent(t, f.public.URL, true, func(conduit.RPCHandler) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(ctx context.Context, _ *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &conduitv1.RpcResponse{Status: 200}
		})
	})
	start := time.Now()
	status, _, err := f.srv.callAgentControlJSON(context.Background(), f.launched.ID,
		agentControlRouteRotateToken, []byte(`{}`), 300*time.Millisecond)
	require.Error(t, err)
	assert.Zero(t, status)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "err = %v", err)
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.Equal(t, int32(1), ca.rpcs.Load(), "one RPC, no retry")
}

// TestCallAgentControlJSON_NotAdvertisedAndNoSession: the error classes a
// caller maps to "not reached" carry no status, and nothing is sent.
func TestCallAgentControlJSON_NotAdvertisedAndNoSession(t *testing.T) {
	f := newPTYConduitFixture(t)
	_, _, err := f.srv.callAgentControlJSON(context.Background(), f.launched.ID, "deliver-message", []byte(`{}`), time.Minute)
	require.ErrorIs(t, err, router.ErrNoSession)

	ca := f.startControlAgent(t, f.public.URL, true, nil) // advertises rotate-token only
	status, _, err := f.srv.callAgentControlJSON(context.Background(), f.launched.ID, "deliver-message", []byte(`{}`), time.Minute)
	require.ErrorIs(t, err, ErrAgentControlNotAdvertised)
	assert.Zero(t, status)
	assert.Equal(t, int32(0), ca.rpcs.Load())
}
