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
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if wel.GetConnectionEpoch() != 1 || wel.GetRelayInstanceId() != "relay-a" || len(wel.GetGrantKeys()) != 1 || wel.GetEndpointIncarnation() != "L2" {
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
	if got := n.Relay.SourceForTest(t, rec.SessionID); got != relay.IncarnationSourceLaunchID {
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

// TestAdmissionRefusals maps every admission refusal path to its design
// v2.5 §3.3.1 close code and reason token. Every refusal leaves no row and,
// except where noted, consumes no epoch: the refusals that do not need the
// registry happen before InsertSessionWithNextEpoch.
func TestAdmissionRefusals(t *testing.T) {
	agent := agentPrincipal("L2", 3)
	l2 := relaytest.AgentHello(agentID, "L2", "", "pty")
	userHelloWithIncarnation := relaytest.UserHello("user-1")
	userHelloWithIncarnation.Capabilities.EndpointIncarnation = "x"
	noProject := agent
	noProject.ProjectID = ""
	for _, tc := range []struct {
		name      string
		principal relay.Principal
		hello     *conduitv1.Hello
		fault     string // store op that fails
		keysErr   bool
		notStart  bool // relay built but never started
		direct    bool // Admit called directly (the dialer refuses to send the Hello)
		supersede bool // relay instance re-registered before the Hello
		setup     func(t *testing.T, w *relaytest.World, n *relaytest.Node)
		wantCode  uint32
		wantToken string
	}{
		{name: "superseded launch id", principal: agent, hello: relaytest.AgentHello(agentID, "L1", "", "pty"),
			wantCode: relay.CloseSupersededIncarnation, wantToken: relay.ReasonSupersededIncarnation},
		{name: "legacy hello while the current launch is connected", principal: agent, hello: relaytest.AgentHello(agentID, "", "", "pty"),
			setup: func(t *testing.T, w *relaytest.World, n *relaytest.Node) {
				w.SetPrincipal("l2", agent)
				n.MustDial("l2", l2, conduit.Config{})
			}, wantCode: relay.CloseSupersededIncarnation, wantToken: relay.ReasonLegacyHelloSuperseded},
		{name: "hello id differs from authenticated id", principal: agent, hello: relaytest.AgentHello("agent-2", "L2", ""),
			wantCode: conduit.CloseForbidden, wantToken: relay.ReasonForbidden},
		{name: "hello kind differs", principal: agent, hello: relaytest.BrokerHello(agentID, "b1"),
			wantCode: conduit.CloseForbidden, wantToken: relay.ReasonForbidden},
		{name: "exec scope claim differs", principal: agent, hello: relaytest.AgentHello(agentID, "L2", "scope-x"),
			wantCode: conduit.CloseForbidden, wantToken: relay.ReasonForbidden},
		{name: "relay-peer may not hold a session", principal: relay.Principal{Kind: registry.PrincipalRelayPeer, ID: "relay-z"},
			hello:    &conduitv1.Hello{PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_RELAY_PEER, PrincipalId: "relay-z"},
			wantCode: conduit.CloseForbidden, wantToken: relay.ReasonForbidden},
		{name: "session not admissible (no project)", principal: noProject, hello: l2,
			wantCode: conduit.CloseForbidden, wantToken: relay.ReasonForbidden},
		{name: "unknown principal kind", principal: agent, hello: &conduitv1.Hello{PrincipalId: agentID}, direct: true,
			wantCode: conduit.CloseProtocolError, wantToken: relay.ReasonBadHello},
		{name: "broker without incarnation", principal: relay.Principal{Kind: registry.PrincipalBroker, ID: "broker-1"}, hello: relaytest.BrokerHello("broker-1", ""),
			wantCode: conduit.CloseProtocolError, wantToken: relay.ReasonBadHello},
		{name: "user presenting an incarnation", principal: relay.Principal{Kind: registry.PrincipalUser, ID: "user-1"}, hello: userHelloWithIncarnation,
			wantCode: conduit.CloseProtocolError, wantToken: relay.ReasonBadHello},
		{name: "grant keys unavailable", principal: agent, hello: l2, keysErr: true,
			wantCode: conduit.CloseRelayTimeout, wantToken: relay.ReasonGrantKeysUnavailable},
		{name: "registry insert fault", principal: agent, hello: l2, fault: registry.OpInsertSessionWithNextEpoch,
			wantCode: conduit.CloseRelayTimeout, wantToken: relay.ReasonRegistryUnavailable},
		{name: "fallback fence read fault", principal: agent, hello: relaytest.AgentHello(agentID, "", "", "pty"), fault: registry.OpListPrincipalSessions,
			wantCode: conduit.CloseRelayTimeout, wantToken: relay.ReasonRegistryUnavailable},
		{name: "relay not started", principal: agent, hello: l2, notStart: true,
			wantCode: conduit.CloseRelayRestart, wantToken: relay.ReasonNotServing},
		{name: "relay superseded at insert", principal: agent, hello: l2, supersede: true,
			wantCode: conduit.CloseRelayRestart, wantToken: relay.ReasonRelayRestart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			mod := func(c *relay.Config) {
				if tc.keysErr {
					c.GrantKeys = func(context.Context) ([]*conduitv1.GrantKey, error) { return nil, errors.New("key store down") }
				}
			}
			var n *relaytest.Node
			if tc.notStart {
				var err error
				if n, err = w.NewNode("relay-a", mod); err != nil {
					t.Fatal(err)
				}
			} else {
				n = w.StartNode("relay-a", mod)
			}
			if tc.setup != nil {
				tc.setup(t, w, n)
			}
			before := w.Sessions(tc.principal.Kind, tc.principal.ID)
			if tc.supersede {
				if _, err := w.Registry.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: "relay-a", InternalEndpoint: n.Internal.URL}); err != nil {
					t.Fatal(err)
				}
			}
			w.SetPrincipal("p", tc.principal)
			if tc.fault != "" {
				w.SetFault(func(op string) error {
					if op == tc.fault {
						return errors.New("injected")
					}
					return nil
				})
			}
			var err error
			if tc.direct {
				adm, _ := n.Relay.NewAdmitterForTest(tc.principal, registry.TransportWS)
				_, err = adm.Admit(context.Background(), tc.hello)
			} else {
				_, _, err = n.Dial(context.Background(), "p", tc.hello, conduit.Config{})
			}
			w.SetFault(nil)
			assertClose(t, err, tc.wantCode, tc.wantToken)
			ps := w.Sessions(tc.principal.Kind, tc.principal.ID)
			if len(ps.Sessions) != len(before.Sessions) || ps.CurrentEpoch != before.CurrentEpoch {
				t.Fatalf("refusal changed the rows (%d -> %d) or the epoch (%d -> %d)",
					len(before.Sessions), len(ps.Sessions), before.CurrentEpoch, ps.CurrentEpoch)
			}
		})
	}
}

