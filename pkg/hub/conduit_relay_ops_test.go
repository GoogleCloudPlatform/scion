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
	"encoding/base64"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

const (
	relayPublicHost = "relay-0.example.com"
	relayPublicURL  = "https://" + relayPublicHost
)

// relayOpsFixture is a hub in external relay mode with relay-0 registered,
// and relay-0's client of the hub internal API.
type relayOpsFixture struct {
	srv      *Server
	store    store.Store
	reg      *registry.Registry
	regStore registry.Store
	relayGen int64
	internal *httptest.Server
	hub      *relay.HubClient
}

// startRelayOpsHub starts external relay mode on srv and registers relay-0.
func startRelayOpsHub(t *testing.T, srv *Server, st store.Store) *relayOpsFixture {
	t.Helper()
	srv.config.SharedSigningSecret = testRelaySessionSecret
	regStore := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	return startRelayOpsHubOn(t, srv, st, regStore, registry.New(regStore, registry.Config{}), true)
}

// startRelayOpsHubOn starts external relay mode on srv over the given
// conduit registry, which several hub replicas may share. register
// registers relay-0 (the first replica only; the others read the same
// registration through f.relayGen set by the caller).
func startRelayOpsHubOn(t *testing.T, srv *Server, st store.Store, regStore registry.Store, reg *registry.Registry, register bool) *relayOpsFixture {
	t.Helper()
	f := &relayOpsFixture{srv: srv, store: st, regStore: regStore, reg: reg}
	auth, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: append([]byte(nil), externalPeerSecret...), SelfID: "hub-ext"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, srv.StartConduitExternal(ctx, ConduitExternalOptions{InstanceID: "hub-ext", PeerAuth: auth, Store: f.regStore, Registry: f.reg, Clock: clock.NewFake(time.Now()),
		replayStore: testReplayStore(t)}))
	if register {
		f.relayGen = f.registerRelay(t, "relay-0")
	}
	f.internal = httptest.NewServer(srv.ConduitInternalHandler())
	t.Cleanup(f.internal.Close)
	f.hub = f.client(t, "relay-0")
	return f
}

func (f *relayOpsFixture) registerRelay(t *testing.T, id string) int64 {
	t.Helper()
	gen, err := f.reg.RegisterRelay(context.Background(), registry.RelayInstance{InstanceID: id, InternalEndpoint: "http://" + id + ":9811", PublicEndpoint: "https://" + id + ".example.com"})
	require.NoError(t, err)
	return gen
}

// client is the hub client of relay id.
func (f *relayOpsFixture) client(t *testing.T, id string) *relay.HubClient {
	t.Helper()
	return &relay.HubClient{BaseURL: f.internal.URL, Auth: externalRelayAuth(t, id)}
}

// admit asks the hub, as relay-0, to admit a dialer presenting h for sessionID.
func (f *relayOpsFixture) admit(t *testing.T, h http.Header, sessionID string) (relay.HubAdmitResponse, error) {
	t.Helper()
	var resp relay.HubAdmitResponse
	err := f.hub.Call(context.Background(), relay.HubAdmitPath, relay.HubAdmitRequest{
		RelayInstanceID: "relay-0", RelayGeneration: f.relayGen, SessionID: sessionID,
		Host: relayPublicHost, Headers: relay.ForwardCredentialHeaders(h),
	}, &resp)
	return resp, err
}

func (f *relayOpsFixture) mustAdmit(t *testing.T, h http.Header, sessionID string) relay.HubAdmitResponse {
	t.Helper()
	resp, err := f.admit(t, h, sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, resp.AdmissionToken)
	return resp
}

// rpc forwards an RPC on sessionID with token, as client c.
func rpcCall(t *testing.T, c *relay.HubClient, sessionID, token, path string) (*conduitv1.RpcResponse, error) {
	t.Helper()
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: path})
	require.NoError(t, err)
	var out relay.HubRPCResponse
	if err := c.Call(context.Background(), relay.HubSessionPath(sessionID, "rpc"), relay.HubRPCRequest{AdmissionToken: token, RPC: b}, &out); err != nil {
		return nil, err
	}
	resp := &conduitv1.RpcResponse{}
	require.NoError(t, proto.Unmarshal(out.RPC, resp))
	return resp, nil
}

