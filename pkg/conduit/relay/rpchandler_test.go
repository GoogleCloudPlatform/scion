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

package relay_test

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestServe_PrincipalRPCHandler: a principal's RPCHandler serves that
// session's RPCs, and its context names the session
// (conduit.LocalSessionFromContext). A principal without one keeps the
// relay default: no handler, 501.
func TestServe_PrincipalRPCHandler(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("r1", nil)
	seen := make(chan conduit.LocalSession, 1)
	w.SetPrincipal("u", relay.Principal{
		Kind: registry.PrincipalUser,
		ID:   "user-1",
		RPCHandler: conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			seen <- conduit.LocalSessionFromContext(ctx)
			return &conduitv1.RpcResponse{Status: 200, Body: []byte(req.GetPath())}
		}),
	})
	w.SetPrincipal("a", agentPrincipal("L1", 1))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user, wel := n.MustDial("u", relaytest.UserHello("user-1"), conduit.Config{})
	resp, err := user.Call(ctx, &conduitv1.RpcRequest{Method: "POST", Path: "/v1/tunnels"})
	if err != nil {
		t.Fatalf("user call: %v", err)
	}
	if resp.GetStatus() != 200 || string(resp.GetBody()) != "/v1/tunnels" {
		t.Fatalf("user call = %d %q, want 200 from the principal's handler", resp.GetStatus(), resp.GetBody())
	}
	ls := relaytest.Wait(t, seen, "handler call")
	if ls == nil {
		t.Fatal("LocalSessionFromContext returned nil in the handler")
	}
	if info := ls.Info(); info.SessionID != wel.GetSessionId() || info.PrincipalKind != registry.PrincipalUser {
		t.Fatalf("handler session = %s/%s, want %s/user", info.SessionID, info.PrincipalKind, wel.GetSessionId())
	}
	if got := conduit.LocalSessionFromContext(context.Background()); got != nil {
		t.Fatal("LocalSessionFromContext on a plain context is not nil")
	}

	agent, _ := n.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "tcp"), conduit.Config{})
	resp, err = agent.Call(ctx, &conduitv1.RpcRequest{Method: "POST", Path: "/v1/tunnels"})
	if err != nil {
		t.Fatalf("agent call: %v", err)
	}
	if resp.GetStatus() != 501 {
		t.Fatalf("agent call = %d, want 501 (no handler)", resp.GetStatus())
	}
}
