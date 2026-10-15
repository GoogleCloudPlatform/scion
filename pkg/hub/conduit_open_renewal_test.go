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
	"go/ast"
	"go/parser"
	"go/token"
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

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// openRefusedRecorder records stream_open_refused metric calls.
type openRefusedRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *openRefusedRecorder) RecordConduitOpenRefused(route, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, route+"/"+reason)
}

func (r *openRefusedRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// recordOpenRefusals installs a recorder for the open refusal metric.
func recordOpenRefusals(srv *Server) *openRefusedRecorder {
	rec := &openRefusedRecorder{}
	var m conduitOpenRefusedMetrics = rec
	srv.conduitOpenRefusedMetrics.Store(&m)
	return rec
}

// enforceTargetDeadlines sets target_stream_deadlines to enforce on srv.
func enforceTargetDeadlines(srv *Server) {
	srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesEnforce
}

// countingGrantKeyStore counts reads of the grant key ring, so a test can
// show that no grant was signed (the first signer call reads the ring).
type countingGrantKeyStore struct {
	inner conduitGrantKeyStore
	reads atomic.Int64
}

func (c *countingGrantKeyStore) Load(ctx context.Context) (*grant.KeyRing, int, error) {
	c.reads.Add(1)
	return c.inner.Load(ctx)
}

func (c *countingGrantKeyStore) Create(ctx context.Context, ring *grant.KeyRing) error {
	c.reads.Add(1)
	return c.inner.Create(ctx, ring)
}

func (c *countingGrantKeyStore) CompareAndSwap(ctx context.Context, ring *grant.KeyRing, rev int) (bool, error) {
	return c.inner.CompareAndSwap(ctx, ring, rev)
}

// countSigning replaces f's grant key set with a fresh one over a counting
// store; its reads stay zero until a grant is signed.
func countSigning(f *conduitFixture) *countingGrantKeyStore {
	c := &countingGrantKeyStore{inner: &memoryConduitGrantKeyStore{}}
	f.srv.conduitGrants = newConduitGrantKeys(c, f.clock.Now)
	return c
}

// TestMintConduitGrant_StampsRenewalTargets (design §3.5 Open grant): a
// user stream to a stream_renewal.v2 target carries the route's via, its
// authz_until (mint time plus the user stream interval) and
// authz_interval, for every origin and kind the origin table lists. An
// unscoped user gets no scope; a scoped token's grant carries the scope
// the renewal re-check rebuilds the identity from.
func TestMintConduitGrant_StampsRenewalTargets(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	iv := f.srv.conduitUserStreamAuthzInterval()
	for _, tc := range []struct {
		via string
		h   grant.StreamHeader
	}{
		{grant.ViaTunnel, tcpHeader("3000")},
		{grant.ViaTunnel, grant.StreamHeader{Kind: grant.StreamKindSSH}},
		{grant.ViaTunnel, ptyHeader("80", "24")},
		{grant.ViaProxy, tcpHeader("3000")},
		{grant.ViaPTY, ptyHeader("80", "24")},
	} {
		t.Run(tc.via+"/"+tc.h.Kind, func(t *testing.T) {
			tok, c, err := f.srv.mintConduitGrant(ctx, conduitGrantRequest{
				Identity: f.owner, Agent: f.agent, Stream: tc.h, Target: f.target(), Via: tc.via, TargetRenews: true,
			})
			require.NoError(t, err)
			require.NotEmpty(t, tok)
			assert.Equal(t, tc.via, c.Via)
			assert.Nil(t, c.Scope, "an unscoped user gets no scope")
			assert.Equal(t, iv, c.AuthzInterval)
			assert.True(t, c.AuthzUntil.Equal(c.NotBefore.Add(iv)), "authz_until is mint time plus the interval")
			assert.NoError(t, f.targetVerify(t, tok, tc.h))
		})
	}

	t.Run("scoped token", func(t *testing.T) {
		_, c, err := f.srv.mintConduitGrant(ctx, conduitGrantRequest{
			Identity: f.portOnly, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true,
		})
		require.NoError(t, err)
		require.NotNil(t, c.Scope)
		assert.True(t, grant.ScopeEqual(mustScope(t, f.portOnly), c.Scope), "the scope is the token's credential context")
	})
}

// TestMintConduitGrant_UnchangedWithoutRenewal: a target without
// stream_renewal.v2, and an agent-originated stream to one with it, get
// the payload without via, scope or authz_until.
func TestMintConduitGrant_UnchangedWithoutRenewal(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	self := newTestAgentIdentity(f.agent.ID, f.agent.ProjectID, []AgentTokenScope{ScopeProjectRead})
	for name, req := range map[string]conduitGrantRequest{
		"user, target without renewal":        {Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy},
		"scoped user, target without renewal": {Identity: f.portOnly, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaTunnel},
		"agent caller, renewal target":        {Identity: self, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), TargetRenews: true},
		"no origin, target without renewal":   {Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target()},
	} {
		t.Run(name, func(t *testing.T) {
			_, c, err := f.srv.mintConduitGrant(ctx, req)
			require.NoError(t, err)
			assert.Empty(t, c.Via)
			assert.Nil(t, c.Scope)
			assert.True(t, c.AuthzUntil.IsZero())
			assert.Zero(t, c.AuthzInterval)
		})
	}
}