func refusalStatus(t *testing.T, err error) int {
	t.Helper()
	var r *relay.HubRefusal
	require.ErrorAs(t, err, &r)
	return r.Status
}

func bearer(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}

// relayClock replaces the admission token clock for the test.
func relayClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	var mu sync.Mutex
	prev := conduitRelayNow
	conduitRelayNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	t.Cleanup(func() { conduitRelayNow = prev })
	return &now
}

// capturedRPCHandler records the identity each forwarded RPC runs as.
type capturedRPCHandler struct {
	mu  sync.Mutex
	ids []string
}

func captureRelayRPCs(t *testing.T) *capturedRPCHandler {
	t.Helper()
	c := &capturedRPCHandler{}
	prev := newConduitRelayRPCHandler
	newConduitRelayRPCHandler = func(s *Server, identity UserIdentity, credExpiry time.Time) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			c.mu.Lock()
			c.ids = append(c.ids, identity.ID())
			c.mu.Unlock()
			return prev(s, identity, credExpiry).HandleRPC(ctx, req)
		})
	}
	t.Cleanup(func() { newConduitRelayRPCHandler = prev })
	return c
}

func newUserRelayOps(t *testing.T) (*relayOpsFixture, *conduitFixture) {
	t.Helper()
	cf := newConduitFixture(t)
	return startRelayOpsHub(t, cf.srv, cf.store), cf
}

