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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Routed broker requests (design §7, broker RPC routing; contracts.md §2
// "Ambiguous broker results"). The fixture is a hub with its in-process
// relay and hub.conduit on, the dispatcher built by the production wiring
// (CreateAuthenticatedDispatcher), and broker conduit sessions dialed with
// the broker's credential that list the broker RPCs. The legacy control
// channel is a mock, so a fallback is observed directly.

// routedBrokerFixture is a brokerConduitFixture with the production broker
// client and a mock legacy control channel.
type routedBrokerFixture struct {
	*brokerConduitFixture
	client *HybridBrokerClient
	legacy *mockControlChannelTunnel
}

func newRoutedBrokerFixture(t *testing.T, mod func(*ConduitRelayOptions)) *routedBrokerFixture {
	t.Helper()
	f := &routedBrokerFixture{brokerConduitFixture: newBrokerConduitFixture(t, nil, mod)}
	f.srv.SetDispatcher(f.srv.CreateAuthenticatedDispatcher())
	f.client = f.srv.brokerClient
	require.NotNil(t, f.client, "the production wiring builds a HybridBrokerClient")
	f.legacy = &mockControlChannelTunnel{connected: true, body: []byte(`{"output":"legacy","exitCode":0}`)}
	f.client.controlChannel = &ControlChannelBrokerClient{manager: f.legacy, signer: &mockBrokerSigner{}}
	return f
}

// routedTestBroker is one broker conduit session with a scripted RPC
// handler that counts the operations it receives.
type routedTestBroker struct {
	ls        conduit.LocalSession
	sessionID string
	answer    func(ctx context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope

	mu    sync.Mutex
	calls map[string]int
}

func (b *routedTestBroker) count(op string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[op]
}

func (b *routedTestBroker) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.calls {
		n += c
	}
	return n
}

// brokerOpOf names the broker operation a tunnelled request performs.
func brokerOpOf(env *wsprotocol.RequestEnvelope) string {
	switch {
	case strings.HasSuffix(env.Path, "/exec"):
		return conduit.RPCAgentExec
	case strings.HasSuffix(env.Path, "/logs"):
		return conduit.RPCAgentLogs
	case env.Path == "/api/v1/images/status":
		return conduit.RPCImageStatus
	case env.Path == "/api/v1/images/pull":
		return conduit.RPCImagePull
	case env.Path == "/api/v1/images/local":
		return conduit.RPCImageDelete
	case strings.HasPrefix(env.Path, "/api/v1/projects/"):
		return conduit.RPCProjectCleanup
	}
	return "other"
}

// defaultBrokerAnswer is a healthy broker's answer to op.
func defaultBrokerAnswer(_ context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
	switch op {
	case conduit.RPCAgentExec:
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, []byte(`{"output":"routed","exitCode":0}`))
	case conduit.RPCAgentLogs:
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, []byte("routed logs"))
	case conduit.RPCImageStatus:
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, []byte(`{"local_short":{"exists":true}}`))
	case conduit.RPCProjectCleanup:
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusNoContent, nil, nil)
	}
	return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, nil)
}

