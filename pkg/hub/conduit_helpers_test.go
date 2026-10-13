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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/envelope"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/idtoken"
)

type hubTunnelTransport struct {
	name string
	// wrap returns the tunnel ControlChannelBrokerClient uses to reach m.
	wrap func(t *testing.T, m *mockControlChannelTunnel) controlChannelTunnel
}

// runOverHubTunnels runs test once per transport, as a subtest named after
// it.
func runOverHubTunnels(t *testing.T, test func(t *testing.T, tr hubTunnelTransport)) {
	for _, tr := range hubTunnelTransports {
		t.Run(tr.name, func(t *testing.T) { test(t, tr) })
	}
}

type silentBrokerTransport struct {
	name string
	// new returns a silent broker whose requests time out after timeout.
	new func(t *testing.T, timeout time.Duration) silentBroker
}

// runOverSilentBrokers runs test once per transport, as a subtest named
// after it.
func runOverSilentBrokers(t *testing.T, test func(t *testing.T, tr silentBrokerTransport)) {
	for _, tr := range silentBrokerTransports {
		t.Run(tr.name, func(t *testing.T) { test(t, tr) })
	}
}

// echoBroker answers every RpcRequest with 200, echoing the request's
// method, path, query, headers and body back in a response, and records
// the request envelope it saw.
type echoBroker struct {
	mu   sync.Mutex
	seen []*wsprotocol.RequestEnvelope
}

func newEchoTunnel(t *testing.T) (*conduitBrokerTunnel, *echoBroker, conduit.LocalSession) {
	echo := &echoBroker{}
	hub, _ := newBrokerConduitPair(t, conduit.Config{RPCHandler: echo})
	return newConduitBrokerTunnel(func(string) conduit.Session { return hub }, 0), echo, hub
}

// newOIDCModeAuth returns an oidc-mode authenticator for selfID that
// presents token and checks tokens with fakeIDTokens.
func newOIDCModeAuth(t *testing.T, selfID, token string, gotAudience *string, mod func(*ConduitPeerAuthOptions)) relay.PeerAuth {
	t.Helper()
	o := ConduitPeerAuthOptions{
		Mode: config.ConduitPeerAuthOIDC, SelfID: selfID, SharedSecret: testPeerSecret,
		OwnServiceAccount: "hub@p.iam.gserviceaccount.com",
		TokenSource:       staticTokenSource{tok: token}, Validate: fakeIDTokens(gotAudience),
	}
	if mod != nil {
		mod(&o)
	}
	a, mode, err := NewConduitPeerAuth(o)
	require.NoError(t, err)
	require.Equal(t, config.ConduitPeerAuthOIDC, mode)
	return a
}

var hubTunnelTransports = []hubTunnelTransport{
	{name: "control-channel", wrap: func(_ *testing.T, m *mockControlChannelTunnel) controlChannelTunnel { return m }},
	{name: "conduit", wrap: conduitTunnelOverMock},
}

var silentBrokerTransports = []silentBrokerTransport{
	{name: "control-channel", new: newSilentControlChannelBroker},
	{name: "conduit", new: newSilentConduitBroker},
}

func (e *echoBroker) HandleRPC(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
	env := envelope.RPCToRequest(req)
	e.mu.Lock()
	e.seen = append(e.seen, env)
	e.mu.Unlock()
	return envelope.ResponseToRPC(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK, env.Headers, env.Body))
}

func (e *echoBroker) requests() []*wsprotocol.RequestEnvelope {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*wsprotocol.RequestEnvelope(nil), e.seen...)
}

