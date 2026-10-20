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
	"fmt"
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
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// openRefusedRecorder records stream_open_refused metric calls.
type openRefusedRecorder struct {
	mu        sync.Mutex
	seen      []string
	unstamped []string
}

func (r *openRefusedRecorder) RecordConduitOpenRefused(route, reason, cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, route+"/"+reason+"/"+cause)
}

func (r *openRefusedRecorder) RecordConduitOpenUnstamped(route, principalKind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unstamped = append(r.unstamped, route+"/"+principalKind)
}

func (r *openRefusedRecorder) unstampedList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.unstamped...)
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
// Call it before srv serves (openSetup.mod), never while it serves.
func enforceTargetDeadlines(srv *Server) {
	srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesEnforce
}

// countingGrantKeyStore counts reads of the grant key ring, so a test can
// show that no grant was signed: with the cached ring dropped, signing
// reads the store.
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

// countSigning wraps srv's grant key ring store (the same ring, so targets
// keep verifying) in a read counter and drops the cached ring. Install it
// before srv serves.
func countSigning(srv *Server) *countingGrantKeyStore {
	keys := srv.conduitGrantKeySet()
	keys.mu.Lock()
	c := &countingGrantKeyStore{inner: keys.store}
	keys.store = c
	keys.ring = nil
	keys.gen++
	keys.mu.Unlock()
	return c
}

// arm drops the cached ring and resets the counter right before the
// request under test, so any grant it signs reads the store.
func (c *countingGrantKeyStore) arm(srv *Server) {
	srv.conduitGrantKeySet().invalidate()
	c.reads.Store(0)
}

// openSetup configures a server for a route-level open test. mod runs
// before the server serves, so nothing it sets races a serving goroutine.
type openSetup struct {
	enforce  bool
	scopeErr bool
	allowAll bool
	// noRenewalRoute makes every route one this node cannot renew on
	// (conduitRouteCanRenewTest), as a bridged leaf is until 6.6b-3.
	noRenewalRoute bool
	rec            *openRefusedRecorder
	keys           *countingGrantKeyStore
}

func (o *openSetup) mod(srv *Server) {
	if o.enforce {
		enforceTargetDeadlines(srv)
	}
	if o.scopeErr {
		srv.conduitScopeOfTest = func(Identity) (*grant.Scope, error) { return nil, errConduitScope }
	}
	if o.allowAll {
		allowAllConduitActions(srv)
	}
	if o.noRenewalRoute {
		srv.conduitRouteCanRenewTest = func(router.Resolved) bool { return false }
	}
	o.rec = recordOpenRefusals(srv)
	o.keys = countSigning(srv)
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
	keys := countSigning(f.srv)
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
	keys := countSigning(f.srv)
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
	keys := countSigning(f.srv)
	req := conduitGrantRequest{Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true}

	enforceTargetDeadlines(f.srv)
	tok, _, err := f.srv.mintConduitGrant(ctx, req)
	assert.ErrorIs(t, err, errConduitStreamRenewalUnavailable)
	assert.Nil(t, tok)
	assert.Zero(t, keys.reads.Load(), "nothing was signed")
	assert.Equal(t, []string{conduitRouteProxy + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, rec.list())

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

// TestConduitScopeOfTestUnsetInProduction: the test-only hooks the open
// mint consults (conduitScopeOfTest, conduitAuthorizeActionTestHook) are nil
// on a server built by the production constructor, and no non-test file
// assigns them.
func TestConduitScopeOfTestUnsetInProduction(t *testing.T) {
	srv, _ := testServer(t)
	assert.Nil(t, srv.conduitScopeOfTest)
	assert.Nil(t, srv.conduitAuthorizeActionTestHook)
	assert.Nil(t, srv.conduitRouteCanRenewTest)

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
				if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "conduitScopeOfTest" || sel.Sel.Name == "conduitAuthorizeActionTestHook" || sel.Sel.Name == "conduitRouteCanRenewTest") {
					t.Errorf("%s assigns %s", name, sel.Sel.Name)
				}
			}
			return true
		})
		for _, field := range []string{"conduitScopeOfTest:", "conduitAuthorizeActionTestHook:", "conduitRouteCanRenewTest:"} {
			if strings.Contains(string(src), field) {
				t.Errorf("%s sets %s in a composite literal", name, strings.TrimSuffix(field, ":"))
			}
		}
	}
}

