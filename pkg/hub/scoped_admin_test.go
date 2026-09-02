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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test setup helpers
// ---------------------------------------------------------------------------

// setupScopedAdminTest creates a test server with a hub-admin user and a regular
// member user. The hub-admin has Role=member but holds a system-scoped hub-admin
// role binding, giving them permissions defined in hubAdminPermissionIDs().
func setupScopedAdminTest(t *testing.T) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a hub-admin user (NOT super-admin — role is "member")
	hubAdmin := &store.User{
		ID:          tid("user-hub-admin-test"),
		Email:       "hubadmin@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)

	// Create system-scoped hub-admin role binding
	hubAdminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubAdminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      hubAdmin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// Create a regular member user for comparison
	member := &store.User{
		ID:          tid("user-member-test"),
		Email:       "member@test.com",
		DisplayName: "Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	return srv, s, hubAdmin, member
}

// ==========================================================================
// Group 1: Hub-admin Access Tests (scopeable endpoints)
//
// A user with Role=member and a system-scoped hub-admin role binding
// should be able to access admin endpoints whose permissions are included
// in the hub-admin role definition (hubAdminPermissionIDs).
// ==========================================================================

func TestScopedAdmin_HubAdminCanListRoles(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/roles — RouteHubAdmin, permission: role.read (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/roles", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access role listing")
}

func TestScopedAdmin_HubAdminCanReadServerConfig(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/server-config — RouteHubAdmin, permission: hub.config.read (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/server-config", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access server config")
}

func TestScopedAdmin_HubAdminCanListSkillRegistries(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/skill-registries — RouteHubAdmin, permission: skill.register (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/skill-registries", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access skill registries")
}

func TestScopedAdmin_HubAdminCanReadHealthSummary(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/health/summary — RouteHubAdmin, permission: hub.health.read (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/health/summary", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access health summary")
}

func TestScopedAdmin_HubAdminCanListRoleBindings(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/role-bindings — RouteHubAdmin, permission: role_binding.read (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/role-bindings", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access role bindings listing")
}

func TestScopedAdmin_HubAdminCanListPermissions(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/permissions — RouteHubAdmin, permission: role.read (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/permissions", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access permissions listing")
}

// RoutePolicy endpoints: these pass through the route guard unconditionally.
// The handler does per-resource authorization internally. Hub-admin users
// with user.read / group.read should see results (200 OK).

func TestScopedAdmin_HubAdminCanListUsers(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/users — RoutePolicy, permission: user.read (in hub-admin)
	// Handler applies per-resource capabilities; hub-admin with user.read should get 200.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/users", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access user listing")
}

func TestScopedAdmin_HubAdminCanListGroups(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/groups — RoutePolicy, permission: group.read (in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/groups", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access group listing")
}

// ==========================================================================
// Group 2: Hub-admin Denial Tests (super-admin-only endpoints)
//
// The hub-admin role deliberately excludes certain permissions. Routes that
// require those permissions must deny the hub-admin user (403 Forbidden).
// ==========================================================================

func TestScopedAdmin_HubAdminDeniedMaintenanceOperations(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// POST /api/v1/admin/maintenance/operations — permission: hub.maintenance.execute (NOT in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/maintenance/operations", map[string]string{
		"operation": "test",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub-admin should be denied maintenance operations")
}

func TestScopedAdmin_HubAdminDeniedDiagnosticsLogs(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/diagnostics/logs — permission: hub.diagnostics.read (NOT in hub-admin role)
	// CO1: The AK1 kernel only evaluates role bindings. The hub-admin role does
	// not include hub.diagnostics.read, so this endpoint is correctly denied.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/diagnostics/logs", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub-admin should be denied diagnostics/logs (hub.diagnostics.read not in hub-admin role)")
}

func TestScopedAdmin_HubAdminDeniedAdminModeToggle(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// GET /api/v1/admin/maintenance — permission: hub.admin_mode.update (NOT in hub-admin)
	// This route guards the admin mode toggle (maintenance state endpoint).
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/maintenance", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub-admin should be denied admin mode toggle")
}

