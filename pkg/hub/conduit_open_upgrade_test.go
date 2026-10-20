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
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Targets without stream renewal (design §3.5 Q14, v2.21.43; contracts §2
// "Target lacks stream renewal"): in external relay mode with enforce, a
// user stream open to a target session without stream_renewal.v2 is refused
// before any grant is minted, on every open path.

// upgradeRefusal is the open refusal metric entry of the rule on route.
func upgradeRefusal(route string) string {
	return route + "/" + conduitReasonStreamRenewalUnsupported + "/" + conduitCauseTargetRenewalUnsupported
}

// upgradeFixture is a hub in external relay mode, with relay-0 registered
// and the owner's user session s1 admitted through it. Tests insert the
// agent's session rows. The registry store fails every session listing
// while regFault is set, and calls listHook (with the count since the last
// armLists) before each listing.
type upgradeFixture struct {
	cf       *conduitFixture
	ops      *relayOpsFixture
	o        *openSetup
	innerReg *registry.Registry // the same rows, without the fault layer
	token    string

	regFault atomic.Bool
	lists    atomic.Int64
	listHook atomic.Pointer[func(n int64)]
}

// newUpgradeFixture builds the fixture; o.mod runs before the hub serves.
func newUpgradeFixture(t *testing.T, o *openSetup) *upgradeFixture {
	t.Helper()
	cf := newConduitFixture(t)
	o.mod(cf.srv)
	f := &upgradeFixture{cf: cf, o: o}
	f.startHub(t)
	ctx := context.Background()
	_, err := cf.store.SetAgentRunID(ctx, cf.agent.ID, "launch-1", nil)
	require.NoError(t, err)
	got, err := cf.store.GetAgent(ctx, cf.agent.ID)
	require.NoError(t, err)
	cf.agent = got
	f.token = f.ops.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1").AdmissionToken
	return f
}

// startHub starts external relay mode as startRelayOpsHub does, over a
// registry store with the fault and hook layer. relay-0's internal
// endpoint refuses connections at once, so an open that reaches it fails
// fast.
func (f *upgradeFixture) startHub(t *testing.T) {
	t.Helper()
	srv := f.cf.srv
	inner := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.innerReg = registry.New(inner, registry.Config{})
	ops := &relayOpsFixture{srv: srv, store: f.cf.store}
	ops.regStore = &registry.FaultStore{Inner: inner, Fault: func(_ context.Context, op string) error {
		if op != registry.OpListPrincipalSessions {
			return nil
		}
		if f.regFault.Load() {
			return errors.New("injected registry fault")
		}
		n := f.lists.Add(1)
		if h := f.listHook.Load(); h != nil {
			(*h)(n)
		}
		return nil
	}}
	ops.reg = registry.New(ops.regStore, registry.Config{})
	auth, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: append([]byte(nil), externalPeerSecret...), SelfID: "hub-ext"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, srv.StartConduitExternal(ctx, ConduitExternalOptions{InstanceID: "hub-ext", PeerAuth: auth, Store: ops.regStore, Registry: ops.reg,
		Clock: clock.NewFake(time.Now()), replayStore: testReplayStore(t)}))
	ops.relayGen, err = f.innerReg.RegisterRelay(context.Background(), registry.RelayInstance{
		InstanceID: "relay-0", InternalEndpoint: "http://127.0.0.1:1", PublicEndpoint: relayPublicURL,
	})
	require.NoError(t, err)
	ops.internal = httptest.NewServer(srv.ConduitInternalHandler())
	t.Cleanup(ops.internal.Close)
	ops.hub = ops.client(t, "relay-0")
	f.ops = ops
}

// addRow registers a session of the agent on relay-0 serving kinds with
// features, as the agent's newest connection, and returns its epoch.
func (f *upgradeFixture) addRow(t *testing.T, sessionID string, kinds, features []string) int64 {
	t.Helper()
	now := time.Now()
	epoch, err := f.innerReg.InsertSessionWithNextEpoch(context.Background(), registry.SessionRecord{
		SessionID: sessionID, PrincipalKind: registry.PrincipalAgent, PrincipalID: f.cf.agent.ID, ProjectID: f.cf.agent.ProjectID,
		RelayInstanceID: "relay-0", RelayGeneration: f.ops.relayGen, Transport: registry.TransportWS,
		EndpointIncarnation: f.cf.agent.RunID, ConnectedAt: now, LastSeen: now,
		Capabilities: registry.Capabilities{
			StreamKinds: kinds, EndpointIncarnation: f.cf.agent.RunID, IncarnationSource: "launch_id",
			Features: features,
		},
	})
	require.NoError(t, err)
	return epoch
}

