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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// DG1: Group Resolution and Delegation Safety Tests
//
// Merge gate requirements:
//   - Escalation tests for ordinary role-bearing groups
//   - Escalation tests for nested groups
//   - Escalation tests for agent callers
//   - Concurrent membership/role change safety
//   - Cycle detection tests
//   - No special-project-group exception remains in delegation checks
// =============================================================================

// --- DG1.1: Escalation tests for ordinary role-bearing groups ---

// TestCanDelegate_GroupMembership_RoleBearingGroup_ActorHoldsPerms verifies
// that an actor who holds all permissions inherited through a role-bearing
// group can add members to that group.
func TestCanDelegate_GroupMembership_RoleBearingGroup_ActorHoldsPerms(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-1")
	ownerID := tid("dg1-owner-1")
	groupID := tid("dg1-group-1")

	// Create project, owner, and group.
	createDelegateTestProject(t, s, projectID, "dg1-test-proj-1", ownerID)
	createTestUserWithProjectRole(t, s, ownerID, "dg1-owner@test.com", projectID, store.ProjectRoleOwner)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-1", Name: "DG1 Group 1",
	}))

	// Bind project-member role to the group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	owner := NewAuthenticatedUser(ownerID, "dg1-owner@test.com", "Owner", "member", "api")

	// Owner holds project-owner permissions which are a superset of project-member.
	decision := authz.CanDelegate(ctx, owner, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"project-owner should be able to add members to a group with project-member binding")
}

// TestCanDelegate_GroupMembership_RoleBearingGroup_ActorLacksPerms verifies
// that an actor who does NOT hold the permissions inherited through a
// role-bearing group is denied from adding members.
func TestCanDelegate_GroupMembership_RoleBearingGroup_ActorLacksPerms(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-2")
	userID := tid("dg1-user-2")
	groupID := tid("dg1-group-2")

	// Create project but user has NO project role.
	createDelegateTestProject(t, s, projectID, "dg1-test-proj-2", tid("some-owner-2"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dg1-noperm@test.com", DisplayName: "NoPerm", Role: "member", Status: "active",
	}))

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-2", Name: "DG1 Group 2",
	}))

	// Bind project-admin role to the group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(userID, "dg1-noperm@test.com", "NoPerm", "member", "api")

	// User has no project permissions — should be denied.
	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"user without project permissions should NOT be able to add members to a role-bearing group")
	assert.Contains(t, decision.Reason, "cannot delegate")
}

// TestCanDelegate_GroupMembership_NoRoleBindings_Allowed verifies that adding
// members to a group with no role bindings requires no delegation check.
func TestCanDelegate_GroupMembership_NoRoleBindings_Allowed(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	userID := tid("dg1-user-norb")
	groupID := tid("dg1-group-norb")

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dg1-norb@test.com", DisplayName: "NoRB", Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-norb", Name: "DG1 NoRB Group",
	}))

	user := NewAuthenticatedUser(userID, "dg1-norb@test.com", "NoRB", "member", "api")

	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"adding members to group with no role bindings should be allowed")
	assert.Contains(t, decision.Reason, "no role bindings")
}

// TestCanDelegate_GroupMembership_SystemScopeBinding verifies delegation
// check for a group with a system-scoped role binding.
func TestCanDelegate_GroupMembership_SystemScopeBinding(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	memberID := tid("dg1-user-sys")
	groupID := tid("dg1-group-sys")

	// Create a user with only hub-member role.
	createTestUserWithRole(t, s, memberID, "dg1-sys@test.com", "member", store.SystemRoleHubMember)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-sys", Name: "DG1 System Group",
	}))

	// Bind hub-admin role to the group at system scope.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	member := NewAuthenticatedUser(memberID, "dg1-sys@test.com", "Member", "member", "api")

	// Hub-member lacks hub-admin permissions — should be denied.
	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"hub-member should NOT be able to add members to a group with hub-admin binding")
}

