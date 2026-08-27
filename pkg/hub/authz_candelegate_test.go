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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test helpers ---

// setupCanDelegateTest creates a test AuthzService with role definitions seeded.
func setupCanDelegateTest(t *testing.T) (*AuthzService, store.Store) {
	t.Helper()
	authz, s := authzTestSetup(t)
	return authz, s
}

// createTestUserWithRole creates a user and assigns them a system-scoped role binding.
// For super-admin bindings, CreatedBy is set to the system reconciler sentinel
// because the D10 store-level guard rejects non-reconciler callers.
func createTestUserWithRole(t *testing.T, s store.Store, userID, email, role, roleName string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: email, DisplayName: email, Role: role, Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err, "role definition %q not found", roleName)

	createdBy := "test"
	if roleName == store.SystemRoleSuperAdmin {
		createdBy = store.SystemReconcileCreatedBy
	}

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        createdBy,
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// createTestUserWithProjectRole creates a user and assigns them a project-scoped role binding.
func createTestUserWithProjectRole(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	// Create user if not exists
	if _, err := s.GetUser(ctx, userID); err != nil {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: email, DisplayName: email, Role: "member", Status: "active",
		}))
	}

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create project role binding: %v", err)
	}
}

// createDelegateTestProject creates a project in the store.
func createDelegateTestProject(t *testing.T, s store.Store, projectID, slug, createdBy string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      slug,
		Slug:      slug,
		CreatedBy: createdBy,
	}))
}

// --- Part D.1: Direct role binding tests ---

func TestCanDelegate_RoleBinding_SuperAdminCanDelegateAnything(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-delegate")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin@test.com", "Admin", "admin", "api")

	// Super-admin can create any role binding
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:             GrantTypeRoleBinding,
		RoleDefinitionID: rd.ID,
		ScopeType:        store.RoleScopeSystem,
	})
	assert.True(t, decision.Allowed, "super-admin should be able to delegate super-admin role binding")
	assert.Contains(t, decision.Reason, "super-admin")
}

func TestCanDelegate_RoleBinding_ScopedAdminCannotDelegateWider(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-rb")
	memberID := tid("member-rb")

	createDelegateTestProject(t, s, projectID, "test-project-rb", tid("owner"))
	createTestUserWithProjectRole(t, s, memberID, "member@test.com", projectID, store.ProjectRoleMember)

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	// Get project-owner role definition (has more perms than member)
	ownerRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)

	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:             GrantTypeRoleBinding,
		RoleDefinitionID: ownerRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	assert.False(t, decision.Allowed, "project-member should not be able to delegate project-owner role binding")
	assert.Contains(t, decision.Reason, "lacks permission")
}

func TestCanDelegate_RoleBinding_ProjectAdminCanDelegateProjectMember(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-rb2")
	adminUserID := tid("proj-admin-rb2")

	createDelegateTestProject(t, s, projectID, "test-project-rb2", tid("owner"))
	createTestUserWithProjectRole(t, s, adminUserID, "projadmin@test.com", projectID, store.ProjectRoleAdmin)

	projAdmin := NewAuthenticatedUser(adminUserID, "projadmin@test.com", "ProjAdmin", "member", "api")

	// Get project-member role definition
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	decision := authz.CanDelegate(ctx, projAdmin, GrantDescriptor{
		Type:             GrantTypeRoleBinding,
		RoleDefinitionID: memberRD.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
	})
	assert.True(t, decision.Allowed, "project-admin should be able to delegate project-member role binding")
}

// --- Part D.2: Group binding and nested group tests ---

func TestCanDelegate_GroupMembership_ScopedAdminCannotAddOwner(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-gm")
	memberID := tid("member-gm")

	createDelegateTestProject(t, s, projectID, "test-project-gm", tid("owner"))
	createTestUserWithProjectRole(t, s, memberID, "member@test.com", projectID, store.ProjectRoleMember)

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	// Member trying to add an owner to a project group
	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:      GrantTypeGroupMembership,
		GroupID:   tid("group-gm"),
		GroupRole: store.GroupMemberRoleOwner,
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	assert.False(t, decision.Allowed, "project member should not be able to add group owner")
}

func TestCanDelegate_GroupMembership_SuperAdminCanAddAnyRole(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-gm")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin@test.com", "Admin", "admin", "api")

	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:      GrantTypeGroupMembership,
		GroupID:   tid("group-gm2"),
		GroupRole: store.GroupMemberRoleOwner,
		ProjectID: tid("any-project"),
		ScopeType: store.RoleScopeProject,
		ScopeID:   tid("any-project"),
	})
	assert.True(t, decision.Allowed, "super-admin should be able to add any group role")
}