// TestMintConduitGrant_OversizeScopeNotMinted (design §3.5 Scope claim,
// Size): a real scoped token whose ceiling's permission ids push the grant
// past the grant size limit is authorized, but its grant is not minted;
// the open fails as an internal error, never a grant without its scope.
func TestMintConduitGrant_OversizeScopeNotMinted(t *testing.T) {
	f := newConduitFixture(t)
	ids := []string{"agent.port_access"}
	for i := 0; i < 1000; i++ {
		ids = append(ids, fmt.Sprintf("zz.oversize.permission.%04d", i))
	}
	scoped := NewScopedUserIdentityWithBoundary(f.owner,
		TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.agent.ProjectID}, []string{store.UATScopeAgentPortAccess}, "",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: ids})
	keysBefore := mustScope(t, scoped)
	require.Len(t, keysBefore.Ceiling.PermissionIDs, len(ids))

	tok, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
		Identity: scoped, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true,
	})
	assert.ErrorIs(t, err, errConduitGrantInternal)
	assert.ErrorIs(t, err, grant.ErrTooLarge)
	assert.NotErrorIs(t, err, errConduitForbidden, "the token is authorized; only the grant is too large")
	assert.Nil(t, tok)
	assert.Nil(t, c)
}

// TestConduitGrantRequestCallersSetOrigin pins every open-mint call site in
// production code (H1, the lead's call-site guard). It finds each
// mintConduitGrant call in the package's non-test files and requires
// exactly the four known ones. Each must pass an inline
// conduitGrantRequest literal (or its address) whose Via is the grant
// origin constant of its route: the hub-held and relay-held tunnels
// grant.ViaTunnel, the port proxy grant.ViaProxy and the PTY agent path
// grant.ViaPTY. No value read from a request can reach Via, and a new
// caller fails here until it is added deliberately. It also fails on any
// later assignment to the Via field of a conduitGrantRequest variable.
func TestConduitGrantRequestCallersSetOrigin(t *testing.T) {
	want := map[string][]string{
		"conduit_tunnel.go":       {"ViaTunnel"},
		"conduit_tunnel_relay.go": {"ViaTunnel"},
		"conduit_proxy.go":        {"ViaProxy"},
		"pty_conduit.go":          {"ViaPTY"},
	}
	got := map[string][]string{}
	calls := 0
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		// Variables and parameters of type conduitGrantRequest.
		reqVars := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Field:
				if isGrantRequestType(x.Type) {
					for _, id := range x.Names {
						reqVars[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				if isGrantRequestType(x.Type) {
					for _, id := range x.Names {
						reqVars[id.Name] = true
					}
				}
				for i, v := range x.Values {
					if grantRequestLiteral(v) != nil && i < len(x.Names) {
						reqVars[x.Names[i].Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, v := range x.Rhs {
					if grantRequestLiteral(v) != nil && i < len(x.Lhs) {
						if id, ok := x.Lhs[i].(*ast.Ident); ok {
							reqVars[id.Name] = true
						}
					}
				}
			}
			return true
		})
		// Every mintConduitGrant selector must be the function of a call:
		// a method value (m := s.mintConduitGrant, or passing it as a func)
		// would hide a call site from this test.
		called := map[*ast.SelectorExpr]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					called[sel] = true
				}
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if x.Sel.Name == "mintConduitGrant" && !called[x] {
					t.Errorf("%s: mintConduitGrant is used as a value, not called", name)
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "mintConduitGrant" {
					return true
				}
				calls++
				require.Len(t, x.Args, 2, "%s: mintConduitGrant(ctx, req)", name)
				lit := grantRequestLiteral(x.Args[1])
				if lit == nil {
					t.Errorf("%s: mintConduitGrant's request must be an inline conduitGrantRequest literal", name)
					return true
				}
				got[name] = append(got[name], grantRequestVia(lit))
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Via" {
						continue
					}
					if id, ok := sel.X.(*ast.Ident); ok && reqVars[id.Name] {
						t.Errorf("%s: assigns %s.Via of a conduitGrantRequest after it is built", name, id.Name)
					}
				}
			}
			return true
		})
	}
	assert.Equal(t, 4, calls, "the production mintConduitGrant calls")
	assert.Equal(t, want, got, "each call site's origin constant")
}

