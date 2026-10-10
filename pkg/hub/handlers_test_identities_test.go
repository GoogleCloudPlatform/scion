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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for hub-issued test identities (ptone/scion#4240, Phase 2a). All
// identities here are synthetic fixtures in an in-memory store.

// enableTestIdentitiesForTest turns the feature on for a server built by a
// shared helper (the effect of --enable-test-identities).
func enableTestIdentitiesForTest(srv *Server) {
	srv.testIdentities = newTestIdentityState(true)
	srv.authConfig.TestIdentitiesEnabled = true
}

// newTestIdentityServer builds a test server with the feature on or off.
func newTestIdentityServer(t *testing.T, enabled bool) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	if enabled {
		enableTestIdentitiesForTest(srv)
	}
	return srv, s
}

// tiIssuer creates a super-admin user and returns its ID and a hub-bound
// access token carrying only test_identity:issue: the issuer credential.
func tiIssuer(t *testing.T, srv *Server, s store.Store, name string) (string, string) {
	t.Helper()
	id := hubConfigTokenUser(t, s, name, store.SystemRoleSuperAdmin)
	return id, mintHubConfigToken(t, srv, id, hubBoundary(), "test_identity:issue")
}

func tiPost(t *testing.T, srv *Server, token, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, srv, token, http.MethodPost, path, body)
}

func tiDecode(t *testing.T, rec *httptest.ResponseRecorder) TestIdentityTokenResponse {
	t.Helper()
	var resp TestIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// tiIssue issues a test identity with token and returns the decoded
// response, requiring 201.
func tiIssue(t *testing.T, srv *Server, token string, body interface{}) TestIdentityTokenResponse {
	t.Helper()
	rec := tiPost(t, srv, token, "/api/v1/test-identities", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return tiDecode(t, rec)
}

// tiStoreFixture writes a test-fixture row directly through the store's
// dedicated create path.
func tiStoreFixture(t *testing.T, s store.Store, issuer string, expiresAt time.Time) *store.User {
	t.Helper()
	u := &store.User{
		ID:          generateID(),
		Email:       "test-identity-" + generateID()[:12] + "@" + store.TestFixtureEmailDomain,
		DisplayName: "Stored fixture",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestFixture,
		ExpiresAt:   &expiresAt,
		IssuedBy:    &issuer,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateTestFixtureUser(context.Background(), u))
	ensureHubMembership(context.Background(), s, u.ID)
	return u
}

func tiTokenFor(t *testing.T, srv *Server, u *store.User) string {
	t.Helper()
	token, _, err := srv.userTokenService.GenerateAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, u.Role, ClientTypeAPI, 10*time.Minute)
	require.NoError(t, err)
	return token
}

// jwtLifetime returns exp-iat of a JWT without verifying it.
func jwtLifetime(t *testing.T, token string) time.Duration {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims struct {
		Exp int64 `json:"exp"`
		Iat int64 `json:"iat"`
	}
	require.NoError(t, json.Unmarshal(raw, &claims))
	return time.Duration(claims.Exp-claims.Iat) * time.Second
}

func tiAuthMe(t *testing.T, srv *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithToken(t, srv, token, http.MethodGet, "/api/v1/auth/me", nil)
}

// --- Flag off ---------------------------------------------------------------

// Flag off: every route returns 404, and a pre-existing fixture's token is
// refused with 401 on /api/v1/auth/me (and reported invalid).
func TestTestIdentity_FlagOff(t *testing.T) {
	srv, s := newTestIdentityServer(t, false)
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-off-issuer")

	rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, issuerTok, http.MethodGet, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusNotFound, rec.Code, "an admin dev credential also gets 404: %s", rec.Body.String())

	fixture := tiStoreFixture(t, s, issuerID, time.Now().Add(time.Hour))
	rec = tiPost(t, srv, issuerTok, "/api/v1/test-identities/"+fixture.ID+"/token", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	token := tiTokenFor(t, srv, fixture)
	rec = tiAuthMe(t, srv, token)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeUserNotFound, errorCodeOf(t, rec))

	rec = doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: token})
	require.Equal(t, http.StatusOK, rec.Code)
	var v AuthValidateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	assert.False(t, v.Valid)

	// Turning the flag on (a restart) makes the same live fixture usable.
	enableTestIdentitiesForTest(srv)
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, token).Code)
}

// --- Issuer authorization ---------------------------------------------------

