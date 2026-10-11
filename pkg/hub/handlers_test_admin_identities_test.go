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

//go:build !no_sqlite && (!hubshard || hubshard_1)

package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin-kind test identities (ptone/scion#4240, admin tier A1). Every
// identity, user and project here is synthetic, in an in-memory store.

const (
	taPath          = "/api/v1/test-identities/admin"
	taRevokeAllPath = "/api/v1/test-identities/admin/revoke-all"
	taPurpose       = "admin tier a1 test"
)

// taServer builds a test server with the member tier and the hub-admin
// kind of the admin tier on, and a captured test admin audit sink.
func taServer(t *testing.T) (*Server, store.Store, *taSinkBuffer) {
	t.Helper()
	srv, s := newTestIdentityServer(t, true)
	enableTestAdminGateForTest(srv)
	buf := &taSinkBuffer{}
	srv.testAdminAudit = newTestAdminAuditSink(buf)
	return srv, s, buf
}

// taSinkBuffer is a goroutine-safe sink output.
type taSinkBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	err error
}

func (b *taSinkBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	return b.buf.Write(p)
}

func (b *taSinkBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// taUser creates a user holding systemRole (super-admin users get the
// admin role) and returns its row.
func taUser(t *testing.T, s store.Store, name, systemRole string) *store.User {
	t.Helper()
	id := hubConfigTokenUser(t, s, name, systemRole)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func taBody(role string) map[string]any {
	return map[string]any{"adminRole": role, "purpose": taPurpose}
}

// taPAT mints a hub-boundary access token for userID with scopes and
// returns the key and the token ID.
func taPAT(t *testing.T, srv *Server, userID string, scopes ...string) (string, string) {
	t.Helper()
	key, tok, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "ta-" + tid("tok"), Boundary: hubBoundary(), Scopes: scopes,
	})
	require.NoError(t, err, "mint hub token with %v", scopes)
	return key, tok.ID
}

func taDecode(t *testing.T, rec *httptest.ResponseRecorder) TestAdminIdentityTokenResponse {
	t.Helper()
	var resp TestAdminIdentityTokenResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp
}

// taIssue issues a hub-admin kind identity as the super-admin session u and
// returns the decoded response, requiring 201.
func taIssue(t *testing.T, srv *Server, u *store.User) TestAdminIdentityTokenResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, u, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return taDecode(t, rec)
}

func taRevokeAll(t *testing.T, srv *Server, admin *store.User) RevokeAllTestAdminIdentitiesResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, taRevokeAllPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RevokeAllTestAdminIdentitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func taReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	r, _ := body.Error.Details["reason"].(string)
	return r
}

// --- Gate -------------------------------------------------------------------

// Both routes answer 404 for every caller while the admin tier is off, even
// with the member tier on.
func TestTestAdmin_GateOff404(t *testing.T) {
	srv, s := newTestIdentityServer(t, true)
	super := taUser(t, s, "ta-off-super", store.SystemRoleSuperAdmin)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, super, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin)).Code)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, super, http.MethodPost, taRevokeAllPath, nil).Code)
	member := taUser(t, s, "ta-off-member", store.SystemRoleHubMember)
	assert.Equal(t, http.StatusNotFound, doRequestAsUser(t, srv, member, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin)).Code)
	assert.Equal(t, http.StatusNotFound, doRequestNoAuth(t, srv, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin)).Code)
}

// --- Issuance matrix (admin tier design §7 A1, DEC-3 C1-C3) ----------------