func isGrantRequestType(e ast.Expr) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "conduitGrantRequest"
}

// grantRequestLiteral returns e as a conduitGrantRequest composite literal
// (or the literal whose address e takes), or nil.
func grantRequestLiteral(e ast.Expr) *ast.CompositeLit {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok || !isGrantRequestType(lit.Type) {
		return nil
	}
	return lit
}

// grantRequestVia returns the grant.Via* constant name lit sets Via to, or
// "" when Via is unset or set to anything else.
func grantRequestVia(lit *ast.CompositeLit) string {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Via" {
			continue
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "grant" && strings.HasPrefix(sel.Sel.Name, "Via") {
			return sel.Sel.Name
		}
		return ""
	}
	return ""
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

// TestConduitOpen_RenewalUnavailable_Tunnel (C5, tunnel): with enforce, on
// a route this node cannot renew on (conduitRouteCanRenewTest), a tunnel to
// a stream_renewal.v2 agent is refused with 503 unavailable,
// reason stream_renewal_unavailable; nothing is signed, nothing reaches
// the agent and the refusal is counted.
func TestConduitOpen_RenewalUnavailable_Tunnel(t *testing.T) {
	o := &openSetup{enforce: true, noRenewalRoute: true}
	tf := newTunnelFixtureServer(t, o.mod)
	tf.startAgent(t, tf.public.URL, tf.launched)
	owner := tf.ownerClient(t)

	o.keys.arm(tf.srv)
	r := owner.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	require.Equal(t, http.StatusServiceUnavailable, r.status, r.body)
	assert.Equal(t, ErrCodeUnavailable, r.err.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, r.err.Details["reason"])
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, tf.dials, "nothing reached the agent")
	assert.Equal(t, []string{conduitRouteTunnel + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, o.rec.list())
}

// TestConduitOpen_ScopeError_Tunnel (C5, tunnel): a scope that cannot be
// determined fails the tunnel with 500 internal_error; nothing is signed
// and nothing reaches the agent.
func TestConduitOpen_ScopeError_Tunnel(t *testing.T) {
	o := &openSetup{scopeErr: true}
	tf := newTunnelFixtureServer(t, o.mod)
	tf.startAgent(t, tf.public.URL, tf.launched)
	owner := tf.ownerClient(t)

	o.keys.arm(tf.srv)
	r := owner.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	require.Equal(t, http.StatusInternalServerError, r.status, r.body)
	assert.Equal(t, ErrCodeInternalError, r.err.Code)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, tf.dials, "nothing reached the agent")
}