func ownerJWT(t *testing.T, cf *conduitFixture) string {
	t.Helper()
	tok, _, _, err := cf.srv.userTokenService.GenerateTokenPair(cf.owner.ID(), "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

// TestRelayAdmit_SameOutcomeAsInProcess (criterion 30): Admit returns the
// principal and credential expiry the in-process handshake admits each
// credential kind with, and refuses a bad credential with the same status
// and error code.
func TestRelayAdmit_SameOutcomeAsInProcess(t *testing.T) {
	f, cf := newUserRelayOps(t)

	jwt := ownerJWT(t, cf)
	resp := f.mustAdmit(t, bearer(jwt), "s-jwt")
	assert.Equal(t, registry.PrincipalUser, resp.PrincipalKind)
	assert.Equal(t, cf.owner.ID(), resp.PrincipalID)
	wantExp, err := cf.srv.userTokenService.GetTokenExpiry(jwt)
	require.NoError(t, err)
	assert.Equal(t, wantExp.Unix(), resp.CredentialExpiry.Unix(), "a user JWT's own expiry, as conduitCredentialExpiry reads it")
	assert.NotEmpty(t, resp.GrantKeys, "the grant keys for the dialer's Welcome")

	key, row, err := cf.srv.uatService.CreateToken(rs4MintContext(cf.owner.ID()), cf.owner.ID(), "relay", cf.agent.ProjectID, []string{store.UATScopeAgentPortAccess}, nil)
	require.NoError(t, err)
	resp = f.mustAdmit(t, bearer(key), "s-uat")
	assert.Equal(t, cf.owner.ID(), resp.PrincipalID)
	require.NotNil(t, row.ExpiresAt)
	assert.Equal(t, row.ExpiresAt.Unix(), resp.CredentialExpiry.Unix(), "an access token's stored expiry")

	agentTok, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	h := http.Header{}
	h.Set("X-Scion-Agent-Token", agentTok)
	resp = f.mustAdmit(t, h, "s-agent")
	assert.Equal(t, registry.PrincipalAgent, resp.PrincipalKind)
	assert.Equal(t, cf.agent.ID, resp.PrincipalID)
	assert.Equal(t, cf.agent.ProjectID, resp.ProjectID, "the project comes from the agent row")

	// A bad credential: the status and code the public handshake answers.
	for _, bad := range []http.Header{bearer("not-a-token"), {"X-Scion-Agent-Token": {"not-a-token"}}} {
		_, err := f.admit(t, bad, "s-bad")
		var refusal *relay.HubRefusal
		require.ErrorAs(t, err, &refusal)
		pub := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
		for k, v := range bad {
			req.Header[k] = v
		}
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		cf.srv.Handler().ServeHTTP(pub, req)
		assert.Equal(t, pub.Code, refusal.Status, "same status as the public handshake")
		assert.Equal(t, relayErrorCode(t, pub.Body.Bytes()), relayErrorCode(t, refusal.Body), "same error code as the public handshake")
	}
}

func relayErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Error.Code
}

// TestRelayAdmit_NotOnPublicMux (criterion 30): the hub's public router has
// no route to the hub internal API.
func TestRelayAdmit_NotOnPublicMux(t *testing.T) {
	_, cf := newUserRelayOps(t)
	for _, path := range []string{relay.HubAdmitPath, relay.HubSessionRecheckPath, relay.HubSessionPath("s1", "rpc")} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		// An authenticated caller, so the answer is the router's.
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		cf.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}

// TestRelayAdmit_BindsRelayAndHost: Admit is refused for another relay
// instance or generation (409) and for a host the relay does not serve
// (403), and needs relay-peer identity.
func TestRelayAdmit_BindsRelayAndHost(t *testing.T) {
	f, cf := newUserRelayOps(t)
	h := bearer(ownerJWT(t, cf))
	call := func(req relay.HubAdmitRequest) error {
		return f.hub.Call(context.Background(), relay.HubAdmitPath, req, nil)
	}
	base := relay.HubAdmitRequest{RelayInstanceID: "relay-0", RelayGeneration: f.relayGen, SessionID: "s1", Host: relayPublicHost, Headers: relay.ForwardCredentialHeaders(h)}

	other := base
	other.RelayInstanceID = "relay-1"
	assert.Equal(t, http.StatusConflict, refusalStatus(t, call(other)), "admit for another relay")
	stale := base
	stale.RelayGeneration = f.relayGen - 1
	assert.Equal(t, http.StatusConflict, refusalStatus(t, call(stale)), "admit for an old generation")
	host := base
	host.Host = "hub.example.com"
	assert.Equal(t, http.StatusForbidden, refusalStatus(t, call(host)), "a host the relay does not serve")

	unsigned, err := http.NewRequest(http.MethodPost, f.internal.URL+relay.HubAdmitPath, strings.NewReader("{}"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, doRaw(t, unsigned).StatusCode, "no relay-peer identity")
}

// TestRelayRPC_ReachesHandlerWithAdmittedPrincipal: a forwarded RPC runs in
// the unchanged in-process handler as the principal Admit admitted; POST
// /v1/tunnels from a relay that does not open tunnels (no
// relay.HubFeatureTunnels: an older relay) gets the typed refusal.
func TestRelayRPC_ReachesHandlerWithAdmittedPrincipal(t *testing.T) {
	f, cf := newUserRelayOps(t)
	captured := captureRelayRPCs(t)
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")

	resp, err := rpcCall(t, f.hub, "s1", adm.AdmissionToken, "/v1/unknown")
	require.NoError(t, err)
	assert.Equal(t, int32(http.StatusNotFound), resp.GetStatus(), "the in-process handler's answer to an unknown path")
	assert.Equal(t, []string{cf.owner.ID()}, captured.ids, "it ran as the admitted principal")

	resp, err = rpcCall(t, f.hub, "s1", adm.AdmissionToken, conduitTunnelsPath)
	require.NoError(t, err)
	assert.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus())
	assert.Contains(t, string(resp.GetBody()), ptyReasonConduitNotServing)
	assert.Contains(t, string(resp.GetBody()), conduitRelayTunnelsMessage)
	assert.Equal(t, []string{cf.owner.ID()}, captured.ids, "tunnel requests do not reach the in-process handler")
}

// TestRelayRPC_TokenOnly: a forwarded RPC without a token, or with a
// malformed one, is refused 401 even when it names a plausible principal;
// the hub reads no principal from the relay.
func TestRelayRPC_TokenOnly(t *testing.T) {
	f, cf := newUserRelayOps(t)
	captured := captureRelayRPCs(t)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: "/v1/unknown"})
	require.NoError(t, err)
	for name, req := range map[string]any{
		"principal fields, no token": map[string]any{"principal_kind": "user", "principal_id": cf.owner.ID(), "rpc": b},
		"malformed token":            relay.HubRPCRequest{AdmissionToken: "e30.AAAA", RPC: b},
		"empty token":                relay.HubRPCRequest{RPC: b},
	} {
		err := f.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"), req, nil)
		assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), name)
	}
	assert.Empty(t, captured.ids, "no RPC ran")
}