func TestTestAdmin_IssuanceMatrix(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	revoker := taUser(t, s, "ta-mx-revoker", store.SystemRoleSuperAdmin)

	// Human super-admin session: 201 for hub-admin.
	super := taUser(t, s, "ta-mx-super", store.SystemRoleSuperAdmin)
	resp := taIssue(t, srv, super)
	assert.NotEmpty(t, resp.AccessToken)
	assert.Equal(t, store.UserKindTestHubAdmin, resp.Identity.Kind)

	// super-admin: 404 until A2 and its own gate.
	rec := doRequestAsUser(t, srv, super, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// An unknown adminRole: 400.
	rec = doRequestAsUser(t, srv, super, http.MethodPost, taPath, taBody("owner"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Human hub-admin session: 403.
	hubAdmin := taUser(t, s, "ta-mx-hubadmin", store.SystemRoleHubAdmin)
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Member-tier issuer token (test_identity:issue): 403.
	memberIssuer := taUser(t, s, "ta-mx-member-pat", store.SystemRoleSuperAdmin)
	memberTok, _ := taPAT(t, srv, memberIssuer.ID, "test_identity:issue")
	rec = tiPost(t, srv, memberTok, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Super-admin's token with exactly test_identity:issue_admin: 201, and
	// the identity records the token's ID.
	patIssuer := taUser(t, s, "ta-mx-pat", store.SystemRoleSuperAdmin)
	patKey, patID := taPAT(t, srv, patIssuer.ID, uatScopeTestIdentityIssueAdmin)
	rec = tiPost(t, srv, patKey, taPath, taBody(testAdminRoleHubAdmin))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	issued, err := s.GetUser(ctx, taDecode(t, rec).Identity.ID)
	require.NoError(t, err)
	require.NotNil(t, issued.IssuerCredentialID)
	assert.Equal(t, patID, *issued.IssuerCredentialID)
	taRevokeAll(t, srv, revoker)

	// An extra scope: 403.
	extraIssuer := taUser(t, s, "ta-mx-extra", store.SystemRoleSuperAdmin)
	extraKey, _ := taPAT(t, srv, extraIssuer.ID, uatScopeTestIdentityIssueAdmin, "hub_health:read")
	rec = tiPost(t, srv, extraKey, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonIssuerPAT, taReason(t, rec))

	// Older than 8 h: 403.
	oldIssuer := taUser(t, s, "ta-mx-old", store.SystemRoleSuperAdmin)
	oldKey, _ := taPAT(t, srv, oldIssuer.ID, uatScopeTestIdentityIssueAdmin)
	srv.testIdentities.now = func() time.Time { return time.Now().Add(8*time.Hour + time.Minute) }
	rec = tiPost(t, srv, oldKey, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonIssuerPAT, taReason(t, rec))
	srv.testIdentities.now = time.Now

	// Revoked between two mints: the second is refused. The JWT-less
	// access token is refused by the auth middleware (401) before the
	// issuance handler's own load would refuse it (403, below).
	revIssuer := taUser(t, s, "ta-mx-rev", store.SystemRoleSuperAdmin)
	revKey, revID := taPAT(t, srv, revIssuer.ID, uatScopeTestIdentityIssueAdmin)
	rec = tiPost(t, srv, revKey, taPath, taBody(testAdminRoleHubAdmin))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	taRevokeAll(t, srv, revoker)
	require.NoError(t, s.RevokeUserAccessToken(ctx, revID))
	rec = tiPost(t, srv, revKey, taPath, taBody(testAdminRoleHubAdmin))
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())
	assert.ErrorIs(t, srv.verifyTestAdminIssuerPAT(ctx, revIssuer.ID, revID, uatScopeTestIdentityIssueAdmin), errTestAdminIssuerPAT,
		"C3: the live load refuses a revoked token")

	// C1: the issuer's super-admin binding deleted between two mints: the
	// second is refused, for a token and for a session, although the
	// session's role claim still says admin.
	for _, viaToken := range []bool{true, false} {
		name := "ta-mx-c1-session"
		if viaToken {
			name = "ta-mx-c1-token"
		}
		issuer := taUser(t, s, name, store.SystemRoleSuperAdmin)
		key, _ := taPAT(t, srv, issuer.ID, uatScopeTestIdentityIssueAdmin)
		mint := func() *httptest.ResponseRecorder {
			if viaToken {
				return tiPost(t, srv, key, taPath, taBody(testAdminRoleHubAdmin))
			}
			return doRequestAsUser(t, srv, issuer, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
		}
		rec = mint()
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		taRevokeAll(t, srv, revoker)
		taDeleteSystemRole(t, s, issuer.ID, store.SystemRoleSuperAdmin)
		rec = mint()
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
	}

	// A test identity as caller: 403 (admin kind and member kind).
	callerIssuer := taUser(t, s, "ta-mx-caller", store.SystemRoleSuperAdmin)
	adminKind := taIssue(t, srv, callerIssuer)
	rec = tiPost(t, srv, adminKind.AccessToken, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = tiPost(t, srv, adminKind.AccessToken, "/api/v1/test-identities", map[string]string{"role": "member"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "an admin kind does not issue member identities: %s", rec.Body.String())
	fixture := tiStoreFixture(t, s, callerIssuer.ID, time.Now().Add(time.Hour))
	rec = tiPost(t, srv, tiTokenFor(t, srv, fixture), taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// taDeleteSystemRole deletes userID's system bindings of roleName.
func taDeleteSystemRole(t *testing.T, s store.Store, userID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	n := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			n++
		}
	}
	require.Positive(t, n)
}

// The issuer access-token checks, one by one (DEC-3 C2).
func TestTestAdmin_IssuerPATChecks(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "ta-pat-issuer", store.SystemRoleSuperAdmin)
	other := taUser(t, s, "ta-pat-other", store.SystemRoleSuperAdmin)
	_, good := taPAT(t, srv, issuer.ID, uatScopeTestIdentityIssueAdmin)
	_, wide := taPAT(t, srv, issuer.ID, uatScopeTestIdentityIssueAdmin, "hub_health:read")
	_, wrong := taPAT(t, srv, issuer.ID, "test_identity:issue")
	scope := uatScopeTestIdentityIssueAdmin

	require.NoError(t, srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, good, scope))
	for name, err := range map[string]error{
		"empty id":        srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, "", scope),
		"unknown id":      srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, generateID(), scope),
		"other owner":     srv.verifyTestAdminIssuerPAT(ctx, other.ID, good, scope),
		"extra scope":     srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, wide, scope),
		"wrong scope":     srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, wrong, scope),
		"super-admin cap": srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, good, uatScopeTestIdentityIssueSuperAdmin),
	} {
		assert.ErrorIs(t, err, errTestAdminIssuerPAT, name)
	}
	srv.testIdentities.now = func() time.Time { return time.Now().Add(9 * time.Hour) }
	assert.ErrorIs(t, srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, good, scope), errTestAdminIssuerPAT, "older than 8 h")
	srv.testIdentities.now = time.Now
	require.NoError(t, s.RevokeUserAccessToken(ctx, good))
	assert.ErrorIs(t, srv.verifyTestAdminIssuerPAT(ctx, issuer.ID, good, scope), errTestAdminIssuerPAT, "revoked")
}

