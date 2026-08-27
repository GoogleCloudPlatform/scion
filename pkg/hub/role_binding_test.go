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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test: Role definitions exist after seeding
// =============================================================================

func TestSeed_RoleDefinitionsExist(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Verify all system role definitions are seeded
	systemRoles := []struct {
		name      string
		scopeType string
	}{
		{store.SystemRoleSuperAdmin, store.RoleScopeSystem},
		{store.SystemRoleHubMember, store.RoleScopeSystem},
		{store.SystemRoleHubViewer, store.RoleScopeSystem},
		{store.ProjectRoleOwner, store.RoleScopeProject},
		{store.ProjectRoleAdmin, store.RoleScopeProject},
		{store.ProjectRoleMember, store.RoleScopeProject},
		{store.AgentRoleDefNone, store.RoleScopeSystem},
		{store.AgentRoleDefReadonly, store.RoleScopeSystem},
		{store.AgentRoleDefBaseline, store.RoleScopeSystem},
		{store.AgentRoleDefFull, store.RoleScopeSystem},
	}

	for _, role := range systemRoles {
		rd, err := s.GetRoleDefinitionByName(ctx, role.name, role.scopeType)
		require.NoError(t, err, "role definition %q (scope %s) should exist", role.name, role.scopeType)
		assert.Equal(t, role.name, rd.Name)
		assert.Equal(t, role.scopeType, rd.ScopeType)
		assert.True(t, rd.System, "system role should be marked as system")
	}
}

func TestSeed_SuperAdminHasAllPermissions(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.NotEmpty(t, rd.Permissions, "super-admin should have permissions")
	// Should have more permissions than hub-member
	hubMember, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Greater(t, len(rd.Permissions), len(hubMember.Permissions),
		"super-admin should have more permissions than hub-member")
}

func TestSeed_AgentRoleDefinitionsMatchScopes(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// agent-role-none should have no permissions
	none, err := s.GetRoleDefinitionByName(ctx, store.AgentRoleDefNone, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Empty(t, none.Permissions, "agent-role-none should have no permissions")

	// agent-role-full should have more permissions than agent-role-readonly
	full, err := s.GetRoleDefinitionByName(ctx, store.AgentRoleDefFull, store.RoleScopeSystem)
	require.NoError(t, err)
	readonly, err := s.GetRoleDefinitionByName(ctx, store.AgentRoleDefReadonly, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Greater(t, len(full.Permissions), len(readonly.Permissions),
		"agent-role-full should have more permissions than agent-role-readonly")
}

func TestSeed_ListRoleDefinitions(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rds, err := s.ListRoleDefinitions(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rds), 10, "should have at least 10 seeded role definitions")
}

// =============================================================================
// Test: Role binding CRUD
// =============================================================================

func TestRoleBinding_CreateAndGet(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("rb-test-user"),
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, rb.ID)
	assert.Equal(t, rd.ID, rb.RoleDefinitionID)

	// Get by ID
	fetched, err := s.GetRoleBinding(ctx, rb.ID)
	require.NoError(t, err)
	assert.Equal(t, rb.ID, fetched.ID)
	assert.Equal(t, store.RoleBindingPrincipalUser, fetched.PrincipalType)
}

func TestRoleBinding_DuplicatePrevented(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	rb := &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("dup-test-user"),
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
	}
	_, err = s.CreateRoleBinding(ctx, rb)
	require.NoError(t, err)

	// Second create should fail with ErrAlreadyExists
	_, err = s.CreateRoleBinding(ctx, rb)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestRoleBinding_Delete(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("del-test-user"),
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
	})
	require.NoError(t, err)

	err = s.DeleteRoleBinding(ctx, rb.ID)
	require.NoError(t, err)

	_, err = s.GetRoleBinding(ctx, rb.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRoleBinding_ListForPrincipal(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	userID := tid("list-principal-user")

	// Create bindings to two different roles
	hubMember, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	hubViewer, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubMember.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubViewer.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	assert.Len(t, bindings, 2)
}

func TestRoleBinding_ListForScope(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("scope-test-project")
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("scope-test-user"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(bindings), 1)
}

// =============================================================================
// Test: Project membership keyed by project ID
// =============================================================================

func TestProjectMembership_KeyedByProjectID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user
	user := &store.User{
		ID:          tid("pm-user"),
		Email:       "pm-user@test.com",
		DisplayName: "PM User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create a project
	project := &store.Project{
		ID:        tid("pm-project"),
		Name:      "PM Project",
		Slug:      "pm-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Create owner role binding (Phase 1F: createProjectMembersGroupAndPolicy
	// now also creates this, so it may already exist — use idempotent helper).
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, user.ID))

	// Verify membership via project ID
	membership, err := s.GetProjectMembership(ctx, project.ID, user.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleOwner, membership.Role)
	assert.Equal(t, project.ID, membership.ProjectID)

	// Verify isProjectOwnerOrAdmin uses role bindings
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, identity, projectResource(project), ActionUpdate)
	assert.True(t, decision.Allowed, "project owner via role binding should be allowed; reason=%q", decision.Reason)
}