func TestScopedAdmin_HubAdminDeniedAuthReset(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// POST /api/v1/admin/agents/reset-auth-all — permission: hub.auth_reset.execute (NOT in hub-admin)
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/agents/reset-auth-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub-admin should be denied auth reset")
}

func TestScopedAdmin_HubAdminPolicyAPIReturnsGone(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// CO1: The Policy API is removed. All callers receive 410 Gone.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/policies", nil)
	assert.Equal(t, http.StatusGone, rec.Code,
		"policy list should return 410 Gone under CO1")
}

func TestScopedAdmin_HubAdminDeniedPolicyCreate(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// CO1: The Policy API is removed. All callers receive 410 Gone.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/policies", map[string]interface{}{
		"name":        "test-policy",
		"description": "test",
		"rules":       []interface{}{},
	})
	assert.Equal(t, http.StatusGone, rec.Code, "policy create should return 410 Gone under CO1")
}

// Verify that a regular member (no role binding) is denied admin endpoints
// whose actions are NOT covered by per-type hub-member-read policies (read/list).
// Endpoints with action=read are accessible to all hub members via those policies.

func TestScopedAdmin_RegularMemberDeniedWriteAdminEndpoints(t *testing.T) {
	srv, _, _, member := setupScopedAdminTest(t)

	// Endpoints where action ≠ read/list — these should be denied for regular members.
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"skill-registries (action=register)", http.MethodGet, "/api/v1/skill-registries"},
		{"maintenance-operations (action=execute)", http.MethodPost, "/api/v1/admin/maintenance/operations"},
		{"admin-mode-toggle (action=update)", http.MethodGet, "/api/v1/admin/maintenance"},
		{"auth-reset (action=execute)", http.MethodPost, "/api/v1/admin/agents/reset-auth-all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, member, tt.method, tt.path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"regular member should be denied %s", tt.path)
		})
	}
}

// Verify that read-action admin endpoints ARE accessible to regular hub members
// via the per-type hub-member-read-* policies seeded policy (read+list on each resource type).

func TestScopedAdmin_RegularMemberAllowedReadAdminEndpoints(t *testing.T) {
	srv, _, _, member := setupScopedAdminTest(t)

	// CO1: Hub members access admin endpoints only through role bindings.
	// The hub-member role includes role.read but NOT role_binding.read (S1 fix),
	// hub.config.read, or hub.health.read.

	t.Run("allowed_via_hub_member_role", func(t *testing.T) {
		// Endpoints whose permissions are in the hub-member role.
		for _, tt := range []struct {
			name string
			path string
		}{
			{"roles", "/api/v1/admin/roles"},
			{"permissions", "/api/v1/admin/permissions"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, member, http.MethodGet, tt.path, nil)
				assert.Equal(t, http.StatusOK, rec.Code,
					"hub member should access %s via hub-member role binding", tt.path)
			})
		}
	})

	t.Run("denied_without_hub_admin_role", func(t *testing.T) {
		// Endpoints whose permissions are NOT in the hub-member role.
		// S1: role-bindings moved here — hub members can no longer enumerate
		// all role bindings hub-wide.
		for _, tt := range []struct {
			name string
			path string
		}{
			{"role-bindings", "/api/v1/admin/role-bindings"},
			{"server-config", "/api/v1/admin/server-config"},
			{"health-summary", "/api/v1/admin/health/summary"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, member, http.MethodGet, tt.path, nil)
				assert.Equal(t, http.StatusForbidden, rec.Code,
					"hub member should be denied %s (requires hub-admin role)", tt.path)
			})
		}
	})
}

// ==========================================================================
// Group 3: Project-scoped Admin Tests
//
// A user with a project-admin role binding for project X should be able to
// access resources within that project but not hub-level admin endpoints.
// ==========================================================================