// routedBrokerOpts tunes dialRoutedBroker.
type routedBrokerOpts struct {
	incarnation string   // default "proc-1"
	rpcs        []string // default every broker RPC; set noRPCs for none
	noRPCs      bool
	noHandler   bool // the session serves no RPCs (501 unsupported)
	interceptor conduit.Interceptor
	answer      func(ctx context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope
	clk         clock.Clock        // the broker session's clock (default real)
	handler     conduit.RPCHandler // replaces the scripted handler (answer)
}

// dialRoutedBroker opens a broker conduit session as the fixture's broker
// and returns once the hub registered it.
func (f *routedBrokerFixture) dialRoutedBroker(t *testing.T, o routedBrokerOpts) *routedTestBroker {
	t.Helper()
	b, cfg, hello := f.routedBrokerSession(o)
	d := &ws.Dialer{
		URL:    f.wsURL("/api/v1/conduit"),
		Header: func(context.Context) (http.Header, error) { return f.signedHeaderAs(t, f.brokerID, f.secret), nil },
	}
	return f.dialRoutedBrokerWith(t, d, b, cfg, hello)
}

// dialRoutedBrokerOn opens a broker conduit session as the fixture's broker
// on the second relay x (newTwoRelayRoutedFixture) and returns once the
// registry lists it.
func (f *routedBrokerFixture) dialRoutedBrokerOn(t *testing.T, x *routedExtraRelay, o routedBrokerOpts) *routedTestBroker {
	t.Helper()
	b, cfg, hello := f.routedBrokerSession(o)
	return f.dialRoutedBrokerWith(t, &ws.Dialer{URL: "ws" + strings.TrimPrefix(x.public.URL, "http") + "/"}, b, cfg, hello)
}

// dialRoutedBrokerWith dials b's session through d.
func (f *routedBrokerFixture) dialRoutedBrokerWith(t *testing.T, d *ws.Dialer, b *routedTestBroker, cfg conduit.Config, hello *conduitv1.Hello) *routedTestBroker {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, w, err := conduit.Dial(ctx, d, cfg, hello)
	require.NoError(t, err)
	b.ls = s.(conduit.LocalSession)
	b.sessionID = w.GetSessionId()
	t.Cleanup(func() { _ = b.ls.Close() })
	f.waitBrokerSession(t, w.GetSessionId())
	return b
}

// routedBrokerSession builds the scripted broker, its session config and
// its Hello from o.
func (f *routedBrokerFixture) routedBrokerSession(o routedBrokerOpts) (*routedTestBroker, conduit.Config, *conduitv1.Hello) {
	if o.incarnation == "" {
		o.incarnation = "proc-1"
	}
	if o.rpcs == nil && !o.noRPCs {
		o.rpcs = conduit.BrokerRPCs()
	}
	b := &routedTestBroker{answer: o.answer, calls: map[string]int{}}
	if b.answer == nil {
		b.answer = defaultBrokerAnswer
	}
	clk := o.clk
	if clk == nil {
		clk = clock.Real()
	}
	cfg := conduit.Config{Clock: clk, Interceptor: o.interceptor}
	if o.handler != nil {
		cfg.RPCHandler = o.handler
	} else if !o.noHandler {
		cfg.RPCHandler = conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			env := envelope.RPCToRequest(req)
			op := brokerOpOf(env)
			b.mu.Lock()
			b.calls[op]++
			b.mu.Unlock()
			resp := b.answer(ctx, op, env)
			if resp == nil {
				return nil
			}
			return envelope.ResponseToRPC(resp)
		})
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   f.brokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: o.rpcs, EndpointIncarnation: o.incarnation},
	}
	return b, cfg, hello
}

// routedExtraRelay is a second relay on the routed fixture's registry: the
// relay of another hub node, which the fixture's router reaches through
// its internal API.
type routedExtraRelay struct {
	relay  *relay.Relay
	public *httptest.Server
}

// newTwoRelayRoutedFixture is a routed fixture whose relay (hub-a) and a
// second relay (hub-b) share one registry and one relay-peer secret.
// hub-b checks its own address at start (RequireSelfCheck), so the
// fixture's router can reach it. A broker's sessions on two relays are
// each current at once (design §3.4: a broker session is current while no
// newer session of the broker exists on its relay), so a request retried
// after one session fails finds the other eligible, whatever the timing of
// row deletes.
func newTwoRelayRoutedFixture(t *testing.T) (*routedBrokerFixture, *routedExtraRelay) {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	peerAuth := func(self string) relay.PeerAuth {
		a, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: secret, SelfID: self})
		require.NoError(t, err)
		return a
	}
	f := newRoutedBrokerFixture(t, func(o *ConduitRelayOptions) { o.PeerAuth = peerAuth("hub-a") })

	var internal atomic.Pointer[http.Handler]
	in := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := internal.Load()
		if h == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(w, r)
	}))
	t.Cleanup(in.Close)
	r, err := relay.New(relay.Config{
		InstanceID:       "hub-b",
		InternalEndpoint: in.URL,
		RequireSelfCheck: true,
		Registry:         registry.New(f.regStore, registry.Config{}),
		Store:            f.regStore,
		Session:          conduit.Config{Clock: clock.Real()},
		GrantKeys:        func(context.Context) ([]*conduitv1.GrantKey, error) { return nil, nil },
		PeerAuth:         peerAuth("hub-b"),
		HTTPClient:       in.Client(),
	})
	require.NoError(t, err)
	h := r.InternalHandler()
	internal.Store(&h)
	require.NoError(t, r.Start(context.Background()))
	t.Cleanup(r.Kill)

	x := &routedExtraRelay{relay: r}
	principal := relay.Principal{Kind: registry.PrincipalBroker, ID: f.brokerID}
	x.public = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := ws.Upgrade(w, req, nil, ws.Options{})
		if err != nil {
			return
		}
		_ = r.Serve(context.Background(), conn, principal)
	}))
	t.Cleanup(x.public.Close)
	return f, x
}

// waitBrokerSession checks that the registry lists sessionID. The relay
// inserts the session's row during admission, before it sends the
// Welcome that conduit.Dial returns, so no wait is needed.
func (f *routedBrokerFixture) waitBrokerSession(t *testing.T, sessionID string) {
	t.Helper()
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalBroker, f.brokerID)
	require.NoError(t, err)
	for _, v := range ps.Sessions {
		if v.Session.SessionID == sessionID {
			return
		}
	}
	t.Fatalf("broker session %s is not registered after its Welcome", sessionID)
}