// Request validation: purpose, lifetime and token TTL bounds, unknown
// fields.
func TestTestAdmin_RequestValidation(t *testing.T) {
	srv, s, _ := taServer(t)
	super := taUser(t, s, "ta-val-super", store.SystemRoleSuperAdmin)
	for name, body := range map[string]map[string]any{
		"no purpose":      {"adminRole": "hub-admin"},
		"short purpose":   {"adminRole": "hub-admin", "purpose": "short"},
		"long purpose":    {"adminRole": "hub-admin", "purpose": strings.Repeat("p", 201)},
		"lifetime > cap":  {"adminRole": "hub-admin", "purpose": taPurpose, "lifetimeSeconds": 3601},
		"lifetime < 60":   {"adminRole": "hub-admin", "purpose": taPurpose, "lifetimeSeconds": 30},
		"token ttl > cap": {"adminRole": "hub-admin", "purpose": taPurpose, "tokenTtlSeconds": 1801},
		"unknown field":   {"adminRole": "hub-admin", "purpose": taPurpose, "email": "x@example.com"},
		"kind field":      {"adminRole": "hub-admin", "purpose": taPurpose, "kind": "test_super_admin"},
	} {
		rec := doRequestAsUser(t, srv, super, http.MethodPost, taPath, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

// --- Row, binding, audit (C4), sink -------------------------------------------

func TestTestAdmin_IssuedRowBindingAndAudit(t *testing.T) {
	srv, s, sink := taServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "ta-row-issuer", store.SystemRoleSuperAdmin)
	key, patID := taPAT(t, srv, issuer.ID, uatScopeTestIdentityIssueAdmin)
	rec := tiPost(t, srv, key, taPath, map[string]any{"adminRole": "hub-admin", "purpose": taPurpose, "lifetimeSeconds": 1800})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := taDecode(t, rec)
	assert.Equal(t, "Bearer", resp.TokenType)
	assert.Equal(t, testHubAdminDefaultTokenTTL, jwtLifetime(t, resp.AccessToken))
	assert.NotContains(t, rec.Body.String(), "refresh")

	u, err := s.GetUser(ctx, resp.Identity.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserKindTestHubAdmin, u.Kind)
	assert.Equal(t, store.UserRoleMember, u.Role)
	assert.True(t, strings.HasSuffix(u.Email, "@"+store.TestHubAdminEmailDomain), u.Email)
	require.NotNil(t, u.IssuanceID)
	assert.Equal(t, resp.Identity.IssuanceID, *u.IssuanceID)
	require.NotNil(t, u.IssuedBy)
	assert.Equal(t, issuer.ID, *u.IssuedBy)
	require.NotNil(t, u.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(30*time.Minute), *u.ExpiresAt, time.Minute)

	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
	require.NoError(t, err)
	var issuance *store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem {
			require.Nil(t, issuance, "exactly one system binding")
			issuance = b
		}
	}
	require.NotNil(t, issuance)
	assert.Equal(t, hubAdminRD.ID, issuance.RoleDefinitionID)
	assert.Equal(t, store.TestIdentityIssuanceCreatedBy, issuance.CreatedBy)
	require.NotNil(t, issuance.ExpiresAt)
	assert.True(t, issuance.ExpiresAt.Equal(*u.ExpiresAt), "binding expires_at = identity expiry")

	// C4: the audit records the issuer's credential ID, never the token.
	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminIssueMutation, TargetID: u.ID})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, issuer.ID, a.ActorPrincipalID)
	assert.Equal(t, patID, a.ActorCredentialID)
	var summary map[string]any
	require.NoError(t, json.Unmarshal([]byte(a.AfterSummary), &summary))
	assert.Equal(t, patID, summary["issuer_credential_id"])
	assert.Equal(t, store.UserKindTestHubAdmin, summary["kind"])
	assert.Equal(t, *u.IssuanceID, summary["issuance_id"])
	assert.NotContains(t, a.AfterSummary, resp.AccessToken)

	// The sink line: one issuance event, no token.
	lines := sink.String()
	assert.Contains(t, lines, `"event":"test_admin_issue"`)
	assert.Contains(t, lines, *u.IssuanceID)
	assert.NotContains(t, lines, resp.AccessToken)

	// A mutation by the identity carries the admin-kind labels and the
	// issuance correlation.
	gid := "ta-row-group-" + generateID()[:8]
	rec = tiPost(t, srv, resp.AccessToken, "/api/v1/groups", map[string]any{"name": gid, "slug": gid})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
	var byActor []*store.MutationAuditRecord
	assert.Eventually(t, func() bool {
		byActor, _, err = s.ListMutationAudits(ctx, store.MutationAuditFilter{ActorPrincipalID: u.ID})
		return err == nil && len(byActor) > 0
	}, 5*time.Second, 20*time.Millisecond, "a mutation under the admin-kind identity is audited")
	// The same labels straight from the context the row block builds.
	direct := auditActorFromContext(contextWithTestAdminActor(ctx, u))
	byActor = append(byActor, &store.MutationAuditRecord{CredentialLabels: direct.CredentialLabels, CorrelationID: direct.CorrelationID})
	for _, r := range byActor {
		var labels map[string]string
		require.NoError(t, json.Unmarshal([]byte(r.CredentialLabels), &labels), r.CredentialLabels)
		assert.Equal(t, "admin", labels["test_tier"])
		assert.Equal(t, store.UserKindTestHubAdmin, labels["kind"])
		assert.Equal(t, issuer.ID, labels["issuer_id"])
		assert.Equal(t, patID, labels["issuer_credential_id"])
		assert.Equal(t, *u.IssuanceID, labels["issuance_id"])
		assert.Equal(t, testAdminPurposeHash(taPurpose), labels["purpose_hash"])
		assert.True(t, strings.HasPrefix(r.CorrelationID, *u.IssuanceID+":"), r.CorrelationID)
	}

	// No re-issue: 409 for the issuer.
	rec = tiPost(t, srv, key, "/api/v1/test-identities/"+u.ID+"/token", nil)
	assert.NotEqual(t, http.StatusOK, rec.Code)
	issuerUser, err := s.GetUser(ctx, issuer.ID)
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, issuerUser, http.MethodPost, "/api/v1/test-identities/"+u.ID+"/token", nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// The sink write failing refuses issuance (503) and writes nothing.
func TestTestAdmin_SinkFailureRefusesIssuance(t *testing.T) {
	srv, s, sink := taServer(t)
	super := taUser(t, s, "ta-sink-super", store.SystemRoleSuperAdmin)
	sink.err = errors.New("synthetic sink failure")
	rec := doRequestAsUser(t, srv, super, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	n, err := s.CountLiveTestAdminUsers(context.Background(), "", "", time.Now())
	require.NoError(t, err)
	assert.Zero(t, n, "nothing is issued when the sink line cannot be written")
}

// --- Fidelity ---------------------------------------------------------------

// A test_hub_admin passes IsHubAdmin, not IsSystemAdmin, and gets the same
// status as a human hub-admin on a sample of hub-admin and super-admin
// routes.
func TestTestAdmin_FidelityMatchesHumanHubAdmin(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	super := taUser(t, s, "ta-fid-super", store.SystemRoleSuperAdmin)
	resp := taIssue(t, srv, super)
	id := resp.Identity.ID
	assert.True(t, srv.authzService.IsHubAdmin(ctx, id))
	assert.False(t, srv.authzService.IsSystemAdmin(ctx, id))

	human := taUser(t, s, "ta-fid-human", store.SystemRoleHubAdmin)
	target := taUser(t, s, "ta-fid-target", store.SystemRoleHubMember)
	humanTok := tiTokenFor(t, srv, human)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/users/" + target.ID},
		{http.MethodGet, "/api/v1/admin/roles"},
		{http.MethodGet, "/api/v1/admin/role-bindings"},
		{http.MethodGet, "/api/v1/groups"},
		{http.MethodGet, "/api/v1/projects"},
		{http.MethodGet, "/api/v1/admin/health/summary"},
		{http.MethodGet, "/api/v1/admin/server-config"},
		{http.MethodGet, "/api/v1/admin/maintenance/operations"},
		{http.MethodGet, "/api/v1/test-identities"},
		{http.MethodDelete, "/api/v1/users/" + target.ID},
	}
	for _, r := range routes {
		want := doRequestWithToken(t, srv, humanTok, r.method, r.path, nil).Code
		got := doRequestWithToken(t, srv, resp.AccessToken, r.method, r.path, nil).Code
		assert.Equal(t, want, got, "%s %s: test_hub_admin %d, human hub-admin %d", r.method, r.path, got, want)
	}
	// Last-admin finding: neither can suspend or delete a human super-admin.
	for _, tok := range []string{humanTok, resp.AccessToken} {
		rec := doRequestWithToken(t, srv, tok, http.MethodPatch, "/api/v1/users/"+super.ID, map[string]any{"status": "suspended"})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/users/"+super.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
}

// --- Revocation (§3.5, §4.7) --------------------------------------------------

func TestTestAdmin_Revocation(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		srv, s, _ := taServer(t)
		resp := taIssue(t, srv, taUser(t, s, "ta-rev-exp", store.SystemRoleSuperAdmin))
		require.Equal(t, http.StatusOK, tiAuthMe(t, srv, resp.AccessToken).Code)
		srv.testIdentities.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		assert.Equal(t, http.StatusUnauthorized, tiAuthMe(t, srv, resp.AccessToken).Code)
		assert.False(t, srv.authzService.IsHubAdmin(context.Background(), resp.Identity.ID), "decide-time: the identity has expired")
	})
	t.Run("suspend", func(t *testing.T) {
		srv, s, _ := taServer(t)
		resp := taIssue(t, srv, taUser(t, s, "ta-rev-susp", store.SystemRoleSuperAdmin))
		u, err := s.GetUser(context.Background(), resp.Identity.ID)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, s.UpdateUser(context.Background(), u))
		assert.Equal(t, http.StatusForbidden, tiAuthMe(t, srv, resp.AccessToken).Code)
	})
	t.Run("delete", func(t *testing.T) {
		srv, s, _ := taServer(t)
		resp := taIssue(t, srv, taUser(t, s, "ta-rev-del", store.SystemRoleSuperAdmin))
		ctx := context.Background()
		require.NoError(t, s.WithTx(ctx, func(tx store.Store) error {
			return deleteUserRowCascadeTx(ctx, tx, resp.Identity.ID, time.Now())
		}))
		assert.Equal(t, http.StatusUnauthorized, tiAuthMe(t, srv, resp.AccessToken).Code)
	})
	t.Run("restart without the flag", func(t *testing.T) {
		srv, s, _ := taServer(t)
		resp := taIssue(t, srv, taUser(t, s, "ta-rev-restart", store.SystemRoleSuperAdmin))
		bypass := taBypassRowBlock(t, srv, resp.Identity.ID, resp.Identity.Email)
		require.Equal(t, http.StatusOK, bypass("/api/v1/admin/roles"), "the identity holds hub-admin authority while the gate is on")
		// A restart without the flag: the gate is captured off.
		srv.testAdminGate = testAdminGate{}
		assert.Equal(t, http.StatusUnauthorized, tiAuthMe(t, srv, resp.AccessToken).Code)
		assert.Equal(t, http.StatusForbidden, bypass("/api/v1/admin/roles"), "decide grants nothing with the row block bypassed")
		assert.False(t, srv.authzService.IsHubAdmin(context.Background(), resp.Identity.ID))
		perms, err := srv.authzService.getEffectivePermissions(context.Background(), store.RoleBindingPrincipalUser, resp.Identity.ID, "", "")
		require.NoError(t, err)
		assert.Empty(t, perms)
	})
	t.Run("role claim differs from the kind", func(t *testing.T) {
		srv, s, _ := taServer(t)
		resp := taIssue(t, srv, taUser(t, s, "ta-rev-claim", store.SystemRoleSuperAdmin))
		u, err := s.GetUser(context.Background(), resp.Identity.ID)
		require.NoError(t, err)
		forged, _, err := srv.userTokenService.GenerateAccessTokenWithTTL(u.ID, u.Email, u.DisplayName, store.UserRoleAdmin, ClientTypeAPI, 5*time.Minute)
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, tiAuthMe(t, srv, forged).Code)
		rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: forged})
		require.Equal(t, http.StatusOK, rec.Code)
		var v AuthValidateResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
		assert.False(t, v.Valid)
	})
}