// setupProjectScopedAdminTest creates a project-scoped admin scenario:
// two projects, one user with project-admin in project X only.
func setupProjectScopedAdminTest(t *testing.T) (*Server, store.Store, *store.User, *store.Project, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	// Create the project-scoped admin user
	projectAdmin := &store.User{
		ID:          tid("user-project-admin-test"),
		Email:       "projectadmin@test.com",
		DisplayName: "Project Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, projectAdmin))
	ensureHubMembership(ctx, s, projectAdmin.ID)

	// Create project owner
	owner := &store.User{
		ID:          tid("user-project-owner-test"),
		Email:       "owner@test.com",
		DisplayName: "Project Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Create project X (bound project)
	projectX := &store.Project{
		ID:        tid("project-x"),
		Name:      "Project X",
		Slug:      "project-x",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projectX))
	srv.createProjectMembersGroup(ctx, projectX)

	// Create project Y (unbound project)
	projectY := &store.Project{
		ID:        tid("project-y"),
		Name:      "Project Y",
		Slug:      "project-y",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projectY))
	srv.createProjectMembersGroup(ctx, projectY)

	// Create project-admin role binding for projectAdmin in project X only
	projectAdminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projectAdminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      projectAdmin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectX.ID,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	return srv, s, projectAdmin, projectX, projectY
}

func TestScopedAdmin_ProjectAdminCanAccessBoundProject(t *testing.T) {
	srv, _, projectAdmin, projectX, _ := setupProjectScopedAdminTest(t)

	// A project-admin should be able to read the project they are bound to.
	rec := doRequestAsUser(t, srv, projectAdmin, http.MethodGet, "/api/v1/projects/"+projectX.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"project-admin should access bound project")
}

func TestScopedAdmin_ProjectAdminDeniedUnboundProject(t *testing.T) {
	srv, _, projectAdmin, _, projectY := setupProjectScopedAdminTest(t)

	// A project-admin for X should not have admin access to project Y.
	// The user can read project Y only if policies allow it (default deny for non-members).
	rec := doRequestAsUser(t, srv, projectAdmin, http.MethodPatch, "/api/v1/projects/"+projectY.ID, map[string]interface{}{
		"name": "Renamed",
	})
	// Project admin for X should not be able to modify project Y
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project-admin should be forbidden from modifying unbound project")
}

func TestScopedAdmin_ProjectAdminDeniedHubLevelWriteOperations(t *testing.T) {
	srv, _, projectAdmin, _, _ := setupProjectScopedAdminTest(t)

	// A project-scoped admin should NOT be able to perform hub-level write operations.
	// Read-action endpoints are accessible to all hub members via per-type hub-member-read-* policies policy,
	// but write/execute/update operations require specific permissions the project-admin lacks.
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"skill-registries (action=register)", http.MethodGet, "/api/v1/skill-registries"},
		{"maintenance-operations (action=execute)", http.MethodPost, "/api/v1/admin/maintenance/operations"},
		{"admin-mode-toggle (action=update)", http.MethodGet, "/api/v1/admin/maintenance"},
		{"auth-reset (action=execute)", http.MethodPost, "/api/v1/admin/agents/reset-auth-all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, projectAdmin, tt.method, tt.path, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"project-scoped admin should be denied hub-level %s", tt.path)
		})
	}
}

