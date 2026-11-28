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
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
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

	bobGroup := &store.Group{
		ID:        tid("bob-fault-group"),
		Name:      "Bob Fault Group",
		Slug:      "bob-fault-group",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGroup(ctx, bobGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    bobGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	bobProject := &store.Project{
		ID:        tid("bob-fault-project"),
		Name:      "Bob Fault Project",
		Slug:      "bob-fault-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, bobProject))
	srv.seedProjectCreatorMembership(ctx, bobProject)

	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)

	fault.Arm()

	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/groups?limit=100", nil},
		{http.MethodGet, "/api/v1/groups/" + membersGroupA.ID, nil},
		{http.MethodPatch, "/api/v1/groups/" + membersGroupA.ID, map[string]any{"description": "x"}},
		{http.MethodDelete, "/api/v1/groups/" + membersGroupA.ID, nil},
		{http.MethodGet, "/api/v1/groups/" + membersGroupA.ID + "/members", nil},
		{http.MethodPost, "/api/v1/groups/" + membersGroupA.ID + "/members", map[string]any{"memberType": "user", "memberId": bob.ID, "role": "member"}},
		{http.MethodGet, "/api/v1/groups/" + membersGroupA.ID + "/members/user/" + alice.ID, nil},
		{http.MethodDelete, "/api/v1/groups/" + membersGroupA.ID + "/members/user/" + alice.ID, nil},
		{http.MethodGet, "/api/v1/principals/group/" + membersGroupA.ID, nil},
		{http.MethodGet, "/api/v1/principals/user/" + alice.ID, nil},
		{http.MethodPost, "/api/v1/groups/" + bobGroup.ID + "/members", map[string]any{"memberType": "group", "memberId": membersGroupA.Slug, "role": "member"}},
		{http.MethodPost, "/api/v1/projects/" + bobProject.ID + "/members", map[string]any{"roleDefinitionId": viewerRole.ID, "principalType": "group", "principalId": membersGroupA.Slug}},
		{http.MethodPut, "/api/v1/projects/" + bobProject.ID + "/members/principals/group/" + membersGroupA.Slug, map[string]any{"roleDefinitionIds": []string{viewerRole.ID}}},
		{http.MethodPost, "/api/v1/admin/role-bindings", map[string]any{"roleDefinitionId": viewerRole.ID, "principalType": "group", "principalId": membersGroupA.Slug, "scopeType": "project", "scopeId": bobProject.ID}},
	} {
		rec := doRequestAsUser(t, srv, bob, tc.method, tc.path, tc.body)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "%s %s should fail closed with 500; body=%s", tc.method, tc.path, rec.Body.String())
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

func assertResponseMatchesMissing(t *testing.T, label string, hiddenRec, missingRec *httptest.ResponseRecorder, hiddenRef, missingRef string, forbiddenSubstrings ...string) {
	t.Helper()
	assert.Equal(t, missingRec.Code, hiddenRec.Code, "%s: status code must match nonexistent group (hidden=%s missing=%s)", label, hiddenRec.Body.String(), missingRec.Body.String())
	assert.Equal(t, missingRec.Header().Get("Content-Type"), hiddenRec.Header().Get("Content-Type"), "%s: Content-Type header must match", label)

	hiddenBody := hiddenRec.Body.String()
	missingBody := missingRec.Body.String()
	normHidden := hiddenBody
	normMissing := missingBody
	if hiddenRef != "" && missingRef != "" {
		normHidden = strings.ReplaceAll(normHidden, hiddenRef, "<ref>")
		normMissing = strings.ReplaceAll(normMissing, missingRef, "<ref>")
	}
	assert.Equal(t, normMissing, normHidden, "%s: response body must match nonexistent group", label)

	for _, s := range forbiddenSubstrings {
		if s == "" || s == hiddenRef {
			continue
		}
		assert.NotContains(t, hiddenBody, s, "%s: response body must not contain %q", label, s)
	}
}

// TestProjectMembersGroupRead_PrincipalsAndAgentGroups verifies that principal
// resolution and agent group endpoints filter project members groups by project
// read access.
func TestProjectMembersGroupRead_PrincipalsAndAgentGroups(t *testing.T) {
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := &store.User{
		ID:          tid("user-carol-princ"),
		Email:       "carol-princ@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalUser, carol.ID)

	hubAdmin := &store.User{
		ID:          tid("user-admin-princ"),
		Email:       "admin-princ@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	dirGroup := &store.Group{
		ID:        tid("dir-group-princ"),
		Name:      "Team Gamma",
		Slug:      "team-gamma",
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

	agentA := &store.Agent{
		ID:        tid("agent-princ-a"),
		Slug:      "agent-princ-a",
		Name:      "Agent A",
		ProjectID: projectA.ID,
		Phase:     string(state.PhaseRunning),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroupA.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentA.ID,
		Role:       store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    dirGroup.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentA.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// 1. GET /api/v1/principals/group/{id}
	missingGroupRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/principals/group/nonexistent-group-id", nil)
	require.Equal(t, http.StatusNotFound, missingGroupRec.Code)
	hiddenGroupRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/principals/group/"+membersGroupA.ID, nil)
	assertResponseMatchesMissing(t, "GET /api/v1/principals/group/{id}", hiddenGroupRec, missingGroupRec, "", "", projectA.Name, membersGroupA.Name)

	// Bob still resolves directory groups.
	dirRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/principals/group/"+dirGroup.ID, nil)
	assert.Equal(t, http.StatusOK, dirRec.Code, dirRec.Body.String())

	// Carol (project reader) and hubAdmin resolve membersGroupA.
	for _, tc := range []struct {
		name string
		user *store.User
	}{
		{"project reader", carol},
		{"hub admin", hubAdmin},
	} {
		rec := doRequestAsUser(t, srv, tc.user, http.MethodGet, "/api/v1/principals/group/"+membersGroupA.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tc.name, rec.Body.String())
		var resp PrincipalResolutionResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, membersGroupA.ID, resp.Principal.ID)
	}

	// 2. GET /api/v1/principals/user/{aliceID}
	bobUserRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/principals/user/"+alice.ID, nil)
	require.Equal(t, http.StatusOK, bobUserRec.Code, bobUserRec.Body.String())
	var bobUserResp PrincipalResolutionResponse
	require.NoError(t, json.NewDecoder(bobUserRec.Body).Decode(&bobUserResp))
	assert.NotContains(t, bobUserResp.DirectGroups, membersGroupA.ID)
	assert.NotContains(t, bobUserResp.EffectiveGroups, membersGroupA.ID)
	assert.Contains(t, bobUserResp.DirectGroups, dirGroup.ID)
	assert.Contains(t, bobUserResp.EffectiveGroups, dirGroup.ID)

	for _, tc := range []struct {
		name string
		user *store.User
	}{
		{"project reader", carol},
		{"hub admin", hubAdmin},
	} {
		rec := doRequestAsUser(t, srv, tc.user, http.MethodGet, "/api/v1/principals/user/"+alice.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tc.name, rec.Body.String())
		var resp PrincipalResolutionResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, resp.DirectGroups, membersGroupA.ID, "%s should see membersGroupA in directGroups", tc.name)
		assert.Contains(t, resp.EffectiveGroups, membersGroupA.ID, "%s should see membersGroupA in effectiveGroups", tc.name)
	}

	// 3. GET /api/v1/principals/agent/{agentA.ID}
	bobAgentPrincRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/principals/agent/"+agentA.ID, nil)
	require.Equal(t, http.StatusOK, bobAgentPrincRec.Code, bobAgentPrincRec.Body.String())
	var bobAgentPrincResp PrincipalResolutionResponse
	require.NoError(t, json.NewDecoder(bobAgentPrincRec.Body).Decode(&bobAgentPrincResp))
	assert.NotContains(t, bobAgentPrincResp.DirectGroups, membersGroupA.ID)
	assert.NotContains(t, bobAgentPrincResp.EffectiveGroups, membersGroupA.ID)
	assert.Contains(t, bobAgentPrincResp.EffectiveGroups, dirGroup.ID)

	for _, tc := range []struct {
		name string
		user *store.User
	}{
		{"project reader", carol},
		{"hub admin", hubAdmin},
	} {
		rec := doRequestAsUser(t, srv, tc.user, http.MethodGet, "/api/v1/principals/agent/"+agentA.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tc.name, rec.Body.String())
		var resp PrincipalResolutionResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, resp.EffectiveGroups, membersGroupA.ID, "%s should see membersGroupA in agent principal groups", tc.name)
	}

	// 4. GET /api/v1/agents/{agentA.ID}/groups
	// Agent endpoints require agent.read (project reader / admin), and if a caller
	// reaches handleAgentGroups without projectA read, membersGroupA is still omitted.
	carolAgentGroupsRec := doRequestAsUser(t, srv, carol, http.MethodGet, "/api/v1/agents/"+agentA.ID+"/groups", nil)
	require.Equal(t, http.StatusOK, carolAgentGroupsRec.Code, carolAgentGroupsRec.Body.String())
	var carolAgentGroupsResp GroupsResponse
	require.NoError(t, json.NewDecoder(carolAgentGroupsRec.Body).Decode(&carolAgentGroupsResp))
	carolAgentGroupIDs := make([]string, 0, len(carolAgentGroupsResp.Groups))
	for _, g := range carolAgentGroupsResp.Groups {
		carolAgentGroupIDs = append(carolAgentGroupIDs, g.ID)
	}
	assert.Contains(t, carolAgentGroupIDs, membersGroupA.ID)

	// An agent in bob's project that also holds a raw membership in projectA's members group:
	projectB := &store.Project{
		ID:        tid("project-bob-agent-groups"),
		Name:      "Bob Agent Project",
		Slug:      "bob-agent-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	srv.seedProjectCreatorMembership(ctx, projectB)
	agentB := &store.Agent{
		ID:        tid("agent-princ-b"),
		Slug:      "agent-princ-b",
		Name:      "Agent B",
		ProjectID: projectB.ID,
		Phase:     string(state.PhaseRunning),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroupA.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentB.ID,
		Role:       store.GroupMemberRoleMember,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    dirGroup.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentB.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	bobAgentGroupsRec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/agents/"+agentB.ID+"/groups", nil)
	require.Equal(t, http.StatusOK, bobAgentGroupsRec.Code, bobAgentGroupsRec.Body.String())
	var bobAgentGroupsResp GroupsResponse
	require.NoError(t, json.NewDecoder(bobAgentGroupsRec.Body).Decode(&bobAgentGroupsResp))
	bobAgentGroupIDs := make([]string, 0, len(bobAgentGroupsResp.Groups))
	for _, g := range bobAgentGroupsResp.Groups {
		bobAgentGroupIDs = append(bobAgentGroupIDs, g.ID)
	}
	assert.NotContains(t, bobAgentGroupIDs, membersGroupA.ID)
	assert.Contains(t, bobAgentGroupIDs, dirGroup.ID)
}

// TestProjectMembersGroupRead_MemberAddAndRoleBindingResolution verifies that
// member-add, project-member principal, role-binding, and child-group endpoints
// return the same not-found response for an unreadable project members group as
// for a nonexistent group.
func TestProjectMembersGroupRead_MemberAddAndRoleBindingResolution(t *testing.T) {
	srv, s, _, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := &store.User{
		ID:          tid("user-carol-madd"),
		Email:       "carol-madd@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalUser, carol.ID)

	hubAdmin := &store.User{
		ID:          tid("user-admin-madd"),
		Email:       "admin-madd@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	bobGroup := &store.Group{
		ID:        tid("bob-owned-group"),
		Name:      "Bob Group",
		Slug:      "bob-group",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGroup(ctx, bobGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    bobGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	carolGroup := &store.Group{
		ID:        tid("carol-owned-group"),
		Name:      "Carol Group",
		Slug:      "carol-group",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   carol.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGroup(ctx, carolGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    carolGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   carol.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	bobProject := &store.Project{
		ID:        tid("bob-owned-project"),
		Name:      "Bob Owned Project",
		Slug:      "bob-owned-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, bobProject))
	srv.seedProjectCreatorMembership(ctx, bobProject)
	grantProjectRoleForTest(t, s, bobProject.ID, store.ProjectRoleOwner, store.RoleBindingPrincipalUser, hubAdmin.ID)

	carolProject := &store.Project{
		ID:        tid("carol-owned-project"),
		Name:      "Carol Owned Project",
		Slug:      "carol-owned-project",
		OwnerID:   carol.ID,
		CreatedBy: carol.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, carolProject))
	srv.seedProjectCreatorMembership(ctx, carolProject)

	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	missingSlug := "project:does-not-exist:members"
	missingID := tid("nonexistent-members-group")

	for _, refPair := range []struct {
		kind       string
		hiddenRef  string
		missingRef string
	}{
		{"slug", membersGroupA.Slug, missingSlug},
		{"id", membersGroupA.ID, missingID},
	} {
		// 1. POST /api/v1/groups/{bobGroup}/members
		hiddenRec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/groups/"+bobGroup.ID+"/members", map[string]any{
			"memberType": "group",
			"memberId":   refPair.hiddenRef,
			"role":       "member",
		})
		missingRec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/groups/"+bobGroup.ID+"/members", map[string]any{
			"memberType": "group",
			"memberId":   refPair.missingRef,
			"role":       "member",
		})
		require.Equal(t, http.StatusBadRequest, missingRec.Code)
		assertResponseMatchesMissing(t, "POST group member ("+refPair.kind+")", hiddenRec, missingRec,
			refPair.hiddenRef, refPair.missingRef, membersGroupA.ID, projectMembersGroupDetailReason)

		// 2. POST /api/v1/projects/{bobProject}/members
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/projects/"+bobProject.ID+"/members", map[string]any{
			"roleDefinitionId": viewerRole.ID,
			"principalType":    "group",
			"principalId":      refPair.hiddenRef,
		})
		missingRec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/projects/"+bobProject.ID+"/members", map[string]any{
			"roleDefinitionId": viewerRole.ID,
			"principalType":    "group",
			"principalId":      refPair.missingRef,
		})
		require.Equal(t, http.StatusBadRequest, missingRec.Code)
		assertResponseMatchesMissing(t, "POST project member ("+refPair.kind+")", hiddenRec, missingRec,
			refPair.hiddenRef, refPair.missingRef, membersGroupA.ID, projectMembersGroupDetailReason, ErrCodePrincipalIneligible)

		// 3. PUT /api/v1/projects/{bobProject}/members/principals/group/{ref}
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodPut, "/api/v1/projects/"+bobProject.ID+"/members/principals/group/"+refPair.hiddenRef, map[string]any{
			"roleDefinitionIds": []string{viewerRole.ID},
		})
		missingRec = doRequestAsUser(t, srv, bob, http.MethodPut, "/api/v1/projects/"+bobProject.ID+"/members/principals/group/"+refPair.missingRef, map[string]any{
			"roleDefinitionIds": []string{viewerRole.ID},
		})
		require.Equal(t, http.StatusBadRequest, missingRec.Code)
		assertResponseMatchesMissing(t, "PUT project member principal ("+refPair.kind+")", hiddenRec, missingRec,
			refPair.hiddenRef, refPair.missingRef, membersGroupA.ID, projectMembersGroupDetailReason, ErrCodePrincipalIneligible)

		// 4. DELETE /api/v1/projects/{bobProject}/members/principals/group/{ref}
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/projects/"+bobProject.ID+"/members/principals/group/"+refPair.hiddenRef, nil)
		missingRec = doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/projects/"+bobProject.ID+"/members/principals/group/"+refPair.missingRef, nil)
		require.Equal(t, http.StatusNotFound, missingRec.Code)
		assertResponseMatchesMissing(t, "DELETE project member principal ("+refPair.kind+")", hiddenRec, missingRec,
			"", "", membersGroupA.ID, projectMembersGroupDetailReason)

		// 5. POST /api/v1/admin/role-bindings
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/admin/role-bindings", map[string]any{
			"roleDefinitionId": viewerRole.ID,
			"principalType":    "group",
			"principalId":      refPair.hiddenRef,
			"scopeType":        "project",
			"scopeId":          bobProject.ID,
		})
		missingRec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/admin/role-bindings", map[string]any{
			"roleDefinitionId": viewerRole.ID,
			"principalType":    "group",
			"principalId":      refPair.missingRef,
			"scopeType":        "project",
			"scopeId":          bobProject.ID,
		})
		require.Equal(t, http.StatusBadRequest, missingRec.Code)
		assertResponseMatchesMissing(t, "POST admin role-binding ("+refPair.kind+")", hiddenRec, missingRec,
			refPair.hiddenRef, refPair.missingRef, membersGroupA.ID, projectMembersGroupDetailReason, ErrCodePrincipalIneligible)
	}

	// Project reader (carol) and hubAdmin still receive the ineligible-principal refusal.
	carolAddRec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/groups/"+carolGroup.ID+"/members", map[string]any{
		"memberType": "group",
		"memberId":   membersGroupA.Slug,
		"role":       "member",
	})
	assert.Equal(t, http.StatusBadRequest, carolAddRec.Code)
	assert.Contains(t, carolAddRec.Body.String(), projectMembersGroupDetailReason)

	carolProjAddRec := doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/projects/"+carolProject.ID+"/members", map[string]any{
		"roleDefinitionId": viewerRole.ID,
		"principalType":    "group",
		"principalId":      membersGroupA.Slug,
	})
	assert.Equal(t, http.StatusBadRequest, carolProjAddRec.Code)
	assert.Contains(t, carolProjAddRec.Body.String(), ErrCodePrincipalIneligible)

	adminPutRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPut, "/api/v1/projects/"+bobProject.ID+"/members/principals/group/"+membersGroupA.Slug, map[string]any{
		"roleDefinitionIds": []string{viewerRole.ID},
	})
	assert.Equal(t, http.StatusBadRequest, adminPutRec.Code)
	assert.Contains(t, adminPutRec.Body.String(), ErrCodePrincipalIneligible)
}