// fakeIDTokens validates the fixed test tokens below.
func fakeIDTokens(gotAudience *string) func(context.Context, string, string) (*idtoken.Payload, error) {
	const own = "hub@p.iam.gserviceaccount.com"
	claims := map[string]map[string]any{
		"own":        {"email": own, "email_verified": true},
		"own-upper":  {"email": "HUB@p.iam.gserviceaccount.com", "email_verified": true},
		"stranger":   {"email": "other@p.iam.gserviceaccount.com", "email_verified": true},
		"unverified": {"email": own, "email_verified": false},
		"no-email":   {"email_verified": true},
	}
	return func(_ context.Context, tok, aud string) (*idtoken.Payload, error) {
		if gotAudience != nil {
			*gotAudience = aud
		}
		c, ok := claims[tok]
		if !ok {
			return nil, errors.New("bad signature")
		}
		return &idtoken.Payload{Audience: aud, Claims: c}, nil
	}
}

// conduitTunnelOverMock returns a conduitBrokerTunnel whose broker session
// is answered by m: each RpcRequest reaches m as the request envelope the
// broker would see, and m's response travels back as the RpcResponse.
// m.connected decides whether the broker has a session; m.err makes the
// broker drop the session after receiving the request, so the response is
// lost after the request was sent.
func conduitTunnelOverMock(t *testing.T, m *mockControlChannelTunnel) controlChannelTunnel {
	var (
		mu       sync.Mutex
		brokerID string
		drop     func()
	)
	rpc := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		mu.Lock()
		id, dropSession := brokerID, drop
		mu.Unlock()
		resp, err := m.TunnelRequest(ctx, id, envelope.RPCToRequest(req))
		if err != nil {
			dropSession()
			<-ctx.Done() // the session has ended: no response is sent
			return nil
		}
		return envelope.ResponseToRPC(resp)
	})
	hub, d := newBrokerConduitPair(t, conduit.Config{RPCHandler: rpc})
	mu.Lock()
	drop = d
	mu.Unlock()
	return newConduitBrokerTunnel(func(id string) conduit.Session {
		if !m.connected {
			return nil
		}
		mu.Lock()
		brokerID = id
		mu.Unlock()
		return hub
	}, 0)
}

func newSilentConduitBroker(t *testing.T, timeout time.Duration) silentBroker {
	b := &silentConduitBroker{reqs: make(chan wsprotocol.RequestEnvelope, 4), cancels: make(chan string, 4)}
	rpc := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		b.reqs <- *envelope.RPCToRequest(req)
		<-ctx.Done() // never answers; the session cancels ctx on RpcCancel
		b.cancels <- req.GetRequestId()
		return nil
	})
	intercept := func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		switch f.GetBody().(type) {
		case *conduitv1.Frame_RpcRequest:
			if dir == conduit.Inbound {
				b.requestFrames.Add(1)
			}
		case *conduitv1.Frame_RpcCancel:
			if dir == conduit.Inbound {
				b.cancelFrames.Add(1)
			}
		case *conduitv1.Frame_RpcResponse:
			if dir == conduit.Outbound {
				b.responseFrames.Add(1)
			}
		}
		return []*conduitv1.Frame{f}
	}
	hub, _ := newBrokerConduitPair(t, conduit.Config{RPCHandler: rpc, Interceptor: intercept})
	b.t = newConduitBrokerTunnel(func(id string) conduit.Session {
		if id != "broker-1" {
			return nil
		}
		return hub
	}, timeout)
	return b
}

func newSilentControlChannelBroker(t *testing.T, timeout time.Duration) silentBroker {
	hubSide, brokerSide, cleanup := newHubWSPair(t)
	t.Cleanup(cleanup)
	mgr := NewControlChannelManager(ControlChannelConfig{RequestTimeout: timeout}, slog.Default())
	hc := &BrokerConnection{
		brokerID:        "broker-1",
		conn:            hubSide,
		config:          mgr.config,
		log:             slog.Default(),
		pendingRequests: make(map[string]chan *wsprotocol.ResponseEnvelope),
		ctx:             context.Background(),
	}
	mgr.connections["broker-1"] = hc
	b := &silentControlChannelBroker{mgr: mgr, hc: hc, brokerSide: brokerSide, readDone: make(chan wsprotocol.RequestEnvelope, 1)}
	// Read the request the broker side receives, simulating a broker
	// that is busy with a slow create and never responds.
	go func() {
		var got wsprotocol.RequestEnvelope
		if err := brokerSide.ReadJSON(&got); err == nil {
			b.readDone <- got
		}
	}()
	return b
}

