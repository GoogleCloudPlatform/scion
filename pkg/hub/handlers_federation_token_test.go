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
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	federationTokenPath     = "/api/v1/auth/federation-token"
	federationTokenAudience = "https://peer-hub.example.com"
)

// federationTokenServer returns a test server with an OIDC key manager,
// federation.project_visitors on, and federationTokenAudience allowed.
func federationTokenServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv := testOIDCServer(t)
	visitorSetExperiment(t, srv, true)
	srv.config.OIDCConfig.UserTokenAudiences = []string{federationTokenAudience}
	return srv, srv.store
}

// federationTokenUser stores a user of the given kind (human or empty) and
// status through the store's CreateUser.
func federationTokenUser(t *testing.T, s store.Store, kind, status string) *store.User {
	t.Helper()
	id := uuid.New().String()
	u := &store.User{
		ID: id, Email: "ft-" + id[:8] + "@example.com", DisplayName: "FT " + id[:8],
		Role: "member", Status: status, Kind: kind,
	}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

// callFederationToken calls the handler directly with identity and its
// credential context, as the auth middleware would set them.
func callFederationToken(t *testing.T, srv *Server, identity Identity, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var cred *CredentialContext
	if identity != nil {
		c := credentialContextForIdentity(identity)
		cred = &c
	}
	return callFederationTokenAs(t, srv, identity, cred, body)
}

// callFederationTokenAs calls the handler directly with identity and the
// given credential context (none when cred is nil).
func callFederationTokenAs(t *testing.T, srv *Server, identity Identity, cred *CredentialContext, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, federationTokenPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := req.Context()
	if identity != nil {
		ctx = contextWithIdentity(ctx, identity)
	}
	if cred != nil {
		ctx = contextWithCredentialContext(ctx, *cred)
	}
	w := httptest.NewRecorder()
	srv.handleFederationToken(w, req.WithContext(ctx))
	return w
}

func sessionIdentityFor(u *store.User) *AuthenticatedUser {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

func federationTokenBody(aud string) map[string]string {
	return map[string]string{"audience": aud}
}

// federationTokenErrorMessage returns the error message of a JSON error response.
func federationTokenErrorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body.Error.Message
}

// TestFederationToken_RefusedCallers covers every caller other than an
// active local human user with a session or a scoped token: each gets the
// same 403 and message, and no token.
func TestFederationToken_RefusedCallers(t *testing.T) {
	srv, s := federationTokenServer(t)

	identities := map[string]Identity{
		"dev":             NewDevUser(DevUserConfig{}),
		"agent":           &agentIdentityWrapper{AgentTokenClaims: &AgentTokenClaims{Claims: jwt.Claims{Subject: uuid.New().String()}, ProjectID: "p1"}},
		"federated user":  NewFederatedUserIdentity("https://idp.example.com", "sub-1", "fed@example.com", "Fed", "viewer", nil),
		"federated agent": NewFederatedAgentIdentity("https://hub-x.example.com", "agent-x", "proj-x", "ax", "root-x", []string{"root-x"}, nil),
		"visitor":         NewVisitorIdentity(uuid.New().String(), "https://idp.example.com", "sub-2", "v@example.com", "V"),
		"broker":          NewBrokerIdentity("broker-1"),
	}
	// Rows the store's CreateUser does not write (test identity and
	// federated kinds, the pending status) come from a stub GetUser.
	stub := federationTokenUserStub{Store: s, users: map[string]*store.User{}}
	stubUser := func(kind, status string) *store.User {
		id := uuid.New().String()
		u := &store.User{ID: id, Email: "ft-" + id[:8] + "@example.com", DisplayName: "FT", Role: "member", Status: status, Kind: kind}
		stub.users[id] = u
		return u
	}
	for _, kind := range []string{store.UserKindTestFixture, store.UserKindTestHubAdmin, store.UserKindTestSuperAdmin, store.UserKindFederated} {
		identities["stored kind "+kind] = sessionIdentityFor(stubUser(kind, store.UserStatusActive))
	}
	for _, status := range []string{store.UserStatusSuspended, store.UserStatusInvited, store.UserStatusPending} {
		identities["status "+status] = sessionIdentityFor(stubUser(store.UserKindHuman, status))
	}
	srv.store = stub
	identities["no stored user"] = NewAuthenticatedUser(uuid.New().String(), "ghost@example.com", "Ghost", "member", string(ClientTypeWeb))

	names := make([]string, 0, len(identities))
	for name := range identities {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			w := callFederationToken(t, srv, identities[name], federationTokenBody(federationTokenAudience))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.Equal(t, federationTokenRefusalMessage, federationTokenErrorMessage(t, w))
			assert.NotContains(t, w.Body.String(), `"token"`)
		})
	}

	t.Run("unauthenticated", func(t *testing.T) {
		w := callFederationToken(t, srv, nil, federationTokenBody(federationTokenAudience))
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestFederationToken_RefusedCredentialKinds pins the credential kind
// check on its own: the identity is an active local human user that would
// otherwise be admitted, but the request's credential is neither an
// interactive session nor a user access token.
func TestFederationToken_RefusedCredentialKinds(t *testing.T) {
	srv, s := federationTokenServer(t)
	alice := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)
	session := sessionIdentityFor(alice)
	scoped := selfToken(t, alice.ID, hubBoundary(), "federation_token:mint")

	// Control: both identities are admitted with their own credential.
	require.Equal(t, http.StatusOK, callFederationToken(t, srv, session, federationTokenBody(federationTokenAudience)).Code)
	require.Equal(t, http.StatusOK, callFederationToken(t, srv, scoped, federationTokenBody(federationTokenAudience)).Code)

	cases := []struct {
		name     string
		identity Identity
		kind     CredentialKind
	}{
		{"session identity, broker credential", session, CredentialKindBroker},
		{"session identity, dev credential", session, CredentialKindDev},
		{"session identity, federation credential", session, CredentialKindFederation},
		{"session identity, agent credential", session, CredentialKindAgentJWT},
		{"scoped identity, workload delegation credential", scoped, CredentialKindWorkloadDelegation},
		{"session identity, no credential kind", session, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := callFederationTokenAs(t, srv, tc.identity, &CredentialContext{Kind: tc.kind}, federationTokenBody(federationTokenAudience))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.Equal(t, federationTokenRefusalMessage, federationTokenErrorMessage(t, w))
		})
	}
}