// TestConduitOpen_RenewalUnavailable_Proxy (C5, port proxy): with
// enforce, on a route this node cannot renew on, the port proxy to a stream_renewal.v2 agent answers 503
// unavailable, reason stream_renewal_unavailable; nothing is signed and
// the app is never reached.
func TestConduitOpen_RenewalUnavailable_Proxy(t *testing.T) {
	o := &openSetup{enforce: true, noRenewalRoute: true}
	f := newConduitProxyFixtureServer(t, nil, o.mod)
	f.startAgent(t)

	o.keys.arm(f.srv)
	resp := f.get(t, "/hello", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	requireRenewalUnavailableBody(t, er)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, f.app.got, "the app was not reached")
	assert.Equal(t, []string{conduitRouteProxy + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, o.rec.list())
}

// TestConduitOpen_ScopeError_Proxy (C5, port proxy): a scope that cannot
// be determined answers 500 internal_error; nothing is signed and the app
// is never reached.
func TestConduitOpen_ScopeError_Proxy(t *testing.T) {
	o := &openSetup{scopeErr: true}
	f := newConduitProxyFixtureServer(t, nil, o.mod)
	f.startAgent(t)

	o.keys.arm(f.srv)
	resp := f.get(t, "/hello", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	assert.Equal(t, ErrCodeInternalError, er.Error.Code)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, f.app.got, "the app was not reached")
}

// TestConduitOpen_RenewalUnavailable_PTY (C5 and Q6, PTY preflight and
// WebSocket open): with enforce, on a route this node cannot renew on, the
// agent path to a stream_renewal.v2
// agent is refused before any upgrade with 503 unavailable, reason
// stream_renewal_unavailable, by the preflight and the WebSocket open
// alike; nothing is signed or spawned.
func TestConduitOpen_RenewalUnavailable_PTY(t *testing.T) {
	o := &openSetup{enforce: true, noRenewalRoute: true}
	f := newPTYConduitFixtureServer(t, o.mod)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	o.keys.arm(f.srv)
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
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, f.spawned)
	assert.Equal(t, []string{
		conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable,
		conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable,
	}, o.rec.list(), "preflight and open each decided it")
}

// TestConduitOpen_ScopeError_PTY (C5, web PTY and its preflight): the
// preflight mints nothing, so it still names the agent path; the
// WebSocket open then reaches the mint, and a scope that cannot be
// determined closes it with 1011 internal_error; nothing is signed or
// spawned.
func TestConduitOpen_ScopeError_PTY(t *testing.T) {
	o := &openSetup{scopeErr: true}
	f := newPTYConduitFixtureServer(t, o.mod)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	o.keys.arm(f.srv)
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
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, f.spawned)
}

// TestConduitOpen_RenewalUnavailable_PTYBackstop (arch ruling, PTY
// backstop): an agent-path open that reaches the mint on a route that
// cannot renew (as after a re-resolution following the pre-upgrade check)
// is closed with 4504 stream_renewal_unavailable; nothing is signed, no
// stream is opened and the refusal is counted.
func TestConduitOpen_RenewalUnavailable_PTYBackstop(t *testing.T) {
	o := &openSetup{enforce: true, noRenewalRoute: true}
	f := newPTYConduitFixtureServer(t, o.mod)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)

	// The upstream as runAgentPTY builds it, opened directly: the
	// pre-upgrade decision (resolvePTYPath) is not consulted here.
	o.keys.arm(f.srv)
	u := &agentPTYUpstream{s: f.srv, identity: f.owner, agent: f.launched}
	err := u.Open(context.Background(), 80, 24)
	var sc *StreamClosedError
	require.ErrorAs(t, err, &sc)
	assert.Equal(t, wsprotocol.ClosePTYUpstreamTimeout, sc.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnavailable, sc.Reason)
	assert.Nil(t, u.st, "no stream was opened")
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, f.spawned)
	assert.Equal(t, []string{conduitRoutePTY + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, o.rec.list())
}

// TestConduitOpen_PTYPlacementBeforeRenewalPath pins the PTY order (an
// accepted deviation from the contracts order): resolvePTYPath checks the
// agent's saved placement before choosing a path, and the renewal path can
// only be decided once the agent path's row is resolved, so on PTY a
// pinned agent whose placement is inconsistent gets its 409, not the 503,
// on the preflight and the WebSocket open, and no renewal refusal is
// counted.
func TestConduitOpen_PTYPlacementBeforeRenewalPath(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newPTYConduitFixtureServer(t, o.mod)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	ctx := context.Background()
	// Pin the agent to its own, non-flat Runtime Broker: an inconsistent
	// placement (runtime_target_pin_stale).
	_, err := f.store.SetAgentPinnedRuntimeTarget(ctx, f.launched.ID,
		store.PinnedPlacement{RuntimeBrokerID: f.launched.RuntimeBrokerID},
		store.PinnedPlacement{RuntimeBrokerID: f.launched.RuntimeBrokerID, RuntimeTargetID: "target-of-a-non-flat-row", RuntimeTargetType: "docker"})
	require.NoError(t, err)

	status, _, _ := f.preflight(t)
	assert.Equal(t, http.StatusConflict, status)

	_, resp, err := f.dialPTY(t, "")
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, er.Error.Code)
	assert.Empty(t, o.rec.list(), "the renewal path was not decided")
	assert.Empty(t, f.spawned)
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