// armLists restarts the listing count and installs hook (nil removes it).
func (f *upgradeFixture) armLists(hook func(n int64)) {
	f.lists.Store(0)
	if hook == nil {
		f.listHook.Store(nil)
		return
	}
	f.listHook.Store(&hook)
}

// postTunnel forwards a tcp tunnel request for the agent as relay-0, on
// the owner's session s1, excluding the given target sessions (a relay's
// retry).
func (f *upgradeFixture) postTunnel(t *testing.T, exclude ...string) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{Agent: f.cf.agent.ID, Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: "3000"}})
	require.NoError(t, err)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	var out relay.HubRPCResponse
	require.NoError(t, f.ops.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"),
		relay.HubRPCRequest{AdmissionToken: f.token, RPC: b, Features: []string{relay.HubFeatureTunnels}, ExcludeTargets: exclude}, &out))
	resp := &conduitv1.RpcResponse{}
	require.NoError(t, proto.Unmarshal(out.RPC, resp))
	return resp, out.Tunnel
}

// withBroker returns a copy of the agent assigned to a new attach-capable
// Runtime Broker, which no node holds a control channel for.
func (f *upgradeFixture) withBroker(t *testing.T) *store.Agent {
	t.Helper()
	id := tid("upgrade-broker")
	require.NoError(t, f.cf.store.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: id, Name: "upgrade-broker", Slug: "upgrade-broker", Status: store.BrokerStatusOnline,
		Capabilities: attachCaps(true),
	}))
	a := *f.cf.agent
	a.RuntimeBrokerID = id
	return &a
}

// requireUpgradeRequiredBody checks a 409 agent_upgrade_required body with
// reason stream_renewal_unsupported and the contracts message.
func requireUpgradeRequiredBody(t *testing.T, body []byte) {
	t.Helper()
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), string(body))
	assert.Equal(t, ErrCodeAgentUpgradeRequired, er.Error.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnsupported, er.Error.Details["reason"])
	assert.Equal(t, conduitAgentUpgradeRequiredMessage, er.Error.Message)
}

// requirePTYUpgradeRequired checks a PTY path refusal for a target without
// stream renewal: 503 runtime_attach_unsupported, reason
// stream_renewal_unsupported, no path.
func requirePTYUpgradeRequired(t *testing.T, d ptyPathDecision) {
	t.Helper()
	assert.Equal(t, ptyPathNone, d.Path)
	assert.Equal(t, http.StatusServiceUnavailable, d.Status)
	assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, d.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnsupported, d.Reason)
}

// TestConduitTargetUpgradeRequired_Condition (T1): the rule applies only in
// external relay mode, with enforce, to a user principal (a scoped token
// over a user included) and a target without stream_renewal.v2. A target
// advertising only the earlier stream_renewal lacks renewal.
func TestConduitTargetUpgradeRequired_Condition(t *testing.T) {
	renews := func(features ...string) bool {
		return conduitTargetRenews(router.Resolved{Record: registry.SessionRecord{Capabilities: registry.Capabilities{Features: features}}})
	}
	assert.True(t, renews(conduit.FeatureStreamRenewal))
	assert.False(t, renews("stream_renewal"), "the earlier capability name lacks renewal")
	assert.False(t, renews())

	inProcess := newConduitFixture(t)
	extEnforce := newUpgradeFixture(t, &openSetup{enforce: true})
	extOff := newUpgradeFixture(t, &openSetup{})
	enforceTargetDeadlines(inProcess.srv)

	agent := newAgentIdentityFromStore(inProcess.agent)
	for name, tc := range map[string]struct {
		srv  *Server
		want bool
	}{
		"external enforce": {extEnforce.cf.srv, true},
		"external off":     {extOff.cf.srv, false},
		"in-process":       {inProcess.srv, false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.srv.conduitTargetUpgradeRequiredFor(inProcess.owner, false), "user")
			assert.Equal(t, tc.want, tc.srv.conduitTargetUpgradeRequiredFor(inProcess.portOnly, false), "scoped token over a user")
			assert.False(t, tc.srv.conduitTargetUpgradeRequiredFor(inProcess.owner, true), "a target that renews")
			assert.False(t, tc.srv.conduitTargetUpgradeRequiredFor(agent, false), "an agent principal")
			assert.False(t, tc.srv.conduitTargetUpgradeRequiredFor(nil, false), "no principal")
		})
	}
	// Renewal-unsupported principals are user principals: refused too.
	for kind, ident := range renewalUnsupportedPrincipals(inProcess) {
		assert.True(t, extEnforce.cf.srv.conduitTargetUpgradeRequiredFor(ident, false), kind)
	}
}

