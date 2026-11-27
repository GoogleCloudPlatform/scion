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

//go:build integration

package hub

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// conduitHAOptions tunes newConduitHAEnvWith for tests that need custom
// intervals (such as a short AuthzRecheckInterval for C16b).
type conduitHAOptions struct {
	AuthzRecheckInterval time.Duration
}

// conduitHANode is one hub replica in a 2-node Postgres Conduit HA test
// environment.
type conduitHANode struct {
	InstanceID string
	AppName    string
	Client     *ent.Client
	Store      *entadapter.CompositeStore
	Server     *Server
	Ops        *OperationalSettings
	Events     *PostgresEventPublisher
	CommandBus *PostgresCommandBus
	RegStore   registry.Store
	Public     *httptest.Server
	Internal   *httptest.Server

	internalRPCHops    atomic.Int64
	internalStreamHops atomic.Int64
	internalSelfProbes atomic.Int64

	stopOnce sync.Once
	stopped  atomic.Bool
}

// InternalRPCHops returns the number of internal relay RPC hop requests
// (/internal/v1/conduit/sessions/{id}/rpc) handled by this node's internal
// peer listener.
func (n *conduitHANode) InternalRPCHops() int64 {
	return n.internalRPCHops.Load()
}

// InternalStreamHops returns the number of internal relay stream hop requests
// (/internal/v1/conduit/sessions/{id}/stream) handled by this node's internal
// peer listener.
func (n *conduitHANode) InternalStreamHops() int64 {
	return n.internalStreamHops.Load()
}

// Relay returns this node's running in-process relay, or nil if stopped.
func (n *conduitHANode) Relay() *relay.Relay {
	if rt := n.Server.conduit.Load(); rt != nil {
		return rt.relay
	}
	return nil
}

// Router returns this node's conduit router, or nil if stopped.
func (n *conduitHANode) Router() *router.Router {
	return n.Server.conduitRouter()
}

