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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// TestConduitBrokerTunnel_HybridFallsBackToHTTP checks that a create too
// large for the broker's conduit session goes to the broker's HTTP
// endpoint, as it does for the control channel. The client's own body
// check (checkBodySize) rejects this body before the tunnel is reached, so
// this covers the end-to-end fallback; the tunnel's own size check is
// covered by TestConduitBrokerTunnel_PayloadTooLarge.
func TestConduitBrokerTunnel_HybridFallsBackToHTTP(t *testing.T) {
	tun, echo, _ := newEchoTunnel(t)
	httpClient := &mockRuntimeBrokerClient{}
	c := &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: tun, signer: &mockBrokerSigner{}},
		httpClient:     httpClient,
	}
	req := &RemoteCreateAgentRequest{ID: "agent-1", Slug: "a", Name: "a", ResolvedEnv: map[string]string{"BIG": strings.Repeat("x", conduit.MaxRPCBody)}}
	if _, err := c.CreateAgent(context.Background(), "broker-1", "http://broker.invalid", req); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if !httpClient.createCalled {
		t.Fatal("expected the create to fall back to direct HTTP")
	}
	if n := len(echo.requests()); n != 0 {
		t.Fatalf("broker session saw %d requests, want 0", n)
	}
}