// tamper flips one character of the token's claims.
func tamper(token string) string {
	b := []byte(token)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// TestRelayRPC_TokenBinding (criterion 30): a token from an actual Admit is
// refused on another session (401), when presented by another relay
// instance or by a re-registered generation (409), when altered (401), and
// on RPC forward once expired (401).
func TestRelayRPC_TokenBinding(t *testing.T) {
	f, cf := newUserRelayOps(t)
	now := relayClock(t, time.Now())
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")

	_, err := rpcCall(t, f.hub, "s1", adm.AdmissionToken, "/v1/unknown")
	require.NoError(t, err, "the token works on its own session")

	_, err = rpcCall(t, f.hub, "s2", adm.AdmissionToken, "/v1/unknown")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "another session")

	f.registerRelay(t, "relay-1")
	_, err = rpcCall(t, f.client(t, "relay-1"), "s1", adm.AdmissionToken, "/v1/unknown")
	assert.Equal(t, http.StatusConflict, refusalStatus(t, err), "another relay instance")

	_, err = rpcCall(t, f.hub, "s1", tamper(adm.AdmissionToken), "/v1/unknown")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "altered token")

	*now = now.Add(conduitAdmissionMaxAge + time.Minute)
	_, err = rpcCall(t, f.hub, "s1", adm.AdmissionToken, "/v1/unknown")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "expired token on RPC forward")

	*now = time.Now()
	f.registerRelay(t, "relay-0") // relay-0 restarted: a new generation
	_, err = rpcCall(t, f.hub, "s1", adm.AdmissionToken, "/v1/unknown")
	assert.Equal(t, http.StatusConflict, refusalStatus(t, err), "a token of the relay's previous generation")
}

// TestRelayRPC_RevokedCredentialRefusedBeforeExpiry (criterion 30): an
// access token revoked after admission is refused on the next forwarded
// RPC while the admission token is still valid: the per-operation
// re-check, not the expiry, refuses it.
func TestRelayRPC_RevokedCredentialRefusedBeforeExpiry(t *testing.T) {
	f, cf := newUserRelayOps(t)
	relayClock(t, time.Now())
	key, row, err := cf.srv.uatService.CreateToken(rs4MintContext(cf.owner.ID()), cf.owner.ID(), "relay", cf.agent.ProjectID, []string{store.UATScopeAgentPortAccess}, nil)
	require.NoError(t, err)
	adm := f.mustAdmit(t, bearer(key), "s1")
	_, err = rpcCall(t, f.hub, "s1", adm.AdmissionToken, "/v1/unknown")
	require.NoError(t, err)

	require.NoError(t, cf.store.RevokeUserAccessToken(context.Background(), row.ID))
	_, err = rpcCall(t, f.hub, "s1", adm.AdmissionToken, "/v1/unknown")
	var refusal *relay.HubRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, http.StatusUnauthorized, refusal.Status)
	assert.Equal(t, "4401", refusal.Header.Get(relay.HeaderCloseCode))
}

func sessionRecheck(t *testing.T, c *relay.HubClient, items ...relay.HubSessionRecheckItem) []relay.HubSessionRecheckResult {
	t.Helper()
	var resp relay.HubSessionRecheckResponse
	require.NoError(t, c.Call(context.Background(), relay.HubSessionRecheckPath, relay.HubSessionRecheckRequest{Items: items}, &resp))
	require.Len(t, resp.Results, len(items))
	return resp.Results
}

// TestRelaySessionRecheck_ExpiredTokenStillChecked: a session re-check
// accepts an expired token that is otherwise valid, and still re-checks the
// credential: a revoked access token closes the session with 4401.
func TestRelaySessionRecheck_ExpiredTokenStillChecked(t *testing.T) {
	f, cf := newUserRelayOps(t)
	now := relayClock(t, time.Now())
	key, row, err := cf.srv.uatService.CreateToken(rs4MintContext(cf.owner.ID()), cf.owner.ID(), "relay", cf.agent.ProjectID, []string{store.UATScopeAgentPortAccess}, nil)
	require.NoError(t, err)
	adm := f.mustAdmit(t, bearer(key), "s1")
	*now = now.Add(conduitAdmissionMaxAge + time.Minute)

	res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: adm.AdmissionToken})
	assert.True(t, res[0].OK, "an expired token is accepted by re-check: %+v", res[0])
	assert.NotEmpty(t, res[0].AdmissionToken, "and re-issued")

	require.NoError(t, cf.store.RevokeUserAccessToken(context.Background(), row.ID))
	res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: adm.AdmissionToken})
	assert.True(t, res[0].Close)
	assert.Equal(t, uint32(4401), res[0].CloseCode)

	res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: tamper(adm.AdmissionToken)})
	assert.True(t, res[0].Close, "a bad token closes the session")
}

