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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// callDrainingTarget dials a target of the given kind on relay A, keeps one
// RPC in flight on it, has the target send GoAway (the registry row is not
// marked draining), and returns the error of a second RPC sent to it from
// relay B through the internal API.
func callDrainingTarget(t *testing.T, kind string) error {
	t.Helper()
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	cfg := echoConfig()
	echo := cfg.RPCHandler
	cfg.RPCHandler = conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		if req.GetPath() == "/hold" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &conduitv1.RpcResponse{Status: 200}
		}
		return echo.HandleRPC(ctx, req)
	})

	var target conduit.LocalSession
	var want registry.Want
	var rec registry.SessionRecord
	switch kind {
	case registry.PrincipalBroker:
		w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1", Incarnation: "inc-1"})
		target, _ = a.MustDial("b", relaytest.BrokerHello("broker-1", "inc-1"), cfg)
		rec = w.Sessions(registry.PrincipalBroker, "broker-1").Sessions[0].Session
		want = registry.Want{Incarnation: "inc-1"}
	default:
		w.SetPrincipal("a", agentPrincipal("L1", 1))
		target, _ = a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), cfg)
		rec = w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session
		want = registry.Want{ProjectID: project, Incarnation: "L1"}
	}
	ls, _, ok := a.Relay.Local(context.Background(), rec.SessionID)
	if !ok {
		t.Fatal("owner relay does not hold the target session")
	}
	remote := relay.NewRemoteSession(b.Peers, a.Internal.URL, rec, want)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// An RPC in flight keeps the target's drain open.
	held := make(chan error, 1)
	go func() {
		_, err := remote.Call(ctx, &conduitv1.RpcRequest{RequestId: "hold", Method: "GET", Path: "/hold"})
		held <- err
	}()
	relaytest.Wait[struct{}](t, entered, "held RPC reached the target")
	if err := target.GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute}); err != nil {
		t.Fatal(err)
	}
	relaytest.WaitClosed(t, ls.GoAwayReceived(), "owner relay saw the target's GoAway")

	_, err := remote.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "GET", Path: "/x"})
	close(release)
	if herr := <-held; herr != nil {
		t.Fatalf("held RPC = %v, want it to complete during the drain", herr)
	}
	return err
}

// TestServeRPCDrainingTargetIsStaleRoute: an internal RPC to a broker
// target session that is draining (the target sent GoAway) is refused as
// a stale route for that session, so the caller re-resolves. It is never
// a 502, which the caller would have to treat as an unknown outcome.
func TestServeRPCDrainingTargetIsStaleRoute(t *testing.T) {
	err := callDrainingTarget(t, registry.PrincipalBroker)
	var stale *relay.StaleRouteError
	if !errors.As(err, &stale) {
		t.Fatalf("Call = %v, want a stale route", err)
	}
	if stale.Reason != string(registry.ReasonDraining) {
		t.Fatalf("stale reason = %q, want %q", stale.Reason, registry.ReasonDraining)
	}
}

// TestServeRPCDrainingAgentTargetUnchanged: for an agent target the
// answer is unchanged: a draining target session gives the 502 with a
// close code (4504), never a stale route.
func TestServeRPCDrainingAgentTargetUnchanged(t *testing.T) {
	err := callDrainingTarget(t, registry.PrincipalAgent)
	if errors.Is(err, relay.ErrStaleRoute) {
		t.Fatalf("Call = %v, want no stale route for an agent target", err)
	}
	var ce *conduit.CloseError
	if !errors.As(err, &ce) || ce.Code != conduit.CloseRelayTimeout {
		t.Fatalf("Call = %v, want the 502 close error %d", err, conduit.CloseRelayTimeout)
	}
}
