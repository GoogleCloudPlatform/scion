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
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
)

// fakeLegacy is a legacy session to a broker.
type fakeLegacy struct{ id string }

func (f fakeLegacy) LegacyTarget() (string, string) { return registry.PrincipalBroker, f.id }

// stubSession is a non-nil conduit.Session that is never used.
type stubSession struct{ conduit.Session }

// fakeBrokers is an owner-only BrokerResolver: it resolves the brokers in
// owned (broker id -> control channel session id) and answers
// ErrNoSession for any other.
type fakeBrokers struct {
	owned map[string]string
	calls int
	bad   *router.Resolved // when set, returned as is
	fresh bool             // a new session id on every call (the broker keeps reconnecting)
}

func (f *fakeBrokers) ResolveBroker(_ context.Context, req router.Request) (router.Resolved, error) {
	f.calls++
	if f.bad != nil {
		return *f.bad, nil
	}
	sid, ok := f.owned[req.ID]
	if !ok {
		return router.Resolved{}, router.ErrNoSession
	}
	if f.fresh {
		sid = fmt.Sprintf("%s-%d", sid, f.calls)
	}
	return router.Resolved{
		Legacy: fakeLegacy{id: req.ID},
		Record: registry.SessionRecord{SessionID: sid, PrincipalKind: registry.PrincipalBroker, PrincipalID: req.ID, RelayInstanceID: "relay-a"},
		Want:   req.Want,
		Local:  true,
	}, nil
}

func newLegacyRouter(t *testing.T, n *relaytest.Node, b router.BrokerResolver) *router.Router {
	t.Helper()
	rt, err := router.New(router.Config{Relay: n.Relay, Registry: n.W.Registry, Store: n.W.Store, Peers: n.Peers, Now: n.W.Now, Brokers: b})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// legacyBrokerReq is a broker request as a Phase 2 caller makes it: no
// broker incarnation (the legacy channel has none).
func legacyBrokerReq() router.Request {
	return router.Request{Op: router.OpStream, Kind: registry.PrincipalBroker, ID: brokerID}
}

// TestLegacyBrokerResolve_OnOwner: when this node holds the broker's
// control channel, Resolve returns the local legacy session.
func TestLegacyBrokerResolve_OnOwner(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := &fakeBrokers{owned: map[string]string{brokerID: "cc-1"}}
	res, err := newLegacyRouter(t, a, b).Resolve(context.Background(), legacyBrokerReq(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Session != nil || res.Legacy == nil || !res.Local {
		t.Fatalf("got Session=%v Legacy=%v Local=%v, want a local legacy session only", res.Session, res.Legacy, res.Local)
	}
	if kind, id := res.Legacy.LegacyTarget(); kind != registry.PrincipalBroker || id != brokerID {
		t.Fatalf("legacy target = %s/%s, want broker/%s", kind, id, brokerID)
	}
	if res.Record.SessionID != "cc-1" {
		t.Fatalf("session id = %q, want cc-1", res.Record.SessionID)
	}
}

// TestLegacyBrokerResolve_OffOwner: when this node does not hold the
// broker's control channel, Resolve returns ErrNoSession, even if the
// broker has a conduit session on this very relay: no broker conduit
// capability is assumed before Phase 3.
func TestLegacyBrokerResolve_OffOwner(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	a.MustDial("b", relaytest.BrokerHello(brokerID, "inc-1"), conduit.Config{})
	b := &fakeBrokers{owned: map[string]string{}}
	rt := newLegacyRouter(t, a, b)
	for _, req := range []router.Request{
		legacyBrokerReq(),
		{Op: router.OpStatefulRPC, Kind: registry.PrincipalBroker, ID: brokerID, BrokerIncarnation: "inc-1"},
	} {
		if _, err := rt.Resolve(context.Background(), req, nil); !errors.Is(err, router.ErrNoSession) {
			t.Fatalf("Resolve(%+v) err = %v, want ErrNoSession", req, err)
		}
	}
	called := false
	err := rt.Do(context.Background(), legacyBrokerReq(), func(context.Context, router.Resolved) error {
		called = true
		return nil
	})
	if !errors.Is(err, router.ErrNoSession) || called {
		t.Fatalf("Do err = %v (called %v), want ErrNoSession without calling", err, called)
	}
}

// TestLegacyBrokerResolve_AgentsUnaffected: the broker adapter is not
// consulted for agent targets.
func TestLegacyBrokerResolve_AgentsUnaffected(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project, Agent: facts})
	a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoTarget())
	b := &fakeBrokers{owned: map[string]string{brokerID: "cc-1"}}
	res, err := newLegacyRouter(t, a, b).Resolve(context.Background(), agentReq(router.OpStream), nil)
	if err != nil || res.Session == nil || res.Legacy != nil {
		t.Fatalf("agent Resolve = (%+v, %v), want a conduit session", res, err)
	}
	if b.calls != 0 {
		t.Fatalf("broker resolver called %d times for an agent target", b.calls)
	}
}