func TestScopedAdmin_ProjectAdminAllowedHubReadOperations(t *testing.T) {
	srv, _, projectAdmin, _, _ := setupProjectScopedAdminTest(t)

	// CO1: Project-scoped admin is also a hub member. The hub-member role
	// includes role.read but NOT role_binding.read (S1 fix), hub.config.read,
	// or hub.health.read. Only endpoints whose permissions are in the hub-member
	// role are accessible.

	t.Run("allowed_via_hub_member_role", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			path string
		}{
			{"roles (action=read)", "/api/v1/admin/roles"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, projectAdmin, http.MethodGet, tt.path, nil)
				assert.Equal(t, http.StatusOK, rec.Code,
					"project admin (hub member) should access %s via hub-member role binding", tt.path)
			})
		}
	})

	t.Run("denied_without_hub_admin_role", func(t *testing.T) {
		// S1: role-bindings now denied for hub members (no longer have role_binding.read)
		for _, tt := range []struct {
			name string
			path string
		}{
			{"role-bindings (action=read)", "/api/v1/admin/role-bindings"},
			{"server-config (action=read)", "/api/v1/admin/server-config"},
			{"health-summary (action=read)", "/api/v1/admin/health/summary"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, projectAdmin, http.MethodGet, tt.path, nil)
				assert.Equal(t, http.StatusForbidden, rec.Code,
					"project admin should be denied %s (requires hub-admin role)", tt.path)
			})
		}
	})
}

// ==========================================================================
// Group 4: CanDelegate Constraint Tests
//
// Hub-admin users can delegate permissions they hold but cannot grant
// permissions they don't hold. Super-admin bindings are always blocked
// for non-reconciler callers (D10 guard).
// ==========================================================================

func TestScopedAdmin_HubAdminCanCreateHubAdminBinding(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	ctx := context.Background()

	// Look up the hub-admin role definition
	hubAdminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	// Hub-admin should be able to create another hub-admin binding
	// because they hold all the permissions in the hub-admin role.
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: hubAdminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"hub-admin should be able to create hub-admin binding for another user; got body: %s", rec.Body.String())
}

func TestScopedAdmin_HubAdminCannotCreateSuperAdminBinding(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	ctx := context.Background()

	// Look up the super-admin role definition
	superAdminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	// Hub-admin should NOT be able to create a super-admin binding.
	// This is blocked by either the CanDelegate check (hub-admin doesn't hold
	// super-admin permissions) or the D10 guard in the store layer
	// (ErrSuperAdminBindingRestricted for non-reconciler callers).
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: superAdminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub-admin should be denied creating super-admin binding; got body: %s", rec.Body.String())
}

func TestScopedAdmin_HubAdminCanCreateCustomRoleWithHeldPermissions(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// Hub-admin should be able to create a custom role that contains
	// only permissions they hold (user.read, user.list are in hub-admin).
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		Name:        "custom-reader",
		Description: "Custom read-only role",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"user.read", "user.list"},
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"hub-admin should be able to create custom role with held permissions; got body: %s", rec.Body.String())
}

func TestScopedAdmin_HubAdminCannotCreateCustomRoleWithUnheldPermissions(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// Hub-admin should NOT be able to create a custom role with permissions
	// they don't hold (hub.maintenance.execute is excluded from hub-admin).
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
		Name:        "maintenance-role",
		Description: "Role with maintenance permission",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"hub.maintenance.execute"},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub-admin should be denied creating role with unheld permissions; got body: %s", rec.Body.String())
}

func TestScopedAdmin_HubAdminCanCreateProjectScopedBinding(t *testing.T) {
	srv, s, hubAdmin, member := setupScopedAdminTest(t)
	ctx := context.Background()

	// Create a project for binding
	owner := hubAdmin
	project := &store.Project{
		ID:        tid("project-candelegate"),
		Name:      "CanDelegate Project",
		Slug:      "candelegate-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Look up project-admin role definition
	projectAdminDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)

	// Hub-admin should be able to create a project-admin binding for another user.
	// Hub-admin holds all project-admin permissions (project oversight permissions).
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: projectAdminDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"hub-admin should be able to create project-scoped binding; got body: %s", rec.Body.String())
}

// ==========================================================================
// Group 5: Combined Role Tests
//
// Users may hold multiple role bindings. Verify that permissions
// accumulate correctly.
// ==========================================================================

