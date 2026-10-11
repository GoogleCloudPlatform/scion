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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptyConduitFixture is a hub (relay hub-a, hub.conduit on) whose launched
// agent runs a sciontool conduit agent with a fake PTY spawner, plus a
// user token for the agent's owner.
type ptyConduitFixture struct {
	*relayFixture
	userToken  string
	peerSecret []byte
	spawned    chan *fakePTY
	// regFault, while set, fails every conduit registry operation on this
	// hub node (the relay's and the router's).
	regFault atomic.Bool
}

// newPTYConduitFixture builds the fixture; mods adjust the relay options
// after the fixture's own (e.g. a lifetime cap).
func newPTYConduitFixture(t *testing.T, mods ...func(*ConduitRelayOptions)) *ptyConduitFixture {
	t.Helper()
	f := &ptyConduitFixture{peerSecret: make([]byte, 32), spawned: make(chan *fakePTY, 8)}
	_, _ = rand.Read(f.peerSecret)
	f.relayFixture = newRelayFixture(t, func(o *ConduitRelayOptions) {
		auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: f.peerSecret, SelfID: "hub-a"})
		require.NoError(t, err)
		o.PeerAuth = auth
		faulty := &registry.FaultStore{Inner: o.Store, Fault: func(context.Context, string) error {
			if f.regFault.Load() {
				return errors.New("injected registry fault")
			}
			return nil
		}}
		o.Store = faulty
		o.Registry = registry.New(faulty, registry.Config{})
		for _, m := range mods {
			m(o)
		}
	})
	require.True(t, f.srv.conduitServing())
	// Broker ids are UUIDs in the store: register the agent's broker under
	// one (attach supported until a test says otherwise) and point the
	// launched agent at it.
	ctx := context.Background()
	brokerID := uuid.NewString()
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "pty-broker", Slug: "pty-broker", Status: store.BrokerStatusOnline,
		Capabilities: attachCaps(true),
	}))
	f.launched.RuntimeBrokerID = brokerID
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, brokerID, got.RuntimeBrokerID)
	f.launched = got
	f.userToken, _, _, err = f.srv.userTokenService.GenerateTokenPair(
		f.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return f
}

func attachCaps(attach bool) *store.BrokerCapabilities {
	return &store.BrokerCapabilities{Attach: attach, Sync: true}
}

func waitSpawn(t *testing.T, ch <-chan *fakePTY) *fakePTY {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("no PTY spawned on the agent")
		return nil
	}
}

func waitClosedPTY(t *testing.T, p *fakePTY) {
	t.Helper()
	select {
	case <-p.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's PTY process was not closed")
	}
}

// echoRoundTrip sends s as client input and reads output until s comes
// back.
func echoRoundTrip(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	require.NoError(t, c.WriteJSON(wsprotocol.NewPTYDataMessage([]byte(s))))
	var got strings.Builder
	for !strings.Contains(got.String(), s) {
		var msg wsprotocol.PTYDataMessage
		require.NoError(t, c.ReadJSON(&msg))
		require.Equal(t, wsprotocol.TypeData, msg.Type)
		got.Write(msg.Data)
	}
}

// readUntilClose reads until the server's close frame and returns it.
func readUntilClose(t *testing.T, c *websocket.Conn) *websocket.CloseError {
	t.Helper()
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		require.ErrorAs(t, err, &ce)
		return ce
	}
}