// assertClose checks a close error's code and reason token (the part of
// the reason before ": "), and the reason bound.
func assertClose(t *testing.T, err error, code uint32, token string) {
	t.Helper()
	var ce *conduit.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want close %d %s", err, code, token)
	}
	got, _, _ := strings.Cut(ce.Reason, ":")
	if ce.Code != code || got != token {
		t.Fatalf("close = %d %q, want %d %s", ce.Code, ce.Reason, code, token)
	}
	if len(ce.Reason) > relay.MaxReasonBytes {
		t.Fatalf("reason is %d bytes, max %d", len(ce.Reason), relay.MaxReasonBytes)
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
// session. (For agents only the current-epoch session is ever eligible,
// so the launch-id session wins by lookup order while it holds the
// current epoch; a fallback Hello cannot take that over while the current
// launch is connected, see TestFallbackRefusedWhileLaunchIDLive.)
func TestRouterPrefersLaunchIDSession(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))
	_, legacy := a.MustDial("a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
	_, current := b.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
	if n := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions); n != 2 {
		t.Fatalf("rows = %d, want both sessions present", n)
	}
	res, err := resolveAgent(t, w, a, relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Record.SessionID != current.GetSessionId() || res.Want.Incarnation != "L2" {
		t.Fatalf("resolved %s (incarnation %s), want the launch-id session %s, not the gen-N session %s",
			res.Record.SessionID, res.Want.Incarnation, current.GetSessionId(), legacy.GetSessionId())
	}
}