// TestLegacyBrokerResolve_ReResolveBudget: a stale legacy route is
// excluded like any other, and re-resolution stays within the shared
// MaxReResolves budget, ending in ErrNoSession.
func TestLegacyBrokerResolve_ReResolveBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		fresh        bool
		wantCalls    int
		wantResolves int
	}{
		// The same session comes back: it is excluded, so the second
		// resolution is ErrNoSession without calling fn again.
		{name: "same session excluded", fresh: false, wantCalls: 1, wantResolves: 2},
		// A new session on every resolution: fn runs until the budget is
		// spent.
		{name: "fresh session each time", fresh: true, wantCalls: router.MaxReResolves + 1, wantResolves: router.MaxReResolves + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			a := w.StartNode("relay-a", nil)
			b := &fakeBrokers{owned: map[string]string{brokerID: "cc"}, fresh: tc.fresh}
			calls := 0
			err := newLegacyRouter(t, a, b).Do(context.Background(), legacyBrokerReq(), func(context.Context, router.Resolved) error {
				calls++
				return &relay.StaleRouteError{Reason: "test"}
			})
			if !errors.Is(err, router.ErrNoSession) {
				t.Fatalf("err = %v, want ErrNoSession", err)
			}
			if calls != tc.wantCalls || b.calls != tc.wantResolves {
				t.Fatalf("fn calls = %d, resolver calls = %d; want %d and %d", calls, b.calls, tc.wantCalls, tc.wantResolves)
			}
		})
	}
}

// TestLegacyBrokerResolve_InvalidRequest: the shared request checks apply
// on the legacy path and the resolver is not consulted.
func TestLegacyBrokerResolve_InvalidRequest(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	for _, tc := range []struct {
		name string
		mod  func(*router.Request)
	}{
		{name: "caller-supplied incarnation", mod: func(r *router.Request) { r.Want.Incarnation = "inc-1" }},
		{name: "AnyExecScope on a stream", mod: func(r *router.Request) { r.Want.AnyExecScope = true }},
		{name: "broker with a project", mod: func(r *router.Request) { r.Want.ProjectID = project }},
		{name: "empty broker id", mod: func(r *router.Request) { r.ID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBrokers{owned: map[string]string{brokerID: "cc-1", "": "cc-x"}}
			req := legacyBrokerReq()
			tc.mod(&req)
			if _, err := newLegacyRouter(t, a, b).Resolve(context.Background(), req, nil); !errors.Is(err, router.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if b.calls != 0 {
				t.Fatalf("resolver consulted for an invalid request")
			}
		})
	}
}

// TestLegacyBrokerResolve_MalformedResolution: a resolver answer without
// exactly one legacy session is refused rather than handed to the caller.
func TestLegacyBrokerResolve_MalformedResolution(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	rec := registry.SessionRecord{SessionID: "cc-1"}
	for _, tc := range []struct {
		name string
		res  router.Resolved
	}{
		{name: "neither Session nor Legacy", res: router.Resolved{Record: rec}},
		{name: "both Session and Legacy", res: router.Resolved{Session: stubSession{}, Legacy: fakeLegacy{id: brokerID}, Record: rec}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &fakeBrokers{bad: &tc.res}
			if _, err := newLegacyRouter(t, a, b).Resolve(context.Background(), legacyBrokerReq(), nil); err == nil {
				t.Fatal("a malformed resolution was accepted")
			}
		})
	}
}
