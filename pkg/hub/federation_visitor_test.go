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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Federated visitors (ptone/scion#4603): the auth middleware's visitor
// resolution and the route guard's interim refusal.

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
// user-type issuer and a hub-type issuer, both signing with key/kid.
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
			{IssuerURL: visitorTestUserIssuer, IssuerType: string(IssuerTypeUser), JWKSURL: jwksSrv.URL, ExpectedAudience: visitorTestAudience},
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
	claims.EmailVerified = true
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
		"pending account":   {"sub-pending", "pending@corp.com"},
		"suspended account": {"sub-suspended", "suspended@corp.com"},
		"no link":           {"sub-unlinked", "unlinked@corp.com"},
		"unknown subject":   {"sub-nobody", "nobody@corp.com"},
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

	t.Run("hub issuer agent token", func(t *testing.T) {
		claims := validFederationClaims(visitorTestHubIssuer, visitorTestAudience)
		identity, _, code := f.identityThroughMiddleware(t, signFederationToken(t, f.key, f.kid, claims))
		require.Equal(t, http.StatusOK, code)
		_, ok := identity.(*FederatedAgentIdentity)
		assert.True(t, ok, "identity = %T, want *FederatedAgentIdentity", identity)
	})
}

// visitorLookupErrStore fails the federated subject lookup.
type visitorLookupErrStore struct {
	store.Store
}

func (s *visitorLookupErrStore) GetFederatedUserBySubject(context.Context, string, string) (*store.User, error) {
	return nil, errors.New("lookup failure for test")
}

func TestFederatedVisitor_LookupErrorAnswers503(t *testing.T) {
	f := newVisitorFixture(t)
	visitorSetExperiment(t, f.srv, true)
	f.srv.store = &visitorLookupErrStore{Store: f.st}

	identity, _, code := f.identityThroughMiddleware(t, f.userToken(t, "sub-alice", "alice@corp.com"))
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Nil(t, identity, "the handler never runs")
}

// Interim guard: until per-route visitor admission exists, routeGuard
// refuses a visitor on every registered route, with one response.
func TestRouteGuard_RefusesVisitorOnEveryRoute(t *testing.T) {
	srv, _ := testServer(t)
	visitor := NewVisitorIdentity(tid("guard-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")

	require.NotEmpty(t, routeMetadataTable)
	for pattern, meta := range routeMetadataTable {
		called := false
		guarded := srv.routeGuard(meta, func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx := contextWithIdentity(req.Context(), visitor)
		ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(visitor))
		rec := httptest.NewRecorder()
		guarded(rec, req.WithContext(ctx))

		assert.False(t, called, "%s: the handler must not run for a visitor", pattern)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s", pattern)
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "%s", pattern)
		assert.Equal(t, ErrCodeForbidden, body.Error.Code, "%s", pattern)
		assert.Equal(t, visitorRouteRefusalMessage, body.Error.Message, "%s", pattern)
	}
}

// The guard leaves every other identity alone: an unlinked federated user
// on a public route still reaches the handler.
func TestRouteGuard_VisitorRefusalLeavesOtherIdentities(t *testing.T) {
	srv, _ := testServer(t)
	fu := NewFederatedUserIdentity(visitorTestUserIssuer, "sub", "u@corp.com", "U", "viewer", nil)
	called := false
	guarded := srv.routeGuard(RouteMetadata{Classification: RoutePublic}, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	guarded(rec, req.WithContext(contextWithIdentity(req.Context(), fu)))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
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
