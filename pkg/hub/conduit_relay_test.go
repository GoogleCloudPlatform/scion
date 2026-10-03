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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// relayFixture is a conduit fixture with a running in-process relay, the
// hub's public handler on an httptest server and an agent that has an
// active launch (so it has a launch_id).
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
	_, err := f.store.BeginLaunch(ctx, agent.ID, "create", time.Hour)
	require.NoError(t, err)
	f.launched, err = f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotEmpty(t, f.launched.LaunchID)

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

// TestConduitEndpoint_HTTPGate: GET /api/v1/conduit is 404 with hub.conduit
// off, open to agent principals with agent:port:forward only, and 503 when
// the flag is on but no relay runs on this node.
func TestConduitEndpoint_HTTPGate(t *testing.T) {
	f := newConduitFixture(t)
	agentTok, err := f.srv.GenerateAgentToken(f.agent.ID, f.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	noScopeTok, err := f.srv.agentTokenService.GenerateAgentToken(f.agent.ID, f.agent.ProjectID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		flag     bool
		header   string
		value    string
		wantCode int
	}{
		{name: "flag off", flag: false, header: "X-Scion-Agent-Token", value: agentTok, wantCode: http.StatusNotFound},
		{name: "user principal", flag: true, header: "Authorization", value: "Bearer " + testDevToken, wantCode: http.StatusForbidden},
		{name: "agent without port forward scope", flag: true, header: "X-Scion-Agent-Token", value: noScopeTok, wantCode: http.StatusForbidden},
		{name: "agent, no relay on this node", flag: true, header: "X-Scion-Agent-Token", value: agentTok, wantCode: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setConduitExperiment(t, f.srv, tc.flag)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
			req.Header.Set(tc.header, tc.value)
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			assert.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
		})
	}
}

// TestConduitEndpoint_AgentSession: an agent presenting its launch_id is
// admitted with the hub's grant keys in the Welcome; a stale launch id is
// refused 4409 and writes no session row.
func TestConduitEndpoint_AgentSession(t *testing.T) {
	f := newRelayFixture(t, nil)
	tok := f.agentToken(t, f.launched)

	_, _, err := f.dial(t, tok, "stale-launch")
	require.Error(t, err)
	assert.True(t, relay.IsSupersededIncarnation(err), "stale launch id: %v", err)
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	assert.Empty(t, ps.Sessions, "a refused Hello writes no row")

	_, wel, err := f.dial(t, tok, f.launched.LaunchID)
	require.NoError(t, err)
	assert.Equal(t, "hub-a", wel.GetRelayInstanceId())
	assert.Equal(t, f.launched.LaunchID, wel.GetEndpointIncarnation())
	assert.NotEmpty(t, wel.GetGrantKeys(), "Welcome carries the grant keys")
	ps, err = f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	require.Len(t, ps.Sessions, 1)
	assert.Equal(t, f.launched.ProjectID, ps.Sessions[0].Session.ProjectID, "project comes from the agent row")
}

// TestConduitInternalHandler_PeerIdentity (C7, hub side): the internal
// relay API answers 503 before a relay runs and 401 to a caller without a
// relay-peer identity.
func TestConduitInternalHandler_PeerIdentity(t *testing.T) {
	f := newConduitFixture(t)
	h := f.srv.ConduitInternalHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/v1/conduit/self", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "no relay yet")

	rf := newRelayFixture(t, nil)
	for _, path := range []string{"/internal/v1/conduit/self", "/internal/v1/conduit/sessions/x/rpc", "/internal/v1/conduit/sessions/x/stream"} {
		rec := httptest.NewRecorder()
		rf.srv.ConduitInternalHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
	}
	// The internal API is never on the public mux.
	// An authenticated request to the internal path on the public mux is
	// not routed to the relay.
	rec = doRequest(t, rf.srv, http.MethodGet, "/internal/v1/conduit/self", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "not served by the relay on the public mux")
}