// TestMintConduitGrant_RenewalTargetNeedsOrigin: a user stream to a
// renewal target without an origin is refused as invalid, so a stamped
// grant always carries a via from the origin table; an origin that does
// not carry the kind is invalid too.
func TestMintConduitGrant_RenewalTargetNeedsOrigin(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	keys := countSigning(f)
	_, _, err := f.srv.mintConduitGrant(ctx, conduitGrantRequest{
		Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), TargetRenews: true,
	})
	assert.ErrorIs(t, err, errConduitInvalid)
	_, _, err = f.srv.mintConduitGrant(ctx, conduitGrantRequest{
		Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaPTY,
	})
	assert.ErrorIs(t, err, errConduitInvalid)
	assert.Zero(t, keys.reads.Load(), "nothing was signed")
}

// TestMintConduitGrant_ScopeUndeterminedNotMinted (design §3.5 Scope
// claim): when the stream's scope cannot be determined, nothing is minted
// and the open fails as an internal error, never a grant without scope.
func TestMintConduitGrant_ScopeUndeterminedNotMinted(t *testing.T) {
	f := newConduitFixture(t)
	keys := countSigning(f)
	f.srv.conduitScopeOfTest = func(Identity) (*grant.Scope, error) { return nil, errConduitScope }
	tok, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
		Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true,
	})
	assert.ErrorIs(t, err, errConduitGrantInternal)
	assert.ErrorIs(t, err, errConduitScope)
	assert.Nil(t, tok)
	assert.Nil(t, c)
	assert.Zero(t, keys.reads.Load(), "nothing was signed")
}

// TestMintConduitGrant_OversizeNotMinted: a grant over the grant size limit
// is not minted; the open fails as an internal error.
func TestMintConduitGrant_OversizeNotMinted(t *testing.T) {
	f := newConduitFixture(t)
	target := f.target()
	target.EndpointIncarnation = strings.Repeat("i", 16<<10)
	tok, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
		Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: target, Via: grant.ViaProxy, TargetRenews: true,
	})
	assert.ErrorIs(t, err, errConduitGrantInternal)
	assert.ErrorIs(t, err, grant.ErrTooLarge)
	assert.Nil(t, tok)
	assert.Nil(t, c)
}