// stop shuts down this hub node cleanly (public HTTP server, conduit relay,
// internal peer listener, operational-settings propagation, command bus, and
// Postgres event publisher). It is safe to call multiple times.
func (n *conduitHANode) stop() {
	n.stopOnce.Do(func() {
		n.stopped.Store(true)
		if n.Public != nil {
			n.Public.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		n.Server.shutdownConduitRelay(ctx)
		if n.Internal != nil {
			n.Internal.Close()
		}
		if n.Ops != nil {
			n.Ops.StopPropagation()
		}
		if n.CommandBus != nil {
			n.CommandBus.Close()
		}
		if n.Events != nil {
			n.Events.Close()
		}
	})
}

// conduitHAEnv holds two complete hub.Server instances (HubA and HubB) sharing
// one isolated Postgres schema, each with:
//   - hub.conduit and hub.conduit_deliver enabled via Postgres hub_settings,
//   - a shared at-rest encryption key (ConduitGrantRingShared() == true),
//   - an in-process conduit relay started with RequireHA: true and a distinct
//     instance_id ("hub-a", "hub-b"),
//   - a loopback internal peer listener authenticated with a shared HMAC key
//     and instrumented to count cross-node relay hops,
//   - a real PostgresEventPublisher with OperationalSettings propagation, and
//   - a real PostgresCommandBus (LISTEN/NOTIFY on scion_broker_cmd).
//
// Seeded fixtures: Project, Owner, Broker (with HMAC secret persisted in
// Postgres), and Agent (assigned to Broker, with a launch RunID).
//
// Fault hooks for HA scenario tests:
//   - CloseSession(node, sessionID): closes a session from the owning relay.
//   - agent.CloseSession() / broker.CloseSession(): abruptly closes the
//     client's underlying WebSocket TCP connection.
//   - agent.SilentDrop() / agent.Restore() and broker.SilentDrop() /
//     broker.Restore(): blackholes reads and writes on the client's WebSocket
//     TCP connection without sending a close frame.
//   - StopHub(node): shuts down a hub node cleanly (listeners, relay, buses).
//   - BlockLISTEN(t, node): terminates the node's Postgres LISTEN backend and
//     holds reconnect down until the returned unblock function is called.
type conduitHAEnv struct {
	DSN          string
	HubA         *conduitHANode
	HubB         *conduitHANode
	Project      *store.Project
	Owner        *AuthenticatedUser
	BrokerID     string
	BrokerSecret []byte
	Agent        *store.Agent

	peerSecret   []byte
	sharedSecret string
}

// newConduitHAEnv builds a 2-node Postgres Conduit HA test environment with
// default options. Without SCION_TEST_POSTGRES_URL it skips cleanly via
// requirePG.
func newConduitHAEnv(t *testing.T) *conduitHAEnv {
	t.Helper()
	return newConduitHAEnvWith(t, conduitHAOptions{})
}

// newConduitHAEnvWith builds a 2-node Postgres Conduit HA test environment
// with custom options.
func newConduitHAEnvWith(t *testing.T, opts conduitHAOptions) *conduitHAEnv {
	t.Helper()
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	peerSecret := make([]byte, 32)
	_, err := rand.Read(peerSecret)
	require.NoError(t, err)

	sharedSecretRaw := make([]byte, 32)
	_, err = rand.Read(sharedSecretRaw)
	require.NoError(t, err)
	sharedSecret := hex.EncodeToString(sharedSecretRaw)

	suffix := uuid.NewString()[:8]
	env := &conduitHAEnv{
		DSN:          dsn,
		peerSecret:   peerSecret,
		sharedSecret: sharedSecret,
	}

	env.HubA = env.startNode(t, ctx, "hub-a", "conduit-ha-a-"+suffix, true, opts)
	env.HubB = env.startNode(t, ctx, "hub-b", "conduit-ha-b-"+suffix, false, opts)

	env.seedFixtures(t, ctx)
	return env
}

func (e *conduitHAEnv) startNode(t *testing.T, ctx context.Context, instanceID, appName string, seedExperiments bool, opts conduitHAOptions) *conduitHANode {
	t.Helper()

	nodeDSN := conduitHADSNParam(t, e.DSN, "application_name", appName)
	client, err := entc.OpenPostgres(nodeDSN, entc.PoolConfig{MaxOpenConns: 8, MaxIdleConns: 2})
	require.NoError(t, err)

	cs := entadapter.NewCompositeStore(client)
	require.NoError(t, cs.Migrate(ctx))

	cfg := testServerConfig()
	cfg.HubID = "conduit-ha-hub"
	cfg.SharedSigningSecret = e.sharedSecret
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()

	srv, _ := testServerWithStoreConfig(t, cs, cfg)
	srv.SetIntegrationHA("postgres", client, nodeDSN)
	require.True(t, srv.ConduitGrantRingShared(), "shared at-rest key must enable persisted grant ring on %s", instanceID)

	node := &conduitHANode{
		InstanceID: instanceID,
		AppName:    appName,
		Client:     client,
		Store:      cs,
		Server:     srv,
	}

	pub, err := NewPostgresEventPublisher(ctx, nodeDSN, nil, slog.Default())
	require.NoError(t, err)
	node.Events = pub
	srv.SetEventPublisher(pub)

	if seedExperiments {
		raw, err := json.Marshal(map[string]any{
			"overrides": map[string]bool{
				conduitExperiment:          true,
				experiments.ConduitDeliver: true,
			},
		})
		require.NoError(t, err)
		_, err = cs.UpsertHubSetting(ctx, "experiments", raw, "conduit-ha-test", -1, "seeded")
		require.NoError(t, err)
	}

	ops := NewOperationalSettings(cs, koanf.New("."), koanf.New("."))
	ops.PollInterval = 200 * time.Millisecond
	ops.SetEventPublisher(pub)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	ops.StartPropagation(ctx, srv)
	node.Ops = ops

	ownsLocally := func(brokerID string) bool {
		return srv.controlChannel != nil && srv.controlChannel.IsConnected(brokerID)
	}
	cmdBus, err := NewPostgresCommandBus(ctx, nodeDSN, ownsLocally, srv.ReconcileBroker, slog.Default())
	require.NoError(t, err)
	node.CommandBus = cmdBus
	srv.SetCommandBus(cmdBus)

	// Start the loopback internal peer listener before StartConduitRelay so
	// RequireHA's registry.SelfCheck probe succeeds against it.
	internalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == relay.InternalPathPrefix+"self":
			node.internalSelfProbes.Add(1)
		case strings.HasPrefix(p, relay.InternalPathPrefix+"sessions/") && strings.HasSuffix(p, "/rpc"):
			node.internalRPCHops.Add(1)
		case strings.HasPrefix(p, relay.InternalPathPrefix+"sessions/") && strings.HasSuffix(p, "/stream"):
			node.internalStreamHops.Add(1)
		}
		srv.ConduitInternalHandler().ServeHTTP(w, r)
	})
	node.Internal = httptest.NewServer(internalHandler)

	peerAuth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{
		Secret: e.peerSecret,
		SelfID: instanceID,
	})
	require.NoError(t, err)

	regStore := entadapter.NewConduitRegistryStore(client)
	node.RegStore = regStore
	err = srv.StartConduitRelay(ctx, ConduitRelayOptions{
		InstanceID:           instanceID,
		InternalEndpoint:     node.Internal.URL,
		RequireHA:            true,
		PeerAuth:             peerAuth,
		Store:                regStore,
		Registry:             registry.New(regStore, registry.Config{}),
		AuthzRecheckInterval: opts.AuthzRecheckInterval,
	})
	require.NoError(t, err, "StartConduitRelay(%s) with RequireHA=true", instanceID)
	require.True(t, srv.conduitServing(), "%s must serve conduit", instanceID)
	require.True(t, srv.conduitDeliverEnabled(), "%s must serve conduit_deliver", instanceID)

	node.Public = httptest.NewServer(srv.Handler())

	t.Cleanup(node.stop)
	return node
}

