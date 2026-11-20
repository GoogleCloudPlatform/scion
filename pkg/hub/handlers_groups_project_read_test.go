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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func grantProjectRoleForTest(t *testing.T, s store.Store, projectID, roleName, principalType, principalID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func grantSystemRoleForTest(t *testing.T, s store.Store, roleName, principalID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      principalID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

func listGroupIDsFromResponse(resp ListGroupsResponse) []string {
	ids := make([]string, 0, len(resp.Groups))
	for _, g := range resp.Groups {
		ids = append(ids, g.ID)
	}
	return ids
}

// TestProjectMembersGroupRead_ListFilteredByProjectReadAccess verifies that
// GET /api/v1/groups filters project members groups by project read access
// while preserving the hub-wide directory for non-project groups.
func TestProjectMembersGroupRead_ListFilteredByProjectReadAccess(t *testing.T) {
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := &store.User{
		ID:          tid("user-carol-reader"),
		Email:       "carol-reader@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalUser, carol.ID)

	hubAdmin := &store.User{
		ID:          tid("user-hub-admin"),
		Email:       "hub-admin@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	// A second project where bob is a member.
	projectB := &store.Project{
		ID:        tid("project-bob-readable"),
		Name:      "Bob Project",
		Slug:      "bob-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	srv.seedProjectCreatorMembership(ctx, projectB)

	// A regular non-project directory group.
	dirGroup := &store.Group{
		ID:        tid("dir-group-alpha"),
		Name:      "Team Alpha",
		Slug:      "team-alpha",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGroup(ctx, dirGroup))

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)
	membersGroupB, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectB.Slug))
	require.NoError(t, err)

	// Bob (hub member without read access on projectA) sees dirGroup and
	// projectB's members group, but not projectA's members group.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bobResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&bobResp))
	bobIDs := listGroupIDsFromResponse(bobResp)
	assert.Contains(t, bobIDs, dirGroup.ID, "non-project group remains visible")
	assert.Contains(t, bobIDs, membersGroupB.ID, "readable project members group is visible")
	assert.NotContains(t, bobIDs, membersGroupA.ID, "unreadable project members group is filtered out")
	assert.Equal(t, len(bobResp.Groups), bobResp.TotalCount, "totalCount matches visible groups only")

	// Filtering by projectId or search for projectA returns empty for bob.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/groups?projectId="+url.QueryEscape(projectA.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bobProjResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&bobProjResp))
	assert.Empty(t, bobProjResp.Groups)
	assert.Equal(t, 0, bobProjResp.TotalCount)
	assert.Empty(t, bobProjResp.NextCursor)

	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/groups?search="+url.QueryEscape(projectA.Name), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bobSearchResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&bobSearchResp))
	assert.NotContains(t, listGroupIDsFromResponse(bobSearchResp), membersGroupA.ID)
	assert.Equal(t, len(bobSearchResp.Groups), bobSearchResp.TotalCount)

	// Alice (projectA owner) and Carol (projectA viewer) see membersGroupA.
	for _, tc := range []struct {
		name string
		user *store.User
	}{
		{"project owner", alice},
		{"project viewer", carol},
	} {
		rec = doRequestAsUser(t, srv, tc.user, http.MethodGet, "/api/v1/groups?limit=100", nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tc.name, rec.Body.String())
		var resp ListGroupsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		ids := listGroupIDsFromResponse(resp)
		assert.Contains(t, ids, membersGroupA.ID, "%s should see projectA members group", tc.name)
		assert.Contains(t, ids, dirGroup.ID, "%s should see directory group", tc.name)
		assert.NotContains(t, ids, membersGroupB.ID, "%s should not see projectB members group", tc.name)
		assert.Equal(t, len(resp.Groups), resp.TotalCount)
	}

	// Hub admin sees all groups, including both project members groups.
	rec = doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var adminResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&adminResp))
	adminIDs := listGroupIDsFromResponse(adminResp)
	assert.Contains(t, adminIDs, dirGroup.ID)
	assert.Contains(t, adminIDs, membersGroupA.ID)
	assert.Contains(t, adminIDs, membersGroupB.ID)
	assert.Equal(t, len(adminResp.Groups), adminResp.TotalCount)

	// Granting hub-members project-viewer on projectA makes membersGroupA visible to bob.
	hubMembers, err := s.GetGroupBySlug(ctx, hubMembersSlug)
	require.NoError(t, err)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalGroup, hubMembers.ID)

	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bobAfterResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&bobAfterResp))
	assert.Contains(t, listGroupIDsFromResponse(bobAfterResp), membersGroupA.ID)
}