// TestCanDelegate_GroupMembership_MultipleBindings verifies that the actor
// must hold permissions from ALL bindings on the group, not just one.
func TestCanDelegate_GroupMembership_MultipleBindings(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID1 := tid("dg1-proj-multi1")
	projectID2 := tid("dg1-proj-multi2")
	adminID := tid("dg1-admin-multi")
	groupID := tid("dg1-group-multi")

	createDelegateTestProject(t, s, projectID1, "dg1-multi-proj1", tid("owner-multi"))
	createDelegateTestProject(t, s, projectID2, "dg1-multi-proj2", tid("owner-multi"))

	// User is admin in project1 but has NO role in project2.
	createTestUserWithProjectRole(t, s, adminID, "dg1-admin-multi@test.com", projectID1, store.ProjectRoleAdmin)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-multi", Name: "DG1 Multi Group",
	}))

	// Bind to project1 AND project2.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	for _, pid := range []string{projectID1, projectID2} {
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      groupID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          pid,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	admin := NewAuthenticatedUser(adminID, "dg1-admin-multi@test.com", "Admin", "member", "api")

	// Admin in project1 but not project2 — should be denied because the group
	// confers authority in BOTH projects.
	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"actor must hold permissions in ALL scopes the group has bindings for")
}

// --- DG1.2: Escalation tests for nested groups ---

// TestCanDelegate_GroupMembership_NestedGroup_InheritsParentAuthority verifies
// that adding a member to a group inherits authority from parent group bindings.
func TestCanDelegate_GroupMembership_NestedGroup_InheritsParentAuthority(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-nest")
	ownerID := tid("dg1-owner-nest")
	parentGroupID := tid("dg1-parent-grp")
	childGroupID := tid("dg1-child-grp")

	createDelegateTestProject(t, s, projectID, "dg1-nest-proj", ownerID)
	createTestUserWithProjectRole(t, s, ownerID, "dg1-owner-nest@test.com", projectID, store.ProjectRoleOwner)

	// Create parent and child groups, then nest child under parent.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: parentGroupID, Slug: "dg1-parent", Name: "DG1 Parent",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: childGroupID, Slug: "dg1-child", Name: "DG1 Child",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    parentGroupID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   childGroupID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Bind project-admin to the PARENT group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      parentGroupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	owner := NewAuthenticatedUser(ownerID, "dg1-owner-nest@test.com", "Owner", "member", "api")

	// Adding a user to the CHILD group means that user inherits the parent's
	// project-admin binding. Owner can delegate this.
	decision := authz.CanDelegate(ctx, owner, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: childGroupID,
	})
	assert.True(t, decision.Allowed,
		"owner should be able to add members to child group that inherits parent authority")
}

// TestCanDelegate_GroupMembership_NestedGroup_ActorLacksInheritedPerms verifies
// that an actor who lacks the permissions inherited through a parent group
// is denied from adding members to the child group.
func TestCanDelegate_GroupMembership_NestedGroup_ActorLacksInheritedPerms(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-nest2")
	memberID := tid("dg1-member-nest2")
	parentGroupID := tid("dg1-parent-grp2")
	childGroupID := tid("dg1-child-grp2")

	createDelegateTestProject(t, s, projectID, "dg1-nest-proj2", tid("owner-nest2"))
	createTestUserWithProjectRole(t, s, memberID, "dg1-member-nest@test.com", projectID, store.ProjectRoleMember)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: parentGroupID, Slug: "dg1-parent2", Name: "DG1 Parent 2",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: childGroupID, Slug: "dg1-child2", Name: "DG1 Child 2",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    parentGroupID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   childGroupID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Bind project-admin to the PARENT group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      parentGroupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	member := NewAuthenticatedUser(memberID, "dg1-member-nest@test.com", "Member", "member", "api")

	// Project-member lacks project-admin permissions — should be denied.
	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: childGroupID,
	})
	assert.False(t, decision.Allowed,
		"project-member should NOT be able to add members to child group that inherits project-admin from parent")
}

