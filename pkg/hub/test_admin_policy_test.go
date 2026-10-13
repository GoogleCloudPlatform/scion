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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin tier A2 (ptone/scion#4240): the test super-admin kind, the
// test-admin policy enforced on live requests, and the hash-chained test
// admin audit sink. Every identity, user and project here is synthetic, in
// an in-memory store.

// tsaServer builds a test server with the member tier and both admin
// kinds on (--test-mode=super-admin), DB-backed operational settings, and a
// captured test admin audit sink.
func tsaServer(t *testing.T) (*Server, store.Store, *taSinkBuffer) {
	t.Helper()
	srv, s, buf := taServer(t)
	srv.testAdminGate = testAdminGate{hubAdmin: true, superAdmin: true, hubID: taHubID, hubClass: config.HubClassTest}
	srv.SetOperationalSettings(NewOperationalSettings(s, emptyKoanf(), emptyKoanf()))
	return srv, s, buf
}

// tsaIssue issues an admin-kind identity of role as the super-admin
// session u, requiring 201.
func tsaIssue(t *testing.T, srv *Server, u *store.User, role string) TestAdminIdentityTokenResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, u, http.MethodPost, taPath, taBody(role))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	return taDecode(t, rec)
}

// tsaSinkLines returns the sink lines of event (all lines for "").
func tsaSinkLines(buf *taSinkBuffer, event string) []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(l, testAdminAuditTag+" ") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(l, testAdminAuditTag+" ")), &m) != nil {
			continue
		}
		if event == "" || m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

// tsaIdentity is the authenticated identity of an admin-kind row, for
// direct Decide calls.
func tsaIdentity(t *testing.T, s store.Store, id string) UserIdentity {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeAPI))
}

// --- Issuance of the test super-admin kind (G3, TTLs, caps) -----------------

