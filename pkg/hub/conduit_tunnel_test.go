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
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tunnelFixture is a hub (relay hub-a, hub.conduit on) whose launched
// agent exposes port 3000, with a relay clock the test drives (the
// re-check sweep runs every 30s on it).
type tunnelFixture struct {
	*ptyConduitFixture
	relayClock *clock.Fake
	// dials receives the agent-side end of every tcp connection a
	// sciontool agent of this fixture dialed (it echoes).
	dials chan net.Conn
}

const tunnelSweep = 30 * time.Second

func newTunnelFixture(t *testing.T, mods ...func(*ConduitRelayOptions)) *tunnelFixture {
	t.Helper()
	tf := &tunnelFixture{relayClock: clock.NewFake(time.Now()), dials: make(chan net.Conn, 16)}
	all := append([]func(*ConduitRelayOptions){func(o *ConduitRelayOptions) {
		o.Clock = tf.relayClock
		o.AuthzRecheckInterval = tunnelSweep
	}}, mods...)
	tf.ptyConduitFixture = newPTYConduitFixture(t, all...)
	tf.launched = tf.exposePort(t, tf.launched, 3000)
	return tf
}

// exposePort exposes port on a and returns the re-read row.
func (tf *tunnelFixture) exposePort(t *testing.T, a *store.Agent, ports ...int) *store.Agent {
	t.Helper()
	ctx := context.Background()
	var eps []store.ExposedPort
	for _, p := range ports {
		eps = append(eps, store.ExposedPort{Port: p, Host: "127.0.0.1", Mode: "rw"})
	}
	require.NoError(t, tf.store.UpdateAgentExposedPorts(ctx, a.ID, eps))
	got, err := tf.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	return got
}

// newAgent creates another launched agent of the owner in the project.
func (tf *tunnelFixture) newAgent(t *testing.T, slug string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug,
		ProjectID: tf.launched.ProjectID, OwnerID: tf.launched.OwnerID, Ancestry: tf.launched.Ancestry,
		RuntimeBrokerID: tf.launched.RuntimeBrokerID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, tf.store.CreateAgent(ctx, a))
	_, err := tf.store.SetAgentRunID(ctx, a.ID, uuid.NewString(), nil)
	require.NoError(t, err)
	got, err := tf.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	return tf.exposePort(t, got, 3000)
}

// startAgent runs a sciontool conduit agent for a against hubURL, serving
// tcp (echo, through tf.dials) and pty (fake PTY, through tf.spawned).
func (tf *tunnelFixture) startAgent(t *testing.T, hubURL string, a *store.Agent) {
	t.Helper()
	guardSciontoolLog()
	tok := tf.agentToken(t, a)
	admitted := make(chan *conduitv1.Welcome, 4)
	ag, err := sconduit.New(sconduit.Options{
		HubURL:    hubURL,
		AgentID:   a.ID,
		ProjectID: a.ProjectID,
		LaunchID:  a.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(tf.clock.Now()), now: tf.clock.Now},
		DialLocal: func(_ context.Context, _, _ string) (net.Conn, error) {
			c1, c2 := net.Pipe()
			go func() { _, _ = io.Copy(c2, c2) }()
			tf.dials <- c2
			return c1, nil
		},
		SpawnPTY: func(_ context.Context, req sconduit.PTYRequest) (sconduit.PTYProcess, error) {
			p := newFakePTY(req)
			tf.spawned <- p
			return p, nil
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = ag.Run(ctx)
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

// userToken returns a hub JWT for a user.
func (tf *tunnelFixture) userJWT(t *testing.T, userID, email string) string {
	t.Helper()
	tok, _, _, err := tf.srv.userTokenService.GenerateTokenPair(userID, email, "User", store.UserRoleMember, ClientTypeCLI)
	require.NoError(t, err)
	return tok
}

// uat mints, through the production token service, a user access token
// of the agent's owner confined to the project with scopes. The owner's own
// agent:attach and agent:port_access selectors are relationship-eligible
// (as in TestProjectUAT_OwnedAgentRequiresSelectedScope).
func (tf *tunnelFixture) uat(t *testing.T, name string, scopes ...string) string {
	t.Helper()
	tok, _, err := tf.srv.uatService.CreateToken(rs4MintContext(tf.launched.OwnerID), tf.launched.OwnerID, name, tf.launched.ProjectID, scopes, nil)
	require.NoError(t, err)
	return tok
}

// uatExpiring is uat with an explicit expiry.
func (tf *tunnelFixture) uatExpiring(t *testing.T, name string, expiresAt time.Time, scopes ...string) string {
	t.Helper()
	tok, _, err := tf.srv.uatService.CreateToken(rs4MintContext(tf.launched.OwnerID), tf.launched.OwnerID, name, tf.launched.ProjectID, scopes, &expiresAt)
	require.NoError(t, err)
	return tok
}

// tunnelClient plays the CLI: a user conduit session that accepts every
// stream the hub opens toward it.
type tunnelClient struct {
	ls      conduit.LocalSession
	mu      sync.Mutex
	cond    *sync.Cond
	streams map[uint32]conduit.Stream
}

type tunnelDialOpts struct {
	interceptor conduit.Interceptor
}

func (tf *tunnelFixture) dialUser(t *testing.T, bearer, userID string, opts ...tunnelDialOpts) (*tunnelClient, error) {
	t.Helper()
	c := &tunnelClient{streams: map[uint32]conduit.Stream{}}
	c.cond = sync.NewCond(&c.mu)
	cfg := conduit.Config{
		Clock: clock.Real(),
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return nil
			}
			c.mu.Lock()
			c.streams[st.ID()] = st
			c.cond.Broadcast()
			c.mu.Unlock()
			return nil
		}),
	}
	for _, o := range opts {
		cfg.Interceptor = o.interceptor
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(tf.public.URL, "http") + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer " + bearer}}, nil
		},
	}
	hello := &conduitv1.Hello{PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_USER, PrincipalId: userID}
	s, _, err := conduit.Dial(ctx, d, cfg, hello)
	if err != nil {
		return nil, err
	}
	c.ls = s.(conduit.LocalSession)
	t.Cleanup(func() { _ = c.ls.Close() })
	return c, nil
}

// tunnelResult is a decoded POST /v1/tunnels response.
type tunnelResult struct {
	status int
	ok     conduitTunnelResponse
	err    APIError
	body   string
}