func TestTestIdentity_IssuerAuthorization(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-authz-issuer")

	// An issuer token with test_identity:issue: 201.
	resp := tiIssue(t, srv, issuerTok, map[string]string{"role": "member", "purpose": "authz"})
	assert.NotEmpty(t, resp.AccessToken)

	// The issuer token can do nothing else.
	// A list is filtered to nothing; a mutation is refused.
	rec := doRequestWithToken(t, srv, issuerTok, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"totalCount":0`, "issuer token sees no projects: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, issuerTok, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "ti-issuer-project"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "issuer token creating a project: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, issuerTok, http.MethodGet, "/api/v1/admin/server-config", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "issuer token on server config: %s", rec.Body.String())

	// A token without the scope: 403.
	admin := hubConfigTokenUser(t, s, "ti-authz-noscope", store.SystemRoleSuperAdmin)
	noScope := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_health:read")
	rec = tiPost(t, srv, noScope, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// test_identity:issue cannot be minted on a project boundary.
	projectID, owner := tid("ti-authz-project"), tid("ti-authz-project-owner")
	rs4Project(t, s, projectID, owner)
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(owner), CreateTokenParams{
		UserID: owner, Name: "ti-project-bound", Boundary: projectBoundary(projectID), Scopes: []string{"test_identity:issue"},
	})
	assert.Error(t, err, "test_identity:issue is hub-only")

	// A member session without the permission: 403.
	member := &store.User{ID: tid("ti-authz-member"), Email: "ti-authz-member@test.com", DisplayName: "m", Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)
	rec = doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A non-fixture member's session in general: 403 on every route.
	rec = doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// A fixture's own session: 403 (no permission).
	fixtureTok := resp.AccessToken
	rec = tiPost(t, srv, fixtureTok, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// An admin session: 201.
	adminUser, err := s.GetUser(ctx, admin)
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "viewer"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// A member granted the permission by a role binding: 201.
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID: api.NewUUID(), Name: "ti-issuer-role", Description: "test identity issuer",
		ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: member.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// No built-in role except super-admin holds test_identity.issue.
func TestTestIdentity_PermissionInNoDefaultRole(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	defs, err := s.ListRoleDefinitions(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, defs)
	for _, rd := range defs {
		if rd.Name == store.SystemRoleSuperAdmin {
			assert.Contains(t, rd.Permissions, permissionTestIdentityIssue, "super-admin holds every permission")
			continue
		}
		assert.NotContains(t, rd.Permissions, permissionTestIdentityIssue, "role %q must not hold test_identity.issue", rd.Name)
	}
}

// --- Request validation -----------------------------------------------------

// The role enum is {member, viewer}: admin, Admin and unknown values get
// 400, and no other user field can be bound from the request.
func TestTestIdentity_RequestValidation(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	_, issuerTok := tiIssuer(t, srv, s, "ti-val-issuer")

	keys := make([]string, 0, len(testIdentityRoles))
	for k := range testIdentityRoles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{store.UserRoleMember, store.UserRoleViewer}, keys, "the request role enum")

	bad := []interface{}{
		map[string]interface{}{"role": "admin"},
		map[string]interface{}{"role": "Admin"},
		map[string]interface{}{"role": "ADMIN"},
		map[string]interface{}{"role": "owner"},
		map[string]interface{}{"role": "Member"},
		map[string]interface{}{"role": 1},
		map[string]interface{}{"kind": "human"},
		map[string]interface{}{"email": "chosen@example.com"},
		map[string]interface{}{"expiresAt": "2099-01-01T00:00:00Z"},
		map[string]interface{}{"issuedBy": "someone"},
		map[string]interface{}{"lifetimeSeconds": 59},
		map[string]interface{}{"lifetimeSeconds": 8*3600 + 1},
		map[string]interface{}{"tokenTtlSeconds": -1},
		map[string]interface{}{"tokenTtlSeconds": 8*3600 + 1},
		map[string]interface{}{"purpose": strings.Repeat("x", testIdentityPurposeMaxRunes+1)},
		map[string]interface{}{"purpose": "bad\x07purpose"},
	}
	for _, body := range bad {
		rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body %v: %s", body, rec.Body.String())
	}
	list, err := s.ListTestFixtureUsers(context.Background(), "", time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, list, "no rejected request may create a row")
}

// --- Issued row, token and response -----------------------------------------

func TestTestIdentity_IssuedRowAndToken(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-row-issuer")

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", map[string]interface{}{
		"role": "member", "purpose": "row check", "lifetimeSeconds": 3600, "tokenTtlSeconds": 600,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"), "no cookie")

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	gotKeys := make([]string, 0, len(raw))
	for k := range raw {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	assert.Equal(t, []string{"accessToken", "expiresIn", "identity", "tokenExpiresAt", "tokenType"}, gotKeys,
		"exactly one token and no refresh token")
	assert.NotContains(t, rec.Body.String(), "refresh")

	resp := tiDecode(t, rec)
	assert.Equal(t, 10*time.Minute, jwtLifetime(t, resp.AccessToken), "token lifetime equals the requested TTL")
	assert.Equal(t, int64(600), resp.ExpiresIn)

	// The issued row carries BOTH the test_fixture kind and the reserved
	// domain, plus its expiry, issuer and purpose.
	row, err := s.GetUser(ctx, resp.Identity.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserKindTestFixture, row.Kind)
	assert.True(t, store.IsTestFixtureEmail(row.Email), "email %q", row.Email)
	assert.True(t, strings.HasSuffix(row.Email, "@"+store.TestFixtureEmailDomain))
	assert.Equal(t, store.UserRoleMember, row.Role)
	require.NotNil(t, row.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *row.ExpiresAt, time.Minute)
	require.NotNil(t, row.IssuedBy)
	assert.Equal(t, issuerID, *row.IssuedBy)
	require.NotNil(t, row.Purpose)
	assert.Equal(t, "row check", *row.Purpose)

	// Hub grants were synced: the identity is a hub member.
	group, err := s.GetGroupBySlug(ctx, hubMembersSlug)
	require.NoError(t, err)
	_, err = s.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, row.ID)
	assert.NoError(t, err, "a member test identity is in hub-members")

	// The token works.
	me := tiAuthMe(t, srv, resp.AccessToken)
	require.Equal(t, http.StatusOK, me.Code, me.Body.String())

	// Every issued row has both markers.
	for i := 0; i < 2; i++ {
		r := tiIssue(t, srv, issuerTok, map[string]string{"role": "viewer"})
		u, err := s.GetUser(ctx, r.Identity.ID)
		require.NoError(t, err)
		assert.True(t, u.IsTestFixture() && store.IsTestFixtureEmail(u.Email), "row %s", u.ID)
		assert.Equal(t, store.UserRoleViewer, u.Role)
	}

	// A token never outlives its identity.
	short := tiIssue(t, srv, issuerTok, map[string]interface{}{"lifetimeSeconds": 120, "tokenTtlSeconds": 3600})
	assert.LessOrEqual(t, jwtLifetime(t, short.AccessToken), 120*time.Second)
	assert.Greater(t, jwtLifetime(t, short.AccessToken), 100*time.Second)

	// Default TTL is 30 minutes.
	def := tiIssue(t, srv, issuerTok, nil)
	assert.Equal(t, testIdentityDefaultTokenTTL, jwtLifetime(t, def.AccessToken))

	// One durable audit row per issuance, never containing a token.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityIssueMutation, TargetID: row.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, issuerID, a.ActorPrincipalID)
	assert.Equal(t, string(CredentialKindUAT), a.ActorCredentialType)
	assert.NotEmpty(t, a.ActorCredentialID, "the issuer credential id")
	var summary map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &summary))
	assert.Equal(t, row.ID, summary["user_id"])
	assert.Equal(t, "member", summary["role"])
	assert.Equal(t, issuerID, summary["issued_by"])
	assert.Equal(t, "row check", summary["purpose"])
	assert.Equal(t, float64(600), summary["token_ttl_seconds"])

	all, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{Limit: 1000})
	require.NoError(t, err)
	for _, tok := range []string{resp.AccessToken, short.AccessToken, def.AccessToken} {
		sig := tok[strings.LastIndex(tok, ".")+1:]
		assert.NotContains(t, logs.String(), sig, "no token in logs")
		for _, rec := range all {
			assert.NotContains(t, rec.AfterSummary+rec.BeforeSummary, sig, "no token in audit")
		}
	}
}

// --- Token re-issue ---------------------------------------------------------

func TestTestIdentity_TokenReissue(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-re-issuer")
	_, otherTok := tiIssuer(t, srv, s, "ti-re-other")

	first := tiIssue(t, srv, issuerTok, map[string]interface{}{"lifetimeSeconds": 3600})
	path := "/api/v1/test-identities/" + first.Identity.ID + "/token"

	rec := tiPost(t, srv, issuerTok, path, map[string]interface{}{"tokenTtlSeconds": 300})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	again := tiDecode(t, rec)
	assert.NotEqual(t, first.AccessToken, again.AccessToken)
	assert.Equal(t, 5*time.Minute, jwtLifetime(t, again.AccessToken))
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, again.AccessToken).Code)

	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityTokenIssueMutation, TargetID: first.Identity.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, issuerID, audits[0].ActorPrincipalID)
	assert.NotContains(t, audits[0].AfterSummary, again.AccessToken[strings.LastIndex(again.AccessToken, ".")+1:])

	// Another issuer, a non-fixture user and an unknown ID all get 404.
	assert.Equal(t, http.StatusNotFound, tiPost(t, srv, otherTok, path, nil).Code)
	assert.Equal(t, http.StatusNotFound, tiPost(t, srv, issuerTok, "/api/v1/test-identities/"+issuerID+"/token", nil).Code)
	assert.Equal(t, http.StatusNotFound, tiPost(t, srv, issuerTok, "/api/v1/test-identities/"+generateID()+"/token", nil).Code)
	assert.Equal(t, http.StatusNotFound, tiPost(t, srv, issuerTok, "/api/v1/test-identities/not-a-uuid/token", nil).Code)

	// An expired identity: 409, no token.
	srv.testIdentities.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	rec = tiPost(t, srv, issuerTok, path, nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "accessToken")
}

// --- Expiry and the auth path -----------------------------------------------

// Past expires_at (fake clock): 401 user_not_found-class error. The check
// sits in the JWT arm's per-request user-row block.
func TestTestIdentity_ExpiryRefusedAtAuth(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	_, issuerTok := tiIssuer(t, srv, s, "ti-exp-issuer")
	resp := tiIssue(t, srv, issuerTok, map[string]interface{}{"lifetimeSeconds": 600})
	require.Equal(t, http.StatusOK, tiAuthMe(t, srv, resp.AccessToken).Code)

	srv.testIdentities.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	rec := tiAuthMe(t, srv, resp.AccessToken)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeUserNotFound, errorCodeOf(t, rec))
	rec = doRequestWithToken(t, srv, resp.AccessToken, http.MethodGet, "/api/v1/projects", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: resp.AccessToken})
	var v AuthValidateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	assert.False(t, v.Valid)
}

// T4: a fixture row with no expiry is treated as expired. The DB CHECK
// refuses such a row (pkg/store/entadapter
// TestUsers_DBCheckRejectsFixtureWithoutExpiry), so the middleware is
// driven here with a user store that returns one.
func TestTestIdentity_NullExpiryFailsClosed(t *testing.T) {
	now := time.Now()
	assert.Equal(t, "test_identity_expired", testFixtureRejection(&store.User{Kind: store.UserKindTestFixture}, true, now))
	exp := now.Add(time.Minute)
	assert.Equal(t, "", testFixtureRejection(&store.User{Kind: store.UserKindTestFixture, ExpiresAt: &exp}, true, now))
	assert.Equal(t, "test_identity_expired", testFixtureRejection(&store.User{Kind: store.UserKindTestFixture, ExpiresAt: &exp}, true, exp))
	assert.Equal(t, "test_identities_disabled", testFixtureRejection(&store.User{Kind: store.UserKindTestFixture, ExpiresAt: &exp}, false, now))
	assert.Equal(t, "reserved_test_identity_domain", testFixtureRejection(&store.User{Email: "x@" + store.TestFixtureEmailDomain}, true, now))
	assert.Equal(t, "", testFixtureRejection(&store.User{Email: "x@scion-test.invalid"}, true, now))
	assert.Equal(t, "", testFixtureRejection(&store.User{Email: "x@example.com"}, false, now))

	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	fixture := &store.User{ID: generateID(), Email: "n@" + store.TestFixtureEmailDomain, Role: "member", Status: "active", Kind: store.UserKindTestFixture}
	users := &mockUserStore{users: map[string]*store.User{fixture.ID: fixture}}
	token, _, err := tokenSvc.GenerateAccessTokenWithTTL(fixture.ID, fixture.Email, "", "member", ClientTypeAPI, time.Minute)
	require.NoError(t, err)

	handler := UnifiedAuthMiddleware(AuthConfig{Mode: "production", UserTokenSvc: tokenSvc, UserStore: users, TestIdentitiesEnabled: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

// Placement: with the flag on and no user store, the JWT arm fails closed
// rather than skipping the row block, and New refuses the configuration.
func TestTestIdentity_RequiresUserStore(t *testing.T) {
	_, err := New(ServerConfig{EnableTestIdentities: true}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a user store")

	tokenSvc, err := NewUserTokenService(UserTokenConfig{})
	require.NoError(t, err)
	token, _, err := tokenSvc.GenerateAccessTokenWithTTL(generateID(), "someone@example.com", "", "member", ClientTypeAPI, time.Minute)
	require.NoError(t, err)
	handler := UnifiedAuthMiddleware(AuthConfig{Mode: "production", UserTokenSvc: tokenSvc, TestIdentitiesEnabled: true})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("must not reach the handler without a user store")
		}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// --- Caps, rate limit, rollback ---------------------------------------------

// T5: N parallel issuances for one issuer create exactly the per-issuer
// cap; the rest get 429.
func TestTestIdentity_IssuerCapAtomic(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	issuerID, issuerTok := tiIssuer(t, srv, s, "ti-cap-issuer")

	const n = 9
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = tiPost(t, srv, issuerTok, "/api/v1/test-identities", map[string]string{"role": "member"}).Code
		}(i)
	}
	wg.Wait()
	created, capped := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			capped++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	assert.Equal(t, testIdentityPerIssuerCap, created)
	assert.Equal(t, n-testIdentityPerIssuerCap, capped)
	live, err := s.CountLiveTestFixtureUsers(context.Background(), issuerID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, testIdentityPerIssuerCap, live)

	// Expired identities do not count against the cap.
	srv.testIdentities.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// T5, hub-wide: parallel issuances from several issuers never exceed the
// hub cap.
func TestTestIdentity_HubCapAtomic(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	// Fill the hub to two below the cap with stored fixtures.
	for i := 0; i < testIdentityHubCap-2; i++ {
		tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))
	}
	const issuers = 6
	toks := make([]string, issuers)
	for i := range toks {
		_, toks[i] = tiIssuer(t, srv, s, "ti-hubcap-issuer-"+string(rune('a'+i)))
	}
	codes := make([]int, issuers)
	var wg sync.WaitGroup
	for i := 0; i < issuers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = tiPost(t, srv, toks[i], "/api/v1/test-identities", nil).Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else {
			assert.Equal(t, http.StatusTooManyRequests, c)
		}
	}
	assert.Equal(t, 2, created)
	live, err := s.CountLiveTestFixtureUsers(context.Background(), "", time.Now())
	require.NoError(t, err)
	assert.Equal(t, testIdentityHubCap, live)
}

func TestTestIdentity_RateLimit(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	_, issuerTok := tiIssuer(t, srv, s, "ti-rate-issuer")
	srv.testIdentities.limiter = NewGCPTokenRateLimiter(0.0001, 2)
	assert.Equal(t, http.StatusCreated, tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil).Code)
	assert.Equal(t, http.StatusCreated, tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil).Code)
	rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), testIdentityReasonRateLimited)
}

// A failing audit write, and a failing hub grant sync, roll the issuance
// back: no row is left.
func TestTestIdentity_FailuresRollBack(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-rb-issuer")

	srv.testIdentities.hooks.writeAudit = func(context.Context, store.Store, *store.MutationAuditRecord) error {
		return errors.New("audit unavailable")
	}
	rec := tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "accessToken")
	list, err := s.ListTestFixtureUsers(ctx, "", time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, list, "audit failure leaves no identity")

	srv.testIdentities.hooks = newTestIdentityState(true).hooks
	srv.testIdentities.hooks.syncGrants = func(context.Context, store.Store, string, string, string) error {
		return errors.New("grant sync failed")
	}
	rec = tiPost(t, srv, issuerTok, "/api/v1/test-identities", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	list, err = s.ListTestFixtureUsers(ctx, "", time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, list, "grant sync failure leaves no identity")
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testIdentityIssueMutation})
	require.NoError(t, err)
	assert.Empty(t, audits)

	// Re-issue: an audit failure returns no token.
	srv.testIdentities.hooks = newTestIdentityState(true).hooks
	resp := tiIssue(t, srv, issuerTok, nil)
	srv.testIdentities.hooks.writeAudit = func(context.Context, store.Store, *store.MutationAuditRecord) error {
		return errors.New("audit unavailable")
	}
	rec = tiPost(t, srv, issuerTok, "/api/v1/test-identities/"+resp.Identity.ID+"/token", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "accessToken")
}

// --- List isolation ---------------------------------------------------------

// GET lists only the caller's own identities; an admin session sees all;
// a fixture itself cannot list.
func TestTestIdentity_ListIsolation(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	aID, aTok := tiIssuer(t, srv, s, "ti-list-a")
	_, bTok := tiIssuer(t, srv, s, "ti-list-b")

	a1 := tiIssue(t, srv, aTok, nil)
	a2 := tiIssue(t, srv, aTok, nil)
	b1 := tiIssue(t, srv, bTok, nil)

	ids := func(token string) []string {
		rec := doRequestWithToken(t, srv, token, http.MethodGet, "/api/v1/test-identities", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListTestIdentitiesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		out := []string{}
		for _, it := range resp.Items {
			out = append(out, it.ID)
			assert.NotContains(t, rec.Body.String(), "accessToken")
		}
		sort.Strings(out)
		return out
	}
	wantA := []string{a1.Identity.ID, a2.Identity.ID}
	sort.Strings(wantA)
	assert.Equal(t, wantA, ids(aTok))
	assert.Equal(t, []string{b1.Identity.ID}, ids(bTok))

	// Fixtures cannot list at all.
	for _, tok := range []string{a1.AccessToken, b1.AccessToken} {
		rec := doRequestWithToken(t, srv, tok, http.MethodGet, "/api/v1/test-identities", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	}

	// An admin session sees every identity.
	aUser, err := s.GetUser(ctx, aID)
	require.NoError(t, err)
	rec := doRequestAsUser(t, srv, aUser, http.MethodGet, "/api/v1/test-identities", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var all ListTestIdentitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	assert.Len(t, all.Items, 3)
}

// --- Member fidelity and cross-user isolation -------------------------------

// tiCreateProject creates a project as token and returns its ID.
func tiCreateProject(t *testing.T, srv *Server, token, name string) string {
	t.Helper()
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: name})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusOK, "create project: %d %s", rec.Code, rec.Body.String())
	var p store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	require.NotEmpty(t, p.ID, rec.Body.String())
	return p.ID
}

// tiAddProvider makes a broker serve projectID so agents can be created.
func tiAddProvider(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{ID: tid("ti-broker-" + projectID), Name: "ti broker", Slug: "ti-broker-" + projectID[:8], Status: store.BrokerStatusOnline, Endpoint: "http://localhost:9800"}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: projectID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline}))
}

// The issued identity, as an ordinary member, creates a project (becoming
// its owner), creates, PATCHes, starts and deletes an agent in it, and
// deletes the project.
func TestTestIdentity_MemberFidelity(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	_, issuerTok := tiIssuer(t, srv, s, "ti-fid-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member", "purpose": "fidelity"})
	tok := fx.AccessToken

	projectID := tiCreateProject(t, srv, tok, "ti-fidelity-project")
	tiAddProvider(t, s, projectID)

	rec := doRequestWithToken(t, srv, tok, http.MethodPost, "/api/v1/projects/"+projectID+"/agents", CreateAgentRequest{Name: "ti-fidelity-agent"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create agent: %d %s", rec.Code, rec.Body.String())
	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.Agent)
	agentID := created.Agent.ID
	stored, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	assert.Equal(t, fx.Identity.ID, stored.OwnerID, "the test identity owns its agent")

	rec = doRequestWithToken(t, srv, tok, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]any{"taskSummary": "edited by a test identity"})
	require.Equal(t, http.StatusOK, rec.Code, "patch agent: %s", rec.Body.String())

	rec = doRequestWithToken(t, srv, tok, http.MethodPost, "/api/v1/agents/"+agentID+"/start", nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "start agent: %d %s", rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/agents/"+agentID, nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "delete agent: %d %s", rec.Code, rec.Body.String())

	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/projects/"+projectID, nil)
	require.True(t, rec.Code >= 200 && rec.Code < 300, "delete project: %d %s", rec.Code, rec.Body.String())
}

// Two concurrent fixtures, each a member with its own project and agent,
// cannot reach each other's project or agent.
func TestTestIdentity_CrossUserIsolation(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	srv.SetDispatcher(&deleteGuardDispatcher{})
	_, issuerTok := tiIssuer(t, srv, s, "ti-iso-issuer")
	a := tiIssue(t, srv, issuerTok, nil)
	b := tiIssue(t, srv, issuerTok, nil)

	type owned struct{ project, agent string }
	setup := func(fx TestIdentityTokenResponse, name string) owned {
		p := tiCreateProject(t, srv, fx.AccessToken, name)
		agentID := tid(name + "-agent")
		createCredTestAgent(t, s, agentID, p, fx.Identity.ID)
		return owned{p, agentID}
	}
	oa := setup(a, "ti-iso-a")
	ob := setup(b, "ti-iso-b")

	refused := func(code int) bool { return code == http.StatusForbidden || code == http.StatusNotFound }
	for _, c := range []struct {
		name  string
		token string
		other owned
	}{{"A on B", a.AccessToken, ob}, {"B on A", b.AccessToken, oa}} {
		t.Run(c.name, func(t *testing.T) {
			reqs := []struct {
				method, path string
				body         interface{}
			}{
				{http.MethodGet, "/api/v1/projects/" + c.other.project, nil},
				{http.MethodPatch, "/api/v1/projects/" + c.other.project, map[string]string{"name": "taken"}},
				{http.MethodDelete, "/api/v1/projects/" + c.other.project, nil},
				{http.MethodGet, "/api/v1/agents/" + c.other.agent, nil},
				{http.MethodPatch, "/api/v1/agents/" + c.other.agent, map[string]any{"taskSummary": "taken"}},
				{http.MethodPost, "/api/v1/agents/" + c.other.agent + "/start", nil},
				{http.MethodDelete, "/api/v1/agents/" + c.other.agent, nil},
			}
			for _, r := range reqs {
				rec := doRequestWithToken(t, srv, c.token, r.method, r.path, r.body)
				assert.True(t, refused(rec.Code), "%s %s: got %d %s", r.method, r.path, rec.Code, rec.Body.String())
			}
		})
	}
	// The owners still reach their own.
	assert.Equal(t, http.StatusOK, doRequestWithToken(t, srv, a.AccessToken, http.MethodGet, "/api/v1/agents/"+oa.agent, nil).Code)
	assert.Equal(t, http.StatusOK, doRequestWithToken(t, srv, b.AccessToken, http.MethodGet, "/api/v1/projects/"+ob.project, nil).Code)
}

// --- Containment ------------------------------------------------------------

func TestTestIdentity_Containment(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	ctx := context.Background()
	_, issuerTok := tiIssuer(t, srv, s, "ti-cont-issuer")
	fx := tiIssue(t, srv, issuerTok, map[string]string{"role": "member"})
	fid := fx.Identity.ID

	t.Run("fixture cannot create an access token", func(t *testing.T) {
		rec := doRequestWithToken(t, srv, fx.AccessToken, http.MethodPost, "/api/v1/auth/tokens", map[string]interface{}{
			"name": "fixture-pat", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"project:read"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		_, _, err := srv.uatService.CreateTokenWithParams(contextWithCredentialContext(
			contextWithIdentity(ctx, NewAuthenticatedUser(fid, fx.Identity.Email, "", "member", string(ClientTypeAPI))),
			CredentialContext{Kind: CredentialKindInteractive, ID: "s"}), CreateTokenParams{
			UserID: fid, Name: "x", Boundary: hubBoundary(), Scopes: []string{"project:read"},
		})
		assert.ErrorIs(t, err, ErrUATTestIdentityDenied)
	})

	// Every role-mutating path returns 4xx for a fixture row.
	t.Run("role mutations refused", func(t *testing.T) {
		for _, role := range []string{"admin", "viewer"} {
			rec := doRequest(t, srv, http.MethodPatch, "/api/v1/users/"+fid, map[string]string{"role": role})
			assert.Equal(t, http.StatusConflict, rec.Code, "PATCH role %s: %s", role, rec.Body.String())
		}
		superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		for _, principal := range []string{fid, fx.Identity.Email} {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", map[string]string{
				"roleDefinitionId": superRD.ID, "principalType": "user", "principalId": principal, "scopeType": "system",
			})
			assert.True(t, rec.Code >= 400 && rec.Code < 500, "system binding for %s: %d %s", principal, rec.Code, rec.Body.String())
		}
		hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/role-bindings", map[string]string{
			"roleDefinitionId": hubAdminRD.ID, "principalType": "user", "principalId": fid, "scopeType": "system",
		})
		assert.True(t, rec.Code >= 400 && rec.Code < 500, "hub-admin binding: %d %s", rec.Code, rec.Body.String())

		// admin_emails reconciliation never promotes a fixture.
		_, err = ReconcileSuperAdminBindings(ctx, s, []string{fx.Identity.Email}, store.UserRoleMember)
		require.NoError(t, err)

		// The in-transaction role transition refuses too.
		row, err := s.GetUser(ctx, fid)
		require.NoError(t, err)
		_, err = srv.executeRoleTransition(ctx, s, row, store.UserRoleAdmin, superRD, DevUserID, superAdminBindingState{})
		assert.ErrorIs(t, err, errRoleOnTestFixture)

		row, err = s.GetUser(ctx, fid)
		require.NoError(t, err)
		assert.Equal(t, store.UserRoleMember, row.Role)
		bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, fid)
		require.NoError(t, err)
		for _, b := range bindings {
			assert.NotEqual(t, store.RoleScopeSystem, b.ScopeType, "no hub-level binding: %+v", b)
		}
		assert.Equal(t, http.StatusOK, tiAuthMe(t, srv, fx.AccessToken).Code)
	})

	t.Run("fixture with a stored issuer grant still cannot issue", func(t *testing.T) {
		rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: api.NewUUID(), Name: "ti-cont-issuer-role", Description: "x",
			ScopeType: store.RoleScopeSystem, Permissions: []string{permissionTestIdentityIssue},
		})
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID: fid, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
		require.NoError(t, err)
		rec := tiPost(t, srv, fx.AccessToken, "/api/v1/test-identities", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), testIdentityReasonFixtureCall)
	})
}

// --- Reserved domain at every other path (T2 iii) ---------------------------

func TestTestIdentity_ReservedDomainRefusedEverywhere(t *testing.T) {
	ctx := context.Background()
	srv, s := newTestIdentityServer(t, true)
	reserved := "someone@" + store.TestFixtureEmailDomain
	existing := tiStoreFixture(t, s, generateID(), time.Now().Add(time.Hour))

	t.Run("provisionUser (hub OAuth, CLI and device sign-in, proxy provisioner)", func(t *testing.T) {
		for _, email := range []string{reserved, existing.Email, "X@SCION-FIXTURE.INVALID"} {
			_, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: email})
			assert.ErrorIs(t, err, ErrAccessDenied, email)
		}
		_, err := s.GetUserByEmail(ctx, reserved)
		assert.ErrorIs(t, err, store.ErrNotFound)
		u, err := srv.provisionUser(ctx, &ExternalUserInfo{Email: "ok-" + generateID()[:6] + "@scion-test.invalid"})
		require.NoError(t, err, "the test-login domain is ordinary here")
		assert.False(t, u.IsTestFixture())
	})

	t.Run("hub proxy-auth assertion", func(t *testing.T) {
		handler := UnifiedAuthMiddleware(AuthConfig{
			Mode:                 "production",
			ProxyAuthenticator:   &fakeProxyAuthenticator{info: &ProxyUserInfo{Email: existing.Email}},
			ProxyUserProvisioner: MakeProxyUserProvisioner(srv),
		})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("handler reached") }))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("legacy trusted proxy headers", func(t *testing.T) {
		reached := false
		handler := UnifiedAuthMiddleware(AuthConfig{Mode: "production", TrustedProxies: []string{"192.0.2.1"}})(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(http.StatusOK) }))
		for _, email := range []string{existing.Email, reserved} {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			req.Header.Set("X-Forwarded-User-Id", existing.ID)
			req.Header.Set("X-Forwarded-User-Email", email)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusForbidden, rec.Code, email)
		}
		assert.False(t, reached)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("X-Forwarded-User-Id", "u1")
		req.Header.Set("X-Forwarded-User-Email", "person@scion-test.invalid")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "an ordinary email still passes")
	})

	t.Run("GSA resolver", func(t *testing.T) {
		resolver := NewGoogleIdentityResolver(newFakeUserStore(), newMemExtIDStore(), alwaysAuthorized, nil, nil)
		user, err := resolver.Resolve(ctx, &ValidatedGoogleIdentity{
			Subject: "1234", Email: reserved, EmailVerified: true, Issuer: googleIssuerHTTPS,
			IsServiceAccount: true, UpstreamExpiry: time.Now().Add(time.Hour),
		}, ResolvePolicy{PreAuthorized: true})
		assert.ErrorIs(t, err, ErrAccessDenied)
		assert.Nil(t, user)
	})

	t.Run("provision API", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"email":"`+reserved+`"}`))
		_, rerr := decodeProvisionRequest(req)
		require.NotNil(t, rerr)
		assert.Equal(t, http.StatusUnprocessableEntity, rerr.status)
		assert.Equal(t, provisionReasonReservedTestIdentity, rerr.details["reason"])
	})

	t.Run("invite and allow list", func(t *testing.T) {
		_, err := NormalizeInviteEmail(reserved)
		assert.Error(t, err)
		_, err = NormalizeInviteEmail("ok@scion-test.invalid")
		assert.NoError(t, err)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/allow-list", map[string]string{"email": reserved})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("refresh token", func(t *testing.T) {
		_, refresh, _, err := srv.userTokenService.GenerateTokenPair(existing.ID, existing.Email, "", "member", ClientTypeWeb)
		require.NoError(t, err)
		rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: refresh})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("broker on-behalf-of, flag on and off, before and after expiry", func(t *testing.T) {
		expired := tiStoreFixture(t, s, generateID(), time.Now().Add(-time.Minute))
		_, signReq := setupSignedBrokerRequest(t, srv.brokerAuthService, s)
		reached := false
		handler := BrokerAuthMiddleware(srv.brokerAuthService)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		for _, enabled := range []bool{true, false} {
			srv.testIdentities.enabled = enabled
			for _, email := range []string{existing.Email, expired.Email, reserved} {
				req := signReq(http.MethodGet, "/api/v1/projects", map[string]string{HeaderOnBehalfOf: "user:" + email})
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusForbidden, rec.Code, "enabled=%v %s: %s", enabled, email, rec.Body.String())
			}
		}
		srv.testIdentities.enabled = true
		assert.False(t, reached)
	})
}