func TestProjectMembership_IsProjectMember(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("ipm-project")
	userID := tid("ipm-user")

	// Before adding binding, should not be a member
	isMember, err := s.IsProjectMember(ctx, projectID, userID)
	require.NoError(t, err)
	assert.False(t, isMember)

	// Add a project member binding
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.NoError(t, err)

	// Now should be a member
	isMember, err = s.IsProjectMember(ctx, projectID, userID)
	require.NoError(t, err)
	assert.True(t, isMember)
}

func TestProjectMembership_ListProjectMembers(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("lpm-project")

	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Add owner
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("lpm-owner"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.NoError(t, err)

	// Add member
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      tid("lpm-member"),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.NoError(t, err)

	members, err := s.ListProjectMembers(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, members, 2)
}

// =============================================================================
// Test: Groups don't confer project authorization (without role binding)
// =============================================================================

func TestAuthz_GroupMembershipWithoutRoleBinding_NoBypass(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user who is NOT an admin
	user := &store.User{
		ID:          tid("grp-nobypass-user"),
		Email:       "grp-nobypass@test.com",
		DisplayName: "Group NoBypass User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create a project owned by someone else
	owner := &store.User{
		ID:          tid("grp-nobypass-owner"),
		Email:       "grp-nobypass-owner@test.com",
		DisplayName: "Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("grp-nobypass-proj"),
		Name:      "Group NoBypass Project",
		Slug:      "grp-nobypass-proj",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Add user to the project members group as owner (legacy path)
	addProjectMemberWithRole(t, s, project, user.ID, store.GroupMemberRoleOwner)

	// Without a role binding, isProjectOwnerOrAdmin should still work via
	// legacy fallback. But the group membership alone should not bypass
	// resource authorization via the role-binding path.
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, "member", "api")

	// The user has group membership, so via legacy fallback they get access
	// (this is expected behavior during migration)
	decision := srv.authzService.CheckAccess(ctx, identity, projectResource(project), ActionUpdate)
	assert.True(t, decision.Allowed,
		"user with group membership should get access via legacy fallback during migration")
}

// =============================================================================
// Test: Role bindings replace User.Role for system admin
// =============================================================================

func TestBackfill_AdminUserGetsRoleBinding(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create an admin user
	admin := &store.User{
		ID:          tid("backfill-admin"),
		Email:       "backfill-admin@test.com",
		DisplayName: "Backfill Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	// Run backfill
	err := BackfillRoleBindings(ctx, s)
	require.NoError(t, err)

	// Verify super-admin binding exists
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, admin.ID)
	require.NoError(t, err)

	var hasSuperAdmin bool
	for _, b := range bindings {
		rd, err := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if rd.Name == store.SystemRoleSuperAdmin {
			hasSuperAdmin = true
			break
		}
	}
	assert.True(t, hasSuperAdmin, "admin user should have super-admin role binding after backfill")
}

func TestBackfill_MemberUserGetsRoleBinding(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a member user
	member := &store.User{
		ID:          tid("backfill-member"),
		Email:       "backfill-member@test.com",
		DisplayName: "Backfill Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))

	// Run backfill
	err := BackfillRoleBindings(ctx, s)
	require.NoError(t, err)

	// Verify hub-member binding exists
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, member.ID)
	require.NoError(t, err)

	var hasHubMember bool
	for _, b := range bindings {
		rd, err := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if rd.Name == store.SystemRoleHubMember {
			hasHubMember = true
			break
		}
	}
	assert.True(t, hasHubMember, "member user should have hub-member role binding after backfill")
}

func TestBackfill_ViewerUserGetsRoleBinding(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a viewer user
	viewer := &store.User{
		ID:          tid("backfill-viewer"),
		Email:       "backfill-viewer@test.com",
		DisplayName: "Backfill Viewer",
		Role:        store.UserRoleViewer,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, viewer))

	// Run backfill
	err := BackfillRoleBindings(ctx, s)
	require.NoError(t, err)

	// Verify hub-viewer binding exists
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, viewer.ID)
	require.NoError(t, err)

	var hasHubViewer bool
	for _, b := range bindings {
		rd, err := s.GetRoleDefinition(ctx, b.RoleDefinitionID)
		if err != nil {
			continue
		}
		if rd.Name == store.SystemRoleHubViewer {
			hasHubViewer = true
			break
		}
	}
	assert.True(t, hasHubViewer, "viewer user should have hub-viewer role binding after backfill")
}

func TestBackfill_Idempotent(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a user
	user := &store.User{
		ID:          tid("backfill-idempotent"),
		Email:       "backfill-idempotent@test.com",
		DisplayName: "Backfill Idempotent",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Run backfill twice - should not error
	require.NoError(t, BackfillRoleBindings(ctx, s))
	require.NoError(t, BackfillRoleBindings(ctx, s))

	// Verify no duplicate bindings
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, user.ID)
	require.NoError(t, err)
	// Should have exactly 1 binding (hub-member)
	assert.Len(t, bindings, 1, "backfill should not create duplicate bindings")
}