func TestTestSuperAdmin_Issuance(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-iss", store.SystemRoleSuperAdmin)

	// Lifetime and token TTL caps (30 min, 15 min) are enforced.
	for _, body := range []map[string]any{
		{"adminRole": testAdminRoleSuperAdmin, "purpose": taPurpose, "lifetimeSeconds": 1801},
		{"adminRole": testAdminRoleSuperAdmin, "purpose": taPurpose, "tokenTtlSeconds": 901},
	} {
		rec := doRequestAsUser(t, srv, issuer, http.MethodPost, taPath, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", body, rec.Body.String())
	}

	before := time.Now()
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	assert.Equal(t, store.UserKindTestSuperAdmin, resp.Identity.Kind)
	assert.Equal(t, testAdminRoleSuperAdmin, resp.Identity.AdminRole)
	assert.True(t, strings.HasSuffix(resp.Identity.Email, "@"+store.TestSuperAdminEmailDomain), resp.Identity.Email)
	assert.Equal(t, store.UserRoleAdmin, resp.Identity.Role, "stored role admin")
	assert.NotEmpty(t, resp.Identity.IssuanceID)
	// Defaults: identity 30 min, token 10 min.
	assert.WithinDuration(t, before.Add(30*time.Minute), resp.Identity.ExpiresAt, time.Minute)
	assert.Equal(t, int64(600), resp.ExpiresIn)
	assert.Equal(t, 10*time.Minute, taJWTLifetime(t, resp.AccessToken))
	// The real, expiring super-admin binding.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, resp.Identity.ID)
	require.NoError(t, err)
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	found := false
	for _, b := range bindings {
		if b.RoleDefinitionID == superRD.ID && b.ScopeType == store.RoleScopeSystem {
			found = true
			assert.Equal(t, store.TestIdentityIssuanceCreatedBy, b.CreatedBy)
			require.NotNil(t, b.ExpiresAt)
			assert.WithinDuration(t, resp.Identity.ExpiresAt, *b.ExpiresAt, time.Second, "the binding expires with the identity")
		}
	}
	assert.True(t, found, "the test super-admin holds a system super-admin binding")
	assert.True(t, srv.authzService.IsSystemAdmin(ctx, resp.Identity.ID))
	// The token authenticates (role claim admin matches the kind).
	assert.Equal(t, http.StatusOK, doRequestWithToken(t, srv, resp.AccessToken, http.MethodGet, "/api/v1/users/"+issuer.ID, nil).Code)

	// At most one test super-admin per hub: a second issuer gets 429.
	issuer2 := taUser(t, s, "tsa-iss2", store.SystemRoleSuperAdmin)
	rec := doRequestAsUser(t, srv, issuer2, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, testAdminReasonSuperAdminCap, taReason(t, rec))

	// Two super-admin issuances per issuer per hour. The per-issuer limiter
	// is a token bucket on wall time (two tokens, refilled at 2/h), not on
	// the test identity clock: the clock below only expires the identities
	// so the caps free up, and the third issuance, a moment later in wall
	// time, finds the bucket empty.
	srv.testIdentities.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	srv.testIdentities.now = func() time.Time { return time.Now().Add(62 * time.Minute) }
	rec = doRequestAsUser(t, srv, issuer, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, testIdentityReasonRateLimited, taReason(t, rec))
}

// The super-admin kind answers 404 on a hub started with
// --test-mode=admin, as in A1.
func TestTestSuperAdmin_OffWithAdminLevel404(t *testing.T) {
	srv, s, _ := taServer(t)
	issuer := taUser(t, s, "tsa-off", store.SystemRoleSuperAdmin)
	rec := doRequestAsUser(t, srv, issuer, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// Issuer tokens: a hub token scoped to exactly test_identity:issue_super_admin
// issues the super-admin kind (the route guard also admits that
// capability); it does not issue the hub-admin kind, and an issue_admin
// token does not issue the super-admin kind. A hub-admin is refused.
func TestTestSuperAdmin_IssuerTokens(t *testing.T) {
	srv, s, _ := tsaServer(t)
	issuer := taUser(t, s, "tsa-pat", store.SystemRoleSuperAdmin)
	superKey, superTokID := taPAT(t, srv, issuer.ID, uatScopeTestIdentityIssueSuperAdmin)
	adminKey, _ := taPAT(t, srv, issuer.ID, "test_identity:issue_admin")

	rec := doRequestWithToken(t, srv, superKey, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, "issue_super_admin token, hub-admin kind: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, adminKey, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, "issue_admin token, super-admin kind: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, superKey, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := taDecode(t, rec)
	row, err := s.GetUser(context.Background(), resp.Identity.ID)
	require.NoError(t, err)
	require.NotNil(t, row.IssuerCredentialID)
	assert.Equal(t, superTokID, *row.IssuerCredentialID)

	hubAdmin := taUser(t, s, "tsa-pat-hubadmin", store.SystemRoleHubAdmin)
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	// A test identity never issues, not even a test super-admin.
	rec = doRequestWithToken(t, srv, resp.AccessToken, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// --- The test-admin policy on live requests --------------------------------

// tsaDenyRoutes are the deny-set and default-deny routes the matrix
// exercises: every server configuration and settings surface, the GitHub
// app, hub environment variables, maintenance, auth reset, key rotation,
// integrations, access constraint administration, issuance and delegation
// adoption (a legacy role-claim admin route).
var tsaDenyRoutes = []struct {
	method, path string
	body         any
	// humanOK is true when a human super-admin gets 200 on the test server.
	humanOK bool
}{
	{http.MethodGet, "/api/v1/admin/server-config", nil, true},
	{http.MethodGet, "/api/v1/admin/server-config/schema", nil, false},
	{http.MethodPut, "/api/v1/admin/server-config", map[string]any{"telemetry": map[string]any{"enabled": true}}, false},
	{http.MethodGet, "/api/v1/admin/experiments", nil, true},
	{http.MethodPut, "/api/v1/admin/experiments", map[string]any{"overrides": map[string]any{experiments.AgentEdit: true}, "expected_revision": 0}, false},
	{http.MethodPut, "/api/v1/admin/messaging", map[string]any{}, false},
	{http.MethodPut, "/api/v1/admin/project-defaults", map[string]any{}, false},
	{http.MethodPut, "/api/v1/admin/profiling", map[string]any{}, false},
	{http.MethodGet, "/api/v1/github-app", nil, false},
	{http.MethodGet, "/api/v1/env?scope=hub", nil, true},
	{http.MethodGet, "/api/v1/admin/maintenance/operations", nil, false},
	{http.MethodPost, "/api/v1/admin/agents/reset-auth-all", nil, false},
	{http.MethodPost, "/api/v1/admin/conduit/grant-keys/rotate", nil, false},
	{http.MethodGet, "/api/v1/admin/integrations", nil, false},
	{http.MethodPost, "/api/v1/admin/access-constraint-previews", map[string]any{}, false},
	{http.MethodPost, "/api/v1/test-identities", map[string]any{"role": "member"}, false},
	{http.MethodGet, "/api/v1/admin/delegation-adoption", nil, false},
}

// Every deny-set route answers 403 to both admin kinds, including the
// role-claim admin routes, while a human super-admin passes the policy
// (200 where the test server can serve the route). Every settings section
// reset is refused too.
func TestTestAdminPolicy_DenySetRoutes(t *testing.T) {
	srv, s, buf := tsaServer(t)
	issuer := taUser(t, s, "tsa-deny-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-deny-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin)
	human := taTokenFor(t, srv, taUser(t, s, "tsa-deny-human", store.SystemRoleSuperAdmin))

	for _, rt := range tsaDenyRoutes {
		for who, token := range map[string]string{"test_super_admin": superKind.AccessToken, "test_hub_admin": hubKind.AccessToken} {
			rec := doRequestWithToken(t, srv, token, rt.method, rt.path, rt.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s %s: %s", who, rt.method, rt.path, rec.Body.String())
		}
		rec := doRequestWithToken(t, srv, human, rt.method, rt.path, rt.body)
		assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
			"human super-admin %s %s passes the policy: %s", rt.method, rt.path, rec.Body.String())
		if rt.humanOK {
			assert.Equal(t, http.StatusOK, rec.Code, "human super-admin %s %s: %s", rt.method, rt.path, rec.Body.String())
		}
	}
	for _, sec := range []string{"access", "telemetry", "federation", "experiments", "maintenance", "runtimes"} {
		rec := doRequestWithToken(t, srv, superKind.AccessToken, http.MethodDelete, "/api/v1/admin/server-config/sections/"+sec, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "section reset %s: %s", sec, rec.Body.String())
	}

	// Each refusal of the test super-admin is in the sink, with its
	// issuance ID.
	denials := tsaSinkLines(buf, testAdminAuditEventPolicyDenial)
	require.NotEmpty(t, denials)
	subjects := map[string]bool{}
	for _, d := range denials {
		if d["issuance_id"] == superKind.Identity.IssuanceID {
			subjects[d["class"].(string)+":"+d["subject"].(string)] = true
		}
	}
	for _, want := range []string{"permission:hub.config.read", "permission:hub.experiments.update", "role_gate:handlers_env_secrets.go:authorizeHubEnvVarList",
		"permission:hub.github_app.read", "legacy_route:admin.delegationAdoption"} {
		assert.True(t, subjects[want], "sink policy denial %s; got %v", want, subjects)
	}
}

// Decide: every deny-classified permission is refused to both admin kinds
// at step 0c (before any binding), and every allow-classified permission
// decides for a test super-admin exactly as for a human super-admin.
func TestTestAdminPolicy_DecideMatrix(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-dm-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-dm-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIdentity(t, s, tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID)
	hubKind := tsaIdentity(t, s, tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin).Identity.ID)
	human := tsaIdentity(t, s, taUser(t, s, "tsa-dm-human", store.SystemRoleSuperAdmin).ID)
	decide := func(u UserIdentity, p permissions.Permission) Decision {
		return srv.authzService.Decide(ctx, AuthzRequest{
			Principal: principalContextForIdentity(u), Credential: credentialContextForIdentity(u),
			Resource: Resource{Type: p.Resource, ID: "hub"}, Permission: p.ID,
		})
	}
	denied, allowed := 0, 0
	for _, p := range permissions.Registry {
		h := decide(human, p)
		if testAdminPermissionDenied(p.ID) {
			denied++
			for who, u := range map[string]UserIdentity{"test_super_admin": superKind, "test_hub_admin": hubKind} {
				d := decide(u, p)
				assert.False(t, d.Allowed, "%s %s", who, p.ID)
				if h.Reason == deliveryGateReason {
					continue // the delivery credential gate (step 0) refuses every user first
				}
				assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, "%s %s: %s", who, p.ID, d.Reason)
			}
			continue
		}
		allowed++
		d := decide(superKind, p)
		assert.Equal(t, h.Allowed, d.Allowed, "allow-classified %s: test super-admin decides as a human super-admin (%s / %s)", p.ID, h.Reason, d.Reason)
	}
	assert.Greater(t, denied, 20)
	assert.Greater(t, allowed, 80)
	// A human super-admin holds the caged permissions.
	for _, id := range []string{"hub.config.update", "hub.config.read", "hub.experiments.update", "hub.env_vars.read"} {
		assert.True(t, decide(human, permissions.Permission{ID: id, Resource: permissions.ResourceHub}).Allowed, id)
	}
}

// A test super-admin in a group that holds super-admin, and in a group
// whose custom role holds the caged permissions, is still refused the
// caged routes and permissions.
func TestTestAdminPolicy_GroupPathStillCaged(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-grp-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)

	superGroup := tiGroupWithSystemRole(t, s, "tsa-super-group", store.SystemRoleHubMember, issuer.ID)
	taRawGroupBinding(t, s, superGroup.ID, store.SystemRoleSuperAdmin)
	tiAddToGroup(t, s, superGroup.ID, resp.Identity.ID, store.GroupMemberRoleMember)

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "tsa-caged-" + generateID()[:6],
		ScopeType: store.RoleScopeSystem, Permissions: []string{"hub.config.read", "hub.config.update", "hub.experiments.update"}})
	require.NoError(t, err)
	customGroup := tiGroupWithSystemRole(t, s, "tsa-custom-group", store.SystemRoleHubMember, issuer.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: customGroup.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
	tiAddToGroup(t, s, customGroup.ID, resp.Identity.ID, store.GroupMemberRoleMember)

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/server-config"},
		{http.MethodPut, "/api/v1/admin/experiments"},
		{http.MethodGet, "/api/v1/admin/delegation-adoption"},
	} {
		rec := doRequestWithToken(t, srv, resp.AccessToken, rt.method, rt.path, map[string]any{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", rt.method, rt.path, rec.Body.String())
	}
	u := tsaIdentity(t, s, resp.Identity.ID)
	for _, id := range []string{"hub.config.read", "hub.config.update", "hub.experiments.update"} {
		d := srv.authzService.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(u), Credential: credentialContextForIdentity(u),
			Resource: Resource{Type: permissions.ResourceHub, ID: "hub"}, Permission: id})
		assert.False(t, d.Allowed, id)
		assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, id)
	}
}

// Legacy admin routes and role-claim gates: an allow-classified legacy
// site reaches its handler for a test super-admin; a deny-classified or
// unknown site refuses it; humans are never refused; a deny role gate
// (hub environment variables) refuses it.
func TestTestAdminPolicy_LegacyRoutesAndRoleGates(t *testing.T) {
	srv, s, _ := tsaServer(t)
	issuer := taUser(t, s, "tsa-lr-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	superID := tsaIdentity(t, s, resp.Identity.ID)
	humanID := tsaIdentity(t, s, taUser(t, s, "tsa-lr-human", store.SystemRoleSuperAdmin).ID)

	run := func(u UserIdentity, site string) int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		ctx := contextWithIdentity(req.Context(), u)
		ctx = context.WithValue(ctx, userContextKey{}, u)
		rec := httptest.NewRecorder()
		if _, ok := srv.requireAdminAt(rec, req.WithContext(ctx), "", site); ok {
			return http.StatusOK
		}
		return rec.Code
	}
	for _, site := range []string{"agents.hold.lift", "users.sessions.revoke"} {
		assert.Equal(t, http.StatusOK, run(superID, site), site)
	}
	for _, site := range []string{"admin.delegationAdoption", "admin.delegationAdoption.commits", "", "unknown.site"} {
		assert.Equal(t, http.StatusForbidden, run(superID, site), "site %q", site)
		assert.Equal(t, http.StatusOK, run(humanID, site), "human, site %q", site)
	}

	// users.sessions.revoke (allow) reaches the handler over HTTP.
	target := taUser(t, s, "tsa-lr-target", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, resp.AccessToken, http.MethodPost, "/api/v1/users/"+target.ID+"/revoke-sessions", nil)
	assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Role gates: the hub environment variable list and hub-scope
	// variables are refused; a human super-admin lists them.
	assert.Equal(t, http.StatusForbidden, doRequestWithToken(t, srv, resp.AccessToken, http.MethodGet, "/api/v1/env?scope=hub", nil).Code)
	assert.True(t, srv.testAdminPolicyRefuses(contextWithIdentityAndUser(superID), "handlers_env_secrets.go:resolveEnvSecretAccess"))
	assert.False(t, srv.testAdminPolicyRefuses(contextWithIdentityAndUser(humanID), "handlers_env_secrets.go:resolveEnvSecretAccess"))
	assert.False(t, srv.testAdminPolicyRefuses(contextWithIdentityAndUser(superID), "web.go:authorizeSSESubjects"), "allow gate")
	assert.True(t, srv.testAdminPolicyRefuses(contextWithIdentityAndUser(superID), "unclassified.go:gate"), "an unclassified gate refuses")
	assert.True(t, srv.testAdminPolicyRefusesUser(context.Background(), resp.Identity.ID, "admin_delegation_adoption.go:recheckDelegationAdoptionAdmin"))
	assert.False(t, srv.testAdminPolicyRefusesUser(context.Background(), humanID.ID(), "admin_delegation_adoption.go:recheckDelegationAdoptionAdmin"))
	ctxSuper := contextWithIdentityAndUser(superID)
	assert.False(t, gcpIAMHubAdmin(ctxSuper) && !srv.testAdminPolicyRefuses(ctxSuper, "gcp_iam_settings.go:gcpIAMHubAdmin"))

	// Broker credential remint and rotation (key material): the
	// super-admin arm admits a human, not a test super-admin.
	broker := &store.RuntimeBroker{ID: generateID(), Name: "tsa-broker"}
	assert.False(t, srv.brokerRemintTargetAuthorized(ctxSuper, superID, broker))
	assert.True(t, srv.brokerRemintTargetAuthorized(contextWithIdentityAndUser(humanID), humanID, broker))
	ok, err := srv.authorizedForBrokerRotate(ctxSuper, superID, nil, broker.ID, func() (*store.RuntimeBroker, error) { return broker, nil })
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = srv.authorizedForBrokerRotate(contextWithIdentityAndUser(humanID), humanID, nil, broker.ID, func() (*store.RuntimeBroker, error) { return broker, nil })
	require.NoError(t, err)
	assert.True(t, ok)
}

// tsaErrCode returns the error.code of an API error response.
func tsaErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func contextWithIdentityAndUser(u UserIdentity) context.Context {
	ctx := contextWithIdentity(context.Background(), u)
	ctx = context.WithValue(ctx, userContextKey{}, u)
	return contextWithCredentialContext(ctx, credentialContextForIdentity(u))
}

// The operational settings write guard refuses every section write and
// delete by an admin-kind identity; human and background writes pass. The
// settings.yaml and experiment handlers refuse before writing.
func TestTestAdminPolicy_SettingsWriteGuard(t *testing.T) {
	srv, s, _ := tsaServer(t)
	issuer := taUser(t, s, "tsa-sw-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	superCtx := contextWithIdentityAndUser(tsaIdentity(t, s, resp.Identity.ID))
	humanCtx := contextWithIdentityAndUser(tsaIdentity(t, s, issuer.ID))
	ops := srv.GetOperationalSettings()
	require.NotNil(t, ops)
	for _, sec := range tsaSectionNames() {
		_, err := ops.Update(superCtx, sec, json.RawMessage(`{}`), "x", -1, "managed")
		assert.ErrorIs(t, err, errTestAdminSettingsRefused, sec)
		assert.ErrorIs(t, ops.DeleteSection(superCtx, sec), errTestAdminSettingsRefused, sec)
		assert.NoError(t, srv.testAdminSettingsWriteGuard(humanCtx, sec), sec)
		assert.NoError(t, srv.testAdminSettingsWriteGuard(context.Background(), sec), sec)
	}

	// Handler-level refusals (defense in depth behind the route guard).
	r := httptest.NewRequest(http.MethodPut, "/x", nil).WithContext(superCtx)
	rec := httptest.NewRecorder()
	assert.True(t, srv.rejectTestAdminSettingsWrite(rec, r, []string{"telemetry.enabled"}, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, testAdminReasonPolicyDenied, taReason(t, rec))
	rec = httptest.NewRecorder()
	assert.True(t, srv.rejectTestAdminSettingsWrite(rec, r, nil, []string{experiments.AuthorizationDecisionAuditV2}))
	rec = httptest.NewRecorder()
	assert.True(t, srv.rejectTestAdminSettingsWrite(rec, r, nil, nil), "an empty write is refused")
	rec = httptest.NewRecorder()
	assert.False(t, srv.rejectTestAdminSettingsWrite(rec, r.WithContext(humanCtx), []string{"telemetry.enabled"}, []string{experiments.AgentEdit}))
}

// tsaSectionNames lists every classified operational settings section.
func tsaSectionNames() []string {
	var out []string
	for k := range testAdminSettingsSectionPolicy {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- The test admin audit sink on live requests ----------------------------

// Every request by an admin-kind identity yields one chained request line
// (before the handler) and one response line (status, decision,
// issuance_id); human requests yield none; the chain verifies, and a
// deleted line is detected.
func TestTestAdminSink_EveryAdminRequestChained(t *testing.T) {
	srv, s, buf := tsaServer(t)
	issuer := taUser(t, s, "tsa-sink-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	human := taTokenFor(t, srv, issuer)
	startReq := len(tsaSinkLines(buf, testAdminAuditEventRequest))

	requests := []struct {
		method, path string
		decision     string
		route        string
	}{
		{http.MethodGet, "/api/v1/users/" + issuer.ID, "allowed", "/api/v1/users/"},
		{http.MethodGet, "/api/v1/admin/server-config", "policy_denied", "/api/v1/admin/server-config"},
		{http.MethodGet, "/api/v1/projects", "allowed", ""},
		{http.MethodGet, "/api/v1/no-such-route-" + generateID()[:6], "failed", ""},
	}
	for _, rq := range requests {
		doRequestWithToken(t, srv, resp.AccessToken, rq.method, rq.path, nil)
	}
	for i := 0; i < 3; i++ {
		doRequestWithToken(t, srv, human, http.MethodGet, "/api/v1/admin/server-config", nil)
	}
	reqLines := tsaSinkLines(buf, testAdminAuditEventRequest)[startReq:]
	respLines := tsaSinkLines(buf, testAdminAuditEventResponse)
	require.Len(t, reqLines, len(requests), "one request line per admin-kind request, none for human requests")
	require.Len(t, respLines, len(requests))
	for i, rq := range requests {
		for _, l := range []map[string]any{reqLines[i], respLines[i]} {
			assert.Equal(t, resp.Identity.IssuanceID, l["issuance_id"])
			assert.Equal(t, resp.Identity.ID, l["user_id"])
			assert.Equal(t, store.UserKindTestSuperAdmin, l["kind"])
			assert.Equal(t, rq.method, l["method"])
			if rq.route != "" {
				assert.Equal(t, rq.route, l["route"], rq.path)
			}
		}
		assert.Equal(t, rq.decision, respLines[i]["decision"], rq.path)
		assert.NotNil(t, respLines[i]["status"])
	}
	assert.EqualValues(t, http.StatusForbidden, respLines[1]["status"])

	n, err := verifyTestAdminAuditChain(strings.NewReader(buf.String()))
	require.NoError(t, err)
	assert.Equal(t, len(tsaSinkLines(buf, "")), n)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Greater(t, len(lines), 3)
	_, err = verifyTestAdminAuditChain(strings.NewReader(strings.Join(append(append([]string(nil), lines[:2]...), lines[3:]...), "\n")))
	assert.ErrorIs(t, err, errTestAdminAuditChainBroken, "deleting a line breaks the chain")
}

// A sink write failure refuses issuance and admin-kind requests with 503
// before the handler runs; human requests are unaffected.
func TestTestAdminSink_FailClosed(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-fc-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)

	buf.mu.Lock()
	buf.err = errors.New("journal unavailable")
	buf.mu.Unlock()

	groupsBefore, err := s.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	rec := doRequestWithToken(t, srv, resp.AccessToken, http.MethodPost, "/api/v1/groups", map[string]any{"name": "tsa-fc-group", "slug": "tsa-fc-group"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeTestAdminAuditUnavailable)
	groupsAfter, err := s.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, len(groupsBefore.Items), len(groupsAfter.Items), "the handler did not run")
	assert.Equal(t, http.StatusServiceUnavailable, doRequestWithToken(t, srv, resp.AccessToken, http.MethodGet, "/api/v1/users/"+issuer.ID, nil).Code)

	// Human requests are unaffected.
	assert.Equal(t, http.StatusOK, doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/users/"+issuer.ID, nil).Code)
	// Issuance fails closed.
	issuer2 := taUser(t, s, "tsa-fc-iss2", store.SystemRoleSuperAdmin)
	rec = doRequestAsUser(t, srv, issuer2, http.MethodPost, taPath, taBody(testAdminRoleHubAdmin))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

// Turning the decision audit experiment off (by a human) does not reduce
// the sink lines an admin-kind request writes.
func TestTestAdminSink_DecisionAuditExperimentIndependent(t *testing.T) {
	srv, s, buf := tsaServer(t)
	issuer := taUser(t, s, "tsa-exp-iss", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	setExperiment := func(on bool) {
		rec := doRequestAsUser(t, srv, issuer, http.MethodGet, "/api/v1/admin/experiments", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var cur adminExperimentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cur))
		rec = doRequestAsUser(t, srv, issuer, http.MethodPut, "/api/v1/admin/experiments", map[string]any{
			"overrides": map[string]any{experiments.AuthorizationDecisionAuditV2: on}, "expected_revision": cur.Revision})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, on, srv.experimentEnabled(experiments.AuthorizationDecisionAuditV2))
	}
	batch := func() int {
		before := len(tsaSinkLines(buf, ""))
		doRequestWithToken(t, srv, resp.AccessToken, http.MethodGet, "/api/v1/users/"+issuer.ID, nil)
		doRequestWithToken(t, srv, resp.AccessToken, http.MethodGet, "/api/v1/admin/server-config", nil)
		return len(tsaSinkLines(buf, "")) - before
	}
	setExperiment(true)
	on := batch()
	setExperiment(false)
	off := batch()
	assert.Equal(t, on, off, "the sink does not depend on the decision audit experiment")
	assert.GreaterOrEqual(t, off, 5, "two request lines, two response lines and a policy denial")
	_, err := verifyTestAdminAuditChain(strings.NewReader(buf.String()))
	require.NoError(t, err)
}

// No token appears in any sink line or log line written while identities
// are issued and used (golden: the issued tokens, the issuer's session
// token, and any bearer or JWT-shaped string).
func TestTestAdminSink_NoTokenInSinkOrLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv, s, buf := tsaServer(t)
	issuer := taUser(t, s, "tsa-tok-iss", store.SystemRoleSuperAdmin)
	issuerToken := taTokenFor(t, srv, issuer)
	patKey, _ := taPAT(t, srv, issuer.ID, uatScopeTestIdentityIssueSuperAdmin)
	rec := doRequestWithToken(t, srv, patKey, http.MethodPost, taPath, taBody(testAdminRoleSuperAdmin))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	superKind := taDecode(t, rec)
	issuer2 := taUser(t, s, "tsa-tok-iss2", store.SystemRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin)
	for _, tok := range []string{superKind.AccessToken, hubKind.AccessToken} {
		doRequestWithToken(t, srv, tok, http.MethodGet, "/api/v1/users/"+issuer.ID, nil)
		doRequestWithToken(t, srv, tok, http.MethodGet, "/api/v1/admin/server-config", nil)
		doRequestWithToken(t, srv, tok, http.MethodPut, "/api/v1/admin/experiments", map[string]any{})
	}
	doRequestWithToken(t, srv, issuerToken, http.MethodPost, taRevokeAllPath, nil)

	sink := buf.String()
	require.NotEmpty(t, tsaSinkLines(buf, testAdminAuditEventRequest))
	for name, out := range map[string]string{"sink": sink, "logs": logs.String()} {
		for _, secret := range []string{superKind.AccessToken, hubKind.AccessToken, issuerToken, patKey} {
			require.NotEmpty(t, secret)
			assert.NotContains(t, out, secret, "%s carries a token", name)
		}
		assert.NotContains(t, out, "Bearer ", name)
	}
	assert.NotContains(t, sink, "eyJ", "no JWT-shaped value in the sink")
	for _, l := range tsaSinkLines(buf, "") {
		for k := range l {
			assert.NotContains(t, []string{"token", "access_token", "accessToken", "authorization"}, k)
		}
	}
}

// --- Last human super-admin ----------------------------------------------------

// What a test super-admin can do to the last human super-admin: it cannot
// delete or suspend it (the last-admin and constraint-admin guards do not
// count test identities), demote it (user.promote is deny) or remove any
// super-admin binding (CanDelegate's test-admin policy check: the role
// holds deny-classified permissions); it can revoke its web sessions (the
// human signs in again). A super-admin binding a human admin removes is
// restored by the admin_emails reconcile at restart.
func TestTestSuperAdmin_LastHumanAdmin(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	lastHuman := taUser(t, s, "tsa-last-human", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, lastHuman, testAdminRoleSuperAdmin)
	tok := resp.AccessToken
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	// Make lastHuman the only human super-admin: remove every other human's
	// super-admin binding (the seeded development user's, for one).
	system, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeSystem, "")
	require.NoError(t, err)
	for _, b := range system {
		if b.RoleDefinitionID != superRD.ID || b.PrincipalType != store.RoleBindingPrincipalUser || b.PrincipalID == lastHuman.ID {
			continue
		}
		if u, err := s.GetUser(ctx, b.PrincipalID); err == nil && !u.IsHumanKind() {
			continue
		}
		require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
	}
	bindingOf := func(userID string) *store.RoleBinding {
		bs, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
		require.NoError(t, err)
		for _, b := range bs {
			if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == superRD.ID {
				return b
			}
		}
		return nil
	}
	stillAdmin := func(what string) {
		u, err := s.GetUser(ctx, lastHuman.ID)
		require.NoError(t, err, what)
		assert.Equal(t, store.UserStatusActive, u.Status, what)
		assert.Equal(t, store.UserRoleAdmin, u.Role, what)
		assert.NotNil(t, bindingOf(lastHuman.ID), what)
	}

	rec := doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/users/"+lastHuman.ID, nil)
	t.Logf("delete last human super-admin: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, ErrCodeConflict, tsaErrCode(t, rec))
	assert.Contains(t, rec.Body.String(), "cannot delete the last super-admin")
	stillAdmin("delete")
	rec = doRequestWithToken(t, srv, tok, http.MethodPatch, "/api/v1/users/"+lastHuman.ID, map[string]any{"status": store.UserStatusSuspended})
	t.Logf("suspend last human super-admin: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, ErrCodeConstraintAdminLockout, tsaErrCode(t, rec))
	stillAdmin("suspend")
	rec = doRequestWithToken(t, srv, tok, http.MethodPatch, "/api/v1/users/"+lastHuman.ID, map[string]any{"role": store.UserRoleMember})
	t.Logf("demote last human super-admin: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusForbidden, rec.Code, "user.promote is denied to test identities")
	assert.Equal(t, ErrCodeForbidden, tsaErrCode(t, rec))
	assert.Contains(t, rec.Body.String(), "requires user.promote permission")
	stillAdmin("demote")
	b := bindingOf(lastHuman.ID)
	require.NotNil(t, b)
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/admin/role-bindings/"+b.ID, nil)
	t.Logf("remove last human super-admin binding: %d %s", rec.Code, rec.Body.String())
	// Removing a super-admin binding runs CanDelegate over the super-admin
	// role, which holds deny-classified permissions: the test-admin policy
	// refuses it before the last-admin guard is reached.
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, ErrCodeForbidden, tsaErrCode(t, rec))
	assert.Contains(t, rec.Body.String(), "test-admin policy")
	stillAdmin("binding removal")
	rec = doRequestWithToken(t, srv, tok, http.MethodPost, "/api/v1/users/"+lastHuman.ID+"/revoke-sessions", nil)
	t.Logf("revoke last human super-admin sessions: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, rec.Code, "web session revocation is allowed (users.sessions.revoke)")

	// With a second human admin, the test super-admin still cannot remove
	// a human's super-admin binding. If a human admin removes it, the
	// admin_emails reconcile at restart restores it.
	second := taUser(t, s, "tsa-second-human", store.SystemRoleSuperAdmin)
	require.NotNil(t, bindingOf(second.ID))
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/admin/role-bindings/"+b.ID, nil)
	t.Logf("test super-admin removes a non-last human super-admin binding: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusForbidden, rec.Code)
	stillAdmin("non-last binding removal by the test super-admin")
	rec = doRequestAsUser(t, srv, second, http.MethodDelete, "/api/v1/admin/role-bindings/"+b.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "a human admin removes it: %s", rec.Body.String())
	assert.Nil(t, bindingOf(lastHuman.ID))
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{lastHuman.Email, second.Email}, "member")
	require.NoError(t, err)
	stillAdmin("after reconcile")
	assert.NotNil(t, bindingOf(resp.Identity.ID), "reconcile leaves the test super-admin alone")
}

// --- AE-1 scenario (consumer ae-lead) -------------------------------------------

// A test super-admin verifies agent absence by reads (with a visibility
// control: a project with known live agents returns exactly that count),
// then removes a test-login-style human user's leftovers on the general
// routes, resource by resource: agents, then the project, then the user.
// The user delete is refused while the user still owns live agents and
// while it is a project's last owner. Every request is in the sink with
// the issuance ID.
func TestTestSuperAdmin_AE1Scenario(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()

	hex := generateID()[:8]
	human := &store.User{ID: generateID(), Email: "test-fixture-" + hex + "@scion-test.invalid", DisplayName: "test-fixture-" + hex,
		Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, human))
	ensureHubMembership(ctx, s, human.ID)
	projectID := tid("tsa-ae1-project")
	taProject(t, s, projectID, human.ID)
	var live, deleted []string
	for i := 0; i < 6; i++ {
		a := &store.Agent{ID: generateID(), Slug: "tsa-ae1-" + string(rune('a'+i)), Name: "ae1 " + string(rune('a'+i)),
			ProjectID: projectID, Phase: "stopped", CreatedBy: human.ID, OwnerID: human.ID}
		require.NoError(t, s.CreateAgent(ctx, a))
		if i >= 4 {
			a.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(ctx, a))
			deleted = append(deleted, a.ID)
			continue
		}
		live = append(live, a.ID)
	}
	controlOwner := &store.User{ID: generateID(), Email: "tsa-ae1-control-" + hex + "@example.com", DisplayName: "control",
		Role: store.UserRoleMember, Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, controlOwner))
	ensureHubMembership(ctx, s, controlOwner.ID)
	controlProject := tid("tsa-ae1-control")
	taProject(t, s, controlProject, controlOwner.ID)
	const controlLive = 3
	for i := 0; i < controlLive; i++ {
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: generateID(), Slug: "tsa-ae1-ctl-" + string(rune('a'+i)), Name: "ctl " + string(rune('a'+i)),
			ProjectID: controlProject, Phase: "stopped", CreatedBy: controlOwner.ID, OwnerID: controlOwner.ID}))
	}

	issuer := taUser(t, s, "tsa-ae1-issuer", store.SystemRoleSuperAdmin)
	resp := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	tok := resp.AccessToken
	get := func(path string) *httptest.ResponseRecorder {
		return doRequestWithToken(t, srv, tok, http.MethodGet, path, nil)
	}
	count := func(body, prefix string) int { return strings.Count(body, `"slug":"`+prefix) }
	byProject := "/api/v1/agents?projectId=" + projectID + "&includeDeleted=true"
	byOwner := "/api/v1/agents?ownerId=" + human.ID + "&includeDeleted=true"
	liveByProject := "/api/v1/agents?projectId=" + projectID

	// Reads: the user, the project, every agent, and the lists with known
	// counts; the control project returns exactly its live agents.
	for _, path := range []string{"/api/v1/users?search=scion-test.invalid", "/api/v1/users/" + human.ID, "/api/v1/projects/" + projectID} {
		assert.Equal(t, http.StatusOK, get(path).Code, path)
	}
	for _, id := range append(append([]string(nil), live...), deleted...) {
		rec := get("/api/v1/agents/" + id)
		assert.Contains(t, []int{http.StatusOK, http.StatusNotFound}, rec.Code, "GET agent %s: %s", id, rec.Body.String())
	}
	for _, id := range live {
		assert.Equal(t, http.StatusOK, get("/api/v1/agents/"+id).Code, "a live agent is readable")
	}
	for path, want := range map[string]int{byProject: 6, byOwner: 6, liveByProject: 4} {
		rec := get(path)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, want, count(rec.Body.String(), "tsa-ae1-")-count(rec.Body.String(), "tsa-ae1-ctl-"), path)
	}
	for _, path := range []string{"/api/v1/agents?projectId=" + controlProject + "&includeDeleted=true", "/api/v1/agents?ownerId=" + controlOwner.ID + "&includeDeleted=true"} {
		rec := get(path)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, controlLive, count(rec.Body.String(), "tsa-ae1-ctl-"), "visibility control: %s", path)
	}

	// The user delete is refused while the user owns live agents.
	rec := doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/users/"+human.ID, nil)
	t.Logf("user delete with live agents: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusConflict, rec.Code)
	// Agents, one by one.
	for _, id := range live {
		rec := doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/agents/"+id, nil)
		require.Less(t, rec.Code, 300, "DELETE agent %s: %s", id, rec.Body.String())
	}
	rec = get(liveByProject)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, count(rec.Body.String(), "tsa-ae1-"), "no live agent is left")
	// Still the project's last owner: refused.
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/users/"+human.ID, nil)
	t.Logf("user delete while last project owner: %d %s", rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusConflict, rec.Code)
	// The project, then the user.
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/projects/"+projectID, nil)
	require.Less(t, rec.Code, 300, "DELETE project: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, tok, http.MethodDelete, "/api/v1/users/"+human.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := s.GetUser(ctx, human.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	// The control project is untouched.
	rec = get("/api/v1/agents?projectId=" + controlProject)
	assert.Equal(t, controlLive, count(rec.Body.String(), "tsa-ae1-ctl-"))

	// Every delete is in the sink with the issuance ID, and in
	// mutation audit with the correlation ID.
	deletes := 0
	for _, l := range tsaSinkLines(buf, testAdminAuditEventResponse) {
		if l["method"] == http.MethodDelete {
			deletes++
			assert.Equal(t, resp.Identity.IssuanceID, l["issuance_id"])
		}
	}
	assert.Equal(t, len(live)+4, deletes, "agents, three user deletes and the project delete")
	_, err = verifyTestAdminAuditChain(strings.NewReader(buf.String()))
	require.NoError(t, err)
}

