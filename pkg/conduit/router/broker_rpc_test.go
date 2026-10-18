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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// brokerRPCHello is a broker Hello listing the given RPC operations.
func brokerRPCHello(incarnation string, rpcs ...string) *conduitv1.Hello {
	return &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: rpcs, EndpointIncarnation: incarnation},
	}
}

// brokerRPCReq is a broker RPC request naming its operation, without a
// caller-supplied incarnation (the router derives it).
func brokerRPCReq(op string) router.Request {
	return router.Request{Op: router.OpStatefulRPC, Kind: registry.PrincipalBroker, ID: brokerID, Want: registry.Want{Capability: op}}
}

// TestResolveBrokerRPCUsesRegistry: a broker RPC that names its
// operation is resolved to the broker's conduit session through the
// registry, even with the legacy adapter configured, and the adapter is
// not consulted.
func TestResolveBrokerRPCUsesRegistry(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	a.MustDial("b", brokerRPCHello("inc-1", "agent_exec", "agent_logs"), echoTarget())
	b := &fakeBrokers{owned: map[string]string{brokerID: "cc-1"}}
	res, err := newLegacyRouter(t, a, b).Resolve(context.Background(), brokerRPCReq("agent_exec"), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Session == nil || res.Legacy != nil {
		t.Fatalf("got Session=%v Legacy=%v, want a conduit session", res.Session, res.Legacy)
	}
	if res.Want.Incarnation != "inc-1" || res.Record.EndpointIncarnation != "inc-1" {
		t.Fatalf("resolved incarnation want=%q record=%q, want inc-1", res.Want.Incarnation, res.Record.EndpointIncarnation)
	}
	if b.calls != 0 {
		t.Fatalf("legacy adapter called %d times for a broker RPC", b.calls)
	}
}

// TestResolveBrokerRPCRequiresListedOperation: a broker session that does
// not list the operation is never chosen for it.
func TestResolveBrokerRPCRequiresListedOperation(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	a.MustDial("b", brokerRPCHello("inc-1", "agent_exec"), echoTarget())
	b := &fakeBrokers{owned: map[string]string{brokerID: "cc-1"}}
	if _, err := newLegacyRouter(t, a, b).Resolve(context.Background(), brokerRPCReq("agent_logs"), nil); !errors.Is(err, router.ErrNoSession) {
		t.Fatalf("Resolve err = %v, want ErrNoSession", err)
	}
	if b.calls != 0 {
		t.Fatalf("legacy adapter called %d times for a broker RPC", b.calls)
	}
}

// TestResolveBrokerStreamUsesLegacyAdapter: broker streams (PTY) keep the
// legacy adapter, even when the broker's conduit session lists RPCs.
func TestResolveBrokerStreamUsesLegacyAdapter(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	a.MustDial("b", brokerRPCHello("inc-1", "agent_exec"), echoTarget())
	b := &fakeBrokers{owned: map[string]string{brokerID: "cc-1"}}
	res, err := newLegacyRouter(t, a, b).Resolve(context.Background(), legacyBrokerReq(), nil)
	if err != nil || res.Legacy == nil || res.Session != nil {
		t.Fatalf("Resolve = (%+v, %v), want the legacy session", res, err)
	}
	if b.calls != 1 {
		t.Fatalf("legacy adapter called %d times, want 1", b.calls)
	}
}

// TestResolveBrokerRPCNewestIncarnationOnly: with sessions of two
// incarnations live, only the most recently admitted incarnation is
// resolved, and a caller naming the older one gets nothing.
func TestResolveBrokerRPCNewestIncarnationOnly(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	bn := w.StartNode("relay-b", nil)
	w.SetPrincipal("old", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	w.SetPrincipal("new", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-2"})
	a.MustDial("old", brokerRPCHello("inc-1", "agent_exec"), echoTarget())
	bn.MustDial("new", brokerRPCHello("inc-2", "agent_exec"), echoTarget())
	rt := newLegacyRouter(t, a, &fakeBrokers{owned: map[string]string{}})

	seen := map[string]bool{}
	err := rt.Do(context.Background(), brokerRPCReq("agent_exec"), func(_ context.Context, res router.Resolved) error {
		seen[res.Record.EndpointIncarnation] = true
		return &relay.StaleRouteError{Reason: "test"} // exclude it and look for another
	})
	if !errors.Is(err, router.ErrNoSession) {
		t.Fatalf("Do err = %v, want ErrNoSession once the newest incarnation is excluded", err)
	}
	if !seen["inc-2"] || seen["inc-1"] {
		t.Fatalf("incarnations resolved = %v, want only inc-2", seen)
	}

	stale := brokerRPCReq("agent_exec")
	stale.BrokerIncarnation = "inc-1"
	if _, err := rt.Resolve(context.Background(), stale, nil); !errors.Is(err, router.ErrNoSession) {
		t.Fatalf("Resolve with the older incarnation err = %v, want ErrNoSession", err)
	}
}

// TestResolveBrokerRPCRegistryUnavailable: a registry read error while
// deriving the broker's incarnation is ErrRegistryUnavailable, never a
// guess.
func TestResolveBrokerRPCRegistryUnavailable(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	a.MustDial("b", brokerRPCHello("inc-1", "agent_exec"), conduit.Config{})
	rt := newLegacyRouter(t, a, &fakeBrokers{owned: map[string]string{}})
	w.SetFault(func(string) error { return errors.New("registry test fault") })
	defer w.SetFault(nil)
	if _, err := rt.Resolve(context.Background(), brokerRPCReq("agent_exec"), nil); !errors.Is(err, router.ErrRegistryUnavailable) {
		t.Fatalf("Resolve err = %v, want ErrRegistryUnavailable", err)
	}
}
