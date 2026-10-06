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
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
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

// front stands in for a proxy or mesh in front of a relay's internal
// endpoint, so a test can make the endpoint fail in ways the relay itself
// would not.
type front struct {
	srv       *httptest.Server
	mode      atomic.Int32
	forwarded atomic.Int32 // requests handed to the relay
}

const (
	frontForward        int32 = iota
	frontBare503              // 503 with no refusal reason
	frontOtherReason503       // 503 whose reason is not a pre-admission refusal
	frontLoseResponse         // deliver to the relay, then drop the connection
)

func newFront(t *testing.T, target string) *front {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	f := &front{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch f.mode.Load() {
		case frontBare503:
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		case frontLoseResponse:
			f.forwarded.Add(1)
			rp.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		default:
			f.forwarded.Add(1)
			rp.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestRouterExcludesUnreachableOwner (design §3.5): a failed dial to an
// owner excludes that relay and re-resolves within the bound.
// With no alternative the original error is returned. A bare 503, a
// status produced by the target, a lost response and a failure after
// admission are not re-resolved.
func TestRouterExcludesUnreachableOwner(t *testing.T) {
	const live = "relay-live"
	type env struct {
		fronts map[string]*front
		nodes  map[string]*relaytest.Node
	}
	dialFail := func(e env, id string) { e.fronts[id].srv.Close() }
	frontMode := func(m int32) func(env, string) {
		return func(e env, id string) { e.fronts[id].mode.Store(m) }
	}
	isOwnerUnreachable := func(err error) error {
		var ce *conduit.CloseError
		switch {
		case !errors.Is(err, relay.ErrOwnerUnreachable):
			return errors.New("want ErrOwnerUnreachable")
		case errors.Is(err, router.ErrNoSession):
			return errors.New("want the owner's error, not ErrNoSession")
		case !errors.As(err, &ce) && !strings.Contains(err.Error(), "dialing owner relay"):
			return errors.New("want the original dial or close error")
		}
		return nil
	}
	notRetriable := func(err error) error {
		if err == nil || errors.Is(err, relay.ErrOwnerUnreachable) || errors.Is(err, router.ErrNoSession) {
			return errors.New("want a non-retriable owner error")
		}
		return nil
	}
	blockingTarget := conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		<-ctx.Done()
		return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: 200}
	})}
	target503 := conduit.Config{RPCHandler: conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		return &conduitv1.RpcResponse{RequestId: req.GetRequestId(), Status: http.StatusServiceUnavailable, Body: []byte("target busy")}
	})}

	for _, tc := range []struct {
		name    string
		stream  bool
		failing []string // dialled after the live relay, so ranked first
		live    bool
		target  conduit.Config // the failing relays' sessions
		mod     func(*relay.Config)
		breakFn func(env, string)
		want    []string // relays tried, in order
		check   func(error) error
		status  int32 // RpcResponse status seen by the last call (rpc)
		// delivered: the request reached relay-a exactly once.
		delivered bool
	}{
		{name: "dial failure re-resolves (rpc)", failing: []string{"relay-a"}, live: true, breakFn: dialFail,
			want: []string{"relay-a", live}, status: 200},
		{name: "dial failure re-resolves (stream)", stream: true, failing: []string{"relay-a"}, live: true, breakFn: dialFail,
			want: []string{"relay-a", live}},
		{name: "excluded relays are not chosen again", failing: []string{"relay-a", "relay-b"}, live: true, breakFn: dialFail,
			want: []string{"relay-b", "relay-a", live}, status: 200},
		{name: "bound reached returns the owner error", failing: []string{"relay-a", "relay-b", "relay-c"}, live: true, breakFn: dialFail,
			want: []string{"relay-c", "relay-b", "relay-a"}, check: isOwnerUnreachable},
		{name: "no alternative after a dial failure", failing: []string{"relay-a"}, breakFn: dialFail,
			want: []string{"relay-a"}, check: isOwnerUnreachable},
		{name: "bare 503 is not re-resolved (rpc)", failing: []string{"relay-a"}, live: true, breakFn: frontMode(frontBare503),
			want: []string{"relay-a"}, check: notRetriable},
		{name: "bare 503 is not re-resolved (stream)", stream: true, failing: []string{"relay-a"}, live: true, breakFn: frontMode(frontBare503),
			want: []string{"relay-a"}, check: notRetriable},
		{name: "target 503 in the RpcResponse is not re-resolved", failing: []string{"relay-a"}, live: true, target: target503,
			want: []string{"relay-a"}, status: http.StatusServiceUnavailable},
		{name: "lost response is not re-resolved", failing: []string{"relay-a"}, live: true, breakFn: frontMode(frontLoseResponse),
			want: []string{"relay-a"}, check: notRetriable, delivered: true},
		{name: "failure after admission is not re-resolved", failing: []string{"relay-a"}, live: true, target: blockingTarget,
			mod: func(c *relay.Config) { c.RPCTimeout = 50 * time.Millisecond }, want: []string{"relay-a"}, check: notRetriable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			w.SetPrincipal("b", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "inc-1"})
			e := env{fronts: map[string]*front{}, nodes: map[string]*relaytest.Node{}}
			if tc.live {
				n := w.StartNode(live, nil)
				n.MustDial("b", relaytest.BrokerHello(brokerID, "inc-1"), echoTarget())
			}
			for _, id := range tc.failing {
				n := w.StartNode(id, func(c *relay.Config) {
					f := newFront(t, c.InternalEndpoint)
					c.InternalEndpoint = f.srv.URL
					e.fronts[id] = f
					if tc.mod != nil {
						tc.mod(c)
					}
				})
				e.nodes[id] = n
				target := tc.target
				if target.RPCHandler == nil {
					target = echoTarget()
				}
				n.MustDial("b", relaytest.BrokerHello(brokerID, "inc-1"), target)
			}
			if tc.breakFn != nil {
				for _, id := range tc.failing {
					tc.breakFn(e, id)
				}
			}
			before := map[string]int32{}
			for id, f := range e.fronts {
				before[id] = f.forwarded.Load()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rt := newRouter(t, w.StartNode("relay-r", nil))
			req := router.Request{Op: router.OpStatefulRPC, Kind: registry.PrincipalBroker, ID: brokerID, BrokerIncarnation: "inc-1"}
			if tc.stream {
				req.Op = router.OpStream
			}
			var tried []string
			var status int32
			err := rt.Do(ctx, req, func(ctx context.Context, res router.Resolved) error {
				tried = append(tried, res.Record.RelayInstanceID)
				if tc.stream {
					st, err := res.Session.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
					if err != nil {
						return err
					}
					return st.Close()
				}
				resp, err := res.Session.Call(ctx, &conduitv1.RpcRequest{RequestId: "r1", Method: "exec"})
				if err != nil {
					return err
				}
				status = resp.GetStatus()
				return nil
			})
			if !slices.Equal(tried, tc.want) {
				t.Fatalf("relays tried = %v, want %v (err %v)", tried, tc.want, err)
			}
			if tc.check != nil {
				if cerr := tc.check(err); cerr != nil {
					t.Fatalf("err = %v: %v", err, cerr)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want success", err)
			}
			if status != tc.status {
				t.Fatalf("RpcResponse status = %d, want %d", status, tc.status)
			}
			if tc.delivered {
				if got := e.fronts["relay-a"].forwarded.Load() - before["relay-a"]; got != 1 {
					t.Fatalf("requests delivered to relay-a = %d, want 1", got)
				}
			}
		})
	}
}