// Web sign-in paths refuse the reserved domain: OAuth callback, fresh
// proxy identity, an existing proxy session, and session-to-bearer.
func TestTestIdentity_WebPathsRefuseReservedDomain(t *testing.T) {
	reserved := "web-" + generateID()[:6] + "@" + store.TestFixtureEmailDomain

	t.Run("OAuth callback", func(t *testing.T) {
		const secret = "test-session-secret-for-test-identity-oauth-123456"
		ws := newTestWebServer(t, WebServerConfig{SessionSecret: secret, BaseURL: "http://localhost:8080"})
		ws.oauthService = NewOAuthService(OAuthConfig{Web: OAuthClientConfig{Google: OAuthProviderConfig{ClientID: "id", ClientSecret: "secret"}}}, nil)
		ws.oauthService.httpClient = &http.Client{Transport: &mockOAuthTransport{
			tokenJSON:    `{"access_token":"mock-token","token_type":"Bearer","expires_in":3600}`,
			userinfoJSON: `{"id":"subj","email":"` + reserved + `","verified_email":true,"name":"Fixture"}`,
		}}
		st := newProxyAuthStore()
		ws.store = st
		ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
		reqSetup := httptest.NewRequest(http.MethodGet, "/auth/login/google", nil)
		recSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyOAuthState] = "ti-state"
		require.NoError(t, sess.Save(reqSetup, recSetup))
		req := httptest.NewRequest(http.MethodGet, "/auth/callback/google?code=c&state=ti-state", nil)
		for _, c := range recSetup.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusFound, rec.Code)
		assert.Contains(t, rec.Header().Get("Location"), "/login?error=")
		_, lookupErr := st.GetUserByEmail(context.Background(), reserved)
		assert.ErrorIs(t, lookupErr, store.ErrNotFound)
	})

	t.Run("fresh proxy identity", func(t *testing.T) {
		st := newProxyAuthStore()
		ws := newTestWebServer(t, WebServerConfig{AuthMode: "proxy", ProxyAuthenticator: &mockProxyAuthenticator{
			user: &ProxyUserInfo{Subject: "1", Email: reserved, Domain: store.TestFixtureEmailDomain}}})
		ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
		ws.SetStore(st)
		tokenSvc, err := NewUserTokenService(UserTokenConfig{})
		require.NoError(t, err)
		ws.SetUserTokenService(tokenSvc)
		req := httptest.NewRequest(http.MethodGet, "/projects", nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Empty(t, rec.Result().Cookies())
		_, lookupErr := st.GetUserByEmail(context.Background(), reserved)
		assert.ErrorIs(t, lookupErr, store.ErrNotFound)
	})

	t.Run("existing proxy session", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		row := &store.User{ID: "ti-web-fixture", Email: reserved, Role: "member", Status: "active", Kind: store.UserKindTestFixture, ExpiresAt: &exp}
		st := newProxyAuthStore()
		_ = st.CreateUser(context.Background(), row)
		ws := newTestWebServer(t, WebServerConfig{AuthMode: "proxy", ProxyAuthenticator: &fakeProxyAuthenticator{}})
		ws.SetAccessSettingsProvider(&staticAccessSettings{adminEmails: []string{}})
		ws.SetStore(st)
		tokenSvc, err := NewUserTokenService(UserTokenConfig{})
		require.NoError(t, err)
		ws.SetUserTokenService(tokenSvc)
		cookies := loginSession(t, ws, row.ID, row.Email, row.Role)
		req := httptest.NewRequest(http.MethodGet, "/projects", nil)
		req.Header.Set("Accept", "text/html")
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
		assert.Equal(t, "/login", rec.Header().Get("Location"))
	})

	t.Run("session to bearer", func(t *testing.T) {
		ws := newTestWebServer(t, WebServerConfig{})
		tokenSvc, err := NewUserTokenService(UserTokenConfig{})
		require.NoError(t, err)
		ws.SetUserTokenService(tokenSvc)
		var auth string
		reached := false
		ws.MountHubAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			auth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}), func(ctx context.Context) error { return nil })
		reqSetup := httptest.NewRequest(http.MethodGet, "/", nil)
		recSetup := httptest.NewRecorder()
		sess, err := ws.sessionStore.Get(reqSetup, webSessionName)
		require.NoError(t, err)
		sess.Values[sessKeyUserID] = "ti-web-fixture"
		sess.Values[sessKeyUserEmail] = reserved
		sess.Values[sessKeyUserRole] = "member"
		require.NoError(t, sess.Save(reqSetup, recSetup))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		for _, c := range recSetup.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		ws.Handler().ServeHTTP(rec, req)
		assert.True(t, reached)
		assert.Empty(t, auth, "no token minted for a reserved-domain session")
		assertSessionCookieCleared(t, rec)
	})
}

// The web OAuth and proxy paths above use an in-memory web store; this
// checks that the real store backs the same rule (a human row cannot move
// into the reserved domain).
func TestTestIdentity_StoreRefusesEmailIntoReservedDomain(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()
	u := &store.User{ID: generateID(), Email: "mover@example.com", DisplayName: "m", Role: "member", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, u))
	u.Email = "mover@" + store.TestFixtureEmailDomain
	assert.ErrorIs(t, s.UpdateUser(ctx, u), store.ErrTestFixtureKindRefused)
}