// TestCanDelegate_GroupMembership_DeeplyNestedGroup verifies delegation
// check traverses multiple levels of nesting (A contains B contains C).
func TestCanDelegate_GroupMembership_DeeplyNestedGroup(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-deep")
	memberID := tid("dg1-member-deep")
	groupA := tid("dg1-group-A")
	groupB := tid("dg1-group-B")
	groupC := tid("dg1-group-C")

	createDelegateTestProject(t, s, projectID, "dg1-deep-proj", tid("owner-deep"))
	createTestUserWithProjectRole(t, s, memberID, "dg1-deep@test.com", projectID, store.ProjectRoleMember)

	// Create chain: A contains B contains C
	for _, g := range []struct{ id, slug string }{
		{groupA, "dg1-grp-A"},
		{groupB, "dg1-grp-B"},
		{groupC, "dg1-grp-C"},
	} {
		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID: g.id, Slug: g.slug, Name: g.slug,
		}))
	}
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupA, MemberType: store.GroupMemberTypeGroup, MemberID: groupB, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupB, MemberType: store.GroupMemberTypeGroup, MemberID: groupC, Role: store.GroupMemberRoleMember,
	}))

	// Bind project-admin to group A.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupA,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	member := NewAuthenticatedUser(memberID, "dg1-deep@test.com", "Member", "member", "api")

	// Adding to group C means member inherits B→A's project-admin binding.
	// Project-member lacks project-admin perms — should be denied.
	decision := authz.CanDelegate(ctx, member, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupC,
	})
	assert.False(t, decision.Allowed,
		"delegation check must traverse deeply nested groups to find inherited authority")
}

// --- DG1.3: Escalation tests for agent callers ---

// TestCanDelegate_GroupMembership_AgentCaller_PassesSameTest verifies that
// an agent caller with group.addMember authority must pass the same
// delegation test as a user caller. The agent is not exempted from
// checking role-bearing group authority.
func TestCanDelegate_GroupMembership_AgentCaller_PassesSameTest(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-agent-proj1")
	groupID := tid("dg1-agent-grp1")

	createDelegateTestProject(t, s, projectID, "dg1-agent-proj1", tid("owner-agent1"))

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-agent-grp1", Name: "DG1 Agent Group 1",
	}))

	// Bind project-admin to the group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Agent with only baseline scopes — insufficient to delegate admin perms.
	agentIdentity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("dg1-agent-caller1")},
		ProjectID: projectID,
		Scopes:    ScopesForRole(AgentRoleBaseline),
	}}

	// Agent must pass the same delegation test — it lacks the project-admin
	// permissions so it should be denied.
	decision := authz.CanDelegate(ctx, agentIdentity, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"agent caller should be denied when it lacks permissions inherited through the group")
}

// TestCanDelegate_GroupMembership_AgentCaller_RoleBearingGroup verifies that
// an agent cannot add members to a role-bearing group if it lacks the
// inherited permissions.
func TestCanDelegate_GroupMembership_AgentCaller_RoleBearingGroup(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-agent-proj2")
	groupID := tid("dg1-agent-grp2")
	agentID := tid("dg1-agent-2")

	createDelegateTestProject(t, s, projectID, "dg1-agent-proj2", tid("owner-agent2"))

	// Create agent in store.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Slug:      "dg1-test-agent",
		Name:      "dg1-test-agent",
		ProjectID: projectID,
	}))

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-agent-grp2", Name: "DG1 Agent Group",
	}))

	// Bind project-admin to the group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	agentIdentity := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    ScopesForRole(AgentRoleBaseline),
	}}

	// Agent with baseline scopes cannot delegate project-admin authority.
	decision := authz.CanDelegate(ctx, agentIdentity, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"agent caller should be denied adding members to group with role-bearing authority it lacks")
}

// TestCanDelegate_GroupMembership_SuperAdminBypass verifies that a super-admin
// can add members to any role-bearing group.
func TestCanDelegate_GroupMembership_SuperAdminBypass(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-sa")
	adminID := tid("dg1-admin-sa")
	groupID := tid("dg1-group-sa")

	createDelegateTestProject(t, s, projectID, "dg1-sa-proj", adminID)
	createTestUserWithRole(t, s, adminID, "dg1-sa@test.com", "admin", store.SystemRoleSuperAdmin)

	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-sa", Name: "DG1 SA Group",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	admin := NewAuthenticatedUser(adminID, "dg1-sa@test.com", "Admin", "admin", "api")

	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"super-admin should be able to add members to any role-bearing group")
}

// --- DG1.4: No special-project-group exception remains ---

