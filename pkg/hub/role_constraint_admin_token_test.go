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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Role definition, role binding and access constraint administration by
// user access token. A token is admitted on role:create, role:update,
// role:delete, role_binding:create, role_binding:delete and
// access_constraint:admin, each on a hub boundary only, with the holder's
// live authority. Every grant is judged within the token's selectors: the
// holder must hold each permission a role confers, and a constraint is
// relaxed only over permissions the token carries. No token creates a
// system-scoped role binding or a system-scoped custom role, and no token
// deletes a super-admin binding.

// governanceWriteSelectorPins maps every role, role binding and access
// constraint write selector to the tests that pin its guards: the hub
// boundary, the token ceiling and the holder's live authority, and for role
// bindings the system-scope and super-admin binding refusals.
var governanceWriteSelectorPins = map[string][]string{
	"role:create": {"TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute", "TestCustomRoleCreateToken_ProjectScopeJudgedOnHubAuthority", "TestCustomRoleDelegation_ProjectBoundaryTokenRefused"},
	"role:update": {"TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute"},
	"role:delete": {"TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute"},
	"role_binding:create": {"TestRoleBindingCreateToken_SystemScopeAlwaysDenied", "TestCustomRoleBindingToken_NeedsCreateSelectorAndProjectAccess",
		"TestRoleBindingToken_ChangeAndAuditCommitTogether"},
	"role_binding:delete": {"TestRoleBindingDeleteToken_SuperAdminBindingDenied", "TestRoleBindingDeleteToken_SystemBindingNeedsDeleteSelector",
		"TestRoleBindingToken_ChangeAndAuditCommitTogether"},
	"access_constraint:admin": {"TestConstraintAdminToken_RequiresConstraintAdminSelector", "TestConstraintRelaxToken_SessionOnly",
		"TestConstraintCommit_AppliesTokenSelectors"},
}

// TestGovernanceWriteSelectors_EachPinnedByTest requires every mintable
// selector of a role, role binding or access constraint permission other
// than a read to be mapped in governanceWriteSelectorPins to declared tests,
// and every mapped selector to be mintable.
func TestGovernanceWriteSelectors_EachPinnedByTest(t *testing.T) {
	declared := declaredTestFunctions(t)
	valid := permissions.UATValidScopes()
	seen := map[string]bool{}
	for _, p := range permissions.Registry {
		if p.UATScope == "" || p.Action == permissions.ActionRead {
			continue
		}
		if p.Resource != permissions.ResourceRole && p.Resource != permissions.ResourceRoleBinding && p.Resource != permissions.ResourceAccessConstraint {
			continue
		}
		seen[p.UATScope] = true
		assert.NotEmpty(t, governanceWriteSelectorPins[p.UATScope], "%s (%s) has no test mapped in governanceWriteSelectorPins", p.UATScope, p.ID)
	}
	for sel, pins := range governanceWriteSelectorPins {
		assert.True(t, seen[sel], "%s is mapped but is not a write selector of a role, role binding or access constraint permission", sel)
		assert.True(t, valid[sel], "%s is mapped but is not mintable", sel)
		for _, pin := range pins {
			assert.True(t, declared[pin], "%s: mapped test %s is not declared", sel, pin)
		}
	}
}

// rcaUser creates an active user with hub membership.
func rcaUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	ctx := context.Background()
	id := tid("rca-" + name)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: id, Email: id + "@test.com", DisplayName: name, Role: "member", Status: "active"}))
	ensureHubMembership(ctx, s, id)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

// rcaRole creates a custom role definition with a unique name.
func rcaRole(t *testing.T, s store.Store, name, scopeType string, perms ...string) *store.RoleDefinition {
	t.Helper()
	return createTestRoleDefinition(t, s, "rca-"+name, scopeType, perms)
}

// rcaBindSystem binds principal userID to rd at system scope and returns the
// binding ID.
func rcaBindSystem(t *testing.T, s store.Store, userID string, rd *store.RoleDefinition) string {
	t.Helper()
	b, err := s.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	return b.ID
}

// rcaRoleExists reports whether a role definition named name is stored.
func rcaRoleExists(t *testing.T, s store.Store, name string) bool {
	t.Helper()
	defs, err := s.ListRoleDefinitions(context.Background())
	require.NoError(t, err)
	for _, d := range defs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// rcaBindingCount returns how many role bindings of rd userID holds.
func rcaBindingCount(t *testing.T, s store.Store, userID, roleID string) int {
	t.Helper()
	return len(userRoleBindings(t, s, userID, roleID))
}

func rcaCreateRole(name, scopeType string, perms ...string) createRoleDefinitionRequest {
	return createRoleDefinitionRequest{Name: name, ScopeType: scopeType, Permissions: perms}
}

// TestCustomRoleCreateToken_ProjectScopeJudgedOnHubAuthority pins the
// delegation rule for a project-scoped custom role created by a token. The
// definition names no project and can be bound in every project, so it is
// judged on the holder's system-scope authority within the token's
// selectors, as a session is judged on the holder's system-scope
// authority: a project role does not count. A system-scoped definition is
// refused to every token, and role:create cannot be minted on a project
// boundary.
func TestCustomRoleCreateToken_ProjectScopeJudgedOnHubAuthority(t *testing.T) {
	srv, s := testServer(t)
	admin := rcaUser(t, s, "role-admin")
	rcaBindSystem(t, s, admin.ID, rcaRole(t, s, "role-admin", store.RoleScopeSystem, "role.create", "role.read"))
	project := igProject(t, s, "rca-cr-project")
	igProjectBinding(t, s, admin.ID, project, store.ProjectRoleOwner)
	// agent:read is mintable on a hub boundary through the project role.
	key := mtMint(t, srv, admin.ID, hubBoundary(), "role:create", "role:read", "agent:read")

	// The holder holds agent.read only in one project: refused for the
	// token and for the session.
	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-a", store.RoleScopeProject, "agent.read"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token, project authority only: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-b", store.RoleScopeProject, "agent.read"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "session, project authority only: %s", rec.Body.String())
	assert.False(t, rcaRoleExists(t, s, "rca-cr-a"))
	assert.False(t, rcaRoleExists(t, s, "rca-cr-b"))

	// With system-scope agent.read and agent.list the token creates a
	// project-scoped role inside its selectors.
	rcaBindSystem(t, s, admin.ID, rcaRole(t, s, "agent-reader", store.RoleScopeSystem, "agent.read", "agent.list"))
	rec = doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-c", store.RoleScopeProject, "agent.read"))
	require.Equal(t, http.StatusCreated, rec.Code, "token, system authority: %s", rec.Body.String())
	var created store.RoleDefinition
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, store.RoleScopeProject, created.ScopeType)
	assert.Equal(t, []string{"agent.read"}, created.Permissions)

	// A permission outside the token's selectors is refused to the token;
	// the holder's session creates the same role.
	rec = doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-d", store.RoleScopeProject, "agent.read", "agent.list"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token, permission outside its selectors: %s", rec.Body.String())
	assert.False(t, rcaRoleExists(t, s, "rca-cr-d"))
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-e", store.RoleScopeProject, "agent.read", "agent.list"))
	assert.Equal(t, http.StatusCreated, rec.Code, "session: %s", rec.Body.String())

	// A system-scoped definition is refused to the token; the session
	// creates it.
	rec = doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-f", store.RoleScopeSystem, "agent.read"))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token, system scope: %s", rec.Body.String())
	assert.False(t, rcaRoleExists(t, s, "rca-cr-f"))
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-cr-g", store.RoleScopeSystem, "agent.read"))
	assert.Equal(t, http.StatusCreated, rec.Code, "session, system scope: %s", rec.Body.String())

	igMintRefused(t, srv, admin.ID, projectBoundary(project), "role:create")
}