func (c *tunnelClient) call(t *testing.T, method, path string, body any) tunnelResult {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := c.ls.Call(ctx, &conduitv1.RpcRequest{Method: method, Path: path, Body: data})
	require.NoError(t, err)
	r := tunnelResult{status: int(resp.GetStatus()), body: string(resp.GetBody())}
	if r.status == http.StatusOK {
		require.NoError(t, json.Unmarshal(resp.GetBody(), &r.ok), r.body)
	} else if strings.HasPrefix(r.body, "{") {
		var er ErrorResponse
		require.NoError(t, json.Unmarshal(resp.GetBody(), &er), r.body)
		r.err = er.Error
	}
	return r
}

func (c *tunnelClient) post(t *testing.T, agentID, kind string, params map[string]string) tunnelResult {
	t.Helper()
	return c.call(t, http.MethodPost, conduitTunnelsPath, conduitTunnelRequest{Agent: agentID, Kind: kind, Params: params})
}

// open posts a tunnel request that must succeed and returns its stream.
func (c *tunnelClient) open(t *testing.T, agentID, kind string, params map[string]string) conduit.Stream {
	t.Helper()
	r := c.post(t, agentID, kind, params)
	require.Equal(t, http.StatusOK, r.status, r.body)
	return c.stream(t, r.ok.StreamID)
}

func (c *tunnelClient) stream(t *testing.T, id uint32) conduit.Stream {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.streams[id] == nil {
		if time.Now().After(deadline) {
			t.Fatalf("stream %d was not opened toward the user session", id)
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		c.mu.Lock()
	}
	return c.streams[id]
}

func tcpParams(port string) map[string]string { return map[string]string{grant.ParamPort: port} }

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

// streamEnd reads st until it ends and returns the error it ended with.
func streamEnd(t *testing.T, st conduit.Stream) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := st.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("stream did not end")
		return nil
	}
}

func closeCode(t *testing.T, err error) uint32 {
	t.Helper()
	var ce *conduit.CloseError
	require.True(t, errors.As(err, &ce), "want a close code, got %v", err)
	return ce.Code
}

func (tf *tunnelFixture) ownerClient(t *testing.T) *tunnelClient {
	t.Helper()
	c, err := tf.dialUser(t, tf.userToken, tf.launched.OwnerID)
	require.NoError(t, err)
	return c
}

func setTunnelCap(t *testing.T, n int64) {
	t.Helper()
	prev := conduitTunnelStreamCap.Load()
	conduitTunnelStreamCap.Store(n)
	t.Cleanup(func() { conduitTunnelStreamCap.Store(prev) })
}

// TestConduitTunnel_AuthzMatrix: holding or lacking the tunnel permission
// (agent.port_access) crossed with attach, crossed with the kind. Port
// access never implies a shell; a user outside the agent's project is
// refused everything; an unknown kind is invalid for everyone.
func TestConduitTunnel_AuthzMatrix(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	ctx := context.Background()

	outsiderProject := &store.Project{ID: tid("tunnel-other-project"), Name: "Other", Slug: "other"}
	require.NoError(t, tf.store.CreateProject(ctx, outsiderProject))
	outsiderID := tid("tunnel-outsider")
	createTestUserWithProjectRole(t, tf.store, outsiderID, "outsider@conduit.test", outsiderProject.ID, store.ProjectRoleMember)
	ensureHubMembership(ctx, tf.store, outsiderID)

	type who struct {
		name, token, userID string
	}
	owner := who{"tunnel+attach (owner)", tf.userToken, tf.launched.OwnerID}
	portOnly := who{"tunnel only (port-access token)", tf.uat(t, "port-only", store.UATScopeAgentPortAccess), tf.launched.OwnerID}
	attachOnly := who{"attach only (attach token)", tf.uat(t, "attach-only", store.UATScopeAgentAttach), tf.launched.OwnerID}
	neither := who{"neither (project member)", tf.userJWT(t, tf.stranger.ID(), "stranger@conduit.test"), tf.stranger.ID()}
	outsider := who{"outside the project", tf.userJWT(t, outsiderID, "outsider@conduit.test"), outsiderID}

	type cell struct {
		status int
		code   string
	}
	allow := cell{http.StatusOK, ""}
	deny := cell{http.StatusForbidden, ErrCodeForbidden}
	invalid := cell{http.StatusBadRequest, ErrCodeInvalidRequest}
	// ssh is authorized for the owner, but the agent's session does not
	// serve ssh (no embedded SSH server yet).
	noSSH := cell{http.StatusServiceUnavailable, ErrCodeAgentSessionUnavailable}

	kinds := []struct {
		name, kind string
		params     map[string]string
	}{
		{"tcp exposed", grant.StreamKindTCP, tcpParams("3000")},
		{"tcp unexposed", grant.StreamKindTCP, tcpParams("4000")},
		{"tcp reserved", grant.StreamKindTCP, tcpParams("9810")},
		{"pty", grant.StreamKindPTY, nil},
		{"ssh", grant.StreamKindSSH, nil},
		{"unknown", "logs", nil},
	}
	matrix := []struct {
		who  who
		want []cell
	}{
		{owner, []cell{allow, deny, deny, allow, noSSH, invalid}},
		{portOnly, []cell{allow, deny, deny, deny, deny, invalid}},
		{attachOnly, []cell{deny, deny, deny, deny, deny, invalid}},
		{neither, []cell{deny, deny, deny, deny, deny, invalid}},
		{outsider, []cell{deny, deny, deny, deny, deny, invalid}},
	}
	for _, row := range matrix {
		c, err := tf.dialUser(t, row.who.token, row.who.userID)
		require.NoError(t, err, row.who.name)
		for i, k := range kinds {
			t.Run(row.who.name+"/"+k.name, func(t *testing.T) {
				r := c.post(t, tf.launched.ID, k.kind, k.params)
				want := row.want[i]
				require.Equal(t, want.status, r.status, r.body)
				if want.status != http.StatusOK {
					assert.Equal(t, want.code, r.err.Code, r.body)
					return
				}
				st := c.stream(t, r.ok.StreamID)
				assert.Equal(t, uint32(0), r.ok.StreamID%2, "the hub opens even stream ids")
				roundTrip(t, st, "matrix")
				require.NoError(t, st.Close())
			})
		}
	}
}

