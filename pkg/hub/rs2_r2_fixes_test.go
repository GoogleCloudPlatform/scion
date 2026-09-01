//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==========================================================================
// R2-3: Group-change cursor replay with transitive (nested) groups
// ==========================================================================

// TestRS2_TransitiveGroupAccess verifies that transitive group membership
// (user → group A → group B → project grant) provides list access through
// both endpoints, and that removing the user from group A revokes access.
func TestRS2_TransitiveGroupAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-tga-u"), Email: "r2-tga@test.com",
		DisplayName: "Transitive Group User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Group A (user is a direct member)
	groupA := &store.Group{ID: tid("r2-tga-ga"), Name: "group-a", Slug: "r2-tga-ga"}
	require.NoError(t, s.CreateGroup(ctx, groupA))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupA.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   user.ID,
		Role:       "member",
		AddedBy:    "test",
	}))

	// Group B (group A is a child of group B → B is parent of A)
	groupB := &store.Group{ID: tid("r2-tga-gb"), Name: "group-b", Slug: "r2-tga-gb"}
	require.NoError(t, s.CreateGroup(ctx, groupB))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupB.ID,
		MemberType: store.GroupMemberTypeGroup,
		MemberID:   groupA.ID,
		Role:       "member",
		AddedBy:    "test",
	}))

	// Project accessible only via transitive group B
	transitiveProj := &store.Project{
		ID: tid("r2-tga-tp"), Name: "Transitive Proj", Slug: "r2-tga-trans",
		OwnerID: tid("r2-tga-oth"), CreatedBy: tid("r2-tga-oth"),
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	// Control project with no access
	noAccessProj := &store.Project{
		ID: tid("r2-tga-np"), Name: "NoAccess Proj", Slug: "r2-tga-noacc",
		OwnerID: tid("r2-tga-oth"), CreatedBy: tid("r2-tga-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, transitiveProj))
	require.NoError(t, s.CreateProject(ctx, noAccessProj))

	// Grant group B (the parent) member access to the project
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupB.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          transitiveProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Agent in the transitive project for agent list testing
	transitiveAgent := &store.Agent{
		ID: tid("r2-tga-ta"), Slug: "r2-tga-ta", Name: "transitive-agent",
		ProjectID: transitiveProj.ID, OwnerID: tid("r2-tga-oth"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, transitiveAgent))

	t.Run("project_list_via_transitive_group", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, transitiveProj.ID,
			"user must see project via transitive group chain user→A→B→project")
		assert.NotContains(t, projectIDs, noAccessProj.ID,
			"user must not see project without any grant")
	})

	t.Run("agent_list_via_transitive_group", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		agentIDs := extractAgentIDs(resp.Agents)
		assert.Contains(t, agentIDs, transitiveAgent.ID,
			"user must see agent in transitively-granted project")
	})

	t.Run("group_removal_revokes_transitive_access", func(t *testing.T) {
		// Mint cursors for both endpoints before revocation
		recProjCursor := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=1", nil)
		require.Equal(t, http.StatusOK, recProjCursor.Code)
		var projPage ListProjectsResponse
		require.NoError(t, json.NewDecoder(recProjCursor.Body).Decode(&projPage))
		projCursor := projPage.NextCursor // may be empty if only 1 project

		recAgentCursor := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?limit=1", nil)
		require.Equal(t, http.StatusOK, recAgentCursor.Code)
		var agentPage ListAgentsResponse
		require.NoError(t, json.NewDecoder(recAgentCursor.Body).Decode(&agentPage))

		// Remove user from group A → breaks transitive chain
		require.NoError(t, s.RemoveGroupMember(ctx, groupA.ID, store.GroupMemberTypeUser, user.ID))

		// Project list: revoked
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var projResp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&projResp))
		projectIDs := extractProjectIDs(projResp.Projects)
		assert.NotContains(t, projectIDs, transitiveProj.ID,
			"user must lose transitively-granted project access after group removal")
		assert.Equal(t, 0, projResp.TotalCount,
			"project totalCount must be zero after losing all grants")

		// Agent list: revoked (same scope resolution path)
		recAg := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, recAg.Code)
		var agentResp ListAgentsResponse
		require.NoError(t, json.NewDecoder(recAg.Body).Decode(&agentResp))
		agentIDs := extractAgentIDs(agentResp.Agents)
		assert.NotContains(t, agentIDs, transitiveAgent.ID,
			"user must lose transitively-granted agent access after group removal")
		assert.Equal(t, 0, agentResp.TotalCount,
			"agent totalCount must be zero after losing all grants")

		// Prior project cursor replay: rejected or empty (None scope → 200 empty)
		if projCursor != "" {
			recCursorReplay := doRequestAsUser(t, srv, user, http.MethodGet,
				"/api/v1/projects?limit=1&cursor="+projCursor, nil)
			if recCursorReplay.Code == http.StatusOK {
				var emptyResp ListProjectsResponse
				require.NoError(t, json.NewDecoder(recCursorReplay.Body).Decode(&emptyResp))
				assert.Equal(t, 0, emptyResp.TotalCount,
					"project cursor replay after revocation must return zero results")
			} else {
				assert.Equal(t, http.StatusBadRequest, recCursorReplay.Code,
					"project cursor must be rejected after transitive group revocation")
			}
		}

		// Prior agent cursor replay: replay the actual captured cursor.
		// After revocation, scope is None → handler returns 200 empty without
		// reaching cursor validation (short-circuits). Either 400 rejection or
		// 200 with zero results, empty array, and no next cursor are accepted.
		agentCursorURL := "/api/v1/agents?limit=1"
		if agentPage.NextCursor != "" {
			agentCursorURL += "&cursor=" + agentPage.NextCursor
		}
		recAgCursorReplay := doRequestAsUser(t, srv, user, http.MethodGet, agentCursorURL, nil)
		if recAgCursorReplay.Code == http.StatusOK {
			var agCursorResp ListAgentsResponse
			require.NoError(t, json.NewDecoder(recAgCursorReplay.Body).Decode(&agCursorResp))
			assert.Equal(t, 0, agCursorResp.TotalCount,
				"agent cursor replay after revocation must return zero total")
			assert.Empty(t, agCursorResp.Agents,
				"agent cursor replay after revocation must return empty array")
			assert.Empty(t, agCursorResp.NextCursor,
				"agent cursor replay after revocation must have no next cursor")
		} else {
			assert.Equal(t, http.StatusBadRequest, recAgCursorReplay.Code,
				"agent cursor must be rejected after transitive group revocation")
		}
	})
}