// The test super-admin kind is allowed by G3 only with a complete
// classification (the hub re-checks the gate configuration at New).
func TestTestSuperAdmin_GateRequiresCompletePolicy(t *testing.T) {
	cfg := ServerConfig{HubID: taHubID, EnableTestIdentities: true, TestAdminGate: TestAdminGateConfig{
		HubAdmin: true, SuperAdmin: true, HubID: taHubID, HubClass: config.HubClassTest,
		MarkerHubID: taHubID, MarkerClass: config.HubClassTest,
	}}
	require.NoError(t, validateTestAdminGateConfig(cfg, true))
	cfg.TestAdminGate.HubAdmin = false
	require.ErrorIs(t, validateTestAdminGateConfig(cfg, true), ErrTestAdminInconsistent, "super-admin requires the hub-admin kind (G2)")
	cfg.TestAdminGate.HubAdmin = true

	prev := testAdminExperimentPolicy[experiments.AgentEdit]
	delete(testAdminExperimentPolicy, experiments.AgentEdit)
	t.Cleanup(func() { testAdminExperimentPolicy[experiments.AgentEdit] = prev })
	require.ErrorIs(t, validateTestAdminGateConfig(cfg, true), ErrTestSuperAdminRequiresPolicyRegistry)
	cfg.TestAdminGate.SuperAdmin = false
	require.NoError(t, validateTestAdminGateConfig(cfg, true), "the hub-admin kind does not need the policy")
}

