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

package runtimebroker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestConduitHelloAdvertisesRPCs: a dialer with an RPC handler lists
// every broker RPC capability name in its Hello, and still no stream
// kinds.
func TestConduitHelloAdvertisesRPCs(t *testing.T) {
	hub := newFakeConduitHub(t, hubStep{})
	_, _, stop := startTestDialer(t, hub.srv.URL, clock.Real(), maxRand, func(c *ConduitDialConfig) {
		c.RPCHandler = newGuardedConduitRPCHandler(http.NotFoundHandler(), "hub-a", 0, nil, passAllChecker{})
	})
	hello := waitHello(t, hub)
	require.NoError(t, stop())
	caps := hello.GetCapabilities()
	assert.Equal(t, conduit.BrokerRPCs(), caps.GetRpc())
	assert.Equal(t, []string{"agent_logs", "agent_exec", "image_status", "image_pull", "image_delete", "project_cleanup"}, caps.GetRpc(),
		"the names are the contracts section 2 vocabulary")
	assert.Empty(t, caps.GetStreamKinds())
	assert.NotEmpty(t, caps.GetEndpointIncarnation())
}

// TestConduitSessionServesRPCs: the dialer's RPC handler answers the
// hub's RpcRequests on the session through the broker handler tree.
func TestConduitSessionServesRPCs(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	answered := make(chan *conduitv1.RpcResponse, 1)
	callErr := make(chan error, 1)
	hub := newFakeConduitHub(t, hubStep{after: func(ls conduit.LocalSession) {
		resp, err := ls.Call(context.Background(), envelope.RequestToRPC(
			wsprotocol.NewRequestEnvelope("c-1", http.MethodDelete, "/api/v1/projects/p", "project_id=p1", nil, nil)))
		if err != nil {
			callErr <- err
			return
		}
		answered <- resp
	}})
	_, _, stop := startTestDialer(t, hub.srv.URL, clock.Real(), maxRand, func(c *ConduitDialConfig) {
		c.RPCHandler = newGuardedConduitRPCHandler(handler, "hub-a", 0, nil, passAllChecker{})
	})
	select {
	case resp := <-answered:
		env := envelope.RPCToResponse(resp)
		if env.StatusCode != http.StatusNoContent || env.Headers["X-Path"] != "/api/v1/projects/p" {
			t.Fatalf("response %+v", env)
		}
	case err := <-callErr:
		t.Fatalf("Call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("hub never got an answer")
	}
	require.NoError(t, stop())
}

// TestConduitHelloWithoutRPCHandlerListsNone: without an RPC handler the
// session serves no RPCs and lists none, even when a session-level
// handler is set through the Session test seam.
func TestConduitHelloWithoutRPCHandlerListsNone(t *testing.T) {
	hub := newFakeConduitHub(t, hubStep{})
	_, _, stop := startTestDialer(t, hub.srv.URL, clock.Real(), maxRand, func(c *ConduitDialConfig) {
		c.Session.RPCHandler = newGuardedConduitRPCHandler(http.NotFoundHandler(), "hub-a", 0, nil, passAllChecker{})
	})
	hello := waitHello(t, hub)
	require.NoError(t, stop())
	assert.Empty(t, hello.GetCapabilities().GetRpc())
}