// seedRoutedAgent creates a project and an agent on the fixture's broker.
func (f *routedBrokerFixture) seedRoutedAgent(t *testing.T, mod func(*store.Agent)) *store.Agent {
	t.Helper()
	ctx := context.Background()
	proj := &store.Project{ID: uuid.NewString(), Name: "routed", Slug: "routed-" + uuid.NewString()[:8], OwnerID: uuid.NewString()}
	require.NoError(t, f.store.CreateProject(ctx, proj))
	a := &store.Agent{
		ID: uuid.NewString(), Name: "routed-agent", Slug: "ra-" + uuid.NewString()[:8],
		ProjectID: proj.ID, RuntimeBrokerID: f.brokerID,
	}
	if mod != nil {
		mod(a)
	}
	require.NoError(t, f.store.CreateAgent(ctx, a))
	return a
}

// TestHybridBrokerClient_TransientOpsRouted: every transient operation
// reaches the broker over its conduit session; the legacy channel is not
// used.
func TestHybridBrokerClient_TransientOpsRouted(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		op  string
		run func(c *HybridBrokerClient, brokerID string) error
	}{
		{conduit.RPCAgentLogs, func(c *HybridBrokerClient, id string) error {
			out, err := c.GetAgentLogs(ctx, id, "", "agent-a", "p1", 10)
			if err == nil && out != "routed logs" {
				return errors.New("unexpected logs " + out)
			}
			return err
		}},
		{conduit.RPCAgentExec, func(c *HybridBrokerClient, id string) error {
			out, code, err := c.ExecAgent(ctx, id, "", "agent-a", "p1", []string{"true"}, 5)
			if err == nil && (out != "routed" || code != 0) {
				return errors.New("unexpected exec output " + out)
			}
			return err
		}},
		{conduit.RPCImageStatus, func(c *HybridBrokerClient, id string) error {
			st, err := c.ImageStatus(ctx, id, "", "img:short", "")
			if err == nil && (st.LocalShort == nil || !st.LocalShort.Exists) {
				return errors.New("unexpected image status")
			}
			return err
		}},
		{conduit.RPCImagePull, func(c *HybridBrokerClient, id string) error { return c.PullImage(ctx, id, "", "img:1") }},
		{conduit.RPCImageDelete, func(c *HybridBrokerClient, id string) error { return c.DeleteImage(ctx, id, "", "img:1") }},
		{conduit.RPCProjectCleanup, func(c *HybridBrokerClient, id string) error {
			return c.CleanupProject(ctx, id, "", "proj-a", "p1")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			f := newRoutedBrokerFixture(t, nil)
			b := f.dialRoutedBroker(t, routedBrokerOpts{})
			require.NoError(t, tc.run(f.client, f.brokerID))
			assert.Equal(t, 1, b.count(tc.op), "the broker served the operation over conduit")
			assert.Equal(t, 0, f.legacy.calls, "the legacy channel was not used")
		})
	}
}

// TestHybridBrokerClient_NoCapabilityUsesFallback: an operation the
// broker's session does not list goes to the fallback.
func TestHybridBrokerClient_NoCapabilityUsesFallback(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	b := f.dialRoutedBroker(t, routedBrokerOpts{rpcs: []string{conduit.RPCAgentLogs}})
	out, _, err := f.client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
	require.NoError(t, err)
	assert.Equal(t, "legacy", out)
	assert.Equal(t, 1, f.legacy.calls)
	assert.Equal(t, 0, b.total())
}

// TestHybridBrokerClient_ExperimentOffUnchanged: with hub.conduit off,
// transient operations take the legacy path even when a broker session
// lists them.
func TestHybridBrokerClient_ExperimentOffUnchanged(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	b := f.dialRoutedBroker(t, routedBrokerOpts{})
	setConduitExperiment(t, f.srv, false)
	out, _, err := f.client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
	require.NoError(t, err)
	assert.Equal(t, "legacy", out)
	assert.Equal(t, 1, f.legacy.calls)
	assert.Equal(t, 0, b.total())
}

// TestHybridBrokerClient_LifecycleStaysOnRoute: durable operations keep
// route() and never go over the broker's conduit session.
func TestHybridBrokerClient_LifecycleStaysOnRoute(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	b := f.dialRoutedBroker(t, routedBrokerOpts{})
	require.NoError(t, f.client.StopAgent(context.Background(), f.brokerID, "", "agent-a", "p1", ""))
	assert.Equal(t, 1, f.legacy.calls)
	assert.Equal(t, 0, b.total())
}

// TestRoutedBrokerRPC_UnsupportedRefusalFallsBack: a session that lists
// the operation but serves no RPCs refuses with the marked 501; nothing
// ran, so the request takes the fallback.
func TestRoutedBrokerRPC_UnsupportedRefusalFallsBack(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	f.dialRoutedBroker(t, routedBrokerOpts{noHandler: true})
	out, _, err := f.client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
	require.NoError(t, err)
	assert.Equal(t, "legacy", out)
	assert.Equal(t, 1, f.legacy.calls)
}