func TestCanDelegate_GroupMembership_AdminCanAddMember(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-gm3")
	adminUserID := tid("proj-admin-gm3")

	createDelegateTestProject(t, s, projectID, "test-project-gm3", tid("owner"))
	createTestUserWithProjectRole(t, s, adminUserID, "projadmin@test.com", projectID, store.ProjectRoleAdmin)

	projAdmin := NewAuthenticatedUser(adminUserID, "projadmin@test.com", "ProjAdmin", "member", "api")

	decision := authz.CanDelegate(ctx, projAdmin, GrantDescriptor{
		Type:      GrantTypeGroupMembership,
		GroupID:   tid("group-gm3"),
		GroupRole: store.GroupMemberRoleMember,
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	assert.True(t, decision.Allowed, "project-admin should be able to add group member")
}

// --- Part D.2b: Group-as-member nested group CanDelegate tests ---
// These tests verify that adding a GROUP as a member of a project members
// group is subject to the same CanDelegate check as adding a user. When
// group B is added to group A (a project members group) with a given role,
// group B's members inherit that role's project authority — so the actor
// must be able to delegate that authority level.

func TestCanDelegate_GroupMembership_AdminCannotAddGroupWithOwnerRole(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-grp-nest1")
	adminUserID := tid("admin-grp-nest1")

	createDelegateTestProject(t, s, projectID, "test-project-grp-nest1", tid("owner"))
	createTestUserWithProjectRole(t, s, adminUserID, "projadmin-nest@test.com", projectID, store.ProjectRoleAdmin)

	projAdmin := NewAuthenticatedUser(adminUserID, "projadmin-nest@test.com", "ProjAdmin", "member", "api")

	// A project-admin trying to add a group with owner role — this would
	// escalate authority because group members would inherit owner-level
	// project access. CanDelegate must deny.
	decision := authz.CanDelegate(ctx, projAdmin, GrantDescriptor{
		Type:      GrantTypeGroupMembership,
		GroupID:   tid("nested-group-1"),
		GroupRole: store.GroupMemberRoleOwner,
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	assert.False(t, decision.Allowed, "project-admin should NOT be able to add a group with owner role (escalation)")
	assert.Contains(t, decision.Reason, "lacks permission")
}

func TestCanDelegate_GroupMembership_OwnerCanAddGroupAsMember(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-grp-nest2")
	ownerID := tid("owner-grp-nest2")

	createDelegateTestProject(t, s, projectID, "test-project-grp-nest2", ownerID)
	createTestUserWithProjectRole(t, s, ownerID, "owner-nest@test.com", projectID, store.ProjectRoleOwner)

	owner := NewAuthenticatedUser(ownerID, "owner-nest@test.com", "Owner", "member", "api")

	// A project-owner adding a group with member role — owner has sufficient
	// authority to delegate member-level access.
	decision := authz.CanDelegate(ctx, owner, GrantDescriptor{
		Type:      GrantTypeGroupMembership,
		GroupID:   tid("nested-group-2"),
		GroupRole: store.GroupMemberRoleMember,
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	assert.True(t, decision.Allowed, "project-owner should be able to add a group as member")
}

func TestCanDelegate_GroupMembership_SuperAdminCanAddGroupWithAnyRole(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-grp-nest3")
	createTestUserWithRole(t, s, adminID, "admin-nest@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin-nest@test.com", "Admin", "admin", "api")

	// Super-admin can add a group with any role, including owner.
	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:      GrantTypeGroupMembership,
		GroupID:   tid("nested-group-3"),
		GroupRole: store.GroupMemberRoleOwner,
		ProjectID: tid("any-project-nest"),
		ScopeType: store.RoleScopeProject,
		ScopeID:   tid("any-project-nest"),
	})
	assert.True(t, decision.Allowed, "super-admin should be able to add a group with any role")
}

// --- Part D.3: Agent delegation tests ---

func TestCanDelegate_Agent_SuperAdminCanCreateAnyRole(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-agent")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin@test.com", "Admin", "admin", "api")

	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: tid("any-project"),
		ScopeType: store.RoleScopeProject,
		ScopeID:   tid("any-project"),
	})
	assert.True(t, decision.Allowed, "super-admin should be able to create full-role agent")
}

func TestCanDelegate_Agent_MemberWithCreatePermCanCreateFullRole(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-agent")
	memberID := tid("member-agent")

	createDelegateTestProject(t, s, projectID, "test-project-agent", tid("owner"))
	createTestUserWithProjectRole(t, s, memberID, "member@test.com", projectID, store.ProjectRoleMember)

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	// A project member with agent.create permission (from role binding) can
	// create agents of any role. The effective role is governed by the role
	// ceiling logic (project max, user ceiling), not by CanDelegate.
	// CanDelegate verifies the user has the base agent-create authority.
	assert.True(t, decision.Allowed, "project member with create permission should be able to request full-role agent")
}

func TestCanDelegate_Agent_MemberWithoutCreatePermCannotCreateAgent(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-agent-noperm")
	userID := tid("user-agent-noperm")

	createDelegateTestProject(t, s, projectID, "test-project-agent-noperm", tid("owner"))
	// Create user with NO project role binding — they have no agent.create
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "noperm@test.com", DisplayName: "NoPerm", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(userID, "noperm@test.com", "NoPerm", "member", "api")

	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	assert.False(t, decision.Allowed, "user without agent.create should not be able to create agents")
}

func TestCanDelegate_Agent_AgentCannotDelegateScopesItLacks(t *testing.T) {
	authz, _ := setupCanDelegateTest(t)
	ctx := context.Background()

	// Agent with only baseline scopes
	agentIdentity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("parent-agent")},
		ProjectID: tid("project-agent2"),
		Scopes:    ScopesForRole(AgentRoleBaseline),
	}}

	decision := authz.CanDelegate(ctx, agentIdentity, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: tid("project-agent2"),
		ScopeType: store.RoleScopeProject,
		ScopeID:   tid("project-agent2"),
	})
	assert.False(t, decision.Allowed, "baseline agent should not delegate full role")
	assert.Contains(t, decision.Reason, "agent lacks scope")
}