// TestCustomRoleDelegation_ProjectBoundaryTokenRefused pins the boundary
// half of the custom role rule at CanDelegate: a project-scoped custom role
// names no project, so a project-boundary credential is refused before any
// authority check, while a hub-boundary credential reaches it.
func TestCustomRoleDelegation_ProjectBoundaryTokenRefused(t *testing.T) {
	srv, s := testServer(t)
	authz := srv.authzService
	sa := mtSuperAdmin(t, s, "sa")
	user := NewAuthenticatedUser(sa.ID, sa.Email, sa.DisplayName, "admin", "api")
	project := igProject(t, s, "rca-cd-project")
	grant := GrantDescriptor{Type: GrantTypeCustomRole, CustomRolePermissions: []string{"agent.read"}, ScopeType: store.RoleScopeProject}

	projectCred := NewScopedUserIdentityWithBoundaryAndDecoration(user, projectBoundary(project),
		[]string{"agent:read"}, tid("rca-cd-project-cred"), bearerCeiling(t, "agent:read"), nil)
	d := authz.CanDelegate(context.Background(), projectCred, grant)
	assert.False(t, d.Allowed)
	assert.Contains(t, d.Reason, "names no project")

	hubCred := NewScopedUserIdentityWithBoundaryAndDecoration(user, hubBoundary(),
		[]string{"agent:read"}, tid("rca-cd-hub-cred"), bearerCeiling(t, "agent:read"), nil)
	d = authz.CanDelegate(context.Background(), hubCred, grant)
	assert.True(t, d.Allowed, d.Reason)

	grant.CustomRolePermissions = []string{"agent.read", "agent.list"}
	d = authz.CanDelegate(context.Background(), hubCred, grant)
	assert.False(t, d.Allowed, "agent.list is outside the credential's selectors")
}

// TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute pins every role
// definition write for tokens: each needs its write selector plus role:read
// (the route guard) on a hub boundary and the holder's live authority, and
// each write selector is refused at mint on a project boundary. A token
// without the write selector, or without role:read, changes nothing; the
// holder's session is admitted on the same requests.
func TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute(t *testing.T) {
	srv, s := testServer(t)
	sa := mtSuperAdmin(t, s, "sa")
	project := igProject(t, s, "rca-rw-project")
	igProjectBinding(t, s, sa.ID, project, store.ProjectRoleOwner)
	for _, sel := range []string{"role:create", "role:update", "role:delete"} {
		igMintRefused(t, srv, sa.ID, projectBoundary(project), sel)
	}

	create := mtMint(t, srv, sa.ID, hubBoundary(), "role:create", "role:read", "agent:read")
	update := mtMint(t, srv, sa.ID, hubBoundary(), "role:update", "role:read", "agent:read")
	del := mtMint(t, srv, sa.ID, hubBoundary(), "role:delete", "role:read")
	readOnly := mtMint(t, srv, sa.ID, hubBoundary(), "role:read", "agent:read")
	noGuard := mtMint(t, srv, sa.ID, hubBoundary(), "role:create", "role:update", "role:delete", "agent:read")

	source := rcaRole(t, s, "rw-source", store.RoleScopeProject, "agent.read")
	systemSource := rcaRole(t, s, "rw-system-source", store.RoleScopeSystem, "agent.read")
	target := rcaRole(t, s, "rw-target", store.RoleScopeProject, "agent.read")
	doomed := rcaRole(t, s, "rw-doomed", store.RoleScopeProject, "agent.read")

	importBody := roleImportRequest{Version: "1", Roles: []exportedRole{
		{Name: "rca-rw-imported", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.read"}},
		{Name: "rca-rw-imported-system", ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.read"}},
	}}
	type write struct {
		name, method, path string
		body               interface{}
		admit              string
	}
	writes := []write{
		{"create", http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-rw-created", store.RoleScopeProject, "agent.read"), create},
		{"import", http.MethodPost, "/api/v1/admin/roles/import", importBody, create},
		{"duplicate", http.MethodPost, "/api/v1/admin/roles/" + source.ID + "/duplicate", duplicateRoleDefinitionRequest{Name: "rca-rw-duplicate"}, create},
		{"update", http.MethodPut, "/api/v1/admin/roles/" + target.ID, updateRoleDefinitionRequest{Name: target.Name, Description: "changed", Permissions: []string{"agent.read"}}, update},
		{"delete", http.MethodDelete, "/api/v1/admin/roles/" + doomed.ID, nil, del},
	}

	// Refused without the write selector, and without role:read.
	for _, w := range writes {
		for label, key := range map[string]string{"without the write selector": readOnly, "without role:read": noGuard} {
			rec := doRequestWithUAT(t, srv, key, w.method, w.path, w.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", w.name, label, rec.Body.String())
		}
	}
	assert.False(t, rcaRoleExists(t, s, "rca-rw-created"))
	assert.False(t, rcaRoleExists(t, s, "rca-rw-imported"))
	assert.False(t, rcaRoleExists(t, s, "rca-rw-duplicate"))
	assert.True(t, rcaRoleExists(t, s, doomed.Name))
	got, err := s.GetRoleDefinition(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Description, "the refused update changes nothing")

	// Admitted with the selector on a hub boundary.
	for _, w := range writes {
		rec := doRequestWithUAT(t, srv, w.admit, w.method, w.path, w.body)
		assert.Less(t, rec.Code, 300, "%s: %s", w.name, rec.Body.String())
		if w.name == "import" {
			var resp roleImportResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, 1, resp.Created, "the project-scoped role is imported")
			assert.Equal(t, 1, resp.Errors, "the system-scoped role is refused to a token")
		}
	}
	assert.True(t, rcaRoleExists(t, s, "rca-rw-created"))
	assert.True(t, rcaRoleExists(t, s, "rca-rw-imported"))
	assert.False(t, rcaRoleExists(t, s, "rca-rw-imported-system"))
	assert.True(t, rcaRoleExists(t, s, "rca-rw-duplicate"))
	assert.False(t, rcaRoleExists(t, s, doomed.Name))
	got, err = s.GetRoleDefinition(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, "changed", got.Description)

	// Duplicating a system role yields a system-scoped definition: refused
	// to the token. An update adding a permission outside the selectors is
	// refused too.
	rec := doRequestWithUAT(t, srv, create, http.MethodPost, "/api/v1/admin/roles/"+systemSource.ID+"/duplicate", duplicateRoleDefinitionRequest{Name: "rca-rw-system-duplicate"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doRequestWithUAT(t, srv, update, http.MethodPut, "/api/v1/admin/roles/"+target.ID, updateRoleDefinitionRequest{Name: target.Name, Permissions: []string{"agent.read", "agent.list"}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	got, err = s.GetRoleDefinition(context.Background(), target.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.read"}, got.Permissions)

	// The holder's session is admitted on the same writes.
	rec = doRequestAsUser(t, srv, sa, http.MethodPost, "/api/v1/admin/roles/"+systemSource.ID+"/duplicate", duplicateRoleDefinitionRequest{Name: "rca-rw-session-duplicate"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, sa, http.MethodPut, "/api/v1/admin/roles/"+target.ID, updateRoleDefinitionRequest{Name: target.Name, Permissions: []string{"agent.read", "agent.list"}})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A token whose holder loses role.create is refused.
	holder := rcaUser(t, s, "rw-holder")
	holderBinding := rcaBindSystem(t, s, holder.ID, rcaRole(t, s, "rw-holder", store.RoleScopeSystem, "role.create", "role.read", "agent.read"))
	holderKey := mtMint(t, srv, holder.ID, hubBoundary(), "role:create", "role:read", "agent:read")
	rec = doRequestWithUAT(t, srv, holderKey, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-rw-holder-a", store.RoleScopeProject, "agent.read"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NoError(t, s.DeleteRoleBinding(context.Background(), holderBinding))
	rec = doRequestWithUAT(t, srv, holderKey, http.MethodPost, "/api/v1/admin/roles", rcaCreateRole("rca-rw-holder-b", store.RoleScopeProject, "agent.read"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.False(t, rcaRoleExists(t, s, "rca-rw-holder-b"))
}

// TestRoleBindingCreateToken_SystemScopeAlwaysDenied pins that no token
// creates a system-scoped role binding, whatever role it binds: a
// super-admin's hub token with role_binding:create and the role's selectors
// is refused for a custom system role, hub-admin and super-admin, and
// nothing is written. The holder's session binds the custom system role.
func TestRoleBindingCreateToken_SystemScopeAlwaysDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	sa := mtSuperAdmin(t, s, "sa")
	target := rcaUser(t, s, "rbc-target")
	custom := rcaRole(t, s, "rbc-system", store.RoleScopeSystem, "agent.read")
	hubAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	superAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, every := rcaHubSelectors(t, srv, sa.ID)
	key := mtMint(t, srv, sa.ID, hubBoundary(), every...)

	for _, rd := range []*store.RoleDefinition{custom, hubAdmin, superAdmin} {
		rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID, ScopeType: store.RoleScopeSystem,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", rd.Name, rec.Body.String())
		assert.Zero(t, rcaBindingCount(t, s, target.ID, rd.ID), "%s: nothing is bound", rd.Name)
	}

	rec := doRequestAsUser(t, srv, sa, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: custom.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID, ScopeType: store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, "session: %s", rec.Body.String())
}

// rcaHubSelectors returns every selector userID can mint on
// a hub boundary, without minting a token that carries them.
func rcaHubSelectors(t *testing.T, srv *Server, userID string) (int, []string) {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, opt := range registryUATScopes(false) {
		if seen[opt] {
			continue
		}
		seen[opt] = true
		_, tok, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
			UserID: userID, Name: "rca-" + tid("probe"), Boundary: hubBoundary(), Scopes: []string{opt},
		})
		if err != nil {
			continue
		}
		require.NoError(t, srv.uatService.DeleteToken(rs4MintContext(userID), userID, tok.ID))
		out = append(out, opt)
	}
	require.Contains(t, out, "role_binding:create")
	return len(out), out
}

// TestCustomRoleBindingToken_NeedsCreateSelectorAndProjectAccess pins how a
// token binds a custom project role: it needs role_binding:create on a hub
// boundary, the holder's current project.manage access to the binding's
// project, and every permission of the role within its selectors. A token
// whose holder has no access to the project is refused while the holder's
// session, which needs no project access for this route, binds the role.
func TestCustomRoleBindingToken_NeedsCreateSelectorAndProjectAccess(t *testing.T) {
	srv, s := testServer(t)
	sa := mtSuperAdmin(t, s, "sa")
	project := igProject(t, s, "rca-crb-project")
	owner := rcaUser(t, s, "crb-owner")
	igProjectBinding(t, s, owner.ID, project, store.ProjectRoleOwner)
	custom := rcaRole(t, s, "crb-project", store.RoleScopeProject, "agent.read")
	bind := func(target string) createRoleBindingRequest {
		return createRoleBindingRequest{RoleDefinitionID: custom.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target, ScopeType: store.RoleScopeProject, ScopeID: project}
	}
	igMintRefused(t, srv, owner.ID, projectBoundary(project), "role_binding:create")

	// The super-admin's token: refused without role_binding:create and
	// without agent:read; admitted with both.
	t1 := rcaUser(t, s, "crb-t1")
	for label, key := range map[string]string{
		"without role_binding:create": mtMint(t, srv, sa.ID, hubBoundary(), "agent:read", "project:manage"),
		"without agent:read":          mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:create"),
	} {
		rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/role-bindings", bind(t1.ID))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	}
	assert.Zero(t, rcaBindingCount(t, s, t1.ID, custom.ID))
	full := mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:create", "agent:read")
	rec := doRequestWithUAT(t, srv, full, http.MethodPost, "/api/v1/admin/role-bindings", bind(t1.ID))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, 1, rcaBindingCount(t, s, t1.ID, custom.ID))

	// A holder with hub role_binding.create and agent.read but no access
	// to the project: the token is refused, the session binds.
	holder := rcaUser(t, s, "crb-holder")
	rcaBindSystem(t, s, holder.ID, rcaRole(t, s, "crb-holder", store.RoleScopeSystem, "role_binding.create", "agent.read"))
	holderKey := mtMint(t, srv, holder.ID, hubBoundary(), "role_binding:create", "agent:read")
	t2, t3, t4 := rcaUser(t, s, "crb-t2"), rcaUser(t, s, "crb-t3"), rcaUser(t, s, "crb-t4")
	rec = doRequestWithUAT(t, srv, holderKey, http.MethodPost, "/api/v1/admin/role-bindings", bind(t2.ID))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token, no project access: %s", rec.Body.String())
	assert.Zero(t, rcaBindingCount(t, s, t2.ID, custom.ID))
	rec = doRequestAsUser(t, srv, holder, http.MethodPost, "/api/v1/admin/role-bindings", bind(t3.ID))
	assert.Equal(t, http.StatusCreated, rec.Code, "session: %s", rec.Body.String())

	// Once the holder administers the project, the token binds.
	igProjectBinding(t, s, holder.ID, project, store.ProjectRoleAdmin)
	rec = doRequestWithUAT(t, srv, holderKey, http.MethodPost, "/api/v1/admin/role-bindings", bind(t4.ID))
	assert.Equal(t, http.StatusCreated, rec.Code, "token, project admin: %s", rec.Body.String())
	assert.Equal(t, 1, rcaBindingCount(t, s, t4.ID, custom.ID))
}

// rcaRoleLookupFaultStore fails GetRoleDefinition while its switch is armed.
type rcaRoleLookupFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *rcaRoleLookupFaultStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if f.fault.Active() {
		return nil, errors.New("injected role definition lookup fault")
	}
	return f.Store.GetRoleDefinition(ctx, id)
}

// TestRoleBindingCreate_RoleLookupErrorAnswers500 pins that a project role
// binding request whose role definition lookup fails with an error other
// than not-found answers 500 before any admission decision, for a token and
// for a session, and writes nothing.
func TestRoleBindingCreate_RoleLookupErrorAnswers500(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *rcaRoleLookupFaultStore {
		return &rcaRoleLookupFaultStore{Store: inner, fault: f}
	})
	sa := mtSuperAdmin(t, s, "sa")
	project := igProject(t, s, "rca-rlf-project")
	custom := rcaRole(t, s, "rlf-project", store.RoleScopeProject, "agent.read")
	target := rcaUser(t, s, "rlf-target")
	body := createRoleBindingRequest{RoleDefinitionID: custom.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID, ScopeType: store.RoleScopeProject, ScopeID: project}
	key := mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:create", "agent:read")

	fault.Arm()
	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/role-bindings", body)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "token: %s", rec.Body.String())
	rec = doRequestAsUser(t, srv, sa, http.MethodPost, "/api/v1/admin/role-bindings", body)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "session: %s", rec.Body.String())
	assert.Zero(t, rcaBindingCount(t, s, target.ID, custom.ID), "nothing is written")
}

// TestBuiltInProjectRoleBindingToken_NeedsProjectManage pins that a
// built-in project role binding by token is judged by project governance:
// a super-admin's hub token carrying role_binding:create but not
// project:manage is refused, and nothing is written; the holder's session
// binds.
func TestBuiltInProjectRoleBindingToken_NeedsProjectManage(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	sa := mtSuperAdmin(t, s, "sa")
	project := igProject(t, s, "rca-bip-project")
	member, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	bind := func(target string) createRoleBindingRequest {
		return createRoleBindingRequest{RoleDefinitionID: member.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target, ScopeType: store.RoleScopeProject, ScopeID: project}
	}
	key := mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:create")

	t1, t2 := rcaUser(t, s, "bip-t1"), rcaUser(t, s, "bip-t2")
	rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/role-bindings", bind(t1.ID))
	assert.Equal(t, http.StatusForbidden, rec.Code, "token: %s", rec.Body.String())
	assert.Zero(t, rcaBindingCount(t, s, t1.ID, member.ID), "nothing is written")

	rec = doRequestAsUser(t, srv, sa, http.MethodPost, "/api/v1/admin/role-bindings", bind(t2.ID))
	assert.Equal(t, http.StatusCreated, rec.Code, "session: %s", rec.Body.String())
	assert.Equal(t, 1, rcaBindingCount(t, s, t2.ID, member.ID))
}

// TestRoleBindingDeleteToken_SuperAdminBindingDenied pins that no token
// deletes a super-admin binding: a super-admin's hub token with
// role_binding:delete gets the session-only refusal and the binding stays;
// the holder's session deletes it.
func TestRoleBindingDeleteToken_SuperAdminBindingDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	sa := mtSuperAdmin(t, s, "sa")
	other := mtSuperAdmin(t, s, "other")
	superAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	bindings := userRoleBindings(t, s, other.ID, superAdmin.ID)
	require.Len(t, bindings, 1)
	path := "/api/v1/admin/role-bindings/" + bindings[0].ID

	key := mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:delete")
	rec := doRequestWithUAT(t, srv, key, http.MethodDelete, path, nil)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "super-admin binding delete")
	_, err = s.GetRoleBinding(ctx, bindings[0].ID)
	require.NoError(t, err, "the binding stays")

	rec = doRequestAsUser(t, srv, sa, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "session: %s", rec.Body.String())
}

// TestRoleBindingDeleteToken_SystemBindingNeedsDeleteSelector pins token
// deletes of system-scoped bindings: admitted with role_binding:delete on a
// hub boundary and the holder's live authority, refused without the
// selector or after the holder loses the authority, and the selector cannot
// be minted on a project boundary. A token with role_binding:delete but not
// project:manage cannot remove a project-scoped binding: a token acts on
// project bindings only through project governance.
func TestRoleBindingDeleteToken_SystemBindingNeedsDeleteSelector(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	holder := rcaUser(t, s, "rbd-holder")
	holderBinding := rcaBindSystem(t, s, holder.ID, rcaRole(t, s, "rbd-holder", store.RoleScopeSystem, "role_binding.delete", "role_binding.read"))
	target := rcaUser(t, s, "rbd-target")
	custom := rcaRole(t, s, "rbd-system", store.RoleScopeSystem, "agent.read")
	b1 := rcaBindSystem(t, s, target.ID, custom)
	other := rcaRole(t, s, "rbd-system-2", store.RoleScopeSystem, "agent.list")
	b2 := rcaBindSystem(t, s, target.ID, other)

	sa := mtSuperAdmin(t, s, "sa")
	project := igProject(t, s, "rca-rbd-project")
	igProjectBinding(t, s, sa.ID, project, store.ProjectRoleOwner)
	igMintRefused(t, srv, sa.ID, projectBoundary(project), "role_binding:delete")

	readKey := mtMint(t, srv, holder.ID, hubBoundary(), "role_binding:read")
	rec := doRequestWithUAT(t, srv, readKey, http.MethodDelete, "/api/v1/admin/role-bindings/"+b1, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "without role_binding:delete: %s", rec.Body.String())
	_, err := s.GetRoleBinding(ctx, b1)
	require.NoError(t, err)

	key := mtMint(t, srv, holder.ID, hubBoundary(), "role_binding:delete")
	rec = doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/admin/role-bindings/"+b1, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = s.GetRoleBinding(ctx, b1)
	assert.ErrorIs(t, err, store.ErrNotFound)
	audits := grantAudits(t, s, "role_binding_delete", b1)
	require.Len(t, audits, 1)
	assert.Equal(t, holder.ID, audits[0].ActorPrincipalID)

	require.NoError(t, s.DeleteRoleBinding(ctx, holderBinding))
	rec = doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/admin/role-bindings/"+b2, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "holder without role_binding.delete: %s", rec.Body.String())
	_, err = s.GetRoleBinding(ctx, b2)
	require.NoError(t, err)

	// A project-scoped custom role binding: refused to a super-admin's
	// token without project:manage; the session removes it.
	projectRole := rcaRole(t, s, "rbd-project", store.RoleScopeProject, "agent.read")
	pb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projectRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID,
		ScopeType: store.RoleScopeProject, ScopeID: project, CreatedBy: "test",
	})
	require.NoError(t, err)
	saKey := mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:delete")
	rec = doRequestWithUAT(t, srv, saKey, http.MethodDelete, "/api/v1/admin/role-bindings/"+pb.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "project binding without project:manage: %s", rec.Body.String())
	_, err = s.GetRoleBinding(ctx, pb.ID)
	require.NoError(t, err, "the project binding stays")
	rec = doRequestAsUser(t, srv, sa, http.MethodDelete, "/api/v1/admin/role-bindings/"+pb.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "session: %s", rec.Body.String())
}

// TestRoleBindingToken_ChangeAndAuditCommitTogether pins the transactional
// audit of role bindings written by a token: a custom project
// role binding created and a system binding deleted with a token commit
// together with their audit record, an injected audit failure leaves the
// binding state intact, and the healed request writes exactly one audit
// record naming the holder as actor.
func TestRoleBindingToken_ChangeAndAuditCommitTogether(t *testing.T) {
	for _, mutation := range []string{"role_binding_create", "role_binding_delete"} {
		t.Run(mutation, func(t *testing.T) {
			srv, real, fault, heal := newGrantAuditFaultServer(t, grantAuditFaultStore{auditErrFor: mutation})
			ctx := context.Background()
			sa := mtSuperAdmin(t, real, "sa")
			target := rcaUser(t, real, mutation+"-target")
			key := mtMint(t, srv, sa.ID, hubBoundary(), "role_binding:create", "role_binding:delete", "agent:read")

			if mutation == "role_binding_create" {
				project := igProject(t, real, "rca-audit-"+mutation)
				custom := rcaRole(t, real, "audit-project", store.RoleScopeProject, "agent.read")
				body := createRoleBindingRequest{RoleDefinitionID: custom.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: target.ID, ScopeType: store.RoleScopeProject, ScopeID: project}

				fault.Arm()
				rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/role-bindings", body)
				assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
				assert.Zero(t, rcaBindingCount(t, real, target.ID, custom.ID), "no binding after the audit failure")

				heal.Arm()
				rec = doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/role-bindings", body)
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				var rb store.RoleBinding
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rb))
				audits := grantAudits(t, real, mutation, rb.ID)
				require.Len(t, audits, 1, "exactly one audit record")
				assert.Equal(t, sa.ID, audits[0].ActorPrincipalID)
				assert.Equal(t, "allow", audits[0].CanDelegateResult)
				return
			}

			custom := rcaRole(t, real, "audit-system", store.RoleScopeSystem, "agent.read")
			bindingID := rcaBindSystem(t, real, target.ID, custom)
			fault.Arm()
			rec := doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/admin/role-bindings/"+bindingID, nil)
			assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			_, err := real.GetRoleBinding(ctx, bindingID)
			require.NoError(t, err, "the binding stays after the audit failure")
			assert.Empty(t, grantAudits(t, real, mutation, bindingID))

			heal.Arm()
			rec = doRequestWithUAT(t, srv, key, http.MethodDelete, "/api/v1/admin/role-bindings/"+bindingID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			audits := grantAudits(t, real, mutation, bindingID)
			require.Len(t, audits, 1, "exactly one audit record")
			assert.Equal(t, sa.ID, audits[0].ActorPrincipalID)
		})
	}
}

// rcaConstraintDraft is a system-scoped principal constraint on subject.
func rcaConstraintDraft(name, subject string, perms ...string) *previewDraftRequest {
	return &previewDraftRequest{
		Name: name, Purpose: "token constraint test",
		Subject:            subjectSelectorRequest{Kind: "principal", PrincipalType: "user", PrincipalID: subject},
		Scope:              constraintScopeRequest{Type: "system"},
		MaximumPermissions: perms,
	}
}

// rcaDo sends a request with bearer credential key (a user access token or
// a session token) and, when ifMatch is set, an If-Match header.
func rcaDo(t *testing.T, srv *Server, key, method, path string, body interface{}, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// rcaSessionKey returns a session token for user.
func rcaSessionKey(t *testing.T, srv *Server, user *store.User) string {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
	require.NoError(t, err)
	return token
}

// rcaPreview requests a constraint preview with key and returns the status
// and, on 200, the preview token.
func rcaPreview(t *testing.T, srv *Server, key string, body previewCreateRequest) (int, string) {
	t.Helper()
	rec := rcaDo(t, srv, key, http.MethodPost, "/api/v1/admin/access-constraint-previews", body, "")
	if rec.Code != http.StatusOK {
		return rec.Code, ""
	}
	var preview PreviewResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	require.NotEmpty(t, preview.PreviewToken)
	return rec.Code, preview.PreviewToken
}

// rcaCreateConstraint creates a system-scoped principal constraint on
// subject through the preview and create routes with key, and returns it.
func rcaCreateConstraint(t *testing.T, srv *Server, s store.Store, key, name, subject string, perms ...string) *store.AccessConstraint {
	t.Helper()
	return rcaCreateConstraintWindow(t, srv, s, key, name, subject, nil, perms...)
}

// rcaCreateConstraintWindow is rcaCreateConstraint with the time window
// window (nil for none).
func rcaCreateConstraintWindow(t *testing.T, srv *Server, s store.Store, key, name, subject string, window *constraintConditionReq, perms ...string) *store.AccessConstraint {
	t.Helper()
	d := rcaConstraintDraft(name, subject, perms...)
	d.AppliesWhen = window
	code, token := rcaPreview(t, srv, key, previewCreateRequest{Operation: "create", Draft: d})
	require.Equal(t, http.StatusOK, code)
	rec := rcaDo(t, srv, key, http.MethodPost, "/api/v1/admin/access-constraints", accessConstraintCreateRequest{
		Name: d.Name, Purpose: d.Purpose, Subject: d.Subject, Scope: d.Scope, MaximumPermissions: d.MaximumPermissions, AppliesWhen: d.AppliesWhen, PreviewToken: token,
	}, "")
	require.Equal(t, http.StatusCreated, rec.Code, "create: %s", rec.Body.String())
	var created mutationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	stored, err := s.GetAccessConstraint(context.Background(), created.ID)
	require.NoError(t, err)
	return stored
}

// rcaUpdateConstraint previews and sends an update of c to perms with key,
// and returns the update response (or the preview refusal).
func rcaUpdateConstraint(t *testing.T, srv *Server, key string, c *store.AccessConstraint, subject string, perms ...string) *httptest.ResponseRecorder {
	t.Helper()
	return rcaUpdateConstraintWindow(t, srv, key, c, subject, nil, perms...)
}

// rcaUpdateConstraintWindow is rcaUpdateConstraint with the proposed time
// window window (nil for none).
func rcaUpdateConstraintWindow(t *testing.T, srv *Server, key string, c *store.AccessConstraint, subject string, window *constraintConditionReq, perms ...string) *httptest.ResponseRecorder {
	t.Helper()
	d := rcaConstraintDraft(c.Name, subject, perms...)
	d.AppliesWhen = window
	rev := strconv.FormatInt(c.Revision, 10)
	preview := rcaDo(t, srv, key, http.MethodPost, "/api/v1/admin/access-constraint-previews",
		previewCreateRequest{Operation: "update", ConstraintID: c.ID, BaseRevision: rev, Draft: d}, "")
	if preview.Code != http.StatusOK {
		return preview
	}
	var result PreviewResult
	require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &result))
	return rcaDo(t, srv, key, http.MethodPut, "/api/v1/admin/access-constraints/"+c.ID, accessConstraintUpdateRequest{
		Name: d.Name, Purpose: d.Purpose, Subject: d.Subject, Scope: d.Scope, MaximumPermissions: d.MaximumPermissions, AppliesWhen: d.AppliesWhen, PreviewToken: result.PreviewToken,
	}, `"`+rev+`"`)
}