// TestRelaySessionRecheck_HealthySessionPastOneHour (criterion 30): a
// healthy session whose credential has no expiry stays open past the 1h
// token cap: the re-check re-issues its token once less than half of the
// lifetime is left, and forwarded RPCs keep working with it.
func TestRelaySessionRecheck_HealthySessionPastOneHour(t *testing.T) {
	f, _ := newUserRelayOps(t)
	start := time.Now()
	now := relayClock(t, start)
	adm := f.mustAdmit(t, bearer(testDevToken), "s1")
	assert.True(t, adm.CredentialExpiry.IsZero(), "the development credential has no expiry")
	token := adm.AdmissionToken

	*now = start.Add(20 * time.Minute)
	res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: token})
	require.True(t, res[0].OK)
	assert.Empty(t, res[0].AdmissionToken, "more than half the lifetime left: no re-issue")

	for _, at := range []time.Duration{35 * time.Minute, 70 * time.Minute, 105 * time.Minute} {
		*now = start.Add(at)
		res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: token})
		require.True(t, res[0].OK, "at %v", at)
		require.NotEmpty(t, res[0].AdmissionToken, "re-issued at %v", at)
		token = res[0].AdmissionToken
		_, err := rpcCall(t, f.hub, "s1", token, "/v1/unknown")
		require.NoError(t, err, "forwarded RPC at %v", at)
	}
}

// TestRelaySessionRecheck_AgentDeleted: a relay-held agent session whose
// agent is deleted closes with 4404, as in process.
func TestRelaySessionRecheck_AgentDeleted(t *testing.T) {
	f, cf := newUserRelayOps(t)
	agentTok, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	adm := f.mustAdmit(t, http.Header{"X-Scion-Agent-Token": {agentTok}}, "s-agent")
	res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: adm.AdmissionToken})
	require.True(t, res[0].OK)

	require.NoError(t, cf.store.DeleteAgent(context.Background(), cf.agent.ID))
	res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: adm.AdmissionToken})
	assert.True(t, res[0].Close)
	assert.Equal(t, uint32(4404), res[0].CloseCode)
}

// TestRelayRefresh_AgentCredential: an agent's in-band credential refresh on
// a relay-held session is checked by the hub: a valid credential re-issues
// the token (an expired token is accepted), a bad one is refused 401 with
// 4401.
func TestRelayRefresh_AgentCredential(t *testing.T) {
	f, cf := newUserRelayOps(t)
	now := relayClock(t, time.Now())
	agentTok, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	adm := f.mustAdmit(t, http.Header{"X-Scion-Agent-Token": {agentTok}}, "s-agent")
	*now = now.Add(conduitAdmissionMaxAge + time.Minute)

	fresh, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	var out relay.HubRefreshResponse
	require.NoError(t, f.hub.Call(context.Background(), relay.HubSessionPath("s-agent", "refresh"),
		relay.HubRefreshRequest{AdmissionToken: adm.AdmissionToken, Credential: []byte(fresh)}, &out))
	assert.NotEmpty(t, out.AdmissionToken)
	assert.NotEqual(t, adm.AdmissionToken, out.AdmissionToken)

	err = f.hub.Call(context.Background(), relay.HubSessionPath("s-agent", "refresh"),
		relay.HubRefreshRequest{AdmissionToken: out.AdmissionToken, Credential: []byte("not-a-token")}, nil)
	var refusal *relay.HubRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, http.StatusUnauthorized, refusal.Status)
	assert.Equal(t, "4401", refusal.Header.Get(relay.HeaderCloseCode))
}