// TestConduitTunnel_UserStreamOpenRefused: a user session that sends a
// StreamOpen is refused with 4403 and the session is closed, through the
// hub handler.
func TestConduitTunnel_UserStreamOpenRefused(t *testing.T) {
	tf := newTunnelFixture(t)
	c := tf.ownerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.ls.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_TCP})
	require.Error(t, err)
	assert.Equal(t, conduit.CloseForbidden, closeCode(t, err))
	select {
	case <-c.ls.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the session was not closed")
	}
	assert.Equal(t, conduit.CloseForbidden, closeCode(t, c.ls.Err()))
}

// TestConduitTunnel_AdmissionRequiresUser: with hub.conduit on, a user is
// admitted; an anonymous caller is refused 401 and a broker identity 403
// before any upgrade. With the flag off every caller gets 404, as before.
func TestConduitTunnel_AdmissionRequiresUser(t *testing.T) {
	tf := newTunnelFixture(t)
	_, err := tf.dialUser(t, tf.userToken, tf.launched.OwnerID)
	require.NoError(t, err, "a user is admitted")

	anon := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	rec := httptest.NewRecorder()
	tf.srv.Handler().ServeHTTP(rec, anon)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	broker := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	broker = broker.WithContext(contextWithIdentity(broker.Context(), NewBrokerIdentity("broker-x")))
	rec = httptest.NewRecorder()
	tf.srv.handleConduit(rec, broker)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	setConduitExperiment(t, tf.srv, false)
	for _, hdr := range []http.Header{
		{"Authorization": {"Bearer " + tf.userToken}},
		{"X-Scion-Agent-Token": {tf.agentToken(t, tf.launched)}},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
		req.Header = hdr
		rec := httptest.NewRecorder()
		tf.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	}
	_, err = tf.dialUser(t, tf.userToken, tf.launched.OwnerID)
	require.Error(t, err, "flag off: users are refused as before")
}

// TestConduitTunnel_AgentRPCStill501: an agent session has no RPC handler
// on the hub, as before.
func TestConduitTunnel_AgentRPCStill501(t *testing.T) {
	tf := newTunnelFixture(t)
	ls, _, err := tf.dial(t, tf.agentToken(t, tf.launched), tf.launched.RunID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(conduitTunnelRequest{Agent: tf.launched.ID, Kind: grant.StreamKindTCP, Params: tcpParams("3000")})
	resp, err := ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	assert.Equal(t, int32(http.StatusNotImplemented), resp.GetStatus())
}

// TestConduitTunnel_TCPSplice: bytes round-trip both ways, a close from
// the user closes the agent's connection, and a close from the agent side
// closes the user's stream.
func TestConduitTunnel_TCPSplice(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)

	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	agentConn := <-tf.dials
	roundTrip(t, st, "hello through the tunnel")
	roundTrip(t, st, strings.Repeat("x", 300<<10)) // beyond one stream window
	require.NoError(t, st.Close())
	readDone := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, agentConn); readDone <- err }()
	select {
	case <-readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("a user close did not reach the agent's connection")
	}

	st2 := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	agentConn2 := <-tf.dials
	roundTrip(t, st2, "second")
	require.NoError(t, agentConn2.Close())
	err := streamEnd(t, st2)
	assert.True(t, errors.Is(err, io.EOF) || errors.As(err, new(*conduit.CloseError)), "agent close reached the user: %v", err)
	assert.True(t, c.ls.Err() == nil, "the session stays up")
}

// TestConduitTunnel_PTYResize: a pty tunnel spawns at the requested size,
// echoes, and forwards resizes; closing it ends the agent's PTY.
func TestConduitTunnel_PTYResize(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)

	st := c.open(t, tf.launched.ID, grant.StreamKindPTY, map[string]string{grant.ParamCols: "100", grant.ParamRows: "30"})
	p := waitSpawn(t, tf.spawned)
	assert.Equal(t, sconduit.PTYRequest{Cols: 100, Rows: 30, Session: "scion"}, p.req)
	roundTrip(t, st, "pty bytes")
	require.NoError(t, st.Resize(132, 43))
	select {
	case sz := <-p.resizes:
		assert.Equal(t, [2]uint16{132, 43}, sz)
	case <-time.After(10 * time.Second):
		t.Fatal("resize did not reach the agent")
	}
	require.NoError(t, st.Close())
	waitClosedPTY(t, p)
}

// TestConduitTunnel_CrossNode: the agent's session is held by another
// relay; the tunnel is opened on hub-a and spliced through hub-b.
func TestConduitTunnel_CrossNode(t *testing.T) {
	tf := newTunnelFixture(t)
	hubB := tf.startPeerRelay(t, "hub-b")
	tf.startAgent(t, hubB, tf.launched)
	recs := tf.agentSessions(t)
	require.Len(t, recs, 1)
	require.Equal(t, "hub-b", recs[0].RelayInstanceID)
	c := tf.ownerClient(t)

	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	agentConn := <-tf.dials
	roundTrip(t, st, "across nodes")
	require.NoError(t, agentConn.Close())
	streamEnd(t, st)

	pty := c.open(t, tf.launched.ID, grant.StreamKindPTY, nil)
	p := waitSpawn(t, tf.spawned)
	roundTrip(t, pty, "pty across nodes")
	require.NoError(t, pty.Resize(90, 20))
	select {
	case sz := <-p.resizes:
		assert.Equal(t, [2]uint16{90, 20}, sz)
	case <-time.After(10 * time.Second):
		t.Fatal("resize did not reach the agent across nodes")
	}
	require.NoError(t, pty.Close())
	waitClosedPTY(t, p)
}

// TestConduitTunnel_RevokedUserClosed4401: a user suspended while a tunnel
// is open is closed with 4401 authz_expired by the next sweep (relay
// clock), and only that tunnel's stream is closed.
func TestConduitTunnel_RevokedUserClosed4401(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)
	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "before")
	a := tf.srv.conduitAuthz.Load()
	require.Equal(t, 1, a.Len(), "the tunnel is tracked for re-checks")

	ctx := context.Background()
	u, err := tf.store.GetUser(ctx, tf.launched.OwnerID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, tf.store.UpdateUser(ctx, u))
	tf.relayClock.Advance(tunnelSweep)

	err = streamEnd(t, st)
	assert.Equal(t, conduit.CloseUnauthenticated, closeCode(t, err))
	var ce *conduit.CloseError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, conduitReasonAuthzExpired, ce.Reason)
	assert.Nil(t, c.ls.Err(), "the session stays up")
	require.Eventually(t, func() bool { return a.Len() == 0 }, 10*time.Second, 20*time.Millisecond)
}

