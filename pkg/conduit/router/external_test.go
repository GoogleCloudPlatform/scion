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

package router_test

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestResolveWithoutLocalRelay (design §3.9, external-relay mode): a
// router with no local relay, as on a hub that runs none, resolves every
// session to its owner relay's internal endpoint and reaches it there.
func TestResolveWithoutLocalRelay(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project, Agent: facts})
	_, wel := a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoTarget())

	if _, err := router.New(router.Config{Registry: w.Registry, Store: w.Store}); err == nil {
		t.Fatal("router.New without Peers succeeded; Registry, Store and Peers stay required")
	}
	hub := &relay.PeerClient{HTTP: a.Internal.Client(), Auth: w.PeerAuth("hub-1")}
	rt, err := router.New(router.Config{Registry: w.Registry, Store: w.Store, Peers: hub, Now: w.Now})
	if err != nil {
		t.Fatalf("router.New without a local relay: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := rt.Resolve(ctx, agentReq(router.OpStream), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Local || res.Record.SessionID != wel.GetSessionId() || res.Record.RelayInstanceID != "relay-a" {
		t.Fatalf("resolved %+v local=%v; want relay-a's session, remote", res.Record, res.Local)
	}
	if _, ok := res.Session.(*relay.RemoteSession); !ok {
		t.Fatalf("resolved session is %T, want *relay.RemoteSession", res.Session)
	}
	err = rt.Do(ctx, agentReq(router.OpStatefulRPC), func(ctx context.Context, res router.Resolved) error {
		resp, err := res.Session.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "exec"})
		if err != nil || string(resp.GetBody()) != "from target" {
			t.Fatalf("Call through the owner relay = %v, %v", resp, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