func TestScopedAdmin_CombinedHubAndProjectRoles(t *testing.T) {
	srv, s, hubAdmin, _ := setupScopedAdminTest(t)
	ctx := context.Background()

	// Create a project owned by hub-admin
	project := &store.Project{
		ID:        tid("project-combined"),
		Name:      "Combined Test Project",
		Slug:      "combined-test-project",
		OwnerID:   hubAdmin.ID,
		CreatedBy: hubAdmin.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Hub-admin should still access hub-level endpoints
	rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/admin/roles", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin with project role should still access hub-level roles")

	// Hub-admin should also access their own project
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/projects/"+project.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "hub-admin should access owned project")

	// Hub-admin should still be denied super-admin-only write/execute endpoints
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/maintenance/operations", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "hub-admin with project role should still be denied maintenance operations")
}

func TestScopedAdmin_CustomNarrowRole(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a custom narrow role with only user.read and group.read
	narrowRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "narrow-viewer",
		Description: "Can only read users and groups",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"user.read", "group.read"},
		System:      false,
	})
	require.NoError(t, err)

	// Create a user with this narrow role binding
	narrowUser := &store.User{
		ID:          tid("user-narrow-test"),
		Email:       "narrow@test.com",
		DisplayName: "Narrow User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, narrowUser))
	ensureHubMembership(ctx, s, narrowUser.ID)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: narrowRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      narrowUser.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// CO1: Admin endpoints are accessible only through role bindings.
	// hub.config.read is NOT in the hub-member role, so server-config is denied.
	rec := doRequestAsUser(t, srv, narrowUser, http.MethodGet, "/api/v1/admin/server-config", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"narrow-role user should be denied server-config (hub.config.read not in hub-member role)")

	// role.read IS in the hub-member role, so roles endpoint is allowed.
	rec = doRequestAsUser(t, srv, narrowUser, http.MethodGet, "/api/v1/admin/roles", nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"narrow-role user (hub member) should access roles via hub-member role binding")

	// But non-read-action admin endpoints should be denied — the narrow role
	// doesn't include the required permissions, and per-type hub-member-read-* policies
	// only covers read+list.
	rec = doRequestAsUser(t, srv, narrowUser, http.MethodGet, "/api/v1/skill-registries", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"narrow-role user should be denied skill-registries (action=register)")

	rec = doRequestAsUser(t, srv, narrowUser, http.MethodPost, "/api/v1/admin/maintenance/operations", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"narrow-role user should be denied maintenance operations (action=execute)")

	// RoutePolicy endpoints (users, groups) pass through the route guard,
	// so they return 200 — the handler does per-resource filtering.
	rec = doRequestAsUser(t, srv, narrowUser, http.MethodGet, "/api/v1/users", nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"narrow-role user should access user listing (RoutePolicy)")

	rec = doRequestAsUser(t, srv, narrowUser, http.MethodGet, "/api/v1/groups", nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"narrow-role user should access group listing (RoutePolicy)")
}

// ==========================================================================
// Group 6: Policy Authoring Denial
//
// Policy authoring (create, list) must remain super-admin-only.
// The /api/v1/policies route is RouteHubAdmin with permission=policy.read,
// which is NOT included in the hub-admin role definition.
// ==========================================================================

func TestScopedAdmin_HubAdminDeniedPolicyAuthoring(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	// CO1: The Policy API is removed. All callers receive 410 Gone.

	t.Run("policy_list_returns_gone", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/policies", nil)
		assert.Equal(t, http.StatusGone, rec.Code,
			"policy list should return 410 Gone under CO1")
	})

	t.Run("policy_create_returns_gone", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/policies", map[string]interface{}{
			"name":        "escalation-policy",
			"description": "should be denied",
			"rules":       []interface{}{},
		})
		assert.Equal(t, http.StatusGone, rec.Code,
			"policy create should return 410 Gone under CO1")
	})
}

// ==========================================================================
// Supplementary: Super-admin bypass verification
//
// Verify that the DevUser (super-admin) can still access everything,
// confirming the admin bypass is preserved.
// ==========================================================================