// TestRoutedBrokerRPC_BrokerStatus501PassesThrough: a broker handler's own
// 501, even one that sets the refusal header, is the broker's answer: it
// is returned as such and never taken as a session refusal.
func TestRoutedBrokerRPC_BrokerStatus501PassesThrough(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	b := f.dialRoutedBroker(t, routedBrokerOpts{answer: func(_ context.Context, _ string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusNotImplemented,
			map[string]string{conduit.RefusalHeader: conduit.RefusalUnsupported},
			[]byte(`{"error":{"code":"runtime_logs_unsupported","message":"no logs"}}`))
	}})
	_, err := f.client.GetAgentLogs(context.Background(), f.brokerID, "", "agent-a", "p1", 0)
	var se *brokerStatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusNotImplemented, se.StatusCode)
	assert.Equal(t, brokerCodeRuntimeLogsUnsupported, se.brokerErrorCode())
	assert.Equal(t, 1, b.count(conduit.RPCAgentLogs))
	assert.Equal(t, 0, f.legacy.calls, "a broker answer is never a fallback")
}

// TestRoutedBrokerRPC_RefusalMarkerNeverForwarded: a broker reply never
// carries the refusal marker to the hub's callers.
func TestRoutedBrokerRPC_RefusalMarkerNeverForwarded(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	f.dialRoutedBroker(t, routedBrokerOpts{answer: func(_ context.Context, _ string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusServiceUnavailable,
			map[string]string{conduit.RefusalHeader: conduit.RefusalDraining, "X-Broker": "yes"}, nil)
	}})
	tun := f.srv.newRoutedBrokerTunnel()
	resp, err := tun.TunnelRequest(withBrokerRPC(context.Background(), conduit.RPCAgentLogs, true), f.brokerID,
		wsprotocol.NewRequestEnvelope("r-1", http.MethodGet, "/api/v1/agents/a/logs", "", nil, nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "the broker's own 503 is its answer")
	assert.Equal(t, "yes", resp.Headers["X-Broker"])
	for k := range resp.Headers {
		assert.NotEqual(t, strings.ToLower(conduit.RefusalHeader), strings.ToLower(k), "refusal marker forwarded")
	}
	assert.Equal(t, 0, f.legacy.calls)
}

// drainOnExec returns an Interceptor for a broker session that puts the
// session into drain (GoAway) when one of the first limit exec RpcRequests
// arriving across the sessions sharing arrivals reaches it, before the
// request is dispatched, so the session layer refuses it with the marked
// 503. drains counts the sessions drained.
func drainOnExec(arrivals, drains *atomic.Int32, ls *atomic.Pointer[conduit.LocalSession], limit int32) conduit.Interceptor {
	return func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		req := f.GetRpcRequest()
		if dir == conduit.Inbound && req != nil && strings.HasSuffix(req.GetPath(), "/exec") && arrivals.Add(1) <= limit {
			if p := ls.Load(); p != nil {
				drains.Add(1)
				_ = (*p).GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute})
			}
		}
		return []*conduitv1.Frame{f}
	}
}

// drainRetryWorld is a router on relay "relay-r" and one session of the
// same broker on each of n other relays, so every session is current
// (broker epoch currency is per relay, design §3.4). The first drainFirst
// exec requests to arrive put their session into drain. Each session holds
// one RPC in flight, so a GoAway leaves it draining instead of closing it.
type drainRetryWorld struct {
	client *HybridBrokerClient
	legacy *mockControlChannelTunnel
	execs  []*atomic.Int32 // exec requests each session ran
	drains *atomic.Int32   // sessions that refused an exec while draining
}