// rcaDeleteConstraint previews and sends a delete of c with key.
func rcaDeleteConstraint(t *testing.T, srv *Server, key string, c *store.AccessConstraint) *httptest.ResponseRecorder {
	t.Helper()
	preview := rcaDo(t, srv, key, http.MethodPost, "/api/v1/admin/access-constraint-previews",
		previewCreateRequest{Operation: "delete", ConstraintID: c.ID, BaseRevision: strconv.FormatInt(c.Revision, 10)}, "")
	var result PreviewResult
	if preview.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &result))
	}
	return rcaDo(t, srv, key, http.MethodDelete, "/api/v1/admin/access-constraints/"+c.ID, accessConstraintDeleteRequest{PreviewToken: result.PreviewToken}, "")
}

// TestConstraintAdminToken_RequiresConstraintAdminSelector pins constraint
// administration by token: previews need access_constraint:admin, and
// constraint create and a tightening update also need access_constraint:read
// (the route guard), on a hub boundary with the holder's live authority;
// the selector cannot be minted on a project boundary. Without the selector
// nothing changes.
func TestConstraintAdminToken_RequiresConstraintAdminSelector(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	sa := mtSuperAdmin(t, s, "sa")
	subject := rcaUser(t, s, "ca-subject")
	project := igProject(t, s, "rca-ca-project")
	igProjectBinding(t, s, sa.ID, project, store.ProjectRoleOwner)
	igMintRefused(t, srv, sa.ID, projectBoundary(project), "access_constraint:admin")

	admin := mtMint(t, srv, sa.ID, hubBoundary(), "access_constraint:admin", "access_constraint:read")
	adminOnly := mtMint(t, srv, sa.ID, hubBoundary(), "access_constraint:admin")
	readOnly := mtMint(t, srv, sa.ID, hubBoundary(), "access_constraint:read")

	// Previews.
	createPreview := previewCreateRequest{Operation: "create", Draft: rcaConstraintDraft("rca-ca-boundary", subject.ID, "agent.read", "agent.list")}
	code, _ := rcaPreview(t, srv, readOnly, createPreview)
	assert.Equal(t, http.StatusForbidden, code, "preview without access_constraint:admin")
	code, adminOnlyToken := rcaPreview(t, srv, adminOnly, createPreview)
	require.Equal(t, http.StatusOK, code, "preview with access_constraint:admin")
	jobID := tid("rca-ca-job")
	srv.previewService.asyncJobs.Store(jobID, &PreviewJob{JobID: jobID, Status: JobStatusSucceeded, Operation: "create", Result: &PreviewResult{}})
	for _, path := range []string{"/api/v1/admin/access-constraint-previews/" + jobID, "/api/v1/admin/access-constraint-previews/" + jobID + "/result"} {
		rec := doRequestWithUAT(t, srv, readOnly, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "GET %s without access_constraint:admin: %s", path, rec.Body.String())
		rec = doRequestWithUAT(t, srv, adminOnly, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", path, rec.Body.String())
	}

	// Create: refused without either selector, admitted with both.
	d := createPreview.Draft
	createBody := accessConstraintCreateRequest{Name: d.Name, Purpose: d.Purpose, Subject: d.Subject, Scope: d.Scope, MaximumPermissions: d.MaximumPermissions, PreviewToken: adminOnlyToken}
	for label, key := range map[string]string{"without access_constraint:read": adminOnly, "without access_constraint:admin": readOnly} {
		rec := doRequestWithUAT(t, srv, key, http.MethodPost, "/api/v1/admin/access-constraints", createBody)
		assert.Equal(t, http.StatusForbidden, rec.Code, "create %s: %s", label, rec.Body.String())
	}
	list, err := s.ListAccessConstraints(ctx, 100, 0)
	require.NoError(t, err)
	for _, c := range list {
		assert.NotEqual(t, "rca-ca-boundary", c.Name, "nothing is created without the selectors")
	}
	c := rcaCreateConstraint(t, srv, s, admin, "rca-ca-boundary", subject.ID, "agent.read", "agent.list")

	// A tightening update (a permission removed from the maximum set):
	// refused without access_constraint:admin, admitted with it.
	rec := rcaUpdateConstraint(t, srv, readOnly, c, subject.ID, "agent.read")
	assert.Equal(t, http.StatusForbidden, rec.Code, "tightening update without access_constraint:admin: %s", rec.Body.String())
	rec = rcaUpdateConstraint(t, srv, admin, c, subject.ID, "agent.read")
	assert.Equal(t, http.StatusOK, rec.Code, "tightening update: %s", rec.Body.String())
	got, err := s.GetAccessConstraint(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.read"}, got.MaximumPermissions)

	// A token whose holder loses access_constraint.admin is refused.
	holder := rcaUser(t, s, "ca-holder")
	holderBinding := rcaBindSystem(t, s, holder.ID, rcaRole(t, s, "ca-holder", store.RoleScopeSystem, "access_constraint.admin", "access_constraint.read"))
	holderKey := mtMint(t, srv, holder.ID, hubBoundary(), "access_constraint:admin")
	code, _ = rcaPreview(t, srv, holderKey, createPreview)
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, s.DeleteRoleBinding(ctx, holderBinding))
	code, _ = rcaPreview(t, srv, holderKey, createPreview)
	assert.Equal(t, http.StatusForbidden, code, "holder without access_constraint.admin")
}

// TestConstraintRelaxToken_SessionOnly pins that relaxing a constraint
// needs a session: a super-admin's hub token carrying every mintable
// selector gets the session-only refusal on a constraint delete, on an
// update that adds a permission to the maximum set, and on an update that
// narrows or adds the constraint's time window, and the constraint is kept
// as stored; the holder's session makes each of these changes. A token
// update that widens or removes the time window is admitted.
func TestConstraintRelaxToken_SessionOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	sa := mtSuperAdmin(t, s, "sa")
	subject := rcaUser(t, s, "cr-subject")
	_, every := rcaHubSelectors(t, srv, sa.ID)
	key := mtMint(t, srv, sa.ID, hubBoundary(), every...)
	session := rcaSessionKey(t, srv, sa)

	widen := rcaCreateConstraint(t, srv, s, key, "rca-cr-widen", subject.ID, "agent.read")
	rec := rcaUpdateConstraint(t, srv, key, widen, subject.ID, "agent.read", "agent.list")
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "relaxing update")
	got, err := s.GetAccessConstraint(ctx, widen.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent.read"}, got.MaximumPermissions, "the refused update changes nothing")

	doomed := rcaCreateConstraint(t, srv, s, key, "rca-cr-doomed", subject.ID, "agent.read")
	rec = rcaDeleteConstraint(t, srv, key, doomed)
	requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, "delete")
	_, err = s.GetAccessConstraint(ctx, doomed.ID)
	require.NoError(t, err, "the constraint stays")

	rec = rcaUpdateConstraint(t, srv, session, got, subject.ID, "agent.read", "agent.list")
	assert.Equal(t, http.StatusOK, rec.Code, "session relaxing update: %s", rec.Body.String())
	rec = rcaDeleteConstraint(t, srv, session, doomed)
	assert.Equal(t, http.StatusOK, rec.Code, "session delete: %s", rec.Body.String())
	_, err = s.GetAccessConstraint(ctx, doomed.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Time window: the constraint is inactive outside its window, so an
	// update whose window does not cover the stored one is session only.
	base := srv.governanceService.nowFunc().UTC().Truncate(time.Second)
	at := func(d time.Duration) *time.Time {
		v := base.Add(d)
		return &v
	}
	for _, tc := range []struct {
		name     string
		stored   *constraintConditionReq
		proposed *constraintConditionReq
	}{
		{"narrowed expiresAt", &constraintConditionReq{ExpiresAt: at(72 * time.Hour)}, &constraintConditionReq{ExpiresAt: at(time.Hour)}},
		{"added future notBefore", &constraintConditionReq{ExpiresAt: at(72 * time.Hour)}, &constraintConditionReq{NotBefore: at(24 * time.Hour), ExpiresAt: at(72 * time.Hour)}},
		{"window added to an unwindowed constraint", nil, &constraintConditionReq{ExpiresAt: at(72 * time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rcaCreateConstraintWindow(t, srv, s, key, "rca-cr-window-"+strings.ReplaceAll(tc.name, " ", "-"), subject.ID, tc.stored, "agent.read")
			rec := rcaUpdateConstraintWindow(t, srv, key, c, subject.ID, tc.proposed, "agent.read")
			requireSessionOnlyRefusal(t, rec, authzop.ReasonGovernancePending, tc.name)
			got, err := s.GetAccessConstraint(ctx, c.ID)
			require.NoError(t, err)
			assert.Equal(t, c.Revision, got.Revision, "the refused update changes nothing")
			rcaRequireWindow(t, got, c.NotBefore, c.ExpiresAt)

			rec = rcaUpdateConstraintWindow(t, srv, session, got, subject.ID, tc.proposed, "agent.read")
			require.Equal(t, http.StatusOK, rec.Code, "session %s: %s", tc.name, rec.Body.String())
			got, err = s.GetAccessConstraint(ctx, c.ID)
			require.NoError(t, err)
			rcaRequireWindow(t, got, tc.proposed.NotBefore, tc.proposed.ExpiresAt)
		})
	}

	// Widening or removing the time window is a tightening: admitted.
	for _, tc := range []struct {
		name     string
		proposed *constraintConditionReq
	}{
		{"widened window", &constraintConditionReq{ExpiresAt: at(96 * time.Hour)}},
		{"removed window", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rcaCreateConstraintWindow(t, srv, s, key, "rca-cr-window-"+strings.ReplaceAll(tc.name, " ", "-"), subject.ID,
				&constraintConditionReq{NotBefore: at(time.Hour), ExpiresAt: at(48 * time.Hour)}, "agent.read")
			rec := rcaUpdateConstraintWindow(t, srv, key, c, subject.ID, tc.proposed, "agent.read")
			require.Equal(t, http.StatusOK, rec.Code, "token %s: %s", tc.name, rec.Body.String())
			got, err := s.GetAccessConstraint(ctx, c.ID)
			require.NoError(t, err)
			var nb, ea *time.Time
			if tc.proposed != nil {
				nb, ea = tc.proposed.NotBefore, tc.proposed.ExpiresAt
			}
			rcaRequireWindow(t, got, nb, ea)
		})
	}
}