// --- Delegation by an admin kind (review C1) ----------------------------------

// An admin-kind test identity cannot grant a deny-classified permission to
// anyone, through (a) a direct role binding, (b) a group binding or a group
// membership whose group holds such a role, or (c) a custom role definition;
// grants of allow-classified permissions still work; (d) a human
// super-admin is unaffected. Both admin kinds.
func TestTestAdminPolicy_DelegationOfDeniedPermissionsRefused(t *testing.T) {
	srv, s, buf := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-dlg-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-dlg-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin)
	humanTok := taTokenFor(t, srv, taUser(t, s, "tsa-dlg-human", store.SystemRoleSuperAdmin))
	grantee := taUser(t, s, "tsa-dlg-grantee", store.SystemRoleHubMember)

	// Roles written by the store (not by a test identity).
	denyRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "tsa-dlg-deny-" + generateID()[:6],
		ScopeType: store.RoleScopeSystem, Permissions: []string{"user.read", "hub.config.update"}})
	require.NoError(t, err)
	allowRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "tsa-dlg-allow-" + generateID()[:6],
		ScopeType: store.RoleScopeSystem, Permissions: []string{"user.read"}})
	require.NoError(t, err)
	hubAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	group := tiGroupWithSystemRole(t, s, "tsa-dlg-group-"+generateID()[:6], store.SystemRoleHubMember, issuer.ID)
	denyGroup := tiGroupWithSystemRole(t, s, "tsa-dlg-dgroup-"+generateID()[:6], store.SystemRoleHubMember, issuer.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: denyRole.ID, PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: denyGroup.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)

	binding := func(roleID, principalType, principalID string) map[string]any {
		return map[string]any{"roleDefinitionId": roleID, "principalType": principalType, "principalId": principalID, "scopeType": "system"}
	}
	customRole := func(perms ...string) map[string]any {
		return map[string]any{"name": "tsa-dlg-custom-" + generateID()[:8], "scopeType": "system", "permissions": perms}
	}
	refused := []struct {
		name, method, path string
		body               map[string]any
	}{
		{"(a) role binding of a deny-holding role to a human", http.MethodPost, "/api/v1/admin/role-bindings", binding(denyRole.ID, "user", grantee.ID)},
		{"(a) role binding of the built-in hub-admin role to a human", http.MethodPost, "/api/v1/admin/role-bindings", binding(hubAdminRD.ID, "user", grantee.ID)},
		{"(b) role binding of a deny-holding role to a group", http.MethodPost, "/api/v1/admin/role-bindings", binding(denyRole.ID, "group", group.ID)},
		{"(b) membership in a group holding a deny role", http.MethodPost, "/api/v1/groups/" + denyGroup.ID + "/members", map[string]any{"memberType": "user", "memberId": grantee.ID, "role": "member"}},
		{"(c) custom role with hub.config.update", http.MethodPost, "/api/v1/admin/roles", customRole("user.read", "hub.config.update")},
		{"(c) custom role with test_identity.issue_super_admin", http.MethodPost, "/api/v1/admin/roles", customRole(permissionTestIdentityIssueSuperAdmin)},
	}
	for _, tc := range refused {
		for who, tok := range map[string]string{"test_super_admin": superKind.AccessToken, "test_hub_admin": hubKind.AccessToken} {
			rec := doRequestWithToken(t, srv, tok, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", who, tc.name, rec.Body.String())
		}
	}
	// The grantee and the deny group gained nothing.
	bs, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, grantee.ID)
	require.NoError(t, err)
	for _, b := range bs {
		assert.NotEqual(t, denyRole.ID, b.RoleDefinitionID)
		assert.NotEqual(t, hubAdminRD.ID, b.RoleDefinitionID)
	}

	// Allow-only grants still work for both kinds.
	for who, tok := range map[string]string{"test_super_admin": superKind.AccessToken, "test_hub_admin": hubKind.AccessToken} {
		rec := doRequestWithToken(t, srv, tok, http.MethodPost, "/api/v1/admin/roles", customRole("user.read"))
		assert.Less(t, rec.Code, 300, "%s allow-only custom role: %s", who, rec.Body.String())
		target := taUser(t, s, "tsa-dlg-ok-"+who, store.SystemRoleHubMember)
		rec = doRequestWithToken(t, srv, tok, http.MethodPost, "/api/v1/admin/role-bindings", binding(allowRole.ID, "user", target.ID))
		assert.Less(t, rec.Code, 300, "%s allow-only role binding: %s", who, rec.Body.String())
	}

	// (d) A human super-admin is unaffected by the new refusal.
	for _, tc := range refused {
		rec := doRequestWithToken(t, srv, humanTok, tc.method, tc.path, tc.body)
		assert.Less(t, rec.Code, 300, "human super-admin %s: %s", tc.name, rec.Body.String())
	}

	// The refusal is the test-admin policy's (before the super-admin
	// shortcut), and it is in the sink.
	for _, id := range []string{superKind.Identity.ID, hubKind.Identity.ID} {
		d := srv.authzService.CanDelegate(ctx, tsaIdentity(t, s, id), GrantDescriptor{Type: GrantTypeCustomRole,
			CustomRolePermissions: []string{"hub.config.update"}, ScopeType: store.RoleScopeSystem})
		assert.False(t, d.Allowed)
		assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, d.Reason)
	}
	found := false
	for _, l := range tsaSinkLines(buf, testAdminAuditEventPolicyDenial) {
		if l["class"] == "delegation" && l["issuance_id"] == superKind.Identity.IssuanceID {
			found = true
		}
	}
	assert.True(t, found, "a delegation refusal is written to the sink")
}