func newDrainRetryWorld(t *testing.T, n int, drainFirst int32) *drainRetryWorld {
	t.Helper()
	w := relaytest.NewWorld(t)
	r := w.StartNode("relay-r", nil)
	const brokerID = "broker-dr"
	w.SetPrincipal("bdr", relay.Principal{Kind: registry.PrincipalBroker, ID: brokerID, Incarnation: "proc-dr"})
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		Capabilities:  &conduitv1.Capabilities{Rpc: conduit.BrokerRPCs(), EndpointIncarnation: "proc-dr"},
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	entered := make(chan struct{}, n)
	arrivals := &atomic.Int32{}
	d := &drainRetryWorld{drains: &atomic.Int32{}}
	for i := 0; i < n; i++ {
		node := w.StartNode(fmt.Sprintf("relay-%d", i), nil)
		execs := &atomic.Int32{}
		d.execs = append(d.execs, execs)
		var ls atomic.Pointer[conduit.LocalSession]
		handler := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			env := envelope.RPCToRequest(req)
			if env.Path == "/hold" {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, nil))
			}
			if strings.HasSuffix(env.Path, "/exec") {
				execs.Add(1)
			}
			return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, nil, []byte(`{"output":"routed","exitCode":0}`)))
		})
		target, welcome := node.MustDial("bdr", hello, conduit.Config{RPCHandler: handler, Interceptor: drainOnExec(arrivals, d.drains, &ls, drainFirst)})
		ls.Store(&target)
		relaySide, _, ok := node.Relay.Local(context.Background(), welcome.GetSessionId())
		require.True(t, ok, "relay %d holds the broker session", i)
		go func() {
			_, _ = relaySide.Call(context.Background(), &conduitv1.RpcRequest{Method: http.MethodGet, Path: "/hold"})
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("held RPC did not reach the session on relay %d", i)
		}
	}

	rt, err := router.New(router.Config{Relay: r.Relay, Registry: w.Registry, Store: w.Store, Peers: r.Peers, Now: w.Now, Brokers: noBrokerControlChannel{}})
	require.NoError(t, err)
	d.legacy = &mockControlChannelTunnel{connected: true, body: []byte(`{"output":"legacy","exitCode":0}`)}
	d.client = &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: d.legacy, signer: &mockBrokerSigner{}},
		httpClient:     &mockRuntimeBrokerClient{},
	}
	d.client.SetConduitRouting(&routedBrokerTunnel{
		router:    func() brokerRPCRouter { return rt },
		enabled:   func() bool { return true },
		execScope: func(context.Context, string) (string, error) { return "", nil },
	})
	return d
}

func (d *drainRetryWorld) execTotal() int32 {
	var n int32
	for _, e := range d.execs {
		n += e.Load()
	}
	return n
}

// TestRoutedBrokerRPC_DrainRefusalRetriedOnceOnReplacement: a draining
// session's refusal is retried once on a replacement session (here on
// another relay, where it is current); a second draining refusal takes
// the fallback, although a third session could serve the request.
//
// If the retry broke (the refusal fell back at once), subtest 1 would get
// the legacy answer and no session would run the exec; if the refusal
// were taken as the broker's answer, ExecAgent would return its 503. If
// the one-retry limit broke, subtest 2 would reach the third session,
// which serves the exec, and get the routed answer instead of the legacy
// one.
func TestRoutedBrokerRPC_DrainRefusalRetriedOnceOnReplacement(t *testing.T) {
	t.Run("retried on the replacement", func(t *testing.T) {
		d := newDrainRetryWorld(t, 2, 1)
		out, _, err := d.client.ExecAgent(context.Background(), "broker-dr", "", "agent-a", "p1", []string{"true"}, 5)
		require.NoError(t, err)
		assert.Equal(t, int32(1), d.drains.Load(), "the first session refused while draining")
		assert.Equal(t, "routed", out, "served by the replacement, not the fallback")
		assert.Equal(t, int32(1), d.execTotal(), "exactly one session ran the request")
		assert.Equal(t, 0, d.legacy.calls)
	})
	t.Run("a second drain falls back", func(t *testing.T) {
		d := newDrainRetryWorld(t, 3, 2)
		out, _, err := d.client.ExecAgent(context.Background(), "broker-dr", "", "agent-a", "p1", []string{"true"}, 5)
		require.NoError(t, err)
		assert.Equal(t, int32(2), d.drains.Load(), "two sessions refused while draining")
		assert.Equal(t, "legacy", out, "the second drain takes the fallback, not the third session")
		assert.Equal(t, 1, d.legacy.calls)
		assert.Equal(t, int32(0), d.execTotal(), "no session ran the request")
	})
}

// dropResponseThenClose returns an Interceptor for a broker whose ack is
// lost: the broker runs the request, its response frame is dropped, and
// the session then ends.
func dropResponseThenClose(ls *atomic.Pointer[conduit.LocalSession]) conduit.Interceptor {
	return func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if dir == conduit.Outbound && f.GetRpcResponse() != nil {
			if p := ls.Load(); p != nil {
				go func() { _ = (*p).Close() }()
			}
			return nil
		}
		return []*conduitv1.Frame{f}
	}
}