// TestRS2_GroupChangeCursorReplay mints a cursor through group-derived access,
// then removes the user from the group and replays. The cursor must be rejected
// because the principal closure changed → different bindings → different scope hash.
func TestRS2_GroupChangeCursorReplay(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-gcc-u"), Email: "r2-gcc@test.com",
		DisplayName: "Group Cursor User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Direct group
	group := &store.Group{ID: tid("r2-gcc-g"), Name: "cursor-group", Slug: "r2-gcc-g"}
	require.NoError(t, s.CreateGroup(ctx, group))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   user.ID,
		Role:       "member",
		AddedBy:    "test",
	}))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Create 4 projects, all granted via the group
	for i := 0; i < 4; i++ {
		p := &store.Project{
			ID: tid(fmt.Sprintf("r2-gcc-p%d", i)), Name: fmt.Sprintf("GCC Proj %d", i),
			Slug: fmt.Sprintf("r2-gcc-%d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      group.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          p.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	// Mint cursor with group-derived access
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var page1 ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
	require.NotEmpty(t, page1.NextCursor)
	assert.Equal(t, 4, page1.TotalCount, "all 4 group-granted projects visible before removal")

	// Remove user from group → principal closure changes → scope hash changes.
	// If the user had other grants, cursor validation would see a different hash
	// and return 400. When ALL group-derived grants are removed, scope becomes
	// None and the handler returns 200 with empty list (no store query or cursor
	// check issued). Both outcomes protect against data leak.
	require.NoError(t, s.RemoveGroupMember(ctx, group.ID, store.GroupMemberTypeUser, user.ID))

	// Replay cursor → rejected (400) or empty (200 with no data)
	rec = doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/projects?limit=2&cursor="+page1.NextCursor, nil)
	if rec.Code == http.StatusOK {
		// None scope → empty list returned before cursor check
		var emptyResp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&emptyResp))
		assert.Equal(t, 0, emptyResp.TotalCount,
			"after group removal with None scope, must return zero results (no data leak)")
		assert.Empty(t, emptyResp.Projects)
	} else {
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"cursor must be rejected when scope hash changed")
	}

	t.Run("transitive_group_cursor_replay", func(t *testing.T) {
		// Re-add user to group for transitive test
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    group.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   user.ID,
			Role:       "member",
			AddedBy:    "test",
		}))

		// Create a parent group and nest group into it
		parentGroup := &store.Group{ID: tid("r2-gcc-pg"), Name: "parent-group", Slug: "r2-gcc-pg"}
		require.NoError(t, s.CreateGroup(ctx, parentGroup))
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    parentGroup.ID,
			MemberType: store.GroupMemberTypeGroup,
			MemberID:   group.ID,
			Role:       "member",
			AddedBy:    "test",
		}))

		// Grant parent group access to a new project
		extraProj := &store.Project{
			ID: tid("r2-gcc-ep"), Name: "Extra Proj", Slug: "r2-gcc-extra",
			OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(-5 * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, extraProj))
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalGroup,
			PrincipalID:      parentGroup.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          extraProj.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)

		// Mint cursor with transitive access
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		require.NotEmpty(t, resp.NextCursor)
		require.GreaterOrEqual(t, resp.TotalCount, 5,
			"should see 4 via direct group + 1 via transitive parent group")

		// Remove child group from parent → breaks transitive chain
		require.NoError(t, s.RemoveGroupMember(ctx, parentGroup.ID, store.GroupMemberTypeGroup, group.ID))

		// Replay cursor → must be rejected (transitive group removal)
		rec = doRequestAsUser(t, srv, user, http.MethodGet,
			"/api/v1/projects?limit=2&cursor="+resp.NextCursor, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"cursor must be rejected after transitive group removal")
	})
}

// ==========================================================================
// R2-4: Constraint-change cursor replay
// ==========================================================================