// TestStartConduitRelay_StartupChecks (C8, hub side): hosted HA refuses a
// grant key ring without the shared at-rest key and an unaddressable
// relay; with the flag off StartConduitRelay is a no-op.
func TestStartConduitRelay_StartupChecks(t *testing.T) {
	auth := func(t *testing.T) relay.PeerAuth {
		a, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: make([]byte, 32), SelfID: "hub-a"})
		require.NoError(t, err)
		return a
	}
	for _, tc := range []struct {
		name    string
		flag    bool
		noKey   bool
		ha      bool
		wantErr error
		running bool
	}{
		{name: "flag off is a no-op", flag: false, ha: true, noKey: true},
		{name: "HA without the at-rest key", flag: true, ha: true, noKey: true, wantErr: ErrConduitNoAtRestKey},
		{name: "HA unaddressable", flag: true, ha: true, wantErr: registry.ErrUnaddressable},
		{name: "single node unaddressable runs", flag: true, noKey: true, running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			setConduitExperiment(t, srv, tc.flag)
			if tc.noKey {
				srv.encryptionKey = nil
			} else {
				srv.encryptionKey = testGrantEncryptionKey
			}
			assert.Equal(t, !tc.noKey, srv.ConduitGrantRingShared())
			st := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
			err := srv.StartConduitRelay(context.Background(), ConduitRelayOptions{
				InstanceID: "hub-a", RequireHA: tc.ha, PeerAuth: auth(t), Store: st, Clock: clock.NewFake(time.Now()),
			})
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				srv.shutdownConduitRelay(ctx)
			})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, srv.conduit.Load(), "a failed start leaves no relay")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.running, srv.conduit.Load() != nil)
		})
	}
}

// TestConduitRelay_ReconnectWindowWired: the configured window reaches the
// GoAway the hub's relay sends.
func TestConduitRelay_ReconnectWindowWired(t *testing.T) {
	f := newRelayFixture(t, func(o *ConduitRelayOptions) { o.ReconnectWindow = 1500 * time.Millisecond })
	sess, _, err := f.dial(t, f.agentToken(t, f.launched), f.launched.LaunchID)
	require.NoError(t, err)
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	require.Len(t, ps.Sessions, 1)
	rt := f.srv.conduit.Load()
	// Wait for registration, then send a planned close.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, ok := rt.relay.Local(ctx, ps.Sessions[0].Session.SessionID)
	require.True(t, ok)
	require.NoError(t, rt.relay.GoAway(ps.Sessions[0].Session.SessionID, conduit.GoAwayOptions{Reason: relay.ReasonDraining, ReconnectAfter: 1500 * time.Millisecond}))
	select {
	case <-sess.GoAwayReceived():
	case <-time.After(10 * time.Second): // safety net
		t.Fatal("no GoAway")
	}
}

// TestConduitForgetAgent: agent deletion closes the agent's sessions with
// 4401, deletes its rows on every relay and forgets its epoch.
func TestConduitForgetAgent(t *testing.T) {
	f := newRelayFixture(t, nil)
	sess, _, err := f.dial(t, f.agentToken(t, f.launched), f.launched.LaunchID)
	require.NoError(t, err)
	ctx := context.Background()
	// A row of the same agent on another (stale) relay.
	_, err = f.regStore.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: "remote-row", PrincipalKind: registry.PrincipalAgent, PrincipalID: f.launched.ID,
		ProjectID: f.launched.ProjectID, RelayInstanceID: "hub-b", RelayGeneration: 1,
	})
	if err != nil {
		t.Logf("seeding a remote row: %v (continuing with the local row only)", err)
	}

	f.srv.conduitForgetAgent(ctx, f.launched.ID)
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second): // safety net
		t.Fatal("session not closed")
	}
	assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(sess.Err(), 0), "session ended with %v", sess.Err())
	ps, err := f.regStore.ListPrincipalSessions(ctx, registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	assert.Empty(t, ps.Sessions, "every row of the deleted agent is gone")
}