// TestRelayAdmit_BrokerHMAC (criterion 30): a broker's signed handshake,
// forwarded with the host the relay received, is admitted as that broker;
// the same signed request again is refused (the broker replay store); a
// broker whose secret is rotated is closed 4401 by the session re-check.
func TestRelayAdmit_BrokerHMAC(t *testing.T) {
	srv, st := testServerWithBrokerAuth(t)
	ctx := context.Background()
	brokerID := tid("relay-broker")
	require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: brokerID, Name: "relay-broker", Slug: "relay-broker", Status: store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now()}))
	key, err := srv.brokerAuthService.GenerateAndStoreSecret(ctx, brokerID)
	require.NoError(t, err)
	secret, err := base64.StdEncoding.DecodeString(key)
	require.NoError(t, err)
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
	setConduitExperiment(t, srv, true)
	f := startRelayOpsHub(t, srv, st)

	req, err := http.NewRequest(http.MethodGet, relayPublicURL+"/api/v1/conduit", nil)
	require.NoError(t, err)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req))
	adm, err := f.admit(t, req.Header, "s-broker")
	require.NoError(t, err)
	assert.Equal(t, registry.PrincipalBroker, adm.PrincipalKind)
	assert.Equal(t, brokerID, adm.PrincipalID)

	_, err = f.admit(t, req.Header, "s-broker-2")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "a replayed broker signature")

	res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-broker", AdmissionToken: adm.AdmissionToken})
	require.True(t, res[0].OK)
	// GenerateAndStoreSecret returns an existing secret unchanged; rotate.
	_, err = srv.brokerAuthService.RotateBrokerSecret(ctx, brokerID, 0)
	require.NoError(t, err)
	res = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-broker", AdmissionToken: adm.AdmissionToken})
	assert.True(t, res[0].Close, "a rotated broker secret closes the session")
	assert.Equal(t, uint32(4401), res[0].CloseCode)
}

// TestRelayAdmissionToken_NotLogged (criterion 30): the admission token
// never appears in the hub's logs (slog and the standard logger) on any
// operation: Admit, RPC forward (accepted and refused), refresh, session
// and stream re-checks.
func TestRelayAdmissionToken_NotLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := lockedWriter{w: &buf, mu: &mu}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(w)
	t.Cleanup(func() { slog.SetDefault(prev); log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	f, cf := newUserRelayOps(t)
	user := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")
	agentTok, err := cf.srv.GenerateAgentToken(cf.agent.ID, cf.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	agent := f.mustAdmit(t, http.Header{"X-Scion-Agent-Token": {agentTok}}, "s-agent")
	_, _ = rpcCall(t, f.hub, "s1", user.AdmissionToken, "/v1/unknown")
	_, _ = rpcCall(t, f.hub, "s2", user.AdmissionToken, "/v1/unknown")
	_ = f.hub.Call(context.Background(), relay.HubSessionPath("s-agent", "refresh"), relay.HubRefreshRequest{AdmissionToken: agent.AdmissionToken, Credential: []byte("bad")}, nil)
	_ = sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: user.AdmissionToken},
		relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: agent.AdmissionToken})
	_ = f.hub.Call(context.Background(), relay.HubStreamRecheckPath, relay.HubStreamRecheckRequest{Items: []relay.HubStreamRecheckItem{{UserSessionID: "s1", AdmissionToken: user.AdmissionToken}}}, nil)

	mu.Lock()
	defer mu.Unlock()
	for _, tok := range []string{user.AdmissionToken, agent.AdmissionToken} {
		assert.NotContains(t, buf.String(), tok, "a token in the log")
		assert.NotContains(t, buf.String(), tok[:24], "a token prefix in the log")
	}
}