// TestRS2_ConstraintChangeCursorReplay mints a cursor under All scope before
// a relevant project constraint exists, then creates the constraint and
// replays. The cursor must be rejected because ExcludedProjectIDs changed.
func TestRS2_ConstraintChangeCursorReplay(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := &store.User{
		ID: tid("r2-ccr-u"), Email: "r2-ccr@test.com",
		DisplayName: "Constraint Cursor User", Role: store.UserRoleAdmin, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	// Create 4 projects
	var projectIDs []string
	for i := 0; i < 4; i++ {
		p := &store.Project{
			ID: tid(fmt.Sprintf("r2-ccr-p%d", i)), Name: fmt.Sprintf("CCR Proj %d", i),
			Slug: fmt.Sprintf("r2-ccr-%d", i), OwnerID: admin.ID, CreatedBy: admin.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		projectIDs = append(projectIDs, p.ID)
	}

	// Create agents in projects for agent endpoint testing
	for i := 0; i < 4; i++ {
		ag := &store.Agent{
			ID: tid(fmt.Sprintf("r2-ccr-a%d", i)), Slug: fmt.Sprintf("r2-ccr-a%d", i),
			Name: fmt.Sprintf("ccr-agent-%d", i), ProjectID: projectIDs[i],
			OwnerID: admin.ID, Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
	}

	t.Run("project_cursor_rejected_after_constraint", func(t *testing.T) {
		// Mint cursor before constraint
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects?limit=2", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var page1 ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
		require.NotEmpty(t, page1.NextCursor)
		preCTotal := page1.TotalCount
		require.GreaterOrEqual(t, preCTotal, 4, "admin should see all projects before constraint")

		// Create a constraint blocking project.list for project 0
		pType := "user"
		_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
			Name:                 "block-p0-list",
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: &pType,
			SubjectPrincipalID:   strPtr(admin.ID),
			ScopeType:            "project",
			ScopeID:              projectIDs[0],
			MaximumPermissions:   []string{"agent.read"}, // omits project.list
			Purpose:              "test constraint cursor",
			CreatedBy:            admin.ID,
		})
		require.NoError(t, err)

		// Confirm constraint reduced scope
		recCheck := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects?limit=10", nil)
		require.Equal(t, http.StatusOK, recCheck.Code)
		var checkResp ListProjectsResponse
		require.NoError(t, json.NewDecoder(recCheck.Body).Decode(&checkResp))
		assert.Less(t, checkResp.TotalCount, preCTotal,
			"constraint must reduce visible project count")

		// Replay stale cursor → must be rejected
		rec = doRequestAsUser(t, srv, admin, http.MethodGet,
			"/api/v1/projects?limit=2&cursor="+page1.NextCursor, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"cursor must be rejected after constraint changes ExcludedProjectIDs")
	})

	t.Run("agent_cursor_rejected_after_constraint", func(t *testing.T) {
		// Mint cursor before constraint on project 1
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/agents?limit=2", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var page1 ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
		require.NotEmpty(t, page1.NextCursor)

		// Create a constraint blocking agent.list for project 1
		pType := "user"
		_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
			Name:                 "block-p1-agent-list",
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: &pType,
			SubjectPrincipalID:   strPtr(admin.ID),
			ScopeType:            "project",
			ScopeID:              projectIDs[1],
			MaximumPermissions:   []string{"project.read"}, // omits agent.list
			Purpose:              "test agent constraint cursor",
			CreatedBy:            admin.ID,
		})
		require.NoError(t, err)

		// Replay stale cursor → must be rejected
		rec = doRequestAsUser(t, srv, admin, http.MethodGet,
			"/api/v1/agents?limit=2&cursor="+page1.NextCursor, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"agent cursor must be rejected after constraint changes ExcludedProjectIDs")
	})
}

// ==========================================================================
// R2-5: Suspension cursor replay through real auth middleware
// ==========================================================================

// TestRS2_SuspensionCursorReplay mints a cursor with an active user, suspends
// the user, then replays through the real auth middleware. The suspended user
// must be denied at the middleware layer — no list data returned.
func TestRS2_SuspensionCursorReplay(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-scr-u"), Email: "r2-scr@test.com",
		DisplayName: "Suspension Cursor User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	for i := 0; i < 4; i++ {
		p := &store.Project{
			ID: tid(fmt.Sprintf("r2-scr-p%d", i)), Name: fmt.Sprintf("SCR Proj %d", i),
			Slug: fmt.Sprintf("r2-scr-%d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          p.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	// Mint cursor with active user
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var page1 ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
	require.NotEmpty(t, page1.NextCursor)
	assert.Equal(t, 4, page1.TotalCount)

	// Suspend the user
	user.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, user))

	// Replay through real auth middleware (doRequestAsUser generates a JWT
	// and goes through the full HTTP handler stack including auth middleware).
	// Suspended user is denied at the middleware layer with a deterministic 403
	// and error code "user_suspended" (see auth.go UnifiedAuthMiddleware).
	rec = doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/projects?limit=2&cursor="+page1.NextCursor, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"suspended user must receive exactly 403 from auth middleware")

	// Verify the exact error code in the JSON response
	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&errResp),
		"suspended user response must be valid JSON")
	assert.Equal(t, "user_suspended", errResp.Error.Code,
		"suspended user must receive 'user_suspended' error code")
	assert.Contains(t, errResp.Error.Message, "suspended",
		"suspended user error message must mention suspension")
}

// ==========================================================================
// R2-6: Credential change/scope replay with real scoped UAT
// ==========================================================================

