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
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	sciontoollog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const recheckWait = 10 * time.Second

// streamWatch observes one tracked stream: its close and the re-checks
// recorded for it.
type streamWatch struct {
	closed chan string // "<code> <reason>", once
	once   sync.Once
}

// metricWaiter records re-check metrics and lets a test wait for one.
type metricWaiter struct {
	mu   sync.Mutex
	seen []string
	cond chan struct{}
}

func newMetricWaiter() *metricWaiter { return &metricWaiter{cond: make(chan struct{})} }

func newListenGapPublisher() *listenGapPublisher {
	return &listenGapPublisher{ChannelEventPublisher: NewChannelEventPublisher(), hooks: map[*listenHook]struct{}{}}
}

// conduitFaultStore makes selected reads fail.
type conduitFaultStore struct {
	store.Store
	failGetAgent     atomic.Bool
	getAgentOKCalls  atomic.Int32 // GetAgent calls that still succeed when failGetAgent is set
	failGetUser      atomic.Bool
	failBindingsList atomic.Bool
}

// fakeBroker is a broker connected to srv's control channel through the
// real upgrade path, so the hub runs its normal message loop for it.
type fakeBroker struct {
	ws        *websocket.Conn
	sessionID string
}

func connectFakeBroker(t *testing.T, srv *Server, brokerID string) *fakeBroker {
	t.Helper()
	require.NotNil(t, srv.controlChannel)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = srv.controlChannel.HandleUpgrade(w, r, brokerID)
	}))
	t.Cleanup(ts.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var connected wsprotocol.ConnectedMessage
	require.NoError(t, ws.ReadJSON(&connected))
	require.Equal(t, wsprotocol.TypeConnected, connected.Type)
	require.NotEmpty(t, connected.SessionID)
	return &fakeBroker{ws: ws, sessionID: connected.SessionID}
}

func brokerRequest(id string) router.Request {
	return router.Request{Op: router.OpStream, Kind: registry.PrincipalBroker, ID: id}
}

// brokerConduitFixture is a hub with broker HMAC authentication, hub.conduit
// on and a running in-process relay, served on an httptest server, plus one
// registered broker with its secret.
type brokerConduitFixture struct {
	srv      *Server
	store    store.Store
	public   *httptest.Server
	regStore registry.Store
	brokerID string
	secret   []byte
}

func newBrokerConduitFixture(t *testing.T, target *api.RuntimeTargetDescriptor, mod func(*ConduitRelayOptions)) *brokerConduitFixture {
	t.Helper()
	srv, s := testServerWithBrokerAuth(t)
	ctx := context.Background()
	f := &brokerConduitFixture{srv: srv, store: s, brokerID: tid("conduit-broker")}
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: f.brokerID, Name: "conduit-broker", Slug: "conduit-broker", Status: store.BrokerStatusOnline,
		RuntimeTarget: target, Created: time.Now(), Updated: time.Now(),
	}))
	key, err := srv.brokerAuthService.GenerateAndStoreSecret(ctx, f.brokerID)
	require.NoError(t, err)
	f.secret, err = base64.StdEncoding.DecodeString(key)
	require.NoError(t, err)

	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
	setConduitExperiment(t, srv, true)
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	peerSecret := make([]byte, 32)
	_, _ = rand.Read(peerSecret)
	auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: peerSecret, SelfID: "hub-a"})
	require.NoError(t, err)
	opts := ConduitRelayOptions{InstanceID: "hub-a", PeerAuth: auth, Store: f.regStore, Registry: registry.New(f.regStore, registry.Config{}), Clock: clock.NewFake(time.Now())}
	if mod != nil {
		mod(&opts)
	}
	require.NoError(t, srv.StartConduitRelay(ctx, opts))
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.shutdownConduitRelay(sctx)
	})
	f.public = httptest.NewServer(srv.Handler())
	t.Cleanup(f.public.Close)
	return f
}

// setConduitExperiment overrides hub.conduit on srv the way an admin toggle
// would; every other experiment keeps its production registry value.
func setConduitExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, conduitExperiment, enabled)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

type conduitTestClock struct {
	mu  sync.Mutex
	now time.Time
}

type conduitFixture struct {
	srv      *Server
	store    store.Store
	clock    *conduitTestClock
	agent    *store.Agent
	owner    *AuthenticatedUser
	portOnly *ScopedUserIdentity
	stranger *AuthenticatedUser
}