// TestCanDelegate_GroupMembership_NoSpecialProjectGroupException verifies
// that GroupMembership.Role (governance) does NOT substitute for resource
// authority. A user with no project permissions cannot add members to a
// role-bearing group, even if they have governance access to a different
// group.
func TestCanDelegate_GroupMembership_NoSpecialProjectGroupException(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-noexc")
	userID := tid("dg1-user-noexc")
	roleBearingGroupID := tid("dg1-group-noexc")
	governanceGroupID := tid("dg1-gov-group")

	createDelegateTestProject(t, s, projectID, "dg1-noexc-proj", tid("owner-noexc"))

	// User has NO project permissions.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dg1-noexc@test.com", DisplayName: "NoExc", Role: "member", Status: "active",
	}))

	// User owns and is member of a governance-only group (no bindings).
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: governanceGroupID, Slug: "dg1-gov-group", Name: "DG1 Governance Group",
		OwnerID: userID,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    governanceGroupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleOwner,
	}))

	// Create a separate role-bearing group (user is NOT a member).
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: roleBearingGroupID, Slug: "dg1-group-noexc", Name: "DG1 Role-Bearing Group",
	}))

	// Bind project-admin to the role-bearing group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      roleBearingGroupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(userID, "dg1-noexc@test.com", "NoExc", "member", "api")

	// The user has governance authority (group owner) on one group but
	// lacks resource authority (project-admin permissions) needed by the
	// role-bearing group. The delegation check must use role bindings,
	// not governance roles.
	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: roleBearingGroupID,
	})
	assert.False(t, decision.Allowed,
		"group governance role must NOT substitute for resource authority in delegation check")
}

// --- DG1.5: Cycle detection tests ---
// (These are integration-level cycle detection via the store's WouldCreateCycle.)

func TestCanDelegate_GroupMembership_CycleDetection_Self(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	groupID := tid("dg1-cycle-self")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-cycle-self", Name: "DG1 Self Cycle",
	}))

	wouldCycle, err := s.WouldCreateCycle(ctx, groupID, groupID)
	require.NoError(t, err)
	assert.True(t, wouldCycle, "adding a group to itself should detect a cycle")
}

func TestCanDelegate_GroupMembership_CycleDetection_Direct(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	groupA := tid("dg1-cycle-a")
	groupB := tid("dg1-cycle-b")

	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupA, Slug: "dg1-cycle-a", Name: "A"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupB, Slug: "dg1-cycle-b", Name: "B"}))

	// A contains B.
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupA, MemberType: store.GroupMemberTypeGroup, MemberID: groupB, Role: store.GroupMemberRoleMember,
	}))

	// B containing A would create a cycle.
	wouldCycle, err := s.WouldCreateCycle(ctx, groupB, groupA)
	require.NoError(t, err)
	assert.True(t, wouldCycle, "A→B and then B→A should detect a cycle")
}

func TestCanDelegate_GroupMembership_CycleDetection_Transitive(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	groupA := tid("dg1-tcycle-a")
	groupB := tid("dg1-tcycle-b")
	groupC := tid("dg1-tcycle-c")

	for _, g := range []struct{ id, slug string }{
		{groupA, "dg1-tcycle-a"},
		{groupB, "dg1-tcycle-b"},
		{groupC, "dg1-tcycle-c"},
	} {
		require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: g.id, Slug: g.slug, Name: g.slug}))
	}

	// A contains B, B contains C.
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupA, MemberType: store.GroupMemberTypeGroup, MemberID: groupB, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupB, MemberType: store.GroupMemberTypeGroup, MemberID: groupC, Role: store.GroupMemberRoleMember,
	}))

	// C containing A would create a cycle.
	wouldCycle, err := s.WouldCreateCycle(ctx, groupC, groupA)
	require.NoError(t, err)
	assert.True(t, wouldCycle, "A→B→C and then C→A should detect a transitive cycle")

	// A containing C is valid (C is already a descendant — this just adds
	// a second path but no cycle since we check child_groups).
	wouldCycle, err = s.WouldCreateCycle(ctx, groupA, groupC)
	require.NoError(t, err)
	assert.False(t, wouldCycle, "A already contains C transitively; adding A→C is not a cycle")
}

// --- DG1.6: Concurrent membership/role change safety ---