// TestConduitOpen_UpgradeRequired_RelayTunnel (T2): a relay-held tunnel to
// a target without stream_renewal.v2 gets 409 agent_upgrade_required, also
// when the relay-held route could not renew anyway (it is the 409, not
// 503 stream_renewal_unavailable). Nothing is signed and the relay gets no
// instruction, so nothing is opened or spliced.
func TestConduitOpen_UpgradeRequired_RelayTunnel(t *testing.T) {
	for name, features := range map[string][]string{
		"no renewal feature":        nil,
		"only the earlier renewal":  {"stream_renewal"},
		"other features, no v2 one": {"something_else"},
	} {
		t.Run(name, func(t *testing.T) {
			o := &openSetup{enforce: true}
			f := newUpgradeFixture(t, o)
			f.addRow(t, "s-agent", []string{grant.StreamKindTCP}, features)

			o.keys.arm(f.cf.srv)
			resp, open := f.postTunnel(t)
			require.Equal(t, int32(http.StatusConflict), resp.GetStatus(), string(resp.GetBody()))
			requireUpgradeRequiredBody(t, resp.GetBody())
			assert.Nil(t, open, "no instruction to open the tunnel")
			assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
			assert.Equal(t, []string{upgradeRefusal(conduitRouteRelayTunnel)}, o.rec.list())
		})
	}
}

// TestConduitOpen_UpgradeRequired_RelayTunnelOff (T2): with off the same
// tunnel is served as before: the relay gets the instruction, with an
// unstamped grant.
func TestConduitOpen_UpgradeRequired_RelayTunnelOff(t *testing.T) {
	o := &openSetup{}
	f := newUpgradeFixture(t, o)
	epoch := f.addRow(t, "s-agent", []string{grant.StreamKindTCP}, nil)

	resp, open := f.postTunnel(t)
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	require.NotNil(t, open)
	pubs, err := f.cf.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	c, err := grant.Verify(context.Background(), open.Grant, keys, grant.Expectation{
		Target: grant.Target{
			Kind: grant.TargetKindAgent, ID: f.cf.agent.ID, EndpointIncarnation: open.WantIncarnation,
			SessionID: open.TargetSessionID, ConnectionEpoch: epoch,
		},
		Header:    grant.StreamHeader{Kind: open.Kind, Params: open.Params},
		ProjectID: f.cf.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.cf.clock.Now, 0), f.cf.clock.Now())
	require.NoError(t, err)
	assert.Empty(t, c.Via, "unstamped")
	assert.True(t, c.AuthzUntil.IsZero(), "unstamped")
	assert.Empty(t, o.rec.list())
}

// TestConduitOpen_UpgradeRequired_RelayTunnelRetry (T3): the relay's retry
// after a stale target is checked again on the row it resolves: here the
// agent reconnected with a sciontool without stream renewal.
func TestConduitOpen_UpgradeRequired_RelayTunnelRetry(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	f.addRow(t, "s-old", []string{grant.StreamKindTCP}, []string{conduit.FeatureStreamRenewal})
	f.addRow(t, "s-new", []string{grant.StreamKindTCP}, nil)

	o.keys.arm(f.cf.srv)
	resp, open := f.postTunnel(t, "s-old")
	require.Equal(t, int32(http.StatusConflict), resp.GetStatus(), string(resp.GetBody()))
	requireUpgradeRequiredBody(t, resp.GetBody())
	assert.Nil(t, open)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")

	// The exclusion decides the row the check reads: excluding the row the
	// retry above was refused on (only an agent's newest connection is
	// eligible) leaves no row, so the answer differs: no session, not the
	// renewal refusal, and still nothing signed.
	o.keys.arm(f.cf.srv)
	resp, open = f.postTunnel(t, "s-old", "s-new")
	require.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), string(resp.GetBody()))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
	assert.Equal(t, ErrCodeAgentSessionUnavailable, er.Error.Code)
	assert.Nil(t, open)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Equal(t, []string{upgradeRefusal(conduitRouteRelayTunnel)}, o.rec.list(), "only the first retry was refused for lack of renewal")
}

