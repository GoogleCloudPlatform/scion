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
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Federated visitors (ptone/scion#4603): the auth middleware's visitor
// resolution and the route guard's visitor allow-list.

const (
	visitorTestUserIssuer = "https://idp-visitor.example.com"
	visitorTestHubIssuer  = "https://hub-visitor.example.com"
	visitorTestAudience   = "https://hub-local.example.com"
)

// visitorSetExperiment turns federation.project_visitors on or off on srv
// by replacing its registry; every other experiment keeps its default.
func visitorSetExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.FederationProjectVisitors {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.experimentEnabled(experiments.FederationProjectVisitors))
}

// visitorFixture is a test server whose federation authenticator trusts a
// user-type issuer (with allow_visitor_grants on, so its linked active
// accounts act as visitors) and a hub-type issuer, both signing with
// key/kid.
type visitorFixture struct {
	srv  *Server
	st   store.Store
	auth *FederationAuthenticator
	key  *rsa.PrivateKey
	kid  string
}

func newVisitorFixture(t *testing.T) *visitorFixture {
	t.Helper()
	srv, st := testServer(t)
	key, jwksSrv, kid := setupFederationTestServer(t)
	cfg := config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			{IssuerURL: visitorTestUserIssuer, IssuerType: string(IssuerTypeUser), JWKSURL: jwksSrv.URL, ExpectedAudience: visitorTestAudience,
				AllowVisitorGrants: true},
			{IssuerURL: visitorTestHubIssuer, JWKSURL: jwksSrv.URL, ExpectedAudience: visitorTestAudience},
		},
	}
	auth, err := NewFederationAuthenticator(cfg, visitorTestAudience,
		&http.Client{Timeout: 5 * time.Second}, "dev", slog.Default())
	require.NoError(t, err)
	srv.federationAuth.Store(auth)
	return &visitorFixture{srv: srv, st: st, auth: auth, key: key, kid: kid}
}

// userToken signs a user-issuer token for subject and email.
func (f *visitorFixture) userToken(t *testing.T, subject, email string) string {
	t.Helper()
	claims := validFederationClaims(visitorTestUserIssuer, visitorTestAudience)
	claims.Subject = subject
	claims.Email = email
	verified := true
	claims.EmailVerified = &verified
	claims.Name = "Visitor " + subject
	claims.ProjectID, claims.AgentName, claims.RootUser, claims.Ancestry = "", "", "", nil
	return signFederationToken(t, f.key, f.kid, claims)
}

// account creates a federated account for email with status, linked to
// subject on the user issuer when subject is non-empty.
func (f *visitorFixture) account(t *testing.T, email, status, subject string) *store.User {
	t.Helper()
	ctx := context.Background()
	issuer := visitorTestUserIssuer
	u := &store.User{
		DisplayName: "Visitor", Kind: store.UserKindFederated, Status: status,
		HomeIssuer: &issuer, FederatedEmail: &email,
	}
	require.NoError(t, f.st.CreateFederatedUser(ctx, u))
	if subject != "" {
		_, err := f.st.LinkFederatedIdentity(ctx, u.ID, visitorTestUserIssuer, subject, email)
		require.NoError(t, err)
	}
	return u
}

// identityThroughMiddleware runs token through the server's auth config and
// returns the request identity and credential context the next handler sees,
// with the response status.
func (f *visitorFixture) identityThroughMiddleware(t *testing.T, token string) (Identity, CredentialContext, int) {
	t.Helper()
	var gotIdentity Identity
	var gotCredential CredentialContext
	h := UnifiedAuthMiddleware(f.srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetIdentityFromContext(r.Context())
		gotCredential = GetCredentialContextFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return gotIdentity, gotCredential, rec.Code
}

func TestFederatedVisitor_ExperimentOffKeepsFederatedUserIdentity(t *testing.T) {
	f := newVisitorFixture(t)
	visitorSetExperiment(t, f.srv, false)
	f.account(t, "alice@corp.com", store.UserStatusActive, "sub-alice")

	identity, credential, code := f.identityThroughMiddleware(t, f.userToken(t, "sub-alice", "alice@corp.com"))
	require.Equal(t, http.StatusOK, code)
	fu, ok := identity.(*FederatedUserIdentity)
	require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
	assert.Equal(t, visitorTestUserIssuer+":sub-alice", fu.ID())
	assert.Equal(t, CredentialKindFederation, credential.Kind)
}

func TestFederatedVisitor_LinkedActiveAccountBecomesVisitor(t *testing.T) {
	f := newVisitorFixture(t)
	visitorSetExperiment(t, f.srv, true)
	acct := f.account(t, "alice@corp.com", store.UserStatusActive, "sub-alice")

	identity, credential, code := f.identityThroughMiddleware(t, f.userToken(t, "sub-alice", "Alice@Corp.com"))
	require.Equal(t, http.StatusOK, code)
	v, ok := identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, acct.ID, v.ID(), "the visitor is keyed by the local account ID")
	assert.Equal(t, "visitor", v.Type())
	assert.Equal(t, visitorTestUserIssuer, v.IssuerURL())
	assert.Equal(t, "sub-alice", v.Subject())
	assert.Equal(t, "alice@corp.com", v.Email(), "the account's federated email, never the synthetic address")
	assert.Equal(t, "viewer", v.Role())

	assert.Equal(t, CredentialKindFederation, credential.Kind, "never a session or user access credential")
	assert.Equal(t, PrincipalKindVisitor, principalContextForIdentity(v).Kind)
	assert.Equal(t, store.RoleBindingPrincipalUser, NormalizePrincipalType(string(PrincipalKindVisitor)))
	assert.False(t, sessionCredentialAllowed(contextWithCredentialContext(contextWithIdentity(context.Background(), v), credential)))
}