// silentBroker is a broker that receives a tunnelled request and never
// answers it, for the cancel tests.
type silentBroker interface {
	// tunnel reaches the broker as "broker-1".
	tunnel() controlChannelTunnel
	// received returns the request the broker received.
	received(t *testing.T) wsprotocol.RequestEnvelope
	// cancelled returns the request id of the cancel the broker received.
	cancelled(t *testing.T) string
	// noFurther checks that the broker received exactly one request and
	// one cancel, and that the hub no longer tracks requestID.
	noFurther(t *testing.T, requestID string)
}

// newHubWSPair creates a connected pair of wsprotocol.Connection for testing
// BrokerConnection in isolation, mirroring the "hub" end (returned first)
// and the simulated "broker" end (returned second) of a real control
// channel, without needing a full ControlChannelManager/broker process.
func newHubWSPair(t *testing.T) (hubSide, brokerSide *wsprotocol.Connection, cleanup func()) {
	t.Helper()
	ready := make(chan *wsprotocol.Connection, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		cfg := wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}
		ready <- wsprotocol.NewConnection(ws, cfg)
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	rawConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cfg := wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}
	brokerSide = wsprotocol.NewConnection(rawConn, cfg)
	hubSide = <-ready

	return hubSide, brokerSide, func() {
		_ = hubSide.Close()
		_ = brokerSide.Close()
		srv.Close()
	}
}