// TestMintConduitGrant_RenewalPathBackstop (contracts, Renewal path
// unavailable): with enforce, a user stream to a renewal target on a route
// that cannot renew is refused before the placement guard and before any
// grant is signed, and the refusal is counted; with off, or a route that
// can renew, the grant is minted.
func TestMintConduitGrant_RenewalPathBackstop(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	rec := recordOpenRefusals(f.srv)
	keys := countSigning(f)
	req := conduitGrantRequest{Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true}

	enforceTargetDeadlines(f.srv)
	tok, _, err := f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitStreamRenewalUnavailable)
	assert.Nil(t, tok)
	assert.Zero(t, keys.reads.Load(), "nothing was signed")
	assert.Equal(t, []string{conduitRouteProxy + "/" + conduitReasonStreamRenewalUnavailable}, rec.list())

	// Before the placement guard: a pinned agent whose saved placement is
	// inconsistent still gets the renewal refusal, not its 409.
	pinned := *f.agent
	pinned.PinnedRuntimeBrokerID, pinned.PinnedRuntimeTargetID, pinned.PinnedRuntimeTargetType = "rb-other", "t-1", "docker"
	pr := req
	pr.Agent = &pinned
	_, _, err = f.srv.mintConduitGrant(ctx, pr)
	assert.ErrorIs(t, err, errConduitStreamRenewalUnavailable)
	var placement *PlacementConflictError
	assert.False(t, errors.As(err, &placement))
	nrp := pr
	nrp.TargetRenews = false
	_, _, err = f.srv.mintConduitGrant(ctx, nrp)
	require.ErrorAs(t, err, &placement, "without the renewal refusal the same agent gets its placement 409")

	// A route that can renew, a target without renewal, an agent caller:
	// no refusal.
	ok := req
	ok.CanRenew = true
	_, _, err = f.srv.mintConduitGrant(ctx, ok)
	assert.NoError(t, err)
	nr := req
	nr.TargetRenews = false
	_, _, err = f.srv.mintConduitGrant(ctx, nr)
	assert.NoError(t, err)

	f.srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesOff
	_, c, err := f.srv.mintConduitGrant(ctx, req)
	require.NoError(t, err, "off never refuses")
	assert.Equal(t, grant.ViaProxy, c.Via, "off still stamps the claims")
	assert.Len(t, rec.list(), 2)
}

// TestConduitScopeOfTestUnsetInProduction: the scope test hook is nil on a
// server built by the production constructor, and no non-test file
// assigns it.
func TestConduitScopeOfTestUnsetInProduction(t *testing.T) {
	srv, _ := testServer(t)
	assert.Nil(t, srv.conduitScopeOfTest)

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "conduitScopeOfTest" {
					t.Errorf("%s assigns conduitScopeOfTest", name)
				}
			}
			return true
		})
		if strings.Contains(string(src), "conduitScopeOfTest:") {
			t.Errorf("%s sets conduitScopeOfTest in a composite literal", name)
		}
	}
}

// TestConduitGrantRequestCallersSetOrigin pins every open-mint call site in
// production code: each conduitGrantRequest literal sets Via to one of the
// origin constants (grant.ViaTunnel, grant.ViaProxy, grant.ViaPTY), never
// to a value read from the request, and no caller leaves it empty and
// falls back to the unstamped path. Every production open mint is a
// user-capable stream (tunnel, relay-held tunnel, port proxy, PTY), so a
// new caller must be added here deliberately.
func TestConduitGrantRequestCallersSetOrigin(t *testing.T) {
	origins := map[string]bool{"ViaTunnel": true, "ViaProxy": true, "ViaPTY": true}
	want := map[string]int{"conduit_tunnel.go": 1, "conduit_tunnel_relay.go": 1, "conduit_proxy.go": 1, "pty_conduit.go": 1}
	got := map[string]int{}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "conduitGrantRequest" {
				return true
			}
			got[name]++
			via := ""
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Via" {
					if sel, ok := kv.Value.(*ast.SelectorExpr); ok {
						if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "grant" && origins[sel.Sel.Name] {
							via = sel.Sel.Name
						}
					}
				}
			}
			assert.NotEmpty(t, via, "%s: a conduitGrantRequest must set Via to a grant.Via* constant", name)
			return true
		})
	}
	assert.Equal(t, want, got, "the production open-mint call sites")
}