func resolveAgent(t *testing.T, w *relaytest.World, n *relaytest.Node, f relay.AgentIncarnationFacts) (router.Resolved, error) {
	t.Helper()
	rt, err := router.New(router.Config{Relay: n.Relay, Registry: w.Registry, Store: w.Store, Peers: n.Peers, Now: w.Now})
	if err != nil {
		t.Fatal(err)
	}
	return rt.Resolve(context.Background(), router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: agentID,
		Want: registry.Want{ProjectID: project}, Agent: f}, nil)
}

// TestFallbackRefusedWhileLaunchIDLive (conduit-em hardening of the
// interim gap): a Hello without a launch id is refused with 4409 while a
// live, non-draining session admitted with the CURRENT launch id exists,
// before any row or epoch is written; otherwise the fallback is admitted
// as gen-N.
func TestFallbackRefusedWhileLaunchIDLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		// setup connects whatever exists before the fallback Hello.
		setup     func(t *testing.T, w *relaytest.World, a *relaytest.Node)
		fault     string // store op that fails during the fallback Hello
		wantCode  uint32 // 0 = admitted as gen-3
		wantRows  int    // rows after the fallback attempt
		l2Routing bool   // L2 must keep routing afterwards
	}{
		{name: "mixed version: L2 connected, pre-launch-id zombie refused", setup: func(t *testing.T, w *relaytest.World, a *relaytest.Node) {
			a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
		}, wantCode: relay.CloseSupersededIncarnation, wantRows: 1, l2Routing: true},
		{name: "legacy only: no launch-id session, fallback admitted", setup: func(*testing.T, *relaytest.World, *relaytest.Node) {},
			wantRows: 1},
		{name: "current launch draining: fallback admitted (window)", setup: func(t *testing.T, w *relaytest.World, a *relaytest.Node) {
			_, wel := a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
			if err := w.Inner.SetSessionDraining(context.Background(), wel.GetSessionId()); err != nil {
				t.Fatal(err)
			}
		}, wantRows: 2},
		{name: "live session of a superseded launch does not block", setup: func(t *testing.T, w *relaytest.World, a *relaytest.Node) {
			w.SetPrincipal("old", agentPrincipal("L1", 2))
			a.MustDial("old", relaytest.AgentHello(agentID, "L1", "", "pty"), conduit.Config{})
		}, wantRows: 2},
		{name: "L2 row on a stale relay still blocks", setup: func(t *testing.T, w *relaytest.World, a *relaytest.Node) {
			a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
			// relay-a stops heartbeating: the row is no longer live, but
			// liveness is ignored by the fence.
			w.Advance(2 * time.Minute)
		}, wantCode: relay.CloseSupersededIncarnation, wantRows: 1},
		{name: "registry read error fails closed (4504, never 4409)", setup: func(t *testing.T, w *relaytest.World, a *relaytest.Node) {
			a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
		}, fault: registry.OpListPrincipalSessions, wantCode: conduit.CloseRelayTimeout, wantRows: 1, l2Routing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			a := w.StartNode("relay-a", nil)
			logs := &captureHandler{}
			b := w.StartNode("relay-b", func(c *relay.Config) { c.Logger = slog.New(logs) })
			w.SetPrincipal("a", agentPrincipal("L2", 3))
			tc.setup(t, w, a)
			b.Relay.HeartbeatForTest() // relay-b stays live across w.Advance
			before := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions)

			var inserts atomic.Int32
			w.SetFault(func(op string) error {
				if op == registry.OpInsertSessionWithNextEpoch {
					inserts.Add(1)
				}
				if op == tc.fault {
					return errors.New("injected read error")
				}
				return nil
			})
			// The zombie dials a different relay than L2's.
			_, wel, err := b.Dial(context.Background(), "a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
			w.SetFault(nil)
			if got, want := logs.count("reason", relay.ReasonLegacyHelloSuperseded), btoi(tc.wantCode == relay.CloseSupersededIncarnation); got != want {
				t.Fatalf("reason=%s logged %d times, want %d", relay.ReasonLegacyHelloSuperseded, got, want)
			}
			if tc.wantCode != 0 {
				token := relay.ReasonRegistryUnavailable
				if tc.wantCode == relay.CloseSupersededIncarnation {
					token = relay.ReasonLegacyHelloSuperseded
				}
				assertClose(t, err, tc.wantCode, token)
				if !relay.IsSupersededIncarnation(err) != (tc.wantCode != relay.CloseSupersededIncarnation) {
					t.Fatalf("IsSupersededIncarnation(%v) disagrees with the code", err)
				}
				if inserts.Load() != 0 {
					t.Fatal("refusal reached InsertSessionWithNextEpoch")
				}
			} else {
				if err != nil {
					t.Fatalf("fallback Hello refused: %v", err)
				}
				if wel.GetEndpointIncarnation() != "gen-3" {
					t.Fatalf("welcome endpoint_incarnation = %q, want the admitted gen-3", wel.GetEndpointIncarnation())
				}
				rows := w.Sessions(registry.PrincipalAgent, agentID).Sessions
				var got registry.SessionRecord
				for _, v := range rows {
					if v.Session.SessionID == wel.GetSessionId() {
						got = v.Session
					}
				}
				if got.EndpointIncarnation != "gen-3" || got.Capabilities.IncarnationSource != relay.IncarnationSourceGeneration {
					t.Fatalf("fallback row incarnation %q source %q", got.EndpointIncarnation, got.Capabilities.IncarnationSource)
				}
			}
			if n := len(w.Sessions(registry.PrincipalAgent, agentID).Sessions); n != tc.wantRows {
				t.Fatalf("rows = %d (before %d), want %d", n, before, tc.wantRows)
			}
			if !tc.l2Routing {
				return
			}
			res, err := resolveAgent(t, w, b, relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3})
			if err != nil || res.Want.Incarnation != "L2" || res.Record.ConnectionEpoch != 1 {
				t.Fatalf("after refusal: resolved %+v (want L2 at epoch 1, no epoch burned), %v", res.Record, err)
			}
		})
	}
}