// taBypassRowBlock returns a GET runner that serves the mux as the user
// with id, without the auth middleware (and so without the JWT row block).
func taBypassRowBlock(t *testing.T, srv *Server, id, email string) func(path string) int {
	t.Helper()
	return func(path string) int {
		user := NewAuthenticatedUser(id, email, "bypass", store.UserRoleMember, string(ClientTypeAPI))
		ctx := context.WithValue(context.Background(), userContextKey{}, user)
		ctx = contextWithIdentity(ctx, user)
		ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(user))
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		return rec.Code
	}
}

// --- Revoke-all (§3.3) --------------------------------------------------------

func TestTestAdmin_RevokeAll(t *testing.T) {
	srv, s, sink := taServer(t)
	ctx := context.Background()
	revoker := taUser(t, s, "ta-ra-revoker", store.SystemRoleSuperAdmin)
	a := taIssue(t, srv, taUser(t, s, "ta-ra-a", store.SystemRoleSuperAdmin))
	b := taIssue(t, srv, taUser(t, s, "ta-ra-b", store.SystemRoleSuperAdmin))

	// Refused for: the admin-kind identity itself, a human hub-admin, a
	// super-admin's access token (session only), and a member fixture.
	rec := tiPost(t, srv, a.AccessToken, taRevokeAllPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, taUser(t, s, "ta-ra-hubadmin", store.SystemRoleHubAdmin), http.MethodPost, taRevokeAllPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	patKey, _ := taPAT(t, srv, revoker.ID, uatScopeTestIdentityIssueAdmin)
	rec = tiPost(t, srv, patKey, taRevokeAllPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	fixture := tiStoreFixture(t, s, revoker.ID, time.Now().Add(time.Hour))
	rec = tiPost(t, srv, tiTokenFor(t, srv, fixture), taRevokeAllPath, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// One identity is a project's last owner: it is suspended and loses
	// its bindings instead of being deleted.
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	projectID := tid("ta-ra-project")
	taProject(t, s, projectID, revoker.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: b.Identity.ID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	taDeleteProjectOwner(t, s, projectID, revoker.ID, ownerRD.ID)

	resp := taRevokeAll(t, srv, revoker)
	require.Equal(t, 2, resp.Count)
	outcomes := map[string]string{}
	for _, r := range resp.Revoked {
		outcomes[r.ID] = r.Outcome
		assert.Equal(t, store.UserKindTestHubAdmin, r.Kind)
	}
	assert.Equal(t, "deleted", outcomes[a.Identity.ID])
	assert.Equal(t, "suspended", outcomes[b.Identity.ID])

	_, err = s.GetUser(ctx, a.Identity.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	bu, err := s.GetUser(ctx, b.Identity.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserStatusSuspended, bu.Status)
	left, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, b.Identity.ID)
	require.NoError(t, err)
	assert.Empty(t, left)
	assert.Equal(t, http.StatusUnauthorized, tiAuthMe(t, srv, a.AccessToken).Code)
	assert.Equal(t, http.StatusForbidden, tiAuthMe(t, srv, b.AccessToken).Code)

	audits, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: testAdminRevokeMutation})
	require.NoError(t, err)
	assert.Len(t, audits, 2)
	assert.Equal(t, 2, strings.Count(sink.String(), `"event":"test_admin_revoke"`))

	// The fixture is untouched; a second revoke-all finds the suspended
	// identity only.
	_, err = s.GetUser(ctx, fixture.ID)
	require.NoError(t, err)
	n, err := s.CountLiveTestAdminUsers(ctx, "", "", time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the suspended identity's row remains until it expires or is purged")
}

// taDeleteProjectOwner removes userID's owner binding on projectID so
// another principal is the last owner.
func taDeleteProjectOwner(t *testing.T, s store.Store, projectID, userID, ownerRDID string) {
	t.Helper()
	ctx := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID && b.RoleDefinitionID == ownerRDID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}
}

// --- Kind discipline (§4.4) ---------------------------------------------------

func TestTestAdmin_KindDiscipline(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	super := taUser(t, s, "ta-kd-super", store.SystemRoleSuperAdmin)
	resp := taIssue(t, srv, super)
	adminID := resp.Identity.ID

	// kindForPrincipal fails closed on a read error and on an unknown kind.
	_, err := kindForPrincipalVia(ctx, kindErrStore{Store: s}, nil, adminID)
	require.Error(t, err)
	_, err = classifyUserKind(&store.User{ID: "x", Kind: "bogus"})
	require.ErrorIs(t, err, errUnknownPrincipalKind)
	k, err := kindForPrincipalVia(ctx, s, nil, generateID())
	require.NoError(t, err)
	assert.False(t, k.isHuman(), "a missing user is never human")
	_, reason, err := testAdminRowRejection(ctx, func(context.Context, string) (principalKind, error) {
		return principalKind{}, errors.New("synthetic kind read failure")
	}, func(string) bool { return true }, &store.User{ID: adminID}, store.UserRoleMember, time.Now())
	require.Error(t, err, "the row block fails closed on a kind read error")
	assert.Empty(t, reason)
	errAuthz := NewAuthzService(kindErrStore{Store: s}, srv.authzService.logger)
	d := errAuthz.CanDelegate(ctx, NewAuthenticatedUser(super.ID, super.Email, "s", store.UserRoleAdmin, "web"),
		GrantDescriptor{Type: GrantTypeRoleBinding, ScopeType: store.RoleScopeProject, ScopeID: "p", GranteeType: store.RoleBindingPrincipalUser, GranteeID: adminID})
	assert.False(t, d.Allowed, "CanDelegate denies when the grantee's kind cannot be read")

	// Role change on an admin-kind target: refused (409, as for fixtures).
	rec := doRequestAsUser(t, srv, super, http.MethodPatch, "/api/v1/users/"+adminID, map[string]any{"role": "admin"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, "test_identity_role_fixed", taReason(t, rec))

	// A human super-admin granting any binding to an admin-kind row:
	// system scope 422, project scope 403 (CanDelegate, before the
	// super-admin shortcut).
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, super, http.MethodPost, "/api/v1/admin/role-bindings", map[string]any{
		"roleDefinitionId": hubAdminRD.ID, "principalType": "user", "principalId": adminID, "scopeType": "system"})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	projectID := tid("ta-kd-project")
	taProject(t, s, projectID, super.ID)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	superIdent := NewAuthenticatedUser(super.ID, super.Email, "s", store.UserRoleAdmin, "web")
	for name, g := range map[string]GrantDescriptor{
		"project binding":    {Type: GrantTypeRoleBinding, RoleDefinitionID: memberRD.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, GranteeType: "user", GranteeID: adminID},
		"by email":           {Type: GrantTypeRoleBinding, RoleDefinitionID: memberRD.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, GranteeType: "user", GranteeID: resp.Identity.Email},
		"group membership":   {Type: GrantTypeGroupMembership, GroupID: generateID(), GranteeType: "user", GranteeID: adminID},
		"project membership": {Type: GrantTypeProjectMembership, ScopeType: store.RoleScopeProject, ScopeID: projectID, GranteeType: "user", GranteeID: adminID},
	} {
		d := srv.authzService.CanDelegate(ctx, superIdent, g)
		assert.False(t, d.Allowed, "%s: %s", name, d.Reason)
	}
	// The same grant to a human passes the grantee check.
	human := taUser(t, s, "ta-kd-human", store.SystemRoleHubMember)
	d = srv.authzService.CanDelegate(ctx, superIdent, GrantDescriptor{Type: GrantTypeRoleBinding, RoleDefinitionID: memberRD.ID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, GranteeType: "user", GranteeID: human.ID})
	assert.True(t, d.Allowed, d.Reason)
	rec = doRequestAsUser(t, srv, super, http.MethodPost, "/api/v1/admin/role-bindings", map[string]any{
		"roleDefinitionId": memberRD.ID, "principalType": "user", "principalId": adminID, "scopeType": "project", "scopeId": projectID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Access tokens: an admin-kind identity creates none.
	_, _, err = srv.uatService.CreateTokenWithParams(rs4MintContext(adminID), CreateTokenParams{
		UserID: adminID, Name: "ta-kd-pat", Boundary: hubBoundary(), Scopes: []string{"hub_health:read"}})
	assert.ErrorIs(t, err, ErrUATTestIdentityDenied)

	// Both admin domains are refused at test-login style creation paths
	// (the general CreateUser) and are not mistaken for scion-test.invalid.
	assert.True(t, isReservedTestIdentityEmail("x@"+store.TestHubAdminEmailDomain))
	assert.True(t, isReservedTestIdentityEmail("x@"+store.TestSuperAdminEmailDomain))
	assert.False(t, isReservedTestIdentityEmail("x@scion-test.invalid"))
}

// Reconcile leaves a live test_super_admin untouched with a non-empty
// admin_emails, and does not count it toward the intended admin set.
func TestTestAdmin_ReconcileSkipsTestKinds(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	superKind := taRawTestSuperAdmin(t, s)
	issuer := taUser(t, s, "ta-rc-issuer", store.SystemRoleSuperAdmin)
	hubKind := taIssue(t, srv, issuer)

	// admin_emails names a real human admin, so demotions are enabled:
	// every other role=admin user not in the list is demoted, except the
	// test super-admin, which reconcile skips.
	_, err := ReconcileSuperAdminBindings(ctx, s, []string{issuer.Email}, "member")
	require.NoError(t, err)
	got, err := s.GetUser(ctx, superKind)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleAdmin, got.Role, "not demoted")
	assert.True(t, taHasSystemRole(t, s, superKind, store.SystemRoleSuperAdmin), "its super-admin binding stays")

	// admin_emails lists the test identities themselves: never promoted.
	hubRow, err := s.GetUser(ctx, hubKind.Identity.ID)
	require.NoError(t, err)
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{hubRow.Email, got.Email}, "member")
	require.NoError(t, err)
	hubRow, err = s.GetUser(ctx, hubKind.Identity.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleMember, hubRow.Role)
	assert.False(t, taHasSystemRole(t, s, hubRow.ID, store.SystemRoleSuperAdmin))
	for _, id := range []string{superKind, hubRow.ID} {
		assert.True(t, reconcileSkipsUser(ctx, s, id))
	}
	assert.True(t, reconcileSkipsUser(ctx, kindErrStore{Store: s}, hubRow.ID), "a kind read error leaves the user untouched")
}

func taHasSystemRole(t *testing.T, s store.Store, userID, roleName string) bool {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			return true
		}
	}
	return false
}