func TestCanDelegate_Agent_AgentCanDelegateOwnScopes(t *testing.T) {
	authz, _ := setupCanDelegateTest(t)
	ctx := context.Background()

	// Agent with full scopes
	agentIdentity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("parent-agent-full")},
		ProjectID: tid("project-agent3"),
		Scopes:    ScopesForRole(AgentRoleFull),
	}}

	decision := authz.CanDelegate(ctx, agentIdentity, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleBaseline),
		ProjectID: tid("project-agent3"),
		ScopeType: store.RoleScopeProject,
		ScopeID:   tid("project-agent3"),
	})
	assert.True(t, decision.Allowed, "full-role agent should be able to delegate baseline role")
}

func TestCanDelegate_Agent_NoneRoleAllowed(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-agent-none")
	memberID := tid("member-agent-none")

	createDelegateTestProject(t, s, projectID, "test-project-agent-none", tid("owner"))
	createTestUserWithProjectRole(t, s, memberID, "member@test.com", projectID, store.ProjectRoleMember)

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleNone),
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	assert.True(t, decision.Allowed, "any user should be able to create none-role agent")
}

// --- Part D.4: Custom role tests ---

func TestCanDelegate_CustomRole_SuperAdminCanCreateAny(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-cr")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin@test.com", "Admin", "admin", "api")

	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:                  GrantTypeCustomRole,
		CustomRolePermissions: []string{"agent.create", "agent.delete", "agent.read"},
		ScopeType:             store.RoleScopeSystem,
	})
	assert.True(t, decision.Allowed, "super-admin should be able to create any custom role")
}

func TestCanDelegate_CustomRole_UserCannotCreateWithUnheldPerms(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-cr")
	memberID := tid("member-cr")

	createDelegateTestProject(t, s, projectID, "test-project-cr", tid("owner"))
	createTestUserWithProjectRole(t, s, memberID, "member@test.com", projectID, store.ProjectRoleMember)

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	// Member does not hold agent.delete; trying to create a custom role with it should fail
	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:                  GrantTypeCustomRole,
		CustomRolePermissions: []string{"agent.create", "agent.delete"},
		ScopeType:             store.RoleScopeProject,
		ScopeID:               projectID,
	})
	assert.False(t, decision.Allowed, "project member should not create custom role with agent.delete")
}

// --- Part D.5: Scheduled dispatch tests ---

func TestCanDelegate_ScheduledDispatch_NilActor(t *testing.T) {
	authz, _ := setupCanDelegateTest(t)
	ctx := context.Background()

	decision := authz.CanDelegate(ctx, nil, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: tid("any"),
	})
	assert.False(t, decision.Allowed, "nil actor should be denied")
}