func TestFederatedVisitor_OtherCasesKeepFederatedUserIdentity(t *testing.T) {
	f := newVisitorFixture(t)
	visitorSetExperiment(t, f.srv, true)
	f.account(t, "pending@corp.com", store.UserStatusPending, "sub-pending")
	suspended := f.account(t, "suspended@corp.com", store.UserStatusActive, "sub-suspended")
	suspended.Status = store.UserStatusSuspended
	require.NoError(t, f.st.UpdateUser(context.Background(), suspended))
	f.account(t, "unlinked@corp.com", store.UserStatusActive, "")

	for name, tc := range map[string]struct{ subject, email string }{
		"pending account": {"sub-pending", "pending@corp.com"},
		"no link":         {"sub-unlinked", "unlinked@corp.com"},
		"unknown subject": {"sub-nobody", "nobody@corp.com"},
	} {
		t.Run(name, func(t *testing.T) {
			identity, credential, code := f.identityThroughMiddleware(t, f.userToken(t, tc.subject, tc.email))
			require.Equal(t, http.StatusOK, code)
			fu, ok := identity.(*FederatedUserIdentity)
			require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
			assert.Equal(t, visitorTestUserIssuer+":"+tc.subject, fu.ID())
			assert.Equal(t, CredentialKindFederation, credential.Kind)
		})
	}

	// A suspended account is refused outright (the status policy), not
	// downgraded to the verified identity.
	t.Run("suspended account", func(t *testing.T) {
		identity, _, code := f.identityThroughMiddleware(t, f.userToken(t, "sub-suspended", "suspended@corp.com"))
		assert.Equal(t, http.StatusForbidden, code)
		assert.Nil(t, identity, "the handler never runs")
	})

	t.Run("hub issuer agent token", func(t *testing.T) {
		claims := validFederationClaims(visitorTestHubIssuer, visitorTestAudience)
		identity, _, code := f.identityThroughMiddleware(t, signFederationToken(t, f.key, f.kid, claims))
		require.Equal(t, http.StatusOK, code)
		_, ok := identity.(*FederatedAgentIdentity)
		assert.True(t, ok, "identity = %T, want *FederatedAgentIdentity", identity)
	})
}

// visitorLookupErrStore fails the federated subject lookup once its fault
// switch is armed.
type visitorLookupErrStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *visitorLookupErrStore) GetFederatedUserBySubject(ctx context.Context, issuerURL, subject string) (*store.User, error) {
	if !s.fault.Active() {
		return s.Store.GetFederatedUserBySubject(ctx, issuerURL, subject)
	}
	return nil, errors.New("lookup failure for test")
}

func TestFederatedVisitor_LookupErrorAnswers503(t *testing.T) {
	f := newVisitorFixture(t)
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, fault *storeFaultSwitch) *visitorLookupErrStore {
		return &visitorLookupErrStore{Store: inner, fault: fault}
	})
	visitorSetExperiment(t, f.srv, true)
	fault.Arm()

	identity, _, code := f.identityThroughMiddleware(t, f.userToken(t, "sub-alice", "alice@corp.com"))
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Nil(t, identity, "the handler never runs")
}

// visitorRouteMethod is one route table pattern and one method sent to it.
type visitorRouteMethod struct {
	pattern, method string
}

