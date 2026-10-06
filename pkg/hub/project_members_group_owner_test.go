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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
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
		Annotations: map[string]string{store.AnnotationProjectMembersGroup: "true"},
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
	adminAdded := createStaleOwnerUser(t, f.s, tid("mg-owner-admin-added"), "mg-admin-added@test.com")
	rec = doRequestAsUser(t, f.srv, admin, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
		AddGroupMemberRequest{MemberType: "user", MemberID: adminAdded.ID, Role: "member"})
	assert.Equal(t, http.StatusCreated, rec.Code, "hub admin can still add to the members group: %s", rec.Body.String())
	_, err = f.s.GetGroupMembership(ctx, g.ID, store.GroupMemberTypeUser, adminAdded.ID)
	assert.NoError(t, err, "hub admin add must create the membership")

	rec = doRequestAsUser(t, f.srv, admin, http.MethodDelete,
		"/api/v1/groups/"+g.ID+"/members/user/"+existing.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "hub admin can still manage the members group: %s", rec.Body.String())
}

// TestBackfillClearProjectMembersGroupOwners pins the startup backfill:
// OwnerID is cleared on project members groups identified by either marker
// key (ptone/scion#2556 tracks the key mismatch), other groups are left
// untouched, a creator whose only remaining authority is the legacy
// OwnerID loses group.* once the backfill runs, and a second run is a no-op.
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
	// The owner relationship on this project-scoped group requires active
	// project access (ptone/scion#2141). An access-only binding (no
	// permissions) supplies it, so the legacy OwnerID is the creator's only
	// source of group.addMember before the backfill.
	grantProjectAccessOnly(t, s, f.creator.ID, f.project.ID)
	require.True(t, f.srv.authzService.CheckAccess(ctx, ident, groupResource(hubKeyGroup), ActionAddMember).Allowed,
		"precondition: the legacy OwnerID gives the creator group.addMember")

	// A second project whose members group carries only the entadapter key.
	legacyProject := &store.Project{
		ID: tid("mg-backfill-legacy-project"), Name: "MG Legacy", Slug: "mg-backfill-legacy-project",
		OwnerID: f.creator.ID, CreatedBy: f.creator.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, legacyProject))
	legacyKeyGroup := &store.Group{
		ID: tid("mg-backfill-legacy-key"), Name: "MG Legacy Members", Slug: projectMembersGroupSlug(legacyProject.Slug),
		GroupType: store.GroupTypeExplicit, ProjectID: legacyProject.ID, OwnerID: f.creator.ID,
		Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
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
		Annotations: map[string]string{store.AnnotationProjectMembersGroup: "false"},
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
		"after the backfill the creator has no group.addMember")

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

// TestProjectMembersGroup_CreatorOwnerUsesProjectMembersEndpoint pins the
// contract for a creator who still holds the project-owner binding: the
// members group created by the real creation path carries no OwnerID, so
// the group API is hub-admin-only (403), and membership is managed through
// the project members endpoint, which the owner binding authorizes (201).
func TestProjectMembersGroup_CreatorOwnerUsesProjectMembersEndpoint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := createStaleOwnerUser(t, s, tid("mg-creator-owner"), "mg-creator-owner@test.com")
	outsider := createStaleOwnerUser(t, s, tid("mg-creator-outsider"), "mg-creator-outsider@test.com")
	project := &store.Project{
		ID: tid("mg-creator-project"), Name: "MG Creator", Slug: "mg-creator-project",
		OwnerID: owner.ID, CreatedBy: owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.seedProjectCreatorMembership(ctx, project)

	g := membersGroupFor(t, s, project)
	require.Empty(t, g.OwnerID, "members group must not copy Project.OwnerID")

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/groups/"+g.ID+"/members",
		AddGroupMemberRequest{MemberType: "user", MemberID: outsider.ID, Role: "member"})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"members group mutation through the group API is hub-admin-only; got: %s", rec.Body.String())

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/projects/"+project.ID+"/members",
		addProjectMemberRequest{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      outsider.ID,
		})
	require.Equal(t, http.StatusCreated, rec.Code,
		"project owner adds members through the project members endpoint; got: %s", rec.Body.String())
}

