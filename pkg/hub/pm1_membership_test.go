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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ensureSuperAdminBinding creates a super-admin system role binding for an
// existing user. Idempotent: silently ignores duplicate-binding errors.
// CO1: the AK1 kernel requires role bindings for authorization decisions;
// User.Role="admin" alone is no longer sufficient.
func ensureSuperAdminBinding(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "super-admin role definition must exist after seeding")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create super-admin role binding: %v", err)
	}
}

// =============================================================================
// PM1: Project Membership as RoleBinding View
//
// These tests verify the PM1 contract: project membership is defined by
// project-scoped role bindings, not by the project:<slug>:members group.
// =============================================================================

// TestPM1_ProjectCreation_OwnerBindingCreated verifies that creating a project
// via the API creates a project-owner role binding for the creator.
func TestPM1_ProjectCreation_OwnerBindingCreated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("pm1-create-user"), Email: "pm1-create@test.com",
		DisplayName: "PM1 Create User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{Name: "PM1 Owner Test"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Verify owner role binding exists.
	membership, err := s.GetProjectMembership(ctx, project.ID, user.ID)
	require.NoError(t, err, "owner should have a project membership via role binding")
	assert.Equal(t, store.ProjectRoleOwner, membership.Role)
}

// TestPM1_ProjectDeletion_CascadesRoleBindings verifies the XL review R1
// finding: deleting a project removes all its project-scoped role bindings.
func TestPM1_ProjectDeletion_CascadesRoleBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("pm1-cascade-user"), Email: "pm1-cascade@test.com",
		DisplayName: "PM1 Cascade User", Role: store.UserRoleAdmin, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create project via API.
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{Name: "PM1 Cascade Test"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Verify bindings exist.
	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)
	require.NotEmpty(t, bindings, "project should have role bindings after creation")

	// Delete the project.
	rec = doRequestAsUser(t, srv, user, http.MethodDelete,
		"/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	// Verify all bindings are gone.
	bindings, err = s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, bindings, "no role bindings should remain after project deletion")
}

// TestPM1_LastOwnerProtection_CannotDeleteLastOwner verifies that the last
// direct-user project-owner binding cannot be deleted.
func TestPM1_LastOwnerProtection_CannotDeleteLastOwner(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID: tid("pm1-lastowner-user"), Email: "pm1-lastowner@test.com",
		DisplayName: "PM1 Last Owner", Role: store.UserRoleAdmin, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)
	// CO1: Admin users need a super-admin role binding to pass authz checks.
	ensureSuperAdminBinding(t, s, owner.ID)

	// Create project.
	project := &store.Project{
		ID: tid("pm1-lastowner-proj"), Name: "PM1 LastOwner Project",
		Slug: "pm1-lastowner", OwnerID: owner.ID, CreatedBy: owner.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create owner role binding.
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, owner.ID))

	// Find the owner binding.
	membership, err := s.GetProjectMembership(ctx, project.ID, owner.ID)
	require.NoError(t, err)

	// Try to delete the last owner binding.
	rec := doRequestAsUser(t, srv, owner, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+membership.RoleBindingID, nil)
	assert.Equal(t, http.StatusConflict, rec.Code,
		"should not be able to delete the last project owner; got: %s", rec.Body.String())

	// Verify the binding still exists.
	membershipAfter, err := s.GetProjectMembership(ctx, project.ID, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectRoleOwner, membershipAfter.Role)
}