// TestRS2_CredentialChangeCursorReplay mints a cursor with one scoped UAT,
// then replays with a different credential (different UAT for the same user).
// The cursor must be rejected because the credential/scope context differs.
func TestRS2_CredentialChangeCursorReplay(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-ccs-u"), Email: "r2-ccs@test.com",
		DisplayName: "Credential Cursor User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	projA := &store.Project{
		ID: tid("r2-ccs-pa"), Name: "UAT Proj A", Slug: "r2-ccs-a",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now().Add(-3 * time.Hour), Updated: time.Now(),
	}
	projB := &store.Project{
		ID: tid("r2-ccs-pb"), Name: "UAT Proj B", Slug: "r2-ccs-b",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))

	for _, pid := range []string{projA.ID, projB.ID} {
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          pid,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	// Mint UAT scoped to project A
	uatA := mintScopedUAT(t, srv, user.ID, projA.ID, []string{"project:manage"})

	// Get page 1 with UAT A — should see only project A (scoped)
	rec := doRequestWithUAT(t, srv, uatA, http.MethodGet, "/api/v1/projects", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var respA ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&respA))
	assert.Equal(t, 1, respA.TotalCount, "UAT A should see only scoped project A")

	// Now test with multiple projects to get a cursor
	// Create additional projects in projA scope for pagination
	for i := 0; i < 3; i++ {
		subP := &store.Project{
			ID: tid(fmt.Sprintf("r2-ccs-s%d", i)), Name: fmt.Sprintf("Sub Proj %d", i),
			Slug: fmt.Sprintf("r2-ccs-s%d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-10+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, subP))
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          subP.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	t.Run("uat_to_uat_same_scope_different_credential_id", func(t *testing.T) {
		// Create 3 agents in projA so both UATs can paginate the agent endpoint
		for i := 0; i < 3; i++ {
			ag := &store.Agent{
				ID: tid(fmt.Sprintf("r2-ccs-ag%d", i)), Slug: fmt.Sprintf("r2-ccs-ag%d", i),
				Name: fmt.Sprintf("ccs-agent-%d", i), ProjectID: projA.ID,
				OwnerID: user.ID, Created: time.Now(), Updated: time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, ag))
		}

		// Mint two UATs for the same user, same project, same scope.
		// Each CreateToken call generates a distinct credential record with a unique ID.
		uatA1 := mintScopedUAT(t, srv, user.ID, projA.ID, []string{"project:manage"})
		uatA2 := mintScopedUAT(t, srv, user.ID, projA.ID, []string{"project:manage"})

		// Both UATs see projA's agents
		recA1 := doRequestWithUAT(t, srv, uatA1, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, recA1.Code)
		var respA1 ListAgentsResponse
		require.NoError(t, json.NewDecoder(recA1.Body).Decode(&respA1))
		require.GreaterOrEqual(t, respA1.TotalCount, 3,
			"UAT A1 must see at least 3 agents in projA")

		recA2 := doRequestWithUAT(t, srv, uatA2, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, recA2.Code)
		var respA2 ListAgentsResponse
		require.NoError(t, json.NewDecoder(recA2.Body).Decode(&respA2))
		require.GreaterOrEqual(t, respA2.TotalCount, 3,
			"UAT A2 must see at least 3 agents in projA")

		// Mint cursor with UAT A1 (limit=1 → cursor produced)
		recCursor := doRequestWithUAT(t, srv, uatA1, http.MethodGet,
			"/api/v1/agents?limit=1", nil)
		require.Equal(t, http.StatusOK, recCursor.Code)
		var page1 ListAgentsResponse
		require.NoError(t, json.NewDecoder(recCursor.Body).Decode(&page1))
		require.NotEmpty(t, page1.NextCursor,
			"UAT A1 must produce a cursor when limit=1 and 3+ agents exist")

		// Prove UAT A1 can replay its own cursor
		recReplay := doRequestWithUAT(t, srv, uatA1, http.MethodGet,
			"/api/v1/agents?limit=1&cursor="+page1.NextCursor, nil)
		require.Equal(t, http.StatusOK, recReplay.Code,
			"UAT A1 must successfully replay its own cursor")

		// Replay with UAT A2 (same user, same scope, different credential ID) → 400
		recCross := doRequestWithUAT(t, srv, uatA2, http.MethodGet,
			"/api/v1/agents?limit=1&cursor="+page1.NextCursor, nil)
		assert.Equal(t, http.StatusBadRequest, recCross.Code,
			"cursor from UAT A1 must be rejected when replayed with UAT A2 "+
				"(same user+scope, different credential ID)")
	})

	t.Run("same_scope_different_credential_rejected", func(t *testing.T) {
		// Mint with session JWT (full scope)
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var page1 ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
		if page1.NextCursor == "" {
			t.Skip("not enough projects for pagination")
		}

		// Replay with a UAT scoped to projA — different credential identity
		rec = doRequestWithUAT(t, srv, uatA, http.MethodGet,
			"/api/v1/projects?limit=2&cursor="+page1.NextCursor, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"cursor minted with session JWT must be rejected when replayed with UAT")
	})

	t.Run("different_scope_uat_rejected", func(t *testing.T) {
		// Mint with UAT scoped to projA
		rec := doRequestWithUAT(t, srv, uatA, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var respA2 ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&respA2))
		// UAT A sees only projA, no cursor needed — but verify scope is different
		// by using session JWT which sees more

		// Mint a different UAT scoped to projB
		uatB := mintScopedUAT(t, srv, user.ID, projB.ID, []string{"project:manage"})

		// Even without pagination, the cursor binding includes credential context.
		// Use session JWT to get a cursor, then replay with UAT B.
		rec = doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var sessionPage ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&sessionPage))
		if sessionPage.NextCursor == "" {
			t.Skip("not enough projects for pagination")
		}

		rec = doRequestWithUAT(t, srv, uatB, http.MethodGet,
			"/api/v1/projects?limit=2&cursor="+sessionPage.NextCursor, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"cursor minted with session must be rejected with different-scope UAT")
	})
}

// ==========================================================================
// R2-7: Production-real agent JWT through production middleware
// ==========================================================================