// TestProjectMembersGroupRead_GroupMutationsReturnNotFound verifies that PATCH,
// DELETE, POST .../members, and DELETE .../members/{type}/{id} on a project
// members group return the identical 404 response as a nonexistent group when
// the caller cannot read the owning project.
func TestProjectMembersGroupRead_GroupMutationsReturnNotFound(t *testing.T) {
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := &store.User{
		ID:          tid("user-carol-mut"),
		Email:       "carol-mut@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalUser, carol.ID)

	hubAdmin := &store.User{
		ID:          tid("user-admin-mut"),
		Email:       "admin-mut@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	missingSlug := "project:does-not-exist:members"
	missingID := tid("nonexistent-mut-group")

	for _, refPair := range []struct {
		kind       string
		hiddenRef  string
		missingRef string
	}{
		{"slug", membersGroupA.Slug, missingSlug},
		{"id", membersGroupA.ID, missingID},
	} {
		// PATCH /api/v1/groups/{ref}
		hiddenRec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/groups/"+refPair.hiddenRef, map[string]any{"description": "updated"})
		missingRec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/groups/"+refPair.missingRef, map[string]any{"description": "updated"})
		require.Equal(t, http.StatusNotFound, missingRec.Code)
		assertResponseMatchesMissing(t, "PATCH group ("+refPair.kind+")", hiddenRec, missingRec, "", "", membersGroupA.ID, projectA.Name)

		// DELETE /api/v1/groups/{ref}
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/groups/"+refPair.hiddenRef, nil)
		missingRec = doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/groups/"+refPair.missingRef, nil)
		require.Equal(t, http.StatusNotFound, missingRec.Code)
		assertResponseMatchesMissing(t, "DELETE group ("+refPair.kind+")", hiddenRec, missingRec, "", "", membersGroupA.ID, projectA.Name)

		// POST /api/v1/groups/{ref}/members
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/groups/"+refPair.hiddenRef+"/members", map[string]any{
			"memberType": "user",
			"memberId":   bob.ID,
			"role":       "member",
		})
		missingRec = doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/groups/"+refPair.missingRef+"/members", map[string]any{
			"memberType": "user",
			"memberId":   bob.ID,
			"role":       "member",
		})
		require.Equal(t, http.StatusNotFound, missingRec.Code)
		assertResponseMatchesMissing(t, "POST group members ("+refPair.kind+")", hiddenRec, missingRec, "", "", membersGroupA.ID, projectA.Name)

		// DELETE /api/v1/groups/{ref}/members/user/{aliceID}
		hiddenRec = doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/groups/"+refPair.hiddenRef+"/members/user/"+alice.ID, nil)
		missingRec = doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/groups/"+refPair.missingRef+"/members/user/"+alice.ID, nil)
		require.Equal(t, http.StatusNotFound, missingRec.Code)
		assertResponseMatchesMissing(t, "DELETE group member ("+refPair.kind+")", hiddenRec, missingRec, "", "", membersGroupA.ID, projectA.Name)
	}

	// Project reader (carol) passes visibility and hits normal write authorization (403).
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, carol, http.MethodPatch, "/api/v1/groups/"+membersGroupA.Slug, map[string]any{"description": "updated"}).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, carol, http.MethodDelete, "/api/v1/groups/"+membersGroupA.Slug, nil).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, carol, http.MethodPost, "/api/v1/groups/"+membersGroupA.Slug+"/members", map[string]any{
		"memberType": "user",
		"memberId":   bob.ID,
		"role":       "member",
	}).Code)
	assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, carol, http.MethodDelete, "/api/v1/groups/"+membersGroupA.Slug+"/members/user/"+alice.ID, nil).Code)

	// Hub admin can PATCH and DELETE.
	patchRec := doRequestAsUser(t, srv, hubAdmin, http.MethodPatch, "/api/v1/groups/"+membersGroupA.Slug, map[string]any{"description": "admin updated"})
	assert.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())

	// Dedicated throwaway project for admin DELETE check.
	throwaway := &store.Project{
		ID:        tid("proj-throwaway-del"),
		Name:      "Throwaway Project",
		Slug:      "throwaway-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, throwaway))
	srv.seedProjectCreatorMembership(ctx, throwaway)
	throwawayGroup, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(throwaway.Slug))
	require.NoError(t, err)

	delRec := doRequestAsUser(t, srv, hubAdmin, http.MethodDelete, "/api/v1/groups/"+throwawayGroup.Slug, nil)
	assert.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())
}

