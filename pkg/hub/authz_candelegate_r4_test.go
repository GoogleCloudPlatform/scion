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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// R4: Credential caveats intersected in delegation check
//
// (a) A project-scoped UAT must not delegate system-scoped group authority.
// (b) An actor whose credential caveats restrict permissions must not
//     delegate permissions beyond those caveats.
// =============================================================================

// --- R4 gap (a): UAT scope bypass on group membership ---

// TestCanDelegate_GroupMembership_ScopedUAT_SystemBinding_Denied verifies
// that a user with a project-scoped UAT cannot delegate group membership
// when the target group has system-scoped role bindings.
func TestCanDelegate_GroupMembership_ScopedUAT_SystemBinding_Denied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	// Create a super-admin user — they have all permissions via full session.
	adminID := tid("r4-admin-1")
	createTestUserWithRole(t, s, adminID, "r4-admin@test.com", "admin", store.SystemRoleSuperAdmin)

	groupID := tid("r4-group-sys-1")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r4-group-sys-1", Name: "R4 System Group",
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

	// Full-session admin: should succeed.
	fullAdmin := NewAuthenticatedUser(adminID, "r4-admin@test.com", "Admin", "admin", "api")
	decision := authz.CanDelegate(ctx, fullAdmin, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"full-session super-admin should be able to delegate system-scoped group")

	// Project-scoped UAT: should be denied — cannot delegate system-scoped authority.
	scopedAdmin := NewScopedUserIdentity(fullAdmin, tid("r4-proj"), []string{"project:read"})
	decision = authz.CanDelegate(ctx, scopedAdmin, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"project-scoped UAT must NOT delegate system-scoped group authority")
	assert.Contains(t, decision.Reason, "scoped credential cannot delegate system-scoped group authority")
}

// TestCanDelegate_GroupMembership_ScopedUAT_ProjectBinding_Allowed verifies
// that a user with a project-scoped UAT CAN delegate group membership when
// the target group has only project-scoped bindings within the UAT's project
// and the UAT's scopes cover the delegated permissions.
func TestCanDelegate_GroupMembership_ScopedUAT_ProjectBinding_Allowed(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("r4-proj-2")
	ownerID := tid("r4-owner-2")

	createDelegateTestProject(t, s, projectID, "r4-proj-2", ownerID)
	createTestUserWithProjectRole(t, s, ownerID, "r4-owner@test.com", projectID, store.ProjectRoleOwner)

	groupID := tid("r4-group-proj-1")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r4-group-proj-1", Name: "R4 Project Group",
	}))

	// Create a minimal custom role with just agent.read permission, then
	// bind it to the group at project scope. This keeps the scope set small
	// enough to cover in the UAT scopes.
	customRD, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "r4-test-role",
		Description: "Minimal role for R4 test",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.read"},
	})
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Full-session owner can delegate.
	owner := NewAuthenticatedUser(ownerID, "r4-owner@test.com", "Owner", "member", "api")

	// Project-scoped UAT whose scope covers agent:read — the one permission
	// in the target group's role binding.
	scopedOwner := NewScopedUserIdentity(owner, projectID, []string{"agent:read"})
	decision := authz.CanDelegate(ctx, scopedOwner, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"project-scoped UAT with matching scopes should delegate project-scoped group membership")
}

// --- R4 gap (b): Credential caveats in actorHoldsAllPermissions ---