// TestRS2_ProductionAgentJWT generates a real agent token through the production
// token service, sends it through the full HTTP handler stack (including auth
// middleware), and verifies Mine/Shared/no-scope list semantics.
func TestRS2_ProductionAgentJWT(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-paj-u"), Email: "r2-paj@test.com",
		DisplayName: "Prod JWT User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Create two projects
	projA := &store.Project{
		ID: tid("r2-paj-pa"), Name: "JWT Proj A", Slug: "r2-paj-a",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	projB := &store.Project{
		ID: tid("r2-paj-pb"), Name: "JWT Proj B", Slug: "r2-paj-b",
		OwnerID: tid("r2-paj-oth"), CreatedBy: tid("r2-paj-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Create agents in both projects
	agentA := &store.Agent{
		ID: tid("r2-paj-aa"), Slug: "r2-paj-aa", Name: "jwt-agent-a",
		ProjectID: projA.ID, OwnerID: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	agentB := &store.Agent{
		ID: tid("r2-paj-ab"), Slug: "r2-paj-ab", Name: "jwt-agent-b",
		ProjectID: projB.ID, OwnerID: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create a group for projA and add agentA to it so scope resolution
	// produces Explicit scope for the agent's project. In production,
	// project creation creates a project_agents group automatically;
	// here we use a regular group to avoid the project_agents guard.
	agentGroup := &store.Group{
		ID:   tid("r2-paj-ag"),
		Name: "r2-paj-a-agents",
		Slug: "r2-paj-a-agents",
	}
	require.NoError(t, s.CreateGroup(ctx, agentGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    agentGroup.ID,
		MemberType: store.GroupMemberTypeAgent,
		MemberID:   agentA.ID,
		Role:       "member",
		AddedBy:    "test",
	}))

	// Give the agent group a member binding on projA so the agent has scope.
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      agentGroup.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projA.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Generate a real agent token through the production token service
	// Agent A is scoped to project A with ScopeProjectRead (needed for list).
	tokenA, err := srv.agentTokenService.GenerateAgentToken(
		agentA.ID, projA.ID,
		[]AgentTokenScope{ScopeProjectRead, ScopeAgentStatusUpdate},
		nil, // no ancestry
	)
	require.NoError(t, err)

	t.Run("no_scope_project_caveated", func(t *testing.T) {
		// Agent token without scope filter → sees only its own project
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects", nil, tokenA)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, projA.ID,
			"agent should see its own project")
		assert.NotContains(t, projectIDs, projB.ID,
			"agent JWT caveated to projA must not see projB")
	})

	t.Run("mine_empty_for_agent", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects?scope=mine", nil, tokenA)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 0, resp.TotalCount,
			"Mine must be empty for agent JWT — agents cannot hold owner bindings")
	})

	t.Run("shared_returns_project_agents", func(t *testing.T) {
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents?scope=shared", nil, tokenA)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		// Should see agentA (in projA), not agentB (in projB — outside caveat)
		agentIDs := extractAgentIDs(resp.Agents)
		assert.Contains(t, agentIDs, agentA.ID,
			"agent should see agents in its own project via Shared")
		assert.NotContains(t, agentIDs, agentB.ID,
			"agent must not see agents in other projects")
	})

	t.Run("cross_project_absent", func(t *testing.T) {
		// Filter by projB explicitly — agent caveated to projA must get empty
		rec := doRequestWithAgentToken(t, srv, http.MethodGet,
			"/api/v1/agents?projectId="+projB.ID, nil, tokenA)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 0, resp.TotalCount,
			"agent caveated to projA must not see projB agents")
	})

	t.Run("cross_agent_credential_cursor_rejected", func(t *testing.T) {
		// Create another agent in projA with a different token and group membership
		agentA2 := &store.Agent{
			ID: tid("r2-paj-a2"), Slug: "r2-paj-a2", Name: "jwt-agent-a2",
			ProjectID: projA.ID, OwnerID: user.ID,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, agentA2))

		// Add agentA2 to the same group so it also has scope (otherwise
		// scope is None and cursor validation is never reached)
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    agentGroup.ID,
			MemberType: store.GroupMemberTypeAgent,
			MemberID:   agentA2.ID,
			Role:       "member",
			AddedBy:    "test",
		}))

		tokenA2, err := srv.agentTokenService.GenerateAgentToken(
			agentA2.ID, projA.ID,
			[]AgentTokenScope{ScopeProjectRead, ScopeAgentStatusUpdate},
			nil,
		)
		require.NoError(t, err)

		// Mint cursor with agent A's token
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents?limit=1", nil, tokenA)
		require.Equal(t, http.StatusOK, rec.Code)
		var page1 ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
		if page1.NextCursor == "" {
			t.Skip("not enough agents for pagination with limit=1")
		}

		// Replay with agent A2's token → different identity → rejected
		rec = doRequestWithAgentToken(t, srv, http.MethodGet,
			"/api/v1/agents?limit=1&cursor="+page1.NextCursor, nil, tokenA2)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"cursor minted with agentA token must be rejected when replayed with agentA2 token")
	})
}

// ==========================================================================
// R2-8: Transferred/stale ownership exercise
// ==========================================================================