// TestCanDelegate_GroupMembership_ConcurrentRoleAdd verifies that when a role
// binding is added to a group between the time a member addition is checked
// and committed, the check is based on the bindings at check time. This test
// validates the sequential behavior — true concurrent protection requires
// transactional or revision-based checks which are documented as follow-on.
func TestCanDelegate_GroupMembership_ConcurrentRoleAdd(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-conc")
	userID := tid("dg1-user-conc")
	groupID := tid("dg1-group-conc")

	createDelegateTestProject(t, s, projectID, "dg1-conc-proj", tid("owner-conc"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dg1-conc@test.com", DisplayName: "Conc", Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-conc", Name: "DG1 Concurrent Group",
	}))

	user := NewAuthenticatedUser(userID, "dg1-conc@test.com", "Conc", "member", "api")

	// Step 1: Group has no bindings — delegation is allowed.
	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed, "no role bindings — should be allowed")

	// Step 2: Add a role binding to the group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Step 3: Same user, same group — now delegation is denied.
	decision = authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"after adding role binding to group, delegation should be denied for unprivileged user")
}

// --- DG1.7: Constraint-bearing group removal (AC1 not merged — documented) ---
// AC1 has not merged. Constraint-bearing group removal gating is documented
// as a follow-on. See deliverable 6 in the brief.

// --- DG1.8: Group resolution for users and agents is live ---

// TestCanDelegate_GroupMembership_LiveResolution verifies that group
// resolution is live — adding a member to a group immediately affects
// the next delegation check without restart.
func TestCanDelegate_GroupMembership_LiveResolution(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("dg1-proj-live")
	userID := tid("dg1-user-live")
	groupID := tid("dg1-group-live")

	createDelegateTestProject(t, s, projectID, "dg1-live-proj", tid("owner-live"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dg1-live@test.com", DisplayName: "Live", Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-group-live", Name: "DG1 Live Group",
	}))

	// Bind project-admin to the group.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(userID, "dg1-live@test.com", "Live", "member", "api")

	// Before: user has no project perms — denied.
	decision := authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed, "before group membership, user should be denied")

	// Add user to a group with project-admin for this project.
	adminGroupID := tid("dg1-admin-group-live")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: adminGroupID, Slug: "dg1-admin-group-live", Name: "DG1 Admin Group Live",
	}))
	rdAdmin, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdAdmin.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      adminGroupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    adminGroupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	}))

	// After: user now inherits project-admin via group — allowed.
	decision = authz.CanDelegate(ctx, user, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"after adding user to admin group, delegation should be allowed (live resolution)")
}

// --- DG1.9: GetParentGroups store method tests ---

func TestGetParentGroups_NoParents(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	groupID := tid("dg1-orphan")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "dg1-orphan", Name: "Orphan",
	}))

	parents, err := s.GetParentGroups(ctx, groupID)
	require.NoError(t, err)
	assert.Empty(t, parents, "group with no parents should return empty list")
}

func TestGetParentGroups_DirectParent(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	parentID := tid("dg1-dp-parent")
	childID := tid("dg1-dp-child")

	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: parentID, Slug: "dg1-dp-parent", Name: "Parent"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: childID, Slug: "dg1-dp-child", Name: "Child"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID, Role: store.GroupMemberRoleMember,
	}))

	parents, err := s.GetParentGroups(ctx, childID)
	require.NoError(t, err)
	assert.Len(t, parents, 1, "should return one parent")
	assert.Equal(t, parentID, parents[0])
}

func TestGetParentGroups_TransitiveParents(t *testing.T) {
	_, s := setupCanDelegateTest(t)
	ctx := context.Background()

	grandparentID := tid("dg1-tp-grandparent")
	parentID := tid("dg1-tp-parent")
	childID := tid("dg1-tp-child")

	for _, g := range []struct{ id, slug string }{
		{grandparentID, "dg1-tp-grandparent"},
		{parentID, "dg1-tp-parent"},
		{childID, "dg1-tp-child"},
	} {
		require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: g.id, Slug: g.slug, Name: g.slug}))
	}

	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: grandparentID, MemberType: store.GroupMemberTypeGroup, MemberID: parentID, Role: store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID, Role: store.GroupMemberRoleMember,
	}))

	parents, err := s.GetParentGroups(ctx, childID)
	require.NoError(t, err)
	assert.Len(t, parents, 2, "should return both parent and grandparent")
	parentSet := make(map[string]bool)
	for _, p := range parents {
		parentSet[p] = true
	}
	assert.True(t, parentSet[parentID], "should include direct parent")
	assert.True(t, parentSet[grandparentID], "should include grandparent")
}

// =============================================================================
// R2 REGRESSION: Delegation escalation window is closed
//
// This is the explicit regression test required by the Wave 1 XL review.
// Scenario: actor without permission X cannot obtain permission X by inserting
// themselves (or another principal) into a role-bearing group.
// =============================================================================