// TestConduitOpen_UpgradeRequired_Proxy (T4): the port proxy refuses with
// the tunnel's 409, never as "no session", so the handler never falls back
// to the port-forward tunnel. Nothing is signed or tracked.
func TestConduitOpen_UpgradeRequired_Proxy(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	f.addRow(t, "s-agent", []string{grant.StreamKindTCP}, nil)

	o.keys.arm(f.cf.srv)
	conn, err := f.cf.srv.openConduitPort(context.Background(), f.cf.owner, f.cf.agent, 3000)
	require.ErrorIs(t, err, errConduitAgentUpgradeRequired)
	assert.False(t, errors.Is(err, router.ErrNoSession), "not a missing session: no port-forward fallback")
	assert.Nil(t, conn)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, trackedLeaves(t, f.cf.srv), "nothing is tracked")
	assert.Equal(t, []string{upgradeRefusal(conduitRouteProxy)}, o.rec.list())

	rec := httptest.NewRecorder()
	writeConduitProxyError(rec, httptest.NewRequest(http.MethodGet, "/", nil), f.cf.agent.ID, err)
	assert.Equal(t, http.StatusConflict, rec.Code)
	requireUpgradeRequiredBody(t, rec.Body.Bytes())
}

// TestConduitOpen_UpgradeRequired_PTYPath (T5): the PTY path choice, used
// by the preflight and the WebSocket open alike.
func TestConduitOpen_UpgradeRequired_PTYPath(t *testing.T) {
	ctx := context.Background()
	pty := []string{grant.StreamKindPTY}

	t.Run("broker skipped, no agent pty session is temporary", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, f.withBroker(t))
		// No session: the temporary failure, never the renewal refusal
		// (design v2.21.46).
		assert.Equal(t, ptyPathNone, d.Path)
		assert.Equal(t, http.StatusServiceUnavailable, d.Status)
		assert.Equal(t, ErrCodeUnavailable, d.Code)
		assert.Equal(t, ptyReasonRegistryUnavailable, d.Reason)
		assert.Empty(t, o.rec.list(), "not counted as a renewal refusal")
	})
	t.Run("broker skipped, agent pty session without renewal", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		f.addRow(t, "s-agent", pty, nil)
		requirePTYUpgradeRequired(t, f.cf.srv.resolvePTYPath(ctx, f.cf.owner, f.withBroker(t)))
		assert.Equal(t, []string{upgradeRefusal(conduitRoutePTY)}, o.rec.list())
	})
	t.Run("off keeps the broker path", func(t *testing.T) {
		o := &openSetup{}
		f := newUpgradeFixture(t, o)
		d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, f.withBroker(t))
		assert.Equal(t, ptyReasonBrokerNotConnected, d.Reason, "the broker branch decided")
		assert.Empty(t, o.rec.list())
	})
	t.Run("an agent principal keeps the broker path", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		d := f.cf.srv.resolvePTYPath(ctx, newAgentIdentityFromStore(f.cf.agent), f.withBroker(t))
		assert.Equal(t, ptyReasonBrokerNotConnected, d.Reason, "the broker branch decided")
		assert.Empty(t, o.rec.list())
	})
	t.Run("no runtime broker is 422 first", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		a := *f.cf.agent
		a.RuntimeBrokerID = ""
		d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, &a)
		assert.Equal(t, http.StatusUnprocessableEntity, d.Status)
		assert.Equal(t, ErrCodeNoRuntimeBroker, d.Code)
		assert.Empty(t, o.rec.list())
	})
	t.Run("pinned placement 409 first", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		a := f.withBroker(t)
		a.PinnedRuntimeBrokerID, a.PinnedRuntimeTargetID, a.PinnedRuntimeTargetType = "rb-other", "t-1", "docker"
		d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, a)
		assert.Equal(t, http.StatusConflict, d.Status)
		assert.Empty(t, o.rec.list())
	})
	t.Run("managed runtime without a pty session keeps managed_runtime", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		a := *f.cf.agent
		a.Runtime = ManagedRuntimePrefix + "test"
		d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, &a)
		assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, d.Code)
		assert.Equal(t, ptyReasonManagedRuntime, d.Reason)
	})
	t.Run("managed runtime with a pty session without renewal", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		f.addRow(t, "s-agent", pty, nil)
		a := *f.cf.agent
		a.Runtime = ManagedRuntimePrefix + "test"
		requirePTYUpgradeRequired(t, f.cf.srv.resolvePTYPath(ctx, f.cf.owner, &a))
	})
	t.Run("a transient agent-path fault stays retryable", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newUpgradeFixture(t, o)
		a := f.withBroker(t)
		f.regFault.Store(true)
		d := f.cf.srv.resolvePTYPath(ctx, f.cf.owner, a)
		f.regFault.Store(false)
		assert.Equal(t, http.StatusServiceUnavailable, d.Status)
		assert.Equal(t, ErrCodeUnavailable, d.Code)
		assert.Equal(t, ptyReasonRegistryUnavailable, d.Reason)
		assert.Empty(t, o.rec.list())
	})
}

