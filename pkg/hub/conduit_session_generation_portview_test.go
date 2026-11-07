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
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authctx"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/portview"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Session generation on the port proxy (proxyToAgentPort) and its two
// callers: the agent port route and the port page view. The capture sits in
// proxyToAgentPort, so both callers capture. On the page view, a hub user
// token request by the link's own user is captured; any other user's token
// runs as the link (contextWithPortViewLink) and is never captured.

// Fails if the capture is missing on the page view path (for example moved
// into the agent port route only): fix (2)'s track-time guard then denies
// the link user's own credentialed request on the conduit transport. Also
// fails if a scoped page view identity is refused by the capture.
func TestPortViewSessionGen_SameUserCurrentAllowed(t *testing.T) {
	f := newPortViewFixture(t, nil)
	f.startAgent(t)
	for name, h := range map[string]http.Header{"navigation": navHeader(), "sub-request": subHeader()} {
		res := f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "page.html"), withBearer(h, f.userToken), nil)
		require.Equal(t, http.StatusOK, res.status, "%s: %s", name, res.body)
		assert.Equal(t, "hello /page.html?", res.body, name)
	}
}

// Fails if the page view path stops capturing (for example if the capture
// moves into the agent port route only), on a hub JWT request that carries
// no session generation (here: no user store, so the JWT arm sets none). The
// capture's own refusal answers it with this exact body; without the capture
// on this path the request would instead reach the conduit transport and be
// denied there by the track-time guard, with a different answer. So this
// also pins that the capture runs on the page view path.
func TestPortViewSessionGen_JWTWithoutGenerationRefused(t *testing.T) {
	f := newPortViewFixture(t, nil)
	f.startAgent(t)
	saved := f.srv.authConfig.UserStore
	f.srv.authConfig.UserStore = nil
	t.Cleanup(func() { f.srv.authConfig.UserStore = saved })
	// Positive control, over the same server as the requests below: with no
	// user store, the same token passes the auth middleware on an
	// authenticated route that has no capture, so the 401 below comes from
	// the capture and not from a middleware refusal.
	me := f.send(t, http.MethodGet, f.base+"/api/v1/auth/me", withBearer(http.Header{}, f.userToken), nil)
	require.Equal(t, http.StatusOK, me.status, "the token passes the middleware without a store: %s", me.body)
	for name, h := range map[string]http.Header{"navigation": navHeader(), "sub-request": subHeader()} {
		res := f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "page.html"), withBearer(h, f.userToken), nil)
		assert.Equal(t, http.StatusUnauthorized, res.status, "%s: %s", name, res.body)
		assert.Equal(t, `{"error":{"code":"unauthorized","message":"invalid access token"}}`+"\n", res.body, name)
	}
}

// The documented limit: a credential-less page view link keeps working
// after the link user's sessions are revoked, until the link expires
// (view_ttl). Fails if a revoke-all starts cutting credential-less links
// (a behaviour change this restack does not make) or if the link stops
// being served at all.
func TestPortViewSessionGen_SignedLinkSurvivesRevokeAll(t *testing.T) {
	f := newPortViewFixture(t, nil)
	f.startAgent(t)
	link := f.viewURL(f.ownerCap(), "page.html")
	before := f.send(t, http.MethodGet, link, subHeader(), nil)
	require.Equal(t, http.StatusOK, before.status, before.body)

	revokeSessions(t, f.srv, f.launched.OwnerID)

	after := f.send(t, http.MethodGet, link, subHeader(), nil)
	require.Equal(t, http.StatusOK, after.status, "the credential-less link still works after a revoke-all: %s", after.body)
	assert.Equal(t, "hello /page.html?", after.body)
}

