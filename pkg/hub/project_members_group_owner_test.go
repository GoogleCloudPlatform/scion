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

// ptone/scion#2599: the project members group must not take authority from
// Project.OwnerID. createProjectMembersGroup used to copy Project.OwnerID
// into Group.OwnerID, and the owner/user/group relationship row grants
// group.* to Group.OwnerID, so a creator removed from the project without an
// ownership transfer kept managing the members group.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// membersGroupFor returns the project's members group.
func membersGroupFor(t *testing.T, s store.Store, project *store.Project) *store.Group {
	t.Helper()
	g, err := s.GetGroupBySlug(context.Background(), projectMembersGroupSlug(project.Slug))
	require.NoError(t, err)
	require.True(t, isSystemProjectMembersGroup(g, project.ID), "must be the system members group")
	return g
}

// setupStaleOwnerMembersGroup builds the stale-owner fixture (the creator
// was removed by the co-owner without an ownership transfer, so
// Project.OwnerID still names the creator), ensures the members group, and
// adds a plain member the tests try to remove.
func setupStaleOwnerMembersGroup(t *testing.T) (staleOwnerFixture, *store.Group, *store.User) {
	t.Helper()
	f := setupStaleOwnerFixture(t)
	ctx := context.Background()
	f.srv.createProjectMembersGroup(ctx, f.project)
	g := membersGroupFor(t, f.s, f.project)

	existing := createStaleOwnerUser(t, f.s, tid("mg-owner-existing-member"), "mg-existing-member@test.com")
	require.NoError(t, f.s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: g.ID, MemberType: store.GroupMemberTypeUser, MemberID: existing.ID,
		Role: store.GroupMemberRoleMember,
	}))
	return f, g, existing
}

func TestProjectMembersGroup_CreatedWithoutOwnerID(t *testing.T) {
	f, g, _ := setupStaleOwnerMembersGroup(t)
	assert.Empty(t, g.OwnerID, "members group must not copy Project.OwnerID (%s)", f.project.OwnerID)
}

// TestProjectMembersGroup_AdoptDoesNotRefillOwnerID pins that adopting an
// existing ownerless system members group (on GET, register, re-create or
// clone) does not fill Group.OwnerID from Project.OwnerID.
func TestProjectMembersGroup_AdoptDoesNotRefillOwnerID(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	creator := createStaleOwnerUser(t, s, tid("mg-adopt-creator"), "mg-adopt-creator@test.com")
	project := &store.Project{
		ID: tid("mg-adopt-project"), Name: "MG Adopt", Slug: "mg-adopt-project",
		OwnerID: creator.ID, CreatedBy: creator.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: tid("mg-adopt-group"), Name: "MG Adopt Members", Slug: projectMembersGroupSlug(project.Slug),
		GroupType: store.GroupTypeExplicit, ProjectID: project.ID,
		Annotations: map[string]string{systemProjectMembersGroupAnnotation: "true"},
	}))

	srv.createProjectMembersGroup(ctx, project)

	assert.Empty(t, membersGroupFor(t, s, project).OwnerID,
		"adopting an existing members group must not refill OwnerID from Project.OwnerID")
}