// TestConduitTunnel_SharedSession: two tunnels to two agents share one
// user session; revoking one leaves the other open and the session up.
func TestConduitTunnel_SharedSession(t *testing.T) {
	tf := newTunnelFixture(t)
	second := tf.newAgent(t, "tunnel-second")
	tf.startAgent(t, tf.public.URL, tf.launched)
	tf.startAgent(t, tf.public.URL, second)
	c := tf.ownerClient(t)

	one := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	two := c.open(t, second.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.NotEqual(t, one.ID(), two.ID())
	roundTrip(t, one, "one")
	roundTrip(t, two, "two")

	// The first agent stops exposing the port: its tunnel is revoked.
	tf.exposePort(t, tf.launched)
	tf.srv.conduitAuthz.Load().Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{AgentID: tf.launched.ID})
	assert.Equal(t, conduit.CloseUnauthenticated, closeCode(t, streamEnd(t, one)))

	roundTrip(t, two, "still open")
	assert.Nil(t, c.ls.Err(), "the session stays up")
	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusForbidden, r.status, "a refusal on one agent")
	roundTrip(t, two, "after a refusal")
	assert.Nil(t, c.ls.Err())
}

// TestConduitTunnel_CapIs429: beyond the session's tunnel cap the request
// gets an RPC 429; the session and the open tunnel stay up.
func TestConduitTunnel_CapIs429(t *testing.T) {
	setTunnelCap(t, 1)
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)
	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))

	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusTooManyRequests, r.status, r.body)
	assert.Equal(t, ErrCodeRateLimited, r.err.Code)
	roundTrip(t, st, "still flowing")
	assert.Nil(t, c.ls.Err())

	require.NoError(t, st.Close())
	require.Eventually(t, func() bool {
		return c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000")).status == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "a closed tunnel frees its slot")
}

// TestConduitTunnel_GoAwayDrain: after GoAway a new request gets the
// session layer's bare 503 (no error code), while an open tunnel keeps
// flowing until the drain deadline and is then closed with 4503.
func TestConduitTunnel_GoAwayDrain(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	// The client drops the GoAway so it still sends the request (a
	// client that saw it refuses new calls locally).
	c, err := tf.dialUser(t, tf.userToken, tf.launched.OwnerID, tunnelDialOpts{
		interceptor: conduit.DropFrames(conduit.Inbound, nil, "go_away"),
	})
	require.NoError(t, err)
	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "before")

	rt := tf.srv.conduit.Load()
	const drain = 20 * time.Second
	require.NoError(t, rt.relay.GoAway(context.Background(), c.ls.Info().SessionID, conduit.GoAwayOptions{DrainDeadline: drain}))

	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusServiceUnavailable, r.status)
	assert.Equal(t, conduit.ErrDraining.Error(), r.body, "the bare session-layer answer")
	assert.Empty(t, r.err.Code, "no error code: the session layer answers before the tunnel handler")
	roundTrip(t, st, "during the drain")
	tf.relayClock.Advance(drain / 2)
	roundTrip(t, st, "half way through the drain")

	// The session's timers run on the relay clock.
	tf.relayClock.Advance(drain / 2)
	assert.Equal(t, conduit.CloseRelayRestart, closeCode(t, streamEnd(t, st)), "closed at the drain deadline")
}

// TestConduitTunnel_TypedErrors: every refusal carries its status and
// error code from the closed set; existence is checked before
// authorization, and authorization before the agent's state.
func TestConduitTunnel_TypedErrors(t *testing.T) {
	tf := newTunnelFixture(t)
	ctx := context.Background()
	stranger := tf.userJWT(t, tf.stranger.ID(), "stranger@conduit.test")

	stopped := tf.newAgent(t, "tunnel-stopped")
	stopped.Phase = string(state.PhaseStopped)
	require.NoError(t, tf.store.UpdateAgent(ctx, stopped))

	offlineBroker := uuid.NewString()
	require.NoError(t, tf.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: offlineBroker, Name: "offline", Slug: "offline", Status: store.BrokerStatusOffline,
	}))
	brokerless := tf.newAgent(t, "tunnel-broker-offline")
	brokerless.RuntimeBrokerID = offlineBroker
	require.NoError(t, tf.store.UpdateAgent(ctx, brokerless))

	owner := tf.ownerClient(t)
	strangerC, err := tf.dialUser(t, stranger, tf.stranger.ID())
	require.NoError(t, err)
	portOnlyC, err := tf.dialUser(t, tf.uat(t, "typed-port-only", store.UATScopeAgentPortAccess), tf.launched.OwnerID)
	require.NoError(t, err)
	attachOnlyC, err := tf.dialUser(t, tf.uat(t, "typed-attach-only", store.UATScopeAgentAttach), tf.launched.OwnerID)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		c      *tunnelClient
		agent  string
		kind   string
		params map[string]string
		status int
		code   string
	}{
		{"400 bad kind", owner, tf.launched.ID, "logs", nil, http.StatusBadRequest, ErrCodeInvalidRequest},
		{"400 bad params", owner, tf.launched.ID, grant.StreamKindTCP, tcpParams("03000"), http.StatusBadRequest, ErrCodeInvalidRequest},
		{"403 forbidden", strangerC, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"), http.StatusForbidden, ErrCodeForbidden},
		{"404 agent not found", owner, tid("no-such-agent"), grant.StreamKindTCP, tcpParams("3000"), http.StatusNotFound, ErrCodeAgentNotFound},
		{"404 before 403", strangerC, tid("no-such-agent"), grant.StreamKindTCP, tcpParams("3000"), http.StatusNotFound, ErrCodeAgentNotFound},
		{"409 agent not running", owner, stopped.ID, grant.StreamKindTCP, tcpParams("3000"), http.StatusConflict, ErrCodeAgentNotRunning},
		{"403 before 409", strangerC, stopped.ID, grant.StreamKindTCP, tcpParams("3000"), http.StatusForbidden, ErrCodeForbidden},
		// Each of the two checks on its own decides before the agent's
		// state: without the kind's action (attach) the port-only token
		// would get 409 here; without ActionTunnel the attach-only token
		// would.
		{"403 kind action before 409", portOnlyC, stopped.ID, grant.StreamKindPTY, nil, http.StatusForbidden, ErrCodeForbidden},
		{"403 ActionTunnel before 409", attachOnlyC, stopped.ID, grant.StreamKindPTY, nil, http.StatusForbidden, ErrCodeForbidden},
		{"409 for the owner with both", owner, stopped.ID, grant.StreamKindPTY, nil, http.StatusConflict, ErrCodeAgentNotRunning},
		{"503 broker unavailable", owner, brokerless.ID, grant.StreamKindTCP, tcpParams("3000"), http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail},
		{"503 agent session unavailable", owner, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"), http.StatusServiceUnavailable, ErrCodeAgentSessionUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.c.post(t, tc.agent, tc.kind, tc.params)
			assert.Equal(t, tc.status, r.status, r.body)
			assert.Equal(t, tc.code, r.err.Code, r.body)
		})
	}

	t.Run("404 deleted agent", func(t *testing.T) {
		gone := tf.newAgent(t, "tunnel-deleted")
		require.NoError(t, tf.store.DeleteAgent(ctx, gone.ID))
		r := owner.post(t, gone.ID, grant.StreamKindTCP, tcpParams("3000"))
		assert.Equal(t, http.StatusNotFound, r.status, r.body)
		assert.Equal(t, ErrCodeAgentNotFound, r.err.Code)
	})
	t.Run("429 rate limited", func(t *testing.T) {
		setTunnelCap(t, 0)
		r := owner.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
		assert.Equal(t, http.StatusTooManyRequests, r.status, r.body)
		assert.Equal(t, ErrCodeRateLimited, r.err.Code)
	})
	t.Run("401 credential expired", func(t *testing.T) {
		exp, err := tf.srv.userTokenService.GetTokenExpiry(tf.userToken)
		require.NoError(t, err)
		tf.clock.Advance(exp.Sub(tf.clock.Now()) + time.Second)
		r := owner.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
		assert.Equal(t, http.StatusUnauthorized, r.status, r.body)
		assert.Equal(t, ErrCodeUnauthorized, r.err.Code)
	})
	t.Run("405 names the allowed method", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := owner.ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodGet, Path: conduitTunnelsPath})
		require.NoError(t, err)
		assert.Equal(t, int32(http.StatusMethodNotAllowed), resp.GetStatus())
		assert.Equal(t, http.MethodPost, resp.GetHeaders()["Allow"])
	})
	t.Run("unknown rpc never carries agent_not_found", func(t *testing.T) {
		r := owner.call(t, http.MethodPost, "/v1/no-such-method", map[string]string{"agent": tid("no-such-agent")})
		assert.Equal(t, http.StatusNotFound, r.status, r.body)
		assert.NotEqual(t, ErrCodeAgentNotFound, r.err.Code)
		assert.NotContains(t, r.body, ErrCodeAgentNotFound)
	})
	assert.Nil(t, owner.ls.Err(), "refusals never close the session")
}