// TestFederationToken_UserAccessTokenNeedsMintScope drives real requests:
// a session and a hub-boundary token with federation_token:mint succeed; a
// token without it is refused; the scope cannot be put on a project token.
func TestFederationToken_UserAccessTokenNeedsMintScope(t *testing.T) {
	srv, s := federationTokenServer(t)
	alice := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)
	ensureHubMembership(context.Background(), s, alice.ID)
	body := federationTokenBody(federationTokenAudience)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, federationTokenPath, body)
	assert.Equal(t, http.StatusOK, rec.Code, "session: %s", rec.Body.String())

	with := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "federation_token:mint")
	rec = doRequestWithToken(t, srv, with, http.MethodPost, federationTokenPath, body)
	assert.Equal(t, http.StatusOK, rec.Code, "token with scope: %s", rec.Body.String())

	without := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "inbox:read")
	rec = doRequestWithToken(t, srv, without, http.MethodPost, federationTokenPath, body)
	require.Equal(t, http.StatusForbidden, rec.Code, "token without scope: %s", rec.Body.String())
	assert.Equal(t, federationTokenRefusalMessage, federationTokenErrorMessage(t, rec))

	project := &store.Project{ID: uuid.New().String(), Name: "FT", Slug: "ft-" + uuid.New().String()[:8]}
	require.NoError(t, s.CreateProject(context.Background(), project))
	createTestUserWithProjectRole(t, s, alice.ID, alice.Email, project.ID, store.ProjectRoleOwner)
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(alice.ID), CreateTokenParams{
		UserID: alice.ID, Name: "ft-" + tid("tok"), Boundary: projectBoundary(project.ID), Scopes: []string{"federation_token:mint"},
	})
	assert.Error(t, err, "federation_token:mint is not mintable on a project boundary")
}