// taRawTestSuperAdmin writes a live test_super_admin row and its expiring
// super-admin binding directly in the database: A1 refuses to issue that
// kind, but the tests of kind-driven code (reconcile, the clamps) need one.
func taRawTestSuperAdmin(t *testing.T, s store.Store) string {
	t.Helper()
	ctx := context.Background()
	raw, ok := s.(interface {
		DB() *sql.DB
		Dialect() string
	})
	require.True(t, ok, "the test store exposes its database")
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	id := generateID()
	exp := time.Now().Add(30 * time.Minute).UTC()
	now := time.Now().UTC()
	q := "INSERT INTO users (id, email, display_name, role, status, created, session_generation, kind, expires_at, issued_by, issuance_id) VALUES (?, ?, 'raw super', 'admin', 'active', ?, 0, 'test_super_admin', ?, ?, ?)"
	_, err = raw.DB().ExecContext(ctx, taRebind(raw.Dialect(), q), id, "test-admin-"+id[:8]+"@"+store.TestSuperAdminEmailDomain, now, exp, generateID(), generateID())
	require.NoError(t, err)
	q = "INSERT INTO role_bindings (id, principal_type, principal_id, scope_type, scope_id, expires_at, created_by, created, role_definition_id) VALUES (?, 'user', ?, 'system', '', ?, ?, ?, ?)"
	_, err = raw.DB().ExecContext(ctx, taRebind(raw.Dialect(), q), generateID(), id, exp, store.TestIdentityIssuanceCreatedBy, now, rd.ID)
	require.NoError(t, err)
	return id
}