// TestRoutedExec_AckDroppedIsOutcomeUnknown (criterion 20 T7): an exec
// whose ack is dropped answers 502 outcome_unknown with the fixed message
// and details, carries no output, and is not run again, although another
// session of the broker could serve it: the two sessions are on two
// relays, each current on its own, so the other session is eligible and a
// wrong retry would run the exec a second time.
func TestRoutedExec_AckDroppedIsOutcomeUnknown(t *testing.T) {
	f, hubB := newTwoRelayRoutedFixture(t)
	agent := f.seedRoutedAgent(t, nil)
	var p1, p2 atomic.Pointer[conduit.LocalSession]
	b1 := f.dialRoutedBroker(t, routedBrokerOpts{interceptor: dropResponseThenClose(&p1)})
	p1.Store(&b1.ls)
	b2 := f.dialRoutedBrokerOn(t, hubB, routedBrokerOpts{interceptor: dropResponseThenClose(&p2)})
	p2.Store(&b2.ls)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/exec", map[string]interface{}{"command": []string{"touch", "x"}})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeOutcomeUnknown, body.Error.Code)
	assert.Equal(t, brokerOutcomeUnknownMessage, body.Error.Message)
	assert.Equal(t, conduit.RPCAgentExec, body.Error.Details["operation"])
	assert.Equal(t, f.brokerID, body.Error.Details["broker_id"])
	assert.NotEmpty(t, body.Error.Details["request_id"])
	assert.NotContains(t, rec.Body.String(), "routed", "no command output")
	assert.Equal(t, 1, b1.count(conduit.RPCAgentExec)+b2.count(conduit.RPCAgentExec), "the exec ran exactly once")
	assert.Equal(t, 0, f.legacy.calls, "never retried on the fallback either")
}

// TestHandleAgentExec_RuntimeErrorUnchanged: a broker that answers exec
// with an error still gives 502 runtime_error (the outcome is known).
func TestHandleAgentExec_RuntimeErrorUnchanged(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	agent := f.seedRoutedAgent(t, nil)
	f.dialRoutedBroker(t, routedBrokerOpts{answer: func(_ context.Context, _ string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
		return wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusInternalServerError, nil,
			[]byte(`{"error":{"code":"runtime_error","message":"exec failed"}}`))
	}})
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/exec", map[string]interface{}{"command": []string{"false"}})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"`+ErrCodeRuntimeError+`"`)
	assert.NotContains(t, rec.Body.String(), ErrCodeOutcomeUnknown)
}

// TestRoutedCleanupProject_NonSuccessIsError: on the routed path a broker
// failure is returned as an error, and 404 means there is nothing left.
func TestRoutedCleanupProject_NonSuccessIsError(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusConflict, true},
		{http.StatusNotFound, false},
		{http.StatusNoContent, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newRoutedBrokerFixture(t, nil)
			b := f.dialRoutedBroker(t, routedBrokerOpts{answer: func(_ context.Context, _ string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
				return wsprotocol.NewResponseEnvelope(env.RequestID, tc.status, nil, nil)
			}})
			err := f.client.CleanupProject(context.Background(), f.brokerID, "", "proj-a", "p1")
			if tc.wantErr {
				var se *brokerStatusError
				require.ErrorAs(t, err, &se)
				assert.Equal(t, tc.status, se.StatusCode)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, 1, b.count(conduit.RPCProjectCleanup))
			assert.Equal(t, 0, f.legacy.calls)
		})
	}
}

// TestRoutedCleanupProject_LostResultRetried: cleanup is idempotent, so a
// result lost with its session is retried on another session. The two
// sessions are on two relays, each current on its own, so the other
// session is eligible as soon as the first fails: without the retry the
// call fails or falls back, and the lost session has closed.
func TestRoutedCleanupProject_LostResultRetried(t *testing.T) {
	f, hubB := newTwoRelayRoutedFixture(t)
	var lost atomic.Bool
	var p1, p2 atomic.Pointer[conduit.LocalSession]
	dropOnce := func(p *atomic.Pointer[conduit.LocalSession]) conduit.Interceptor {
		drop := dropResponseThenClose(p)
		return func(dir conduit.Direction, fr *conduitv1.Frame) []*conduitv1.Frame {
			if dir == conduit.Outbound && fr.GetRpcResponse() != nil && lost.CompareAndSwap(false, true) {
				return drop(dir, fr)
			}
			return []*conduitv1.Frame{fr}
		}
	}
	b1 := f.dialRoutedBroker(t, routedBrokerOpts{interceptor: dropOnce(&p1)})
	p1.Store(&b1.ls)
	b2 := f.dialRoutedBrokerOn(t, hubB, routedBrokerOpts{interceptor: dropOnce(&p2)})
	p2.Store(&b2.ls)

	require.NoError(t, f.client.CleanupProject(context.Background(), f.brokerID, "", "proj-a", "p1"))
	assert.True(t, lost.Load(), "one result was lost")
	assert.Equal(t, 1, b1.count(conduit.RPCProjectCleanup), "the session on hub-a ran it once")
	assert.Equal(t, 1, b2.count(conduit.RPCProjectCleanup), "the session on hub-b ran it once: one of them is the retry")
	assert.Equal(t, 0, f.legacy.calls)
}