// TestCanDelegate_ScheduledDispatch_FireTimeRecheck verifies that a scheduled
// dispatch is denied at fire time when the creator's permissions have been
// revoked since creation time. This is the core value proposition of the
// fire-time CanDelegate recheck: authority is checked at fire time, not just
// at schedule creation time.
func TestCanDelegate_ScheduledDispatch_FireTimeRecheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("project-dispatch-recheck")
	userID := tid("user-dispatch-recheck")

	// Create a project and a user with project-admin role. Project-admin
	// passes both CheckAccess (via isProjectOwnerOrAdmin) and CanDelegate
	// (via actorHoldsAllPermissions for agent.create).
	createDelegateTestProject(t, s, projectID, "test-project-dispatch-recheck", tid("some-owner"))
	createTestUserWithProjectRole(t, s, userID, "dispatch-recheck@test.com", projectID, store.ProjectRoleAdmin)

	// Find the role binding we just created so we can revoke it later.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	require.NotEmpty(t, bindings, "user should have a role binding")
	var adminBindingID string
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			adminBindingID = b.ID
			break
		}
	}
	require.NotEmpty(t, adminBindingID, "should find the project-admin role binding")

	// Phase 1: verify that authorizeScheduledAgentCreate passes while the
	// user still has project-admin permissions.
	evt := store.ScheduledEvent{
		ID:        tid("dispatch-recheck-evt"),
		ProjectID: projectID,
		EventType: "dispatch_agent",
		CreatedBy: userID,
	}
	allowed, err := srv.authorizeScheduledAgentCreate(ctx, evt)
	require.NoError(t, err, "should succeed while user has project-admin role")
	assert.True(t, allowed, "user with project-admin role should be authorized")

	// Phase 2: revoke the user's project-admin role binding — simulating
	// permission revocation between schedule creation and fire time.
	require.NoError(t, s.DeleteRoleBinding(ctx, adminBindingID))

	// Phase 3: at fire time, authorizeScheduledAgentCreate should now deny
	// because the user no longer has the required permissions.
	_, err = srv.authorizeScheduledAgentCreate(ctx, evt)
	require.Error(t, err, "should be denied after permission revocation")
	assert.Contains(t, err.Error(), userID, "error should reference the user")
}

// --- Part D.6: Policy authoring tests ---

func TestCanDelegate_Policy_NonSuperAdminDenied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	memberID := tid("member-policy")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "member@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:           GrantTypePolicy,
		PolicyEffect:   "allow",
		PolicyActions:  []string{"read"},
		PolicyResource: "agent",
	})
	assert.False(t, decision.Allowed, "non-super-admin should not create policies")
	assert.Contains(t, decision.Reason, "super-admin")
}

func TestCanDelegate_Policy_SuperAdminAllowed(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-policy")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin@test.com", "Admin", "admin", "api")

	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:           GrantTypePolicy,
		PolicyEffect:   "allow",
		PolicyActions:  []string{"*"},
		PolicyResource: "*",
	})
	assert.True(t, decision.Allowed, "super-admin should be able to create policies")
}

// --- Part D.7: Phase 1E deferred item tests ---

func TestIsProjectOwnerOrAdmin_NoRoleBinding_ReturnsFalse(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-no-rb")
	userID := tid("user-no-rb")

	createDelegateTestProject(t, s, projectID, "test-project-no-rb", tid("some-owner"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "norb@test.com", DisplayName: "NoRB", Role: "member", Status: "active",
	}))

	// No role binding exists for this user in this project
	result := authz.isProjectOwnerOrAdmin(ctx, userID, projectID)
	assert.False(t, result, "should return false when no role binding exists")
}

func TestIsProjectOwnerOrAdmin_WithOwnerBinding_ReturnsTrue(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-with-rb")
	userID := tid("user-with-rb")

	createDelegateTestProject(t, s, projectID, "test-project-with-rb", userID)
	createTestUserWithProjectRole(t, s, userID, "owner@test.com", projectID, store.ProjectRoleOwner)

	result := authz.isProjectOwnerOrAdmin(ctx, userID, projectID)
	assert.True(t, result, "should return true when owner role binding exists")
}

func TestIsProjectOwnerOrAdmin_WithAdminBinding_ReturnsTrue(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-with-admin-rb")
	userID := tid("user-with-admin-rb")

	createDelegateTestProject(t, s, projectID, "test-project-with-admin-rb", tid("some-owner"))
	createTestUserWithProjectRole(t, s, userID, "admin@test.com", projectID, store.ProjectRoleAdmin)

	result := authz.isProjectOwnerOrAdmin(ctx, userID, projectID)
	assert.True(t, result, "should return true when admin role binding exists")
}