// TestConduitOpen_UpgradeRequired_PTYAfterUpgrade (T6): after the upgrade,
// a re-resolution onto a row without stream renewal skips it. With no row
// left the attach closes with 4504 stream_renewal_unsupported, nothing is
// signed or tracked, and the client's next preflight gets the final 503.
func TestConduitOpen_UpgradeRequired_PTYAfterUpgrade(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	f.addRow(t, "s-agent", []string{grant.StreamKindPTY}, nil)

	o.keys.arm(f.cf.srv)
	u := &agentPTYUpstream{s: f.cf.srv, identity: f.cf.owner, agent: f.cf.agent}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := u.Open(ctx, 80, 24)
	var ce *StreamClosedError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamTimeout, ce.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnsupported, ce.Reason)
	assert.Nil(t, u.st, "no stream was opened")
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, trackedLeaves(t, f.cf.srv), "nothing is tracked")
	assert.Equal(t, []string{upgradeRefusal(conduitRoutePTY)}, o.rec.list())

	requirePTYUpgradeRequired(t, f.cf.srv.resolvePTYPath(context.Background(), f.cf.owner, f.cf.agent))
}

// TestConduitOpen_UpgradeRequired_PTYAfterUpgradeContinues (T6): a row
// without stream renewal is skipped and resolution continues: when the
// agent reconnects with stream renewal in between, the open goes on with
// the new row (here it then fails to reach the relay, which this fixture
// does not run), and the attach is not refused for lack of renewal.
func TestConduitOpen_UpgradeRequired_PTYAfterUpgradeContinues(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	pty := []string{grant.StreamKindPTY}
	f.addRow(t, "s-old", pty, nil)
	var (
		mu     sync.Mutex
		passed []string
	)
	// The renewal-route decision runs only for rows past the skip.
	f.cf.srv.conduitRouteCanRenewTest = func(res router.Resolved) bool {
		mu.Lock()
		defer mu.Unlock()
		passed = append(passed, res.Record.SessionID)
		return true
	}
	var inserted atomic.Bool
	f.armLists(func(n int64) {
		if n == 2 && inserted.CompareAndSwap(false, true) {
			f.addRow(t, "s-new", pty, []string{conduit.FeatureStreamRenewal})
		}
	})
	t.Cleanup(func() { f.armLists(nil) })

	u := &agentPTYUpstream{s: f.cf.srv, identity: f.cf.owner, agent: f.cf.agent}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := u.Open(ctx, 80, 24)
	require.Error(t, err, "the relay is not running")
	require.True(t, inserted.Load(), "the agent reconnected during the open")
	var ce *StreamClosedError
	if errors.As(err, &ce) {
		assert.NotEqual(t, conduitReasonStreamRenewalUnsupported, ce.Reason)
	}
	mu.Lock()
	assert.Equal(t, []string{"s-new"}, passed, "the open went on with the row that renews")
	mu.Unlock()
	assert.Empty(t, o.rec.list(), "not refused for lack of renewal")
}