// TestCanDelegate_CredentialCaveats_RestrictPermissions verifies that
// actorHoldsAllPermissions intersects credential caveats so that an actor
// whose effective permissions are restricted by their UAT cannot delegate
// permissions outside the UAT's scope.
func TestCanDelegate_CredentialCaveats_RestrictPermissions(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectID := tid("r4-proj-3")
	adminID := tid("r4-admin-3")

	createDelegateTestProject(t, s, projectID, "r4-proj-3", adminID)
	createTestUserWithProjectRole(t, s, adminID, "r4-admin3@test.com", projectID, store.ProjectRoleAdmin)

	// Full session: admin can delegate agent.create at project scope.
	fullAdmin := NewAuthenticatedUser(adminID, "r4-admin3@test.com", "Admin", "member", "api")
	decision := authz.CanDelegate(ctx, fullAdmin, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.create"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         projectID,
	})
	assert.True(t, decision.Allowed,
		"full-session admin should be able to delegate agent.create")

	// Scoped UAT with only "project:read" scope — should NOT be able to
	// delegate agent.create because the credential caveats exclude it.
	scopedAdmin := NewScopedUserIdentity(fullAdmin, projectID, []string{"project:read"})
	decision = authz.CanDelegate(ctx, scopedAdmin, GrantDescriptor{
		Type:            GrantTypeRoleBinding,
		RolePermissions: []string{"agent.create"},
		ScopeType:       store.RoleScopeProject,
		ScopeID:         projectID,
	})
	assert.False(t, decision.Allowed,
		"scoped UAT without agent:create scope must NOT delegate agent.create")
	assert.Contains(t, decision.Reason, "actor lacks permission for delegation")
}

// TestCanDelegate_FullCredentials_SystemGroup_Allowed verifies that
// an actor with full (unscoped) credentials CAN delegate system-scoped
// group membership.
func TestCanDelegate_FullCredentials_SystemGroup_Allowed(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	adminID := tid("r4-admin-4")
	createTestUserWithRole(t, s, adminID, "r4-admin4@test.com", "admin", store.SystemRoleSuperAdmin)

	groupID := tid("r4-group-sys-2")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r4-group-sys-2", Name: "R4 System Group 2",
	}))

	// Bind hub-member role at system scope.
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Full-session super-admin can delegate system-scoped group authority.
	admin := NewAuthenticatedUser(adminID, "r4-admin4@test.com", "Admin", "admin", "api")
	decision := authz.CanDelegate(ctx, admin, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"full-session super-admin should delegate system-scoped group membership")
}

// --- R-3: Cross-project UAT group delegation gap ---

// TestCanDelegate_GroupMembership_ScopedUAT_CrossProject_Denied verifies that
// a project-scoped UAT cannot delegate group membership when the group carries
// role bindings for a DIFFERENT project.
//
// R-3 fix: previously, the group-membership GrantDescriptor had no ScopeType,
// so enforceUATDelegation no-oped, and intersectCredentialCaveats filtered by
// permission ID but not by project. A UAT scoped to project A could delegate
// a group carrying project-B-scoped bindings.
func TestCanDelegate_GroupMembership_ScopedUAT_CrossProject_Denied(t *testing.T) {
	authz, s := setupCanDelegateTest(t)
	ctx := context.Background()

	projectA := tid("r3-proj-a")
	projectB := tid("r3-proj-b")
	ownerID := tid("r3-owner")

	createDelegateTestProject(t, s, projectA, "r3-proj-a", ownerID)
	createDelegateTestProject(t, s, projectB, "r3-proj-b", ownerID)
	createTestUserWithProjectRole(t, s, ownerID, "r3-owner@test.com", projectA, store.ProjectRoleOwner)
	// Also give owner permissions in project B.
	rdB, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdB.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ownerID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectB,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create a group with a role binding scoped to project B.
	groupID := tid("r3-group-cross")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "r3-group-cross", Name: "R3 Cross Group",
	}))

	customRD, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "r3-test-role",
		Description: "Minimal role for R3 test",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.read"},
	})
	require.NoError(t, err)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectB, // binding is for project B
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Full-session owner can delegate (positive control).
	owner := NewAuthenticatedUser(ownerID, "r3-owner@test.com", "Owner", "member", "api")
	decision := authz.CanDelegate(ctx, owner, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.True(t, decision.Allowed,
		"full-session owner should delegate cross-project group membership")

	// UAT scoped to project A must NOT delegate group authority for project B.
	scopedOwner := NewScopedUserIdentity(owner, projectA, []string{"agent:read"})
	decision = authz.CanDelegate(ctx, scopedOwner, GrantDescriptor{
		Type:    GrantTypeGroupMembership,
		GroupID: groupID,
	})
	assert.False(t, decision.Allowed,
		"R-3: project-A-scoped UAT must NOT delegate group authority in project B")
	assert.Contains(t, decision.Reason, "different project",
		"denial reason should mention cross-project")
}