// TestRoutedBrokerRPC_GoAway: on GoAway an in-flight request continues to
// the drain deadline; at the deadline the session-end rule applies.
func TestRoutedBrokerRPC_GoAway(t *testing.T) {
	t.Run("in flight completes before the deadline", func(t *testing.T) {
		f := newRoutedBrokerFixture(t, nil)
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		b := f.dialRoutedBroker(t, routedBrokerOpts{answer: func(ctx context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
			entered <- struct{}{}
			<-release
			return defaultBrokerAnswer(ctx, op, env)
		}})
		type result struct {
			out string
			err error
		}
		done := make(chan result, 1)
		go func() {
			out, _, err := f.client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
			done <- result{out, err}
		}()
		waitSignal(t, entered, "exec reached the broker")
		require.NoError(t, b.ls.GoAway(conduit.GoAwayOptions{DrainDeadline: time.Minute}))
		close(release)
		r := <-done
		require.NoError(t, r.err)
		assert.Equal(t, "routed", r.out)
	})
	for _, tc := range []struct {
		name string
		op   string
	}{
		{"deadline: exec is outcome unknown", conduit.RPCAgentExec},
		{"deadline: logs retried on another session", conduit.RPCAgentLogs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One session per relay: the session that did not hold the
			// request is eligible at once, so a retry (logs) reaches it and
			// a wrong retry (exec) would run there.
			f, hubB := newTwoRelayRoutedFixture(t)
			entered := make(chan struct{}, 4)
			var drained atomic.Bool
			hold := func(ctx context.Context, op string, env *wsprotocol.RequestEnvelope) *wsprotocol.ResponseEnvelope {
				if drained.CompareAndSwap(false, true) {
					entered <- struct{}{}
					<-ctx.Done() // past the drain deadline: the session cancels it
					return nil
				}
				return defaultBrokerAnswer(ctx, op, env)
			}
			// The broker sessions' drain deadline runs on a fake clock.
			fake := clock.NewFake(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
			b1 := f.dialRoutedBroker(t, routedBrokerOpts{answer: hold, clk: fake})
			b2 := f.dialRoutedBrokerOn(t, hubB, routedBrokerOpts{answer: hold, clk: fake})
			errc := make(chan error, 1)
			var logs string
			go func() {
				var err error
				if tc.op == conduit.RPCAgentExec {
					_, _, err = f.client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
				} else {
					logs, err = f.client.GetAgentLogs(context.Background(), f.brokerID, "", "agent-a", "p1", 0)
				}
				errc <- err
			}()
			waitSignal(t, entered, "request reached the broker")
			holder := b1
			if b2.count(tc.op) == 1 {
				holder = b2
			}
			require.NoError(t, holder.ls.GoAway(conduit.GoAwayOptions{DrainDeadline: 10 * time.Second}))
			fake.Advance(11 * time.Second) // past the drain deadline
			err := <-errc
			if tc.op == conduit.RPCAgentExec {
				var ou *brokerOutcomeUnknownError
				require.ErrorAs(t, err, &ou)
				assert.Equal(t, 1, b1.count(tc.op)+b2.count(tc.op), "exec not re-run")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "routed logs", logs)
				assert.Equal(t, 1, b1.count(tc.op), "each session ran logs once: the holder, then the retry on the other")
				assert.Equal(t, 1, b2.count(tc.op), "each session ran logs once: the holder, then the retry on the other")
			}
			assert.Equal(t, 0, f.legacy.calls)
		})
	}
}