// TestFallbackWindowWhileLaunchDisconnected documents the remaining
// window: with the current launch disconnected, a fallback Hello is
// admitted as gen-N and routes; when the current launch reconnects with
// its launch id it takes routing back.
func TestFallbackWindowWhileLaunchDisconnected(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", agentPrincipal("L2", 3))
	f := relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 3}

	l2, _ := a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
	_ = l2.Close()
	_ = relaytest.Wait(t, a.Served, "L2 session to end")

	_, fb := a.MustDial("a", relaytest.AgentHello(agentID, "", "", "pty"), conduit.Config{})
	res, err := resolveAgent(t, w, a, f)
	if err != nil || res.Record.SessionID != fb.GetSessionId() || res.Want.Incarnation != "gen-3" {
		t.Fatalf("during the window: resolved %+v, %v; want the gen-3 fallback", res.Record, err)
	}

	_, back := a.MustDial("a", relaytest.AgentHello(agentID, "L2", "", "pty"), conduit.Config{})
	res, err = resolveAgent(t, w, a, f)
	if err != nil || res.Record.SessionID != back.GetSessionId() || res.Want.Incarnation != "L2" {
		t.Fatalf("after L2 reconnects: resolved %+v, %v; want L2", res.Record, err)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// captureHandler records log records for assertions on structured fields.
type captureHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// count returns how many records carry the attribute key=value.
func (h *captureHandler) count(key, value string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && a.Value.String() == value {
				n++
			}
			return true
		})
	}
	return n
}