// TestProjectMembersGroupRead_GetAndMembersRequireProjectReadAccess verifies
// that GET group, GET group members, and GET single group member require
// project read access for project members groups and return 404 otherwise.
func TestProjectMembersGroupRead_GetAndMembersRequireProjectReadAccess(t *testing.T) {
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := &store.User{
		ID:          tid("user-carol-get"),
		Email:       "carol-get@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalUser, carol.ID)

	hubAdmin := &store.User{
		ID:          tid("user-admin-get"),
		Email:       "admin-get@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	dirGroup := &store.Group{
		ID:        tid("dir-group-get"),
		Name:      "Team Beta",
		Slug:      "team-beta",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGroup(ctx, dirGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    dirGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   alice.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	// Reference 404 body for a nonexistent group.
	missingRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/groups/nonexistent-group-id", nil)
	require.Equal(t, http.StatusNotFound, missingRec.Code)

	// Bob gets 404 (matching a missing group) on all read endpoints for membersGroupA.
	blockedPaths := []string{
		"/api/v1/groups/" + membersGroupA.ID,
		"/api/v1/groups/" + membersGroupA.Slug,
		"/api/v1/groups/" + membersGroupA.ID + "/members",
		"/api/v1/groups/" + membersGroupA.Slug + "/members",
		"/api/v1/groups/" + membersGroupA.ID + "/members/user/" + alice.ID,
		"/api/v1/groups/" + membersGroupA.Slug + "/members/user/" + alice.ID,
	}
	for _, path := range blockedPaths {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "path %s should return 404 for non-reader", path)
		assert.JSONEq(t, missingRec.Body.String(), rec.Body.String(),
			"path %s 404 response should match nonexistent group", path)
	}

	// Bob can still read the non-project directory group and its members.
	for _, path := range []string{
		"/api/v1/groups/" + dirGroup.ID,
		"/api/v1/groups/" + dirGroup.Slug,
		"/api/v1/groups/" + dirGroup.ID + "/members",
		"/api/v1/groups/" + dirGroup.ID + "/members/user/" + alice.ID,
	} {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusOK, rec.Code, "directory group path %s should remain 200", path)
	}

	// Project owner, project viewer, and hub admin can read membersGroupA on all read endpoints.
	for _, tc := range []struct {
		name string
		user *store.User
	}{
		{"project owner", alice},
		{"project viewer", carol},
		{"hub admin", hubAdmin},
	} {
		for _, path := range blockedPaths {
			rec := doRequestAsUser(t, srv, tc.user, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, rec.Code, "%s should read %s; body=%s", tc.name, path, rec.Body.String())
		}
	}
}

// TestProjectMembersGroupRead_PaginationAndCounts verifies that cursor
// pagination and totalCount remain exact when project members groups are
// filtered out of GET /api/v1/groups.
func TestProjectMembersGroupRead_PaginationAndCounts(t *testing.T) {
	srv, s, alice, bob, _ := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Seed interleaved visible directory groups and unreadable project members groups.
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 4; i++ {
		ts := base.Add(time.Duration(i*2) * time.Second)
		dir := &store.Group{
			ID:        tid(fmt.Sprintf("pg-dir-%d", i)),
			Name:      fmt.Sprintf("Dir Group %d", i),
			Slug:      fmt.Sprintf("pg-dir-%d", i),
			GroupType: store.GroupTypeExplicit,
			OwnerID:   alice.ID,
			Created:   ts,
			Updated:   ts,
		}
		require.NoError(t, s.CreateGroup(ctx, dir))

		proj := &store.Project{
			ID:        tid(fmt.Sprintf("pg-hidden-proj-%d", i)),
			Name:      fmt.Sprintf("Hidden Project %d", i),
			Slug:      fmt.Sprintf("pg-hidden-proj-%d", i),
			OwnerID:   alice.ID,
			CreatedBy: alice.ID,
			Created:   ts.Add(time.Second),
			Updated:   ts.Add(time.Second),
		}
		require.NoError(t, s.CreateProject(ctx, proj))
		srv.seedProjectCreatorMembership(ctx, proj)
	}

	// Baseline: collect all visible groups in a single large page.
	fullRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, fullRec.Code, fullRec.Body.String())
	var fullResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(fullRec.Body).Decode(&fullResp))
	expectedIDs := listGroupIDsFromResponse(fullResp)
	require.Equal(t, len(expectedIDs), fullResp.TotalCount)
	require.Empty(t, fullResp.NextCursor)
	for _, g := range fullResp.Groups {
		assert.False(t, store.IsProjectMembersGroup(&g.Group),
			"bob has no readable projects; no project members group should appear: %s", g.Slug)
	}

	// Walk with small page sizes and verify exact totals, cursors, and union.
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			var gotIDs []string
			seen := make(map[string]bool)
			cursor := ""
			for pageNum := 0; pageNum < 50; pageNum++ {
				path := fmt.Sprintf("/api/v1/groups?limit=%d", limit)
				if cursor != "" {
					path += "&cursor=" + url.QueryEscape(cursor)
				}
				rec := doRequestAsUser(t, srv, bob, http.MethodGet, path, nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var resp ListGroupsResponse
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
				assert.Equal(t, len(expectedIDs), resp.TotalCount,
					"page %d totalCount must equal exact visible count", pageNum)
				for _, g := range resp.Groups {
					assert.False(t, seen[g.ID], "duplicate group %s on page %d", g.ID, pageNum)
					seen[g.ID] = true
					gotIDs = append(gotIDs, g.ID)
				}
				if resp.NextCursor == "" {
					break
				}
				cursor = resp.NextCursor
			}
			assert.Equal(t, expectedIDs, gotIDs, "paginated walk must match full visible list")
		})
	}
}

type failingGetProjectStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *failingGetProjectStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if !f.fault.Active() {
		return f.Store.GetProject(ctx, id)
	}
	return nil, errors.New("injected project store failure")
}

// TestProjectMembersGroupRead_StoreErrorFailsClosed verifies that a store
// failure while checking project read access fails closed with 500 rather
// than allowing access or masking the error.
func TestProjectMembersGroupRead_StoreErrorFailsClosed(t *testing.T) {
	srv, s, alice, bob, projectA, _, fault := setupDemoPolicyTestWithFault(t, func(inner store.Store, f *storeFaultSwitch) *failingGetProjectStore {
		return &failingGetProjectStore{Store: inner, fault: f}
	})
	ctx := context.Background()

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	fault.Arm()

	for _, path := range []string{
		"/api/v1/groups?limit=100",
		"/api/v1/groups/" + membersGroupA.ID,
		"/api/v1/groups/" + membersGroupA.ID + "/members",
		"/api/v1/groups/" + membersGroupA.ID + "/members/user/" + alice.ID,
	} {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "path %s should fail closed with 500", path)
	}
}

// TestProjectMembersGroupRead_MyGroupsFilteredByProjectReadAccess verifies
// that GET /api/v1/users/me/groups omits a project members group when the
// caller does not have read access to the owning project.
func TestProjectMembersGroupRead_MyGroupsFilteredByProjectReadAccess(t *testing.T) {
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	// Add bob directly to projectA's members group without granting projectA read.
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroupA.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Alice (has projectA read) sees membersGroupA in /api/v1/users/me/groups.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/users/me/groups", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var aliceResp GroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&aliceResp))
	aliceIDs := make([]string, 0, len(aliceResp.Groups))
	for _, g := range aliceResp.Groups {
		aliceIDs = append(aliceIDs, g.ID)
	}
	assert.Contains(t, aliceIDs, membersGroupA.ID)

	// Bob (no projectA read) does not see membersGroupA in /api/v1/users/me/groups.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/users/me/groups", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var bobResp GroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&bobResp))
	bobIDs := make([]string, 0, len(bobResp.Groups))
	for _, g := range bobResp.Groups {
		bobIDs = append(bobIDs, g.ID)
	}
	assert.NotContains(t, bobIDs, membersGroupA.ID)
}
