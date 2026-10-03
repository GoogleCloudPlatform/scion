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

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const (
	agentID = "agent-1"
	project = "proj-1"
)

func agentPrincipal(launchID string, gen int64) relay.Principal {
	return relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project,
		Agent: relay.AgentIncarnationFacts{LaunchID: launchID, Generation: gen}}
}

func TestAdmitAgentIncarnation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		facts      relay.AgentIncarnationFacts
		presented  string
		want       relay.Incarnation
		superseded bool
	}{
		{name: "matching launch id", facts: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3}, presented: "L2",
			want: relay.Incarnation{Value: "L2", Source: relay.IncarnationSourceLaunchID}},
		{name: "superseded launch id", facts: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3}, presented: "L1", superseded: true},
		{name: "row without launch id cannot vouch", facts: relay.AgentIncarnationFacts{Generation: 3}, presented: "L1", superseded: true},
		{name: "presented value spoofing the generation namespace", facts: relay.AgentIncarnationFacts{LaunchID: "gen-3", Generation: 3}, presented: "gen-3", superseded: true},
		{name: "no launch id falls back to generation", facts: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3}, presented: "",
			want: relay.Incarnation{Value: "gen-3", Source: relay.IncarnationSourceGeneration}},
		{name: "fallback on a row without launch id", facts: relay.AgentIncarnationFacts{Generation: 0}, presented: "",
			want: relay.Incarnation{Value: "gen-0", Source: relay.IncarnationSourceGeneration}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := relay.AdmitAgentIncarnation(tc.facts, tc.presented)
			if tc.superseded {
				if !relay.IsSupersededIncarnation(err) || conduit.CodeOf(err, 0) != relay.CloseSupersededIncarnation {
					t.Fatalf("err = %v, want 4409 superseded_incarnation", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("= %+v, %v; want %+v", got, err, tc.want)
			}
			// Routing derives the same value through the same policy.
			found := false
			for _, inc := range relay.RouteAgentIncarnations(tc.facts) {
				found = found || inc == got
			}
			if !found {
				t.Fatalf("RouteAgentIncarnations(%+v) = %+v does not include the admitted %+v", tc.facts, relay.RouteAgentIncarnations(tc.facts), got)
			}
		})
	}
}

// TestRouteAgentIncarnationsLaunchIDFirst: the launch id is tried before
// the interim generation value, so a launch-id session wins lookup order.
func TestRouteAgentIncarnationsLaunchIDFirst(t *testing.T) {
	got := relay.RouteAgentIncarnations(relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3})
	want := []relay.Incarnation{{Value: "L2", Source: relay.IncarnationSourceLaunchID}, {Value: "gen-3", Source: relay.IncarnationSourceGeneration}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("= %+v, want %+v", got, want)
	}
	if got := relay.RouteAgentIncarnations(relay.AgentIncarnationFacts{Generation: 3}); len(got) != 1 || got[0].Value != "gen-3" {
		t.Fatalf("without launch id = %+v", got)
	}
}

func TestAdmitLaunchIDMatchAdmitted(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))
	_, wel := n.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
	if wel.GetConnectionEpoch() != 1 || wel.GetRelayInstanceId() != "relay-a" || len(wel.GetGrantKeys()) != 1 {
		t.Fatalf("welcome = %v", wel)
	}
	ps := w.Sessions(registry.PrincipalAgent, agentID)
	if len(ps.Sessions) != 1 {
		t.Fatalf("rows = %d", len(ps.Sessions))
	}
	rec := ps.Sessions[0].Session
	if rec.EndpointIncarnation != "L2" || rec.Capabilities.IncarnationSource != relay.IncarnationSourceLaunchID || rec.Capabilities.EndpointIncarnation != "L2" {
		t.Fatalf("row incarnation = %q source %q", rec.EndpointIncarnation, rec.Capabilities.IncarnationSource)
	}
	if got := n.Relay.SourceForTest(rec.SessionID); got != relay.IncarnationSourceLaunchID {
		t.Fatalf("entry source = %q", got)
	}
}

func TestAdmitMissingLaunchIDFallsBackToGeneration(t *testing.T) {
	w := relaytest.NewWorld(t)
	n := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))
	n.MustDial("a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
	rec := w.Sessions(registry.PrincipalAgent, agentID).Sessions[0].Session
	if rec.EndpointIncarnation != "gen-3" || rec.Capabilities.IncarnationSource != relay.IncarnationSourceGeneration {
		t.Fatalf("row incarnation = %q source %q, want gen-3 / generation", rec.EndpointIncarnation, rec.Capabilities.IncarnationSource)
	}
}

