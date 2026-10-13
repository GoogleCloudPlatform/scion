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
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// igProject creates a project with the given name.
func igProject(t *testing.T, s store.Store, name string) string {
	t.Helper()
	id := tid(name)
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{ID: id, Name: name, Slug: tidSlugSafe(name)}))
	return id
}

// igProjectBinding binds userID to projectID with the named project role and
// returns the binding ID.
func igProjectBinding(t *testing.T, s store.Store, userID, projectID, roleName string) string {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	return b.ID
}

// igMintRefused requires that a token for userID with the boundary and
// selectors cannot be minted.
func igMintRefused(t *testing.T, srv *Server, userID string, boundary TokenBoundary, scopes ...string) {
	t.Helper()
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "ig-" + tid("refused"), Boundary: boundary, Scopes: scopes,
	})
	require.Error(t, err, "a %s token with %v must not mint", boundary.Kind, scopes)
}

// igRequestWithCredential calls handler directly with identity and an
// explicit credential context of kind.
func igRequestWithCredential(handler http.HandlerFunc, identity Identity, kind CredentialKind, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: kind})
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))
	return rec
}

// TestUserListToken_RequiresUserListAndHubBoundary pins the users list for
// tokens: user:list at the hub collection on a hub boundary admits the
// request, and each row also needs user:read. A token with user:read but no
// user:list is refused, a token with user:list but no user:read lists no
// one, user:list cannot be minted on a project boundary, and a
// project-boundary credential carrying both selectors is refused. A session
// lists users as it does without these rules.
func TestUserListToken_RequiresUserListAndHubBoundary(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "ig-list-super", store.SystemRoleSuperAdmin)
	listed := hubConfigTokenUser(t, s, "ig-list-listed", store.SystemRoleHubMember)

	listIDs := func(rec *httptest.ResponseRecorder) ([]string, int) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp struct {
			Users []struct {
				ID string `json:"id"`
			} `json:"users"`
			TotalCount int `json:"totalCount"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := make([]string, 0, len(resp.Users))
		for _, u := range resp.Users {
			ids = append(ids, u.ID)
		}
		return ids, resp.TotalCount
	}

	both := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:list", "user:read")
	ids, total := listIDs(doRequestWithToken(t, srv, both, http.MethodGet, "/api/v1/users", nil))
	assert.Contains(t, ids, listed)
	assert.Equal(t, len(ids), total)

	listOnly := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:list")
	ids, total = listIDs(doRequestWithToken(t, srv, listOnly, http.MethodGet, "/api/v1/users", nil))
	assert.Empty(t, ids, "user:list without user:read lists no one")
	assert.Zero(t, total)

	readOnly := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:read")
	rec := doRequestWithToken(t, srv, readOnly, http.MethodGet, "/api/v1/users", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "user:read without user:list: %s", rec.Body.String())

	project := igProject(t, s, "ig-list-project")
	igProjectBinding(t, s, admin, project, store.ProjectRoleOwner)
	igMintRefused(t, srv, admin, projectBoundary(project), "user:list")

	adminUser := NewAuthenticatedUser(admin, admin+"@test.com", admin, "admin", "api")
	projectCredential := NewScopedUserIdentityWithBoundaryAndDecoration(adminUser, projectBoundary(project),
		[]string{"user:list", "user:read"}, tid("ig-list-project-cred"), bearerCeiling(t, "user:list", "user:read"), nil)
	rec = doRequestAsIdentity(t, srv, projectCredential, http.MethodGet, "/api/v1/users", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project boundary: %s", rec.Body.String())

	ids, _ = listIDs(doRequest(t, srv, http.MethodGet, "/api/v1/users", nil))
	assert.Contains(t, ids, listed, "a session lists users")
}

// TestRoleReadToken_DoesNotAdmitRoleWrites pins that role:read and
// role_binding:read admit the role definition, permission registry and role
// binding reads on a hub boundary, each read needs its own selector, the
// selectors cannot be minted on a project boundary, and a token carrying
// both is refused on every role and role binding write.
func TestRoleReadToken_DoesNotAdmitRoleWrites(t *testing.T) {
	m := newBearerMatrixFixture(t)
	srv := m.srv

	reads := m.mint(t, hubBoundary(), []string{"role:read", "role_binding:read"})
	for _, path := range []string{
		"/api/v1/admin/roles",
		"/api/v1/admin/roles/" + m.ids.roleDefinition,
		"/api/v1/admin/roles/export",
		"/api/v1/admin/roles/" + m.ids.roleDefinition + "/export",
		"/api/v1/admin/permissions",
		"/api/v1/admin/role-bindings",
		"/api/v1/admin/role-bindings/user/" + m.ids.userDel,
	} {
		rec := doRequestWithToken(t, srv, reads, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", path, rec.Body.String())
	}

	roleOnly := m.mint(t, hubBoundary(), []string{"role:read"})
	rec := doRequestWithToken(t, srv, roleOnly, http.MethodGet, "/api/v1/admin/role-bindings", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "role bindings need role_binding:read: %s", rec.Body.String())
	bindingOnly := m.mint(t, hubBoundary(), []string{"role_binding:read"})
	rec = doRequestWithToken(t, srv, bindingOnly, http.MethodGet, "/api/v1/admin/roles", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "role definitions need role:read: %s", rec.Body.String())
	unrelated := m.mint(t, hubBoundary(), []string{"project:read"})
	for _, path := range []string{"/api/v1/admin/roles", "/api/v1/admin/permissions", "/api/v1/admin/role-bindings"} {
		rec = doRequestWithToken(t, srv, unrelated, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "GET %s without the selector: %s", path, rec.Body.String())
	}

	igMintRefused(t, srv, m.adminID, projectBoundary(m.otherProject), "role:read")
	igMintRefused(t, srv, m.adminID, projectBoundary(m.otherProject), "role_binding:read")

	rec = doRequestWithToken(t, srv, reads, http.MethodPost, "/api/v1/admin/role-bindings", map[string]interface{}{
		"roleDefinitionId": m.ids.roleDefinition, "principalType": "user", "principalId": m.ids.user, "scopeType": "system",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "role binding create: %s", rec.Body.String())
	rec = doRequestWithToken(t, srv, reads, http.MethodDelete, "/api/v1/admin/role-bindings/"+m.ids.roleBindingDel, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "role binding delete: %s", rec.Body.String())
	_, err := m.store.GetRoleBinding(context.Background(), m.ids.roleBindingDel)
	assert.NoError(t, err, "the refused delete leaves the binding")
}

// TestRoleRoutesToken_WriteMethodsStillDenied pins that the role routes'
// hub-level read target admits no write: a super-admin hub token carrying
// every mintable selector, role:read included, is refused on every role
// definition write and changes nothing.
func TestRoleRoutesToken_WriteMethodsStillDenied(t *testing.T) {
	m := newBearerMatrixFixture(t)
	ctx := context.Background()
	all, selectors := m.everySelectorHubToken(t)
	require.Contains(t, selectors, "role:read")
	before, err := m.store.ListRoleDefinitions(ctx)
	require.NoError(t, err)

	role := m.ids.roleDefinition
	writes := []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodPost, "/api/v1/admin/roles", map[string]interface{}{"name": "ig-created", "scopeType": "system", "permissions": []string{"agent.read"}}},
		{http.MethodPut, "/api/v1/admin/roles/" + role, map[string]interface{}{"description": "changed", "permissions": []string{"agent.read", "agent.list"}}},
		{http.MethodPost, "/api/v1/admin/roles/" + role + "/duplicate", map[string]interface{}{"name": "ig-duplicate"}},
		{http.MethodPost, "/api/v1/admin/roles/import", map[string]interface{}{"roles": []interface{}{}}},
		{http.MethodDelete, "/api/v1/admin/roles/" + role, nil},
	}
	for _, w := range writes {
		rec := doRequestWithToken(t, m.srv, all, w.method, w.path, w.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", w.method, w.path, rec.Body.String())
	}

	after, err := m.store.ListRoleDefinitions(ctx)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "no role definition is created or deleted")
	got, err := m.store.GetRoleDefinition(ctx, role)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.read"}, got.Permissions, "the role definition keeps its permissions")
}

// TestConstraintReadToken_DoesNotAdmitConstraintAdmin pins that
// access_constraint:read admits the access constraint list, detail and
// affected-principals reads on a hub boundary, cannot be minted on a project
// boundary, does not reach a constraint's audit history, and is refused on
// every constraint write and on the constraint draft route.
func TestConstraintReadToken_DoesNotAdmitConstraintAdmin(t *testing.T) {
	m := newBearerMatrixFixture(t)
	srv := m.srv
	ac := m.ids.accessConstraint

	reads := m.mint(t, hubBoundary(), []string{"access_constraint:read"})
	for _, path := range []string{
		"/api/v1/admin/access-constraints",
		"/api/v1/admin/access-constraints/" + ac,
		"/api/v1/admin/access-constraints/" + ac + "/affected-principals",
	} {
		rec := doRequestWithToken(t, srv, reads, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", path, rec.Body.String())
	}
	rec := doRequestWithToken(t, srv, reads, http.MethodGet, "/api/v1/admin/access-constraints/"+ac+"/audit", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "audit history needs hub_audit:read: %s", rec.Body.String())

	unrelated := m.mint(t, hubBoundary(), []string{"project:read"})
	rec = doRequestWithToken(t, srv, unrelated, http.MethodGet, "/api/v1/admin/access-constraints", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "without the selector: %s", rec.Body.String())

	igMintRefused(t, srv, m.adminID, projectBoundary(m.otherProject), "access_constraint:read")

	for _, w := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/admin/access-constraints"},
		{http.MethodPut, "/api/v1/admin/access-constraints/" + ac},
		{http.MethodDelete, "/api/v1/admin/access-constraints/" + ac},
		{http.MethodPost, "/api/v1/admin/access-constraint-previews"},
	} {
		rec = doRequestWithToken(t, srv, reads, w.method, w.path, map[string]interface{}{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", w.method, w.path, rec.Body.String())
	}
	_, err := m.store.GetAccessConstraint(context.Background(), ac)
	assert.NoError(t, err, "the refused delete leaves the constraint")
}

// TestConstraintWriteRoutesToken_WriteMethodsStillDenied pins that the
// access constraint routes' hub-level read target admits no write: a
// super-admin hub token carrying every mintable selector,
// access_constraint:read included, is refused on every constraint write and
// on the constraint draft route, and changes nothing.
func TestConstraintWriteRoutesToken_WriteMethodsStillDenied(t *testing.T) {
	m := newBearerMatrixFixture(t)
	ctx := context.Background()
	all, selectors := m.everySelectorHubToken(t)
	require.Contains(t, selectors, "access_constraint:read")
	ac := m.ids.accessConstraint
	before, err := m.store.GetAccessConstraint(ctx, ac)
	require.NoError(t, err)

	for _, w := range []struct {
		method, path string
		body         interface{}
	}{
		{http.MethodPost, "/api/v1/admin/access-constraints", map[string]interface{}{"name": "ig-created"}},
		{http.MethodPut, "/api/v1/admin/access-constraints/" + ac, map[string]interface{}{"name": "ig-changed"}},
		{http.MethodDelete, "/api/v1/admin/access-constraints/" + ac, nil},
		{http.MethodPost, "/api/v1/admin/access-constraint-previews", map[string]interface{}{"name": "ig-draft"}},
	} {
		rec := doRequestWithToken(t, m.srv, all, w.method, w.path, w.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", w.method, w.path, rec.Body.String())
	}

	after, err := m.store.GetAccessConstraint(ctx, ac)
	require.NoError(t, err, "the constraint still exists")
	assert.Equal(t, before.Name, after.Name)
	assert.Equal(t, before.Revision, after.Revision, "the constraint keeps its revision")
}

// TestConstraintAuditToken_RequiresHubAuditReadOnHubBoundary pins that a
// hub token with hub_audit:read reads the audit history of a system and of
// a project-scoped constraint, that a hub token without hub_audit:read and
// a project-boundary credential carrying it both get the not-found answer,
// and that hub_audit:read cannot be minted on a project boundary.
func TestConstraintAuditToken_RequiresHubAuditReadOnHubBoundary(t *testing.T) {
	m := newBearerMatrixFixture(t)
	ctx := context.Background()
	subjectType := "user"
	subjectID := m.ids.userDel
	projectAC, err := m.store.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "ig-project-ac", SubjectKind: "principal", SubjectPrincipalType: &subjectType, SubjectPrincipalID: &subjectID,
		ScopeType: store.RoleScopeProject, ScopeID: m.ids.project, MaximumPermissions: []string{"agent.read"},
		Purpose: "audit token test fixture", CreatedBy: m.ids.user, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	require.NoError(t, err)
	paths := []string{
		"/api/v1/admin/access-constraints/" + m.ids.accessConstraint + "/audit",
		"/api/v1/admin/access-constraints/" + projectAC.ID + "/audit",
	}

	audit := m.mint(t, hubBoundary(), []string{"hub_audit:read"})
	without := m.mint(t, hubBoundary(), []string{"access_constraint:read"})
	adminUser := NewAuthenticatedUser(m.adminID, m.adminID+"@test.com", m.adminID, "admin", "api")
	projectCredential := NewScopedUserIdentityWithBoundaryAndDecoration(adminUser, projectBoundary(m.ids.project),
		[]string{"hub_audit:read"}, tid("ig-audit-project-cred"), bearerCeiling(t, "hub_audit:read"), nil)
	for _, path := range paths {
		rec := doRequestWithToken(t, m.srv, audit, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "hub_audit:read %s: %s", path, rec.Body.String())
		rec = doRequestWithToken(t, m.srv, without, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "without hub_audit:read %s: %s", path, rec.Body.String())
		rec = doRequestAsIdentity(t, m.srv, projectCredential, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "project boundary %s: %s", path, rec.Body.String())
		rec = doRequest(t, m.srv, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "session %s: %s", path, rec.Body.String())
	}
	igMintRefused(t, m.srv, m.adminID, projectBoundary(m.otherProject), "hub_audit:read")
}

// TestHubAuditToken_EffectiveAccessAndCrossPrincipalExplain pins that
// hub_audit:read on a hub boundary admits the effective-access read and an
// explain for another principal, and that a token without it, or a
// project-boundary credential carrying it, is refused.
func TestHubAuditToken_EffectiveAccessAndCrossPrincipalExplain(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "ig-audit-super", store.SystemRoleSuperAdmin)
	other := hubConfigTokenUser(t, s, "ig-audit-other", store.SystemRoleHubMember)
	effective := "/api/v1/admin/effective-access?principalType=user&principalId=" + other
	explain := map[string]interface{}{"resource": map[string]interface{}{"type": "user", "id": other}, "action": "read", "principalId": other}

	audit := mintHubConfigToken(t, srv, admin, hubBoundary(), "hub_audit:read")
	rec := doRequestWithToken(t, srv, audit, http.MethodGet, effective, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, audit, http.MethodHead, effective, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, audit, http.MethodPost, "/api/v1/authz/explain", explain)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	without := mintHubConfigToken(t, srv, admin, hubBoundary(), "user:read")
	rec = doRequestWithToken(t, srv, without, http.MethodGet, effective, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithToken(t, srv, without, http.MethodPost, "/api/v1/authz/explain", explain)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	adminUser := NewAuthenticatedUser(admin, admin+"@test.com", admin, "admin", "api")
	projectCredential := NewScopedUserIdentityWithBoundaryAndDecoration(adminUser, projectBoundary(tid("ig-audit-project")),
		[]string{"hub_audit:read"}, tid("ig-audit-cred"), bearerCeiling(t, "hub_audit:read"), nil)
	rec = doRequestAsIdentity(t, srv, projectCredential, http.MethodGet, effective, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestAsIdentity(t, srv, projectCredential, http.MethodPost, "/api/v1/authz/explain", explain)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestAdminStatusToken_ReportsCeilingIntersection pins that admin-status
// for a token lists only the holder's system-scope permissions that the
// token's ceiling allows and that are eligible for its boundary kind, while
// a session sees every permission.
func TestAdminStatusToken_ReportsCeilingIntersection(t *testing.T) {
	srv, s := testServer(t)
	admin := hubConfigTokenUser(t, s, "ig-status-super", store.SystemRoleSuperAdmin)
	project := igProject(t, s, "ig-status-project")
	igProjectBinding(t, s, admin, project, store.ProjectRoleOwner)

	status := func(rec *httptest.ResponseRecorder) AdminStatusResponse {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp AdminStatusResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	hubToken := mintHubConfigToken(t, srv, admin, hubBoundary(), "role:read", "hub_audit:read", "agent:read")
	got := status(doRequestWithToken(t, srv, hubToken, http.MethodGet, "/api/v1/auth/admin-status", nil))
	assert.Equal(t, []string{"agent.read", "hub.audit.read", "role.read"}, got.Permissions)
	assert.False(t, got.IsSuperAdmin)

	projectToken := mintHubConfigToken(t, srv, admin, projectBoundary(project), "agent:read", "project:read")
	got = status(doRequestWithToken(t, srv, projectToken, http.MethodGet, "/api/v1/auth/admin-status", nil))
	assert.Equal(t, []string{"agent.read", "project.read"}, got.Permissions)

	adminSession := NewAuthenticatedUser(admin, admin+"@test.com", admin, "admin", "web")
	got = status(doRequestAsIdentity(t, srv, adminSession, http.MethodGet, "/api/v1/auth/admin-status", nil))
	assert.Contains(t, got.Permissions, "role.create")
	assert.Contains(t, got.Permissions, "hub.audit.read")
	assert.True(t, sort.StringsAreSorted(got.Permissions))
}

// TestExplainSelfToken_ProjectBoundaryLimitedToItsProject pins that a
// project-boundary token explains its holder's own access only on targets
// in its boundary project, lists only the effective permissions its ceiling
// allows, and loses the explain when its holder stops being a member of the
// project; a hub token explains in any project.
func TestExplainSelfToken_ProjectBoundaryLimitedToItsProject(t *testing.T) {
	srv, s := testServer(t)
	user := hubConfigTokenUser(t, s, "ig-explain-user", store.SystemRoleHubMember)
	projectA := igProject(t, s, "ig-explain-a")
	projectB := igProject(t, s, "ig-explain-b")
	bindingA := igProjectBinding(t, s, user, projectA, store.ProjectRoleOwner)
	igProjectBinding(t, s, user, projectB, store.ProjectRoleOwner)

	explain := func(key string, body map[string]interface{}) *httptest.ResponseRecorder {
		return doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/authz/explain", body)
	}
	projectTarget := func(id string) map[string]interface{} {
		return map[string]interface{}{"resource": map[string]interface{}{"type": "project", "id": id}, "action": "read"}
	}

	tokenA := mintHubConfigToken(t, srv, user, projectBoundary(projectA), "project:read")
	rec := explain(tokenA, projectTarget(projectA))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Allowed)

	rec = explain(tokenA, projectTarget(projectB))
	assert.Equal(t, http.StatusForbidden, rec.Code, "another project: %s", rec.Body.String())
	rec = explain(tokenA, map[string]interface{}{"resource": map[string]interface{}{"type": "agent", "id": tid("ig-explain-agent"), "projectId": projectB}, "action": "read"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a target in another project: %s", rec.Body.String())
	rec = explain(tokenA, map[string]interface{}{"resource": map[string]interface{}{"type": "user", "id": user}, "action": "read"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a target in no project: %s", rec.Body.String())

	effective := projectTarget(projectA)
	effective["mode"] = "effective_permissions"
	rec = explain(tokenA, effective)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp = explainResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.EffectivePermissions)
	for _, p := range resp.EffectivePermissions {
		assert.Equal(t, "project.read", p.PermissionID, "only the permissions the ceiling allows are listed")
	}

	hubToken := mintHubConfigToken(t, srv, user, hubBoundary(), "project:read")
	rec = explain(hubToken, projectTarget(projectB))
	assert.Equal(t, http.StatusOK, rec.Code, "a hub token explains in any project: %s", rec.Body.String())

	require.NoError(t, s.DeleteRoleBinding(context.Background(), bindingA))
	rec = explain(tokenA, projectTarget(projectA))
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
		"a holder who left the boundary project is refused: %s", rec.Body.String())
}

// TestInviteRedeem_SessionOnly pins that invite redemption is session-only
// (IDENTITY_BINDING): a user access token and a broker credential acting for
// the user are refused before the code is used, and a session redeems.
func TestInviteRedeem_SessionOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := hubConfigTokenUser(t, s, "ig-redeem-admin", store.SystemRoleSuperAdmin)
	user := hubConfigTokenUser(t, s, "ig-redeem-user", store.SystemRoleHubMember)
	code, invite, err := srv.inviteService.CreateInvite(ctx, admin, time.Now().Add(time.Hour), 3, "session-only test")
	require.NoError(t, err)
	body := map[string]string{"code": code}

	token := mintHubConfigToken(t, srv, user, hubBoundary(), "user:read")
	rec := doRequestWithToken(t, srv, token, http.MethodPost, "/api/v1/auth/invite/redeem", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	reason, credential := sessionOnlyDetailsOf(rec)
	assert.Equal(t, string(authzop.ReasonIdentityBinding), reason)
	assert.Equal(t, sessionRequiredCredential, credential)

	member := NewAuthenticatedUser(user, user+"@test.com", user, "member", "web")
	rec = igRequestWithCredential(srv.handleInviteRedeem, member, CredentialKindBroker, http.MethodPost, "/api/v1/auth/invite/redeem", `{"code":"`+code+`"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	reason, _ = sessionOnlyDetailsOf(rec)
	assert.Equal(t, string(authzop.ReasonIdentityBinding), reason)

	got, err := s.GetInviteCode(ctx, invite.ID)
	require.NoError(t, err)
	assert.Zero(t, got.UseCount, "a refused redeem does not use the invite")

	rec = doRequestAsIdentity(t, srv, member, http.MethodPost, "/api/v1/auth/invite/redeem", body)
	assert.Equal(t, http.StatusOK, rec.Code, "session: %s", rec.Body.String())
	got, err = s.GetInviteCode(ctx, invite.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.UseCount)
}

// TestChatLinkVerify_SessionOnly pins that every chat link verify route is
// session-only (IDENTITY_BINDING): a user access token and a broker
// credential acting for the user are refused and the code stays pending,
// and a session links the account.
func TestChatLinkVerify_SessionOnly(t *testing.T) {
	srv := &Server{
		telegramLinkService: NewTelegramLinkService(),
		discordLinkService:  NewDiscordLinkService(),
		teamsLinkService:    NewTeamsLinkService(),
	}
	t.Cleanup(srv.telegramLinkService.Close)
	t.Cleanup(srv.discordLinkService.Close)
	t.Cleanup(srv.teamsLinkService.Close)

	member := NewAuthenticatedUser("ig-link-user", "link@example.com", "Link", "member", "web")
	token := NewScopedUserIdentity(member, "", []string{"user:read"})
	for _, p := range []struct {
		name    string
		service *chatLinkService
		handler http.HandlerFunc
	}{
		{"telegram", srv.telegramLinkService.chatLinkService, srv.handleTelegramLinkVerify},
		{"discord", srv.discordLinkService.chatLinkService, srv.handleDiscordLinkVerify},
		{"teams", srv.teamsLinkService.chatLinkService, srv.handleTeamsLinkVerify},
	} {
		t.Run(p.name, func(t *testing.T) {
			for _, refused := range []struct {
				name     string
				identity Identity
				kind     CredentialKind
			}{
				{"token", token, CredentialKindUAT},
				{"broker", member, CredentialKindBroker},
			} {
				p.service.RegisterCode("REF-"+refused.name, p.name+"-"+refused.name)
				rec := igRequestWithCredential(p.handler, refused.identity, refused.kind, http.MethodPost, "/", `{"code":"REF-`+refused.name+`"}`)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", refused.name, rec.Body.String())
				reason, credential := sessionOnlyDetailsOf(rec)
				assert.Equal(t, string(authzop.ReasonIdentityBinding), reason, refused.name)
				assert.Equal(t, sessionRequiredCredential, credential, refused.name)
				status, userID, _ := p.service.GetStatusByUser(p.name + "-" + refused.name)
				assert.Equal(t, "pending", status, refused.name)
				assert.Empty(t, userID, refused.name)
			}

			p.service.RegisterCode("SESSION1", p.name+"-session")
			rec := igRequestWithCredential(p.handler, member, CredentialKindInteractive, http.MethodPost, "/", `{"code":"SESSION1"}`)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// TestMyGroupsToken_ProjectBoundaryFiltersProjectGroups pins the caller's
// group list for tokens: a project-boundary token lists groups with no
// project and its boundary project's groups, never another project's
// groups, and lists no project group once its holder stops being a member
// of the boundary project (a live membership check on every request). A hub
// token and a session list every group.
func TestMyGroupsToken_ProjectBoundaryFiltersProjectGroups(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	user := hubConfigTokenUser(t, s, "ig-groups-user", store.SystemRoleHubMember)
	projectA := igProject(t, s, "ig-groups-a")
	projectB := igProject(t, s, "ig-groups-b")
	bindingA := igProjectBinding(t, s, user, projectA, store.ProjectRoleOwner)
	igProjectBinding(t, s, user, projectB, store.ProjectRoleOwner)

	group := func(name, projectID string) string {
		id := tid(name)
		require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: id, Name: name, Slug: tidSlugSafe(name), ProjectID: projectID, CreatedBy: user}))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: id, MemberType: store.GroupMemberTypeUser, MemberID: user, Role: "member", AddedAt: time.Now()}))
		return id
	}
	noProject := group("ig-groups-none", "")
	groupA := group("ig-groups-in-a", projectA)
	groupB := group("ig-groups-in-b", projectB)

	listed := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp GroupsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := make([]string, 0, len(resp.Groups))
		for _, g := range resp.Groups {
			ids = append(ids, g.ID)
		}
		return ids
	}

	tokenA := mintHubConfigToken(t, srv, user, projectBoundary(projectA), "project:read")
	ids := listed(doRequestWithToken(t, srv, tokenA, http.MethodGet, "/api/v1/users/me/groups", nil))
	assert.Contains(t, ids, noProject)
	assert.Contains(t, ids, groupA)
	assert.NotContains(t, ids, groupB)

	hubToken := mintHubConfigToken(t, srv, user, hubBoundary(), "project:read")
	ids = listed(doRequestWithToken(t, srv, hubToken, http.MethodGet, "/api/v1/users/me/groups", nil))
	assert.Subset(t, ids, []string{noProject, groupA, groupB})

	session := NewAuthenticatedUser(user, user+"@test.com", user, "member", "web")
	ids = listed(doRequestAsIdentity(t, srv, session, http.MethodGet, "/api/v1/users/me/groups", nil))
	assert.Subset(t, ids, []string{noProject, groupA, groupB})

	require.NoError(t, s.DeleteRoleBinding(ctx, bindingA))
	rec := doRequestWithToken(t, srv, tokenA, http.MethodGet, "/api/v1/users/me/groups", nil)
	if rec.Code == http.StatusOK {
		ids = listed(rec)
		assert.NotContains(t, ids, groupA, "a holder who left the boundary project lists none of its groups")
		assert.NotContains(t, ids, groupB)
	} else {
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())
	}
}

