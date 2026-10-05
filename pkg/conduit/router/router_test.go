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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const (
	agentID  = "agent-1"
	project  = "proj-1"
	brokerID = "broker-1"
)

var facts = relay.AgentIncarnationFacts{LaunchID: "L1", Generation: 1}

func agentReq(op router.Op) router.Request {
	return router.Request{Op: op, Kind: registry.PrincipalAgent, ID: agentID, Want: registry.Want{ProjectID: project}, Agent: facts}
}

func newRouter(t *testing.T, n *relaytest.Node) *router.Router {
	t.Helper()
	rt, err := router.New(router.Config{Relay: n.Relay, Registry: n.W.Registry, Store: n.W.Store, Peers: n.Peers, Now: n.W.Now})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func echoTarget() conduit.Config {
	return conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return err
			}
			go func() {
				buf := make([]byte, 64)
				n, _ := st.Read(buf)
				_, _ = st.Write(buf[:n])
				_ = st.Close()
			}()
			return nil
		}),
		RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: 200, Body: []byte("from target")}
		}),
	}
}

// TestRouterResolvesRemoteOwner (C15, T1): relay B resolves an agent held
// by relay A to A's session and reaches it for an RPC and a stream; on A
// the same request resolves to the local session.
func TestRouterResolvesRemoteOwner(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	b := w.StartNode("relay-b", nil)
	w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project, Agent: facts})
	_, wel := a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoTarget())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rb := newRouter(t, b)
	err := rb.Do(ctx, agentReq(router.OpStatefulRPC), func(ctx context.Context, res router.Resolved) error {
		if res.Local || res.Record.SessionID != wel.GetSessionId() || res.Record.RelayInstanceID != "relay-a" {
			t.Fatalf("resolved %+v local=%v", res.Record, res.Local)
		}
		if res.Want.Incarnation != "L1" || res.Want.ProjectID != project {
			t.Fatalf("resolved want %+v", res.Want)
		}
		resp, err := res.Session.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "exec"})
		if err != nil || string(resp.GetBody()) != "from target" {
			t.Fatalf("Call = %v, %v", resp, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = rb.Do(ctx, agentReq(router.OpStream), func(ctx context.Context, res router.Resolved) error {
		st, err := res.Session.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		if _, err := st.Write([]byte("ping")); err != nil {
			return err
		}
		buf := make([]byte, 4)
		if _, err := st.Read(buf); err != nil || string(buf) != "ping" {
			t.Fatalf("stream echo %q, %v", buf, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := newRouter(t, a).Resolve(ctx, agentReq(router.OpStream), nil)
	if err != nil || !res.Local {
		t.Fatalf("on the owner: %+v, %v; want local", res, err)
	}
}

// TestRouterReResolveBounded: a stale route is excluded and re-resolved
// at most MaxReResolves times, then ErrNoSession.
func TestRouterReResolveBounded(t *testing.T) {
	w := relaytest.NewWorld(t)
	var nodes []*relaytest.Node
	for _, id := range []string{"relay-a", "relay-b", "relay-c", "relay-d"} {
		nodes = append(nodes, w.StartNode(id, nil))
	}
	// One broker session per relay (brokers may hold several).
	w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
	for _, n := range nodes {
		n.MustDial("b", relaytest.BrokerHello(brokerID, "inc-1"), conduit.Config{})
	}
	req := router.Request{Op: router.OpStatefulRPC, Kind: registry.PrincipalBroker, ID: brokerID, BrokerIncarnation: "inc-1"}
	for _, tc := range []struct {
		name      string
		staleN    int
		wantCalls int
		wantErr   error
	}{
		{name: "first route good", staleN: 0, wantCalls: 1},
		{name: "two stale then good", staleN: 2, wantCalls: 3},
		{name: "always stale", staleN: 100, wantCalls: router.MaxReResolves + 1, wantErr: router.ErrNoSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := map[string]bool{}
			calls := 0
			err := newRouter(t, nodes[3]).Do(context.Background(), req, func(_ context.Context, res router.Resolved) error {
				calls++
				if seen[res.Record.SessionID] {
					t.Fatalf("session %s resolved twice; stale routes must be excluded", res.Record.SessionID)
				}
				seen[res.Record.SessionID] = true
				if calls <= tc.staleN {
					return &relay.StaleRouteError{Reason: "test"}
				}
				return nil
			})
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRouterFailsClosedOnReadError: a registry read error refuses the
// operation instead of guessing a route.
func TestRouterFailsClosedOnReadError(t *testing.T) {
	for _, op := range []string{registry.OpListPrincipalSessions, registry.OpListPrincipalSessionsBySession} {
		t.Run(op, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			a := w.StartNode("relay-a", nil)
			b := w.StartNode("relay-b", nil)
			w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project, Agent: facts})
			a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoTarget())
			w.SetFault(func(o string) error {
				if o == op {
					return errors.New("injected read error")
				}
				return nil
			})
			called := false
			err := newRouter(t, b).Do(context.Background(), agentReq(router.OpStream), func(context.Context, router.Resolved) error {
				called = true
				return nil
			})
			if !errors.Is(err, router.ErrRegistryUnavailable) || called {
				t.Fatalf("err = %v (called %v), want ErrRegistryUnavailable without calling", err, called)
			}
		})
	}
}

func TestRouterNoSession(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	w.SetPrincipal("a", relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project, Agent: facts})
	a.MustDial("a", relaytest.AgentHello(agentID, "L1", "", "pty"), echoTarget())
	rt := newRouter(t, a)
	for _, tc := range []struct {
		name string
		req  router.Request
	}{
		{name: "other agent", req: router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: "agent-2", Want: registry.Want{ProjectID: project}, Agent: facts}},
		{name: "superseded launch id", req: router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: agentID, Want: registry.Want{ProjectID: project},
			Agent: relay.AgentIncarnationFacts{LaunchID: "L2", Generation: 2}}},
		{name: "other project", req: router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: agentID, Want: registry.Want{ProjectID: "proj-2"}, Agent: facts}},
		{name: "exec scope not interchangeable", req: router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: agentID, Want: registry.Want{ProjectID: project, ExecScope: "scope-x"}, Agent: facts}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rt.Resolve(context.Background(), tc.req, nil); !errors.Is(err, router.ErrNoSession) {
				t.Fatalf("err = %v, want ErrNoSession", err)
			}
		})
	}
}