// TestAdmissionRefusals: every refusal that does not need the registry
// happens before InsertSessionWithNextEpoch, so no row is written and no
// epoch is consumed.
func TestAdmissionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		principal relay.Principal
		hello     *conduitv1.Hello
		fault     string // store op that fails
		keysErr   bool
		wantCode  uint32
	}{
		{name: "superseded launch id", principal: agentPrincipal("L2", 3), hello: relaytest.AgentHello(agentID, "L1", "", "pty"), wantCode: relay.CloseSupersededIncarnation},
		{name: "hello id differs from authenticated id", principal: agentPrincipal("L2", 3), hello: relaytest.AgentHello("agent-2", "L2", ""), wantCode: conduit.CloseForbidden},
		{name: "hello kind differs", principal: agentPrincipal("L2", 3), hello: relaytest.BrokerHello(agentID, "b1"), wantCode: conduit.CloseForbidden},
		{name: "exec scope claim differs", principal: agentPrincipal("L2", 3), hello: relaytest.AgentHello(agentID, "L2", "scope-x"), wantCode: conduit.CloseForbidden},
		{name: "relay-peer may not hold a session", principal: relay.Principal{Kind: registry.PrincipalRelayPeer, ID: "relay-z"},
			hello: &conduitv1.Hello{PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_RELAY_PEER, PrincipalId: "relay-z"}, wantCode: conduit.CloseForbidden},
		{name: "grant keys unavailable", principal: agentPrincipal("L2", 3), hello: relaytest.AgentHello(agentID, "L2", ""), keysErr: true, wantCode: conduit.CloseRelayRestart},
		{name: "registry insert fails closed", principal: agentPrincipal("L2", 3), hello: relaytest.AgentHello(agentID, "L2", ""), fault: registry.OpInsertSessionWithNextEpoch, wantCode: conduit.CloseRelayRestart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			n := w.StartNode("relay-a", func(c *relay.Config) {
				if tc.keysErr {
					c.GrantKeys = func(context.Context) ([]*conduitv1.GrantKey, error) { return nil, errors.New("key store down") }
				}
			})
			w.SetPrincipal("p", tc.principal)
			if tc.fault != "" {
				w.SetFault(func(op string) error {
					if op == tc.fault {
						return errors.New("injected")
					}
					return nil
				})
			}
			_, _, err := n.Dial(context.Background(), "p", tc.hello, conduit.Config{})
			if got := conduit.CodeOf(err, 0); got != tc.wantCode {
				t.Fatalf("dial err = %v (code %d), want code %d", err, got, tc.wantCode)
			}
			w.SetFault(nil)
			ps := w.Sessions(tc.principal.Kind, tc.principal.ID)
			if len(ps.Sessions) != 0 || ps.CurrentEpoch != 0 {
				t.Fatalf("refusal left %d rows, epoch %d; want none", len(ps.Sessions), ps.CurrentEpoch)
			}
		})
	}
}

// TestZombieLaunchRefusedSuccessorKeepsRouting (T6, design v2.4 §3.4): the
// container of superseded launch L1 reconnects after launch L2's session
// exists. It is refused with 4409 before any epoch is allocated, and L2's
// session keeps the current epoch and keeps routing.
func TestZombieLaunchRefusedSuccessorKeepsRouting(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))
	echo := conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: 200, Body: []byte("from L2")}
	})}
	_, wel := a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), echo)

	// The zombie dials another relay, as a partitioned pod would.
	_, _, err := b.Dial(context.Background(), "a", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
	if !relay.IsSupersededIncarnation(err) {
		t.Fatalf("zombie dial = %v, want 4409", err)
	}
	ps := w.Sessions(registry.PrincipalAgent, agentID)
	if len(ps.Sessions) != 1 || ps.CurrentEpoch != wel.GetConnectionEpoch() {
		t.Fatalf("after zombie: %d rows, epoch %d; want 1 row at epoch %d", len(ps.Sessions), ps.CurrentEpoch, wel.GetConnectionEpoch())
	}

	rt, err := router.New(router.Config{Relay: b.Relay, Registry: w.Registry, Store: w.Store, Peers: b.Peers, Now: w.Now})
	if err != nil {
		t.Fatal(err)
	}
	req := router.Request{Op: router.OpStatefulRPC, Kind: registry.PrincipalAgent, ID: agentID,
		Want: registry.Want{ProjectID: project}, Agent: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3}}
	err = rt.Do(context.Background(), req, func(ctx context.Context, res router.Resolved) error {
		if res.Record.SessionID != wel.GetSessionId() || res.Want.Incarnation != "L2" || res.Local {
			t.Fatalf("resolved %+v", res.Record)
		}
		resp, err := res.Session.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "GET", Path: "/x"})
		if err != nil {
			return err
		}
		if string(resp.GetBody()) != "from L2" {
			t.Fatalf("body = %q", resp.GetBody())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("routing to L2 after zombie: %v", err)
	}
}

// TestRouterPrefersLaunchIDSession: with a gen-N session and a launch-id
// session of the same agent both present, routing picks the launch-id
// session. (For agents only the current-epoch session is ever eligible;
// the reverse order, a fallback session connecting after the launch-id
// one, is the documented interim gap and routes to the fallback session.)
func TestRouterPrefersLaunchIDSession(t *testing.T) {
	for _, tc := range []struct {
		name     string
		order    []string // launch ids presented, in connection order ("" = old sciontool)
		wantInc  string
		wantFrom int // index into order of the session expected
	}{
		{name: "launch id connected last wins", order: []string{"", "L2"}, wantInc: "L2", wantFrom: 1},
		{name: "interim gap: fallback connected last", order: []string{"L2", ""}, wantInc: "gen-3", wantFrom: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			a := w.StartNode("relay-a", nil)
			b := w.StartNode("relay-b", nil)
			w.SetPrincipal("a", agentPrincipal("L2", 3))
			nodes := []*relaytest.Node{a, b}
			var ids []string
			for i, li := range tc.order {
				_, wel := nodes[i].MustDial("a", relaytest.AgentHello(agentID, li, "", "pty"), conduit.Config{})
				ids = append(ids, wel.GetSessionId())
			}
			if n := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions); n != 2 {
				t.Fatalf("rows = %d, want both sessions present", n)
			}
			rt, _ := router.New(router.Config{Relay: a.Relay, Registry: w.Registry, Store: w.Store, Peers: a.Peers, Now: w.Now})
			res, err := rt.Resolve(context.Background(), router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: agentID,
				Want: registry.Want{ProjectID: project}, Agent: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Record.SessionID != ids[tc.wantFrom] || res.Want.Incarnation != tc.wantInc {
				t.Fatalf("resolved %s (incarnation %s), want %s (%s)", res.Record.SessionID, res.Want.Incarnation, ids[tc.wantFrom], tc.wantInc)
			}
		})
	}
}