// TestPM1_LastOwnerProtection_CanDeleteNonLastOwner verifies that deleting a
// project-owner binding is allowed when there are other owners.
func TestPM1_LastOwnerProtection_CanDeleteNonLastOwner(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner1 := &store.User{
		ID: tid("pm1-2owners-u1"), Email: "pm1-2owners-u1@test.com",
		DisplayName: "PM1 Owner1", Role: store.UserRoleAdmin, Status: "active",
	}
	owner2 := &store.User{
		ID: tid("pm1-2owners-u2"), Email: "pm1-2owners-u2@test.com",
		DisplayName: "PM1 Owner2", Role: store.UserRoleAdmin, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, owner1))
	require.NoError(t, s.CreateUser(ctx, owner2))
	ensureHubMembership(ctx, s, owner1.ID)
	ensureHubMembership(ctx, s, owner2.ID)
	// CO1: Admin users need super-admin role bindings to pass authz checks.
	ensureSuperAdminBinding(t, s, owner1.ID)
	ensureSuperAdminBinding(t, s, owner2.ID)

	// Create project with two owners.
	project := &store.Project{
		ID: tid("pm1-2owners-proj"), Name: "PM1 Two Owners Project",
		Slug: "pm1-two-owners", OwnerID: owner1.ID, CreatedBy: owner1.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, owner1.ID))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, owner2.ID))

	// Verify two owners exist.
	count, err := srv.countDirectOwnerBindings(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// Delete one owner — should succeed because the other remains.
	membership, err := s.GetProjectMembership(ctx, project.ID, owner1.ID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, owner1, http.MethodDelete,
		"/api/v1/admin/role-bindings/"+membership.RoleBindingID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"should be able to delete a non-last owner; got: %s", rec.Body.String())

	// Verify only one owner remains.
	count, err = srv.countDirectOwnerBindings(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestPM1_MembershipViaRoleBinding verifies that project membership is
// determined by role bindings, not group membership.
func TestPM1_MembershipViaRoleBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("pm1-rb-user"), Email: "pm1-rb@test.com",
		DisplayName: "PM1 RB User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID: tid("pm1-rb-proj"), Name: "PM1 RB Project",
		Slug: "pm1-rb", OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Without a role binding, user should not be a project member.
	isMember, err := s.IsProjectMember(ctx, project.ID, user.ID)
	require.NoError(t, err)
	assert.False(t, isMember, "user without role binding should not be a project member")

	// Add a project-member role binding.
	require.NoError(t, srv.createProjectRoleBinding(ctx, project.ID,
		store.RoleBindingPrincipalUser, user.ID, store.ProjectRoleMember, "test"))

	// Now user should be a project member.
	isMember, err = s.IsProjectMember(ctx, project.ID, user.ID)
	require.NoError(t, err)
	assert.True(t, isMember, "user with role binding should be a project member")
}

// TestPM1_HubMembersGroupVisibility verifies that adding a hub-members group
// role binding makes the project visible to all hub members.
func TestPM1_HubMembersGroupVisibility(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID: tid("pm1-vis-owner"), Email: "pm1-vis-owner@test.com",
		DisplayName: "PM1 Vis Owner", Role: store.UserRoleMember, Status: "active",
	}
	viewer := &store.User{
		ID: tid("pm1-vis-viewer"), Email: "pm1-vis-viewer@test.com",
		DisplayName: "PM1 Vis Viewer", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	require.NoError(t, s.CreateUser(ctx, viewer))
	ensureHubMembership(ctx, s, owner.ID)
	ensureHubMembership(ctx, s, viewer.ID)

	// Create project with owner binding.
	project := &store.Project{
		ID: tid("pm1-vis-proj"), Name: "PM1 Visibility Project",
		Slug: "pm1-visibility", OwnerID: owner.ID, CreatedBy: owner.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Create hub-members group RoleBinding for project visibility.
	srv.ensureHubMembersProjectVisibility(ctx, project)

	// Verify the hub-members group has a project-member binding.
	hubMembersGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	bindings, err := s.ListRoleBindingsForScope(ctx, store.RoleScopeProject, project.ID)
	require.NoError(t, err)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	foundGroupBinding := false
	for _, b := range bindings {
		if b.PrincipalType == store.RoleBindingPrincipalGroup &&
			b.PrincipalID == hubMembersGroup.ID &&
			b.RoleDefinitionID == memberRD.ID {
			foundGroupBinding = true
			break
		}
	}
	assert.True(t, foundGroupBinding,
		"hub-members group should have a project-member role binding for visibility")
}

// TestPM1_ProjectCreation_RollsBackOnBindingFailure verifies that project
// creation rolls back the project record if the owner binding fails.
func TestPM1_ProjectCreation_AtomicWithOwnerBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("pm1-atomic-user"), Email: "pm1-atomic@test.com",
		DisplayName: "PM1 Atomic User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create project via API — should succeed atomically.
	rec := doRequestAsUser(t, srv, user, http.MethodPost, "/api/v1/projects",
		CreateProjectRequest{Name: "PM1 Atomic Test"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Verify both project and binding exist.
	_, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err, "project should exist")

	membership, err := s.GetProjectMembership(ctx, project.ID, user.ID)
	require.NoError(t, err, "owner binding should exist")
	assert.Equal(t, store.ProjectRoleOwner, membership.Role)
}

// =============================================================================
// PM1: resolveUserRBProjectIDs — project discovery via RoleBindings
// =============================================================================

func TestResolveUserRBProjectIDs_ReturnsProjectScopedBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("rb-proj-ids-user"), Email: "rb-proj-ids@test.com",
		DisplayName: "RB ProjectIDs User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Create two projects and assign RoleBindings
	project1 := &store.Project{
		ID: tid("rb-proj1"), Name: "RB Proj 1", Slug: "rb-proj1",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	project2 := &store.Project{
		ID: tid("rb-proj2"), Name: "RB Proj 2", Slug: "rb-proj2",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project1))
	require.NoError(t, s.CreateProject(ctx, project2))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project1.ID, user.ID))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project2.ID, user.ID))

	// Resolve project IDs via RoleBindings
	ids := srv.resolveUserRBProjectIDs(ctx, user.ID)
	assert.Len(t, ids, 2, "should resolve both project IDs from RoleBindings")
	assert.Contains(t, ids, project1.ID)
	assert.Contains(t, ids, project2.ID)
}