// TestConduitTunnel_RequestCannotChooseOrigin (H1): the tunnel request
// body has no way to name an origin: a via field, at the top level or as a
// stream param, is refused as invalid before anything is minted.
func TestConduitTunnel_RequestCannotChooseOrigin(t *testing.T) {
	for name, body := range map[string]string{
		"top-level via": `{"agent":"a","kind":"tcp","params":{"port":"3000"},"via":"proxy"}`,
		"via param":     `{"agent":"a","kind":"tcp","params":{"port":"3000","via":"proxy"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, params, err := parseConduitTunnelRequest([]byte(body))
			if err == nil {
				// A param survives parsing only to be refused at the mint.
				f := newConduitFixture(t)
				_, _, err = f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
					Identity: f.owner, Agent: f.agent, Stream: grant.StreamHeader{Kind: grant.StreamKindTCP, Params: params},
					Target: f.target(), Via: grant.ViaTunnel, TargetRenews: true,
				})
				assert.ErrorIs(t, err, errConduitInvalid)
				return
			}
			var te *conduitTunnelError
			require.ErrorAs(t, err, &te)
			assert.Equal(t, tunnelInvalidRequest, te.failure)
		})
	}
}

// requireRenewalUnavailableBody checks a 503 unavailable body with reason
// stream_renewal_unavailable.
func requireRenewalUnavailableBody(t *testing.T, er ErrorResponse) {
	t.Helper()
	assert.Equal(t, ErrCodeUnavailable, er.Error.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, er.Error.Details["reason"])
}

// TestConduitOpen_RenewalUnavailable_Tunnel (C5, tunnel): with enforce, a
// tunnel to a stream_renewal.v2 agent is refused with 503 unavailable,
// reason stream_renewal_unavailable; nothing reaches the agent and the
// refusal is counted. With off, the same tunnel opens.
func TestConduitOpen_RenewalUnavailable_Tunnel(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	rec := recordOpenRefusals(tf.srv)
	owner := tf.ownerClient(t)

	enforceTargetDeadlines(tf.srv)
	r := owner.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	require.Equal(t, http.StatusServiceUnavailable, r.status, r.body)
	assert.Equal(t, ErrCodeUnavailable, r.err.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, r.err.Details["reason"])
	assert.Empty(t, tf.dials, "nothing reached the agent")
	assert.Equal(t, []string{conduitRouteTunnel + "/" + conduitReasonStreamRenewalUnavailable}, rec.list())

	tf.srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesOff
	st := owner.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "off opens")
}

// TestConduitOpen_ScopeError_Tunnel (C5, tunnel): a scope that cannot be
// determined fails the tunnel with 500 internal_error, nothing reaching
// the agent.
func TestConduitOpen_ScopeError_Tunnel(t *testing.T) {
	tf := newTunnelFixture(t)
	tf.startAgent(t, tf.public.URL, tf.launched)
	tf.srv.conduitScopeOfTest = func(Identity) (*grant.Scope, error) { return nil, errConduitScope }
	r := tf.ownerClient(t).post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	require.Equal(t, http.StatusInternalServerError, r.status, r.body)
	assert.Equal(t, ErrCodeInternalError, r.err.Code)
	assert.Empty(t, tf.dials, "nothing reached the agent")
}

// TestConduitOpen_RenewalUnavailable_Proxy (C5, port proxy): with
// enforce, the port proxy to a stream_renewal.v2 agent answers 503
// unavailable, reason stream_renewal_unavailable, and the app is never
// reached.
func TestConduitOpen_RenewalUnavailable_Proxy(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	rec := recordOpenRefusals(f.srv)
	enforceTargetDeadlines(f.srv)

	resp := f.get(t, "/hello", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	requireRenewalUnavailableBody(t, er)
	assert.Empty(t, f.app.got, "the app was not reached")
	assert.Equal(t, []string{conduitRouteProxy + "/" + conduitReasonStreamRenewalUnavailable}, rec.list())
}

// TestConduitOpen_ScopeError_Proxy (C5, port proxy): a scope that cannot
// be determined answers 500 internal_error and the app is never reached.
func TestConduitOpen_ScopeError_Proxy(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	f.srv.conduitScopeOfTest = func(Identity) (*grant.Scope, error) { return nil, errConduitScope }
	resp := f.get(t, "/hello", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	assert.Equal(t, ErrCodeInternalError, er.Error.Code)
	assert.Empty(t, f.app.got, "the app was not reached")
}

// TestConduitOpen_RenewalUnavailable_PTY (C5 and Q6, PTY preflight and
// WebSocket open): with enforce, the agent path to a stream_renewal.v2
// agent is refused before any upgrade with 503 unavailable, reason
// stream_renewal_unavailable, by the preflight and the WebSocket open
// alike; nothing is spawned.
func TestConduitOpen_RenewalUnavailable_PTY(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	rec := recordOpenRefusals(f.srv)
	enforceTargetDeadlines(f.srv)

	status, path, reason := f.preflight(t)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, string(ptyPathNone), path)
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, reason)

	_, resp, err := f.dialPTY(t, "")
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	requireRenewalUnavailableBody(t, er)
	assert.Empty(t, f.spawned)
	assert.Equal(t, []string{
		conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable,
		conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable,
	}, rec.list(), "preflight and open each decided it")

	f.srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesOff
	status, path, _ = f.preflight(t)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, string(ptyPathAgent), path)
}

// TestConduitOpen_ScopeError_PTY (C5, web PTY and its preflight): the
// preflight mints nothing, so it still names the agent path; the
// WebSocket open then mints, and a scope that cannot be determined closes
// it with 1011 internal_error, nothing spawned.
func TestConduitOpen_ScopeError_PTY(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	f.srv.conduitScopeOfTest = func(Identity) (*grant.Scope, error) { return nil, errConduitScope }

	status, path, _ := f.preflight(t)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, string(ptyPathAgent), path)

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	_, _, err = c.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, wsprotocol.ClosePTYInternalError, ce.Code)
	assert.Equal(t, wsprotocol.CloseReasonInternalError, ce.Text)
	assert.Empty(t, f.spawned)
}

// TestConduitOpen_RenewalUnavailable_PTYBackstop (arch ruling, PTY
// backstop): an agent-path open that reaches the mint on a route that
// cannot renew (as after a re-resolution following the pre-upgrade check)
// is closed with 4504 stream_renewal_unavailable; no grant is minted, no
// stream is opened and the refusal is counted.
func TestConduitOpen_RenewalUnavailable_PTYBackstop(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	rec := recordOpenRefusals(f.srv)
	enforceTargetDeadlines(f.srv)

	// The upstream as runAgentPTY builds it, opened directly: the
	// pre-upgrade decision (resolvePTYPath) is not consulted here.
	u := &agentPTYUpstream{s: f.srv, identity: f.owner, agent: f.launched}
	err := u.Open(context.Background(), 80, 24)
	var sc *StreamClosedError
	require.ErrorAs(t, err, &sc)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamTimeout, sc.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, sc.Reason)
	assert.Nil(t, u.st, "no stream was opened")
	assert.Empty(t, f.spawned)
	assert.Equal(t, []string{conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable}, rec.list())
}

// relayTunnelFixture is a hub in external relay mode with an agent whose
// session, advertising stream_renewal.v2 and tcp, is held by relay-0, and
// an admitted user session s1 for the owner.
type relayTunnelFixture struct {
	ops   *relayOpsFixture
	cf    *conduitFixture
	token string
	epoch int64
}

func newRelayTunnelFixture(t *testing.T) *relayTunnelFixture {
	t.Helper()
	ops, cf := newUserRelayOps(t)
	ctx := context.Background()
	_, err := cf.store.SetAgentRunID(ctx, cf.agent.ID, "launch-1", nil)
	require.NoError(t, err)
	got, err := cf.store.GetAgent(ctx, cf.agent.ID)
	require.NoError(t, err)
	cf.agent = got
	now := time.Now()
	epoch, err := ops.reg.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: "s-agent", PrincipalKind: registry.PrincipalAgent, PrincipalID: cf.agent.ID, ProjectID: cf.agent.ProjectID,
		RelayInstanceID: "relay-0", RelayGeneration: ops.relayGen, Transport: registry.TransportWS,
		EndpointIncarnation: cf.agent.RunID, ConnectedAt: now, LastSeen: now,
		Capabilities: registry.Capabilities{
			StreamKinds: []string{grant.StreamKindTCP}, EndpointIncarnation: cf.agent.RunID, IncarnationSource: "launch_id",
			Features: []string{conduit.FeatureStreamRenewal},
		},
	})
	require.NoError(t, err)
	adm := ops.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")
	return &relayTunnelFixture{ops: ops, cf: cf, token: adm.AdmissionToken, epoch: epoch}
}

// post forwards a tcp tunnel request for the agent, as relay-0.
func (f *relayTunnelFixture) post(t *testing.T) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{Agent: f.cf.agent.ID, Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: "3000"}})
	require.NoError(t, err)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	var out relay.HubRPCResponse
	require.NoError(t, f.ops.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"),
		relay.HubRPCRequest{AdmissionToken: f.token, RPC: b, Features: []string{relay.HubFeatureTunnels}}, &out))
	resp := &conduitv1.RpcResponse{}
	require.NoError(t, proto.Unmarshal(out.RPC, resp))
	return resp, out.Tunnel
}

// TestConduitOpen_RenewalUnavailable_RelayTunnel (C5, relay-held tunnel):
// a relay-held stream cannot be renewed until the relay re-check is
// served, so with enforce the tunnel is refused with 503 unavailable,
// reason stream_renewal_unavailable, and the relay gets no instruction to
// open anything. With off, the relay gets the instruction, with a grant
// that carries via.
func TestConduitOpen_RenewalUnavailable_RelayTunnel(t *testing.T) {
	f := newRelayTunnelFixture(t)
	rec := recordOpenRefusals(f.cf.srv)
	enforceTargetDeadlines(f.cf.srv)

	resp, open := f.post(t)
	require.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), string(resp.GetBody()))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
	requireRenewalUnavailableBody(t, er)
	assert.Nil(t, open, "no instruction to open the tunnel")
	assert.Equal(t, []string{conduitRouteRelayTunnel + "/" + conduitReasonStreamRenewalUnavailable}, rec.list())

	f.cf.srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesOff
	resp, open = f.post(t)
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	require.NotNil(t, open)
	pubs, err := f.cf.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	c, err := grant.Verify(context.Background(), open.Grant, keys, grant.Expectation{
		Target: grant.Target{
			Kind: grant.TargetKindAgent, ID: f.cf.agent.ID, EndpointIncarnation: open.WantIncarnation,
			SessionID: open.TargetSessionID, ConnectionEpoch: f.epoch,
		},
		Header:    grant.StreamHeader{Kind: open.Kind, Params: open.Params},
		ProjectID: f.cf.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.cf.clock.Now, 0), f.cf.clock.Now())
	require.NoError(t, err)
	assert.Equal(t, grant.ViaTunnel, c.Via)
	assert.False(t, c.AuthzUntil.IsZero())
}

// TestConduitOpen_ScopeError_RelayTunnel (C5, relay-held tunnel): a scope
// that cannot be determined fails the tunnel with 500 internal_error and
// no instruction.
func TestConduitOpen_ScopeError_RelayTunnel(t *testing.T) {
	f := newRelayTunnelFixture(t)
	f.cf.srv.conduitScopeOfTest = func(Identity) (*grant.Scope, error) { return nil, errConduitScope }
	resp, open := f.post(t)
	require.Equal(t, int32(http.StatusInternalServerError), resp.GetStatus(), string(resp.GetBody()))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
	assert.Equal(t, ErrCodeInternalError, er.Error.Code)
	assert.Nil(t, open)
}

// TestConduitGrantKeys_PublishesTargetStreamDeadlines: the hub-direct key
// response carries the deadline mode, "off" by default and "enforce" when
// set.
func TestConduitGrantKeys_PublishesTargetStreamDeadlines(t *testing.T) {
	f := newConduitFixture(t)
	get := func() string {
		rec := httptest.NewRecorder()
		f.srv.handleConduitGrantKeys(rec, httptest.NewRequest(http.MethodGet, "/api/v1/conduit/grant-keys", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		body, err := io.ReadAll(rec.Body)
		require.NoError(t, err)
		var out struct {
			TargetStreamDeadlines *string `json:"target_stream_deadlines"`
		}
		require.NoError(t, json.Unmarshal(body, &out))
		require.NotNil(t, out.TargetStreamDeadlines, "the mode is always present")
		return *out.TargetStreamDeadlines
	}
	assert.Equal(t, config.ConduitTargetStreamDeadlinesOff, get())
	enforceTargetDeadlines(f.srv)
	assert.Equal(t, config.ConduitTargetStreamDeadlinesEnforce, get())
}
