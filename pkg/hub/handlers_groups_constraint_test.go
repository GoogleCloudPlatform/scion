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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// R5: Constraint-coverage gate on group removal/deletion
//
// Groups that participate in an AccessConstraint subject selector must not
// have members removed or be deleted without access_constraint.admin. These
// tests verify the handler-level gate added to removeGroupMember and
// deleteGroup.
// =============================================================================

// helper: create an AccessConstraint with a group_closure subject targeting groupID.
func createGroupClosureConstraint(t *testing.T, s store.Store, groupID string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "test-constraint-" + groupID,
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     strPtr(groupID),
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test constraint for group closure",
		CreatedBy:          "test",
	})
	require.NoError(t, err)
}

// helper: create an AccessConstraint with a principal subject of type "group".
func createGroupPrincipalConstraint(t *testing.T, s store.Store, groupID string) {
	t.Helper()
	ctx := context.Background()
	principalType := "group"
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "test-principal-constraint-" + groupID,
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &principalType,
		SubjectPrincipalID:   strPtr(groupID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test constraint for group principal",
		CreatedBy:            "test",
	})
	require.NoError(t, err)
}

// --- (a) Removal from a constraint-bearing group denied without admin ---

func TestRemoveGroupMember_ConstraintBearing_DeniedWithoutAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user who owns the group.
	memberUser := &store.User{
		ID: tid("r5-member-1"), Email: "r5-member@test.com",
		DisplayName: "R5 Member", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberUser))

	// Create group owned by this user.
	group := &store.Group{
		ID: tid("r5-group-1"), Slug: "r5-group-1", Name: "R5 Group 1",
		OwnerID: memberUser.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	// Add a member to remove later.
	targetUser := &store.User{
		ID: tid("r5-target-1"), Email: "r5-target@test.com",
		DisplayName: "R5 Target", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, targetUser))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberType: store.GroupMemberTypeUser,
		MemberID: targetUser.ID, Role: store.GroupMemberRoleMember,
	}))

	// Make the group constraint-bearing.
	createGroupClosureConstraint(t, s, group.ID)

	// Attempt removal as non-admin user — should be denied.
	rec := doRequestAsUser(t, srv, memberUser, http.MethodDelete,
		"/api/v1/groups/"+group.ID+"/members/user/"+targetUser.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"removing member from constraint-bearing group without admin should be forbidden")
	assert.Contains(t, rec.Body.String(), "access_constraint.admin")
}

// --- (b) Removal from a non-constraint group allowed ---

func TestRemoveGroupMember_NonConstraint_Allowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create group with dev user as owner (dev user has super-admin).
	group := &store.Group{
		ID: tid("r5-group-2"), Slug: "r5-group-2", Name: "R5 Group 2",
		OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	// Add a member to remove.
	targetUser := &store.User{
		ID: tid("r5-target-2"), Email: "r5-target2@test.com",
		DisplayName: "R5 Target 2", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, targetUser))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberType: store.GroupMemberTypeUser,
		MemberID: targetUser.ID, Role: store.GroupMemberRoleMember,
	}))

	// No constraint on this group — removal should succeed.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/groups/"+group.ID+"/members/user/"+targetUser.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"removing member from non-constraint group should succeed")
}

// --- (c) Deletion of a constraint-bearing group denied without admin ---

func TestDeleteGroup_ConstraintBearing_DeniedWithoutAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user.
	memberUser := &store.User{
		ID: tid("r5-member-3"), Email: "r5-member3@test.com",
		DisplayName: "R5 Member 3", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, memberUser))

	// Create group owned by this user.
	group := &store.Group{
		ID: tid("r5-group-3"), Slug: "r5-group-3", Name: "R5 Group 3",
		OwnerID: memberUser.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	// Make the group constraint-bearing (via principal subject type "group").
	createGroupPrincipalConstraint(t, s, group.ID)

	// Attempt deletion as non-admin user — should be denied.
	rec := doRequestAsUser(t, srv, memberUser, http.MethodDelete,
		"/api/v1/groups/"+group.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"deleting constraint-bearing group without admin should be forbidden")
	assert.Contains(t, rec.Body.String(), "access_constraint.admin")
}

// --- (d) Deletion of a non-constraint group allowed ---

func TestDeleteGroup_NonConstraint_Allowed(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create group with dev user as owner.
	group := &store.Group{
		ID: tid("r5-group-4"), Slug: "r5-group-4", Name: "R5 Group 4",
		OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	// No constraint — deletion should succeed.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/groups/"+group.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"deleting non-constraint group should succeed")
}

// --- Admin CAN remove/delete constraint-bearing groups ---

func TestRemoveGroupMember_ConstraintBearing_AllowedWithAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Dev user has super-admin (which includes access_constraint.admin).
	group := &store.Group{
		ID: tid("r5-group-5"), Slug: "r5-group-5", Name: "R5 Group 5",
		OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	targetUser := &store.User{
		ID: tid("r5-target-5"), Email: "r5-target5@test.com",
		DisplayName: "R5 Target 5", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, targetUser))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: group.ID, MemberType: store.GroupMemberTypeUser,
		MemberID: targetUser.ID, Role: store.GroupMemberRoleMember,
	}))

	// Make the group constraint-bearing.
	createGroupClosureConstraint(t, s, group.ID)

	// Admin (dev user with super-admin) can remove members.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/groups/"+group.ID+"/members/user/"+targetUser.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"admin should be able to remove member from constraint-bearing group")
}

func TestDeleteGroup_ConstraintBearing_AllowedWithAdmin(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID: tid("r5-group-6"), Slug: "r5-group-6", Name: "R5 Group 6",
		OwnerID: DevUserID,
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	// Make constraint-bearing.
	createGroupPrincipalConstraint(t, s, group.ID)

	// Admin can delete.
	rec := doRequest(t, srv, http.MethodDelete,
		"/api/v1/groups/"+group.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"admin should be able to delete constraint-bearing group")
}