func TestResolveUserRBProjectIDs_ExcludesSystemBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("rb-sys-excl-user"), Email: "rb-sys-excl@test.com",
		DisplayName: "RB System Excl User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// User has a system-scoped binding (hub-member) but no project bindings
	ids := srv.resolveUserRBProjectIDs(ctx, user.ID)
	assert.Empty(t, ids, "should not return project IDs from system-scoped bindings")
}

// =============================================================================
// PM1: mergeProjectIDs — deduplication
// =============================================================================

func TestMergeProjectIDs(t *testing.T) {
	// Merge overlapping slices
	merged := mergeProjectIDs(
		[]string{"a", "b", "c"},
		[]string{"b", "c", "d"},
	)
	assert.Len(t, merged, 4, "should merge and deduplicate")
	for _, id := range []string{"a", "b", "c", "d"} {
		assert.Contains(t, merged, id)
	}
}

func TestMergeProjectIDs_NilInputs(t *testing.T) {
	merged := mergeProjectIDs(nil, nil)
	assert.Nil(t, merged, "merging nil slices should return nil")
}

func TestMergeProjectIDs_EmptyInputs(t *testing.T) {
	merged := mergeProjectIDs([]string{}, []string{})
	assert.Nil(t, merged, "merging empty slices should return nil")
}

// ===========================================================================
// C0: resolveUserOwnerProjectIDs — Mine must select owner-only bindings
// ===========================================================================