// expectedVisitorRouteMethods is the complete set of (pattern, method)
// pairs that admit a visitor (RouteMetadata.VisitorMethods). Listing any
// other method on any route fails
// TestRouteGuard_AdmitsVisitorOnlyOnListedMethods until the pair is added
// here. On a subtree pattern the pair admits only the sub-paths pinned in
// expectedVisitorSubRoutes (federation_visitor_messaging_test.go).
var expectedVisitorRouteMethods = []visitorRouteMethod{
	{"/api/v1/auth/me", http.MethodGet}, // own profile, read-only
	{"/api/v1/auth/me", http.MethodHead},
	{"/api/v1/projects/", http.MethodPost},              // send a message to an agent
	{"/api/v1/projects/", http.MethodGet},               // list and read the project's agents (visitor view)
	{"/api/v1/users/me/visitor-grants", http.MethodGet}, // own visitor grants
	{"/api/v1/users/me/visitor-dms", http.MethodGet},    // own DMs with agents
	{"/api/v1/agents/", http.MethodPost},                // send a message to an agent
	{"/api/v1/chat/conversations/", http.MethodGet},     // own DM history and read marker
	{"/api/v1/chat/conversations/", http.MethodPost},    // own DM read marker
}

// visitorRefusedProfileRoutes are the profile write routes reachable by a
// signed-in user: user-scope env and secrets, user templates, injected
// skills, chat preferences, presence, attachment upload, the user record,
// user access credentials, delegations and the terminal workspace. None
// admits a visitor.
var visitorRefusedProfileRoutes = []string{
	"/api/v1/env",
	"/api/v1/env/",
	"/api/v1/secrets",
	"/api/v1/secrets/",
	"/api/v1/users/me/templates",
	"/api/v1/users/me/templates/",
	"/api/v1/users/me/injected-skills",
	"/api/v1/users/me/injected-skills/",
	"/api/v1/chat/prefs",
	"/api/v1/chat/user-prefs",
	"/api/v1/chat/presence",
	"/api/v1/chat/attachments",
	"/api/v1/chat/attachments/",
	"/api/v1/users/",
	"/api/v1/auth/tokens",
	"/api/v1/auth/tokens/",
	"/api/v1/users/me/delegations",
	"/api/v1/users/me/terminal-workspace",
}

// visitorRouteMethods returns the methods to try on a route pattern: the
// pattern's own method when it has one (and HEAD for GET, which the mux
// also serves), otherwise every common method.
func visitorRouteMethods(pattern string) []string {
	if method, _, ok := strings.Cut(pattern, " "); ok {
		if method == http.MethodGet {
			return []string{http.MethodGet, http.MethodHead}
		}
		return []string{method}
	}
	return []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
}

// serveGuardedAs runs one request with method and identity (and its
// credential context) through srv.routeGuard(meta) in front of a handler
// that answers 200, and reports whether the handler ran.
func serveGuardedAs(srv *Server, meta RouteMetadata, method string, identity Identity) (*httptest.ResponseRecorder, bool) {
	called := false
	guarded := srv.routeGuard(meta, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, "/", nil)
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	rec := httptest.NewRecorder()
	guarded(rec, req.WithContext(ctx))
	return rec, called
}

// assertVisitorRefusal asserts rec is the uniform visitor refusal: 403,
// ErrCodeForbidden and visitorRouteRefusalMessage.
func assertVisitorRefusal(t *testing.T, rec *httptest.ResponseRecorder, label string) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "%s", label)
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "%s", label)
	assert.Equal(t, ErrCodeForbidden, body.Error.Code, "%s", label)
	assert.Equal(t, visitorRouteRefusalMessage, body.Error.Message, "%s", label)
}