// TestConduitTunnel_CredentialExpiry: past the admitting credential's
// expiry a new request on the session is 401, and an open tunnel keeps
// flowing.
func TestConduitTunnel_CredentialExpiry(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)
	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "before expiry")

	exp, err := tf.srv.userTokenService.GetTokenExpiry(tf.userToken)
	require.NoError(t, err)
	tf.clock.Advance(exp.Sub(tf.clock.Now()) + time.Second)

	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusUnauthorized, r.status, r.body)
	assert.Equal(t, ErrCodeUnauthorized, r.err.Code)
	roundTrip(t, st, "after expiry")
	assert.Nil(t, c.ls.Err(), "the session stays up")
}

// TestConduitTunnel_ErrorTable: the refusal mapping is the closed set of
// design §3.8 rule 7, and 404 goes only with agent_not_found.
func TestConduitTunnel_ErrorTable(t *testing.T) {
	want := map[conduitTunnelFailure][3]any{
		tunnelInvalidRequest:         {http.StatusBadRequest, "invalid_request", ""},
		tunnelUnauthorized:           {http.StatusUnauthorized, "unauthorized", ""},
		tunnelForbidden:              {http.StatusForbidden, "forbidden", ""},
		tunnelPrincipalInactive:      {http.StatusForbidden, "forbidden", ""},
		tunnelAgentNotFound:          {http.StatusNotFound, "agent_not_found", ""},
		tunnelAgentNotRunning:        {http.StatusConflict, "agent_not_running", ""},
		tunnelRateLimited:            {http.StatusTooManyRequests, "rate_limited", ""},
		tunnelBrokerUnavailable:      {http.StatusServiceUnavailable, "runtime_broker_unavailable", ""},
		tunnelSessionUnavailable:     {http.StatusServiceUnavailable, "agent_session_unavailable", ""},
		tunnelStreamAuthzUnavailable: {http.StatusServiceUnavailable, "unavailable", "stream_authz_unavailable"},
		tunnelRegistryUnavailable:    {http.StatusServiceUnavailable, "unavailable", "registry_unavailable"},
		tunnelGrantKeysUnavailable:   {http.StatusServiceUnavailable, "unavailable", "grant_keys_unavailable"},
		tunnelConduitNotServing:      {http.StatusServiceUnavailable, "unavailable", "conduit_not_serving"},
		tunnelInternal:               {http.StatusInternalServerError, "internal_error", ""},
	}
	require.Len(t, conduitTunnelFailures, len(want))
	for f, m := range conduitTunnelFailures {
		assert.Equal(t, want[f], [3]any{m.status, m.code, m.reason}, "failure %d", f)
		if m.status == http.StatusNotFound {
			assert.Equal(t, ErrCodeAgentNotFound, m.code)
		}
	}
}

// TestJWTExpiryUnverified reads exp from a JWT payload.
func TestJWTExpiryUnverified(t *testing.T) {
	tf := newConduitFixture(t)
	tok, _, _, err := tf.srv.userTokenService.GenerateTokenPair(tf.owner.ID(), "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeCLI)
	require.NoError(t, err)
	want, err := tf.srv.userTokenService.GetTokenExpiry(tok)
	require.NoError(t, err)
	got, err := jwtExpiryUnverified(tok)
	require.NoError(t, err)
	assert.True(t, want.Equal(got), "want %v got %v", want, got)
	_, err = jwtExpiryUnverified("not-a-jwt")
	assert.Error(t, err)
}

// tunnelAgentFaultStore fails GetAgent for one agent id while armed.
type tunnelAgentFaultStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
}