func (e *conduitHAEnv) seedFixtures(t *testing.T, ctx context.Context) {
	t.Helper()
	s := e.HubA.Store

	project := &store.Project{
		ID:   tid("conduit-ha-project"),
		Name: "Conduit HA",
		Slug: "conduit-ha",
	}
	require.NoError(t, s.CreateProject(ctx, project))
	e.Project = project

	ownerID := tid("conduit-ha-owner")
	createTestUserWithProjectRole(t, s, ownerID, "owner@conduit-ha.test", project.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, ownerID)
	e.Owner = NewAuthenticatedUser(ownerID, "owner@conduit-ha.test", "Owner", store.UserRoleMember, "api")

	brokerID := tid("conduit-ha-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "conduit-ha-broker",
		Slug:    "conduit-ha-broker",
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}))
	keyB64, err := e.HubA.Server.brokerAuthService.GenerateAndStoreSecret(ctx, brokerID)
	require.NoError(t, err)
	secretBytes, err := base64.StdEncoding.DecodeString(keyB64)
	require.NoError(t, err)
	e.BrokerID = brokerID
	e.BrokerSecret = secretBytes

	agent := &store.Agent{
		ID:              tid("conduit-ha-agent"),
		Slug:            "conduit-ha-agent",
		Name:            "Conduit HA Agent",
		ProjectID:       project.ID,
		OwnerID:         ownerID,
		Ancestry:        []string{ownerID},
		RuntimeBrokerID: brokerID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err = s.SetAgentRunID(ctx, agent.ID, uuid.NewString(), nil)
	require.NoError(t, err)
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotEmpty(t, got.RunID)
	e.Agent = got
}