// taRebind rewrites ? placeholders as $n for Postgres.
func taRebind(dialect, q string) string {
	if dialect != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- Member clamp table (§4.4) -------------------------------------------------

// The member clamp keys strictly on test_fixture; the admin kinds get
// exactly their role's authority, and humans are unchanged.
func TestTestAdmin_MemberClampTable(t *testing.T) {
	srv, s, _ := taServer(t)
	srv.testAdminGate = testAdminGate{hubAdmin: true, superAdmin: true, hubID: tiaHubID, hubClass: config.HubClassTest}
	ctx := context.Background()
	perms := func(id string) []string {
		p, err := srv.authzService.getEffectivePermissions(ctx, store.RoleBindingPrincipalUser, id, "", "")
		require.NoError(t, err)
		sort.Strings(p)
		return p
	}
	super := taUser(t, s, "ta-ct-super", store.SystemRoleSuperAdmin)
	humanHubAdmin := taUser(t, s, "ta-ct-hubadmin", store.SystemRoleHubAdmin)
	humanMember := taUser(t, s, "ta-ct-member", store.SystemRoleHubMember)

	// A fixture in a hub-admin group keeps the member set.
	group := tiGroupWithSystemRole(t, s, "ta-ct-hubadmin-group", store.SystemRoleHubAdmin, DevUserID)
	fixture := tiStoreFixture(t, s, super.ID, time.Now().Add(time.Hour))
	tiAddToGroup(t, s, group.ID, fixture.ID, store.GroupMemberRoleMember)
	// A human in the same group gains hub-admin.
	humanInGroup := taUser(t, s, "ta-ct-human-group", store.SystemRoleHubMember)
	tiAddToGroup(t, s, group.ID, humanInGroup.ID, store.GroupMemberRoleMember)

	hubKind := taIssue(t, srv, super)
	superKind := taRawTestSuperAdmin(t, s)
	ensureHubMembership(ctx, s, superKind)

	assert.Equal(t, perms(humanMember.ID), perms(fixture.ID), "a fixture in a hub-admin group keeps the member set")
	assert.Equal(t, perms(humanHubAdmin.ID), perms(hubKind.Identity.ID), "test_hub_admin = hub-admin")
	assert.Equal(t, perms(super.ID), perms(superKind), "test_super_admin = super-admin")
	assert.Equal(t, perms(humanHubAdmin.ID), perms(humanInGroup.ID), "a human in the hub-admin group = hub-admin (unchanged)")
	assert.True(t, srv.authzService.IsSystemAdmin(ctx, superKind))
	assert.False(t, srv.authzService.IsSystemAdmin(ctx, fixture.ID))
}

// --- Sibling clamp (DEC-4 C-b) --------------------------------------------------

func TestTestAdmin_SiblingClamp(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	resp := taIssue(t, srv, taUser(t, s, "ta-sc-super", store.SystemRoleSuperAdmin))
	id := resp.Identity.ID
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	exp := *u.ExpiresAt
	later, earlier := exp.Add(time.Minute), exp.Add(-time.Minute)
	user := store.PrincipalRef{Type: store.RoleBindingPrincipalUser, ID: id}
	sys := func(principal store.PrincipalRef, e *time.Time) *store.RoleBinding {
		return &store.RoleBinding{ID: generateID(), PrincipalType: principal.Type, PrincipalID: principal.ID, ScopeType: store.RoleScopeSystem, ExpiresAt: e}
	}
	group := store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: generateID()}
	issuance := sys(user, &exp)
	noExpiry := sys(user, nil)
	outlives := sys(user, &later)
	short := sys(user, &earlier)
	groupSys := sys(group, nil)
	project := &store.RoleBinding{ID: generateID(), PrincipalType: user.Type, PrincipalID: id, ScopeType: store.RoleScopeProject, ScopeID: "p"}
	all := []*store.RoleBinding{issuance, noExpiry, outlives, short, groupSys, project}

	c := srv.authzService.store.(*testFixtureGrantClamp)
	got, err := c.clampTestAdmin(ctx, []store.PrincipalRef{user, group}, all)
	require.NoError(t, err)
	assert.ElementsMatch(t, []*store.RoleBinding{issuance, short, groupSys, project}, got,
		"direct system bindings must expire no later than the identity")

	// Tier off, or the identity expired: no authority at all.
	srv.testAdminGate = testAdminGate{}
	got, err = c.clampTestAdmin(ctx, []store.PrincipalRef{user, group}, all)
	require.NoError(t, err)
	assert.Empty(t, got)
	enableTestAdminGateForTest(srv)
	srv.testIdentities.now = func() time.Time { return exp.Add(time.Second) }
	got, err = c.clampTestAdmin(ctx, []store.PrincipalRef{user}, all)
	require.NoError(t, err)
	assert.Empty(t, got)
	srv.testIdentities.now = time.Now

	// A human's list passes through.
	human := taUser(t, s, "ta-sc-human", store.SystemRoleHubMember)
	hp := store.PrincipalRef{Type: store.RoleBindingPrincipalUser, ID: human.ID}
	hb := []*store.RoleBinding{sys(hp, nil), sys(group, nil)}
	got, err = c.clampTestAdmin(ctx, []store.PrincipalRef{hp, group}, hb)
	require.NoError(t, err)
	assert.Equal(t, hb, got)

	// An unreadable kind fails closed.
	errClamp := &testFixtureGrantClamp{Store: kindErrStore{Store: s}, cache: &testFixtureClampCache{}}
	_, err = errClamp.clampTestAdmin(ctx, []store.PrincipalRef{hp}, hb)
	require.Error(t, err)

	// Outside the authorization service: unwired means off.
	got, err = clampTestFixtureBindings(ctx, s, []store.PrincipalRef{user}, []*store.RoleBinding{issuance})
	require.NoError(t, err)
	assert.Empty(t, got, "no admin tier wiring: the admin kind has no authority")
	got, err = clampTestIdentityBindings(ctx, s, srv.authzService, []store.PrincipalRef{user}, []*store.RoleBinding{issuance})
	require.NoError(t, err)
	assert.Len(t, got, 1, "with the server's wiring the issuance binding stays")
	got, err = clampBindingsForUser(ctx, s, srv.authzService, id, []*store.RoleBinding{groupSys})
	require.NoError(t, err)
	assert.Len(t, got, 1, "a group-only read is evaluated with its member user")
}