// TestConduitAgentDeletedDuringHandshake: an agent deleted (and forgotten)
// after handleConduit read its row but before the session registered is
// closed with 4401 once registered, and leaves no registry rows behind.
func TestConduitAgentDeletedDuringHandshake(t *testing.T) {
	ctx := context.Background()
	var f *relayFixture
	var once sync.Once
	f = newRelayFixture(t, func(o *ConduitRelayOptions) {
		o.testHookAdmission = func() {
			once.Do(func() {
				assert.NoError(t, f.store.DeleteAgent(ctx, f.launched.ID)) // server goroutine: no FailNow
				f.srv.conduitForgetAgent(ctx, f.launched.ID)
			})
		}
	})
	sess, _, err := f.dial(t, f.agentToken(t, f.launched), f.launched.LaunchID)
	if err == nil {
		select {
		case <-sess.Done():
		case <-time.After(10 * time.Second): // safety net
			t.Fatal("session of a deleted agent not closed")
		}
		err = sess.Err()
	}
	assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(err, 0), "session ended with %v", err)
	require.Eventually(t, func() bool { // Serve deletes the row after the close
		ps, err := f.regStore.ListPrincipalSessions(ctx, registry.PrincipalAgent, f.launched.ID)
		return err == nil && len(ps.Sessions) == 0
	}, 10*time.Second, 10*time.Millisecond, "the deleted agent's session row is removed")
}

// TestAgentTokenRefresh_ConduitGrantKeys: the agent token-refresh response
// carries the grant keys with hub.conduit on, and omits them when off.
func TestAgentTokenRefresh_ConduitGrantKeys(t *testing.T) {
	f := newConduitFixture(t)
	tok, err := f.srv.GenerateAgentToken(f.agent.ID, f.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	refresh := func() map[string]json.RawMessage {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/token/refresh", nil)
		req.Header.Set("X-Scion-Agent-Token", tok)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		// A refresh revokes the presented token; the next call uses the
		// new one.
		require.NoError(t, json.Unmarshal(body["token"], &tok))
		return body
	}
	body := refresh()
	require.Contains(t, body, "conduit_grant_keys")
	var keys []map[string]any
	require.NoError(t, json.Unmarshal(body["conduit_grant_keys"], &keys))
	assert.NotEmpty(t, keys)

	setConduitExperiment(t, f.srv, false)
	assert.NotContains(t, refresh(), "conduit_grant_keys")
}

// TestConduitRegistryReap: the maintenance singleton reaps stale sessions
// with the registry's default horizons.
func TestConduitRegistryReap(t *testing.T) {
	now := time.Now()
	var clk = now
	f := newRelayFixture(t, func(o *ConduitRelayOptions) { o.RegistryNow = func() time.Time { return clk } })
	ctx := context.Background()
	gone, err := f.reg.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "gone"})
	require.NoError(t, err)
	_, err = f.regStore.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: "stale", PrincipalKind: registry.PrincipalUser, PrincipalID: "u1",
		RelayInstanceID: "gone", RelayGeneration: gone, Transport: registry.TransportWS,
	})
	require.NoError(t, err)
	clk = now.Add(24 * time.Hour)
	f.srv.conduitRegistryReapHandler()(ctx)
	ps, err := f.regStore.ListPrincipalSessions(ctx, registry.PrincipalUser, "u1")
	require.NoError(t, err)
	assert.Empty(t, ps.Sessions, "a day-old session of a dead relay is reaped")
}

// TestConduitGrantKeyNodeRefreshPinned: the config minimum for
// grant_key_activation tracks the hub's ring cache interval.
func TestConduitGrantKeyNodeRefreshPinned(t *testing.T) {
	assert.Equal(t, conduitGrantKeyRefresh, config.ConduitGrantKeyNodeRefresh)
}