// CreateMessage persists a message row addressed to the seeded agent in state.
func (e *conduitHAEnv) CreateMessage(t *testing.T, dispatchState, body string) string {
	t.Helper()
	m := &store.Message{
		ID:            uuid.NewString(),
		ProjectID:     e.Agent.ProjectID,
		Sender:        "user:owner",
		SenderID:      e.Owner.ID(),
		Recipient:     "agent:" + e.Agent.Slug,
		RecipientID:   e.Agent.ID,
		Msg:           body,
		Type:          "instruction",
		AgentID:       e.Agent.ID,
		DispatchState: dispatchState,
	}
	require.NoError(t, e.HubA.Store.CreateMessage(context.Background(), m))
	return m.ID
}

// CloseSession closes sessionID from node's in-process relay.
func (e *conduitHAEnv) CloseSession(t *testing.T, node *conduitHANode, sessionID string) {
	t.Helper()
	r := node.Relay()
	require.NotNil(t, r, "node %s has no running relay", node.InstanceID)
	ls, _, ok := r.Local(context.Background(), sessionID)
	require.True(t, ok, "session %s not local on %s", sessionID, node.InstanceID)
	require.NoError(t, ls.Close())
}

// StopHub shuts down node cleanly (listeners, relay, ops propagation, command
// bus, and event publisher).
func (e *conduitHAEnv) StopHub(node *conduitHANode) {
	node.stop()
}