// TestProjectMembersGroupRead_RouteCompleteness enumerates registered hub routes
// from routeMetadataTable and authzop.Catalog that touch groups, principals, or
// membership resolution, fails if any such route is unclassified, and exercises
// each classified group-visibility route across non-reader, project reader, and
// hub admin callers.
func TestProjectMembersGroupRead_RouteCompleteness(t *testing.T) {
	// 1. Classify every route in routeMetadataTable that touches groups,
	// principals, or role-bindings.
	classifiedMetadataRoutes := map[string]string{
		"/api/v1/groups":               "group-collection",
		"/api/v1/groups/":              "group-resource-and-members",
		"/api/v1/users/me/groups":      "user-self-groups",
		"/api/v1/principals/":          "principal-resolution",
		"/api/v1/agents/":              "agent-subroutes-includes-groups",
		"/api/v1/projects/":            "project-subroutes-includes-members",
		"/api/v1/admin/role-bindings":  "role-bindings-collection",
		"/api/v1/admin/role-bindings/": "role-bindings-by-id",
		"/api/v1/thread-groups":        "non-group:chat-thread-groups",
		"/api/v1/thread-groups/":       "non-group:chat-thread-groups",
	}

	var unclassifiedMetadata []string
	for pattern, meta := range routeMetadataTable {
		lower := strings.ToLower(pattern)
		touchesGroupSurface := meta.Resource == "group" ||
			strings.Contains(lower, "group") ||
			strings.Contains(lower, "principal") ||
			strings.Contains(lower, "role-binding") ||
			strings.Contains(lower, "member")
		if !touchesGroupSurface {
			continue
		}
		if _, ok := classifiedMetadataRoutes[pattern]; !ok {
			unclassifiedMetadata = append(unclassifiedMetadata, pattern)
		}
	}
	sort.Strings(unclassifiedMetadata)
	require.Empty(t, unclassifiedMetadata, "unclassified group/principal/member routes in routeMetadataTable: %v", unclassifiedMetadata)

	// 2. Classify every HTTP EntryPoint in authzop.Catalog that touches groups,
	// principals, project members, or role-bindings.
	classifiedCatalogEndpoints := map[string]string{
		"GET /api/v1/groups":                                                   "group-list",
		"POST /api/v1/groups":                                                  "group-create-parent-id",
		"GET /api/v1/groups/{id}":                                              "group-read",
		"PATCH /api/v1/groups/{id}":                                            "group-update",
		"DELETE /api/v1/groups/{id}":                                           "group-delete",
		"POST /api/v1/groups/{id}/members":                                     "group-member-add",
		"DELETE /api/v1/groups/{id}/members/{memberType}/{memberId}":           "group-member-remove",
		"GET /api/v1/groups/{id}/usage/budgets":                                "group-usage-budgets-read",
		"GET /api/v1/users/me/groups":                                          "user-self-groups",
		"GET /api/v1/projects/{id}/members":                                    "project-members-list",
		"POST /api/v1/projects/{id}/members":                                   "project-member-add",
		"PATCH /api/v1/projects/{id}/members/{memberId}":                       "project-member-update-by-binding-id",
		"DELETE /api/v1/projects/{id}/members/{memberId}":                      "project-member-delete-by-binding-id",
		"PUT /api/v1/projects/{id}/members/principals/{type}/{principalId}":    "project-member-principal-put",
		"DELETE /api/v1/projects/{id}/members/principals/{type}/{principalId}": "project-member-principal-delete",
		"GET /api/v1/projects/{id}/members/assignable-roles":                   "project-members-assignable-roles",
		"GET /api/v1/admin/role-bindings":                                      "admin-only:role-binding-read",
		"GET /api/v1/admin/role-bindings/user/{userId}":                        "admin-only:role-binding-read",
		"POST /api/v1/admin/role-bindings":                                     "role-binding-create",
		"DELETE /api/v1/admin/role-bindings/{id}":                              "role-binding-delete",
		"GET /api/v1/chat/spaces/{id}/members":                                 "non-group:chat-space-members",
	}

	var unclassifiedCatalog []string
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if ep.Kind != authzop.EntryPointHTTPRoute {
				continue
			}
			lower := strings.ToLower(ep.Pattern)
			touches := spec.Domain == "group" ||
				spec.ResourceResolver == "group-from-url" ||
				strings.Contains(lower, "/groups") ||
				strings.Contains(lower, "/principals") ||
				strings.Contains(lower, "/members") ||
				strings.Contains(lower, "/role-bindings")
			if !touches {
				continue
			}
			key := ep.Method + " " + ep.Pattern
			if _, ok := classifiedCatalogEndpoints[key]; !ok {
				unclassifiedCatalog = append(unclassifiedCatalog, key+" (op="+string(spec.ID)+")")
			}
		}
	}
	sort.Strings(unclassifiedCatalog)
	require.Empty(t, unclassifiedCatalog, "unclassified group/principal/member entry points in authzop.Catalog: %v", unclassifiedCatalog)

	// 3. Exercise every classified group-touching route against hidden vs missing
	// responses for a non-reader, plus project reader and hub admin callers.
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	carol := &store.User{
		ID:          tid("user-carol-comp"),
		Email:       "carol-comp@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)
	grantProjectRoleForTest(t, s, projectA.ID, store.ProjectRoleViewer, store.RoleBindingPrincipalUser, carol.ID)

	hubAdmin := &store.User{
		ID:          tid("user-admin-comp"),
		Email:       "admin-comp@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	// Caller with group.create but without projectA read, to exercise POST /api/v1/groups parentId.
	groupCreatorRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "custom-group-creator",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"group.create", "group.read", "group.list"},
	})
	require.NoError(t, err)
	bobGroupCreator := &store.User{
		ID:          tid("user-bob-gcreator"),
		Email:       "bob-gcreator@test.com",
		DisplayName: "Bob Creator",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bobGroupCreator))
	ensureHubMembership(ctx, s, bobGroupCreator.ID)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: groupCreatorRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      bobGroupCreator.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	bobGroup := &store.Group{
		ID:        tid("bob-comp-group"),
		Name:      "Bob Comp Group",
		Slug:      "bob-comp-group",
		GroupType: store.GroupTypeExplicit,
		OwnerID:   bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGroup(ctx, bobGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    bobGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	bobProject := &store.Project{
		ID:        tid("bob-comp-project"),
		Name:      "Bob Comp Project",
		Slug:      "bob-comp-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, bobProject))
	srv.seedProjectCreatorMembership(ctx, bobProject)
	grantProjectRoleForTest(t, s, bobProject.ID, store.ProjectRoleOwner, store.RoleBindingPrincipalUser, hubAdmin.ID)

	viewerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleViewer, store.RoleScopeProject)
	require.NoError(t, err)

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	missingSlug := "project:does-not-exist:members"
	missingID := tid("missing-comp-group")

	// POST /api/v1/groups with parentId=membersGroupA.ID for non-reader with group.create
	hiddenParentRec := doRequestAsUser(t, srv, bobGroupCreator, http.MethodPost, "/api/v1/groups", map[string]any{
		"name":     "Child Group Hidden",
		"parentId": membersGroupA.ID,
	})
	missingParentRec := doRequestAsUser(t, srv, bobGroupCreator, http.MethodPost, "/api/v1/groups", map[string]any{
		"name":     "Child Group Missing",
		"parentId": missingID,
	})
	require.Equal(t, http.StatusBadRequest, missingParentRec.Code)
	assertResponseMatchesMissing(t, "POST /api/v1/groups parentId", hiddenParentRec, missingParentRec,
		membersGroupA.ID, missingID, projectA.Name, membersGroupA.Name)

	type parityCase struct {
		name        string
		method      string
		hiddenPath  string
		missingPath string
		hiddenBody  any
		missingBody any
		hiddenRef   string
		missingRef  string
		carolStatus int
		adminStatus int
	}

	cases := []parityCase{
		{
			name:        "GET /api/v1/groups/{slug}",
			method:      http.MethodGet,
			hiddenPath:  "/api/v1/groups/" + membersGroupA.Slug,
			missingPath: "/api/v1/groups/" + missingSlug,
			carolStatus: http.StatusOK,
			adminStatus: http.StatusOK,
		},
		{
			name:        "PATCH /api/v1/groups/{slug}",
			method:      http.MethodPatch,
			hiddenPath:  "/api/v1/groups/" + membersGroupA.Slug,
			missingPath: "/api/v1/groups/" + missingSlug,
			hiddenBody:  map[string]any{"description": "probe"},
			missingBody: map[string]any{"description": "probe"},
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusOK,
		},
		{
			name:        "GET /api/v1/groups/{slug}/members",
			method:      http.MethodGet,
			hiddenPath:  "/api/v1/groups/" + membersGroupA.Slug + "/members",
			missingPath: "/api/v1/groups/" + missingSlug + "/members",
			carolStatus: http.StatusOK,
			adminStatus: http.StatusOK,
		},
		{
			name:        "POST /api/v1/groups/{slug}/members (target hidden group)",
			method:      http.MethodPost,
			hiddenPath:  "/api/v1/groups/" + membersGroupA.Slug + "/members",
			missingPath: "/api/v1/groups/" + missingSlug + "/members",
			hiddenBody:  map[string]any{"memberType": "user", "memberId": bob.ID, "role": "member"},
			missingBody: map[string]any{"memberType": "user", "memberId": bob.ID, "role": "member"},
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusCreated,
		},
		{
			name:        "GET /api/v1/groups/{slug}/members/user/{alice}",
			method:      http.MethodGet,
			hiddenPath:  "/api/v1/groups/" + membersGroupA.Slug + "/members/user/" + alice.ID,
			missingPath: "/api/v1/groups/" + missingSlug + "/members/user/" + alice.ID,
			carolStatus: http.StatusOK,
			adminStatus: http.StatusOK,
		},
		{
			name:        "DELETE /api/v1/groups/{slug}/members/user/{bob}",
			method:      http.MethodDelete,
			hiddenPath:  "/api/v1/groups/" + membersGroupA.Slug + "/members/user/" + bob.ID,
			missingPath: "/api/v1/groups/" + missingSlug + "/members/user/" + bob.ID,
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusNoContent,
		},
		{
			name:        "GET /api/v1/principals/group/{id}",
			method:      http.MethodGet,
			hiddenPath:  "/api/v1/principals/group/" + membersGroupA.ID,
			missingPath: "/api/v1/principals/group/" + missingID,
			carolStatus: http.StatusOK,
			adminStatus: http.StatusOK,
		},
		{
			name:        "POST /api/v1/groups/{bobGroup}/members (memberId hidden slug)",
			method:      http.MethodPost,
			hiddenPath:  "/api/v1/groups/" + bobGroup.ID + "/members",
			missingPath: "/api/v1/groups/" + bobGroup.ID + "/members",
			hiddenBody:  map[string]any{"memberType": "group", "memberId": membersGroupA.Slug, "role": "member"},
			missingBody: map[string]any{"memberType": "group", "memberId": missingSlug, "role": "member"},
			hiddenRef:   membersGroupA.Slug,
			missingRef:  missingSlug,
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusBadRequest,
		},
		{
			name:        "POST /api/v1/projects/{bobProject}/members (principalId hidden slug)",
			method:      http.MethodPost,
			hiddenPath:  "/api/v1/projects/" + bobProject.ID + "/members",
			missingPath: "/api/v1/projects/" + bobProject.ID + "/members",
			hiddenBody:  map[string]any{"roleDefinitionId": viewerRole.ID, "principalType": "group", "principalId": membersGroupA.Slug},
			missingBody: map[string]any{"roleDefinitionId": viewerRole.ID, "principalType": "group", "principalId": missingSlug},
			hiddenRef:   membersGroupA.Slug,
			missingRef:  missingSlug,
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusBadRequest,
		},
		{
			name:        "PUT /api/v1/projects/{bobProject}/members/principals/group/{slug}",
			method:      http.MethodPut,
			hiddenPath:  "/api/v1/projects/" + bobProject.ID + "/members/principals/group/" + membersGroupA.Slug,
			missingPath: "/api/v1/projects/" + bobProject.ID + "/members/principals/group/" + missingSlug,
			hiddenBody:  map[string]any{"roleDefinitionIds": []string{viewerRole.ID}},
			missingBody: map[string]any{"roleDefinitionIds": []string{viewerRole.ID}},
			hiddenRef:   membersGroupA.Slug,
			missingRef:  missingSlug,
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusBadRequest,
		},
		{
			name:        "DELETE /api/v1/projects/{bobProject}/members/principals/group/{slug}",
			method:      http.MethodDelete,
			hiddenPath:  "/api/v1/projects/" + bobProject.ID + "/members/principals/group/" + membersGroupA.Slug,
			missingPath: "/api/v1/projects/" + bobProject.ID + "/members/principals/group/" + missingSlug,
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusNotFound,
		},
		{
			name:        "POST /api/v1/admin/role-bindings (principalId hidden slug)",
			method:      http.MethodPost,
			hiddenPath:  "/api/v1/admin/role-bindings",
			missingPath: "/api/v1/admin/role-bindings",
			hiddenBody:  map[string]any{"roleDefinitionId": viewerRole.ID, "principalType": "group", "principalId": membersGroupA.Slug, "scopeType": "project", "scopeId": bobProject.ID},
			missingBody: map[string]any{"roleDefinitionId": viewerRole.ID, "principalType": "group", "principalId": missingSlug, "scopeType": "project", "scopeId": bobProject.ID},
			hiddenRef:   membersGroupA.Slug,
			missingRef:  missingSlug,
			carolStatus: http.StatusForbidden,
			adminStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hiddenRec := doRequestAsUser(t, srv, bob, tc.method, tc.hiddenPath, tc.hiddenBody)
			missingRec := doRequestAsUser(t, srv, bob, tc.method, tc.missingPath, tc.missingBody)
			assertResponseMatchesMissing(t, tc.name, hiddenRec, missingRec,
				tc.hiddenRef, tc.missingRef, membersGroupA.ID, projectA.Name, projectMembersGroupDetailReason)

			carolRec := doRequestAsUser(t, srv, carol, tc.method, tc.hiddenPath, tc.hiddenBody)
			assert.Equal(t, tc.carolStatus, carolRec.Code, "carol: %s", carolRec.Body.String())

			adminRec := doRequestAsUser(t, srv, hubAdmin, tc.method, tc.hiddenPath, tc.hiddenBody)
			assert.Equal(t, tc.adminStatus, adminRec.Code, "hubAdmin: %s", adminRec.Body.String())
		})
	}
}