func TestFederationToken_ExperimentOff(t *testing.T) {
	srv, s := federationTokenServer(t)
	visitorSetExperiment(t, srv, false)
	alice := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, federationTokenPath, federationTokenBody(federationTokenAudience))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	w := callFederationToken(t, srv, NewDevUser(DevUserConfig{}), federationTokenBody(federationTokenAudience))
	assert.Equal(t, http.StatusNotFound, w.Code, "experiment off answers 404 before any caller check")
}

func TestFederationToken_NoOIDCProvider(t *testing.T) {
	srv, s := testServer(t)
	visitorSetExperiment(t, srv, true)
	srv.config.OIDCConfig.UserTokenAudiences = []string{federationTokenAudience}
	alice := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)
	w := callFederationToken(t, srv, sessionIdentityFor(alice), federationTokenBody(federationTokenAudience))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestFederationToken_AudienceAllowList(t *testing.T) {
	srv, s := federationTokenServer(t)
	alice := sessionIdentityFor(federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive))

	t.Run("missing audience is 400", func(t *testing.T) {
		w := callFederationToken(t, srv, alice, map[string]string{})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		w = callFederationToken(t, srv, alice, federationTokenBody("  / "))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
	t.Run("non-matching audience is 403", func(t *testing.T) {
		w := callFederationToken(t, srv, alice, federationTokenBody("https://other-hub.example.com"))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, federationTokenAudienceRefusalMessage, federationTokenErrorMessage(t, w))
	})
	t.Run("trailing slash matches", func(t *testing.T) {
		w := callFederationToken(t, srv, alice, federationTokenBody(federationTokenAudience+"/"))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		claims := verifiedFederationClaims(t, srv, w)
		assert.Equal(t, jwt.Audience{federationTokenAudience}, claims.Audience)
	})
	t.Run("empty allow-list refuses every audience", func(t *testing.T) {
		srv.config.OIDCConfig.UserTokenAudiences = nil
		w := callFederationToken(t, srv, alice, federationTokenBody(federationTokenAudience))
		require.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, federationTokenAudienceRefusalMessage, federationTokenErrorMessage(t, w))
	})
}

// verifiedFederationClaims verifies the response token against the hub's
// JWKS and returns its claims.
func verifiedFederationClaims(t *testing.T, srv *Server, w *httptest.ResponseRecorder) FederationUserTokenClaims {
	t.Helper()
	var resp federationTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	parsed, err := jwt.ParseSigned(resp.Token, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err)
	jwks := srv.oidcKeyManager.JWKS()
	require.NotEmpty(t, jwks.Keys)
	assert.Equal(t, jwks.Keys[0].KeyID, parsed.Headers[0].KeyID)
	var claims FederationUserTokenClaims
	require.NoError(t, parsed.Claims(jwks.Keys[0].Key, &claims))
	assert.True(t, resp.ExpiresAt.Equal(claims.Expiry.Time()), "expires_at matches exp")
	return claims
}