func TestIsProjectOwnerOrAdmin_WithMemberBinding_ReturnsFalse(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-member-rb")
	userID := tid("user-member-rb")

	createDelegateTestProject(t, s, projectID, "test-project-member-rb", tid("some-owner"))
	createTestUserWithProjectRole(t, s, userID, "member@test.com", projectID, store.ProjectRoleMember)

	result := authz.isProjectOwnerOrAdmin(ctx, userID, projectID)
	assert.False(t, result, "should return false for member role binding (not owner/admin)")
}

func TestIsSystemAdmin_WithSuperAdminBinding(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("sys-admin-check")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)

	result := authz.IsSystemAdmin(ctx, adminID)
	assert.True(t, result, "should return true for user with super-admin binding")
}

func TestIsSystemAdmin_WithoutSuperAdminBinding(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	memberID := tid("sys-member-check")
	createTestUserWithRole(t, s, memberID, "member@test.com", "member", store.SystemRoleHubMember)

	result := authz.IsSystemAdmin(ctx, memberID)
	assert.False(t, result, "should return false for user without super-admin binding")
}

func TestReconcileSuperAdminBindings_CreatesBindingForAdminUser(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	// Create an admin user without a super-admin role binding
	adminID := tid("reconcile-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "reconcile@test.com", DisplayName: "Reconcile", Role: "admin", Status: "active",
	}))

	// Run reconciliation with an admin list containing this user (forward mode).
	_, err := ReconcileSuperAdminBindings(ctx, s, []string{"reconcile@test.com"})
	require.NoError(t, err)

	// Verify binding was created
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, adminID)
	require.NoError(t, err)

	found := false
	for _, b := range bindings {
		if b.RoleDefinitionID == rd.ID && b.ScopeType == store.RoleScopeSystem {
			found = true
			break
		}
	}
	assert.True(t, found, "reconciliation should create super-admin binding for admin user")
}

func TestReconcileSuperAdminBindings_Idempotent(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("reconcile-idem")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID, Email: "idem@test.com", DisplayName: "Idem", Role: "admin", Status: "active",
	}))

	// Run twice with explicit admin emails list - should not error
	_, err := ReconcileSuperAdminBindings(ctx, s, []string{"idem@test.com"})
	require.NoError(t, err)
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{"idem@test.com"})
	require.NoError(t, err)
}

// --- UAT credential scope tests ---

func TestCanDelegate_UATCannotDelegateOutsideProject(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-uat")
	userID := tid("user-uat")

	createDelegateTestProject(t, s, projectID, "test-project-uat", userID)
	createTestUserWithProjectRole(t, s, userID, "owner@test.com", projectID, store.ProjectRoleOwner)

	// Create a scoped user identity (UAT)
	baseUser := NewAuthenticatedUser(userID, "owner@test.com", "Owner", "member", "api")
	scopedUser := NewScopedUserIdentity(baseUser, projectID, []string{"agent:create"})

	// Try to delegate to a different project
	decision := authz.CanDelegate(ctx, scopedUser, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleBaseline),
		ProjectID: tid("other-project"),
		ScopeType: store.RoleScopeProject,
		ScopeID:   tid("other-project"),
	})
	assert.False(t, decision.Allowed, "UAT should not delegate outside its project")
	assert.Contains(t, decision.Reason, "scoped credential")
}

func TestCanDelegate_UATCannotCreateSystemGrants(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("admin-uat-sys")
	createTestUserWithRole(t, s, adminID, "admin@test.com", "admin", store.SystemRoleSuperAdmin)

	baseUser := NewAuthenticatedUser(adminID, "admin@test.com", "Admin", "admin", "api")
	scopedUser := NewScopedUserIdentity(baseUser, tid("any-project"), []string{"agent:create"})

	decision := authz.CanDelegate(ctx, scopedUser, GrantDescriptor{
		Type:      GrantTypeRoleBinding,
		ScopeType: store.RoleScopeSystem,
	})
	assert.False(t, decision.Allowed, "UAT should not create system-scoped grants")
	assert.Contains(t, decision.Reason, "scoped credential")
}

// --- Edge cases ---

func TestCanDelegate_EmptyPermissionSet(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	memberID := tid("member-empty")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "member@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         tid("any-project"),
	})
	assert.True(t, decision.Allowed, "empty permission set should be allowed")
}

func TestCanDelegate_UnknownGrantType(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	memberID := tid("member-unknown")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: "member@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))

	member := NewAuthenticatedUser(memberID, "member@test.com", "Member", "member", "api")

	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type: "unknown",
	})
	assert.False(t, decision.Allowed, "unknown grant type should be denied")
}