// TestConduitInternalHandler_SignedInAllModes: with peer_auth oidc the
// hub's internal relay API requires the request signature as well as the
// ID token. An ID token alone, a replayed nonce and a body swapped after
// signing are refused.
func TestConduitInternalHandler_SignedInAllModes(t *testing.T) {
	f := newRelayFixture(t, func(o *ConduitRelayOptions) {
		o.PeerAuth = newOIDCModeAuth(t, "hub-a", "own", nil, nil)
	})
	internal := httptest.NewServer(f.srv.ConduitInternalHandler())
	t.Cleanup(internal.Close)
	caller := newOIDCModeAuth(t, "hub-b", "own", nil, nil)

	_, _, err := f.dial(t, f.agentToken(t, f.launched), f.launched.LaunchID)
	require.NoError(t, err)
	ps, err := f.regStore.ListPrincipalSessions(context.Background(), registry.PrincipalAgent, f.launched.ID)
	require.NoError(t, err)
	require.Len(t, ps.Sessions, 1)
	base := internal.URL + relay.InternalPathPrefix
	rpcURL := base + "sessions/" + ps.Sessions[0].Session.SessionID + "/rpc"
	want, err := json.Marshal(map[string]string{"project_id": f.launched.ProjectID, "incarnation": f.launched.LaunchID})
	require.NoError(t, err)
	rpcBody, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1"})
	require.NoError(t, err)

	rt := f.srv.conduit.Load()
	require.NotNil(t, rt)
	newReq := func(t *testing.T, method, url string, body []byte) *http.Request {
		t.Helper()
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		require.NoError(t, err)
		relay.SetPeerTarget(req, rt.relay.InstanceID(), rt.relay.Generation())
		if body != nil {
			sum := sha256.Sum256(body)
			req.Header.Set(relay.HeaderBodySHA256, hex.EncodeToString(sum[:]))
			req.Header.Set(relay.HeaderWant, string(want))
		}
		return req
	}
	do := func(t *testing.T, req *http.Request) int {
		t.Helper()
		resp, err := internal.Client().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("signed with ID token", func(t *testing.T) {
		req := newReq(t, http.MethodGet, base+"self", nil)
		require.NoError(t, caller.Sign(req))
		assert.Equal(t, http.StatusOK, do(t, req))
	})
	t.Run("signed for another generation is refused", func(t *testing.T) {
		req := newReq(t, http.MethodGet, base+"self", nil)
		relay.SetPeerTarget(req, rt.relay.InstanceID(), rt.relay.Generation()-1)
		require.NoError(t, caller.Sign(req))
		assert.Equal(t, http.StatusConflict, do(t, req))
	})
	t.Run("signed for another instance is refused", func(t *testing.T) {
		req := newReq(t, http.MethodGet, base+"self", nil)
		relay.SetPeerTarget(req, "hub-c", rt.relay.Generation())
		require.NoError(t, caller.Sign(req))
		assert.Equal(t, http.StatusConflict, do(t, req))
	})
	t.Run("ID token alone is refused", func(t *testing.T) {
		req := newReq(t, http.MethodGet, base+"self", nil)
		req.Header.Set("Authorization", "Bearer own")
		assert.Equal(t, http.StatusUnauthorized, do(t, req))
	})
	t.Run("replayed nonce is refused", func(t *testing.T) {
		req := newReq(t, http.MethodGet, base+"self", nil)
		require.NoError(t, caller.Sign(req))
		replay := req.Clone(context.Background())
		require.Equal(t, http.StatusOK, do(t, req))
		assert.Equal(t, http.StatusUnauthorized, do(t, replay))
	})
	t.Run("body swapped after signing is refused", func(t *testing.T) {
		req := newReq(t, http.MethodPost, rpcURL, rpcBody)
		require.NoError(t, caller.Sign(req))
		other, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r2", Method: "exec"})
		require.NoError(t, err)
		req.Body = io.NopCloser(bytes.NewReader(other))
		req.ContentLength = int64(len(other))
		assert.Equal(t, http.StatusBadRequest, do(t, req), "body digest mismatch")
	})
	t.Run("body digest swapped too is refused", func(t *testing.T) {
		other, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r2", Method: "exec"})
		require.NoError(t, err)
		req := newReq(t, http.MethodPost, rpcURL, rpcBody)
		require.NoError(t, caller.Sign(req))
		sum := sha256.Sum256(other)
		req.Header.Set(relay.HeaderBodySHA256, hex.EncodeToString(sum[:]))
		req.Body = io.NopCloser(bytes.NewReader(other))
		req.ContentLength = int64(len(other))
		assert.Equal(t, http.StatusUnauthorized, do(t, req), "the digest is signed")
	})
}