// TestRS2_TransferredOwnership uses the production transfer endpoint
// POST /api/v1/projects/{id}/transfer-ownership to atomically transfer
// ownership from UserA to UserB. It verifies Mine/Shared classification
// before and after the transfer for both project and agent endpoints,
// proving that classification follows active RoleBindings (not legacy
// Project.OwnerID which remains stale after transfer). Also tests mine=true.
func TestRS2_TransferredOwnership(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userA := &store.User{
		ID: tid("r2-txo-ua"), Email: "r2-txo-a@test.com",
		DisplayName: "Original Owner", Role: store.UserRoleMember, Status: "active",
	}
	userB := &store.User{
		ID: tid("r2-txo-ub"), Email: "r2-txo-b@test.com",
		DisplayName: "New Owner", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, userA))
	require.NoError(t, s.CreateUser(ctx, userB))
	ensureHubMembership(ctx, s, userA.ID)
	ensureHubMembership(ctx, s, userB.ID)

	// Create a project with OwnerID = userA and an owner RoleBinding for userA.
	// This is the pre-transfer state: userA is the legitimate owner.
	proj := &store.Project{
		ID: tid("r2-txo-p"), Name: "Transferred Proj", Slug: "r2-txo-proj",
		OwnerID: userA.ID, CreatedBy: userA.ID,
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, proj.ID, userA.ID))

	// Give userB a member binding so they can see the project (needed as transfer target)
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userB.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create an agent in the project
	ag := &store.Agent{
		ID: tid("r2-txo-a"), Slug: "r2-txo-a", Name: "txo-agent",
		ProjectID: proj.ID, OwnerID: userA.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	// ---- BEFORE TRANSFER: verify baseline Mine/Shared ----

	t.Run("before_transfer_userA_mine_includes_project", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, userA, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, extractProjectIDs(resp.Projects), proj.ID,
			"before transfer: Mine for userA (owner) must include project")
	})

	t.Run("before_transfer_userB_mine_excludes_project", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, userB, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.NotContains(t, extractProjectIDs(resp.Projects), proj.ID,
			"before transfer: Mine for userB (member) must NOT include project")
	})

	// ---- EXECUTE TRANSFER via production endpoint ----

	t.Run("transfer_via_production_endpoint", func(t *testing.T) {
		// POST /api/v1/projects/{id}/transfer-ownership
		transferBody := map[string]string{"newOwnerId": userB.ID}
		rec := doRequestAsUser(t, srv, userA, http.MethodPost,
			"/api/v1/projects/"+proj.ID+"/transfer-ownership", transferBody)
		require.Equal(t, http.StatusOK, rec.Code,
			"transfer-ownership must succeed; body: %s", rec.Body.String())
	})

	// ---- AFTER TRANSFER: verify Mine/Shared classification follows RoleBindings ----

	// Verify Project.OwnerID is STILL userA (stale metadata; transfer only changes RoleBindings)
	updatedProj, err := s.GetProject(ctx, proj.ID)
	require.NoError(t, err)
	assert.Equal(t, userA.ID, updatedProj.OwnerID,
		"after transfer: Project.OwnerID must remain stale (userA)")

	t.Run("after_transfer_userB_mine_includes_project", func(t *testing.T) {
		// UserB (new owner via transfer) — Mine must include project
		rec := doRequestAsUser(t, srv, userB, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, proj.ID,
			"after transfer: Mine for userB (new owner via RoleBinding) must include project")
		assert.Equal(t, 1, resp.TotalCount,
			"after transfer: userB Mine must have exactly 1 project")
	})

	t.Run("after_transfer_userA_mine_excludes_project", func(t *testing.T) {
		// UserA (downgraded to member by transfer, legacy OwnerID still set) — Mine must NOT include
		rec := doRequestAsUser(t, srv, userA, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		projectIDs := extractProjectIDs(resp.Projects)
		assert.NotContains(t, projectIDs, proj.ID,
			"after transfer: Mine for userA (stale OwnerID, now member) must NOT include project")
	})

	t.Run("after_transfer_mine_true_alias", func(t *testing.T) {
		// Verify mine=true works as alias of scope=mine after transfer
		rec := doRequestAsUser(t, srv, userB, http.MethodGet, "/api/v1/projects?mine=true", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, extractProjectIDs(resp.Projects), proj.ID,
			"mine=true must work as alias of scope=mine after transfer")
	})

	t.Run("after_transfer_shared_correct_projects", func(t *testing.T) {
		// UserA should see project in Shared (member after transfer)
		rec := doRequestAsUser(t, srv, userA, http.MethodGet, "/api/v1/projects?scope=shared", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, extractProjectIDs(resp.Projects), proj.ID,
			"after transfer: Shared for userA must include project (has member binding)")
	})

	t.Run("after_transfer_mine_agents_userB", func(t *testing.T) {
		// UserB (new owner) — agent Mine should include agents in owned project
		rec := doRequestAsUser(t, srv, userB, http.MethodGet, "/api/v1/agents?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, extractAgentIDs(resp.Agents), ag.ID,
			"after transfer: Mine agents for userB (new owner) must include agent")
	})

	t.Run("after_transfer_shared_agents_userA", func(t *testing.T) {
		// UserA (member) — agent Shared should include agents in the project
		rec := doRequestAsUser(t, srv, userA, http.MethodGet, "/api/v1/agents?scope=shared", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Contains(t, extractAgentIDs(resp.Agents), ag.ID,
			"after transfer: Shared agents for userA must include agent (has member binding)")
	})
}

// ==========================================================================
// R2-10: Filter composition with table-driven matrix
// ==========================================================================

// TestRS2_FilterCompositionMatrix verifies that authorization predicates
// compose correctly with representative caller filters. Each test case
// applies a filter parameter and asserts the authorization intersection
// preserves exact totals and correct row content.
func TestRS2_FilterCompositionMatrix(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-fcm-u"), Email: "r2-fcm@test.com",
		DisplayName: "Filter Matrix User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Create a RuntimeBroker for filter testing
	brokerID := tid("r2-fcm-br")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "test-broker", Slug: "r2-fcm-br",
		Status: "online", ConnectionState: "connected",
	}))

	// Create authorized project A (with a broker)
	projA := &store.Project{
		ID: tid("r2-fcm-pa"), Name: "FilterProjA", Slug: "r2-fcm-a",
		OwnerID: user.ID, CreatedBy: user.ID,
		DefaultRuntimeBrokerID: brokerID,
		Created:                time.Now().Add(-3 * time.Hour), Updated: time.Now(),
	}
	// Create authorized project B (template project)
	projB := &store.Project{
		ID: tid("r2-fcm-pb"), Name: "FilterProjB", Slug: "r2-fcm-b",
		OwnerID: user.ID, CreatedBy: user.ID,
		Labels:  map[string]string{"scion.io/template": "true"},
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	// Create unauthorized project C (same name as A, with same broker)
	projC := &store.Project{
		ID: tid("r2-fcm-pc"), Name: "FilterProjA", Slug: "r2-fcm-c",
		OwnerID: tid("r2-fcm-oth"), CreatedBy: tid("r2-fcm-oth"),
		DefaultRuntimeBrokerID: brokerID,
		Created:                time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	// Create authorized project D (additional for pagination test)
	projD := &store.Project{
		ID: tid("r2-fcm-pd"), Name: "FilterProjD", Slug: "r2-fcm-d",
		OwnerID: user.ID, CreatedBy: user.ID,
		DefaultRuntimeBrokerID: brokerID,
		Created:                time.Now().Add(-4 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projA))
	require.NoError(t, s.CreateProject(ctx, projB))
	require.NoError(t, s.CreateProject(ctx, projC))
	require.NoError(t, s.CreateProject(ctx, projD))

	// Register the broker as a provider (contributor) for projects A, C, D.
	// The brokerId filter matches against the project_contributor table.
	for _, pid := range []string{projA.ID, projC.ID, projD.ID} {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  pid,
			BrokerID:   brokerID,
			BrokerName: "test-broker",
			Status:     "online",
		}))
	}

	// Authorize A, B, and D
	for _, pid := range []string{projA.ID, projB.ID, projD.ID} {
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          pid,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
	}

	// Create agents with various properties including labels and broker IDs
	agentRunning := &store.Agent{
		ID: tid("r2-fcm-ar"), Slug: "r2-fcm-ar", Name: "running-agent",
		ProjectID: projA.ID, OwnerID: user.ID, Phase: "running",
		Labels:          map[string]string{"env": "prod", "tier": "frontend"},
		RuntimeBrokerID: brokerID,
		Created:         time.Now(), Updated: time.Now(),
	}
	agentStopped := &store.Agent{
		ID: tid("r2-fcm-as"), Slug: "r2-fcm-as", Name: "stopped-agent",
		ProjectID: projA.ID, OwnerID: user.ID, Phase: "stopped",
		Labels:  map[string]string{"env": "prod", "tier": "backend"},
		Created: time.Now(), Updated: time.Now(),
	}
	agentBProj := &store.Agent{
		ID: tid("r2-fcm-ab"), Slug: "r2-fcm-ab", Name: "projb-agent",
		ProjectID: projB.ID, OwnerID: user.ID, Phase: "running",
		Labels:          map[string]string{"env": "staging"},
		RuntimeBrokerID: brokerID,
		Created:         time.Now(), Updated: time.Now(),
	}
	// Additional agent in projD for cursor termination
	agentDProj := &store.Agent{
		ID: tid("r2-fcm-ad"), Slug: "r2-fcm-ad", Name: "projd-agent",
		ProjectID: projD.ID, OwnerID: user.ID, Phase: "running",
		Labels:          map[string]string{"env": "prod", "tier": "frontend"},
		RuntimeBrokerID: brokerID,
		Created:         time.Now(), Updated: time.Now(),
	}
	// Agent in unauthorized project (same labels and broker as authorized agents)
	agentUnauth := &store.Agent{
		ID: tid("r2-fcm-au"), Slug: "r2-fcm-au", Name: "unauth-agent",
		ProjectID: projC.ID, OwnerID: tid("r2-fcm-oth"), Phase: "running",
		Labels:          map[string]string{"env": "prod", "tier": "frontend"},
		RuntimeBrokerID: brokerID,
		Created:         time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agentRunning))
	require.NoError(t, s.CreateAgent(ctx, agentStopped))
	require.NoError(t, s.CreateAgent(ctx, agentBProj))
	require.NoError(t, s.CreateAgent(ctx, agentDProj))
	require.NoError(t, s.CreateAgent(ctx, agentUnauth))

	// Table-driven project filter tests
	projectCases := []struct {
		name       string
		query      string
		wantTotal  int
		wantIDs    []string
		notWantIDs []string
	}{
		{
			name:       "name_filter_intersects_auth",
			query:      "name=FilterProjA",
			wantTotal:  1,
			wantIDs:    []string{projA.ID},
			notWantIDs: []string{projC.ID}, // unauthorized, same name
		},
		{
			name:       "slug_filter_intersects_auth",
			query:      "slug=r2-fcm-a",
			wantTotal:  1,
			wantIDs:    []string{projA.ID},
			notWantIDs: []string{projC.ID},
		},
		{
			name:       "slug_filter_unauthorized_empty",
			query:      "slug=r2-fcm-c",
			wantTotal:  0,
			notWantIDs: []string{projC.ID},
		},
		{
			name:       "brokerId_filter_intersects_auth",
			query:      "brokerId=" + brokerID,
			wantTotal:  2, // projA + projD (authorized, have broker); projC has same broker but unauthorized
			wantIDs:    []string{projA.ID, projD.ID},
			notWantIDs: []string{projC.ID},
		},
		{
			name:       "isTemplate_true_intersects_auth",
			query:      "isTemplate=true",
			wantTotal:  1, // only projB is a template
			wantIDs:    []string{projB.ID},
			notWantIDs: []string{projA.ID, projC.ID},
		},
		{
			name:       "isTemplate_false_intersects_auth",
			query:      "isTemplate=false",
			wantTotal:  2, // projA + projD (authorized, non-template); projC unauthorized
			wantIDs:    []string{projA.ID, projD.ID},
			notWantIDs: []string{projC.ID},
		},
	}

	for _, tc := range projectCases {
		t.Run("project/"+tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, user, http.MethodGet,
				"/api/v1/projects?"+tc.query, nil)
			require.Equal(t, http.StatusOK, rec.Code)
			var resp ListProjectsResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, tc.wantTotal, resp.TotalCount,
				"totalCount mismatch for filter %s", tc.query)
			ids := extractProjectIDs(resp.Projects)
			for _, want := range tc.wantIDs {
				assert.Contains(t, ids, want)
			}
			for _, notWant := range tc.notWantIDs {
				assert.NotContains(t, ids, notWant)
			}
		})
	}

	// Table-driven agent filter tests
	agentCases := []struct {
		name       string
		query      string
		wantTotal  int
		wantIDs    []string
		notWantIDs []string
	}{
		{
			name:       "projectId_filter_intersects_auth",
			query:      "projectId=" + projA.ID,
			wantTotal:  2, // running + stopped
			wantIDs:    []string{agentRunning.ID, agentStopped.ID},
			notWantIDs: []string{agentBProj.ID, agentUnauth.ID},
		},
		{
			name:       "projectId_unauthorized_empty",
			query:      "projectId=" + projC.ID,
			wantTotal:  0,
			notWantIDs: []string{agentUnauth.ID},
		},
		{
			name:       "phase_filter_intersects_auth",
			query:      "phase=running",
			wantTotal:  3, // running in projA + running in projB + running in projD
			wantIDs:    []string{agentRunning.ID, agentBProj.ID, agentDProj.ID},
			notWantIDs: []string{agentStopped.ID, agentUnauth.ID},
		},
		{
			name:       "slug_filter_intersects_auth",
			query:      "projectId=" + projA.Slug,
			wantTotal:  2,
			wantIDs:    []string{agentRunning.ID, agentStopped.ID},
			notWantIDs: []string{agentUnauth.ID},
		},
		{
			name:       "includeDeleted_false_no_leak",
			query:      "includeDeleted=false",
			wantTotal:  4, // all agents in authorized projects (A, B, D)
			notWantIDs: []string{agentUnauth.ID},
		},
		{
			name:       "label_filter_intersects_auth",
			query:      "label=env%3Dprod",
			wantTotal:  3, // running(A), stopped(A), projd-agent(D) — all have env=prod; unauth has same label but unauthorized
			wantIDs:    []string{agentRunning.ID, agentStopped.ID, agentDProj.ID},
			notWantIDs: []string{agentUnauth.ID},
		},
		{
			name:       "label_multi_key_filter_intersects_auth",
			query:      "label=env%3Dprod&label=tier%3Dfrontend",
			wantTotal:  2, // running(A) + projd-agent(D) have both env=prod AND tier=frontend
			wantIDs:    []string{agentRunning.ID, agentDProj.ID},
			notWantIDs: []string{agentStopped.ID, agentUnauth.ID},
		},
		{
			name:       "runtimeBrokerId_filter_intersects_auth",
			query:      "runtimeBrokerId=" + brokerID,
			wantTotal:  3, // running(A), projb-agent(B), projd-agent(D) have the broker; unauth has same broker but unauthorized
			wantIDs:    []string{agentRunning.ID, agentBProj.ID, agentDProj.ID},
			notWantIDs: []string{agentUnauth.ID},
		},
	}

	for _, tc := range agentCases {
		t.Run("agent/"+tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, user, http.MethodGet,
				"/api/v1/agents?"+tc.query, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var resp ListAgentsResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, tc.wantTotal, resp.TotalCount,
				"totalCount mismatch for filter %s", tc.query)
			ids := extractAgentIDs(resp.Agents)
			for _, want := range tc.wantIDs {
				assert.Contains(t, ids, want)
			}
			for _, notWant := range tc.notWantIDs {
				assert.NotContains(t, ids, notWant)
			}
		})
	}

	// Paginated filtered project case: brokerId filter + limit=1 → walk pages
	t.Run("project/brokerId_paginated", func(t *testing.T) {
		var allIDs []string
		cursor := ""
		for page := 0; page < 10; page++ {
			url := "/api/v1/projects?brokerId=" + brokerID + "&limit=1"
			if cursor != "" {
				url += "&cursor=" + cursor
			}
			rec := doRequestAsUser(t, srv, user, http.MethodGet, url, nil)
			require.Equal(t, http.StatusOK, rec.Code)
			var resp ListProjectsResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, 2, resp.TotalCount,
				"paginated brokerId filter must report stable totalCount=2")
			for _, p := range resp.Projects {
				allIDs = append(allIDs, p.Project.ID)
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		assert.Len(t, allIDs, 2, "paginated walk must return exactly 2 projects")
		assert.Contains(t, allIDs, projA.ID)
		assert.Contains(t, allIDs, projD.ID)
		assert.NotContains(t, allIDs, projC.ID,
			"unauthorized project with same broker must not appear in paginated results")
	})

	// Agent cursor termination: label filter with multiple authorized matches, limit=1
	t.Run("agent/label_cursor_termination", func(t *testing.T) {
		var allIDs []string
		cursor := ""
		for page := 0; page < 10; page++ {
			url := "/api/v1/agents?label=env%3Dprod&label=tier%3Dfrontend&limit=1"
			if cursor != "" {
				url += "&cursor=" + cursor
			}
			rec := doRequestAsUser(t, srv, user, http.MethodGet, url, nil)
			require.Equal(t, http.StatusOK, rec.Code)
			var resp ListAgentsResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			assert.Equal(t, 2, resp.TotalCount,
				"label-filtered agent list must report stable totalCount=2")
			for _, a := range resp.Agents {
				allIDs = append(allIDs, a.Agent.ID)
			}
			if resp.NextCursor == "" {
				break
			}
			cursor = resp.NextCursor
		}
		assert.Len(t, allIDs, 2, "cursor walk must terminate with exactly 2 agents")
		assert.Contains(t, allIDs, agentRunning.ID)
		assert.Contains(t, allIDs, agentDProj.ID)
		assert.NotContains(t, allIDs, agentUnauth.ID,
			"unauthorized agent with matching labels must not appear")
	})
}