func newConduitFixture(t *testing.T) *conduitFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &conduitFixture{srv: srv, store: s, clock: &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}}

	project := &store.Project{ID: tid("conduit-project"), Name: "Conduit", Slug: "conduit"}
	require.NoError(t, s.CreateProject(ctx, project))
	ownerID, strangerID := tid("conduit-owner"), tid("conduit-stranger")
	createTestUserWithProjectRole(t, s, ownerID, "owner@conduit.test", project.ID, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, strangerID, "stranger@conduit.test", project.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, ownerID)
	ensureHubMembership(ctx, s, strangerID)
	f.owner = NewAuthenticatedUser(ownerID, "owner@conduit.test", "Owner", store.UserRoleMember, "api")
	f.stranger = NewAuthenticatedUser(strangerID, "stranger@conduit.test", "Stranger", store.UserRoleMember, "api")
	// A token holding only port access to the owner's agents: port
	// visibility without shell.
	f.portOnly = NewScopedUserIdentity(f.owner, project.ID, []string{store.UATScopeAgentPortAccess})

	f.agent = &store.Agent{
		ID: tid("conduit-agent"), Slug: "conduit-agent", Name: "Conduit Agent",
		ProjectID: project.ID, OwnerID: ownerID, Ancestry: []string{ownerID},
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, f.agent))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, f.agent.ID, []store.ExposedPort{{Port: 3000, Host: "127.0.0.1", Mode: "rw"}}))
	got, err := s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.agent = got

	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, f.clock.Now)
	setConduitExperiment(t, srv, true)
	return f
}

func tcpHeader(port string) grant.StreamHeader {
	return grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: port}}
}

var testGrantEncryptionKey = secret.DeriveLocalEncryptionKey("test-shared-secret")

func assertNoKeyMaterial(t *testing.T, s string, ring *grant.KeyRing) {
	t.Helper()
	for _, k := range ring.Keys {
		for _, enc := range []string{base64.StdEncoding.EncodeToString(k.Seed), base64.RawURLEncoding.EncodeToString(k.Seed), fmt.Sprintf("%x", k.Seed)} {
			assert.NotContains(t, s, enc)
		}
	}
	assert.NotContains(t, s, "seed")
}

// sseApp serves n events at /events, each sent after a value arrives on
// next (or every interval when next is nil), flushing after each one.
func sseApp(next <-chan struct{}, interval time.Duration, n int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		var tick <-chan time.Time
		if next == nil {
			tk := time.NewTicker(interval)
			defer tk.Stop()
			tick = tk.C
		}
		for i := 1; i <= n; i++ {
			select {
			case <-next:
			case <-tick:
			case <-r.Context().Done():
				return
			}
			if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	})
}

// readEvents reads n SSE data lines from body, calling each(i) after
// event i arrives.
func readEvents(t *testing.T, body io.Reader, n int, each func(int)) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for i := 1; i <= n; {
		require.True(t, sc.Scan(), "event stream ended before event %d: %v", i, sc.Err())
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		require.Equal(t, "data: "+strconv.Itoa(i), line)
		each(i)
		i++
	}
}

// proxyWS opens a WebSocket through the port proxy and checks one echo.
func proxyWS(t *testing.T, f *conduitProxyFixture) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("ping")))
	_, got, err := c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "ping", string(got))
	return c
}

// requireAuthzClose reads until the WebSocket closes and checks the code.
func requireAuthzClose(t *testing.T, c *websocket.Conn) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(recheckWait)))
	_, _, err := c.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, 4401, ce.Code)
	assert.Equal(t, "authz_expired", ce.Text)
}

func echoApp() http.Handler {
	up := websocket.Upgrader{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	})
}

// conduitProxyFixture is a relay fixture with hub.conduit on, an app
// listening inside the "agent" and that port exposed on the launched
// agent.
type conduitProxyFixture struct {
	*relayFixture
	app       *proxyApp
	userToken string
	// base is the hub URL agents and callers use (default f.public).
	base string
	// sciontool is the agent started last; sessions counts its admitted
	// sessions.
	sciontool *sconduit.Agent
	sessions  atomic.Int64
	// agentClock fires the sciontool agent's timers; onEnd, when set,
	// receives its reconnect decisions.
	agentClock *clock.Fake
	onEnd      func(end conduit.End, delay time.Duration, err error)
}