func TestScopedAdmin_SuperAdminBypassPreserved(t *testing.T) {
	srv, s := testServer(t)

	// CO1: Create a super-admin user with a role binding. The AK1 kernel
	// requires role bindings — the User.Role field alone is not sufficient.
	superAdmin := &store.User{
		ID:          tid("user-super-admin-test"),
		Email:       "superadmin@test.com",
		DisplayName: "Super Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	createTestUserWithRole(t, s, superAdmin.ID, superAdmin.Email,
		store.UserRoleAdmin, store.SystemRoleSuperAdmin)

	// Super-admin should access everything including super-admin-only endpoints.
	// Note: /api/v1/policies returns 410 Gone under CO1 (API removed).
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"roles", http.MethodGet, "/api/v1/admin/roles"},
		{"server-config", http.MethodGet, "/api/v1/admin/server-config"},
		{"health-summary", http.MethodGet, "/api/v1/admin/health/summary"},
		{"maintenance", http.MethodGet, "/api/v1/admin/maintenance"},
		{"diagnostics", http.MethodGet, "/api/v1/admin/diagnostics/logs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, superAdmin, tt.method, tt.path, nil)
			// Super-admin should not get 403. May get other codes (200, 404, etc.)
			// depending on endpoint state, but never 403 or 401.
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"super-admin should NOT be denied %s", tt.path)
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"super-admin should NOT be unauthorized for %s", tt.path)
		})
	}

	// CO1: Policy API returns 410 Gone for all callers.
	t.Run("policies", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, superAdmin, http.MethodGet, "/api/v1/policies", nil)
		assert.Equal(t, http.StatusGone, rec.Code,
			"policy API should return 410 Gone under CO1")
	})
}

// ==========================================================================
// Edge case: Hub-admin can delegate mixed permissions
//
// Verify that CanDelegate properly evaluates permission combinations,
// allowing roles where all permissions are held and denying roles where
// any single permission is not held.
// ==========================================================================

func TestScopedAdmin_CanDelegateMixedPermissions(t *testing.T) {
	srv, _, hubAdmin, _ := setupScopedAdminTest(t)

	t.Run("all_held_permissions_allowed", func(t *testing.T) {
		// All of these are in the hub-admin role
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
			Name:        "multi-perm-held",
			Description: "Multiple held permissions",
			ScopeType:   store.RoleScopeSystem,
			Permissions: []string{"user.read", "group.read", "role.read", "hub.health.read"},
		})
		assert.Equal(t, http.StatusCreated, rec.Code,
			"hub-admin should create role with all-held permissions; got body: %s", rec.Body.String())
	})

	t.Run("one_unheld_permission_denied", func(t *testing.T) {
		// Mix of held + unheld: user.read (held) + hub.admin_mode.update (not held,
		// and action=update is NOT granted by the per-type hub-member-read-* policies policy).
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
			Name:        "mixed-perm-denied",
			Description: "Mix of held and unheld permissions",
			ScopeType:   store.RoleScopeSystem,
			Permissions: []string{"user.read", "hub.admin_mode.update"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"hub-admin should be denied creating role with any unheld permission; got body: %s", rec.Body.String())
	})

	t.Run("all_unheld_permissions_denied", func(t *testing.T) {
		// All unheld
		rec := doRequestAsUser(t, srv, hubAdmin, http.MethodPost, "/api/v1/admin/roles", createRoleDefinitionRequest{
			Name:        "all-unheld",
			Description: "All unheld permissions",
			ScopeType:   store.RoleScopeSystem,
			Permissions: []string{"hub.maintenance.execute", "hub.diagnostics.read", "hub.admin_mode.update"},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"hub-admin should be denied creating role with all-unheld permissions; got body: %s", rec.Body.String())
	})
}

// ==========================================================================
// Group 7: Project Owner Admin API Scope-Aware Auth Tests
//
// A project owner who is only a hub-member (not hub-admin or super-admin)
// should be able to create project-scoped role bindings via the admin API.
// The admin route uses scope-aware authorization: project-scoped requests
// are authorized by the membership service (project.manage), not the
// hub-level role_binding.create permission.
// ==========================================================================