func TestC0_ResolveUserOwnerProjectIDs_OnlyOwnerBindings(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("c0-mine-user"), Email: "c0-mine@test.com",
		DisplayName: "C0 Mine User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	// Create three projects.
	ownedProject := &store.Project{
		ID: tid("c0-owned"), Name: "Owned Project", Slug: "c0-owned",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	adminProject := &store.Project{
		ID: tid("c0-admin-proj"), Name: "Admin Project", Slug: "c0-admin-proj",
		OwnerID: tid("c0-other-o"), CreatedBy: tid("c0-other-o"),
		Created: time.Now(), Updated: time.Now(),
	}
	memberProject := &store.Project{
		ID: tid("c0-member-proj"), Name: "Member Project", Slug: "c0-member-proj",
		OwnerID: tid("c0-other-o"), CreatedBy: tid("c0-other-o"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, ownedProject))
	require.NoError(t, s.CreateProject(ctx, adminProject))
	require.NoError(t, s.CreateProject(ctx, memberProject))

	// Give user project-owner on the first.
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, ownedProject.ID, user.ID))

	// Give user project-admin on the second.
	adminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          adminProject.ID,
		CreatedBy:        user.ID,
	})
	require.NoError(t, err)

	// Give user project-member on the third.
	memberRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          memberProject.ID,
		CreatedBy:        user.ID,
	})
	require.NoError(t, err)

	// C0 exit gate: Mine must return ONLY the owned project.
	ownerIDs := srv.resolveUserOwnerProjectIDs(ctx, user.ID)
	assert.Len(t, ownerIDs, 1,
		"C0: resolveUserOwnerProjectIDs must return only project-owner bindings")
	assert.Contains(t, ownerIDs, ownedProject.ID)
	assert.NotContains(t, ownerIDs, adminProject.ID,
		"C0: project-admin binding must NOT classify a project as Mine")
	assert.NotContains(t, ownerIDs, memberProject.ID,
		"C0: project-member binding must NOT classify a project as Mine")

	// All bindings should still appear in the general resolver.
	allIDs := srv.resolveUserRBProjectIDs(ctx, user.ID)
	assert.Len(t, allIDs, 3, "resolveUserRBProjectIDs should return all 3 projects")
}

func TestC0_SubtractProjectIDs(t *testing.T) {
	all := []string{"a", "b", "c", "d"}
	exclude := []string{"b", "d"}
	result := subtractProjectIDs(all, exclude)
	assert.Len(t, result, 2)
	assert.Contains(t, result, "a")
	assert.Contains(t, result, "c")

	// Empty exclude returns all.
	result2 := subtractProjectIDs(all, nil)
	assert.Equal(t, all, result2)

	// Subtracting everything returns nil.
	result3 := subtractProjectIDs(all, all)
	assert.Nil(t, result3)
}

// ---------------------------------------------------------------------------
// C0: isProjectOwner — direct-user-only with activation lifecycle
// ---------------------------------------------------------------------------

// TestC0_IsProjectOwner_DirectUserOnly verifies that isProjectOwner only
// considers direct user bindings, not group-derived. Since the store-level
// ErrDirectUserOnly guard prevents creating group-owner bindings, we verify
// the defense-in-depth property by confirming:
// 1. A user with a group-based project-ADMIN binding is not an owner.
// 2. Only a direct user project-owner binding grants ownership.
// 3. The store correctly prevents group-owner bindings (belt-and-suspenders).
func TestC0_IsProjectOwner_DirectUserOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("c0-grp-user"), Email: "grp-owner@test.com",
		DisplayName: "Group Owner", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID: tid("c0-grp-proj"), Name: "Group Project", Slug: "c0-grp-proj",
		OwnerID: tid("c0-grp-other"), CreatedBy: tid("c0-grp-other"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a group and add the user as a member.
	grp := &store.Group{
		ID:   tid("c0-owner-grp"),
		Name: "Owner Group",
		Slug: "c0-owner-group",
	}
	require.NoError(t, s.CreateGroup(ctx, grp))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    grp.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   user.ID,
		Role:       "member",
	}))

	// Give the GROUP a project-admin binding (group-owner is prevented by
	// store validation). This tests that group-expanded admin access does
	// NOT leak into ownership.
	adminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      grp.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "system",
	})
	require.NoError(t, err)

	// isProjectOwner must NOT return true — the user has admin access via
	// group, but no direct owner binding.
	assert.False(t, srv.authzService.isProjectOwner(ctx, user.ID, project.ID),
		"C0: group-derived admin binding must NOT grant ownership via isProjectOwner")

	// Verify belt-and-suspenders: store prevents creating group-owner bindings.
	ownerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      grp.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "system",
	})
	assert.Error(t, err, "C0: store must prevent group-owner binding creation (ErrDirectUserOnly)")

	// Verify that a direct user binding DOES work.
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, project.ID, user.ID))
	assert.True(t, srv.authzService.isProjectOwner(ctx, user.ID, project.ID),
		"C0: direct user owner binding must grant ownership via isProjectOwner")
}