func newConduitProxyFixture(t *testing.T, app http.Handler) *conduitProxyFixture {
	t.Helper()
	f := &conduitProxyFixture{relayFixture: newRelayFixture(t, nil), app: newProxyApp(t, app)}
	f.base = f.public.URL
	setConduitExperiment(t, f.srv, true)
	require.True(t, f.srv.conduitServing())
	ctx := context.Background()
	require.NoError(t, f.store.UpdateAgentExposedPorts(ctx, f.launched.ID,
		[]store.ExposedPort{{Port: f.app.port, Host: conduitProxyHost, Mode: "rw"}}))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = got
	f.userToken, _, _, err = f.srv.userTokenService.GenerateTokenPair(
		f.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return f
}

// fixtureClock is the sciontool agent's clock: it reads the fixture's
// clock (grants are minted on it); its timers fire only when the test
// advances f.agentClock.
type fixtureClock struct {
	*clock.Fake
	now func() time.Time
}

// guardSciontoolLog trips sciontool/log's lazy Init (which replaces the
// process-wide slog default, ptone/scion#2337) and restores the globals.
func guardSciontoolLog() {
	prevDefault, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	sciontoollog.Init()
	slog.SetDefault(prevDefault)
	log.SetOutput(prevWriter)
	log.SetFlags(prevFlags)
}

// relayFixture is a conduit fixture with a running in-process relay, the
// hub's public handler on an httptest server and an agent that has an
// current run (so it has a run id, its launch id).
type relayFixture struct {
	*conduitFixture
	public   *httptest.Server
	regStore registry.Store
	reg      *registry.Registry
	launched *store.Agent
}

func newRelayFixture(t *testing.T, mod func(*ConduitRelayOptions)) *relayFixture {
	t.Helper()
	f := &relayFixture{conduitFixture: newConduitFixture(t)}
	ctx := context.Background()

	agent := &store.Agent{
		ID: tid("conduit-launched"), Slug: "conduit-launched", Name: "Launched",
		ProjectID: f.agent.ProjectID, OwnerID: f.agent.OwnerID, Ancestry: f.agent.Ancestry,
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseCreated),
	}
	require.NoError(t, f.store.CreateAgent(ctx, agent))
	_, err := f.store.SetAgentRunID(ctx, agent.ID, uuid.NewString(), nil)
	require.NoError(t, err)
	f.launched, err = f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotEmpty(t, f.launched.RunID)

	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: secret, SelfID: "hub-a"})
	require.NoError(t, err)
	opts := ConduitRelayOptions{InstanceID: "hub-a", PeerAuth: auth, Store: f.regStore, Registry: f.reg, Clock: clock.NewFake(time.Now())}
	if mod != nil {
		mod(&opts)
	}
	require.NoError(t, f.srv.StartConduitRelay(ctx, opts))
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.srv.shutdownConduitRelay(sctx)
	})
	f.public = httptest.NewServer(f.srv.Handler())
	t.Cleanup(f.public.Close)
	return f
}

// roundTrip writes msg on st and reads it back (the agent side echoes).
func roundTrip(t *testing.T, st conduit.Stream, msg string) {
	t.Helper()
	got := make([]byte, len(msg))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(st, got); done <- err }()
	_, err := st.Write([]byte(msg))
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Equal(t, msg, string(got))
	case <-time.After(10 * time.Second):
		t.Fatalf("no echo of %q", msg)
	}
}

func (w *streamWatch) close(code uint32, reason string) {
	w.once.Do(func() { w.closed <- strconv.FormatUint(uint64(code), 10) + " " + reason })
}

// waitClosed returns the stream's close, failing after recheckWait.
func (w *streamWatch) waitClosed(t *testing.T) string {
	t.Helper()
	select {
	case c := <-w.closed:
		return c
	case <-time.After(recheckWait):
		t.Fatal("stream was not closed")
		return ""
	}
}

// assertOpen fails if the stream was closed.
func (w *streamWatch) assertOpen(t *testing.T) {
	t.Helper()
	select {
	case c := <-w.closed:
		t.Fatalf("stream closed: %s", c)
	default:
	}
}

func (m *metricWaiter) RecordConduitStreamAuthz(trigger, outcome, kind string) {
	m.mu.Lock()
	m.seen = append(m.seen, trigger+"/"+outcome+"/"+kind)
	close(m.cond)
	m.cond = make(chan struct{})
	m.mu.Unlock()
}