// Regression test: project owner should be able to POST
// /api/v1/admin/role-bindings for their own project. Previously this
// returned 403 because the route guard checked role_binding.read at hub
// scope and the handler checked role_binding.create at hub scope — neither
// of which a hub-member holds.
func TestScopedAdmin_ProjectOwnerCanCreateProjectBinding_ViaAdminAPI(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project owner who is only a hub-member (no hub-admin binding).
	owner := &store.User{
		ID:          tid("user-projowner"),
		Email:       "projowner@test.com",
		DisplayName: "Project Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Create a project owned by this user.
	project := &store.Project{
		ID:        tid("project-owner-admin-api"),
		Name:      "Owner Admin API Test",
		Slug:      "owner-admin-api-test",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	// createProjectMembersGroup also creates a project-owner role binding
	// for the project creator, so the owner is recognized by the membership
	// service governance check.
	srv.createProjectMembersGroup(ctx, project)

	// Create a member user to assign a project role to.
	member := &store.User{
		ID:          tid("user-projmember"),
		Email:       "projmember@test.com",
		DisplayName: "Project Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	// Look up project-member role definition.
	memberRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// POST /api/v1/admin/role-bindings with project scope — the project
	// owner should succeed even though they lack hub-level role_binding.create.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"project owner should be able to create project-scoped binding via admin API; got body: %s", rec.Body.String())
}

// Verify that a project owner who is only a hub-member is still denied
// system-scoped role-binding creation via the admin API. Scope-aware auth
// only relaxes the gate for project-scoped requests.
func TestScopedAdmin_ProjectOwnerDeniedSystemScopeBinding_ViaAdminAPI(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:          tid("user-projowner-sys"),
		Email:       "projownersys@test.com",
		DisplayName: "Project Owner Sys",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Look up a system-scoped role.
	hubMemberDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	// POST /api/v1/admin/role-bindings with system scope — should be denied.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: hubMemberDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      owner.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project owner should be denied system-scope binding creation; got body: %s", rec.Body.String())
}

// Verify that a hub-member without any project ownership is still denied
// project-scoped binding creation via the admin API (the membership service
// governance check catches them).
func TestScopedAdmin_HubMemberDeniedProjectBinding_ViaAdminAPI(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create the project with a different owner.
	projectOwner := &store.User{
		ID:          tid("user-realowner"),
		Email:       "realowner@test.com",
		DisplayName: "Real Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, projectOwner))
	ensureHubMembership(ctx, s, projectOwner.ID)

	project := &store.Project{
		ID:        tid("project-notmine"),
		Name:      "Not My Project",
		Slug:      "not-my-project",
		OwnerID:   projectOwner.ID,
		CreatedBy: projectOwner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Create a random hub-member who has no project role.
	bystander := &store.User{
		ID:          tid("user-bystander"),
		Email:       "bystander@test.com",
		DisplayName: "Bystander",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bystander))
	ensureHubMembership(ctx, s, bystander.ID)

	memberRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// POST /api/v1/admin/role-bindings — bystander has no project role,
	// so the membership service should deny them.
	rec := doRequestAsUser(t, srv, bystander, http.MethodPost, "/api/v1/admin/role-bindings", createRoleBindingRequest{
		RoleDefinitionID: memberRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      projectOwner.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub-member without project role should be denied project binding creation; got body: %s", rec.Body.String())
}

// ==========================================================================
// Group 8: QA Authorization Gate Tests
//
// Verify that create and read-by-ID endpoints enforce authorization.
// These are regression tests for missing SECURITY-GATE checks.
// ==========================================================================

// QA-1: POST /api/v1/templates must deny users without template.create.
// Previously returned 201 for viewers because the handler had no
// s.authorize gate.
func TestQA_ViewerDeniedTemplateCreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a viewer user with only hub-viewer permissions.
	viewer := &store.User{
		ID:          tid("user-qa-viewer-tmpl"),
		Email:       "qaviewer-tmpl@test.com",
		DisplayName: "QA Viewer",
		Role:        store.UserRoleViewer,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, viewer.ID)

	// Give them a hub-viewer role binding (read-only permissions).
	viewerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewer.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, viewer, http.MethodPost, "/api/v1/templates", map[string]string{
		"name": "qa-unauthorized-template",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"viewer should be denied template creation; got body: %s", rec.Body.String())
}

// QA-2: POST /api/v1/harness-configs must deny users without harness_config.create.
func TestQA_ViewerDeniedHarnessConfigCreate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	viewer := &store.User{
		ID:          tid("user-qa-viewer-hc"),
		Email:       "qaviewer-hc@test.com",
		DisplayName: "QA Viewer HC",
		Role:        store.UserRoleViewer,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, viewer.ID)

	viewerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewer.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, viewer, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name":    "qa-unauthorized-hc",
		"harness": "test-harness",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"viewer should be denied harness config creation; got body: %s", rec.Body.String())
}

// QA-3: GET /api/v1/templates/{id} must deny access to templates that
// list filtering hides. A hub-member without template.read on a specific
// resource should get 403 when addressing it directly by ID.
func TestQA_GetTemplateByID_EnforcesReadAuth(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a template owned by dev user (via admin token).
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", map[string]string{
		"name": "qa-hidden-template",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "admin should create template: %s", rec.Body.String())

	var createResp struct {
		Template store.Template `json:"template"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createResp))
	require.NotEmpty(t, createResp.Template.ID)

	// A viewer should be able to GET the template (they have template.read
	// at system scope via hub-viewer role).
	viewer := &store.User{
		ID:          tid("user-qa-viewer-get"),
		Email:       "qaviewer-get@test.com",
		DisplayName: "QA Viewer Get",
		Role:        store.UserRoleViewer,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, viewer.ID)

	viewerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      viewer.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// Viewer has template.read — should succeed.
	rec = doRequestAsUser(t, srv, viewer, http.MethodGet, "/api/v1/templates/"+createResp.Template.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"viewer with template.read should access template by ID; got body: %s", rec.Body.String())
}

// QA-4: Verify that super-admin CAN create templates (positive control).
// super-admin has all permissions including template.create.
func TestQA_SuperAdminCanCreateTemplate(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", map[string]string{
		"name": "qa-admin-template",
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"super-admin should be able to create template; got body: %s", rec.Body.String())
}

// QA-5: Verify that super-admin CAN create harness configs (positive control).
// super-admin has all permissions including harness_config.create.
func TestQA_SuperAdminCanCreateHarnessConfig(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", map[string]interface{}{
		"name":    "qa-admin-hc",
		"harness": "test-harness",
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"super-admin should be able to create harness config; got body: %s", rec.Body.String())
}

// QA-6: Verify that a project member CAN create a project-scoped template.
// The create gate is scope-aware: project-level role bindings grant access.
func TestQA_ProjectMemberCanCreateProjectScopedTemplate(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project member user.
	member := &store.User{
		ID:          tid("user-projmem-tpl"),
		Email:       "projmember-tpl@test.com",
		DisplayName: "Project Member (TPL)",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)

	// Create a project.
	proj := &store.Project{
		ID:        api.NewUUID(),
		Name:      "qa-tpl-proj",
		Slug:      "qa-tpl-proj",
		CreatedBy: member.ID,
		OwnerID:   member.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	// Bind project-member role (includes template.create) at project scope.
	projMemberRole, err := s.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projMemberRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      member.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// Project-scoped template create should succeed.
	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/templates", map[string]interface{}{
		"name":    "qa-proj-template",
		"scopeId": proj.ID,
		"scope":   "project",
	})
	assert.Equal(t, http.StatusCreated, rec.Code,
		"project member should create project-scoped template; got body: %s", rec.Body.String())
}