// TestUpdateGroup_RejectsOwnerIDOnProjectMembersGroup pins that PATCH cannot
// put an owner back on a project members group, even as a hub admin, since
// the owner relationship would re-grant group.* (ptone/scion#2599). Other
// fields stay patchable, and ordinary groups still accept an owner.
func TestUpdateGroup_RejectsOwnerIDOnProjectMembersGroup(t *testing.T) {
	f, g, existing := setupStaleOwnerMembersGroup(t)
	ctx := context.Background()
	admin := newSuperAdminUser(t, f.s, "mg-patch-hub-admin")

	patch := func(id string, body map[string]interface{}) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, admin, http.MethodPatch, "/api/v1/groups/"+id, body)
	}

	t.Run("members group (hub key)", func(t *testing.T) {
		rec := patch(g.ID, map[string]interface{}{"ownerId": existing.ID, "name": "Renamed"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
		stored := membersGroupFor(t, f.s, f.project)
		assert.Empty(t, stored.OwnerID, "rejected PATCH must not set an owner")
		assert.Equal(t, g.Name, stored.Name, "rejected PATCH must not apply other fields")
	})

	t.Run("members group (entadapter key)", func(t *testing.T) {
		legacy := &store.Group{
			ID: tid("mg-patch-legacy-key"), Name: "MG Patch Legacy", Slug: "mg-patch-legacy-key",
			GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
			Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
		}
		require.NoError(t, f.s.CreateGroup(ctx, legacy))
		rec := patch(legacy.ID, map[string]interface{}{"ownerId": existing.ID})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		stored, err := f.s.GetGroup(ctx, legacy.ID)
		require.NoError(t, err)
		assert.Empty(t, stored.OwnerID)
	})

	t.Run("marker added in the same PATCH", func(t *testing.T) {
		unmarked := &store.Group{
			ID: tid("mg-patch-unmarked"), Name: "MG Patch Unmarked", Slug: "mg-patch-unmarked",
			GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
		}
		require.NoError(t, f.s.CreateGroup(ctx, unmarked))
		rec := patch(unmarked.ID, map[string]interface{}{
			"ownerId":     existing.ID,
			"annotations": map[string]string{store.AnnotationProjectMembersGroup: "true"},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		stored, err := f.s.GetGroup(ctx, unmarked.ID)
		require.NoError(t, err)
		assert.Empty(t, stored.OwnerID)
	})

	t.Run("ownerId with marker-less annotations on a marked group", func(t *testing.T) {
		// Pins the stored-group half of the owner guard: the patched
		// annotations carry no marker, so only the stored group identifies
		// this as a members group. The error message is asserted so the
		// later marker-immutability guard cannot mask a regression here.
		rec := patch(g.ID, map[string]interface{}{
			"ownerId":     existing.ID,
			"annotations": map[string]string{"other": "v"},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "ownerId cannot be set on a project members group")
		stored := membersGroupFor(t, f.s, f.project)
		assert.Empty(t, stored.OwnerID, "rejected PATCH must not set an owner")
		assert.Equal(t, "true", stored.Annotations[store.AnnotationProjectMembersGroup],
			"rejected PATCH must keep the marker")
		assert.NotContains(t, stored.Annotations, "other")
	})

	t.Run("members group other fields still patchable", func(t *testing.T) {
		rec := patch(g.ID, map[string]interface{}{"description": "admin note"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "admin note", membersGroupFor(t, f.s, f.project).Description)
	})

	t.Run("ordinary group accepts owner", func(t *testing.T) {
		ordinary := &store.Group{
			ID: tid("mg-patch-ordinary"), Name: "MG Patch Ordinary", Slug: "mg-patch-ordinary",
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, f.s.CreateGroup(ctx, ordinary))
		rec := patch(ordinary.ID, map[string]interface{}{"ownerId": existing.ID})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		stored, err := f.s.GetGroup(ctx, ordinary.ID)
		require.NoError(t, err)
		assert.Equal(t, existing.ID, stored.OwnerID)
	})
}

// TestUpdateGroup_ProjectMembersGroupMarkerImmutable pins that PATCH cannot
// remove, add or change the project-members-group marker keys. Without this, a
// hub admin could strip the marker in one PATCH and set an owner in the next;
// the owner would then escape both the PATCH owner guard and the
// owner-clearing backfill, and createProjectMembersGroup would refuse to adopt
// the group (ptone/scion#2599).
//
// Every mutating subtest uses its own freshly created group, so a regression
// in one guard cannot cascade into the other subtests.
func TestUpdateGroup_ProjectMembersGroupMarkerImmutable(t *testing.T) {
	f, g, existing := setupStaleOwnerMembersGroup(t)
	ctx := context.Background()
	admin := newSuperAdminUser(t, f.s, "mg-marker-hub-admin")

	patch := func(id string, body map[string]interface{}) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, admin, http.MethodPatch, "/api/v1/groups/"+id, body)
	}
	const markerMsg = "project members group marker annotations cannot be removed or changed"

	// newGroup creates a fresh project-scoped group with the given
	// annotations (nil for an unmarked group).
	newGroup := func(t *testing.T, slug string, annotations map[string]string) *store.Group {
		t.Helper()
		grp := &store.Group{
			ID: tid(slug), Name: "Group " + slug, Slug: slug,
			GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
			Annotations: annotations,
		}
		require.NoError(t, f.s.CreateGroup(ctx, grp))
		return grp
	}
	getGroup := func(t *testing.T, id string) *store.Group {
		t.Helper()
		stored, err := f.s.GetGroup(ctx, id)
		require.NoError(t, err)
		return stored
	}
	hubKey := func() map[string]string {
		return map[string]string{store.AnnotationProjectMembersGroup: "true"}
	}

	t.Run("two-step bypass: strip marker then set owner", func(t *testing.T) {
		// The only subtest that uses the real members group.
		before := membersGroupFor(t, f.s, f.project)

		rec := patch(g.ID, map[string]interface{}{
			"name":        "Stripped",
			"annotations": map[string]string{"x": "y"},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
		assert.Contains(t, rec.Body.String(), markerMsg)
		after := membersGroupFor(t, f.s, f.project)
		assert.Equal(t, before.Annotations, after.Annotations, "rejected PATCH must leave annotations unchanged")
		assert.Equal(t, before.Name, after.Name, "rejected PATCH must not apply other fields")

		rec = patch(g.ID, map[string]interface{}{"ownerId": existing.ID})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Empty(t, membersGroupFor(t, f.s, f.project).OwnerID)
	})

	t.Run("marker value changed", func(t *testing.T) {
		grp := newGroup(t, "mg-marker-value", hubKey())
		rec := patch(grp.ID, map[string]interface{}{
			"annotations": map[string]string{store.AnnotationProjectMembersGroup: "false"},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), markerMsg)
		assert.Equal(t, hubKey(), getGroup(t, grp.ID).Annotations)
	})

	t.Run("entadapter key stripped", func(t *testing.T) {
		grp := newGroup(t, "mg-marker-legacy-key", map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"})
		rec := patch(grp.ID, map[string]interface{}{"annotations": map[string]string{}})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), markerMsg)
		assert.Equal(t, map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"}, getGroup(t, grp.ID).Annotations)
	})

	t.Run("entadapter key added to a hub-key group", func(t *testing.T) {
		// Pins the "add" half of changesProjectMembersGroupMarker.
		grp := newGroup(t, "mg-marker-add-legacy", hubKey())
		rec := patch(grp.ID, map[string]interface{}{
			"name": "Added",
			"annotations": map[string]string{
				store.AnnotationProjectMembersGroup:       "true",
				store.LegacyAnnotationProjectMembersGroup: "true",
			},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
		assert.Contains(t, rec.Body.String(), markerMsg)
		stored := getGroup(t, grp.ID)
		assert.Equal(t, hubKey(), stored.Annotations, "rejected PATCH must leave annotations unchanged")
		assert.Equal(t, grp.Name, stored.Name, "rejected PATCH must not apply other fields")
	})

	t.Run("other annotations with marker preserved", func(t *testing.T) {
		grp := newGroup(t, "mg-marker-keep", hubKey())
		annotations := hubKey()
		annotations["note"] = "kept"
		rec := patch(grp.ID, map[string]interface{}{"annotations": annotations, "name": "Members Renamed"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		stored := getGroup(t, grp.ID)
		assert.Equal(t, annotations, stored.Annotations)
		assert.Equal(t, "Members Renamed", stored.Name)
	})

	t.Run("other fields without annotations", func(t *testing.T) {
		grp := newGroup(t, "mg-marker-desc", hubKey())
		rec := patch(grp.ID, map[string]interface{}{"description": "admin note"})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		stored := getGroup(t, grp.ID)
		assert.Equal(t, "admin note", stored.Description)
		assert.Equal(t, hubKey(), stored.Annotations)
	})

	t.Run("unmarked group annotations freely replaceable", func(t *testing.T) {
		ordinary := &store.Group{
			ID: tid("mg-marker-ordinary"), Name: "MG Marker Ordinary", Slug: "mg-marker-ordinary",
			GroupType: store.GroupTypeExplicit, Annotations: map[string]string{"a": "b"},
		}
		require.NoError(t, f.s.CreateGroup(ctx, ordinary))
		rec := patch(ordinary.ID, map[string]interface{}{"annotations": map[string]string{"c": "d"}})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, map[string]string{"c": "d"}, getGroup(t, ordinary.ID).Annotations)
	})
}

// TestUpdateGroup_ProjectMembersGroupMarkerNotAddable pins that the marker
// keys are system-written only: a PATCH cannot add either key to an unmarked
// group, while ordinary annotation edits on that group still succeed
// (ptone/scion#2599, review r3 N3).
func TestUpdateGroup_ProjectMembersGroupMarkerNotAddable(t *testing.T) {
	f, _, _ := setupStaleOwnerMembersGroup(t)
	ctx := context.Background()
	admin := newSuperAdminUser(t, f.s, "mg-marker-add-hub-admin")

	patch := func(id string, body map[string]interface{}) *httptest.ResponseRecorder {
		return doRequestAsUser(t, f.srv, admin, http.MethodPatch, "/api/v1/groups/"+id, body)
	}
	const addMsg = "project members group marker annotations are system-written and cannot be added"

	// newUnmarked creates a fresh unmarked group that has a ProjectID.
	newUnmarked := func(t *testing.T, slug string) *store.Group {
		t.Helper()
		grp := &store.Group{
			ID: tid(slug), Name: "Group " + slug, Slug: slug,
			GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID,
			Annotations: map[string]string{"a": "b"},
		}
		require.NoError(t, f.s.CreateGroup(ctx, grp))
		return grp
	}

	for _, key := range []string{store.AnnotationProjectMembersGroup, store.LegacyAnnotationProjectMembersGroup} {
		t.Run("adding "+key, func(t *testing.T) {
			grp := newUnmarked(t, "mg-add-"+strings.ReplaceAll(strings.TrimPrefix(key, "scion.io/"), "/", "-"))
			rec := patch(grp.ID, map[string]interface{}{
				"name":        "Marked",
				"annotations": map[string]string{"a": "b", key: "true"},
			})
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
			assert.Contains(t, rec.Body.String(), addMsg)
			stored, err := f.s.GetGroup(ctx, grp.ID)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"a": "b"}, stored.Annotations, "rejected PATCH must not apply annotations")
			assert.Equal(t, grp.Name, stored.Name, "rejected PATCH must not apply other fields")
			assert.False(t, hasProjectMembersGroupMarker(stored))
		})
	}

	t.Run("ordinary annotation edit still succeeds", func(t *testing.T) {
		grp := newUnmarked(t, "mg-add-ordinary-edit")
		rec := patch(grp.ID, map[string]interface{}{
			"name":        "Edited",
			"annotations": map[string]string{"c": "d"},
		})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		stored, err := f.s.GetGroup(ctx, grp.ID)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"c": "d"}, stored.Annotations)
		assert.Equal(t, "Edited", stored.Name)
	})
}

// TestCreateGroup_RejectsProjectMembersGroupMarker pins that the marker keys
// are system-written only on POST too: createGroup rejects a request carrying
// either key, whatever its value, while ordinary creates (with annotations,
// nil annotations or an empty annotations map) still succeed
// (ptone/scion#2599, review r4 L1, review r5 N2/N3).
func TestCreateGroup_RejectsProjectMembersGroupMarker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	post := func(body map[string]interface{}) *httptest.ResponseRecorder {
		return doRequest(t, srv, http.MethodPost, "/api/v1/groups", body)
	}
	const addMsg = "project members group marker annotations are system-written and cannot be added"

	assertRejected := func(t *testing.T, slug string, annotations map[string]string) {
		t.Helper()
		rec := post(map[string]interface{}{
			"name":        "Group " + slug,
			"slug":        slug,
			"annotations": annotations,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeValidationError)
		assert.Contains(t, rec.Body.String(), addMsg)
		_, err := s.GetGroupBySlug(ctx, slug)
		assert.ErrorIs(t, err, store.ErrNotFound, "rejected POST must not create the group")
	}

	for _, key := range []string{store.AnnotationProjectMembersGroup, store.LegacyAnnotationProjectMembersGroup} {
		keySlug := strings.ReplaceAll(strings.TrimPrefix(key, "scion.io/"), "/", "-")
		t.Run("creating with "+key, func(t *testing.T) {
			assertRejected(t, "mg-create-"+keySlug, map[string]string{"a": "b", key: "true"})
		})
		// The guard checks key presence, not the value: a non-"true" value
		// is rejected too.
		t.Run("creating with "+key+"=false", func(t *testing.T) {
			assertRejected(t, "mg-create-false-"+keySlug, map[string]string{key: "false"})
		})
	}

	t.Run("ordinary create with annotations still succeeds", func(t *testing.T) {
		rec := post(map[string]interface{}{
			"name":        "Group mg-create-ordinary",
			"slug":        "mg-create-ordinary",
			"annotations": map[string]string{"c": "d"},
		})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		stored, err := s.GetGroupBySlug(ctx, "mg-create-ordinary")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"c": "d"}, stored.Annotations)
		assert.False(t, hasProjectMembersGroupMarker(stored))
	})

	t.Run("create with nil annotations succeeds", func(t *testing.T) {
		rec := post(map[string]interface{}{
			"name": "Group mg-create-nil-annotations",
			"slug": "mg-create-nil-annotations",
		})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		stored, err := s.GetGroupBySlug(ctx, "mg-create-nil-annotations")
		require.NoError(t, err)
		assert.False(t, hasProjectMembersGroupMarker(stored))
	})

	t.Run("create with empty annotations map succeeds", func(t *testing.T) {
		rec := post(map[string]interface{}{
			"name":        "Group mg-create-empty-annotations",
			"slug":        "mg-create-empty-annotations",
			"annotations": map[string]string{},
		})
		assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		stored, err := s.GetGroupBySlug(ctx, "mg-create-empty-annotations")
		require.NoError(t, err)
		assert.False(t, hasProjectMembersGroupMarker(stored))
	})
}

// TestBackfillClearProjectMembersGroupOwners_Paginates pins the NextCursor
// loop: with a small page size, a marked group on a later page is cleared.
func TestBackfillClearProjectMembersGroupOwners_Paginates(t *testing.T) {
	f, g, _ := setupStaleOwnerMembersGroup(t)
	ctx := context.Background()
	s := f.s

	orig := projectMembersGroupOwnerBackfillPageSize
	projectMembersGroupOwnerBackfillPageSize = 2
	t.Cleanup(func() { projectMembersGroupOwnerBackfillPageSize = orig })

	// Filler groups created after the members group, then a second marked
	// group created last so it sorts onto a later page.
	for i := 0; i < 5; i++ {
		id := tid("mg-page-filler-" + string(rune('a'+i)))
		require.NoError(t, s.CreateGroup(ctx, &store.Group{
			ID: id, Name: id, Slug: id, GroupType: store.GroupTypeExplicit,
			OwnerID: f.coOwner.ID, Created: time.Now().Add(time.Duration(i+1) * time.Second),
		}))
	}
	late := &store.Group{
		ID: tid("mg-page-late-marked"), Name: "MG Late Marked", Slug: "mg-page-late-marked",
		GroupType: store.GroupTypeExplicit, ProjectID: f.project.ID, OwnerID: f.creator.ID,
		Annotations: map[string]string{store.AnnotationProjectMembersGroup: "true"},
		Created:     time.Now().Add(time.Hour),
	}
	require.NoError(t, s.CreateGroup(ctx, late))
	g.OwnerID = f.creator.ID
	require.NoError(t, s.UpdateGroup(ctx, g))

	// Precondition: the late marked group is not on the first page.
	first, err := s.ListGroups(ctx, store.GroupFilter{}, store.ListOptions{
		Limit: projectMembersGroupOwnerBackfillPageSize, SkipTotalCount: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, first.NextCursor, "precondition: more than one page")
	for _, item := range first.Items {
		require.NotEqual(t, late.ID, item.ID, "precondition: late marked group must not be on page 1")
	}

	require.NoError(t, backfillClearProjectMembersGroupOwners(ctx, s))

	stored, err := s.GetGroup(ctx, late.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.OwnerID, "marked group on a later page must be cleared")
	assert.Empty(t, membersGroupFor(t, s, f.project).OwnerID, "fixture members group must be cleared")
}

// backfillFailingStore makes selected BackfillRoleBindings steps fail.
type backfillFailingStore struct {
	store.Store
	failListUsers  bool
	failListGroups bool
}

func (b *backfillFailingStore) ListUsers(ctx context.Context, f store.UserFilter, o store.ListOptions) (*store.ListResult[store.User], error) {
	if b.failListUsers {
		return nil, errors.New("injected ListUsers failure")
	}
	return b.Store.ListUsers(ctx, f, o)
}

func (b *backfillFailingStore) ListGroups(ctx context.Context, f store.GroupFilter, o store.ListOptions) (*store.ListResult[store.Group], error) {
	if b.failListGroups {
		return nil, errors.New("injected ListGroups failure")
	}
	return b.Store.ListGroups(ctx, f, o)
}

// TestBackfillRoleBindings_ClearRunsWhenEarlierStepFails pins that the
// security-relevant owner clear runs even when an earlier backfill step
// fails, and that errors from both are reported (neither hides the other).
func TestBackfillRoleBindings_ClearRunsWhenEarlierStepFails(t *testing.T) {
	t.Run("earlier step fails, clear still runs", func(t *testing.T) {
		f, g, _ := setupStaleOwnerMembersGroup(t)
		ctx := context.Background()
		g.OwnerID = f.creator.ID
		require.NoError(t, f.s.UpdateGroup(ctx, g))

		err := BackfillRoleBindings(ctx, &backfillFailingStore{Store: f.s, failListUsers: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "backfill user role bindings")
		assert.Contains(t, err.Error(), "injected ListUsers failure")
		assert.NotContains(t, err.Error(), "clear project members group owners")

		assert.Empty(t, membersGroupFor(t, f.s, f.project).OwnerID,
			"owner clear must run even when an earlier step fails")
	})

	t.Run("both fail, both errors reported", func(t *testing.T) {
		f, _, _ := setupStaleOwnerMembersGroup(t)
		err := BackfillRoleBindings(context.Background(),
			&backfillFailingStore{Store: f.s, failListUsers: true, failListGroups: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "injected ListUsers failure")
		assert.Contains(t, err.Error(), "injected ListGroups failure")
	})
}

// TestProjectMembersGroup_LegacyMarkerAdoptedAfterMigration is the
// ptone/scion#2556 regression: a members group marked only with the legacy
// key (written by the store marker backfill before the fix) used to be
// refused by project re-ensure. After the startup migration rewrites the
// key, re-ensure must adopt it. Adoption sets no Group.OwnerID and creates
// no role binding; as for any canonical members group, the creator is
// (re-)added as a group owner member, which carries no project authority.
func TestProjectMembersGroup_LegacyMarkerAdoptedAfterMigration(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	var logs bytes.Buffer
	srv.projectsLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	creator := createStaleOwnerUser(t, s, tid("mg-legacy-creator"), "mg-legacy-creator@test.com")
	project := &store.Project{
		ID: tid("mg-legacy-project"), Name: "MG Legacy", Slug: "mg-legacy-project",
		OwnerID: creator.ID, CreatedBy: creator.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	legacy := &store.Group{
		ID: tid("mg-legacy-group"), Name: "MG Legacy Members", Slug: projectMembersGroupSlug(project.Slug),
		GroupType: store.GroupTypeExplicit, ProjectID: project.ID,
		Annotations: map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"},
	}
	require.NoError(t, s.CreateGroup(ctx, legacy))

	migrator, ok := s.(interface {
		MigrateLegacyProjectMembersGroupMarkers(context.Context) error
	})
	require.True(t, ok, "test store must expose the legacy marker migration")
	// The test server already ran the store migrations on its empty
	// database, which completed the one-shot rewrite. Clear its completion
	// marker to model a database upgraded with a legacy-marked group in
	// place.
	require.NoError(t, s.DeleteHubSetting(ctx, entadapter.LegacyProjectMembersGroupMarkerMigrationSection))
	// The group must still carry the legacy key right before the migration
	// runs, and the completion marker must really be gone; otherwise the
	// rewrite below could silently be a no-op.
	premigration, err := s.GetGroup(ctx, legacy.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{store.LegacyAnnotationProjectMembersGroup: "true"}, premigration.Annotations,
		"the seeded group must still carry only the legacy key before the migration")
	_, err = s.GetHubSetting(ctx, entadapter.LegacyProjectMembersGroupMarkerMigrationSection)
	require.ErrorIs(t, err, store.ErrNotFound, "the migration completion marker must be cleared")
	require.NoError(t, migrator.MigrateLegacyProjectMembersGroupMarkers(ctx))

	migrated, err := s.GetGroup(ctx, legacy.ID)
	require.NoError(t, err)
	assert.True(t, isSystemProjectMembersGroup(migrated, project.ID),
		"the migrated group must be recognised as the system members group")

	bindingsBefore, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, creator.ID)
	require.NoError(t, err)

	srv.createProjectMembersGroup(ctx, project)

	assert.NotContains(t, logs.String(), "refusing to adopt colliding project members group")
	adopted := membersGroupFor(t, s, project)
	assert.Equal(t, legacy.ID, adopted.ID, "re-ensure must adopt the existing group, not replace it")
	assert.Empty(t, adopted.OwnerID, "adoption must not set Group.OwnerID (ptone/scion#2599)")

	bindingsAfter, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, creator.ID)
	require.NoError(t, err)
	assert.Len(t, bindingsAfter, len(bindingsBefore), "adoption must not grant the creator a role binding")
	groupBindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalGroup, legacy.ID)
	require.NoError(t, err)
	assert.Empty(t, groupBindings, "adoption must not create a role binding naming the members group as principal")

	// As for any canonical members group, re-ensure re-adds the creator as a
	// group owner member (no project authority under PM1).
	membership, err := s.GetGroupMembership(ctx, legacy.ID, store.GroupMemberTypeUser, creator.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GroupMemberRoleOwner, membership.Role)
}