// wait blocks until want has been recorded n times in total.
func (m *metricWaiter) wait(t *testing.T, want string, n int) {
	t.Helper()
	deadline := time.After(recheckWait)
	for {
		m.mu.Lock()
		count := 0
		for _, s := range m.seen {
			if s == want {
				count++
			}
		}
		ch := m.cond
		m.mu.Unlock()
		if count >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("metric %q not recorded %d times; seen %v", want, n, m.list())
		}
	}
}

func (m *metricWaiter) list() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func (s *conduitFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.failGetAgent.Load() {
		if s.getAgentOKCalls.Add(-1) < 0 {
			return nil, errInjectedStoreFault
		}
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *conduitFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.failGetUser.Load() {
		return nil, errInjectedStoreFault
	}
	return s.Store.GetUser(ctx, id)
}

func (s *conduitFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failBindingsList.Load() {
		return nil, errInjectedStoreFault
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// signedHeader returns the broker upgrade headers for path, signed with
// secret (the broker's own when nil).
func (f *brokerConduitFixture) signedHeader(t *testing.T, path string, secret []byte) http.Header {
	t.Helper()
	if secret == nil {
		secret = f.secret
	}
	req, err := http.NewRequest(http.MethodGet, f.public.URL+path, nil)
	require.NoError(t, err)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: f.brokerID, SecretKey: secret}).ApplyAuth(req))
	return req.Header
}

func (f *brokerConduitFixture) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(f.public.URL, "http") + path
}

// dial opens the broker's conduit session with hello's exec scope and
// incarnation.
func (f *brokerConduitFixture) dial(t *testing.T, execScope, incarnation string) (conduit.LocalSession, *conduitv1.Welcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL:    f.wsURL("/api/v1/conduit"),
		Header: func(context.Context) (http.Header, error) { return f.signedHeader(t, "/api/v1/conduit", nil), nil },
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   f.brokerID,
		Capabilities:  &conduitv1.Capabilities{EndpointIncarnation: incarnation, ExecScope: execScope},
	}
	s, w, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	if err != nil {
		return nil, nil, err
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return ls, w, nil
}

func (f *brokerConduitFixture) sessions(t *testing.T) []registry.SessionView {
	t.Helper()
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalBroker, f.brokerID)
	require.NoError(t, err)
	return ps.Sessions
}

// conduitSessionID returns the broker's only conduit session id, or "".
func (f *brokerConduitFixture) conduitSessionID(t *testing.T) string {
	rows := f.sessions(t)
	if len(rows) != 1 {
		return ""
	}
	return rows[0].Session.SessionID
}