// newBrokerConduitPair runs a broker's conduit dialer
// (runtimebroker.ConduitDialer, which presents the broker Hello) with
// brokerCfg as its session config (ConduitDialConfig.Session, which
// carries the RPC handler), against a hub endpoint that admits it over
// WebSocket, and returns the hub's end of the admitted session. drop stops
// the dialer, which closes its session; everything stops when the test
// ends.
func newBrokerConduitPair(t *testing.T, brokerCfg conduit.Config) (hub conduit.LocalSession, drop func()) {
	t.Helper()
	admitted := make(chan conduit.LocalSession, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := ws.Upgrade(w, r, nil, ws.Options{})
		if err != nil {
			return
		}
		s, err := conduit.Accept(context.WithoutCancel(r.Context()), conn, conduit.Config{}, conduitTestAdmitter{})
		if err != nil {
			return
		}
		ls := s.(conduit.LocalSession)
		select {
		case admitted <- ls: // the first session; a redial is not handed out
		default:
		}
		<-ls.Done()
	}))
	t.Cleanup(srv.Close)
	d, err := runtimebroker.NewConduitDialer(runtimebroker.ConduitDialConfig{
		HubEndpoint: srv.URL,
		BrokerID:    "broker-1",
		SecretKey:   []byte("0123456789abcdef0123456789abcdef"),
		Version:     "test",
		Session:     brokerCfg,
		Rand:        func(n int64) int64 { return n - 1 },
	})
	if err != nil {
		t.Fatalf("conduit dialer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case hub = <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("the broker's conduit session was never admitted")
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub, cancel
}

type silentControlChannelBroker struct {
	mgr        *ControlChannelManager
	hc         *BrokerConnection
	brokerSide *wsprotocol.Connection
	readDone   chan wsprotocol.RequestEnvelope
}

type silentConduitBroker struct {
	t       *conduitBrokerTunnel
	reqs    chan wsprotocol.RequestEnvelope
	cancels chan string
	// requestFrames and cancelFrames count the RpcRequest and RpcCancel
	// frames the broker's session received, responseFrames the
	// RpcResponse frames it sent.
	requestFrames, cancelFrames, responseFrames atomic.Int32
}

type staticTokenSource struct {
	tok string
	err error
}

const testPeerSecret = "shared-signing-secret-0123456789ab"

func (b *silentControlChannelBroker) tunnel() controlChannelTunnel { return b.mgr }

func (b *silentControlChannelBroker) received(t *testing.T) wsprotocol.RequestEnvelope {
	t.Helper()
	select {
	case req := <-b.readDone:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("broker side never received the request")
	}
	return wsprotocol.RequestEnvelope{}
}

func (b *silentControlChannelBroker) cancelled(t *testing.T) string {
	t.Helper()
	var cancelMsg wsprotocol.CancelMessage
	envDone := make(chan error, 1)
	go func() { envDone <- b.brokerSide.ReadJSON(&cancelMsg) }()
	select {
	case err := <-envDone:
		if err != nil {
			t.Fatalf("broker side failed to read cancel message: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("broker side never received a cancel message")
	}
	if cancelMsg.Type != wsprotocol.TypeCancel {
		t.Errorf("expected cancel message type %q, got %q", wsprotocol.TypeCancel, cancelMsg.Type)
	}
	return cancelMsg.RequestID
}

func (b *silentControlChannelBroker) noFurther(t *testing.T, requestID string) {
	t.Helper()
	b.hc.pendingMu.Lock()
	_, stillPending := b.hc.pendingRequests[requestID]
	b.hc.pendingMu.Unlock()
	if stillPending {
		t.Error("expected the pending-request entry to be removed once the request returned")
	}
	_ = b.brokerSide.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var extra wsprotocol.RequestEnvelope
	if err := b.brokerSide.ReadJSON(&extra); err == nil {
		t.Fatalf("unexpected second message on the wire after the cancel: %+v", extra)
	}
}

func (b *silentConduitBroker) tunnel() controlChannelTunnel { return b.t }

func (b *silentConduitBroker) received(t *testing.T) wsprotocol.RequestEnvelope {
	t.Helper()
	select {
	case req := <-b.reqs:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("broker side never received the request")
	}
	return wsprotocol.RequestEnvelope{}
}

// cancelled waits for the broker's handler to see its context cancelled,
// which the session does on RpcCancel.
func (b *silentConduitBroker) cancelled(t *testing.T) string {
	t.Helper()
	select {
	case id := <-b.cancels:
		if n := b.cancelFrames.Load(); n != 1 {
			t.Errorf("broker received %d RpcCancel frames, want 1", n)
		}
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("broker handler never saw the request cancelled")
	}
	return ""
}

// noFurther waits the same window as the control channel leg, then checks
// the frame counts: one request, one cancel, and no response.
func (b *silentConduitBroker) noFurther(t *testing.T, _ string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if n := b.responseFrames.Load(); n != 0 {
		t.Errorf("broker sent %d RpcResponse frames, want 0", n)
	}
	if n := b.requestFrames.Load(); n != 1 {
		t.Errorf("broker received %d RpcRequest frames, want 1", n)
	}
	if n := b.cancelFrames.Load(); n != 1 {
		t.Errorf("broker received %d RpcCancel frames, want 1", n)
	}
}

func (s staticTokenSource) Token() (string, error)   { return s.tok, s.err }
func (staticTokenSource) SetToken(string, time.Time) {}
func (staticTokenSource) Expiry() time.Time          { return time.Time{} }

// conduitTestAdmitter admits every Hello with a fixed Welcome.
type conduitTestAdmitter struct{}

func (conduitTestAdmitter) Admit(context.Context, *conduitv1.Hello) (*conduitv1.Welcome, error) {
	return &conduitv1.Welcome{SessionId: "broker-sess-1", RelayInstanceId: "hub-a", ConnectionEpoch: 1, EndpointIncarnation: "inc-1"}, nil
}

func (conduitTestAdmitter) Refresh(context.Context, *conduitv1.AuthRefresh) error { return nil }