// TestRouterInvalidRequests: incomplete or unsafe requests are refused
// before any lookup.
func TestRouterInvalidRequests(t *testing.T) {
	w := relaytest.NewWorld(t)
	a := w.StartNode("relay-a", nil)
	rt := newRouter(t, a)
	base := agentReq(router.OpStream)
	for _, tc := range []struct {
		name string
		mod  func(r *router.Request)
	}{
		{name: "AnyExecScope on a stream", mod: func(r *router.Request) { r.Want.AnyExecScope = true }},
		{name: "AnyExecScope on a stateful rpc", mod: func(r *router.Request) { r.Op = router.OpStatefulRPC; r.Want.AnyExecScope = true }},
		{name: "caller-supplied incarnation", mod: func(r *router.Request) { r.Want.Incarnation = "L1" }},
		{name: "user target", mod: func(r *router.Request) { r.Kind = registry.PrincipalUser }},
		{name: "relay-peer target", mod: func(r *router.Request) { r.Kind = registry.PrincipalRelayPeer }},
		{name: "agent without project", mod: func(r *router.Request) { r.Want.ProjectID = "" }},
		{name: "broker without incarnation", mod: func(r *router.Request) {
			*r = router.Request{Op: router.OpStream, Kind: registry.PrincipalBroker, ID: brokerID}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mod(&req)
			if _, err := rt.Resolve(context.Background(), req, nil); !errors.Is(err, router.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	// AnyExecScope is fine for stateless RPCs.
	req := agentReq(router.OpStatelessRPC)
	req.Want.AnyExecScope = true
	if _, err := rt.Resolve(context.Background(), req, nil); !errors.Is(err, router.ErrNoSession) {
		t.Fatalf("stateless AnyExecScope: err = %v, want ErrNoSession (valid request, no session)", err)
	}
}