func (c *conduitTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conduitTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// verifyWithKeys plays a target holding keys (for example a set it fetched
// before the hub's latest refresh).
func (f *conduitFixture) verifyWithKeys(t *testing.T, tok []byte, h grant.StreamHeader, pubs []grant.PublicKey) error {
	t.Helper()
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	_, err = grant.Verify(context.Background(), tok, keys, grant.Expectation{
		Target: f.target(), Header: h, ProjectID: f.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
	return err
}

func (f *conduitFixture) target() grant.Target {
	return grant.Target{Kind: grant.TargetKindAgent, ID: f.agent.ID, EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 3}
}

func (f *conduitFixture) mint(ident Identity, h grant.StreamHeader) ([]byte, *grant.Claims, error) {
	return f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{Identity: ident, Agent: f.agent, Stream: h, Target: f.target()})
}

// targetVerify plays the target: keys come from the hub's published set.
func (f *conduitFixture) targetVerify(t *testing.T, tok []byte, h grant.StreamHeader) error {
	t.Helper()
	pubs, err := f.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	_, err = grant.Verify(context.Background(), tok, keys, grant.Expectation{
		Target: f.target(), Header: h, ProjectID: f.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
	return err
}

func (f *conduitFixture) brokerTarget() grant.Target {
	return grant.Target{Kind: grant.TargetKindBroker, ID: "broker-1", EndpointIncarnation: "b", SessionID: "s", ConnectionEpoch: 1}
}

// withHeaderTimeout configures the port proxy's response header bound and
// wires a port proxy counter, returning a reader of its value.
func (f *conduitProxyFixture) withHeaderTimeout(t *testing.T, d time.Duration) (upstreamTimeouts func() int64) {
	t.Helper()
	f.srv.config.PortProxyResponseHeaderTimeout = d
	return portProxyCounter(t, f.srv)
}

// requireStreamsReleased waits until the hub holds no tracked port proxy
// stream.
func (f *conduitProxyFixture) requireStreamsReleased(t *testing.T) {
	t.Helper()
	a := f.srv.conduitAuthz.Load()
	require.NotNil(t, a)
	require.Eventually(t, func() bool { return a.Len() == 0 }, 5*time.Second, 20*time.Millisecond,
		"the hub-side conduit stream was not closed")
}

// withWriteTimeout serves the hub on a server with the given write
// timeout (as production does) and makes it f.base.
func (f *conduitProxyFixture) withWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	srv := httptest.NewUnstartedServer(f.srv.Handler())
	srv.Config.WriteTimeout = d
	srv.Start()
	t.Cleanup(srv.Close)
	f.base = srv.URL
}

// startAgent runs a sciontool conduit agent for the launched agent and
// returns once its session is admitted. stop ends it and waits for Run.
func (f *conduitProxyFixture) startAgent(t *testing.T) (stop func()) {
	t.Helper()
	admitted := make(chan *conduitv1.Welcome, 4)
	stop, _ = f.runAgent(t, f.launched.RunID, admitted)
	select {
	case w := <-admitted:
		require.Equal(t, f.launched.RunID, w.GetEndpointIncarnation())
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
	return stop
}

// runAgent runs a sciontool conduit agent presenting launchID, reporting
// admitted sessions on admitted, and returns a stop function and Run's
// result.
func (f *conduitProxyFixture) runAgent(t *testing.T, launchID string, admitted chan<- *conduitv1.Welcome) (stop func(), result <-chan error) {
	t.Helper()
	guardSciontoolLog()
	tok := f.agentToken(t, f.launched)
	f.agentClock = clock.NewFake(f.clock.Now())
	a, err := sconduit.New(sconduit.Options{
		HubURL:    f.base,
		AgentID:   f.launched.ID,
		ProjectID: f.launched.ProjectID,
		LaunchID:  launchID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) {
			f.sessions.Add(1)
			admitted <- w
		},
		OnEnd:   f.onEnd,
		Backoff: &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:   fixtureClock{Fake: f.agentClock, now: f.clock.Now},
	})
	require.NoError(t, err)
	f.sciontool = a
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	res := make(chan error, 1)
	go func() {
		defer close(exited)
		res <- a.Run(ctx)
	}()
	var once bool
	stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	}
	t.Cleanup(stop)
	return stop, res
}

// get sends a request through the hub's port proxy route.
func (f *conduitProxyFixture) get(t *testing.T, path string, header http.Header) *http.Response {
	t.Helper()
	return f.getWithin(t, path, header, 0)
}

// getWithin is get with the whole request, body read included, bounded
// by timeout (0: unbounded, as get).
func (f *conduitProxyFixture) getWithin(t *testing.T, path string, header http.Header, timeout time.Duration) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		f.base+"/api/v1/agents/"+f.launched.ID+"/ports/"+strconv.Itoa(f.app.port)+"/proxy"+path, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	client := http.DefaultClient
	if timeout > 0 {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// setExposedHost re-registers the fixture's exposed port (f.app.port)
// with host.
func (f *conduitProxyFixture) setExposedHost(t *testing.T, host string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.store.UpdateAgentExposedPorts(ctx, f.launched.ID,
		[]store.ExposedPort{{Port: f.app.port, Host: host, Mode: "rw"}}))
	got, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = got
}

// newProxyApp starts the app; a nil handler answers "hello <path>?<query>".
func newProxyApp(t *testing.T, handler http.Handler) *proxyApp {
	t.Helper()
	a := &proxyApp{got: make(chan *http.Request, 16)}
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-App", "ok")
			w.Header().Set("Set-Cookie", "app=1")
			_, _ = io.WriteString(w, "hello "+r.URL.Path+"?"+r.URL.RawQuery)
		})
	}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case a.got <- r.Clone(context.Background()):
		default:
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(a.srv.Close)
	u, err := url.Parse(a.srv.URL)
	require.NoError(t, err)
	a.port, err = strconv.Atoi(u.Port())
	require.NoError(t, err)
	return a
}

func (c fixtureClock) Now() time.Time { return c.now() }

func (f *relayFixture) agentToken(t *testing.T, a *store.Agent) string {
	t.Helper()
	tok, err := f.srv.GenerateAgentToken(a.ID, a.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	return tok
}

// dial opens a conduit session on GET /api/v1/conduit with the agent token.
func (f *relayFixture) dial(t *testing.T, token, launchID string) (conduit.LocalSession, *conduitv1.Welcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(f.public.URL, "http") + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"X-Scion-Agent-Token": {token}}, nil
		},
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   f.launched.ID,
		Capabilities:  &conduitv1.Capabilities{StreamKinds: []string{"pty"}, EndpointIncarnation: launchID},
	}
	s, w, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	if err != nil {
		return nil, nil, err
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return ls, w, nil
}