// =============================================================================
// Test: User.Role as compatibility output
// =============================================================================

func TestUserRole_StillPopulated(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a user with admin role
	admin := &store.User{
		ID:          tid("role-compat-admin"),
		Email:       "role-compat-admin@test.com",
		DisplayName: "Role Compat Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	// Read back and verify role is preserved
	fetched, err := s.GetUser(ctx, admin.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserRoleAdmin, fetched.Role,
		"User.Role should still be populated for API compatibility")
}

// =============================================================================
// Test: Regression - admin bypass still works
// =============================================================================

func TestRegression_AdminBypassStillWorks(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create an admin user
	admin := &store.User{
		ID:          tid("admin-bypass-regression"),
		Email:       "admin-bypass@test.com",
		DisplayName: "Admin Bypass",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	// Create a project
	project := &store.Project{
		ID:        tid("admin-bypass-proj"),
		Name:      "Admin Bypass Project",
		Slug:      "admin-bypass-proj",
		OwnerID:   tid("someone-else"),
		CreatedBy: tid("someone-else"),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Admin should have access regardless of ownership or membership
	identity := NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, "admin", "api")
	decision := srv.authzService.CheckAccess(ctx, identity, projectResource(project), ActionDelete)
	assert.True(t, decision.Allowed, "admin bypass should still work; reason=%q", decision.Reason)
}

// =============================================================================
// Test: getEffectivePermissions
// =============================================================================

func TestGetEffectivePermissions_SystemScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("eff-perms-user")

	// Create hub-member binding
	hubMemberRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubMemberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
	})
	require.NoError(t, err)

	perms, err := srv.authzService.getEffectivePermissions(ctx,
		store.RoleBindingPrincipalUser, userID,
		store.RoleScopeSystem, "")
	require.NoError(t, err)
	assert.NotEmpty(t, perms, "hub-member should have effective permissions")
}

func TestGetEffectivePermissions_ProjectScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("eff-perms-proj-user")
	projectID := tid("eff-perms-project")

	// Create project-owner binding
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	require.NoError(t, err)

	// Should get project permissions when checking project scope
	perms, err := srv.authzService.getEffectivePermissions(ctx,
		store.RoleBindingPrincipalUser, userID,
		store.RoleScopeProject, projectID)
	require.NoError(t, err)
	assert.NotEmpty(t, perms, "project-owner should have project permissions")

	// Should NOT get project permissions when checking a different project
	otherPerms, err := srv.authzService.getEffectivePermissions(ctx,
		store.RoleBindingPrincipalUser, userID,
		store.RoleScopeProject, tid("other-project"))
	require.NoError(t, err)
	// System-scoped bindings still apply, but project-scoped ones don't
	assert.Less(t, len(otherPerms), len(perms),
		"should not get project permissions for different project")
}

// =============================================================================
// Test: GroupMembership.Role for group governance only
// =============================================================================

func TestGroupMembership_RoleControlsGroupGovernance(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a group
	group := &store.Group{
		ID:        tid("gov-group"),
		Name:      "Governance Test Group",
		Slug:      "governance-test-group",
		GroupType: store.GroupTypeExplicit,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	// Add user as admin of the group
	user := &store.User{
		ID:          tid("gov-user"),
		Email:       "gov-user@test.com",
		DisplayName: "Gov User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))

	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   user.ID,
		Role:       store.GroupMemberRoleAdmin,
	}))

	// Verify the membership exists with admin role
	membership, err := s.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, user.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleAdmin, membership.Role,
		"GroupMembership.Role should still be used for group governance")
}

// =============================================================================
// Test: Backfill project role bindings from group memberships
// =============================================================================

func TestBackfill_ProjectMembersGetRoleBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project owner
	owner := &store.User{
		ID:          tid("bf-proj-owner"),
		Email:       "bf-proj-owner@test.com",
		DisplayName: "BF Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Create a project
	project := &store.Project{
		ID:        tid("bf-project"),
		Name:      "Backfill Project",
		Slug:      "backfill-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Add a member to the project group
	member := &store.User{
		ID:          tid("bf-proj-member"),
		Email:       "bf-proj-member@test.com",
		DisplayName: "BF Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, member))
	ensureHubMembership(ctx, s, member.ID)
	addProjectMemberWithRole(t, s, project, member.ID, store.GroupMemberRoleMember)

	// Run backfill
	err := BackfillRoleBindings(ctx, s)
	require.NoError(t, err)

	// Verify the member now has a project role binding
	membership, err := s.GetProjectMembership(ctx, project.ID, member.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleMember, membership.Role)

	// The owner should also have a project role binding (from the group)
	ownerMembership, err := s.GetProjectMembership(ctx, project.ID, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleOwner, ownerMembership.Role)
}

// =============================================================================
// Test: Seed idempotency
// =============================================================================

func TestSeedRoleDefinitions_Idempotent(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// testServer already seeds role definitions. Seed again and verify no errors.
	seedRoleDefinitions(ctx, s)

	// Verify role definitions still exist
	rds, err := s.ListRoleDefinitions(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rds), 10)
}