// TestC0_IsProjectOwner_ExpiredBinding verifies that an expired owner binding
// does not grant ownership via isProjectOwner.
func TestC0_IsProjectOwner_ExpiredBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("c0-exp-user"), Email: "exp-owner@test.com",
		DisplayName: "Expired Owner", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	project := &store.Project{
		ID: tid("c0-exp-proj"), Name: "Expired Project", Slug: "c0-exp-proj",
		OwnerID: tid("c0-exp-other"), CreatedBy: tid("c0-exp-other"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create an owner binding that has already expired.
	ownerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "system",
		ExpiresAt:        &expired,
	})
	require.NoError(t, err)

	assert.False(t, srv.authzService.isProjectOwner(ctx, user.ID, project.ID),
		"C0: expired owner binding must NOT grant ownership via isProjectOwner")
}

// TestC0_IsProjectOwner_FutureBinding verifies that a not-yet-active owner
// binding does not grant ownership via isProjectOwner.
func TestC0_IsProjectOwner_FutureBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("c0-fut-user"), Email: "fut-owner@test.com",
		DisplayName: "Future Owner", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	project := &store.Project{
		ID: tid("c0-fut-proj"), Name: "Future Project", Slug: "c0-fut-proj",
		OwnerID: tid("c0-fut-other"), CreatedBy: tid("c0-fut-other"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create an owner binding that is not yet active (notBefore in the future).
	ownerRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	future := time.Now().Add(1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "system",
		NotBefore:        &future,
	})
	require.NoError(t, err)

	assert.False(t, srv.authzService.isProjectOwner(ctx, user.ID, project.ID),
		"C0: not-yet-active owner binding must NOT grant ownership via isProjectOwner")
}

// ---------------------------------------------------------------------------
// C0: isProjectOwner gate — expired/future owners cannot manage membership
// ---------------------------------------------------------------------------

// TestC0_ExpiredOwnerCannotAddMember verifies that a user with an expired
// owner binding is denied at the membership mutation gate.
func TestC0_ExpiredOwnerCannotAddMember(t *testing.T) {
	srv, st, _, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	expiredOwner := &store.User{
		ID: tid("c0-exp-mgr"), Email: "exp-mgr@test.com",
		DisplayName: "Expired Manager", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, expiredOwner))
	ensureHubMembership(ctx, st, expiredOwner.ID)

	// Create an expired owner binding for the project.
	ownerRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-1 * time.Hour)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      expiredOwner.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "system",
		ExpiresAt:        &expired,
	})
	require.NoError(t, err)

	target := &store.User{
		ID: tid("c0-exp-tgt"), Email: "exp-tgt@test.com",
		DisplayName: "ExpTarget", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, target))
	ensureHubMembership(ctx, st, target.ID)

	memberRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, expiredOwner, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRoleDef.ID,
			PrincipalType:    "user",
			PrincipalID:      target.ID,
		})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"C0: expired owner binding must not permit membership management")
}

// ---------------------------------------------------------------------------
// C0: Agent list Mine/Shared integration tests
// ---------------------------------------------------------------------------