// tunnelInjectedFault carries detail that must never reach the client.
var tunnelInjectedFault = errors.New("injected agent read fault: pg host db-internal-7, table agents")

func (s *tunnelAgentFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.agentID && s.fault.Active() {
		return nil, tunnelInjectedFault
	}
	return s.Store.GetAgent(ctx, id)
}

// TestConduitTunnel_InternalErrorIsGeneric500: an unexpected hub-side
// failure (a store read failing) is 500 internal_error with a generic
// body, never a 503 row; the cause is logged on the hub only, and the
// session stays up.
func TestConduitTunnel_InternalErrorIsGeneric500(t *testing.T) {
	tf := newTunnelFixture(t)
	c := tf.ownerClient(t)
	_, fault := installStoreFault(t, tf.srv, func(inner store.Store, f *storeFaultSwitch) *tunnelAgentFaultStore {
		return &tunnelAgentFaultStore{Store: inner, fault: f, agentID: tf.launched.ID}
	})
	fault.Arm()
	logs := capturePTYLogs(t)

	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusInternalServerError, r.status, r.body)
	assert.Equal(t, ErrCodeInternalError, r.err.Code)
	assert.Equal(t, "tunnel request failed", r.err.Message, "a generic message")
	assert.NotContains(t, r.body, "db-internal-7", "no store detail reaches the client")
	assert.NotContains(t, r.body, "agents")

	recs := logs.records(t, "Conduit tunnel request failed")
	require.Len(t, recs, 1, "the cause is logged on the hub")
	assert.Contains(t, recs[0]["error"], "db-internal-7")
	assert.Nil(t, c.ls.Err(), "the session stays up")
}

// setUserStatus sets the stored status of a user.
func (tf *tunnelFixture) setUserStatus(t *testing.T, userID, status string) {
	t.Helper()
	ctx := context.Background()
	u, err := tf.store.GetUser(ctx, userID)
	require.NoError(t, err)
	u.Status = status
	require.NoError(t, tf.store.UpdateUser(ctx, u))
}

// TestConduitTunnel_InactiveUserRefusedPerRequest: per-request
// authorization covers user status. While the session is open, a user
// that is no longer active is refused on a new request; the refusal closes
// nothing, so open tunnels and the session stay up until the tunnels' own
// re-check; once the user is active again, a request on the same session
// works. It also pins that the per-request rule (userActive) and the
// per-stream re-check (checkConduitUserStream) agree on the same user.
func TestConduitTunnel_InactiveUserRefusedPerRequest(t *testing.T) {
	tf := newTunnelFixture(t)
	second := tf.newAgent(t, "tunnel-inactive-second")
	tf.startAgent(t, tf.public.URL, tf.launched)
	tf.startAgent(t, tf.public.URL, second)
	c := tf.ownerClient(t)
	one := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	two := c.open(t, second.ID, grant.StreamKindTCP, tcpParams("3000"))

	logs := capturePTYLogs(t)
	tf.setUserStatus(t, tf.launched.OwnerID, store.UserStatusSuspended)
	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusForbidden, r.status, r.body)
	assert.Equal(t, ErrCodeForbidden, r.err.Code)
	r2 := c.post(t, second.ID, grant.StreamKindPTY, nil)
	assert.Equal(t, http.StatusForbidden, r2.status, "refused for every agent and kind: %s", r2.body)
	// Checked before the agent is resolved: an unknown agent id gets the
	// same 403 and the same body, not 404.
	missing := c.post(t, tid("no-such-agent"), grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusForbidden, missing.status, missing.body)
	assert.Equal(t, r.body, missing.body, "one generic body")
	assert.NotContains(t, r.body, "suspended", "the cause is not sent")
	var causes []any
	for _, rec := range logs.records(t, "Conduit tunnel refused") {
		causes = append(causes, rec["cause"])
	}
	assert.Contains(t, causes, "user suspended", "the cause is logged on the hub")

	// The refusal closes nothing: both tunnels and the session are up.
	roundTrip(t, one, "one after the refusal")
	roundTrip(t, two, "two after the refusal")
	assert.Nil(t, c.ls.Err())

	// The open tunnels end by their own re-check (4401), not the refusal.
	tf.srv.conduitAuthz.Load().Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{UserID: tf.launched.OwnerID})
	assert.Equal(t, conduit.CloseUnauthenticated, closeCode(t, streamEnd(t, one)))
	assert.Equal(t, conduit.CloseUnauthenticated, closeCode(t, streamEnd(t, two)))
	assert.Nil(t, c.ls.Err(), "the session stays up")

	// Active again: the same session opens tunnels.
	tf.setUserStatus(t, tf.launched.OwnerID, store.UserStatusActive)
	again := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, again, "active again")
}

// tunnelUserFaultStore fails GetUser for one user id while armed.
type tunnelUserFaultStore struct {
	store.Store
	fault  *storeFaultSwitch
	userID string
}

func (s *tunnelUserFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.userID && s.fault.Active() {
		return nil, errors.New("injected user read fault")
	}
	return s.Store.GetUser(ctx, id)
}

// TestConduitTunnel_UserLookupFaultIs500: a user-status lookup that fails
// is 500 internal_error and goes no further: no tunnel is opened or
// tracked, although the owner would otherwise be allowed (200).
func TestConduitTunnel_UserLookupFaultIs500(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)
	_, fault := installStoreFault(t, tf.srv, func(inner store.Store, f *storeFaultSwitch) *tunnelUserFaultStore {
		return &tunnelUserFaultStore{Store: inner, fault: f, userID: tf.launched.OwnerID}
	})
	fault.Arm()
	a := tf.srv.conduitAuthz.Load()

	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusInternalServerError, r.status, r.body)
	assert.Equal(t, ErrCodeInternalError, r.err.Code)
	assert.Equal(t, "tunnel request failed", r.err.Message)
	assert.Zero(t, a.Len(), "nothing was opened or tracked")
	select {
	case <-tf.dials:
		t.Fatal("the agent was dialed")
	default:
	}
	assert.Nil(t, c.ls.Err(), "the session stays up")
}