// TestCanDelegate_Regression_EscalationViaSelfInsert verifies that an actor
// who lacks permission X is blocked from inserting themselves into a group that
// carries permission X via a role binding. This closes the delegation escalation
// window identified in the Wave 1 XL review (R2).
func TestCanDelegate_Regression_EscalationViaSelfInsert(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("r2-proj-esc")
	attackerID := tid("r2-attacker")
	groupID := tid("r2-group-esc")

	// Set up project and the attacker user.
	createDelegateTestProject(t, s, projectID, "r2-escalation-proj", tid("r2-proj-creator"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: attackerID, Email: "attacker@test.com", DisplayName: "Attacker",
		Role: "member", Status: "active",
	}))

	// Give the attacker hub-member (minimal) authority — no project permissions.
	createTestUserWithRole(t, s, tid("r2-attacker-role"), "attacker-role@test.com",
		"member", store.SystemRoleHubMember)

	// Create a group and bind project-admin to it — this is the target.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r2-escalation-group", Name: "R2 Escalation Group",
	}))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	attacker := NewAuthenticatedUser(attackerID, "attacker@test.com", "Attacker", "member", "api")

	// Attacker tries to add themselves to the role-bearing group.
	decision := authz.CanDelegate(ctx, attacker, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})

	// MUST be denied: attacker does not hold project-admin permissions.
	assert.False(t, decision.Allowed,
		"REGRESSION: actor without project-admin must NOT be able to self-insert into a group bearing project-admin")
	assert.Contains(t, decision.Reason, "cannot delegate",
		"denial reason should mention delegation failure")
}

// TestCanDelegate_Regression_EscalationViaProxyInsert verifies that an actor
// who lacks permission X is also blocked from inserting ANOTHER principal (e.g.
// an agent) into a role-bearing group. The check must protect against escalation
// regardless of who the insertee is.
func TestCanDelegate_Regression_EscalationViaProxyInsert(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("r2-proj-proxy")
	actorID := tid("r2-actor-proxy")
	groupID := tid("r2-group-proxy")

	// Set up project and the unprivileged actor.
	createDelegateTestProject(t, s, projectID, "r2-proxy-proj", tid("r2-proxy-creator"))
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: actorID, Email: "proxy-actor@test.com", DisplayName: "ProxyActor",
		Role: "member", Status: "active",
	}))

	// Create a group bearing hub-admin at system scope.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r2-proxy-group", Name: "R2 Proxy Group",
	}))
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	actor := NewAuthenticatedUser(actorID, "proxy-actor@test.com", "ProxyActor", "member", "api")

	// Actor tries to insert another principal into the hub-admin group.
	// The CanDelegate check is caller-side: it doesn't matter who the insertee is.
	decision := authz.CanDelegate(ctx, actor, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})

	assert.False(t, decision.Allowed,
		"REGRESSION: actor without hub-admin must NOT be able to insert any principal into a group bearing hub-admin")
	assert.Contains(t, decision.Reason, "cannot delegate")
}

// TestCanDelegate_Regression_AgentCallerEscalation verifies that an agent caller
// (which was previously exempted from delegation checks) is now subject to the
// same escalation guard. An agent without permission X cannot insert into a
// role-bearing group.
func TestCanDelegate_Regression_AgentCallerEscalation(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("r2-proj-agent")
	agentID := tid("r2-agent-esc")
	groupID := tid("r2-group-agent")

	// Create project and agent.
	createDelegateTestProject(t, s, projectID, "r2-agent-proj", tid("r2-agent-creator"))

	project, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)

	err = s.CreateAgent(ctx, &store.Agent{
		ID:        agentID,
		Name:      "r2-esc-agent",
		Slug:      "r2-esc-agent",
		ProjectID: project.ID,
	})
	require.NoError(t, err)

	// Create group bearing project-admin.
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r2-agent-group", Name: "R2 Agent Group",
	}))
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Agent caller with no special authority.
	agentCaller := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: agentID}, ProjectID: projectID}}

	// Agent tries to add a member to the role-bearing group.
	decision := authz.CanDelegate(ctx, agentCaller, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})

	assert.False(t, decision.Allowed,
		"REGRESSION: agent caller without project-admin must NOT bypass delegation check for role-bearing group")
}
