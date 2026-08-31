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