// startControlAgent runs a sciontool conduit agent for the launched agent
// against hubURL whose session RPC handler is the real control handler,
// as sciontool wires it. With advertise the session lists the handler's
// routes in Hello.capabilities.rpc; without it the handler is still
// installed but nothing is advertised. wrap, when set, sits between the
// counter and the control handler (to answer differently or block). It
// returns once the session is admitted.
func (f *ptyConduitFixture) startControlAgent(t *testing.T, hubURL string, advertise bool, wrap func(conduit.RPCHandler) conduit.RPCHandler) *controlAgent {
	t.Helper()
	guardSciontoolLog()
	ca := &controlAgent{paths: make(chan string, 16)}
	ctl := control.New(control.Options{KickTokenRefresh: func() { ca.kicks.Add(1) }})
	h := control.RPCHandler(ctl)
	if wrap != nil {
		h = wrap(h)
	}
	counted := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		ca.rpcs.Add(1)
		select {
		case ca.paths <- req.GetPath():
		default:
		}
		return h.HandleRPC(ctx, req)
	})
	var routes []string
	if advertise {
		routes = ctl.Routes()
	}
	tok := f.agentToken(t, f.launched)
	admitted := make(chan *conduitv1.Welcome, 4)
	a, err := sconduit.New(sconduit.Options{
		HubURL:    hubURL,
		AgentID:   f.launched.ID,
		ProjectID: f.launched.ProjectID,
		LaunchID:  f.launched.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(f.clock.Now()), now: f.clock.Now},
		NoPTY:     true,
		Session:   conduit.Config{RPCHandler: counted},
		RPCRoutes: routes,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	})
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
	return ca
}

// setBrokerRow replaces the launched agent's broker row.
func (f *ptyConduitFixture) setBrokerRow(t *testing.T, caps *store.BrokerCapabilities, defaultProfile string, profiles ...store.BrokerProfile) {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: f.launched.RuntimeBrokerID, Name: "pty-broker", Slug: "pty-broker", Status: store.BrokerStatusOnline,
		Capabilities: caps, Profiles: profiles, DefaultProfile: defaultProfile,
	}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

// startTCPOnlySession runs a sciontool conduit agent for the launched
// agent with NoPTY set, so its session advertises only tcp, and returns
// once the session is admitted. A tmux stand-in is put first on PATH, so
// the session is tcp-only because the agent honours NoPTY, not because
// the host lacks tmux.
func (f *ptyConduitFixture) startTCPOnlySession(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.startSciontoolAgent(t, f.public.URL, true)
	recs := f.agentSessions(t)
	require.Len(t, recs, 1, "the tcp-only session is registered")
	assert.Equal(t, []string{grant.StreamKindTCP}, recs[0].Capabilities.StreamKinds,
		"the session advertises tcp only")
}

// startPTYAgent runs a sciontool conduit agent for the launched agent
// against hubURL, serving pty with a fake spawner, and returns once its
// session is admitted.
func (f *ptyConduitFixture) startPTYAgent(t *testing.T, hubURL string) {
	t.Helper()
	f.startSciontoolAgent(t, hubURL, false)
}

// startSciontoolAgent runs a sciontool conduit agent for the launched
// agent against hubURL and returns once its session is admitted. With
// noPTY it serves no pty streams (Options.NoPTY); otherwise it serves
// pty with a fake spawner that reports each process on f.spawned.
func (f *ptyConduitFixture) startSciontoolAgent(t *testing.T, hubURL string, noPTY bool) {
	t.Helper()
	guardSciontoolLog()
	tok := f.agentToken(t, f.launched)
	admitted := make(chan *conduitv1.Welcome, 4)
	opts := sconduit.Options{
		HubURL:    hubURL,
		AgentID:   f.launched.ID,
		ProjectID: f.launched.ProjectID,
		LaunchID:  f.launched.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(f.clock.Now()), now: f.clock.Now},
		NoPTY:     noPTY,
	}
	if !noPTY {
		opts.SpawnPTY = func(_ context.Context, req sconduit.PTYRequest) (sconduit.PTYProcess, error) {
			p := newFakePTY(req)
			f.spawned <- p
			return p, nil
		}
	}
	a, err := sconduit.New(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	})
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
}