// BlockLISTEN terminates node's Postgres LISTEN backend(s) and holds its
// PostgresEventPublisher listener reconnect down until the returned unblock
// function is called.
func (e *conduitHAEnv) BlockLISTEN(t *testing.T, node *conduitHANode) (unblock func()) {
	t.Helper()
	peer := e.HubA
	if node == e.HubA {
		peer = e.HubB
	}
	release := make(chan struct{})
	held := make(chan struct{}, 1)
	var holdOnce sync.Once
	node.Events.mu.Lock()
	node.Events.testHookBeforeConnect = func() {
		holdOnce.Do(func() {
			held <- struct{}{}
			<-release
		})
	}
	node.Events.mu.Unlock()

	var releaseOnce sync.Once
	unblock = func() {
		releaseOnce.Do(func() {
			node.Events.mu.Lock()
			node.Events.testHookBeforeConnect = nil
			node.Events.mu.Unlock()
			close(release)
		})
	}
	t.Cleanup(unblock)

	_, err := peer.Events.pool.Exec(context.Background(),
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE application_name = $1 AND query ILIKE 'LISTEN %'`, node.AppName)
	require.NoError(t, err)

	select {
	case <-held:
	case <-time.After(15 * time.Second):
		t.Fatalf("node %s LISTEN connection did not drop within 15s", node.InstanceID)
	}
	return unblock
}

// --- Fault-injectable WebSocket dialer ---

// faultConn wraps a net.Conn so a test can abruptly close it or silently drop
// all reads and writes (blackhole without sending a TCP FIN/RST or WS close).
type faultConn struct {
	net.Conn
	mu           sync.Mutex
	dropped      bool
	unblock      chan struct{}
	closed       chan struct{}
	closeOnce    sync.Once
	readDeadline time.Time
}

func newFaultConn(c net.Conn) *faultConn {
	return &faultConn{
		Conn:    c,
		unblock: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (c *faultConn) setDropped(drop bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == drop {
		return
	}
	c.dropped = drop
	if !drop {
		close(c.unblock)
		c.unblock = make(chan struct{})
	}
}

func (c *faultConn) waitIfDropped() error {
	for {
		c.mu.Lock()
		dropped := c.dropped
		unblock := c.unblock
		deadline := c.readDeadline
		c.mu.Unlock()
		if !dropped {
			return nil
		}
		var timer <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return osTimeoutErr{}
			}
			t := time.NewTimer(d)
			timer = t.C
			defer t.Stop()
		}
		select {
		case <-c.closed:
			return net.ErrClosed
		case <-unblock:
		case <-timer:
			return osTimeoutErr{}
		}
	}
}

func (c *faultConn) Read(b []byte) (int, error) {
	if err := c.waitIfDropped(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(b)
	c.mu.Lock()
	dropped := c.dropped
	c.mu.Unlock()
	if dropped {
		if werr := c.waitIfDropped(); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

func (c *faultConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	dropped := c.dropped
	c.mu.Unlock()
	if dropped {
		// Blackhole outbound bytes without failing the write.
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *faultConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *faultConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *faultConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type osTimeoutErr struct{}

func (osTimeoutErr) Error() string   { return "i/o timeout" }
func (osTimeoutErr) Timeout() bool   { return true }
func (osTimeoutErr) Temporary() bool { return true }

// faultWSDialer tracks the latest dialed faultConn so tests can close or
// silently drop the active WebSocket transport.
type faultWSDialer struct {
	mu      sync.Mutex
	current *faultConn
	dialer  *websocket.Dialer
}

func newFaultWSDialer() *faultWSDialer {
	f := &faultWSDialer{}
	base := &net.Dialer{Timeout: 10 * time.Second}
	f.dialer = &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := base.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			fc := newFaultConn(raw)
			f.mu.Lock()
			f.current = fc
			f.mu.Unlock()
			return fc, nil
		},
	}
	return f
}

func (f *faultWSDialer) CloseCurrent() {
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (f *faultWSDialer) SilentDrop() {
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if c != nil {
		c.setDropped(true)
	}
}

func (f *faultWSDialer) Restore() {
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if c != nil {
		c.setDropped(false)
	}
}

// --- Fake Agent (real pkg/sciontool/conduit + real control.Handler + fake tmuxdeliver.Runner) ---

// recordingTmuxRunner implements tmuxdeliver.Runner and records every argv
// and stdin payload it executes.
type recordingTmuxRunner struct {
	mu      sync.Mutex
	calls   [][]string
	stdins  []string
	flushes int
}

func (r *recordingTmuxRunner) Run(_ context.Context, argv []string, stdin io.Reader) (string, error) {
	cp := append([]string(nil), argv...)
	var stdinStr string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		stdinStr = string(b)
	}
	r.mu.Lock()
	r.calls = append(r.calls, cp)
	r.stdins = append(r.stdins, stdinStr)
	if len(cp) >= 2 && cp[0] == "tmux" && cp[1] == "paste-buffer" {
		r.flushes++
	}
	r.mu.Unlock()
	return "", nil
}

func (r *recordingTmuxRunner) Calls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func (r *recordingTmuxRunner) Flushes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

// conduitHAAgent is a fake agent wired with the real pkg/sciontool/conduit
// session client, the real control.Handler (with deliver-message enabled), and
// a fake tmuxdeliver.Runner recording argv.
type conduitHAAgent struct {
	Runner   *recordingTmuxRunner
	ws       *faultWSDialer
	admitted chan *conduitv1.Welcome

	mu        sync.Mutex
	lastWel   *conduitv1.Welcome
	rpcPaths  []string
	rpcCount  atomic.Int64
	kickCount atomic.Int64

	stopOnce sync.Once
	stopFn   func()
}

// StartAgent starts a fake agent dialing node's public conduit endpoint and
// waits until its first session is admitted.
func (e *conduitHAEnv) StartAgent(t *testing.T, node *conduitHANode) *conduitHAAgent {
	t.Helper()
	guardSciontoolLog()

	tok, err := node.Server.GenerateAgentToken(e.Agent.ID, e.Agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	runner := &recordingTmuxRunner{}
	wsDialer := newFaultWSDialer()
	fa := &conduitHAAgent{
		Runner:   runner,
		ws:       wsDialer,
		admitted: make(chan *conduitv1.Welcome, 8),
	}

	ctl := control.New(control.Options{
		KickTokenRefresh: func() { fa.kickCount.Add(1) },
		Deliver: &control.DeliverOptions{
			Runner:            runner,
			ReadyPollInterval: 10 * time.Millisecond,
		},
	})
	baseRPC := control.RPCHandler(ctl)
	countedRPC := conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
		fa.rpcCount.Add(1)
		fa.mu.Lock()
		fa.rpcPaths = append(fa.rpcPaths, req.GetPath())
		fa.mu.Unlock()
		return baseRPC.HandleRPC(ctx, req)
	})

	a, err := sconduit.New(sconduit.Options{
		HubURL:    node.Public.URL,
		AgentID:   e.Agent.ID,
		ProjectID: e.Agent.ProjectID,
		LaunchID:  e.Agent.RunID,
		Token:     func() string { return tok },
		WS:        wsDialer.dialer,
		OnSession: func(w *conduitv1.Welcome) {
			fa.mu.Lock()
			fa.lastWel = w
			fa.mu.Unlock()
			select {
			case fa.admitted <- w:
			default:
			}
		},
		Backoff:   &conduit.Backoff{Base: 50 * time.Millisecond, Max: 500 * time.Millisecond, Rand: func(int64) int64 { return 0 }},
		NoPTY:     true,
		Session:   conduit.Config{RPCHandler: countedRPC},
		RPCRoutes: ctl.Routes(),
		Features:  ctl.Features(),
	})
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = a.Run(runCtx)
	}()

	fa.stopFn = func() {
		fa.stopOnce.Do(func() {
			wsDialer.Restore()
			cancel()
			wsDialer.CloseCurrent()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Error("conduit HA fake agent did not stop")
			}
		})
	}
	t.Cleanup(fa.stopFn)

	fa.WaitAdmitted(t, 10*time.Second)
	return fa
}

// WaitAdmitted waits for the next Welcome from the relay.
func (a *conduitHAAgent) WaitAdmitted(t *testing.T, timeout time.Duration) *conduitv1.Welcome {
	t.Helper()
	select {
	case w := <-a.admitted:
		return w
	case <-time.After(timeout):
		t.Fatalf("fake agent session not admitted within %s", timeout)
		return nil
	}
}

// SessionID returns the session ID from the latest Welcome.
func (a *conduitHAAgent) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastWel.GetSessionId()
}

// RelayInstanceID returns the relay instance ID from the latest Welcome.
func (a *conduitHAAgent) RelayInstanceID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastWel.GetRelayInstanceId()
}

// RPCCount returns the total number of control RPCs received by the agent.
func (a *conduitHAAgent) RPCCount() int64 { return a.rpcCount.Load() }

// RPCPaths returns a copy of all control RPC paths received by the agent.
func (a *conduitHAAgent) RPCPaths() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.rpcPaths...)
}

// Close stops the fake agent cleanly.
func (a *conduitHAAgent) Close() { a.stopFn() }

// CloseSession abruptly closes the agent's current WebSocket TCP connection.
func (a *conduitHAAgent) CloseSession() { a.ws.CloseCurrent() }

// SilentDrop blackholes reads and writes on the agent's WebSocket TCP conn.
func (a *conduitHAAgent) SilentDrop() { a.ws.SilentDrop() }

// Restore resumes reads and writes on the agent's WebSocket TCP conn.
func (a *conduitHAAgent) Restore() { a.ws.Restore() }

// --- Fake Broker (real runtimebroker.ConduitDialer + real hubclient DeliverMessage) ---

// conduitHABroker is a fake broker wired with the real
// runtimebroker.ConduitDialer and a real HMAC-authenticated hubclient.Client
// (the same HTTP client path hubConduitDeliverer calls).
type conduitHABroker struct {
	BrokerID  string
	Dialer    *runtimebroker.ConduitDialer
	HubClient hubclient.Client
	ws        *faultWSDialer

	stopOnce sync.Once
	stopFn   func()
}

// StartBroker starts a fake broker dialing node's public conduit endpoint and
// waits until its conduit session is established and visible in the Postgres
// registry. rpcHandler handles hub->broker RPCs (optional).
func (e *conduitHAEnv) StartBroker(t *testing.T, node *conduitHANode, rpcHandler conduit.RPCHandler) *conduitHABroker {
	t.Helper()

	wsDialer := newFaultWSDialer()
	d, err := runtimebroker.NewConduitDialer(runtimebroker.ConduitDialConfig{
		HubEndpoint: node.Public.URL,
		BrokerID:    e.BrokerID,
		SecretKey:   e.BrokerSecret,
		Version:     "test",
		Incarnation: "broker-inc-" + uuid.NewString()[:8],
		RPCHandler:  rpcHandler,
		WS:          wsDialer.dialer,
		Rand:        func(int64) int64 { return 0 },
	})
	require.NoError(t, err)

	hc, err := hubclient.New(node.Public.URL, hubclient.WithHMACAuth(e.BrokerID, e.BrokerSecret))
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = d.Run(runCtx)
	}()

	fb := &conduitHABroker{
		BrokerID:  e.BrokerID,
		Dialer:    d,
		HubClient: hc,
		ws:        wsDialer,
	}
	fb.stopFn = func() {
		fb.stopOnce.Do(func() {
			wsDialer.Restore()
			cancel()
			wsDialer.CloseCurrent()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Error("conduit HA fake broker did not stop")
			}
		})
	}
	t.Cleanup(fb.stopFn)

	require.NoError(t, conduitHAPollUntil(10*time.Second, func() error {
		if d.CommandPathState() != api.BrokerHealthHealthy {
			return fmt.Errorf("broker conduit command path state = %q, want %q", d.CommandPathState(), api.BrokerHealthHealthy)
		}
		ps, err := node.RegStore.ListPrincipalSessions(context.Background(), registry.PrincipalBroker, e.BrokerID)
		if err != nil {
			return err
		}
		if len(ps.Sessions) == 0 {
			return errors.New("broker session not yet registered in Postgres")
		}
		return nil
	}))
	return fb
}

// DeliverMessage sends a deliver-message flush over the real broker->hub HTTP
// endpoint (POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/deliver).
func (b *conduitHABroker) DeliverMessage(ctx context.Context, agentID string, req *agentcontrol.DeliverRequest) (int, agentcontrol.DeliverResponse, error) {
	return b.HubClient.RuntimeBrokers().DeliverMessage(ctx, b.BrokerID, agentID, req)
}

// Close stops the fake broker cleanly.
func (b *conduitHABroker) Close() { b.stopFn() }

// CloseSession abruptly closes the broker's current WebSocket TCP connection.
func (b *conduitHABroker) CloseSession() { b.ws.CloseCurrent() }

// SilentDrop blackholes reads and writes on the broker's WebSocket TCP conn.
func (b *conduitHABroker) SilentDrop() { b.ws.SilentDrop() }

// Restore resumes reads and writes on the broker's WebSocket TCP conn.
func (b *conduitHABroker) Restore() { b.ws.Restore() }

// --- Helpers ---

func conduitHADSNParam(t *testing.T, dsn, key, value string) string {
	t.Helper()
	if !strings.Contains(dsn, "://") {
		return dsn + " " + key + "=" + value
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func conduitHAPollUntil(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = fn(); lastErr == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}

// --- Phase 1 Vertical Slice Test ---

// TestConduitHA_DeliverCrossNode exercises the full cross-node deliver-message
// path on a 2-node Postgres Conduit HA topology:
//   - Broker connected to Hub A (hub-a),
//   - Agent connected to Hub B (hub-b),
//   - Broker posts POST /api/v1/runtime-brokers/{brokerId}/agents/{agentId}/deliver
//     to Hub A over real HMAC-authenticated HTTP,
//   - Hub A binds the message row in Postgres, resolves the agent to a
//     RemoteSession on Hub B, and relays the control RPC over the loopback
//     internal peer listener,
//   - Fake agent (real pkg/sciontool/conduit + real control.Handler + fake
//     tmuxdeliver.Runner) executes exactly one flush and answers 200 delivered,
//   - Hub A finishes the message row in Postgres to delivered, and a same-id
//     replay returns the stored outcome without another relay hop or flush.
func TestConduitHA_DeliverCrossNode(t *testing.T) {
	env := newConduitHAEnv(t)
	ctx := context.Background()

	broker := env.StartBroker(t, env.HubA, nil)
	agent := env.StartAgent(t, env.HubB)

	require.Equal(t, env.HubB.InstanceID, agent.RelayInstanceID(), "agent must be admitted on Hub B")

	// Confirm the agent's session is visible in Postgres on Hub A and resolves
	// to a RemoteSession pointing at Hub B.
	var resolved router.Resolved
	require.NoError(t, conduitHAPollUntil(5*time.Second, func() error {
		var err error
		resolved, err = env.HubA.Router().Resolve(ctx, router.Request{
			Op:    router.OpStatefulRPC,
			Kind:  registry.PrincipalAgent,
			ID:    env.Agent.ID,
			Want:  registry.Want{ProjectID: env.Agent.ProjectID},
			Agent: agentIncarnationFacts(env.Agent),
		}, nil)
		return err
	}))
	require.False(t, resolved.Local, "Hub A must resolve the agent as a remote session")
	assert.Equal(t, env.HubB.InstanceID, resolved.Record.RelayInstanceID)
	_, isRemote := resolved.Session.(*relay.RemoteSession)
	assert.True(t, isRemote, "resolved session must be *relay.RemoteSession, got %T", resolved.Session)

	msgID := env.CreateMessage(t, store.MessageDispatchDispatched, "hello cross-node")
	text := "hello cross-node"
	deliveryID := "ha-deliver-" + uuid.NewString()
	req := &agentcontrol.DeliverRequest{
		V:          1,
		DeliveryID: deliveryID,
		MessageIDs: []string{msgID},
		Op:         agentcontrol.OpPaste,
		Text:       &text,
		DeadlineMS: 92000,
		SubmitKeys: []string{"Enter"},
	}

	hopsBeforeB := env.HubB.InternalRPCHops()
	hopsBeforeA := env.HubA.InternalRPCHops()

	status, resp, err := broker.DeliverMessage(ctx, env.Agent.ID, req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agentcontrol.OutcomeDelivered, resp.Outcome)
	assert.False(t, resp.Replayed)

	// Assert the relay hop crossed from Hub A to Hub B over the internal listener.
	assert.Equal(t, hopsBeforeB+1, env.HubB.InternalRPCHops(), "Hub B internal listener must receive exactly one relayed RPC hop")
	assert.Equal(t, hopsBeforeA, env.HubA.InternalRPCHops(), "Hub A internal listener must not receive an RPC hop")

	// Assert exactly one agent flush with recorded tmux argv.
	assert.Equal(t, int64(1), agent.RPCCount(), "agent must receive exactly one control RPC")
	assert.Equal(t, []string{"/v1/control/deliver-message"}, agent.RPCPaths())
	assert.Equal(t, 1, agent.Runner.Flushes(), "agent fake tmux runner must record exactly one flush")
	calls := agent.Runner.Calls()
	require.NotEmpty(t, calls)
	assert.Equal(t, []string{"tmux", "has-session", "-t", tmuxdeliver.Target}, calls[0])

	// Assert the message row reached delivered in real Postgres (readable from both hubs).
	for name, s := range map[string]*entadapter.CompositeStore{"hub-a": env.HubA.Store, "hub-b": env.HubB.Store} {
		m, err := s.GetMessage(ctx, msgID)
		require.NoError(t, err, name)
		assert.Equal(t, store.MessageDispatchDelivered, m.DispatchState, name)
	}

	// Same delivery_id replay must be served from Postgres without another relay hop or flush.
	status, replayResp, err := broker.DeliverMessage(ctx, env.Agent.ID, req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agentcontrol.OutcomeDelivered, replayResp.Outcome)
	assert.True(t, replayResp.Replayed)
	assert.Equal(t, hopsBeforeB+1, env.HubB.InternalRPCHops(), "replay must not trigger another relay hop")
	assert.Equal(t, int64(1), agent.RPCCount(), "replay must not call the agent again")
	assert.Equal(t, 1, agent.Runner.Flushes(), "replay must not flush again")
}