// TestProjectMembersGroup_RemovedCreatorDeniedGroupMutations is the
// ptone/scion#2599 regression: a creator removed without a transfer (no
// binding, OwnerID still names them) must not mutate the members group.
// Group reads are not asserted as denied: group.read comes from the
// hub-member binding, not from Group.OwnerID.
func TestProjectMembersGroup_RemovedCreatorDeniedGroupMutations(t *testing.T) {
	groupPath := func(g *store.Group) string { return "/api/v1/groups/" + g.ID }

	t.Run("add member", func(t *testing.T) {
		f, g, _ := setupStaleOwnerMembersGroup(t)
		newcomer := createStaleOwnerUser(t, f.s, tid("mg-owner-newcomer"), "mg-newcomer@test.com")
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPost, groupPath(g)+"/members",
			map[string]string{"memberType": "user", "memberId": newcomer.ID})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		_, err := f.s.GetGroupMembership(context.Background(), g.ID, store.GroupMemberTypeUser, newcomer.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "add must not create a membership")
	})

	t.Run("remove member", func(t *testing.T) {
		f, g, existing := setupStaleOwnerMembersGroup(t)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodDelete,
			groupPath(g)+"/members/user/"+existing.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		_, err := f.s.GetGroupMembership(context.Background(), g.ID, store.GroupMemberTypeUser, existing.ID)
		assert.NoError(t, err, "remove must not delete the membership")
	})

	t.Run("update", func(t *testing.T) {
		f, g, _ := setupStaleOwnerMembersGroup(t)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodPatch, groupPath(g),
			map[string]string{"name": "Renamed By Removed Creator", "ownerId": f.creator.ID})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		stored := membersGroupFor(t, f.s, f.project)
		assert.Equal(t, g.Name, stored.Name, "PATCH must not rename the group")
		assert.Empty(t, stored.OwnerID, "PATCH must not set an owner")
	})

	t.Run("delete", func(t *testing.T) {
		f, g, _ := setupStaleOwnerMembersGroup(t)
		rec := doRequestAsUser(t, f.srv, f.creator, http.MethodDelete, groupPath(g), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		_, err := f.s.GetGroup(context.Background(), g.ID)
		assert.NoError(t, err, "DELETE must not delete the group")
	})

	t.Run("authz decisions", func(t *testing.T) {
		f, g, _ := setupStaleOwnerMembersGroup(t)
		ident := NewAuthenticatedUser(f.creator.ID, f.creator.Email, f.creator.DisplayName, "member", "api")
		for _, a := range []Action{ActionUpdate, ActionDelete, ActionAddMember, ActionRemoveMember} {
			d := f.srv.authzService.CheckAccess(context.Background(), ident, groupResource(g), a)
			assert.False(t, d.Allowed, "removed creator must not have group %s; reason=%q", a, d.Reason)
		}
	})
}