func TestCanDelegate_ProjectMembership_OwnerCanManage(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-pm")
	ownerID := tid("owner-pm")

	createDelegateTestProject(t, s, projectID, "test-project-pm", ownerID)
	createTestUserWithProjectRole(t, s, ownerID, "owner@test.com", projectID, store.ProjectRoleOwner)

	owner := NewAuthenticatedUser(ownerID, "owner@test.com", "Owner", "member", "api")

	decision := authz.CanDelegate(ctx, owner, GrantDescriptor{
		Type:      GrantTypeProjectMembership,
		ProjectID: projectID,
	})
	assert.True(t, decision.Allowed, "project owner should be able to manage project membership")
}

func TestCanDelegate_ProjectMembership_NonMemberDenied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("project-pm-deny")
	userID := tid("user-pm-deny")

	createDelegateTestProject(t, s, projectID, "test-project-pm-deny", tid("some-owner"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "random@test.com", DisplayName: "Random", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(userID, "random@test.com", "Random", "member", "api")

	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:      GrantTypeProjectMembership,
		ProjectID: projectID,
	})
	assert.False(t, decision.Allowed, "non-member should not manage project membership")
}

// =============================================================================
// D10: Super-Admin Must NOT Be Grantable Through Role-Binding Machinery
// =============================================================================

// D10 AC1: A non-reconciler caller attempting to create a system-scoped
// super-admin binding gets an error.
func TestD10_NonReconcilerCannotCreateSuperAdminBinding(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	userID := tid("d10-user")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "d10@test.com", DisplayName: "D10", Role: "member", Status: "active",
	}))

	// Attempt to create a super-admin binding as a non-reconciler caller.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "handler", // NOT system-reconcile
	})
	require.ErrorIs(t, err, store.ErrSuperAdminBindingRestricted,
		"non-reconciler caller must be refused super-admin binding")
}

// D10 AC2: The system reconciler can still create super-admin bindings.
func TestD10_ReconcilerCanCreateSuperAdminBinding(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	userID := tid("d10-reconciler")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "reconciler@test.com", DisplayName: "Reconciler", Role: "admin", Status: "active",
	}))

	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err, "system reconciler must be allowed to create super-admin binding")
	assert.NotEmpty(t, rb.ID)
}

// D10 AC3: Existing super-admin bindings created by the reconciler continue to work.
func TestD10_ExistingSuperAdminBindingsWork(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("d10-existing")
	createTestUserWithRole(t, s, adminID, "existing@test.com", "admin", store.SystemRoleSuperAdmin)

	// Verify the user is recognized as a system admin via bindings.
	assert.True(t, authz.IsSystemAdmin(ctx, adminID),
		"existing super-admin binding from reconciler must still work")
}

// D10 supplemental: non-super-admin bindings are unaffected by the guard.
func TestD10_NonSuperAdminBindingsUnaffected(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)

	userID := tid("d10-member")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "member@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))

	// Creating a hub-member binding with any CreatedBy should work.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "handler",
	})
	require.NoError(t, err, "non-super-admin bindings must not be affected by D10 guard")
}

// =============================================================================
// D11: Super-Admin Must Be Revocable
// =============================================================================

// D11 AC1: A user removed from AdminEmails loses User.Role == "admin"
// on next restart (via reconciliation).
func TestD11_RemovedFromAdminEmailsLosesRole(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	userID := tid("d11-demote")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "demote@test.com", DisplayName: "Demote", Role: "admin", Status: "active",
	}))

	// Create super-admin binding (as reconciler, to satisfy D10).
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Create the replacement admin so the effect guard sees a non-empty
	// intended admin set (D11-fix3: effect guard requires at least one
	// existing user matching AdminEmails).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("d11-other-admin"), Email: "other-admin@test.com", DisplayName: "Other", Role: "member", Status: "active",
	}))

	// Reconcile with adminEmails that do NOT include this user.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{"other-admin@test.com"})
	require.NoError(t, err)

	// Verify role was demoted.
	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "member", u.Role, "user removed from AdminEmails should be demoted to member")
}

// D11 AC2: A user removed from AdminEmails has their system super-admin
// binding deleted on next restart.
func TestD11_RemovedFromAdminEmailsLosesBinding(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	userID := tid("d11-unbind")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "unbind@test.com", DisplayName: "Unbind", Role: "admin", Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Create the replacement admin so the effect guard sees a non-empty
	// intended admin set (D11-fix3).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("d11-other-unbind"), Email: "other@test.com", DisplayName: "Other", Role: "member", Status: "active",
	}))

	// Reconcile with a list that does NOT include this user.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{"other@test.com"})
	require.NoError(t, err)

	// Verify binding was deleted.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			t.Fatal("orphaned super-admin binding should have been deleted")
		}
	}
}