// TestConduitTunnel_UserStatusAgreesWithRecheck: the tunnel request path
// and the per-stream re-check share one user-status rule
// (conduitUserStatus). For active, suspended, deleted and unknown users,
// a scoped token of a suspended user, and an identity without a user row,
// the request is refused exactly when the re-check denies on user status,
// with the same cause.
func TestConduitTunnel_UserStatusAgreesWithRecheck(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	deletedID := tid("tunnel-deleted-user")
	createTestUserWithProjectRole(t, f.store, deletedID, "deleted@conduit.test", f.agent.ProjectID, store.ProjectRoleMember)
	require.NoError(t, f.store.DeleteUser(ctx, deletedID))
	suspendedID := tid("tunnel-suspended-user")
	createTestUserWithProjectRole(t, f.store, suspendedID, "suspended@conduit.test", f.agent.ProjectID, store.ProjectRoleMember)
	u, err := f.store.GetUser(ctx, suspendedID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(ctx, u))
	suspended := NewAuthenticatedUser(suspendedID, "suspended@conduit.test", "S", store.UserRoleMember, "cli")

	userStatusCauses := map[string]bool{"user not found": true, "user suspended": true}
	for _, tc := range []struct {
		name      string
		ident     UserIdentity
		refused   bool
		wantCause string
	}{
		{"active", f.owner, false, ""},
		{"suspended", suspended, true, "user suspended"},
		{"suspended, scoped token", NewScopedUserIdentity(suspended, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}), true, "user suspended"},
		{"deleted", NewAuthenticatedUser(deletedID, "deleted@conduit.test", "D", store.UserRoleMember, "cli"), true, "user not found"},
		{"unknown", NewAuthenticatedUser(tid("tunnel-ghost"), "ghost@conduit.test", "G", store.UserRoleMember, "cli"), true, "user not found"},
		{"no user row (dev)", NewDevUser(DevUserConfig{Username: "dev"}), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := f.srv.checkConduitUserStream(ctx, &conduitUserStream{
				Kind: grant.StreamKindTCP, Identity: tc.ident, UserID: tc.ident.ID(),
				AgentID: f.agent.ID, ProjectID: f.agent.ProjectID, Port: 3000,
			})
			recheckDeniedOnStatus := verdict == conduitAuthzDenied && userStatusCauses[reason]

			err := newConduitTunnelSession(f.srv, tc.ident, time.Time{}).userActive(ctx)
			var te *conduitTunnelError
			refused := errors.As(err, &te) && te.failure == tunnelPrincipalInactive

			assert.Equal(t, tc.refused, refused, "request path: %v", err)
			assert.Equal(t, tc.refused, recheckDeniedOnStatus, "re-check: %v %q", verdict, reason)
			if tc.refused {
				assert.Equal(t, tc.wantCause, te.cause)
				assert.Equal(t, tc.wantCause, reason)
			}
		})
	}
}

// tunnelBrokerFaultStore fails GetRuntimeBroker for one broker id while
// armed.
type tunnelBrokerFaultStore struct {
	store.Store
	fault    *storeFaultSwitch
	brokerID string
}

func (s *tunnelBrokerFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if id == s.brokerID && s.fault.Active() {
		return nil, errors.New("injected broker read fault")
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

// TestConduitTunnel_BrokerLookupFaultIs500: a broker-row read that fails
// is an internal error (generic 500), not "broker online": nothing is
// opened.
func TestConduitTunnel_BrokerLookupFaultIs500(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	c := tf.ownerClient(t)
	_, fault := installStoreFault(t, tf.srv, func(inner store.Store, f *storeFaultSwitch) *tunnelBrokerFaultStore {
		return &tunnelBrokerFaultStore{Store: inner, fault: f, brokerID: tf.launched.RuntimeBrokerID}
	})
	fault.Arm()

	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusInternalServerError, r.status, r.body)
	assert.Equal(t, ErrCodeInternalError, r.err.Code)
	assert.Equal(t, "tunnel request failed", r.err.Message)
	assert.Zero(t, tf.srv.conduitAuthz.Load().Len(), "nothing was opened or tracked")
	select {
	case <-tf.dials:
		t.Fatal("the agent was dialed")
	default:
	}
	assert.Nil(t, c.ls.Err(), "the session stays up")
}

// TestConduitTunnel_UATExpiryCaptured: a session admitted with a user
// access token takes the token's stored expiry: past it, a request is 401
// and an open tunnel keeps flowing.
func TestConduitTunnel_UATExpiryCaptured(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	expiresAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	tok := tf.uatExpiring(t, "expiring", expiresAt, store.UATScopeAgentPortAccess)
	c, err := tf.dialUser(t, tok, tf.launched.OwnerID)
	require.NoError(t, err)
	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "before expiry")

	tf.clock.Advance(expiresAt.Sub(tf.clock.Now()) + time.Second)
	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusUnauthorized, r.status, r.body)
	assert.Equal(t, ErrCodeUnauthorized, r.err.Code)
	roundTrip(t, st, "after expiry")
	assert.Nil(t, c.ls.Err())
}

// TestConduitTunnel_SessionLossEndsEveryTunnel: losing the user session
// ends every tunnel on it: each target leg closes and the re-check tracks
// nothing.
func TestConduitTunnel_SessionLossEndsEveryTunnel(t *testing.T) {
	tf := newTunnelFixture(t)
	second := tf.newAgent(t, "tunnel-loss-second")
	tf.startAgent(t, tf.public.URL, tf.launched)
	tf.startAgent(t, tf.public.URL, second)
	c := tf.ownerClient(t)
	one := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	connOne := <-tf.dials
	two := c.open(t, second.ID, grant.StreamKindTCP, tcpParams("3000"))
	connTwo := <-tf.dials
	roundTrip(t, one, "one")
	roundTrip(t, two, "two")
	a := tf.srv.conduitAuthz.Load()
	require.Equal(t, 2, a.Len())

	require.NoError(t, c.ls.Close())
	for i, conn := range []net.Conn{connOne, connTwo} {
		done := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, conn); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("target leg %d did not close", i+1)
		}
	}
	require.Eventually(t, func() bool { return a.Len() == 0 }, 10*time.Second, 20*time.Millisecond, "the re-check tracks nothing")
}