// newRelayTunnelFixture builds the fixture; o.mod runs before the hub
// serves.
func newRelayTunnelFixture(t *testing.T, o *openSetup) *relayTunnelFixture {
	t.Helper()
	cf := newConduitFixture(t)
	o.mod(cf.srv)
	ops := startRelayOpsHub(t, cf.srv, cf.store)
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
// reason stream_renewal_unavailable; nothing is signed and the relay gets
// no instruction to open anything.
func TestConduitOpen_RenewalUnavailable_RelayTunnel(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newRelayTunnelFixture(t, o)

	o.keys.arm(f.cf.srv)
	resp, open := f.post(t)
	require.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), string(resp.GetBody()))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
	requireRenewalUnavailableBody(t, er)
	assert.Nil(t, open, "no instruction to open the tunnel")
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Equal(t, []string{conduitRouteRelayTunnel + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, o.rec.list())
}

// TestConduitOpen_RelayTunnelOffStampsGrant: with off, the relay gets the
// instruction to open the tunnel, with a grant that carries via=tunnel and
// authz_until for the renewal-capable target.
func TestConduitOpen_RelayTunnelOffStampsGrant(t *testing.T) {
	f := newRelayTunnelFixture(t, &openSetup{})
	resp, open := f.post(t)
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
// that cannot be determined fails the tunnel with 500 internal_error;
// nothing is signed and there is no instruction.
func TestConduitOpen_ScopeError_RelayTunnel(t *testing.T) {
	o := &openSetup{scopeErr: true}
	f := newRelayTunnelFixture(t, o)

	o.keys.arm(f.cf.srv)
	resp, open := f.post(t)
	require.Equal(t, int32(http.StatusInternalServerError), resp.GetStatus(), string(resp.GetBody()))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
	assert.Equal(t, ErrCodeInternalError, er.Error.Code)
	assert.Nil(t, open)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
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

// allowAllConduitActions makes the mint's permission check allow every
// action (the test-only conduitAuthorizeActionTestHook), so a principal
// that no real binding can grant stream actions to today reaches the
// renewal decisions.
func allowAllConduitActions(srv *Server) {
	srv.conduitAuthorizeActionTestHook = func(context.Context, Identity, *store.Agent, Action) error { return nil }
}

// renewalUnsupportedPrincipals are real identities of the
// renewal-unsupported class (design v2.21.36 §3.5, Principals without a
// renewal path), with the principal kind their logs and metrics carry.
func renewalUnsupportedPrincipals(f *conduitFixture) map[string]Identity {
	federated := NewFederatedUserIdentity("https://issuer.example.com", "subject-1", "fed@example.com", "Fed", "member", nil)
	return map[string]Identity{
		string(PrincipalKindFederatedUser):             federated,
		"scoped_" + string(PrincipalKindFederatedUser): NewScopedUserIdentity(federated, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}),
	}
}

// TestConduitRenewalUnsupportedPrincipal: membership is decided by
// principal class. Federated users and visitors, and scoped identities over
// them, are renewal-unsupported; local users, dev users and scoped
// identities over a local user are supported; any other identity is not a
// member (the scope builder's fail-closed path decides it).
func TestConduitRenewalUnsupportedPrincipal(t *testing.T) {
	f := newConduitFixture(t)
	federated := NewFederatedUserIdentity("https://issuer.example.com", "subject-1", "fed@example.com", "Fed", "member", nil)
	visitor := NewVisitorIdentity(tid("visitor-user"), "https://issuer.example.com", "subject-2", "visitor@example.com", "Visitor")
	for want, ident := range map[string]Identity{
		"federated_user":        federated,
		"visitor":               visitor,
		"scoped_federated_user": NewScopedUserIdentity(federated, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}),
		"scoped_visitor":        NewScopedUserIdentity(visitor, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}),
	} {
		kind, unsupported := conduitRenewalUnsupportedPrincipal(ident)
		assert.True(t, unsupported, want)
		assert.Equal(t, want, kind)
	}
	for name, ident := range map[string]Identity{
		"local user":             f.owner,
		"scoped over local user": f.portOnly,
		"agent":                  newAgentIdentityFromStore(f.agent),
		"opaque wrapper":         opaqueIdentity{f.owner},
		"nil":                    nil,
		// A scoped identity over a typed-nil user, and a typed-nil scoped
		// identity, are not members either.
		"scoped over typed-nil user": &ScopedUserIdentity{UserIdentity: (*AuthenticatedUser)(nil)},
		"typed-nil scoped":           (*ScopedUserIdentity)(nil),
	} {
		_, unsupported := conduitRenewalUnsupportedPrincipal(ident)
		assert.False(t, unsupported, name)
	}
}