// D11 AC3 (REGRESSION TEST — the trap): A functionally-admin user (has ordinary
// admin-right grants, NOT in AdminEmails) keeps EVERY grant across a restart.
func TestD11_FunctionallyAdminUserKeepsOrdinaryGrants(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	// Create a user who is NOT admin and NOT in AdminEmails but has an
	// ordinary hub-member binding (representing functional admin-like grants).
	userID := tid("d11-func-admin")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "func-admin@test.com", DisplayName: "FuncAdmin", Role: "member", Status: "active",
	}))

	hubMemberRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: hubMemberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create the intended admin so the effect guard sees a non-empty
	// intended admin set (D11-fix3).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("d11-real-admin-fa"), Email: "real-admin@test.com", DisplayName: "RealAdmin", Role: "member", Status: "active",
	}))

	// Run reconciliation with an admin list that does NOT include this user.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{"real-admin@test.com"})
	require.NoError(t, err)

	// Verify the ordinary hub-member binding is still there.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)

	var hasHubMember bool
	for _, b := range bindings {
		if b.RoleDefinitionID == hubMemberRD.ID {
			hasHubMember = true
		}
	}
	assert.True(t, hasHubMember, "ordinary hub-member binding must survive reconciliation")

	// Verify the user's role is still "member" (not demoted from something else).
	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "member", u.Role, "member role must not be changed by reconciliation")
}

// D11 AC4: When AdminEmails is empty, no demotions occur and a warning is logged.
func TestD11_EmptyAdminEmailsNoChange(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	userID := tid("d11-empty-guard")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "guard@test.com", DisplayName: "Guard", Role: "admin", Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Reconcile with an EMPTY admin emails list.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{})
	require.NoError(t, err)

	// Verify user was NOT demoted (empty-list safety guard).
	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "admin", u.Role, "empty AdminEmails must not trigger demotion")

	// Verify binding still exists.
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	var found bool
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			found = true
		}
	}
	assert.True(t, found, "super-admin binding must survive when AdminEmails is empty")
}

// D11-fix2 AC1: A user with Role=="admin" who is removed from AdminEmails must
// NOT have a super-admin binding created during reconciliation, even when
// forward repair would ordinarily create one.
func TestD11Fix2_ForwardPassGatedOnAdminList(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	// Create a user with Role=="admin" but NOT in the admin emails list.
	// In the old two-pass code, the forward pass would create a binding for
	// this user (because Role=="admin"), and the reverse pass would delete it.
	// If the reverse pass failed, the binding would leak.
	userID := tid("d11fix2-noleak")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "removed-admin@test.com", DisplayName: "Removed", Role: "admin", Status: "active",
	}))

	// Create the intended admin so the effect guard sees a non-empty
	// intended admin set (D11-fix3).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("d11fix2-real-admin"), Email: "real-admin@test.com", DisplayName: "RealAdmin", Role: "member", Status: "active",
	}))

	// Reconcile with an admin list that does NOT include this user.
	_, err := ReconcileSuperAdminBindings(ctx, s, []string{"real-admin@test.com"})
	require.NoError(t, err)

	// Verify: no super-admin binding was created for the removed admin.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			t.Fatal("forward pass must NOT create super-admin binding for user not in AdminEmails")
		}
	}

	// Verify the user was demoted.
	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "member", u.Role, "user not in AdminEmails should be demoted")
}

// D11-fix2 AC2: nil and empty adminEmails both take the same safety-guard branch
// (no demotions and no promotions).
func TestD11Fix2_NilAndEmptyCollapse(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	userID := tid("d11fix2-collapse")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "stable@test.com", DisplayName: "Stable", Role: "admin", Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// nil adminEmails — should not demote or delete binding.
	_, err = ReconcileSuperAdminBindings(ctx, s, nil)
	require.NoError(t, err)

	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "admin", u.Role, "nil AdminEmails must not trigger demotion")

	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	var found bool
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
			found = true
		}
	}
	assert.True(t, found, "nil AdminEmails must not delete super-admin binding")

	// empty []string{} — same behavior as nil.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{})
	require.NoError(t, err)

	u, err = s.GetUser(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, "admin", u.Role, "empty AdminEmails must not trigger demotion")
}

// D11 AC5: determineUserRole demotes admin when email removed from adminEmails.
func TestD11_DetermineUserRole_DemotesAdmin(t *testing.T) {
	// User was admin, adminEmails non-empty but does not include user → demote.
	got := determineUserRole("former@example.com", []string{"real-admin@example.com"}, "admin", true)
	assert.Equal(t, "member", got, "admin not in adminEmails should be demoted to member")
}