// TestMintConduitGrant_UpgradeRequiredBackstop (T7): the mint refuses a
// user stream origin to a target without stream renewal, before the
// placement guard, with nothing signed. Agent and origin-less callers and
// off are unaffected.
func TestMintConduitGrant_UpgradeRequiredBackstop(t *testing.T) {
	ctx := context.Background()
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	req := conduitGrantRequest{Identity: f.cf.owner, Agent: f.cf.agent, Stream: tcpHeader("3000"), Target: f.cf.target(), Via: grant.ViaProxy}

	o.keys.arm(f.cf.srv)
	tok, _, err := f.cf.srv.mintConduitGrant(ctx, req)
	require.ErrorIs(t, err, errConduitAgentUpgradeRequired)
	assert.Nil(t, tok)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Equal(t, []string{upgradeRefusal(conduitRouteProxy)}, o.rec.list())

	// Before the placement guard.
	pinned := *f.cf.agent
	pinned.PinnedRuntimeBrokerID, pinned.PinnedRuntimeTargetID, pinned.PinnedRuntimeTargetType = "rb-other", "t-1", "docker"
	pr := req
	pr.Agent = &pinned
	_, _, err = f.cf.srv.mintConduitGrant(ctx, pr)
	require.ErrorIs(t, err, errConduitAgentUpgradeRequired)
	var placement *PlacementConflictError
	assert.False(t, errors.As(err, &placement))

	// No origin (agent callers, library uses): not concerned.
	noVia := req
	noVia.Via = ""
	_, _, err = f.cf.srv.mintConduitGrant(ctx, noVia)
	assert.NotErrorIs(t, err, errConduitAgentUpgradeRequired)
	agentReq := req
	agentReq.Identity = newAgentIdentityFromStore(f.cf.agent)
	agentReq.Via = ""
	_, _, err = f.cf.srv.mintConduitGrant(ctx, agentReq)
	assert.NotErrorIs(t, err, errConduitAgentUpgradeRequired)

	// Renewal-unsupported principals are refused too.
	allowAllConduitActions(f.cf.srv)
	for kind, ident := range renewalUnsupportedPrincipals(f.cf) {
		ur := req
		ur.Identity = ident
		_, _, err = f.cf.srv.mintConduitGrant(ctx, ur)
		assert.ErrorIs(t, err, errConduitAgentUpgradeRequired, kind)
	}

	off := newUpgradeFixture(t, &openSetup{})
	offReq := req
	offReq.Agent, offReq.Target = off.cf.agent, off.cf.target()
	_, _, err = off.cf.srv.mintConduitGrant(ctx, offReq)
	assert.NotErrorIs(t, err, errConduitAgentUpgradeRequired, "off never refuses")
}

// infoOnlySession is a LocalSession that only reports its info (the hub
// tunnel handler reads nothing else before it refuses).
type infoOnlySession struct {
	conduit.LocalSession
	info conduit.SessionInfo
}

func (s infoOnlySession) Info() conduit.SessionInfo { return s.info }

// TestConduitOpen_UpgradeRequired_HubHeldTunnel (T8): in external relay
// mode the hub holds no user session, and a hub-held tunnel request is
// refused with 503 conduit_not_serving before any target is resolved:
// nothing is signed or opened. The hub-held open path still checks the
// rule on its own (409) if it is ever reached in that mode.
func TestConduitOpen_UpgradeRequired_HubHeldTunnel(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newUpgradeFixture(t, o)
	f.addRow(t, "s-agent", []string{grant.StreamKindTCP}, nil)
	body, err := json.Marshal(conduitTunnelRequest{Agent: f.cf.agent.ID, Kind: grant.StreamKindTCP, Params: tcpParams("3000")})
	require.NoError(t, err)

	o.keys.arm(f.cf.srv)
	ts := newConduitTunnelSession(f.cf.srv, f.cf.owner, time.Time{})
	_, err = ts.open(context.Background(), infoOnlySession{info: conduit.SessionInfo{SessionID: "hub-held"}}, body)
	var te *conduitTunnelError
	require.ErrorAs(t, err, &te)
	assert.Equal(t, tunnelConduitNotServing, te.failure)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, o.rec.list())

	_, _, _, err = ts.openTarget(context.Background(), f.cf.srv.conduitRouter(), f.cf.agent, grant.StreamKindTCP, tcpParams("3000"))
	require.ErrorAs(t, err, &te)
	assert.Equal(t, tunnelAgentUpgradeRequired, te.failure)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Equal(t, []string{upgradeRefusal(conduitRouteTunnel)}, o.rec.list())
}

// TestConduitOpen_UpgradeRequired_InProcessUnaffected (T8): in an
// in-process profile with enforce, a target without stream renewal still
// gets an unstamped grant.
func TestConduitOpen_UpgradeRequired_InProcessUnaffected(t *testing.T) {
	f := newConduitFixture(t)
	enforceTargetDeadlines(f.srv)
	rec := recordOpenRefusals(f.srv)
	_, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
		Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy,
	})
	require.NoError(t, err)
	assert.Empty(t, c.Via, "unstamped")
	assert.Empty(t, rec.list())
	assert.Equal(t, config.ConduitTargetStreamDeadlinesEnforce, f.srv.conduitTargetStreamDeadlines())
}