// TestMintConduitGrant_RenewalUnsupportedPrincipalOff (design v2.21.36
// §3.5): with off, a renewal-unsupported principal's open grant to a
// renewal target is minted unstamped, through the explicit branch, and
// counted with its principal kind.
func TestMintConduitGrant_RenewalUnsupportedPrincipalOff(t *testing.T) {
	for kind, ident := range renewalUnsupportedPrincipals(newConduitFixture(t)) {
		t.Run(kind, func(t *testing.T) {
			f := newConduitFixture(t)
			allowAllConduitActions(f.srv)
			rec := recordOpenRefusals(f.srv)
			tok, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
				Identity: ident, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true,
			})
			require.NoError(t, err)
			require.NotEmpty(t, tok)
			assert.Empty(t, c.Via)
			assert.Nil(t, c.Scope)
			assert.True(t, c.AuthzUntil.IsZero())
			assert.Zero(t, c.AuthzInterval)
			assert.Equal(t, []string{conduitRouteProxy + "/" + kind}, rec.unstampedList())
			assert.Empty(t, rec.list(), "off refuses nothing")
		})
	}
}

// TestMintConduitGrant_RenewalUnsupportedPrincipalEnforce (design v2.21.36
// §3.5): with enforce, a renewal-unsupported principal's open to a renewal
// target is refused before minting with stream_renewal_unavailable, the
// cause renewal_principal_unsupported only in the metric label; nothing is
// signed, and it is never an internal error.
func TestMintConduitGrant_RenewalUnsupportedPrincipalEnforce(t *testing.T) {
	for kind, ident := range renewalUnsupportedPrincipals(newConduitFixture(t)) {
		t.Run(kind, func(t *testing.T) {
			f := newConduitFixture(t)
			allowAllConduitActions(f.srv)
			enforceTargetDeadlines(f.srv)
			rec := recordOpenRefusals(f.srv)
			keys := countSigning(f.srv)
			tok, _, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
				Identity: ident, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true, CanRenew: true,
			})
			assert.ErrorIs(t, err, errConduitStreamRenewalUnavailable)
			assert.NotErrorIs(t, err, errConduitGrantInternal)
			assert.Nil(t, tok)
			assert.Zero(t, keys.reads.Load(), "nothing was signed")
			assert.Equal(t, []string{conduitRouteProxy + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalPrincipalUnsupported}, rec.list())
			assert.Empty(t, rec.unstampedList())
		})
	}
}