// D11 AC5b: determineUserRole does NOT demote admin when adminEmails is empty.
func TestD11_DetermineUserRole_EmptyListNoChange(t *testing.T) {
	got := determineUserRole("admin@example.com", nil, "admin", true)
	assert.Equal(t, "admin", got, "admin should not be demoted when adminEmails is nil")

	got = determineUserRole("admin@example.com", []string{}, "admin", true)
	assert.Equal(t, "admin", got, "admin should not be demoted when adminEmails is empty")
}

// D11 AC5c: determineUserRole preserves non-admin roles.
func TestD11_DetermineUserRole_PreservesViewer(t *testing.T) {
	got := determineUserRole("viewer@example.com", []string{"admin@example.com"}, "viewer", true)
	assert.Equal(t, "viewer", got, "viewer role must be preserved")
}

// =============================================================================
// D11-fix3: AdminEmails whitespace normalization + effect guard
// =============================================================================

// D11-fix3 AC1: AdminEmails with leading/trailing whitespace still matches
// the admin user after config-level sanitization (SanitizeEmailList).
func TestD11Fix3_WhitespaceAdminEmailsMatch(t *testing.T) {
	// After SanitizeEmailList, "  admin@example.com  " becomes "admin@example.com".
	// determineUserRole should match.
	sanitized := config.SanitizeEmailList([]string{"  admin@example.com  "})
	got := determineUserRole("admin@example.com", sanitized, "member", true)
	assert.Equal(t, "admin", got, "sanitized whitespace-padded admin email should match")
}

// D11-fix3 AC2: AdminEmails with empty entries after trimming are dropped
// (no false matches from empty strings).
func TestD11Fix3_EmptyEntriesDropped(t *testing.T) {
	sanitized := config.SanitizeEmailList([]string{"admin@example.com", "", "  ", "\t"})
	assert.Len(t, sanitized, 1, "empty entries should be dropped after sanitization")
	assert.Equal(t, "admin@example.com", sanitized[0])
}

// D11-fix3 AC3 (effect guard): Reconciliation refuses to demote when it would
// leave ZERO administrators (AdminEmails contains emails that match no existing
// users).
func TestD11Fix3_EffectGuardRefusesZeroAdmins(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	// Create two admin users.
	adminID1 := tid("effect-guard-admin1")
	adminID2 := tid("effect-guard-admin2")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID1, Email: "admin1@test.com", DisplayName: "Admin1", Role: "admin", Status: "active",
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: adminID2, Email: "admin2@test.com", DisplayName: "Admin2", Role: "admin", Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	for _, uid := range []string{adminID1, adminID2} {
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      uid,
			ScopeType:        store.RoleScopeSystem,
			ScopeID:          "",
			CreatedBy:        store.SystemReconcileCreatedBy,
		})
		require.NoError(t, err)
	}

	// Reconcile with AdminEmails that match NO existing users (typos/non-existent).
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{"nobody@test.com", "also-nobody@test.com"})
	require.NoError(t, err)

	// Verify: both admins still have their role (effect guard fired).
	u1, err := s.GetUser(ctx, adminID1)
	require.NoError(t, err)
	assert.Equal(t, "admin", u1.Role, "effect guard should prevent demotion when intended admin set is empty")

	u2, err := s.GetUser(ctx, adminID2)
	require.NoError(t, err)
	assert.Equal(t, "admin", u2.Role, "effect guard should prevent demotion when intended admin set is empty")
}

// D11-fix3 AC4 (admin rotation): alice→bob rotation works when bob is an
// existing user. Alice should be demoted, bob should be promoted.
func TestD11Fix3_AdminRotationAliceToBob(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	aliceID := tid("rotation-alice")
	bobID := tid("rotation-bob")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: aliceID, Email: "alice@test.com", DisplayName: "Alice", Role: "admin", Status: "active",
	}))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: bobID, Email: "bob@test.com", DisplayName: "Bob", Role: "member", Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      aliceID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Rotate: replace alice with bob in AdminEmails.
	_, err = ReconcileSuperAdminBindings(ctx, s, []string{"bob@test.com"})
	require.NoError(t, err)

	// Alice should be demoted.
	alice, err := s.GetUser(ctx, aliceID)
	require.NoError(t, err)
	assert.Equal(t, "member", alice.Role, "alice should be demoted after rotation")

	// Bob should be promoted.
	bob, err := s.GetUser(ctx, bobID)
	require.NoError(t, err)
	assert.Equal(t, "admin", bob.Role, "bob should be promoted after rotation")
}