// Fails if the scoped page view identity's captured generation stops
// driving the stream re-check: a stream opened as the link user's own
// captured identity is closed once that user's sessions are revoked.
func TestPortViewSessionGen_SameUserStreamClosedOnRevoke(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	scoped := NewScopedUserIdentity(f.u1, f.agent.ProjectID, []string{portViewScope})
	ctx := authctx.WithSessionGeneration(contextWithAuthType(context.Background(), AuthTypeJWT), 0)
	captured, ok := conduitCaptureSessionGeneration(ctx, scoped)
	require.True(t, ok, "the scoped page view identity is captured")
	w := f.open(t, captured, grant.StreamKindTCP, 1)

	revokeSessions(t, f.srv, f.u1.ID())

	assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
}

// Fails if another user's hub user token on a page view sub-request stops
// being served as the link (upstream behaviour, port_view_test.go "other
// user signed in, not a navigation"), for example if the cross-user case
// is refused or captured.
func TestPortViewSessionGen_CrossUserSubRequestServedAsLink(t *testing.T) {
	f := newPortViewFixture(t, nil)
	f.startAgent(t)
	res := f.send(t, http.MethodGet, f.viewURL(f.ownerCap(), "page.html"), withBearer(subHeader(), f.strangerToken), nil)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.Equal(t, "hello /page.html?", res.body)
}

// Fails if the track-time guard honours a link marker that does not belong
// to the identity, or stops denying a hub user token request whose identity
// was not captured and carries no marker. The link user's identity in the
// cross-user case carries no captured generation (it is never stamped), and
// the guard admits it only through its own link's marker.
func TestPortViewSessionGen_TrackTimeGuardAndMarker(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	ident := NewScopedUserIdentity(f.u1, f.agent.ProjectID, []string{portViewScope})
	_, set := conduitCapturedSessionGeneration(ident)
	require.False(t, set, "the link user's identity carries no captured generation")
	authz := func(ctx context.Context) error {
		_, _, err := f.srv.authorizeConduitUserStream(ctx, conduitUserStreamAuthz{Identity: ident, Agent: f.agent, Kind: grant.StreamKindTCP})
		return err
	}
	jwt := contextWithAuthType(context.Background(), AuthTypeJWT)

	assert.ErrorIs(t, authz(jwt), errConduitPrincipalInactive, "a hub JWT request, no capture, no marker: denied")
	own := contextWithPortViewLink(jwt, portview.Grant{AgentID: f.agent.ID, Port: 3000, UserID: f.u1.ID()})
	assert.NoError(t, authz(own), "the identity's own verified link: runs as the link")
	forged := contextWithPortViewLink(jwt, portview.Grant{AgentID: f.agent.ID, Port: 3000, UserID: "someone-else"})
	assert.ErrorIs(t, authz(forged), errConduitPrincipalInactive, "a link marker for another user is not honoured")
	assert.False(t, portViewLinkFor(context.Background(), ident), "no marker without contextWithPortViewLink")
}

// Fails if the capture changes any scope field of a scoped identity, which
// could widen what the page view identity may reach (spec (g)).
func TestConduitCaptureSessionGeneration_ScopedPreservesScope(t *testing.T) {
	user := NewAuthenticatedUser("u-1", "u1@example.test", "U1", store.UserRoleMember, "web")
	scoped := NewScopedUserIdentity(user, "proj-1", []string{portViewScope})
	ctx := authctx.WithSessionGeneration(contextWithAuthType(context.Background(), AuthTypeJWT), 4)
	got, ok := conduitCaptureSessionGeneration(ctx, scoped)
	require.True(t, ok)
	stamped, isScoped := got.(*ScopedUserIdentity)
	require.True(t, isScoped, "the capture keeps the scoped type")
	gen, set := conduitCapturedSessionGeneration(stamped)
	require.True(t, set)
	assert.Equal(t, int64(4), gen)

	before := *scoped
	after := *stamped
	after.UserIdentity = before.UserIdentity
	assert.Equal(t, before, after, "every scope field is equal before and after the capture")
	assert.Equal(t, user.ID(), stamped.ID())
}