// rcaRequireWindow asserts that c's stored time window is
// [notBefore, expiresAt] (nil for an unbounded side).
func rcaRequireWindow(t *testing.T, c *store.AccessConstraint, notBefore, expiresAt *time.Time) {
	t.Helper()
	for _, side := range []struct {
		name      string
		got, want *time.Time
	}{{"notBefore", c.NotBefore, notBefore}, {"expiresAt", c.ExpiresAt, expiresAt}} {
		if side.want == nil {
			assert.Nil(t, side.got, side.name)
			continue
		}
		if assert.NotNil(t, side.got, side.name) {
			assert.True(t, side.want.Equal(*side.got), "%s: got %v, want %v", side.name, *side.got, *side.want)
		}
	}
}

// TestConstraintCommit_AppliesTokenSelectors pins that the constraint commit
// re-checks judge the actor within the request credential's limits: a
// commit whose token credential lacks access_constraint.admin is refused at
// the re-authorization, and the relaxation authority check counts only the
// permissions the token carries. A session credential is judged on the
// holder's full authority.
func TestConstraintCommit_AppliesTokenSelectors(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	sa := mtSuperAdmin(t, s, "sa")
	subject := rcaUser(t, s, "cc-subject")
	user := NewAuthenticatedUser(sa.ID, sa.Email, sa.DisplayName, "admin", "api")
	actor := PrincipalContext{Kind: PrincipalKindUser, ID: sa.ID}
	token := func(selectors ...string) Identity {
		return NewScopedUserIdentityWithBoundaryAndDecoration(user, hubBoundary(), selectors, tid("rca-cc-"+strings.Join(selectors, ",")), bearerCeiling(t, selectors...), nil)
	}

	draft, err := srv.draftToStoreConstraint(rcaConstraintDraft("rca-cc-boundary", subject.ID, "agent.read"), user)
	require.NoError(t, err)
	commit := func(credential Identity) error {
		preview, err := srv.previewService.GeneratePreview(ctx, PreviewRequest{Operation: "create", Draft: draft, Actor: actor})
		require.NoError(t, err)
		_, err = srv.governanceService.CommitBoundaryChange(ctx, CommitRequest{
			Operation: "create", Draft: draft, PreviewToken: preview.PreviewToken, Actor: actor, Credential: credential,
		})
		return err
	}
	err = commit(token("access_constraint:read"))
	var ge *GovernanceError
	require.ErrorAs(t, err, &ge, "a token credential without access_constraint.admin")
	assert.Equal(t, ErrCodeMutationPermissionLost, ge.Code)
	require.NoError(t, commit(token("access_constraint:admin")), "a token credential with access_constraint.admin")

	existing := rcaCreateConstraint(t, srv, s, rcaSessionKey(t, srv, sa), "rca-cc-relax", subject.ID, "agent.read")
	relax := func(credential Identity) error {
		return srv.governanceService.checkRelaxationAuthority(ctx, CommitRequest{
			Operation: "delete", ConstraintID: existing.ID, Actor: actor, Credential: credential,
		}, store.RoleScopeSystem, "")
	}
	err = relax(token("access_constraint:admin"))
	require.ErrorAs(t, err, &ge, "a token credential without agent:read")
	assert.Equal(t, ErrCodeInsufficientRelaxationAuthority, ge.Code)
	assert.NoError(t, relax(token("access_constraint:admin", "agent:read")))
	assert.NoError(t, relax(user), "a session credential")
}