// TestProjectMembersGroupRead_ExcludedProjectAndAgentIdentity verifies that a
// hub admin with an access constraint excluding a project does not see that
// project's members group, and an agent identity cannot see another project's
// members group.
func TestProjectMembersGroupRead_ExcludedProjectAndAgentIdentity(t *testing.T) {
	srv, s, alice, bob, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	hubAdmin := &store.User{
		ID:          tid("user-admin-excl"),
		Email:       "admin-excl@test.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, hubAdmin))
	ensureHubMembership(ctx, s, hubAdmin.ID)
	grantSystemRoleForTest(t, s, store.SystemRoleHubAdmin, hubAdmin.ID)

	membersGroupA, err := s.GetGroupBySlug(ctx, projectMembersGroupSlug(projectA.Slug))
	require.NoError(t, err)

	// Constrain hubAdmin so projectA is excluded.
	subjType := store.RoleBindingPrincipalUser
	subjID := hubAdmin.ID
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "exclude-project-a",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &subjType,
		SubjectPrincipalID:   &subjID,
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectA.ID,
		MaximumPermissions:   []string{},
		Purpose:              "test exclusion",
		CreatedBy:            alice.ID,
	})
	require.NoError(t, err)

	// Constrained hubAdmin falls back from hasAdminView and does not see membersGroupA.
	listRec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/groups?limit=100", nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	var listResp ListGroupsResponse
	require.NoError(t, json.NewDecoder(listRec.Body).Decode(&listResp))
	assert.NotContains(t, listGroupIDsFromResponse(listResp), membersGroupA.ID)

	getRec := doRequestAsUser(t, srv, hubAdmin, http.MethodGet, "/api/v1/groups/"+membersGroupA.ID, nil)
	assert.Equal(t, http.StatusNotFound, getRec.Code)

	// Agent identity in another project cannot read projectA's members group.
	projectB := &store.Project{
		ID:        tid("proj-agent-cross"),
		Name:      "Bob Cross Project",
		Slug:      "bob-cross-project",
		OwnerID:   bob.ID,
		CreatedBy: bob.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	srv.seedProjectCreatorMembership(ctx, projectB)

	agentB := &store.Agent{
		ID:        tid("agent-cross-b"),
		Slug:      "agent-cross-b",
		Name:      "Agent Cross B",
		ProjectID: projectB.ID,
		Phase:     string(state.PhaseRunning),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	agentIdent := &storedAgentIdentity{agent: agentB}
	visible, err := srv.isGroupVisibleToCaller(ctx, agentIdent, membersGroupA)
	require.NoError(t, err)
	assert.False(t, visible, "agent in projectB must not see projectA members group")
}