// project.secret_read is allow (the agent material path's project secret
// gate; no user route exercises it). The user-facing hub secret and
// environment variable reads stay denied to both admin kinds; a human
// super-admin reads them. (Agents rooted in a project a test admin owns
// read that project's secrets: the accepted path, see the classification
// comment.)
func TestTestAdminPolicy_SecretValueReadsDenied(t *testing.T) {
	srv, s, _ := tsaServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	issuer := taUser(t, s, "tsa-sec-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-sec-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin)
	human := taTokenFor(t, srv, issuer)
	assert.False(t, testAdminPermissionDenied("project.secret_read"))
	assert.True(t, testAdminPermissionDenied("hub.env_vars.read"))
	for _, path := range []string{"/api/v1/secrets?scope=hub", "/api/v1/env?scope=hub"} {
		for who, tok := range map[string]string{"test_super_admin": superKind.AccessToken, "test_hub_admin": hubKind.AccessToken} {
			rec := doRequestWithToken(t, srv, tok, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s GET %s: %s", who, path, rec.Body.String())
		}
		rec := doRequestWithToken(t, srv, human, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "human super-admin GET %s: %s", path, rec.Body.String())
	}
}

// Review N5: decide step 0c and the delegation check do not depend on the
// clamp's kind cache. An authorization service whose store is not the
// clamped store reads the kind uncached and still refuses a deny-classified
// permission to an admin kind; a human super-admin is unaffected.
func TestTestAdminPolicy_CageWithoutKindCache(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-nc-iss", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	svc := NewAuthzService(s, slog.Default())
	svc.store = s // the plain store: no clamp, no kind cache
	require.Nil(t, svc.kindCache())
	decide := func(u UserIdentity) Decision {
		return svc.Decide(ctx, AuthzRequest{Principal: principalContextForIdentity(u), Credential: credentialContextForIdentity(u),
			Resource: Resource{Type: permissions.ResourceHub, ID: "hub"}, Permission: "hub.config.update"})
	}
	d := decide(tsaIdentity(t, s, superKind.Identity.ID))
	assert.False(t, d.Allowed)
	assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, d.Reason)
	assert.True(t, decide(tsaIdentity(t, s, issuer.ID)).Allowed, "a human super-admin is unaffected")
	dd := svc.CanDelegate(ctx, tsaIdentity(t, s, superKind.Identity.ID), GrantDescriptor{Type: GrantTypeCustomRole,
		CustomRolePermissions: []string{"hub.config.update"}, ScopeType: store.RoleScopeSystem})
	assert.Equal(t, DeniedByTestAdminPolicy, dd.DeniedBy, dd.Reason)
}

// Review RN2: adding a deny-classified permission to a custom role that
// is already bound to a human (PUT /api/v1/admin/roles/{id}) is refused to
// both admin kinds; a human super-admin may.
func TestTestAdminPolicy_RoleUpdateAddingDeniedPermissionRefused(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	superKind := tsaIssue(t, srv, taUser(t, s, "tsa-ru-iss", store.SystemRoleSuperAdmin), testAdminRoleSuperAdmin)
	hubKind := tsaIssue(t, srv, taUser(t, s, "tsa-ru-iss2", store.SystemRoleSuperAdmin), testAdminRoleHubAdmin)
	humanTok := taTokenFor(t, srv, taUser(t, s, "tsa-ru-human", store.SystemRoleSuperAdmin))
	grantee := taUser(t, s, "tsa-ru-grantee", store.SystemRoleHubMember)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "tsa-ru-" + generateID()[:6],
		ScopeType: store.RoleScopeSystem, Permissions: []string{"user.read"}})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: grantee.ID, ScopeType: store.RoleScopeSystem, CreatedBy: "test"})
	require.NoError(t, err)
	body := map[string]any{"name": rd.Name, "description": "x", "permissions": []string{"user.read", "hub.config.update"}}
	for who, tok := range map[string]string{"test_super_admin": superKind.AccessToken, "test_hub_admin": hubKind.AccessToken} {
		rec := doRequestWithToken(t, srv, tok, http.MethodPut, "/api/v1/admin/roles/"+rd.ID, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", who, rec.Body.String())
	}
	got, err := s.GetRoleDefinition(ctx, rd.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"user.read"}, got.Permissions, "the role is unchanged")
	rec := doRequestWithToken(t, srv, humanTok, http.MethodPut, "/api/v1/admin/roles/"+rd.ID, body)
	assert.Less(t, rec.Code, 300, "human super-admin: %s", rec.Body.String())
}