// setRunID sets the agent row's run id to runID ("" clears it, as a
// revert to a run-less row or a broker reporting an unlabelled entry does)
// and refreshes f.launched.
func (f *relayFixture) setRunID(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	if runID == "" {
		ok, err := f.store.CompareAndSwapAgentRunID(ctx, f.launched.ID, f.launched.RunID, "")
		require.NoError(t, err)
		require.True(t, ok)
	} else {
		_, err := f.store.SetAgentRunID(ctx, f.launched.ID, runID, nil)
		require.NoError(t, err)
	}
	a, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	f.launched = a
}

func (f *relayFixture) agentSessions(t *testing.T) []registry.SessionRecord {
	t.Helper()
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	out := make([]registry.SessionRecord, 0, len(ps.Sessions))
	for _, s := range ps.Sessions {
		out = append(out, s.Session)
	}
	return out
}

var errInjectedStoreFault = errors.New("injected store fault")

// proxyApp is the agent's in-container HTTP server, recording what it
// received.
type proxyApp struct {
	srv  *httptest.Server
	port int
	got  chan *http.Request
}

// controlAgent counts what reaches a sciontool conduit agent that serves
// the real control handler (pkg/sciontool/control) as its RPC handler.
type controlAgent struct {
	// rpcs counts every RpcRequest that reached the agent's RPC handler.
	rpcs atomic.Int32
	// kicks counts token refresh kicks made by the control handler.
	kicks atomic.Int32
	// paths receives the path of every RpcRequest (buffered).
	paths chan string
}

// listenGapPublisher is an in-process publisher whose "LISTEN connection"
// can be dropped: while down, published events are lost, and reconnect
// runs the AddOnListen callbacks, as PostgresEventPublisher does.
type listenGapPublisher struct {
	*ChannelEventPublisher
	down  atomic.Bool
	mu    sync.Mutex
	hooks map[*listenHook]struct{}
}

// portProxyCounter wires a port proxy counter on s and returns a reader
// of its value.
func portProxyCounter(t *testing.T, s *Server) func() int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelPortProxyMetrics(mp)
	require.NoError(t, err)
	s.SetPortProxyMetrics(rec)
	return func() int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "scion.hub.port_proxy.upstream_timeout" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
		return total
	}
}

func (p *listenGapPublisher) PublishRaw(subject string, data interface{}) {
	if p.down.Load() {
		return // missed while the listener is down
	}
	p.ChannelEventPublisher.PublishRaw(subject, data)
}

func (p *listenGapPublisher) AddOnListen(fn func()) func() {
	h := &listenHook{fn: fn}
	p.mu.Lock()
	p.hooks[h] = struct{}{}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.hooks, h)
		p.mu.Unlock()
	}
}

// reconnect brings the listener back and runs the callbacks.
func (p *listenGapPublisher) reconnect() {
	p.down.Store(false)
	p.mu.Lock()
	var fns []func()
	for h := range p.hooks {
		fns = append(fns, h.fn)
	}
	p.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}

// streamCalls wraps a tracked stream's Close and Renew to record calls.
// It must run before the stream is next checked.
type streamCalls struct {
	mu     sync.Mutex
	closes []uint32
	renews []error
}

func watchStream(st *conduitUserStream) *streamCalls {
	c := &streamCalls{}
	closeFn, renewFn := st.Close, st.Renew
	st.Close = func(code uint32, reason string) {
		c.mu.Lock()
		c.closes = append(c.closes, code)
		c.mu.Unlock()
		closeFn(code, reason)
	}
	if renewFn != nil {
		st.Renew = func() error {
			err := renewFn()
			c.mu.Lock()
			c.renews = append(c.renews, err)
			c.mu.Unlock()
			return err
		}
	}
	return c
}

func (c *streamCalls) snapshot() (closes []uint32, renews []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint32(nil), c.closes...), append([]error(nil), c.renews...)
}

// soleTrackedStream returns the only stream a tracks.
func soleTrackedStream(t *testing.T, a *conduitStreamAuthz) *conduitUserStream {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Len(t, a.streams, 1)
	for st := range a.streams {
		return st
	}
	return nil
}