// lockedWriter serializes writes from concurrent loggers.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestRelayRPC_TunnelCapAppliesToRelayCount: on a relay-held session the
// hub applies the per-session tunnel cap to the count the relay reports,
// at the point the in-process path reserves a slot: after the request
// shape and before the agent is read (a capped session learns nothing
// about the agent), with no instruction to open anything.
func TestRelayRPC_TunnelCapAppliesToRelayCount(t *testing.T) {
	f, cf := newUserRelayOps(t)
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")
	body, err := json.Marshal(conduitTunnelRequest{Agent: "no-such-agent", Kind: "tcp", Params: map[string]string{"port": "3000"}})
	require.NoError(t, err)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	call := func(open int) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
		var out relay.HubRPCResponse
		require.NoError(t, f.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"),
			relay.HubRPCRequest{AdmissionToken: adm.AdmissionToken, RPC: b, OpenTunnels: open, Features: []string{relay.HubFeatureTunnels}}, &out))
		resp := &conduitv1.RpcResponse{}
		require.NoError(t, proto.Unmarshal(out.RPC, resp))
		return resp, out.Tunnel
	}
	limit := int(conduitTunnelStreamCap.Load())

	resp, tunnel := call(limit)
	assert.Equal(t, int32(http.StatusTooManyRequests), resp.GetStatus(), string(resp.GetBody()))
	assert.Contains(t, string(resp.GetBody()), ErrCodeRateLimited)
	assert.Nil(t, tunnel)

	resp, tunnel = call(limit - 1)
	assert.Equal(t, int32(http.StatusNotFound), resp.GetStatus(), "below the cap the agent is looked up: %s", resp.GetBody())
	assert.Contains(t, string(resp.GetBody()), ErrCodeAgentNotFound)
	assert.Nil(t, tunnel)
}

// TestRelayRPC_TunnelNeedsRelayFeature (mixed versions): a tunnel request
// forwarded by a relay that lists relay.HubFeatureTunnels runs the hub's
// tunnel checks; the same request from a relay without it (an older relay,
// which would ignore an instruction to open the tunnel) gets 503
// unavailable with reason conduit_not_serving and no instruction, never a
// 200 that opens nothing.
func TestRelayRPC_TunnelNeedsRelayFeature(t *testing.T) {
	f, cf := newUserRelayOps(t)
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")
	body, err := json.Marshal(conduitTunnelRequest{Agent: "no-such-agent", Kind: "tcp", Params: map[string]string{"port": "3000"}})
	require.NoError(t, err)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	call := func(features []string) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
		var out relay.HubRPCResponse
		require.NoError(t, f.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"),
			relay.HubRPCRequest{AdmissionToken: adm.AdmissionToken, RPC: b, Features: features}, &out))
		resp := &conduitv1.RpcResponse{}
		require.NoError(t, proto.Unmarshal(out.RPC, resp))
		return resp, out.Tunnel
	}

	for name, features := range map[string][]string{"no features": nil, "other features": {"something-else"}} {
		resp, tunnel := call(features)
		assert.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), name)
		assert.Contains(t, string(resp.GetBody()), ptyReasonConduitNotServing, name)
		assert.Nil(t, tunnel, name)
	}

	resp, tunnel := call([]string{relay.HubFeatureTunnels})
	assert.Equal(t, int32(http.StatusNotFound), resp.GetStatus(), "the tunnel checks ran: %s", resp.GetBody())
	assert.Contains(t, string(resp.GetBody()), ErrCodeAgentNotFound)
	assert.Nil(t, tunnel)
}

// TestRelayTunnelTargetRefusals: the answers the hub gives a relay for a
// target that refuses the tunnel's stream follow the in-process mapping:
// target not found is 404 agent_not_found, forbidden is 403 forbidden.
func TestRelayTunnelTargetRefusals(t *testing.T) {
	refusals, err := relayTunnelTargetRefusals()
	require.NoError(t, err)
	require.Len(t, refusals, 2)
	for code, want := range map[uint32]struct {
		status int32
		code   string
	}{
		relay.CloseTargetNotFound: {http.StatusNotFound, ErrCodeAgentNotFound},
		conduit.CloseForbidden:    {http.StatusForbidden, ErrCodeForbidden},
	} {
		resp := &conduitv1.RpcResponse{}
		require.NoError(t, proto.Unmarshal(refusals[code], resp), "close code %d", code)
		assert.Equal(t, want.status, resp.GetStatus(), "close code %d", code)
		assert.Contains(t, string(resp.GetBody()), want.code, "close code %d", code)
		// The same mapping as a tunnel on a hub-held session.
		var te *conduitTunnelError
		require.ErrorAs(t, conduitTunnelOpenError(&conduit.CloseError{Code: code}), &te)
		assert.Equal(t, want.status, int32(conduitTunnelFailures[te.failure].status), "in-process mapping for %d", code)
	}
}