// startPeerRelay starts a second relay (another hub node) on the shared
// registry, signing peers with the fixture's secret and publishing the
// hub's grant keys, and returns a base URL whose /api/v1/conduit lands on
// it (as the launched agent) while every other path is the hub.
func (f *ptyConduitFixture) startPeerRelay(t *testing.T, id string) string {
	t.Helper()
	var internalH atomic.Pointer[http.Handler]
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := internalH.Load()
		if h == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(w, r)
	}))
	t.Cleanup(internal.Close)
	auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: f.peerSecret, SelfID: id})
	require.NoError(t, err)
	r, err := relay.New(relay.Config{
		InstanceID:       id,
		InternalEndpoint: internal.URL,
		Registry:         f.reg,
		Store:            f.regStore,
		Session:          conduit.Config{Clock: clock.Real()},
		GrantKeys:        f.srv.conduitWelcomeGrantKeys,
		PeerAuth:         auth,
		HTTPClient:       internal.Client(),
		Clock:            clock.NewFake(time.Now()),
	})
	require.NoError(t, err)
	h := r.InternalHandler()
	internalH.Store(&h)
	require.NoError(t, r.Start(context.Background()))
	t.Cleanup(r.Kill)

	hub := f.srv.Handler()
	principal := relay.Principal{
		Kind: registry.PrincipalAgent, ID: f.launched.ID, ProjectID: f.launched.ProjectID,
		Agent: agentIncarnationFacts(f.launched),
	}
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/conduit" {
			hub.ServeHTTP(w, req)
			return
		}
		conn, err := ws.Upgrade(w, req, nil, ws.Options{})
		if err != nil {
			return
		}
		_ = r.Serve(context.Background(), conn, principal)
	}))
	t.Cleanup(public.Close)
	return public.URL
}

func (f *ptyConduitFixture) ptyURL() string {
	return f.public.URL + "/api/v1/agents/" + f.launched.ID + "/pty"
}

// preflight sends the non-upgrade GET /pty and returns the status and the
// path it names (200: body.path; refusal: error.details.reason).
func (f *ptyConduitFixture) preflight(t *testing.T) (status int, path, reason string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ptyURL(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode == http.StatusOK {
		var ok ptyPreflightResponse
		require.NoError(t, json.Unmarshal(body, &ok), "preflight body: %s", body)
		return resp.StatusCode, ok.Path, ""
	}
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), "preflight body: %s", body)
	if r, ok := er.Error.Details["reason"].(string); ok {
		reason = r
	}
	return resp.StatusCode, string(ptyPathNone), reason
}

// dialPTY opens the PTY WebSocket with query (e.g. "?cols=100&rows=30").
func (f *ptyConduitFixture) dialPTY(t *testing.T, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(f.ptyURL(), "http") + query
	c, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + f.userToken}})
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	}
	return c, resp, err
}

// setAgentBroker points the launched agent at brokerID (which may have no
// row, or not be a valid broker id at all).
func (f *ptyConduitFixture) setAgentBroker(t *testing.T, brokerID string) {
	t.Helper()
	ctx := context.Background()
	f.launched.RuntimeBrokerID = brokerID
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, brokerID, got.RuntimeBrokerID)
	f.launched = got
}

// setAgentRuntime sets the launched agent's runtime.
func (f *ptyConduitFixture) setAgentRuntime(t *testing.T, runtime string) {
	t.Helper()
	ctx := context.Background()
	f.launched.Runtime = runtime
	require.NoError(t, f.store.UpdateAgent(ctx, f.launched))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	require.Equal(t, runtime, got.Runtime)
	f.launched = got
}

func newFakePTY(req sconduit.PTYRequest) *fakePTY {
	pr, pw := io.Pipe()
	return &fakePTY{req: req, pr: pr, pw: pw, resizes: make(chan [2]uint16, 8), closed: make(chan struct{})}
}

// fakePTY is a PTY process for the sciontool agent: it echoes input to
// output and records resizes.
type fakePTY struct {
	req     sconduit.PTYRequest
	pr      *io.PipeReader
	pw      *io.PipeWriter
	resizes chan [2]uint16
	closed  chan struct{}
	once    sync.Once
}

func (p *fakePTY) Read(b []byte) (int, error)  { return p.pr.Read(b) }
func (p *fakePTY) Write(b []byte) (int, error) { return p.pw.Write(b) }
func (p *fakePTY) Resize(cols, rows uint16) error {
	select {
	case p.resizes <- [2]uint16{cols, rows}:
	default:
	}
	return nil
}
func (p *fakePTY) Close() error {
	p.once.Do(func() {
		close(p.closed)
		_ = p.pw.Close()
		_ = p.pr.Close()
	})
	return nil
}