// TestRoutedBrokerRPC_EncodeFailureFailsOnlyThatRPC (no regression of
// #4333 on the routed path): a routed attempt whose request cannot be
// encoded fails alone, without a fallback or a retry; the broker's
// session stays up and serves the next request. The envelope already
// replaces invalid UTF-8, so the unencodable request is given to the
// routed attempt directly, on the real hub-side session.
func TestRoutedBrokerRPC_EncodeFailureFailsOnlyThatRPC(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	b := f.dialRoutedBroker(t, routedBrokerOpts{})
	rt := f.srv.conduit.Load()
	require.NotNil(t, rt)
	hubSide, rec, ok := rt.relay.Local(context.Background(), b.sessionID)
	require.True(t, ok, "the hub holds the broker session")
	tun := f.srv.newRoutedBrokerTunnel()
	spec := brokerRPC{op: conduit.RPCAgentLogs, idempotent: true}
	res := router.Resolved{Session: hubSide, Record: rec, Local: true}

	var st routedAttempt
	bad := &conduitv1.RpcRequest{Method: http.MethodGet, Path: "/api/v1/agents/\xff/logs"}
	_, err := tun.attempt(context.Background(), res, bad, spec, &st)
	require.ErrorIs(t, err, conduit.ErrRPCEncode)
	var perm *routedPermanentError
	require.ErrorAs(t, err, &perm, "an unencodable request ends the request: no re-resolve, no fallback")
	assert.False(t, st.written, "nothing was sent")
	select {
	case <-hubSide.Done():
		t.Fatalf("the hub-side session ended: %v", hubSide.Err())
	case <-b.ls.Done():
		t.Fatalf("the broker session ended: %v", b.ls.Err())
	default:
	}

	resp, err := tun.TunnelRequest(withBrokerRPC(context.Background(), conduit.RPCAgentLogs, true), f.brokerID,
		wsprotocol.NewRequestEnvelope("good", http.MethodGet, "/api/v1/agents/a/logs", "", nil, nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, b.count(conduit.RPCAgentLogs), "only the encodable request reached the broker")
	assert.Equal(t, 0, f.legacy.calls)
}

// TestRoutedBrokerRPC_PinnedPlacementBeforeRouting: the pinned-placement
// check refuses before any routing, with a live routed session present.
func TestRoutedBrokerRPC_PinnedPlacementBeforeRouting(t *testing.T) {
	for _, tc := range []struct {
		name, method, suffix string
		body                 interface{}
	}{
		{"exec", http.MethodPost, "/exec", map[string]interface{}{"command": []string{"true"}}},
		{"logs", http.MethodGet, "/logs", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutedBrokerFixture(t, nil)
			b := f.dialRoutedBroker(t, routedBrokerOpts{})
			// Pinned to the broker it is assigned to, which serves no
			// single runtime target: a stale pin.
			agent := f.seedRoutedAgent(t, nil)
			_, err := f.store.SetAgentPinnedRuntimeTarget(context.Background(), agent.ID,
				store.PinnedPlacement{RuntimeBrokerID: f.brokerID},
				store.PinnedPlacement{RuntimeBrokerID: f.brokerID, RuntimeTargetID: "target-1", RuntimeTargetType: "docker"})
			require.NoError(t, err)
			rec := doRequest(t, f.srv, tc.method, "/api/v1/agents/"+agent.ID+tc.suffix, tc.body)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "runtime_target_pin_stale")
			assert.Equal(t, 0, b.total(), "nothing reached the broker")
			assert.Equal(t, 0, f.legacy.calls)
		})
	}
}

// TestRoutedBrokerRPC_AffinityColumnsUnchanged: routed requests leave the
// legacy broker affinity columns as the legacy channel wrote them.
func TestRoutedBrokerRPC_AffinityColumnsUnchanged(t *testing.T) {
	f := newRoutedBrokerFixture(t, nil)
	ctx := context.Background()
	require.NoError(t, f.store.ClaimRuntimeBrokerConnection(ctx, f.brokerID, "hub-legacy", "cc-session-1"))
	before, err := f.store.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	b := f.dialRoutedBroker(t, routedBrokerOpts{})
	_, _, err = f.client.ExecAgent(ctx, f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
	require.NoError(t, err)
	require.Equal(t, 1, b.count(conduit.RPCAgentExec))
	after, err := f.store.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	require.NotNil(t, after.ConnectedHubID)
	assert.Equal(t, "hub-legacy", *after.ConnectedHubID)
	assert.Equal(t, before.ConnectedSessionID, after.ConnectedSessionID)
	assert.Equal(t, before.ConnectedAt, after.ConnectedAt)
}

// brokerRPCFallbackCounter wires the fallback counter on s and returns a
// reader of its count for a reason.
func brokerRPCFallbackCounter(t *testing.T, s *Server) func(reason string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelConduitBrokerRPCMetrics(mp)
	require.NoError(t, err)
	s.SetConduitBrokerRPCMetrics(rec)
	return func(reason string) int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "scion.hub.conduit.broker_rpc_fallback" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value("reason"); ok && v.AsString() == reason {
						total += dp.Value
					}
				}
			}
		}
		return total
	}
}

// TestRoutedBrokerRPC_RegistryUnavailableFallsBack: with the registry
// unreadable the request takes the fallback, and the fallback is counted.
func TestRoutedBrokerRPC_RegistryUnavailableFallsBack(t *testing.T) {
	var fail atomic.Bool
	f := newRoutedBrokerFixture(t, func(o *ConduitRelayOptions) {
		faulty := &registry.FaultStore{Inner: o.Store, Fault: func(_ context.Context, op string) error {
			if fail.Load() && op == registry.OpListPrincipalSessions {
				return errors.New("registry test fault")
			}
			return nil
		}}
		o.Store = faulty
		o.Registry = registry.New(faulty, registry.Config{})
	})
	count := brokerRPCFallbackCounter(t, f.srv)
	b := f.dialRoutedBroker(t, routedBrokerOpts{})
	fail.Store(true)
	defer fail.Store(false)

	out, _, err := f.client.ExecAgent(context.Background(), f.brokerID, "", "agent-a", "p1", []string{"true"}, 5)
	require.NoError(t, err)
	assert.Equal(t, "legacy", out)
	assert.Equal(t, 1, f.legacy.calls)
	assert.Equal(t, 0, b.total())
	assert.Equal(t, int64(1), count(brokerRPCFallbackRegistryUnavailable))
}

// waitSignal waits for one value on ch.
func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out: %s", what)
	}
}