// TestConduitTunnel_CredentialKinds: a user session is admitted for the
// credential kinds with a defined expiry rule (hub user JWT, user access
// token, external bearer, auth proxy) and the dev token; a kind with no
// defined expiry rule is refused with 403 before any upgrade.
func TestConduitTunnel_CredentialKinds(t *testing.T) {
	tf := newTunnelFixture(t)
	owner := NewAuthenticatedUser(tf.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, "cli")
	call := func(ident Identity, authType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
		ctx := contextWithIdentity(req.Context(), ident)
		ctx = context.WithValue(ctx, logging.AuthTypeKey{}, authType)
		rec := httptest.NewRecorder()
		tf.srv.handleConduit(rec, req.WithContext(ctx))
		return rec
	}
	for _, tc := range []struct {
		name     string
		ident    Identity
		authType string
		want     int
	}{
		{"kind without an expiry rule", owner, AuthTypeSignedURL, http.StatusForbidden},
		{"unknown auth type", owner, "mystery", http.StatusForbidden},
		{"no auth type", owner, "", http.StatusForbidden},
		// Admitted kinds pass the credential check; the plain GET then
		// stops at the upgrade check.
		{"auth proxy", owner, AuthTypeProxy, http.StatusBadRequest},
		{"dev token", NewDevUser(DevUserConfig{Username: "dev"}), AuthTypeDevToken, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(tc.ident, tc.authType)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}

// TestConduitTunnel_ProxySessionCap: a session admitted through the auth
// proxy records admission time plus proxy_session_max_age as its
// credential expiry: before it a request opens a tunnel, after it a
// request is 401 and the open tunnel keeps flowing.
func TestConduitTunnel_ProxySessionCap(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.srv.config.ConduitProxySessionMaxAge = 10 * time.Minute
	tf.startAgent(t, tf.public.URL, tf.launched)
	owner := NewAuthenticatedUser(tf.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, "cli")
	// Stands in for the auth middleware's trusted-proxy branch.
	proxied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := contextWithIdentity(r.Context(), owner)
		ctx = context.WithValue(ctx, logging.AuthTypeKey{}, AuthTypeProxy)
		tf.srv.handleConduit(w, r.WithContext(ctx))
	}))
	t.Cleanup(proxied.Close)
	public := tf.public
	tf.public = proxied
	c, err := tf.dialUser(t, "unused", tf.launched.OwnerID)
	tf.public = public
	require.NoError(t, err)

	st := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "within the cap")
	tf.clock.Advance(9 * time.Minute)
	roundTrip(t, c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000")), "still within the cap")

	tf.clock.Advance(time.Minute + time.Second)
	r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	assert.Equal(t, http.StatusUnauthorized, r.status, r.body)
	assert.Equal(t, ErrCodeUnauthorized, r.err.Code)
	roundTrip(t, st, "after the cap")
	assert.Nil(t, c.ls.Err())
}

// TestConduitProxySessionMaxAge_Default: an unset setting is 1h.
func TestConduitProxySessionMaxAge_Default(t *testing.T) {
	s := &Server{}
	assert.Equal(t, time.Hour, s.conduitProxySessionMaxAge())
	s.config.ConduitProxySessionMaxAge = 5 * time.Minute
	assert.Equal(t, 5*time.Minute, s.conduitProxySessionMaxAge())
}

// switchGrantKeyStore is an in-memory grant key store whose loads fail
// while fail is set.
type switchGrantKeyStore struct {
	memoryConduitGrantKeyStore
	fail atomic.Bool
}

func (s *switchGrantKeyStore) Load(ctx context.Context) (*grant.KeyRing, int, error) {
	if s.fail.Load() {
		return nil, 0, errors.New("injected grant key store fault")
	}
	return s.memoryConduitGrantKeyStore.Load(ctx)
}

// TestConduitTunnel_TransientFaults: a transient hub fault is 503
// unavailable with a reason (never 403, and never agent_session_unavailable);
// a missing stream re-check is 500.
func TestConduitTunnel_TransientFaults(t *testing.T) {
	want503 := func(t *testing.T, r tunnelResult, reason string) {
		t.Helper()
		assert.Equal(t, http.StatusServiceUnavailable, r.status, r.body)
		assert.Equal(t, ErrCodeUnavailable, r.err.Code, r.body)
		assert.Equal(t, reason, r.err.Details["reason"], r.body)
	}

	// The fault wrappers below are installed before any session starts
	// (restored by t.Cleanup) and switched on with an atomic flag, so no
	// running goroutine sees a field change.
	t.Run("stream_authz_unavailable", func(t *testing.T) {
		tf := newTunnelFixture(t)
		fs := &conduitFaultStore{Store: tf.srv.authzService.store}
		tf.srv.authzService.store = fs
		t.Cleanup(func() { tf.srv.authzService.store = fs.Store })
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)
		fs.failBindingsList.Store(true)
		want503(t, c.post(t, tf.launched.ID, grant.StreamKindPTY, nil), "stream_authz_unavailable")
		assert.Zero(t, tf.srv.conduitAuthz.Load().Len())
	})
	t.Run("registry_unavailable", func(t *testing.T) {
		tf := newTunnelFixture(t)
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)
		tf.regFault.Store(true)
		t.Cleanup(func() { tf.regFault.Store(false) })
		want503(t, c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000")), "registry_unavailable")
	})
	t.Run("grant_keys_unavailable", func(t *testing.T) {
		tf := newTunnelFixture(t)
		ks := &switchGrantKeyStore{}
		prev := tf.srv.conduitGrants
		tf.srv.conduitGrants = newConduitGrantKeys(ks, tf.clock.Now)
		t.Cleanup(func() { tf.srv.conduitGrants = prev })
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)
		ks.fail.Store(true)
		// Past the ring cache interval, the next grant reloads the ring.
		tf.clock.Advance(conduitGrantKeyRefresh + time.Second)
		want503(t, c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000")), "grant_keys_unavailable")
		select {
		case <-tf.dials:
			t.Fatal("the agent was dialed")
		default:
		}
	})
	t.Run("conduit_not_serving", func(t *testing.T) {
		tf := newTunnelFixture(t)
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)
		setConduitExperiment(t, tf.srv, false)
		want503(t, c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000")), "conduit_not_serving")
	})
	t.Run("missing re-check is 500", func(t *testing.T) {
		tf := newTunnelFixture(t)
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)
		a := tf.srv.conduitAuthz.Swap(nil)
		t.Cleanup(func() { tf.srv.conduitAuthz.Store(a) })
		r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
		assert.Equal(t, http.StatusInternalServerError, r.status, r.body)
		assert.Equal(t, ErrCodeInternalError, r.err.Code)
		assert.Equal(t, "tunnel request failed", r.err.Message)
	})
}