// Every registered route (TestRegisteredRoutesHaveRouteMetadata keeps the
// table equal to the registered set) and method refuses a visitor before
// the handler, with one response, unless the pair is in
// expectedVisitorRouteMethods; the pairs the routes list in VisitorMethods
// are exactly that set. A listed pair on a subtree pattern still refuses
// the request here, whose path is no admitted sub-path.
func TestRouteGuard_AdmitsVisitorOnlyOnListedMethods(t *testing.T) {
	srv, _ := testServer(t)
	visitor := NewVisitorIdentity(tid("guard-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")

	listed := map[visitorRouteMethod]bool{}
	for pattern, meta := range routeMetadataTable {
		for _, method := range meta.VisitorMethods {
			listed[visitorRouteMethod{pattern, method}] = true
		}
	}
	allowed := make(map[visitorRouteMethod]bool, len(expectedVisitorRouteMethods))
	for _, pm := range expectedVisitorRouteMethods {
		allowed[pm] = true
	}
	require.Equal(t, allowed, listed, "the (pattern, method) pairs routes list in VisitorMethods")

	require.NotEmpty(t, routeMetadataTable)
	for pattern, meta := range routeMetadataTable {
		for _, method := range visitorRouteMethods(pattern) {
			label := method + " " + pattern
			rec, called := serveGuardedAs(srv, meta, method, visitor)
			if allowed[visitorRouteMethod{pattern, method}] && !routePatternIsSubtree(pattern) {
				assert.True(t, called, "%s: the handler must run for a visitor", label)
				assert.Equal(t, http.StatusOK, rec.Code, "%s", label)
				continue
			}
			assert.False(t, called, "%s: the handler must not run for a visitor", label)
			assertVisitorRefusal(t, rec, label)
		}
	}
}

// The profile write routes are registered and none admits a visitor.
func TestRouteGuard_ProfileWriteRoutesRefuseVisitor(t *testing.T) {
	srv, _ := testServer(t)
	visitor := NewVisitorIdentity(tid("profile-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")
	for _, pattern := range visitorRefusedProfileRoutes {
		meta, ok := routeMetadataTable[pattern]
		require.True(t, ok, "%s is not a registered route", pattern)
		assert.Empty(t, meta.VisitorMethods, "%s must not admit a visitor", pattern)
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			rec, called := serveGuardedAs(srv, meta, method, visitor)
			assert.False(t, called, "%s %s: the handler must not run for a visitor", method, pattern)
			assertVisitorRefusal(t, rec, method+" "+pattern)
		}
	}
}

// The list defaults to refusal: a route of any classification with no
// VisitorMethods refuses a visitor, and a list admits one on its methods
// only.
func TestRouteGuard_VisitorRefusedUnlessMethodListed(t *testing.T) {
	srv, _ := testServer(t)
	visitor := NewVisitorIdentity(tid("default-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")
	for _, c := range []RouteClassification{RoutePublic, RouteAuthenticated, RoutePolicy, RouteHubAdmin,
		RouteWorkstation, RouteBrokerHMAC, RouteAgentToken, RouteWebhook} {
		meta := RouteMetadata{Pattern: "/api/v1/test/visitor", RouteID: "test.visitor", Classification: c}
		rec, called := serveGuardedAs(srv, meta, http.MethodGet, visitor)
		assert.False(t, called, "%s: the handler must not run for a visitor", c)
		assertVisitorRefusal(t, rec, string(c))
	}

	meta := RouteMetadata{Pattern: "/api/v1/test/visitor", RouteID: "test.visitor",
		Classification: RouteAuthenticated, VisitorMethods: []string{http.MethodGet}}
	rec, called := serveGuardedAs(srv, meta, http.MethodGet, visitor)
	assert.True(t, called, "a listed method admits a visitor")
	assert.Equal(t, http.StatusOK, rec.Code)
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec, called := serveGuardedAs(srv, meta, method, visitor)
		assert.False(t, called, "%s: a method not listed must not run the handler for a visitor", method)
		assertVisitorRefusal(t, rec, method)
	}
}

// The visitor check leaves every other identity alone: on no registered
// route does an unlinked federated user receive the visitor refusal.
func TestRouteGuard_VisitorRefusalLeavesOtherIdentities(t *testing.T) {
	srv, _ := testServer(t)
	fu := NewFederatedUserIdentity(visitorTestUserIssuer, "sub", "u@corp.com", "U", "viewer", nil)
	rec, called := serveGuardedAs(srv, RouteMetadata{Classification: RoutePublic}, http.MethodGet, fu)
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)

	for pattern, meta := range routeMetadataTable {
		rec, _ := serveGuardedAs(srv, meta, visitorRouteMethods(pattern)[0], fu)
		assert.NotContains(t, rec.Body.String(), visitorRouteRefusalMessage, "%s", pattern)
	}
}

// End to end through the server handler: a linked active account's token
// is refused with the visitor response while the experiment is on, and the
// same token is not while it is off.
func TestFederatedVisitor_ServerHandlerRefusesVisitor(t *testing.T) {
	f := newVisitorFixture(t)
	f.account(t, "alice@corp.com", store.UserStatusActive, "sub-alice")
	token := f.userToken(t, "sub-alice", "alice@corp.com")

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		req.Header.Set(FederationTokenHeader, token)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	visitorSetExperiment(t, f.srv, true)
	rec := do()
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), visitorRouteRefusalMessage)

	visitorSetExperiment(t, f.srv, false)
	rec = do()
	assert.NotContains(t, rec.Body.String(), visitorRouteRefusalMessage,
		"with the experiment off the token is an ordinary federated user")
}