// TestMintConduitGrant_VisitorDeniedBeforeMint (design v2.21.36 §3.5): a
// visitor never reaches the mint's renewal decisions: authorization denies
// it every stream action (attach by identity type, port access by its
// role), so it gets 403 and nothing is signed.
func TestMintConduitGrant_VisitorDeniedBeforeMint(t *testing.T) {
	f := newConduitFixture(t)
	enforceTargetDeadlines(f.srv)
	keys := countSigning(f.srv)
	visitor := NewVisitorIdentity(tid("visitor-user"), "https://issuer.example.com", "subject-2", "visitor@example.com", "Visitor")
	for _, tc := range []struct {
		via string
		h   grant.StreamHeader
	}{
		{grant.ViaProxy, tcpHeader("3000")},
		{grant.ViaTunnel, tcpHeader("3000")},
		{grant.ViaPTY, ptyHeader("80", "24")},
	} {
		_, _, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
			Identity: visitor, Agent: f.agent, Stream: tc.h, Target: f.target(), Via: tc.via, TargetRenews: true,
		})
		assert.ErrorIs(t, err, errConduitForbidden, tc.via)
		assert.NotErrorIs(t, err, errConduitStreamRenewalUnavailable, tc.via)
	}
	assert.Zero(t, keys.reads.Load(), "nothing was signed")
}

// TestMintConduitGrant_UnclassifiableDeniedBeforeMint (design v2.21.36
// §3.5): an identity the scope builder cannot classify is denied by
// authorization before the mint, so its fail-closed internal error is
// unreachable.
func TestMintConduitGrant_UnclassifiableDeniedBeforeMint(t *testing.T) {
	f := newConduitFixture(t)
	keys := countSigning(f.srv)
	for name, ident := range map[string]Identity{
		"wrapper hiding a scoped identity": opaqueIdentity{f.portOnly},
		"wrapper hiding a plain user":      opaqueIdentity{f.owner},
		"scoped user with no scopes":       NewScopedUserIdentity(f.owner, f.agent.ProjectID, nil),
		"scoped user with an invalid boundary": NewScopedUserIdentityWithBoundary(f.owner, TokenBoundary{Kind: BoundaryKindProject}, []string{store.UATScopeAgentPortAccess}, "",
			permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.port_access"}}),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
				Identity: ident, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(), Via: grant.ViaProxy, TargetRenews: true,
			})
			assert.ErrorIs(t, err, errConduitForbidden)
			assert.NotErrorIs(t, err, errConduitGrantInternal)
		})
	}
	assert.Zero(t, keys.reads.Load(), "nothing was signed")
}

// TestConduitOpen_RenewalUnsupportedPrincipal_Proxy (design v2.21.36
// §3.5, end to end at the route): a federated user's port stream to a
// stream_renewal.v2 agent under enforce is refused at resolution, by the
// same check as a route that cannot renew, with 503 unavailable, reason
// stream_renewal_unavailable; the metric carries the cause
// renewal_principal_unsupported, nothing is signed and the app is never
// reached.
func TestConduitOpen_RenewalUnsupportedPrincipal_Proxy(t *testing.T) {
	o := &openSetup{enforce: true, allowAll: true}
	f := newConduitProxyFixtureServer(t, nil, o.mod)
	f.startAgent(t)
	federated := NewFederatedUserIdentity("https://issuer.example.com", "subject-1", "fed@example.com", "Fed", "member", nil)

	o.keys.arm(f.srv)
	conn, err := f.srv.openConduitPort(context.Background(), federated, f.launched, f.app.port)
	assert.Nil(t, conn)
	require.ErrorIs(t, err, errConduitStreamRenewalUnavailable)
	rec := httptest.NewRecorder()
	writeConduitProxyError(rec, httptest.NewRequest(http.MethodGet, "/", nil), f.launched.ID, err)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&er))
	requireRenewalUnavailableBody(t, er)
	assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	assert.Empty(t, f.app.got, "the app was not reached")
	assert.Equal(t, []string{conduitRouteProxy + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalPrincipalUnsupported}, o.rec.list())
	assert.Empty(t, o.rec.unstampedList())
}