// TestSelfIdentityReadsToken_AnyTokenReads pins that any token, whatever
// its selectors and boundary, reads its holder's own profile and the
// selector catalog, and that the profile route answers only GET.
func TestSelfIdentityReadsToken_AnyTokenReads(t *testing.T) {
	srv, s := testServer(t)
	user := hubConfigTokenUser(t, s, "ig-self-user", store.SystemRoleHubMember)
	project := igProject(t, s, "ig-self-project")
	igProjectBinding(t, s, user, project, store.ProjectRoleMember)

	for _, key := range []string{
		mintHubConfigToken(t, srv, user, hubBoundary(), "user:read"),
		mintHubConfigToken(t, srv, user, projectBoundary(project), "project:read"),
	} {
		rec := doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/auth/me", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var me UserResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
		assert.Equal(t, user, me.ID)

		rec = doRequestWithToken(t, srv, key, http.MethodGet, "/api/v1/auth/scopes", nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/auth/me", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	}
}

// TestSelfReads_UnknownCredentialKindRefused pins the credential rule of
// admin-status, the caller's groups and explain: an unknown credential kind
// is refused, a user access token kind without a token identity is refused,
// and an interactive session is admitted.
func TestSelfReads_UnknownCredentialKindRefused(t *testing.T) {
	srv, s := testServer(t)
	user := seedRoleUser(t, s, tid("ig-kind-user"), "", true)
	explainBody := `{"resource":{"type":"user","id":"` + user.ID() + `"},"action":"read"}`
	routes := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		body    string
	}{
		{"admin-status", srv.handleAuthAdminStatus, http.MethodGet, "/api/v1/auth/admin-status", ""},
		{"my groups", srv.handleMyGroups, http.MethodGet, "/api/v1/users/me/groups", ""},
		{"explain", srv.handleAuthzExplain, http.MethodPost, "/api/v1/authz/explain", explainBody},
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			rec := igRequestWithCredential(r.handler, user, CredentialKind("unknown-kind"), r.method, r.path, r.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "unknown kind: %s", rec.Body.String())
			rec = igRequestWithCredential(r.handler, user, CredentialKindUAT, r.method, r.path, r.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "token kind without a token identity: %s", rec.Body.String())
			rec = igRequestWithCredential(r.handler, user, CredentialKindInteractive, r.method, r.path, r.body)
			assert.Equal(t, http.StatusOK, rec.Code, "session: %s", rec.Body.String())
		})
	}
}