// Review RN1 (option b): an agent rooted at a test super-admin reads
// project secret values only in a project where its root holds a project
// membership. System authority (the super-admin role's secret.use) does
// not admit an admin-kind root. Creating a project makes the test
// super-admin its owner (the owner binding is written for the creator), so
// its agents read that project's secrets: the accepted residual.
func TestTestAdminPolicy_AgentSecretsOnlyInRootMemberProject(t *testing.T) {
	srv, s, _ := tsaServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-as-iss", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	rootID := superKind.Identity.ID

	// A project the test super-admin does not belong to (owned by a human).
	other := tid("tsa-as-other")
	taProject(t, s, other, issuer.ID)
	// A project the test super-admin creates through the API.
	rec := doRequestWithToken(t, srv, superKind.AccessToken, http.MethodPost, "/api/v1/projects", map[string]any{"name": "tsa-as-own-" + generateID()[:6]})
	require.Less(t, rec.Code, 300, rec.Body.String())
	var created struct {
		ID      string `json:"id"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	own := created.ID
	if own == "" {
		own = created.Project.ID
	}
	require.NotEmpty(t, own, rec.Body.String())
	m := srv.CheckEffectiveMembership(ctx, rootID, own)
	require.NoError(t, m.Err)
	t.Logf("test super-admin membership in its own project: %v", m.IsMember)
	require.True(t, m.IsMember, "creating a project makes the test super-admin a member (owner)")

	fetch := func(projectID string) *httptest.ResponseRecorder {
		agentID := generateID()
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "tsa-as-" + agentID[:6], Name: "tsa as",
			ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{rootID},
			Created: time.Now(), Updated: time.Now()}))
		seedSecret(t, srv.secretBackend, "TSA_SECRET", "tsa-value", "", "", projectID)
		tok, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{rootID})
		require.NoError(t, err)
		return doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"TSA_SECRET"}}, tok)
	}
	rec = fetch(other)
	assert.Equal(t, http.StatusForbidden, rec.Code, "not a member: no secret values: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "tsa-value")
	rec = fetch(own)
	assert.Equal(t, http.StatusOK, rec.Code, "owned project: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "tsa-value")

	// SystemAuthorityProof itself: no system-authority admission for an
	// admin kind on secret.use; a human super-admin keeps it.
	class := ProjectTargetClass{ResourceType: permissions.ResourceSecret, ScopeKind: store.ScopeProject}
	ok, err := srv.authzService.SystemAuthorityProof(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: rootID}, other, "secret.use", class)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = srv.authzService.SystemAuthorityProof(ctx, PrincipalContext{Kind: PrincipalKindUser, ID: issuer.ID}, other, "secret.use", class)
	require.NoError(t, err)
	assert.True(t, ok)
}

// Advisor / ti-a2-rev (fix4): the dispatch-time secret paths
// (resolveSecrets and resolveAsNeededForKeys, both after
// secretBackend.Resolve, with no per-secret authz for a top-level agent)
// clamp agents rooted at an admin-kind test identity: no project secrets
// outside a project where the root is a member, project secrets in its own
// project, never hub- or runtime_broker-scope secrets, its own user-scope
// secrets as usual. Human-rooted agents are unchanged; a kind read error
// withholds every non-user scope.
func TestTestAdminPolicy_DispatchSecretsClamp(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-ds-iss", store.SystemRoleSuperAdmin)
	superKind := tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin)
	root := superKind.Identity.ID
	ownProject := tid("tsa-ds-own")
	taProject(t, s, ownProject, root)
	otherProject := tid("tsa-ds-other")
	taProject(t, s, otherProject, issuer.ID)

	mk := func(name, scope, mode string) secret.SecretWithValue {
		return secret.SecretWithValue{SecretMeta: secret.SecretMeta{Name: name, SecretType: "environment", Target: name,
			Scope: scope, ScopeID: "x", InjectionMode: mode}, Value: "v-" + name}
	}
	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, nil)
	d.SetHubID("test-hub")
	d.SetAuthzService(srv.authzService)
	d.SetSecretBackend(&mockSecretBackend{secrets: []secret.SecretWithValue{
		mk("B_S", secret.ScopeRuntimeBroker, "always"), mk("H_S", "hub", "always"),
		mk("P_S", secret.ScopeProject, "always"), mk("U_S", secret.ScopeUser, "always"),
		mk("B_AN", secret.ScopeRuntimeBroker, "as_needed"), mk("H_AN", "hub", "as_needed"),
		mk("P_AN", secret.ScopeProject, "as_needed"), mk("U_AN", secret.ScopeUser, "as_needed"),
	}})
	agentFor := func(rootID, projectID string) *store.Agent {
		return &store.Agent{ID: generateID(), Name: "tsa-ds", Slug: "tsa-ds-" + generateID()[:6], ProjectID: projectID,
			OwnerID: rootID, Ancestry: []string{rootID}, RuntimeBrokerID: "broker-x", AppliedConfig: &store.AgentAppliedConfig{}}
	}
	first := func(a *store.Agent) map[string]bool {
		resolved, _, err := d.resolveSecrets(ctx, a)
		require.NoError(t, err)
		got := map[string]bool{}
		for _, r := range resolved {
			got[r.Name] = true
		}
		return got
	}
	second := func(a *store.Agent) map[string]string {
		return d.resolveAsNeededForKeys(ctx, a, []string{"B_AN", "H_AN", "P_AN", "U_AN"}, nil)
	}

	// Not a member: user scope only.
	got := first(agentFor(root, otherProject))
	assert.Equal(t, map[string]bool{"U_S": true}, got, "test super-admin root outside its projects")
	assert.Equal(t, map[string]string{"U_AN": "v-U_AN"}, second(agentFor(root, otherProject)))
	// Its own project: project and user scope, never hub or broker.
	got = first(agentFor(root, ownProject))
	assert.Equal(t, map[string]bool{"P_S": true, "U_S": true}, got, "test super-admin root in its own project")
	assert.Equal(t, map[string]string{"P_AN": "v-P_AN", "U_AN": "v-U_AN"}, second(agentFor(root, ownProject)))
	// A human root is unchanged.
	got = first(agentFor(issuer.ID, otherProject))
	for _, n := range []string{"B_S", "H_S", "P_S", "U_S"} {
		assert.True(t, got[n], "human root keeps %s", n)
	}
	assert.Len(t, second(agentFor(issuer.ID, otherProject)), 4)
	// A kind read error (an unrecognized kind) withholds every scope.
	unknown := taRawUnknownKindUser(t, s, store.UserRoleMember)
	assert.Empty(t, first(agentFor(unknown, otherProject)))
	// User scope is the root's own only: an agent whose owner is not its
	// provenance root gets no user-scope material.
	a := agentFor(root, ownProject)
	a.OwnerID = issuer.ID
	assert.Equal(t, map[string]bool{"P_S": true}, first(a))

	// Env vars follow the same clamp (first pass, as_needed pass and the
	// source labels all read envScopesForDispatch).
	scopes := func(a *store.Agent) []string {
		var out []string
		for _, f := range d.envScopesForDispatch(ctx, a) {
			out = append(out, f.Scope)
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, []string{store.ScopeUser}, scopes(agentFor(root, otherProject)))
	assert.Equal(t, []string{store.ScopeProject, store.ScopeUser}, scopes(agentFor(root, ownProject)))
	assert.Empty(t, scopes(agentFor(unknown, otherProject)))
	human := scopes(agentFor(issuer.ID, otherProject))
	assert.Contains(t, human, store.ScopeHub)
	assert.Contains(t, human, store.ScopeProject)
	for name, scope := range map[string]string{"TSA_HUB_ENV": store.ScopeHub, "TSA_PROJECT_ENV": store.ScopeProject, "TSA_USER_ENV": store.ScopeUser} {
		scopeID := "test-hub"
		switch scope {
		case store.ScopeProject:
			scopeID = otherProject
		case store.ScopeUser:
			scopeID = root
		}
		require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{ID: generateID(), Key: name, Value: "v-" + name, Scope: scope, ScopeID: scopeID,
			InjectionMode: store.InjectionModeAlways}))
	}
	env, _, err := d.resolveEnvFromStorage(ctx, agentFor(root, otherProject))
	require.NoError(t, err)
	assert.Equal(t, "v-TSA_USER_ENV", env["TSA_USER_ENV"])
	assert.NotContains(t, env, "TSA_HUB_ENV")
	assert.NotContains(t, env, "TSA_PROJECT_ENV")
	env, _, err = d.resolveEnvFromStorage(ctx, agentFor(issuer.ID, otherProject))
	require.NoError(t, err)
	assert.Equal(t, "v-TSA_HUB_ENV", env["TSA_HUB_ENV"], "a human root keeps hub env vars")
	assert.Equal(t, "v-TSA_PROJECT_ENV", env["TSA_PROJECT_ENV"])
}

// Delegation-grant management (ptone/scion#2131) is deny for both admin
// kinds, on the hub and on a project, like delegation adoption. The grants
// a test admin issued itself stay reachable through the issuer rule, which
// agentDelegationControlAllowed applies before it asks Decide.
func TestTestAdminPolicy_DelegationGrantManagementDenied(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-dg-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-dg-iss2", store.SystemRoleSuperAdmin)
	superKind := tsaIdentity(t, s, tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin).Identity.ID)
	hubKind := tsaIdentity(t, s, tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin).Identity.ID)
	human := tsaIdentity(t, s, taUser(t, s, "tsa-dg-human", store.SystemRoleSuperAdmin).ID)
	ids := []string{agentDelegationRevokePermission, agentDelegationCredentialRevokePermission, agentDelegationListPermission, agentDelegationReadPermission}
	for _, id := range ids {
		assert.True(t, testAdminPermissionDenied(id), id)
		for _, res := range []Resource{{Type: "hub", ID: "hub"}, {Type: "project", ID: "tsa-dg-project"}} {
			for who, u := range map[string]UserIdentity{"test_super_admin": superKind, "test_hub_admin": hubKind} {
				d := srv.authzService.Decide(ctx, AuthzRequest{
					Principal: principalContextForIdentity(u), Credential: credentialContextForIdentity(u),
					Resource: res, Action: Action(agentDelegationActionFor(id)), Permission: id,
				})
				assert.False(t, d.Allowed, "%s %s on %s", who, id, res.Type)
				assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, "%s %s on %s: %s", who, id, res.Type, d.Reason)
			}
		}
		d := srv.authzService.Decide(ctx, AuthzRequest{
			Principal: principalContextForIdentity(human), Credential: credentialContextForIdentity(human),
			Resource: Resource{Type: "hub", ID: "hub"}, Action: Action(agentDelegationActionFor(id)), Permission: id,
		})
		assert.True(t, d.Allowed, "human super-admin %s on the hub: %s", id, d.Reason)
	}
	for who, u := range map[string]UserIdentity{"test_super_admin": superKind, "test_hub_admin": hubKind} {
		c := &agentDelegationCaller{user: u, decisions: map[string]bool{}}
		own := &store.AgentDelegationGrant{AgentID: "tsa-dg-agent", IssuerUserID: u.ID(), BoundaryKind: string(permissions.BoundaryKindHub)}
		other := &store.AgentDelegationGrant{AgentID: "tsa-dg-agent", IssuerUserID: issuer.ID, BoundaryKind: string(permissions.BoundaryKindHub)}
		assert.True(t, srv.agentDelegationControlAllowed(ctx, c, own, nil, agentDelegationRevokePermission), "%s revokes a grant it issued", who)
		assert.False(t, srv.agentDelegationControlAllowed(ctx, c, other, nil, agentDelegationRevokePermission), "%s cannot revoke another user's grant", who)
		assert.False(t, srv.agentDelegationControlAllowed(ctx, c, other, nil, agentDelegationListPermission), "%s cannot list another user's grant", who)
	}
}

// Review R1 (ti-a2-rev-2): the built-in project-owner and project-admin
// roles hold the agent delegation grant management permissions, which are
// deny for a test admin's own use. Both admin kinds still grant those
// roles at project scope (testAdminGrantableAtProjectScope): members POST
// and PUT, and a member role change. Every other deny-classified
// permission stays refused at project scope, and the four permissions stay
// refused in a hub-scope (system) grant.
func TestTestAdminPolicy_ProjectRolesStayGrantable(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-pr-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-pr-iss2", store.SystemRoleSuperAdmin)
	kinds := map[string]TestAdminIdentityTokenResponse{
		"test_super_admin": tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin),
		"test_hub_admin":   tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin),
	}
	owner := taUser(t, s, "tsa-pr-owner", store.SystemRoleHubMember)
	roleID := func(name string) string {
		rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
		require.NoError(t, err)
		return rd.ID
	}
	adminRD, ownerRD, memberRD := roleID(store.ProjectRoleAdmin), roleID(store.ProjectRoleOwner), roleID(store.ProjectRoleMember)
	delegationPerms := []string{agentDelegationRevokePermission, agentDelegationCredentialRevokePermission, agentDelegationListPermission, agentDelegationReadPermission}

	for who, k := range kinds {
		projectID := tid("tsa-pr-project-" + who)
		taProject(t, s, projectID, owner.ID)
		hasRole := func(userID, roleName string) bool {
			roles, err := activeDirectProjectRoles(ctx, s, nil, false, userID, projectID)
			require.NoError(t, err)
			return roles[roleName]
		}

		// Project membership grants. Only a caller that already manages the
		// project's membership (project.manage) reaches the grant check: the
		// test super-admin does; the built-in hub-admin role holds no
		// project.manage, so a test hub-admin is refused before it, exactly
		// like a human hub-admin (the control below), and not by the
		// test-admin policy.
		expectGrant := func(rec *httptest.ResponseRecorder, what string) {
			t.Helper()
			if who == "test_super_admin" {
				assert.Less(t, rec.Code, 300, "%s %s: %s", who, what, rec.Body.String())
				return
			}
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", who, what, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"denied_action":"manage"`, "%s %s: the project.manage check refuses", who, what)
			assert.NotContains(t, rec.Body.String(), "test-admin policy", "%s %s: not the test-admin policy", who, what)
			assert.NotContains(t, rec.Body.String(), "test_admin_policy", "%s %s: not the test-admin policy", who, what)
		}
		wantRole := func(userID, roleName string) {
			t.Helper()
			assert.Equal(t, who == "test_super_admin", hasRole(userID, roleName), "%s: %s binding for the grantee", who, roleName)
		}
		// Members POST with project-admin and with project-owner.
		for _, tc := range []struct{ role, rd string }{{store.ProjectRoleAdmin, adminRD}, {store.ProjectRoleOwner, ownerRD}} {
			u := taUser(t, s, "tsa-pr-post-"+tc.role+"-"+who, store.SystemRoleHubMember)
			rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/projects/"+projectID+"/members",
				addProjectMemberRequest{RoleDefinitionID: tc.rd, PrincipalType: "user", PrincipalID: u.ID})
			expectGrant(rec, "members POST "+tc.role)
			wantRole(u.ID, tc.role)
		}
		// Members PUT with project-admin and with project-owner.
		for _, tc := range []struct{ role, rd string }{{store.ProjectRoleAdmin, adminRD}, {store.ProjectRoleOwner, ownerRD}} {
			u := taUser(t, s, "tsa-pr-put-"+tc.role+"-"+who, store.SystemRoleHubMember)
			rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPut, "/api/v1/projects/"+projectID+"/members/principals/user/"+u.ID,
				setMemberRolesRequestBody{RoleDefinitionIDs: []string{tc.rd}})
			expectGrant(rec, "members PUT "+tc.role)
			wantRole(u.ID, tc.role)
		}
		// Member role change from project-member to project-admin.
		changed := taUser(t, s, "tsa-pr-change-"+who, store.SystemRoleHubMember)
		b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: memberRD, PrincipalType: store.RoleBindingPrincipalUser,
			PrincipalID: changed.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
		require.NoError(t, err)
		rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPatch, "/api/v1/projects/"+projectID+"/members/"+b.ID,
			updateProjectMemberRequest{RoleDefinitionID: adminRD})
		expectGrant(rec, "member role change to project-admin")
		wantRole(changed.ID, store.ProjectRoleAdmin)
		// The refusal for a hub-admin is the project.manage decision, not
		// the test-admin policy.
		d := srv.authzService.Decide(ctx, AuthzRequest{
			Principal: principalContextForIdentity(tsaIdentity(t, s, k.Identity.ID)), Credential: credentialContextForIdentity(tsaIdentity(t, s, k.Identity.ID)),
			Resource: Resource{Type: "project", ID: projectID}, Action: Action(permissions.ActionManage), Permission: "project.manage",
		})
		assert.Equal(t, who == "test_super_admin", d.Allowed, "%s project.manage on the project: %s", who, d.Reason)
		assert.NotEqual(t, DeniedByTestAdminPolicy, d.DeniedBy, "%s project.manage: not the test-admin policy", who)

		// A project-scope custom role holding the four is grantable; one
		// holding another deny-classified permission is not.
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/roles",
			map[string]any{"name": "tsa-pr-pdeleg-" + generateID()[:8], "scopeType": "project", "permissions": delegationPerms})
		assert.Less(t, rec.Code, 300, "%s project-scope custom role with the delegation management permissions: %s", who, rec.Body.String())
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/roles",
			map[string]any{"name": "tsa-pr-psecret-" + generateID()[:8], "scopeType": "project", "permissions": []string{"agent.read", "secret.use"}})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s project-scope custom role with secret.use: %s", who, rec.Body.String())
		// Set-complete, not any-match: one of the four together with
		// another deny-classified permission is still refused.
		for _, extra := range []string{"secret.use", "hub.config.update"} {
			rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/roles",
				map[string]any{"name": "tsa-pr-pmixed-" + generateID()[:8], "scopeType": "project", "permissions": []string{agentDelegationListPermission, extra}})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s project-scope custom role with %s and %s: %s", who, agentDelegationListPermission, extra, rec.Body.String())
		}

		// The four stay refused in a hub-scope (system) grant: a system
		// custom role, and a system role binding of a role holding them.
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/roles",
			map[string]any{"name": "tsa-pr-sdeleg-" + generateID()[:8], "scopeType": "system", "permissions": delegationPerms})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s system-scope custom role with the delegation management permissions: %s", who, rec.Body.String())
		sysRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: generateID(), Name: "tsa-pr-sysrole-" + generateID()[:6],
			ScopeType: store.RoleScopeSystem, Permissions: delegationPerms})
		require.NoError(t, err)
		grantee := taUser(t, s, "tsa-pr-sysgrantee-"+who, store.SystemRoleHubMember)
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, "/api/v1/admin/role-bindings",
			map[string]any{"roleDefinitionId": sysRole.ID, "principalType": "user", "principalId": grantee.ID, "scopeType": "system"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s system role binding of a role holding them: %s", who, rec.Body.String())

		// The refusals are the test-admin policy's.
		id := tsaIdentity(t, s, k.Identity.ID)
		for _, tc := range []struct {
			name  string
			grant GrantDescriptor
			allow bool
		}{
			{"project-scope custom role with the four", GrantDescriptor{Type: GrantTypeCustomRole, CustomRolePermissions: delegationPerms, ScopeType: store.RoleScopeProject}, true},
			{"project-scope custom role with secret.use", GrantDescriptor{Type: GrantTypeCustomRole, CustomRolePermissions: []string{"secret.use"}, ScopeType: store.RoleScopeProject}, false},
			{"system-scope custom role with the four", GrantDescriptor{Type: GrantTypeCustomRole, CustomRolePermissions: delegationPerms, ScopeType: store.RoleScopeSystem}, false},
			{"hub-scope role binding with the four", GrantDescriptor{Type: GrantTypeRoleBinding, RolePermissions: delegationPerms, ScopeType: "hub"}, false},
			{"system-scope role binding with the four", GrantDescriptor{Type: GrantTypeRoleBinding, RolePermissions: delegationPerms, ScopeType: store.RoleScopeSystem}, false},
			{"project role binding of project-admin", GrantDescriptor{Type: GrantTypeRoleBinding, RoleDefinitionID: adminRD, ScopeType: store.RoleScopeProject, ScopeID: projectID}, true},
			{"project membership of project-owner", GrantDescriptor{Type: GrantTypeProjectMembership, RoleDefinitionID: ownerRD, ScopeType: store.RoleScopeProject, ScopeID: projectID}, true},
			{"project-scope custom role mixing the four with secret.use", GrantDescriptor{Type: GrantTypeCustomRole, CustomRolePermissions: append([]string{"secret.use"}, delegationPerms...), ScopeType: store.RoleScopeProject}, false},
			{"project-scope custom role mixing one of the four with hub.config.update", GrantDescriptor{Type: GrantTypeCustomRole, CustomRolePermissions: []string{agentDelegationReadPermission, "hub.config.update"}, ScopeType: store.RoleScopeProject}, false},
			{"project-admin role named with a system scope", GrantDescriptor{Type: GrantTypeRoleBinding, RoleDefinitionID: adminRD, ScopeType: store.RoleScopeSystem}, false},
			{"system role holding the four named with a project scope", GrantDescriptor{Type: GrantTypeRoleBinding, RoleDefinitionID: sysRole.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID}, false},
		} {
			refusal := srv.authzService.refuseTestAdminDeniedGrant(ctx, id, tc.grant)
			if tc.allow {
				assert.Nil(t, refusal, "%s %s", who, tc.name)
				continue
			}
			if assert.NotNil(t, refusal, "%s %s", who, tc.name) {
				assert.Equal(t, DeniedByTestAdminPolicy, refusal.DeniedBy, "%s %s", who, tc.name)
			}
		}
	}

	// Human hub-admin control: the same project membership grants are
	// refused by the same project.manage check.
	humanHubAdmin := taUser(t, s, "tsa-pr-human-hub-admin", store.SystemRoleHubAdmin)
	humanTok := taTokenFor(t, srv, humanHubAdmin)
	controlProject := tid("tsa-pr-project-human")
	taProject(t, s, controlProject, owner.ID)
	for _, tc := range []struct{ role, rd string }{{store.ProjectRoleAdmin, adminRD}, {store.ProjectRoleOwner, ownerRD}} {
		u := taUser(t, s, "tsa-pr-human-"+tc.role, store.SystemRoleHubMember)
		rec := doRequestWithToken(t, srv, humanTok, http.MethodPost, "/api/v1/projects/"+controlProject+"/members",
			addProjectMemberRequest{RoleDefinitionID: tc.rd, PrincipalType: "user", PrincipalID: u.ID})
		assert.Equal(t, http.StatusForbidden, rec.Code, "human hub-admin members POST %s: %s", tc.role, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"denied_action":"manage"`, "human hub-admin members POST %s", tc.role)
		rec = doRequestWithToken(t, srv, humanTok, http.MethodPut, "/api/v1/projects/"+controlProject+"/members/principals/user/"+u.ID,
			setMemberRolesRequestBody{RoleDefinitionIDs: []string{tc.rd}})
		assert.Equal(t, http.StatusForbidden, rec.Code, "human hub-admin members PUT %s: %s", tc.role, rec.Body.String())
	}
}

// Review N1 (ti-a2-rev-2): agent delegation grant management over HTTP
// with a test admin's own token. The admin token is admitted as a user
// credential (not hub-only): the test admin reads and revokes a grant it
// issued and reads a grant on an agent it owns; another user's grant,
// hub-bounded or bounded to a project, is not found. A human project admin
// reads the project-bounded grant (the project half of the decision can
// allow).
func TestTestAdminPolicy_DelegationGrantManagementHTTP(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-dgh-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-dgh-iss2", store.SystemRoleSuperAdmin)
	kinds := map[string]TestAdminIdentityTokenResponse{
		"test_super_admin": tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin),
		"test_hub_admin":   tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin),
	}
	owner := taUser(t, s, "tsa-dgh-owner", store.SystemRoleHubMember)
	projectID := tid("tsa-dgh-project")
	taProject(t, s, projectID, owner.ID)
	projectAdmin := taUser(t, s, "tsa-dgh-padmin", store.SystemRoleHubMember)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: adminRD.ID, PrincipalType: store.RoleBindingPrincipalUser,
		PrincipalID: projectAdmin.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)

	agent := func(ownerID string) *store.Agent {
		id := generateID()
		a := &store.Agent{ID: id, Slug: "tsa-dgh-" + id[:6], Name: "tsa dgh", ProjectID: projectID, Phase: "stopped",
			CreatedBy: ownerID, OwnerID: ownerID}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	grant := func(a *store.Agent, issuerID, boundaryKind, boundaryProject string) *store.AgentDelegationGrant {
		g := &store.AgentDelegationGrant{AgentID: a.ID, AgentProjectID: a.ProjectID, AgentGeneration: a.Generation, AgentStateVersion: a.StateVersion,
			IssuerUserID: issuerID, BoundaryKind: boundaryKind, BoundaryProjectID: boundaryProject, CeilingVersion: 1,
			CeilingPermissionIDs: []string{"agent.read"}, Name: "tsa-dgh", MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL,
			ExpiresAt: time.Now().Add(time.Hour), IssuanceAuditID: "00000000-0000-0000-0000-000000000001"}
		require.NoError(t, s.CreateAgentDelegationGrant(ctx, g))
		return g
	}
	path := func(g *store.AgentDelegationGrant) string {
		return "/api/v1/agents/" + g.AgentID + "/delegations/" + g.ID
	}
	ownerAgent := agent(owner.ID)
	otherHub := grant(ownerAgent, owner.ID, "hub", "")
	otherProject := grant(ownerAgent, owner.ID, "project", projectID)

	for who, k := range kinds {
		own := grant(ownerAgent, k.Identity.ID, "hub", "")
		ownedAgent := agent(k.Identity.ID)
		ownerRule := grant(ownedAgent, owner.ID, "hub", "")

		rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, path(own), nil)
		assert.Equal(t, http.StatusOK, rec.Code, "%s reads its own grant: %s", who, rec.Body.String())
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, path(ownerRule), nil)
		assert.Equal(t, http.StatusOK, rec.Code, "%s reads a grant on an agent it owns: %s", who, rec.Body.String())
		for name, g := range map[string]*store.AgentDelegationGrant{"hub-bounded": otherHub, "project-bounded": otherProject} {
			rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodGet, path(g), nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s reads another user's %s grant: %s", who, name, rec.Body.String())
			rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodDelete, path(g), nil)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s revokes another user's %s grant: %s", who, name, rec.Body.String())
		}
		rec = doRequestWithToken(t, srv, k.AccessToken, http.MethodDelete, path(own), nil)
		assert.Less(t, rec.Code, 300, "%s revokes its own grant: %s", who, rec.Body.String())
		g, err := s.GetAgentDelegationGrant(ctx, own.ID)
		require.NoError(t, err)
		assert.NotNil(t, g.RevokedAt, "%s revoked its own grant", who)
	}
	for _, g := range []*store.AgentDelegationGrant{otherHub, otherProject} {
		row, err := s.GetAgentDelegationGrant(ctx, g.ID)
		require.NoError(t, err)
		assert.Nil(t, row.RevokedAt, "another user's grant is not revoked")
	}

	// Human control at project scope.
	rec := doRequestAsUser(t, srv, projectAdmin, http.MethodGet, path(otherProject), nil)
	assert.Equal(t, http.StatusOK, rec.Code, "a human project admin reads the project-bounded grant: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, projectAdmin, http.MethodGet, path(otherHub), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "a human project admin does not reach a hub-bounded grant: %s", rec.Body.String())
}

// Review N5 (ti-a2-rev-2): a group membership carries no grant scope, so
// the project-scope exemption never applies to it. A test admin adding a
// user to a group that holds project-admin on a project is refused by the
// test-admin policy (both kinds); a human super-admin adds the member.
func TestTestAdminPolicy_GroupWithProjectRoleRefused(t *testing.T) {
	srv, s, _ := tsaServer(t)
	ctx := context.Background()
	issuer := taUser(t, s, "tsa-gpr-iss", store.SystemRoleSuperAdmin)
	issuer2 := taUser(t, s, "tsa-gpr-iss2", store.SystemRoleSuperAdmin)
	kinds := map[string]TestAdminIdentityTokenResponse{
		"test_super_admin": tsaIssue(t, srv, issuer, testAdminRoleSuperAdmin),
		"test_hub_admin":   tsaIssue(t, srv, issuer2, testAdminRoleHubAdmin),
	}
	owner := taUser(t, s, "tsa-gpr-owner", store.SystemRoleHubMember)
	projectID := tid("tsa-gpr-project")
	taProject(t, s, projectID, owner.ID)
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	group := tiGroupWithSystemRole(t, s, "tsa-gpr-group-"+generateID()[:6], "", issuer.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: adminRD.ID, PrincipalType: store.RoleBindingPrincipalGroup,
		PrincipalID: group.ID, ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test"})
	require.NoError(t, err)
	path := "/api/v1/groups/" + group.ID + "/members"

	for who, k := range kinds {
		u := taUser(t, s, "tsa-gpr-user-"+who, store.SystemRoleHubMember)
		rec := doRequestWithToken(t, srv, k.AccessToken, http.MethodPost, path, map[string]any{"memberType": "user", "memberId": u.ID, "role": "member"})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s adds a member to a group holding project-admin: %s", who, rec.Body.String())
		d := srv.authzService.refuseTestAdminDeniedGrant(ctx, tsaIdentity(t, s, k.Identity.ID), GrantDescriptor{Type: GrantTypeGroupMembership,
			GroupID: group.ID, GranteeType: "user", GranteeID: u.ID})
		if assert.NotNil(t, d, who) {
			assert.Equal(t, DeniedByTestAdminPolicy, d.DeniedBy, "%s: %s", who, d.Reason)
		}
	}
	human := taUser(t, s, "tsa-gpr-human-member", store.SystemRoleHubMember)
	rec := doRequestWithToken(t, srv, taTokenFor(t, srv, issuer), http.MethodPost, path, map[string]any{"memberType": "user", "memberId": human.ID, "role": "member"})
	assert.Less(t, rec.Code, 300, "human super-admin adds the member: %s", rec.Body.String())
}