// TestFederationToken_ClaimsExact checks the token's claim set exactly,
// including that no agent claim is present, and the lifetime bounds.
func TestFederationToken_ClaimsExact(t *testing.T) {
	srv, s := federationTokenServer(t)
	u := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)

	before := time.Now().Truncate(time.Second)
	w := callFederationToken(t, srv, sessionIdentityFor(u), federationTokenBody(federationTokenAudience))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	claims := verifiedFederationClaims(t, srv, w)

	require.NoError(t, claims.Validate(jwt.Expected{Issuer: testOIDCIssuerURL, AnyAudience: jwt.Audience{federationTokenAudience}, Time: time.Now()}))
	assert.Equal(t, u.ID, claims.Subject)
	assert.Equal(t, u.Email, claims.Email)
	assert.True(t, claims.EmailVerified)
	assert.Equal(t, u.DisplayName, claims.Name)
	assert.Equal(t, "user", claims.TokenKind)
	assert.NotEmpty(t, claims.ID)
	assert.Equal(t, claims.IssuedAt.Time(), claims.NotBefore.Time())
	assert.False(t, claims.IssuedAt.Time().Before(before))
	assert.Equal(t, 15*time.Minute, claims.Expiry.Time().Sub(claims.IssuedAt.Time()), "default lifetime")

	// The encoded claim set has exactly these claims: no project_id, agent_name,
	// ancestry or root_user.
	var resp federationTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	parts := strings.Split(resp.Token, ".")
	require.Len(t, parts, 3)
	claimSet, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var raw map[string]interface{}
	require.NoError(t, json.Unmarshal(claimSet, &raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"aud", "email", "email_verified", "exp", "iat", "iss", "jti", "name", "nbf", "scion_token_kind", "sub"}, keys)

	// A configured lifetime applies; two tokens get distinct IDs.
	srv.config.OIDCConfig.UserTokenLifetime = time.Hour
	w2 := callFederationToken(t, srv, sessionIdentityFor(u), federationTokenBody(federationTokenAudience))
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	claims2 := verifiedFederationClaims(t, srv, w2)
	assert.Equal(t, time.Hour, claims2.Expiry.Time().Sub(claims2.IssuedAt.Time()), "configured lifetime")
	assert.NotEqual(t, claims.ID, claims2.ID)
}

// TestFederationToken_LifetimeAboveMaxRefusedAtStartup checks that the hub
// does not start with a lifetime above one hour.
func TestFederationToken_LifetimeAboveMaxRefusedAtStartup(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	cfg := testServerConfig()
	cfg.OIDCConfig.UserTokenLifetime = time.Hour + time.Minute
	_, err = New(cfg, s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "user_token_lifetime")

	cfg = testServerConfig()
	cfg.OIDCConfig.UserTokenAudiences = []string{"not a url"}
	_, err = New(cfg, s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "user_token_audiences")
}

func TestFederationToken_AuditWritten(t *testing.T) {
	srv, s := federationTokenServer(t)
	u := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)

	w := callFederationToken(t, srv, sessionIdentityFor(u), federationTokenBody(federationTokenAudience))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	claims := verifiedFederationClaims(t, srv, w)

	records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: "federation_token_issued", TargetID: u.ID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, records, 1)
	rec := records[0]
	assert.Equal(t, "user", rec.TargetType)
	assert.Equal(t, u.ID, rec.ActorPrincipalID)
	var after map[string]string
	require.NoError(t, json.Unmarshal([]byte(rec.AfterSummary), &after))
	assert.Equal(t, federationTokenAudience, after["audience"])
	assert.Equal(t, claims.ID, after["jti"])
	assert.Equal(t, claims.Expiry.Time().UTC().Format(time.RFC3339), after["expires_at"])

	var resp federationTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotContains(t, rec.AfterSummary, resp.Token)

	// A refused request writes no audit record.
	refused := callFederationToken(t, srv, sessionIdentityFor(u), federationTokenBody("https://other-hub.example.com"))
	require.Equal(t, http.StatusForbidden, refused.Code)
	records, _, err = s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: "federation_token_issued", TargetID: u.ID, Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, records, 1)
}

// federationTokenUserStub answers GetUser from users first.
type federationTokenUserStub struct {
	store.Store
	users map[string]*store.User
}

func (s federationTokenUserStub) GetUser(ctx context.Context, id string) (*store.User, error) {
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return s.Store.GetUser(ctx, id)
}

// federationAuditFailStore fails every mutation audit write.
type federationAuditFailStore struct {
	store.Store
}

func (federationAuditFailStore) CreateMutationAudit(context.Context, *store.MutationAuditRecord) error {
	return errors.New("audit backend down")
}

func TestFederationToken_AuditFailureIssuesNothing(t *testing.T) {
	srv, s := federationTokenServer(t)
	u := federationTokenUser(t, s, store.UserKindHuman, store.UserStatusActive)
	srv.store = federationAuditFailStore{Store: s}

	w := callFederationToken(t, srv, sessionIdentityFor(u), federationTokenBody(federationTokenAudience))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), `"token"`)
}