// --- 3a provenance (design §4.6) and health count ------------------------------

func TestTestAdmin_ProvenanceRootAndHealthCount(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "ta-pv-issuer", store.SystemRoleSuperAdmin)
	resp := taIssue(t, srv, issuer)

	stamp, err := resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: fixturePrincipalUser, ID: resp.Identity.ID, Email: resp.Identity.Email})
	require.NoError(t, err)
	assert.Equal(t, store.FixtureProvenance{RootID: resp.Identity.ID, IssuerID: issuer.ID}, stamp)
	superKind := taRawTestSuperAdmin(t, s)
	stamp, err = resolveFixtureRoot(ctx, s, fixturePrincipal{Kind: fixturePrincipalUser, ID: superKind})
	require.NoError(t, err)
	assert.Equal(t, superKind, stamp.RootID)
	_, err = resolveFixtureRoot(ctx, kindErrStore{Store: s}, fixturePrincipal{Kind: fixturePrincipalUser, ID: resp.Identity.ID})
	require.Error(t, err)

	// A group created by the identity carries the stamp.
	gid := "ta-pv-group-" + generateID()[:8]
	rec := tiPost(t, srv, resp.AccessToken, "/api/v1/groups", map[string]any{"name": gid, "slug": gid})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, rec.Code, rec.Body.String())
	g, err := s.GetGroupBySlug(ctx, gid)
	require.NoError(t, err)
	assert.Equal(t, resp.Identity.ID, g.Fixture.RootID)
	assert.Equal(t, issuer.ID, g.Fixture.IssuerID)

	// The health summary counts live admin-kind identities (the raw super
	// kind and the issued hub kind).
	n, err := srv.liveTestAdminIdentityCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	srv.testIdentities.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	n, err = srv.liveTestAdminIdentityCount(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "expired identities are not live")
}