// TestProjectMembersGroup_CurrentOwnerKeepsLegitimateAccess is the positive
// control: the current owner, through their project-owner binding, keeps
// what they could do before the fix: read the members group and manage
// project membership through the project members endpoints. (A non-creator
// owner never had group.* mutation on the members group; that was the
// creator-only Group.OwnerID special case.) A hub admin can still mutate the
// group through the group API.
func TestProjectMembersGroup_CurrentOwnerKeepsLegitimateAccess(t *testing.T) {
	f, g, existing := setupStaleOwnerMembersGroup(t)
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.coOwner, http.MethodGet, "/api/v1/groups/"+g.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodGet, "/api/v1/groups/"+g.ID+"/members", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	projectBase := "/api/v1/projects/" + f.project.ID
	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodGet, projectBase+"/members", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	memberRD, err := f.s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	newMember := createStaleOwnerUser(t, f.s, tid("mg-owner-new-project-member"), "mg-new-project-member@test.com")
	rec = doRequestAsUser(t, f.srv, f.coOwner, http.MethodPost, projectBase+"/members", addProjectMemberRequest{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      newMember.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	admin := newSuperAdminUser(t, f.s, "mg-owner-hub-admin")
	rec = doRequestAsUser(t, f.srv, admin, http.MethodDelete,
		"/api/v1/groups/"+g.ID+"/members/user/"+existing.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "hub admin can still manage the members group: %s", rec.Body.String())
}

// TestBackfillClearProjectMembersGroupOwners pins the startup backfill:
// OwnerID is cleared on project members groups identified by either marker
// key (ptone/scion#2556 tracks the key mismatch), other groups are left
// untouched, a removed creator loses group.* once the backfill runs, and a
// second run is a no-op.
func TestBackfillClearProjectMembersGroupOwners(t *testing.T) {
	f := setupStaleOwnerFixture(t)
	ctx := context.Background()
	s := f.s

	// The fixture project's members group, with the legacy OwnerID that
	// createProjectMembersGroup used to copy from Project.OwnerID.
	f.srv.createProjectMembersGroup(ctx, f.project)
	hubKeyGroup := membersGroupFor(t, s, f.project)
	hubKeyGroup.OwnerID = f.creator.ID
	require.NoError(t, s.UpdateGroup(ctx, hubKeyGroup))

	ident := NewAuthenticatedUser(f.creator.ID, f.creator.Email, f.creator.DisplayName, "member", "api")
	require.True(t, f.srv.authzService.CheckAccess(ctx, ident, groupResource(hubKeyGroup), ActionAddMember).Allowed,
		"precondition: legacy OwnerID gives the removed creator group.addMember")

	// A second project whose members group carries only the entadapter key.
	legacyProject := &store.Project{
		ID: tid("mg-backfill-legacy-project"), Name: "MG Legacy", Slug: "mg-backfill-legacy-project",
		OwnerID: f.creator.ID, CreatedBy: f.creator.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, legacyProject))
	legacyKeyGroup := &store.Group{
		ID: tid("mg-backfill-legacy-key"), Name: "MG Legacy Members", Slug: projectMembersGroupSlug(legacyProject.Slug),
		GroupType: store.GroupTypeExplicit, ProjectID: legacyProject.ID, OwnerID: f.creator.ID,
		Annotations: map[string]string{legacyProjectMembersGroupAnnotation: "true"},
	}
	require.NoError(t, s.CreateGroup(ctx, legacyKeyGroup))

	// Untouched: an ordinary user-owned group, a project-scoped group with a
	// members-like slug but no marker, and a project-scoped group whose
	// marker is not "true".
	ordinary := &store.Group{
		ID: tid("mg-backfill-ordinary"), Name: "Ordinary", Slug: "mg-backfill-ordinary",
		GroupType: store.GroupTypeExplicit, OwnerID: f.creator.ID,
	}
	lookAlike := &store.Group{
		ID: tid("mg-backfill-lookalike"), Name: "Look Alike", Slug: "project:mg-backfill-lookalike:members",
		GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID, OwnerID: f.coOwner.ID,
	}
	falseMarker := &store.Group{
		ID: tid("mg-backfill-false-marker"), Name: "False Marker", Slug: "mg-backfill-false-marker",
		GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID, OwnerID: f.coOwner.ID,
		Annotations: map[string]string{systemProjectMembersGroupAnnotation: "false"},
	}
	for _, g := range []*store.Group{ordinary, lookAlike, falseMarker} {
		require.NoError(t, s.CreateGroup(ctx, g))
	}

	require.NoError(t, backfillClearProjectMembersGroupOwners(ctx, s))

	get := func(id string) *store.Group {
		g, err := s.GetGroup(ctx, id)
		require.NoError(t, err)
		return g
	}
	assert.Empty(t, get(hubKeyGroup.ID).OwnerID, "members group (hub key) owner must be cleared")
	assert.Empty(t, get(legacyKeyGroup.ID).OwnerID, "members group (entadapter key) owner must be cleared")
	assert.Equal(t, f.creator.ID, get(ordinary.ID).OwnerID, "ordinary group untouched")
	assert.Equal(t, f.coOwner.ID, get(lookAlike.ID).OwnerID, "look-alike slug without marker untouched")
	assert.Equal(t, f.coOwner.ID, get(falseMarker.ID).OwnerID, "marker not \"true\" untouched")

	assert.False(t, f.srv.authzService.CheckAccess(ctx, ident, groupResource(get(hubKeyGroup.ID)), ActionAddMember).Allowed,
		"after the backfill the removed creator has no group.addMember")

	// Second run: nothing changes.
	before := map[string]*store.Group{}
	for _, id := range []string{hubKeyGroup.ID, legacyKeyGroup.ID, ordinary.ID, lookAlike.ID, falseMarker.ID} {
		before[id] = get(id)
	}
	require.NoError(t, backfillClearProjectMembersGroupOwners(ctx, s))
	for id, b := range before {
		a := get(id)
		assert.Equal(t, b.OwnerID, a.OwnerID, "second run must not change OwnerID of %s", id)
		assert.True(t, b.Updated.Equal(a.Updated), "second run must not update %s", id)
	}
}

// TestBackfillRoleBindings_ClearsProjectMembersGroupOwners pins that the
// startup entry point runs the owner-clearing pass.
func TestBackfillRoleBindings_ClearsProjectMembersGroupOwners(t *testing.T) {
	f, g, _ := setupStaleOwnerMembersGroup(t)
	ctx := context.Background()
	g.OwnerID = f.creator.ID
	require.NoError(t, f.s.UpdateGroup(ctx, g))

	require.NoError(t, BackfillRoleBindings(ctx, f.s))

	assert.Empty(t, membersGroupFor(t, f.s, f.project).OwnerID)
}