// Fails if a new AuthType constant is added to principal/identity.go
// without an explicit case in conduitAuthTypeCapturesSessionGeneration, or
// if any type other than AuthTypeJWT is classified as capturing (spec (f)).
func TestConduitAuthTypeClassification_Exhaustive(t *testing.T) {
	declared := authTypeConstNames(t, filepath.Join("internal", "principal", "identity.go"))
	require.NotEmpty(t, declared)
	listed := switchCaseNames(t, "conduit_session_generation.go", "conduitAuthTypeCapturesSessionGeneration")
	assert.Equal(t, declared, listed, "every AuthType constant has an explicit case")
	for _, name := range declared {
		v, ok := authTypeValues[name]
		require.True(t, ok, "authTypeValues lists %s", name)
		assert.Equal(t, name == "AuthTypeJWT", conduitAuthTypeCapturesSessionGeneration(v), name)
	}
	assert.False(t, conduitAuthTypeCapturesSessionGeneration(""), "empty")
	assert.False(t, conduitAuthTypeCapturesSessionGeneration("not-a-type"), "unknown")
}

// authTypeValues maps each AuthType constant's name to its value (through
// pkg/hub's aliases); the exhaustiveness test checks it covers them all.
var authTypeValues = map[string]string{
	"AuthTypeJWT": AuthTypeJWT, "AuthTypeUAT": AuthTypeUAT, "AuthTypeDevToken": AuthTypeDevToken,
	"AuthTypeAgent": AuthTypeAgent, "AuthTypeBroker": AuthTypeBroker, "AuthTypeProxy": AuthTypeProxy,
	"AuthTypeFederation": AuthTypeFederation, "AuthTypeSignedURL": AuthTypeSignedURL,
	"AuthTypeExternalBearer": AuthTypeExternalBearer, "AuthTypeAgentDelegation": AuthTypeAgentDelegation,
}

// Fails if the capture moves out of proxyToAgentPort into one caller, or if
// the page view link marker gets a second setter (spec: one place, after
// portview.Parse).
func TestPortViewSessionGen_StaticPlacement(t *testing.T) {
	assert.Equal(t, []string{"proxyToAgentPort"}, callerFuncs(t, "port_forward_handlers.go", "conduitCaptureSessionGeneration"),
		"the capture is called only inside proxyToAgentPort")
	assert.Empty(t, callerFuncs(t, "port_view.go", "conduitCaptureSessionGeneration"), "the page view does not capture by itself")
	var sites []string
	for _, f := range nonTestGoFiles(t) {
		for _, fn := range callerFuncs(t, f, "contextWithPortViewLink") {
			sites = append(sites, f+":"+fn)
		}
	}
	assert.Equal(t, []string{"port_view.go:handlePortView"}, sites, "the link marker has exactly one setter")
}

func parseGo(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err, path)
	return f
}

// authTypeConstNames returns the sorted names of the AuthType* constants
// declared in path.
func authTypeConstNames(t *testing.T, path string) []string {
	t.Helper()
	var names []string
	for _, d := range parseGo(t, path).Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(n.Name, "AuthType") {
					names = append(names, n.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

// switchCaseNames returns the sorted identifiers listed in the case clauses
// of the switch in function fn of path.
func switchCaseNames(t *testing.T, path, fn string) []string {
	t.Helper()
	var names []string
	for _, d := range parseGo(t, path).Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if id, ok := e.(*ast.Ident); ok {
						names = append(names, id.Name)
					}
				}
			}
			return true
		})
	}
	sort.Strings(names)
	return names
}

// callerFuncs returns the sorted names of the functions in path that call
// the function named callee.
func callerFuncs(t *testing.T, path, callee string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, d := range parseGo(t, path).Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == callee {
					seen[fd.Name.Name] = true
				}
			}
			return true
		})
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// nonTestGoFiles returns the package's non-test Go files.
func nonTestGoFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var out []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