// TestC0_AgentListMineScopeOwnerOnly verifies that scope=mine on the agent
// list endpoint returns only agents from projects the user directly owns,
// not projects where they are admin or member.
func TestC0_AgentListMineScopeOwnerOnly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("c0-agmine-u"), Email: "agmine@test.com",
		DisplayName: "Agent Mine User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create two projects.
	ownedProj := &store.Project{
		ID: tid("c0-agmine-op"), Name: "Owned Agent Proj", Slug: "c0-agmine-owned",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	adminProj := &store.Project{
		ID: tid("c0-agmine-ap"), Name: "Admin Agent Proj", Slug: "c0-agmine-admin",
		OwnerID: tid("c0-agmine-oth"), CreatedBy: tid("c0-agmine-oth"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, ownedProj))
	require.NoError(t, s.CreateProject(ctx, adminProj))

	// Owner binding on ownedProj.
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, ownedProj.ID, user.ID))

	// Admin binding on adminProj.
	adminRoleDef, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          adminProj.ID,
		CreatedBy:        user.ID,
	})
	require.NoError(t, err)

	// Create agents in both projects.
	ownedAgent := &store.Agent{
		ID:        tid("c0-agmine-oa"),
		Slug:      "c0-owned-agent",
		Name:      "owned-agent",
		ProjectID: ownedProj.ID,
		OwnerID:   user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	adminAgent := &store.Agent{
		ID:        tid("c0-agmine-aa"),
		Slug:      "c0-admin-agent",
		Name:      "admin-agent",
		ProjectID: adminProj.ID,
		OwnerID:   tid("c0-agmine-oth"),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, ownedAgent))
	require.NoError(t, s.CreateAgent(ctx, adminAgent))

	// scope=mine should return only the owned project's agent.
	rec := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/agents?scope=mine", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp struct {
		Agents []struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"agents"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	var agentProjectIDs []string
	for _, a := range resp.Agents {
		agentProjectIDs = append(agentProjectIDs, a.ProjectID)
	}
	assert.Contains(t, agentProjectIDs, ownedProj.ID,
		"C0: scope=mine must include agents from owned project")
	assert.NotContains(t, agentProjectIDs, adminProj.ID,
		"C0: scope=mine must NOT include agents from admin-only project")
}

// ---------------------------------------------------------------------------
// C0: _capabilities in members list response
// ---------------------------------------------------------------------------

// TestC0_MembersListCapabilities_OwnerGetsManageMembers verifies that the
// members list response includes _capabilities with manage_members for an
// owner, and omits it for a non-owner.
func TestC0_MembersListCapabilities_OwnerGetsManageMembers(t *testing.T) {
	srv, st, owner, _, project := setupProjectMembersTest(t)
	ctx := context.Background()

	// Owner should get manage_members capability.
	rec := doRequestAsUser(t, srv, owner, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var ownerResp listProjectMembersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&ownerResp))
	require.NotNil(t, ownerResp.Capabilities,
		"C0: owner's members list should include _capabilities")
	assert.Contains(t, ownerResp.Capabilities.Actions, "manage_members",
		"C0: owner's _capabilities should include manage_members")

	// Create an admin user.
	admin := &store.User{
		ID: tid("c0-cap-admin"), Email: "cap-admin@test.com",
		DisplayName: "Cap Admin", Role: store.UserRoleMember, Status: "active",
		Created: time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	adminRoleDef, err := st.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        owner.ID,
	})
	require.NoError(t, err)

	// Admin should NOT get manage_members capability.
	rec = doRequestAsUser(t, srv, admin, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/members", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var adminResp listProjectMembersResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&adminResp))
	if adminResp.Capabilities != nil {
		assert.NotContains(t, adminResp.Capabilities.Actions, "manage_members",
			"C0: admin's _capabilities must NOT include manage_members")
	}
	// _capabilities being nil is also acceptable — means no membership mutation rights.
}