// --- Caps (§4.3, T5) ------------------------------------------------------------

func TestTestAdmin_CapsAtomic(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()

	// One issuer, parallel requests: exactly one.
	issuer := taUser(t, s, "ta-cap-issuer", store.SystemRoleSuperAdmin)
	codes := make([]int, 5)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doRequestAsUser(t, srv, issuer, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin)).Code
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
	assert.Equal(t, testAdminPerIssuerCap, created)

	// Several issuers, parallel: the hub holds exactly two.
	issuers := make([]*store.User, 4)
	for i := range issuers {
		issuers[i] = taUser(t, s, "ta-cap-hub-"+string(rune('a'+i)), store.SystemRoleSuperAdmin)
	}
	codes = make([]int, len(issuers))
	for i := range issuers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doRequestAsUser(t, srv, issuers[i], http.MethodPost, taPath, taBody(testAdminRoleHubAdmin)).Code
		}(i)
	}
	wg.Wait()
	created = 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		} else {
			assert.Equal(t, http.StatusTooManyRequests, c)
		}
	}
	assert.Equal(t, testAdminHubCap-1, created, "one slot was left after the per-issuer test")
	n, err := s.CountLiveTestAdminUsers(ctx, "", "", time.Now())
	require.NoError(t, err)
	assert.Equal(t, testAdminHubCap, n)

	// Expired identities free their slots.
	srv.testIdentities.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	rec := doRequestAsUser(t, srv, issuer, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// A second test super-admin hub-wide is refused (the super-admin cap,
// checked in the same serialized transaction; exercised through a raw row
// because A1 refuses to issue the kind).
func TestTestAdmin_SuperAdminHubCapCounted(t *testing.T) {
	_, s, _ := taServer(t)
	taRawTestSuperAdmin(t, s)
	n, err := s.CountLiveTestAdminUsers(context.Background(), store.UserKindTestSuperAdmin, "", time.Now())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, testSuperAdminHubCap, "a live test super-admin fills the super-admin cap")
}

// --- AE-1 read scenario (consumer acceptance) ----------------------------------

// A minted test_hub_admin verifies a test-login-style user's leftovers by
// reads only: the user search, the user, the project, the agents by
// project and by owner (including soft-deleted) and each agent. A
// member-tier test identity is refused the admin-scoped reads.
func TestTestAdmin_AE1ReadScenario(t *testing.T) {
	srv, s, _ := taServer(t)
	ctx := context.Background()

	// The leftovers: one human test-login-style user, one project, six
	// agents (two soft-deleted).
	hex := generateID()[:8]
	human := &store.User{ID: generateID(), Email: "test-fixture-" + hex + "@scion-test.invalid", DisplayName: "test-fixture-" + hex,
		Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	ensureHubMembership(ctx, s, human.ID)
	projectID := tid("ta-ae1-project")
	taProject(t, s, projectID, human.ID)
	var agentIDs []string
	for i := 0; i < 6; i++ {
		a := &store.Agent{ID: generateID(), Slug: "ta-ae1-" + string(rune('a'+i)), Name: "ae1 " + string(rune('a'+i)),
			ProjectID: projectID, Phase: "stopped", CreatedBy: human.ID, OwnerID: human.ID}
		require.NoError(t, s.CreateAgent(ctx, a))
		if i >= 4 {
			a.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(ctx, a))
		}
		agentIDs = append(agentIDs, a.ID)
	}

	resp := taIssue(t, srv, taUser(t, s, "ta-ae1-issuer", store.SystemRoleSuperAdmin))
	member := tiIssue(t, srv, func() string { _, tok := tiIssuer(t, srv, s, "ta-ae1-member-issuer"); return tok }(), map[string]string{"role": "member"})

	get := func(token, path string) *httptest.ResponseRecorder {
		return doRequestWithToken(t, srv, token, http.MethodGet, path, nil)
	}
	reads := []string{
		"/api/v1/users?search=scion-test.invalid",
		"/api/v1/users/" + human.ID,
		"/api/v1/projects/" + projectID,
		"/api/v1/agents?projectId=" + projectID + "&includeDeleted=true",
		"/api/v1/agents?ownerId=" + human.ID + "&includeDeleted=true",
	}
	for _, id := range agentIDs {
		reads = append(reads, "/api/v1/agents/"+id)
	}
	for _, path := range reads {
		rec := get(resp.AccessToken, path)
		assert.Equal(t, http.StatusOK, rec.Code, "test_hub_admin GET %s: %s", path, rec.Body.String())
	}
	rec := get(resp.AccessToken, "/api/v1/users?search=scion-test.invalid")
	assert.Contains(t, rec.Body.String(), human.ID)
	for _, path := range reads[3:5] {
		body := get(resp.AccessToken, path).Body.String()
		assert.Equal(t, 6, strings.Count(body, `"slug":"ta-ae1-`), "all six agents, deleted included: %s", path)
		assert.Contains(t, body, `"deletedAt"`, path)
	}

	// The member-tier identity is refused the admin-scoped reads.
	for _, path := range []string{"/api/v1/users/" + human.ID, "/api/v1/projects/" + projectID, "/api/v1/agents/" + agentIDs[0]} {
		rec := get(member.AccessToken, path)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, "member-tier GET %s: %s", path, rec.Body.String())
	}
	rec = get(member.AccessToken, "/api/v1/users?search=scion-test.invalid")
	assert.NotContains(t, rec.Body.String(), human.ID, "the member-tier identity does not see the user list")
}

// taProject creates a project owned (project-owner binding) by the
// existing user ownerID.
func taProject(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "ta project " + projectID[:8], Slug: "ta-project-" + projectID[:8], CreatedBy: ownerID,
	}))
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: ownerID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}
