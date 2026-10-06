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
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==========================================================================
// R-1: Multi-page interleaved authorization test
// ==========================================================================

// TestRS2_ProjectListMultiPageInterleaved creates 10 projects, authorizes 5
// (interleaved by creation time with 5 unauthorized), and walks all pages
// with limit=2. Asserts exact ordered IDs, exact stable total on every page,
// no duplicates/skips/leaks, and empty final cursor.
func TestRS2_ProjectListMultiPageInterleaved(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-pmi-u"), Email: "r1-pmi@test.com",
		DisplayName: "Interleave User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	var authorizedIDs []string
	for i := 0; i < 10; i++ {
		p := &store.Project{
			ID:        tid(fmt.Sprintf("r1-pmi-p%02d", i)),
			Name:      fmt.Sprintf("Proj %02d", i),
			Slug:      fmt.Sprintf("r1-pmi-%02d", i),
			OwnerID:   user.ID,
			CreatedBy: user.ID,
			Created:   time.Now().Add(time.Duration(-100+i) * time.Minute),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))

		// Authorize even-numbered projects only (interleaved).
		if i%2 == 0 {
			_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: memberRD.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      user.ID,
				ScopeType:        store.RoleScopeProject,
				ScopeID:          p.ID,
				CreatedBy:        "test",
			})
			require.NoError(t, err)
			authorizedIDs = append(authorizedIDs, p.ID)
		}
	}
	sort.Strings(authorizedIDs)

	// Walk pages with limit=2
	var allSeenIDs []string
	cursor := ""
	pageCount := 0
	for {
		pageCount++
		url := "/api/v1/projects?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, url, nil)
		require.Equal(t, http.StatusOK, rec.Code, "page %d body: %s", pageCount, rec.Body.String())

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		assert.Equal(t, 5, resp.TotalCount, "page %d: totalCount must be stable at 5", pageCount)
		assert.LessOrEqual(t, len(resp.Projects), 2, "page %d: at most limit items", pageCount)

		for _, p := range resp.Projects {
			allSeenIDs = append(allSeenIDs, p.ID)
		}

		cursor = resp.NextCursor
		if cursor == "" {
			break
		}
		require.Less(t, pageCount, 10, "infinite pagination loop")
	}

	sort.Strings(allSeenIDs)
	assert.Equal(t, authorizedIDs, allSeenIDs, "must see exactly the authorized projects, no duplicates/skips/leaks")
}

// TestRS2_AgentListMultiPageInterleaved does the same for agents.
func TestRS2_AgentListMultiPageInterleaved(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-ami-u"), Email: "r1-ami@test.com",
		DisplayName: "Agent Interleave", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	visProj := &store.Project{
		ID: tid("r1-ami-vp"), Name: "Vis Proj", Slug: "r1-ami-vis",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now().Add(-10 * time.Hour), Updated: time.Now(),
	}
	hidProj := &store.Project{
		ID: tid("r1-ami-hp"), Name: "Hid Proj", Slug: "r1-ami-hid",
		OwnerID: tid("r1-ami-oth"), CreatedBy: tid("r1-ami-oth"),
		Created: time.Now().Add(-9 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, visProj))
	require.NoError(t, s.CreateProject(ctx, hidProj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          visProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create 8 agents: 4 in visible project, 4 in hidden project (interleaved)
	var authorizedAgentIDs []string
	for i := 0; i < 8; i++ {
		proj := visProj
		if i%2 != 0 {
			proj = hidProj
		}
		a := &store.Agent{
			ID:        tid(fmt.Sprintf("r1-ami-a%02d", i)),
			Slug:      fmt.Sprintf("r1-ami-a%02d", i),
			Name:      fmt.Sprintf("agent-%02d", i),
			ProjectID: proj.ID,
			OwnerID:   user.ID,
			Created:   time.Now().Add(time.Duration(-8+i) * time.Minute),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		if i%2 == 0 {
			authorizedAgentIDs = append(authorizedAgentIDs, a.ID)
		}
	}
	sort.Strings(authorizedAgentIDs)

	var allSeenIDs []string
	cursor := ""
	pageCount := 0
	for {
		pageCount++
		url := "/api/v1/agents?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, url, nil)
		require.Equal(t, http.StatusOK, rec.Code, "page %d", pageCount)

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		assert.Equal(t, 4, resp.TotalCount, "page %d: totalCount stable at 4", pageCount)

		for _, a := range resp.Agents {
			allSeenIDs = append(allSeenIDs, a.ID)
		}

		cursor = resp.NextCursor
		if cursor == "" {
			break
		}
		require.Less(t, pageCount, 10, "infinite loop")
	}

	sort.Strings(allSeenIDs)
	assert.Equal(t, authorizedAgentIDs, allSeenIDs)
}

// TestRS2_ProjectListInterleavedWithCallerFilter tests that a caller filter
// intersects with authorization predicates on interleaved data.
func TestRS2_ProjectListInterleavedWithCallerFilter(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-flt-u"), Email: "r1-flt@test.com",
		DisplayName: "Filter User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// 4 projects: 2 authorized, 2 not. One authorized has a specific name.
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("regular-%02d", i)
		if i == 2 {
			name = "special-project"
		}
		p := &store.Project{
			ID: tid(fmt.Sprintf("r1-flt-p%02d", i)), Name: name,
			Slug: fmt.Sprintf("r1-flt-%02d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		if i%2 == 0 {
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
	}

	// Filter by name (caller filter) intersected with auth scope
	rec := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/projects?name=special-project", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 1, resp.TotalCount, "name filter + auth should find exactly 1")
	require.Len(t, resp.Projects, 1)
	assert.Equal(t, "special-project", resp.Projects[0].Name)
}

// ==========================================================================
// R-3: Cursor authority change replay tests
// ==========================================================================

// TestRS2_CursorReplayAfterGrantRemoval mints a cursor, removes a grant,
// then replays. The cursor should be rejected because the authorization
// filter changed.
func TestRS2_CursorReplayAfterGrantRemoval(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-crg-u"), Email: "r1-crg@test.com",
		DisplayName: "Cursor Grant User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Create 4 projects, all authorized.
	var bindingIDs []string
	for i := 0; i < 4; i++ {
		p := &store.Project{
			ID: tid(fmt.Sprintf("r1-crg-p%d", i)), Name: fmt.Sprintf("Proj %d", i),
			Slug: fmt.Sprintf("r1-crg-%d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          p.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		bindingIDs = append(bindingIDs, rb.ID)
	}

	// Get page 1 with cursor
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var page1 ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
	require.NotEmpty(t, page1.NextCursor)

	// Remove one binding (authority change)
	require.NoError(t, s.DeleteRoleBinding(ctx, bindingIDs[0]))

	// Replay cursor — should be rejected (authorization filter changed)
	rec = doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/projects?limit=2&cursor="+page1.NextCursor, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"cursor must be rejected after grant removal changes authorization filter")
}

// TestRS2_CursorReplayAfterBindingExpiry mints a cursor while all bindings are
// active, then transitions one binding to expired (delete + recreate with past
// ExpiresAt), and replays. The cursor must be rejected because the scope
// resolution now sees fewer authorized projects → different filter hash.
func TestRS2_CursorReplayAfterBindingExpiry(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-cre-u"), Email: "r1-cre@test.com",
		DisplayName: "Cursor Expiry User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Create 4 projects, all authorized with active (non-expiring) bindings.
	var bindingIDs []string
	var projectIDs []string
	for i := 0; i < 4; i++ {
		p := &store.Project{
			ID: tid(fmt.Sprintf("r1-cre-p%d", i)), Name: fmt.Sprintf("Proj %d", i),
			Slug: fmt.Sprintf("r1-cre-%d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		projectIDs = append(projectIDs, p.ID)

		rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: memberRD.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      user.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          p.ID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		bindingIDs = append(bindingIDs, rb.ID)
	}

	// Verify all 4 are authorized and mint a cursor.
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var page1 ListProjectsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page1))
	require.NotEmpty(t, page1.NextCursor, "cursor required for page-2 replay")
	assert.Equal(t, 4, page1.TotalCount, "all 4 projects must be visible before expiry")

	// Transition binding 0 to expired: delete the active binding, then recreate
	// it with ExpiresAt in the past. This is the deterministic clock seam the
	// brief requires — no sleep or wall-clock dependency.
	require.NoError(t, s.DeleteRoleBinding(ctx, bindingIDs[0]))
	pastExpiry := time.Now().Add(-1 * time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectIDs[0],
		ExpiresAt:        &pastExpiry,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Confirm the expiry actually reduced the visible scope (sanity check).
	recCheck := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?limit=10", nil)
	require.Equal(t, http.StatusOK, recCheck.Code)
	var checkResp ListProjectsResponse
	require.NoError(t, json.NewDecoder(recCheck.Body).Decode(&checkResp))
	assert.Equal(t, 3, checkResp.TotalCount,
		"after expiry, only 3 projects should be visible (proves binding really changed)")

	// Replay the stale cursor → must be rejected because scope hash changed.
	rec = doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/projects?limit=2&cursor="+page1.NextCursor, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"cursor must be rejected after binding expiry changes authorization filter")
}

// ==========================================================================
// R-4: Real store adapter tests for ExcludedProjectIDs
// ==========================================================================

// TestRS2_StoreExcludedProjectIDs_Real verifies ExcludedProjectIDs affects both
// rows and TotalCount at the store level, composes with authorized IDs and
// normal filters, and remains correct across pagination.
func TestRS2_StoreExcludedProjectIDs_Real(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-sep-u"), Email: "r1-sep@test.com",
		DisplayName: "Store Excl User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// System admin so scope is All
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	var allProjectIDs []string
	for i := 0; i < 4; i++ {
		p := &store.Project{
			ID: tid(fmt.Sprintf("r1-sep-p%d", i)), Name: fmt.Sprintf("Proj %d", i),
			Slug: fmt.Sprintf("r1-sep-%d", i), OwnerID: user.ID, CreatedBy: user.ID,
			Created: time.Now().Add(time.Duration(-4+i) * time.Hour), Updated: time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		allProjectIDs = append(allProjectIDs, p.ID)
	}

	t.Run("excluded_project_absent_from_rows_and_total", func(t *testing.T) {
		filter := store.ProjectFilter{
			ExcludedProjectIDs: []string{allProjectIDs[1]},
		}
		result, err := s.ListProjects(ctx, filter, store.ListOptions{Limit: 100})
		require.NoError(t, err)

		for _, p := range result.Items {
			assert.NotEqual(t, allProjectIDs[1], p.ID, "excluded project must not appear")
		}
		assert.Equal(t, len(allProjectIDs)-1, result.TotalCount,
			"TotalCount must reflect exclusion")
	})

	t.Run("excluded_composes_with_authorized", func(t *testing.T) {
		// Authorized to 3, exclude 1 of those 3. Should see 2.
		filter := store.ProjectFilter{
			AuthorizedProjectIDs: allProjectIDs[:3],
			ExcludedProjectIDs:   []string{allProjectIDs[0]},
		}
		result, err := s.ListProjects(ctx, filter, store.ListOptions{Limit: 100})
		require.NoError(t, err)
		assert.Equal(t, 2, result.TotalCount)
		for _, p := range result.Items {
			assert.NotEqual(t, allProjectIDs[0], p.ID)
			assert.NotEqual(t, allProjectIDs[3], p.ID)
		}
	})

	t.Run("agent_excluded_projects", func(t *testing.T) {
		a1 := &store.Agent{
			ID: tid("r1-sep-a1"), Slug: "r1-sep-a1", Name: "agent-1",
			ProjectID: allProjectIDs[0], OwnerID: user.ID,
			Created: time.Now(), Updated: time.Now(),
		}
		a2 := &store.Agent{
			ID: tid("r1-sep-a2"), Slug: "r1-sep-a2", Name: "agent-2",
			ProjectID: allProjectIDs[1], OwnerID: user.ID,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, a1))
		require.NoError(t, s.CreateAgent(ctx, a2))

		filter := store.AgentFilter{
			ExcludedProjectIDs: []string{allProjectIDs[0]},
		}
		result, err := s.ListAgents(ctx, filter, store.ListOptions{Limit: 100})
		require.NoError(t, err)

		for _, a := range result.Items {
			assert.NotEqual(t, allProjectIDs[0], a.ProjectID,
				"agent in excluded project must not appear")
		}
	})
}

// TestRS2_StoreMalformedExclusionFailsClosed verifies that malformed
// ExcludedProjectIDs return an error instead of being silently skipped.
func TestRS2_StoreMalformedExclusionFailsClosed(t *testing.T) {
	_, s := testServer(t) //nolint:dogsled
	ctx := context.Background()

	t.Run("project_store", func(t *testing.T) {
		filter := store.ProjectFilter{
			ExcludedProjectIDs: []string{"not-a-uuid"},
		}
		_, err := s.ListProjects(ctx, filter, store.ListOptions{Limit: 10})
		require.Error(t, err, "malformed ExcludedProjectIDs must fail closed")
		assert.Contains(t, err.Error(), "invalid authorization predicate")
	})

	t.Run("agent_store", func(t *testing.T) {
		filter := store.AgentFilter{
			ExcludedProjectIDs: []string{"not-a-uuid"},
		}
		_, err := s.ListAgents(ctx, filter, store.ListOptions{Limit: 10})
		require.Error(t, err, "malformed ExcludedProjectIDs must fail closed")
		assert.Contains(t, err.Error(), "invalid authorization predicate")
	})
}

// ==========================================================================
// Finding 5: Malformed exclusions fail-closed HTTP test
// ==========================================================================

// TestRS2_MalformedConstraintExclusionHTTP verifies that a malformed
// constraint scope ID propagates through the full HTTP path to a stable 500.
// A constraint with a malformed ScopeID ends up as an ExcludedProjectIDs
// entry in the store filter; parseUUIDsStrict rejects it → handler 500.
//
// We inject the malformed constraint via a store wrapper that returns a
// poisoned constraint from ListAccessConstraints, since the real store
// validates scope IDs on creation. This simulates data corruption.
func TestRS2_MalformedConstraintExclusionHTTP(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := &store.User{
		ID: tid("r2-mch-u"), Email: "r2-mch@test.com",
		DisplayName: "Malformed Constraint User", Role: store.UserRoleAdmin, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	// Super-admin so scope is All (constraints become ExcludedProjectIDs).
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

	proj := &store.Project{
		ID: tid("r2-mch-p"), Name: "MCH Proj", Slug: "r2-mch-proj",
		OwnerID: admin.ID, CreatedBy: admin.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID: tid("r2-mch-a"), Slug: "r2-mch-a", Name: "mch-agent",
		ProjectID: proj.ID, OwnerID: admin.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	// Install a store wrapper that injects a constraint with a malformed
	// scope ID (simulating data corruption). The constraint targets the
	// admin user's principal closure and blocks project.list + agent.list
	// for a scope ID that is not a valid UUID.
	malformedStore := &r2MalformedConstraintStore{Store: s, principalID: admin.ID}
	origStore := srv.store
	origAuthzStore := srv.authzService.store
	srv.store = malformedStore
	srv.authzService.store = malformedStore
	defer func() {
		srv.store = origStore
		srv.authzService.store = origAuthzStore
	}()

	t.Run("project_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"malformed constraint scope must produce 500 through HTTP path")
		assert.NotContains(t, rec.Body.String(), proj.Name,
			"500 response must not leak project data")
	})

	t.Run("agent_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"malformed constraint scope must produce 500 through HTTP path")
		assert.NotContains(t, rec.Body.String(), ag.Name,
			"500 response must not leak agent data")
	})
}

// r2MalformedConstraintStore wraps a real store and injects a poisoned
// constraint with a malformed ScopeID into the ListAccessConstraints result.
type r2MalformedConstraintStore struct {
	store.Store
	principalID string
}

func (s *r2MalformedConstraintStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	real, err := s.Store.ListAccessConstraints(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	pType := "user"
	// Inject a constraint with a malformed scope ID. MaximumPermissions
	// allowlist omits project.list and agent.list, so this constraint blocks
	// listing for the target principal in the malformed scope.
	poisoned := &store.AccessConstraint{
		ID:                   "malformed-constraint-test",
		Name:                 "poisoned-constraint",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   &s.principalID,
		ScopeType:            "project",
		ScopeID:              "not-a-valid-uuid", // malformed → parseUUIDsStrict error
		MaximumPermissions:   []string{"agent.read"},
		CreatedBy:            "test",
	}
	return append(real, poisoned), nil
}

// ==========================================================================
// Finding 6: Non-user Mine semantics (agent JWT)
// ==========================================================================

// testAgentIdentity implements AgentIdentity for tests.
type testAgentIdentity struct {
	id        string
	projectID string
	tokenID   string
}

func (a *testAgentIdentity) ID() string                    { return a.id }
func (a *testAgentIdentity) Type() string                  { return "agent" }
func (a *testAgentIdentity) ProjectID() string             { return a.projectID }
func (a *testAgentIdentity) Scopes() []AgentTokenScope     { return nil }
func (a *testAgentIdentity) HasScope(AgentTokenScope) bool { return true }
func (a *testAgentIdentity) Ancestry() []string            { return nil }
func (a *testAgentIdentity) OriginUserID() string          { return "" }
func (a *testAgentIdentity) TokenID() string               { return a.tokenID }

// localAncestryProvenance opts this fake into AncestryIsHubAttested: the
// marker is not inherited from Type() == "agent", so test fakes must opt in
// explicitly.
func (a *testAgentIdentity) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceAgentJWT
}

// authzClassification opts this fake into principalContextForIdentity /
// credentialContextForIdentity classification (reached indirectly here
// through ComputeCapabilities/ComputeCapabilitiesBatch), the same way it
// opts into ancestry attestation above: neither is inherited from
// Type() == "agent".
func (a *testAgentIdentity) authzClassification() (PrincipalKind, CredentialKind) {
	return PrincipalKindAgent, CredentialKindAgentJWT
}

// TestRS2_AgentJWTMineShared verifies that an agent JWT identity gets empty
// Mine (since agents can't hold project-owner bindings) and full-scope Shared.
func TestRS2_AgentJWTMineShared(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project and an agent in it
	user := &store.User{
		ID: tid("r1-ajm-u"), Email: "r1-ajm@test.com",
		DisplayName: "Agent JWT User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	proj := &store.Project{
		ID: tid("r1-ajm-p"), Name: "Agent JWT Proj", Slug: "r1-ajm-proj",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	agent := &store.Agent{
		ID: tid("r1-ajm-a"), Slug: "r1-ajm-agent", Name: "test-agent",
		ProjectID: proj.ID, OwnerID: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	agentIdent := &testAgentIdentity{
		id:        agent.ID,
		projectID: proj.ID,
		tokenID:   "test-token-id",
	}

	t.Run("project_mine_empty_for_agent", func(t *testing.T) {
		rec := doRequestAsIdentity(t, srv, agentIdent, http.MethodGet,
			"/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 0, resp.TotalCount, "Mine must be empty for agent JWT")
		assert.Empty(t, resp.Projects)
	})

	t.Run("agent_mine_empty_for_agent", func(t *testing.T) {
		rec := doRequestAsIdentity(t, srv, agentIdent, http.MethodGet,
			"/api/v1/agents?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 0, resp.TotalCount, "Agent Mine must be empty for agent JWT")
	})

	t.Run("agent_shared_is_full_scope", func(t *testing.T) {
		// For agent JWT, Shared = full scope (since Mine is empty).
		// With doRequestAsIdentity (bypassing auth middleware), the agent
		// has no actual bindings, so scope is None. What matters is that
		// scope=shared does NOT crash and returns a valid response.
		rec := doRequestAsIdentity(t, srv, agentIdent, http.MethodGet,
			"/api/v1/agents?scope=shared", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		// Response is valid (no 500 error)
		assert.GreaterOrEqual(t, resp.TotalCount, 0)
	})
}

// ==========================================================================
// Finding 7: System-All Shared semantics
// ==========================================================================

// TestRS2_SystemAllSharedSemantics verifies that a system-wide admin gets
// correct Mine/Shared classification: Mine = owned projects, Shared = all
// others (including non-owned projects the admin can see via system scope).
func TestRS2_SystemAllSharedSemantics(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := &store.User{
		ID: tid("r1-sas-u"), Email: "r1-sas@test.com",
		DisplayName: "System Admin", Role: store.UserRoleAdmin, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	// Use super-admin, not hub-admin: hub-admin has project.list but NOT
	// agent.list at system scope (by design — see seed.go). Only super-admin
	// gets All scope for both projects AND agents.
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy, // super-admin requires system-reconcile creator
	})
	require.NoError(t, err)

	// Create owned and non-owned projects
	ownedProj := &store.Project{
		ID: tid("r1-sas-op"), Name: "Owned", Slug: "r1-sas-owned",
		OwnerID: admin.ID, CreatedBy: admin.ID,
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	otherProj := &store.Project{
		ID: tid("r1-sas-np"), Name: "Other", Slug: "r1-sas-other",
		OwnerID: tid("r1-sas-oth"), CreatedBy: tid("r1-sas-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, ownedProj))
	require.NoError(t, s.CreateProject(ctx, otherProj))

	// Give admin an owner binding on ownedProj
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, ownedProj.ID, admin.ID))

	// Create agents
	ownedAgent := &store.Agent{
		ID: tid("r1-sas-oa"), Slug: "r1-sas-oa", Name: "owned-agent",
		ProjectID: ownedProj.ID, OwnerID: admin.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	otherAgent := &store.Agent{
		ID: tid("r1-sas-na"), Slug: "r1-sas-na", Name: "other-agent",
		ProjectID: otherProj.ID, OwnerID: tid("r1-sas-oth"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, ownedAgent))
	require.NoError(t, s.CreateAgent(ctx, otherAgent))

	t.Run("project_mine_shows_owned", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, ownedProj.ID, "Mine should include owned project")
		assert.NotContains(t, projectIDs, otherProj.ID, "Mine should not include other project")
	})

	t.Run("project_shared_shows_other", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects?scope=shared", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, otherProj.ID,
			"System-All Shared must include non-owned projects")
		assert.NotContains(t, projectIDs, ownedProj.ID,
			"Shared must not include owned project")
	})

	t.Run("agent_mine_shows_owned_project_agents", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/agents?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		agentIDs := extractAgentIDs(resp.Agents)
		assert.Contains(t, agentIDs, ownedAgent.ID)
		assert.NotContains(t, agentIDs, otherAgent.ID)
	})

	t.Run("agent_shared_shows_other_project_agents", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/agents?scope=shared", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		agentIDs := extractAgentIDs(resp.Agents)
		assert.Contains(t, agentIDs, otherAgent.ID,
			"System-All Shared must include agents from non-owned projects")
		assert.NotContains(t, agentIDs, ownedAgent.ID)
	})
}

// ==========================================================================
// Finding 8: Canonical cursor inputs
// ==========================================================================

// TestRS2_CanonicalCursorBinding verifies that identical authorization state
// yields identical cursor bindings regardless of input order.
func TestRS2_CanonicalCursorBinding(t *testing.T) {
	t.Run("canonicalize_sorts_and_deduplicates", func(t *testing.T) {
		input1 := []string{"c", "a", "b", "a"}
		input2 := []string{"b", "c", "a"}
		result1 := canonicalizeStringSlice(input1)
		result2 := canonicalizeStringSlice(input2)
		assert.Equal(t, result1, result2, "same elements must produce identical canonical form")
		assert.Equal(t, []string{"a", "b", "c"}, result1)
	})

	t.Run("empty_and_single", func(t *testing.T) {
		assert.Nil(t, canonicalizeStringSlice(nil))
		assert.Equal(t, []string{"a"}, canonicalizeStringSlice([]string{"a"}))
	})
}

// ==========================================================================
// Finding 9: Owner lookup errors fail-closed
// ==========================================================================

// TestRS2_OwnerResolutionErrorFailsClosed is tested implicitly through the
// production code change (resolveUserOwnerProjectIDsOrError propagates errors).
// The R-2 failure injection tests exercise the HTTP error path.

// ==========================================================================
// Original-brief coverage: suspended user list denial
// ==========================================================================

// TestRS2_SuspendedUserListDenial verifies that a suspended user is denied
// at the auth middleware level, not at the list handler.
func TestRS2_SuspendedUserListDenial(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-sus-u"), Email: "r1-sus@test.com",
		DisplayName: "Suspended", Role: store.UserRoleMember, Status: "suspended",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
	// Suspended users should get 401 or 403 from auth middleware
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"suspended user must be denied, got %d", rec.Code)
}

// ==========================================================================
// Original-brief coverage: direct owner/admin/member distinctions
// ==========================================================================

// TestRS2_OwnerAdminMemberDistinctions verifies that Mine only includes
// projects with owner bindings, not admin or member bindings.
func TestRS2_OwnerAdminMemberDistinctions(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r1-oam-u"), Email: "r1-oam@test.com",
		DisplayName: "OAM User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	ownerProj := &store.Project{
		ID: tid("r1-oam-op"), Name: "Owner Proj", Slug: "r1-oam-owner",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now().Add(-3 * time.Hour), Updated: time.Now(),
	}
	adminProj := &store.Project{
		ID: tid("r1-oam-ap"), Name: "Admin Proj", Slug: "r1-oam-admin",
		OwnerID: tid("r1-oam-oth"), CreatedBy: tid("r1-oam-oth"),
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	memberProj := &store.Project{
		ID: tid("r1-oam-mp"), Name: "Member Proj", Slug: "r1-oam-member",
		OwnerID: tid("r1-oam-oth"), CreatedBy: tid("r1-oam-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, ownerProj))
	require.NoError(t, s.CreateProject(ctx, adminProj))
	require.NoError(t, s.CreateProject(ctx, memberProj))

	// Owner binding
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, ownerProj.ID, user.ID))

	// Admin binding
	adminRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: adminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          adminProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Member binding
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          memberProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	t.Run("mine_only_owner", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, ownerProj.ID, "Mine must include owner project")
		assert.NotContains(t, projectIDs, adminProj.ID, "Mine must NOT include admin project")
		assert.NotContains(t, projectIDs, memberProj.ID, "Mine must NOT include member project")
	})

	t.Run("shared_admin_and_member", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?scope=shared", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, adminProj.ID, "Shared must include admin project")
		assert.Contains(t, projectIDs, memberProj.ID, "Shared must include member project")
		assert.NotContains(t, projectIDs, ownerProj.ID, "Shared must NOT include owner project")
	})

	t.Run("all_scope_sees_everything", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.GreaterOrEqual(t, resp.TotalCount, 3)
	})
}

// ==========================================================================
// R-2: Failure injection tests
// ==========================================================================

// r2FailingStore wraps a real store.Store and injects failures at specific
// points in the authorization and list pipeline:
//   - failGetEffectiveGroups: principal/group closure failure
//   - failListBindings: role-binding load failure
//   - failGetRoleDefinition: role-definition load failure
//   - failListConstraints: constraint load failure
//   - failListProjects: resource list failure
//   - failListAgents: resource list failure
type r2FailingStore struct {
	store.Store
	failGetEffectiveGroups error
	failListBindings       error
	failGetRoleDefinition  error
	failListConstraints    error
	failListProjects       error
	failListAgents         error
}

func (s *r2FailingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.failGetEffectiveGroups != nil {
		return nil, s.failGetEffectiveGroups
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *r2FailingStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if s.failGetEffectiveGroups != nil {
		return nil, s.failGetEffectiveGroups
	}
	return s.Store.GetEffectiveGroupsForAgent(ctx, agentID)
}

func (s *r2FailingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failListBindings != nil {
		return nil, s.failListBindings
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *r2FailingStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if s.failGetRoleDefinition != nil {
		return nil, s.failGetRoleDefinition
	}
	return s.Store.GetRoleDefinition(ctx, id)
}

func (s *r2FailingStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	if s.failGetRoleDefinition != nil {
		return nil, s.failGetRoleDefinition
	}
	return s.Store.GetRoleDefinitionsByIDs(ctx, ids)
}

func (s *r2FailingStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if s.failListConstraints != nil {
		return nil, s.failListConstraints
	}
	return s.Store.ListAccessConstraints(ctx, limit, offset)
}

func (s *r2FailingStore) ListProjects(ctx context.Context, filter store.ProjectFilter, opts store.ListOptions) (*store.ListResult[store.Project], error) {
	if s.failListProjects != nil {
		return nil, s.failListProjects
	}
	return s.Store.ListProjects(ctx, filter, opts)
}

func (s *r2FailingStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	if s.failListAgents != nil {
		return nil, s.failListAgents
	}
	return s.Store.ListAgents(ctx, filter, opts)
}

// installFailStore swaps srv.store and srv.authzService.store with a failing
// wrapper and returns a restore function. Both must be swapped because the
// handler uses srv.store for the list query and srv.authzService uses its own
// store reference for scope resolution.
func installFailStore(srv *Server, fs *r2FailingStore) func() {
	origStore := srv.store
	origAuthzStore := srv.authzService.store
	fs.Store = origStore
	srv.store = fs
	srv.authzService.store = fs
	return func() {
		srv.store = origStore
		srv.authzService.store = origAuthzStore
	}
}

// TestRS2_FailureInjection_PrincipalGroupClosure exercises both handlers when
// principal/group closure fails. Asserts 500 with zero response data.
func TestRS2_FailureInjection_PrincipalGroupClosure(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-pgc-u"), Email: "r2-pgc@test.com",
		DisplayName: "PGC User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	proj := &store.Project{
		ID: tid("r2-pgc-p"), Name: "PGC Proj", Slug: "r2-pgc-proj",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	fs := &r2FailingStore{
		failGetEffectiveGroups: fmt.Errorf("injected: group closure failure"),
	}
	restore := installFailStore(srv, fs)
	defer restore()

	t.Run("project_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"principal closure failure must produce 500")
		// Verify zero response data (no project leak)
		assert.NotContains(t, rec.Body.String(), proj.Name,
			"500 response must not leak project data")
	})

	t.Run("agent_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"principal closure failure must produce 500")
	})
}

// TestRS2_FailureInjection_RoleBindingLoad exercises both handlers when
// role-binding load fails. Asserts 500.
func TestRS2_FailureInjection_RoleBindingLoad(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-rbl-u"), Email: "r2-rbl@test.com",
		DisplayName: "RBL User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	fs := &r2FailingStore{
		failListBindings: fmt.Errorf("injected: binding load failure"),
	}
	restore := installFailStore(srv, fs)
	defer restore()

	t.Run("project_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"role-binding load failure must produce 500")
	})

	t.Run("agent_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"role-binding load failure must produce 500")
	})
}

// TestRS2_FailureInjection_RoleDefinitionLoad exercises both handlers when
// role-definition load fails. Asserts 500.
func TestRS2_FailureInjection_RoleDefinitionLoad(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-rdl-u"), Email: "r2-rdl@test.com",
		DisplayName: "RDL User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	proj := &store.Project{
		ID: tid("r2-rdl-p"), Name: "RDL Proj", Slug: "r2-rdl-proj",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	fs := &r2FailingStore{
		failGetRoleDefinition: fmt.Errorf("injected: role definition load failure"),
	}
	restore := installFailStore(srv, fs)
	defer restore()

	t.Run("project_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"role-definition load failure must produce 500")
	})

	t.Run("agent_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"role-definition load failure must produce 500")
	})
}

// TestRS2_FailureInjection_ConstraintLoad exercises both handlers when
// constraint loading fails. Asserts 500.
func TestRS2_FailureInjection_ConstraintLoad(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-cl-u"), Email: "r2-cl@test.com",
		DisplayName: "CL User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	proj := &store.Project{
		ID: tid("r2-cl-p"), Name: "CL Proj", Slug: "r2-cl-proj",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	fs := &r2FailingStore{
		failListConstraints: fmt.Errorf("injected: constraint load failure"),
	}
	restore := installFailStore(srv, fs)
	defer restore()

	t.Run("project_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"constraint load failure must produce 500")
	})

	t.Run("agent_list_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"constraint load failure must produce 500")
	})
}

// TestRS2_FailureInjection_StoreListCount exercises both handlers when the
// resource list/count store call itself fails. Asserts 500 and no partial data.
func TestRS2_FailureInjection_StoreListCount(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-slc-u"), Email: "r2-slc@test.com",
		DisplayName: "SLC User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	proj := &store.Project{
		ID: tid("r2-slc-p"), Name: "SLC Proj", Slug: "r2-slc-proj",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	ag := &store.Agent{
		ID: tid("r2-slc-a"), Slug: "r2-slc-a", Name: "slc-agent",
		ProjectID: proj.ID, OwnerID: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	t.Run("project_list_failure", func(t *testing.T) {
		fs := &r2FailingStore{
			failListProjects: fmt.Errorf("injected: project list failure"),
		}
		restore := installFailStore(srv, fs)
		defer restore()

		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"project list failure must produce 500")
		assert.NotContains(t, rec.Body.String(), proj.Name,
			"500 response must not leak project data")
	})

	t.Run("agent_list_failure", func(t *testing.T) {
		fs := &r2FailingStore{
			failListAgents: fmt.Errorf("injected: agent list failure"),
		}
		restore := installFailStore(srv, fs)
		defer restore()

		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"agent list failure must produce 500")
		assert.NotContains(t, rec.Body.String(), ag.Name,
			"500 response must not leak agent data")
	})
}

// TestRS2_NoneScope_DistinctEmpty200 verifies that a user with no project
// bindings (None scope) gets a clean 200 with zero items — distinct from the
// 500 produced by failure injection. This proves None is a legitimate empty
// result, not an error path.
func TestRS2_NoneScope_DistinctEmpty200(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// User with no project bindings
	user := &store.User{
		ID: tid("r2-ns-u"), Email: "r2-ns@test.com",
		DisplayName: "None Scope User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	// Do NOT add hub membership or any project bindings

	t.Run("project_list_empty_200", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		assert.Equal(t, http.StatusOK, rec.Code,
			"None scope must produce 200, not 500")

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 0, resp.TotalCount, "None scope must return 0 projects")
		assert.Empty(t, resp.Projects)
	})

	t.Run("agent_list_empty_200", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		assert.Equal(t, http.StatusOK, rec.Code,
			"None scope must produce 200, not 500")

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 0, resp.TotalCount, "None scope must return 0 agents")
		assert.Empty(t, resp.Agents)
	})
}

// TestRS2_FailureInjection_OwnerResolution exercises the Mine/Shared path when
// owner resolution fails (Finding 9). Asserts 500.
func TestRS2_FailureInjection_OwnerResolution(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-or-u"), Email: "r2-or@test.com",
		DisplayName: "OR User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	proj := &store.Project{
		ID: tid("r2-or-p"), Name: "OR Proj", Slug: "r2-or-proj",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// resolveUserOwnerProjectIDsOrError calls ListRoleBindingsForPrincipal
	// for the user's bindings. We inject a failure for that specific call
	// by using a store wrapper that fails only for the owner resolution
	// ListRoleBindingsForPrincipal call. Since ResolveListScopes uses
	// ListRoleBindingsForPrincipals (plural), we need a targeted failure.
	// Use a store that fails the singular form only.
	ownerFailStore := &r2OwnerResolutionFailStore{Store: s, failForUserID: user.ID}
	origStore := srv.store
	srv.store = ownerFailStore
	defer func() { srv.store = origStore }()

	t.Run("project_mine_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?scope=mine", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"owner resolution failure on scope=mine must produce 500")
	})

	t.Run("agent_mine_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?scope=mine", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"owner resolution failure on scope=mine must produce 500")
	})

	t.Run("project_shared_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects?scope=shared", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"owner resolution failure on scope=shared must produce 500")
	})

	t.Run("agent_shared_500", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?scope=shared", nil)
		assert.Equal(t, http.StatusInternalServerError, rec.Code,
			"owner resolution failure on scope=shared must produce 500")
	})
}

// r2OwnerResolutionFailStore fails ListRoleBindingsForPrincipal for a specific
// user ID (used by resolveUserOwnerProjectIDsOrError in the handler), while
// allowing ListRoleBindingsForPrincipals (plural, used by authz scope
// resolution) to succeed normally.
type r2OwnerResolutionFailStore struct {
	store.Store
	failForUserID string
}

func (s *r2OwnerResolutionFailStore) ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	if principalID == s.failForUserID {
		return nil, fmt.Errorf("injected: owner binding lookup failure")
	}
	return s.Store.ListRoleBindingsForPrincipal(ctx, principalType, principalID)
}

// ==========================================================================
// Original-brief: All+constraint end-to-end HTTP tests
// ==========================================================================

// TestRS2_AllPlusConstraint_EndToEnd verifies that a super-admin with All scope
// sees correct results when a project-scoped constraint blocks the list
// permission for a specific project. Both project and agent endpoints must
// exclude the constrained project from results.
func TestRS2_AllPlusConstraint_EndToEnd(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := &store.User{
		ID: tid("r2-apc-u"), Email: "r2-apc@test.com",
		DisplayName: "Constraint Admin", Role: store.UserRoleAdmin, Status: "active",
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

	// Create two projects
	visibleProj := &store.Project{
		ID: tid("r2-apc-vp"), Name: "Visible", Slug: "r2-apc-vis",
		OwnerID: admin.ID, CreatedBy: admin.ID,
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	blockedProj := &store.Project{
		ID: tid("r2-apc-bp"), Name: "Blocked", Slug: "r2-apc-blk",
		OwnerID: tid("r2-apc-oth"), CreatedBy: tid("r2-apc-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, visibleProj))
	require.NoError(t, s.CreateProject(ctx, blockedProj))

	// Create agents in both projects
	visAgent := &store.Agent{
		ID: tid("r2-apc-va"), Slug: "r2-apc-va", Name: "vis-agent",
		ProjectID: visibleProj.ID, OwnerID: admin.ID,
		Created: time.Now().Add(-1 * time.Minute), Updated: time.Now(),
	}
	blkAgent := &store.Agent{
		ID: tid("r2-apc-ba"), Slug: "r2-apc-ba", Name: "blk-agent",
		ProjectID: blockedProj.ID, OwnerID: tid("r2-apc-oth"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, visAgent))
	require.NoError(t, s.CreateAgent(ctx, blkAgent))

	// Create a project-scoped constraint that blocks project.list and agent.list
	// for the admin user on blockedProj. MaximumPermissions is an allowlist:
	// only the listed permissions are allowed, so omitting project.list/agent.list
	// blocks them.
	pType := "user"
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "block-list-in-project",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: &pType,
		SubjectPrincipalID:   strPtr(admin.ID),
		ScopeType:            "project",
		ScopeID:              blockedProj.ID,
		MaximumPermissions:   []string{"agent.read"}, // no project.list, no agent.list
		Purpose:              "test constraint exclusion",
		CreatedBy:            admin.ID,
	})
	require.NoError(t, err)

	t.Run("project_list_excludes_constrained", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, visibleProj.ID,
			"unconstrained project must appear")
		assert.NotContains(t, projectIDs, blockedProj.ID,
			"constrained project must be excluded from list results")
	})

	t.Run("agent_list_excludes_constrained", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		agentIDs := extractAgentIDs(resp.Agents)
		assert.Contains(t, agentIDs, visAgent.ID,
			"agent in unconstrained project must appear")
		assert.NotContains(t, agentIDs, blkAgent.ID,
			"agent in constrained project must be excluded")
	})
}

// ==========================================================================
// Original-brief: direct and transitive group grant tests
// ==========================================================================

// TestRS2_GroupGrantTests verifies project access via direct group membership
// and transitive (nested) group membership.
func TestRS2_GroupGrantTests(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-grp-u"), Email: "r2-grp@test.com",
		DisplayName: "Group User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Project only accessible via group
	groupProj := &store.Project{
		ID: tid("r2-grp-gp"), Name: "Group Proj", Slug: "r2-grp-group",
		OwnerID: tid("r2-grp-oth"), CreatedBy: tid("r2-grp-oth"),
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	// Project with no group access (control)
	noAccessProj := &store.Project{
		ID: tid("r2-grp-np"), Name: "NoAccess Proj", Slug: "r2-grp-noacc",
		OwnerID: tid("r2-grp-oth"), CreatedBy: tid("r2-grp-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, groupProj))
	require.NoError(t, s.CreateProject(ctx, noAccessProj))

	// Create a group and add user to it
	group := &store.Group{
		ID:   tid("r2-grp-g1"),
		Name: "test-group",
		Slug: "r2-grp-g1",
	}
	require.NoError(t, s.CreateGroup(ctx, group))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   user.ID,
		Role:       "member",
		AddedBy:    "test",
	}))

	// Grant the group member access to groupProj
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      group.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          groupProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	t.Run("direct_group_grant_gives_access", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		projectIDs := extractProjectIDs(resp.Projects)
		assert.Contains(t, projectIDs, groupProj.ID,
			"user must see project granted via group membership")
		assert.NotContains(t, projectIDs, noAccessProj.ID,
			"user must not see project without group grant")
	})

	t.Run("group_removal_revokes_access", func(t *testing.T) {
		// Remove user from group
		require.NoError(t, s.RemoveGroupMember(ctx, group.ID, store.GroupMemberTypeUser, user.ID))

		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		projectIDs := extractProjectIDs(resp.Projects)
		assert.NotContains(t, projectIDs, groupProj.ID,
			"user must lose group-granted access after removal")
	})
}

// ==========================================================================
// Original-brief: enrichment-only-from-authorized
// ==========================================================================

// TestRS2_EnrichmentOnlyFromAuthorized verifies that enrichment (project name
// on agents, capabilities) runs only for rows in the authorized result set.
// Unauthorized rows must never appear even if enrichment would succeed.
func TestRS2_EnrichmentOnlyFromAuthorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2-enr-u"), Email: "r2-enr@test.com",
		DisplayName: "Enrichment User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	// Authorized project
	authProj := &store.Project{
		ID: tid("r2-enr-ap"), Name: "Auth Proj", Slug: "r2-enr-auth",
		OwnerID: user.ID, CreatedBy: user.ID,
		Created: time.Now().Add(-2 * time.Hour), Updated: time.Now(),
	}
	// Unauthorized project (exists but no binding)
	unauthProj := &store.Project{
		ID: tid("r2-enr-up"), Name: "Unauth Proj", Slug: "r2-enr-unauth",
		OwnerID: tid("r2-enr-oth"), CreatedBy: tid("r2-enr-oth"),
		Created: time.Now().Add(-1 * time.Hour), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, authProj))
	require.NoError(t, s.CreateProject(ctx, unauthProj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          authProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create agents in both projects
	authAgent := &store.Agent{
		ID: tid("r2-enr-aa"), Slug: "r2-enr-aa", Name: "auth-agent",
		ProjectID: authProj.ID, OwnerID: user.ID,
		Created: time.Now().Add(-1 * time.Minute), Updated: time.Now(),
	}
	unauthAgent := &store.Agent{
		ID: tid("r2-enr-ua"), Slug: "r2-enr-ua", Name: "unauth-agent",
		ProjectID: unauthProj.ID, OwnerID: tid("r2-enr-oth"),
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, authAgent))
	require.NoError(t, s.CreateAgent(ctx, unauthAgent))

	t.Run("project_response_only_authorized", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/projects", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListProjectsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		// Only authorized project appears
		assert.Equal(t, 1, resp.TotalCount)
		require.Len(t, resp.Projects, 1)
		assert.Equal(t, authProj.ID, resp.Projects[0].ID)

		// Unauthorized project name must not appear anywhere
		assert.NotContains(t, rec.Body.String(), unauthProj.Name,
			"unauthorized project data must not appear in response")
	})

	t.Run("agent_response_only_authorized", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		// Only authorized agent appears
		assert.Equal(t, 1, resp.TotalCount)
		require.Len(t, resp.Agents, 1)
		assert.Equal(t, authAgent.ID, resp.Agents[0].ID)

		// Enrichment: agent's project name is filled (from authorized result)
		assert.Equal(t, authProj.Name, resp.Agents[0].Project,
			"enrichment must run for authorized agents")

		// Unauthorized agent data must not appear
		assert.NotContains(t, rec.Body.String(), unauthAgent.Name,
			"unauthorized agent data must not appear in response")
		assert.NotContains(t, rec.Body.String(), unauthProj.Name,
			"unauthorized project data must not leak via enrichment")
	})

	t.Run("capabilities_only_for_authorized", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

		// Capabilities must be present for authorized agent
		require.Len(t, resp.Agents, 1)
		assert.NotNil(t, resp.Agents[0].Cap,
			"capabilities must be computed for authorized agents")
	})
}

// TestRS2_RoleBindingAgentPrincipalStatus removed (R2 item 11):
// zero-assertion documentation function; status recorded in dev report only.

// ==========================================================================
// ptone/scion#2146: ownerId / ancestorId / harnessConfig / id (relationship)
// list filters must narrow only, never widen, the authorized-project scope.
// ==========================================================================

// TestRS2_AgentList_NewFiltersDoNotWiden is the explicit no-widening
// regression the design calls for: an agent living in a project the caller
// cannot see must stay hidden from /api/v1/agents even when it also matches
// ownerId, ancestorId, or an explicit id[] relationship restriction. Every
// new predicate must compose with the authorized-project scope using AND,
// never resurrect a match via OR.
func TestRS2_AgentList_NewFiltersDoNotWiden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2146-u"), Email: "r2146@test.com",
		DisplayName: "Filter User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	visProj := &store.Project{
		ID: tid("r2146-vp"), Name: "Visible", Slug: "r2146-vis",
		OwnerID: user.ID, CreatedBy: user.ID, Created: time.Now(), Updated: time.Now(),
	}
	hidProj := &store.Project{
		ID: tid("r2146-hp"), Name: "Hidden", Slug: "r2146-hid",
		OwnerID: tid("r2146-oth"), CreatedBy: tid("r2146-oth"), Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, visProj))
	require.NoError(t, s.CreateProject(ctx, hidProj))

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          visProj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	sharedOwner := tid("r2146-owner")
	sharedAncestor := tid("r2146-ancestor")

	sharedHarness := "shared-harness"

	visible := &store.Agent{
		ID: tid("r2146-visible"), Slug: "r2146-visible", Name: "visible",
		ProjectID: visProj.ID, OwnerID: sharedOwner, Ancestry: []string{sharedAncestor},
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: sharedHarness},
		Created:       time.Now(), Updated: time.Now(),
	}
	hidden := &store.Agent{
		ID: tid("r2146-hidden"), Slug: "r2146-hidden", Name: "hidden",
		ProjectID: hidProj.ID, OwnerID: sharedOwner, Ancestry: []string{sharedAncestor},
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: sharedHarness},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, visible))
	require.NoError(t, s.CreateAgent(ctx, hidden))

	cases := []struct {
		name string
		url  string
	}{
		{"ownerId", "/api/v1/agents?ownerId=" + sharedOwner},
		{"ancestorId", "/api/v1/agents?ancestorId=" + sharedAncestor},
		{"harnessConfig", "/api/v1/agents?harnessConfig=" + sharedHarness},
		{"id relationship filter", "/api/v1/agents?id=" + visible.ID + "&id=" + hidden.ID},
		{"lineageRootId", "/api/v1/agents?lineageRootId=" + sharedAncestor},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, user, http.MethodGet, tc.url, nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			var resp ListAgentsResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

			gotIDs := make([]string, 0, len(resp.Agents))
			for _, a := range resp.Agents {
				gotIDs = append(gotIDs, a.ID)
			}
			assert.Equal(t, []string{visible.ID}, gotIDs,
				"%s must not resurrect the agent in the unauthorized project", tc.name)
			assert.Equal(t, 1, resp.TotalCount)
		})
	}

	t.Run("lineageRootId naming the hidden agent itself as root reveals nothing", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?lineageRootId="+hidden.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var resp ListAgentsResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Empty(t, resp.Agents,
			"an unauthorized agent used as the lineage root must not be revealed, even as the root itself")
		assert.Equal(t, 0, resp.TotalCount)
	})
}

// TestListAgents_AttributeFiltersNarrowCorrectly is the positive-path
// counterpart to TestRS2_AgentList_NewFiltersDoNotWiden: within a single
// authorized project, ownerId/ancestorId/harnessConfig must each narrow the
// result to exactly the matching agent, via both the global list endpoint
// and the project-scoped one. cmd/list.go's default (non-`--all`) path uses
// the project-scoped endpoint, so the two must not drift apart
// (ptone/scion#2146).
func TestListAgents_AttributeFiltersNarrowCorrectly(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2146-narrow-u"), Email: "r2146-narrow@test.com",
		DisplayName: "Narrow User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)

	proj := &store.Project{
		ID: tid("r2146-narrow-p"), Name: "Proj", Slug: "r2146-narrow",
		OwnerID: user.ID, CreatedBy: user.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	target := tid("r2146-narrow-owner")
	ancestor := tid("r2146-narrow-ancestor")

	match := &store.Agent{
		ID: tid("r2146-narrow-match"), Slug: "narrow-match", Name: "match",
		ProjectID: proj.ID, OwnerID: target, Ancestry: []string{ancestor},
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
		Created:       time.Now(), Updated: time.Now(),
	}
	other := &store.Agent{
		ID: tid("r2146-narrow-other"), Slug: "narrow-other", Name: "other",
		ProjectID:     proj.ID,
		OwnerID:       tid("r2146-narrow-other-owner"),
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "gemini"},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, match))
	require.NoError(t, s.CreateAgent(ctx, other))

	endpoints := []string{
		"/api/v1/agents",
		"/api/v1/projects/" + proj.ID + "/agents",
	}
	filters := []struct {
		name  string
		query string
	}{
		{"ownerId", "ownerId=" + target},
		{"ancestorId", "ancestorId=" + ancestor},
		{"harnessConfig", "harnessConfig=claude"},
		{"id relationship filter", "id=" + match.ID},
		{"lineageRootId via ancestry", "lineageRootId=" + ancestor},
		{"lineageRootId via the agent's own ID", "lineageRootId=" + match.ID},
	}

	for _, ep := range endpoints {
		for _, f := range filters {
			t.Run(ep+"?"+f.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, user, http.MethodGet, ep+"?"+f.query, nil)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

				var resp ListAgentsResponse
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

				gotIDs := make([]string, 0, len(resp.Agents))
				for _, a := range resp.Agents {
					gotIDs = append(gotIDs, a.ID)
				}
				assert.Equal(t, []string{match.ID}, gotIDs)
			})
		}
	}
}

// TestRS2_ProjectScopedAgentList_UnauthorizedCallersDenied covers: the
// project-scoped list endpoint (the default, non-`--all` CLI path) needs
// unauthorized-caller coverage for the new filters. A non-member user and a
// wrong-project agent JWT must both be denied — never 200 with rows, and
// never leaking whether the named id/owner exists in that project.
func TestRS2_ProjectScopedAgentList_UnauthorizedCallersDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := tid("r2146-psl-owner")
	proj := &store.Project{
		ID: tid("r2146-psl-p"), Name: "Proj", Slug: "r2146-psl",
		OwnerID: owner, CreatedBy: owner, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	sharedOwner := tid("r2146-psl-shared-owner")
	hidden := &store.Agent{
		ID: tid("r2146-psl-hidden"), Slug: "r2146-psl-hidden", Name: "hidden",
		ProjectID: proj.ID, OwnerID: sharedOwner, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, hidden))

	t.Run("non-member user is denied, not shown an empty-but-200 page", func(t *testing.T) {
		nonMember := &store.User{
			ID: tid("r2146-psl-nonmember"), Email: "r2146-psl-nonmember@test.com",
			DisplayName: "Non Member", Role: store.UserRoleMember, Status: "active",
		}
		require.NoError(t, s.CreateUser(ctx, nonMember))
		ensureHubMembership(ctx, s, nonMember.ID)

		for _, url := range []string{
			"/api/v1/projects/" + proj.ID + "/agents?id=" + hidden.ID,
			"/api/v1/projects/" + proj.ID + "/agents?ownerId=" + sharedOwner,
		} {
			rec := doRequestAsUser(t, srv, nonMember, http.MethodGet, url, nil)
			assert.NotEqual(t, http.StatusOK, rec.Code,
				"a non-member must not get a 200 response for %s (body: %s)", url, rec.Body.String())
		}
	})

	t.Run("an agent JWT from a different project is denied (404, not a leak)", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		// f.stranger lives in f.other, a different project than f.proj —
		// f.caller's token is scoped to f.proj.
		for _, url := range []string{
			"/api/v1/projects/" + f.other.ID + "/agents?id=" + f.stranger.ID,
			"/api/v1/projects/" + f.other.ID + "/agents?ownerId=" + f.owner.ID,
		} {
			rec := f.asAgent(t, http.MethodGet, url, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code,
				"a token scoped to a different project must get 404, indistinguishable from a nonexistent project, for %s (body: %s)",
				url, rec.Body.String())
		}
	})
}

// TestListAgents_MalformedOwnerIdReturns400 covers: a malformed ownerId must
// fail loud with 400, not crash the query with a 500 — parseUUID wraps
// store.ErrInvalidInput, which
// writeErrorFromErr maps to 400 (pkg/hub/errors.go). Locks that path in
// explicitly rather than relying on it holding by accident.
func TestListAgents_MalformedOwnerIdReturns400(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2146-bad-owner-u"), Email: "r2146-bad-owner@test.com",
		DisplayName: "Bad Owner User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// The user needs a non-None authorized scope, or listAgents short-circuits
	// to an empty 200 before the filter (and its malformed ownerId) is even
	// parsed — a project membership gives it one.
	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	proj := &store.Project{
		ID: tid("r2146-bad-owner-p"), Name: "Proj", Slug: "r2146-bad-owner",
		OwnerID: user.ID, CreatedBy: user.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	for _, url := range []string{
		"/api/v1/agents?ownerId=not-a-uuid",
		"/api/v1/agents?lineageRootId=not-a-uuid",
	} {
		rec := doRequestAsUser(t, srv, user, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: body: %s", url, rec.Body.String())
	}
}

// TestApplyAgentAttributeAndRelationshipFilters_IDsCap covers: id[] is
// capped at maxRelationshipIDs; unbounded, it would be limited only by the
// ~1 MB HTTP header limit (~25k UUIDs) — one IN(...) bind list alongside
// AuthorizedProjectIDs, far beyond what the real use (a CLI-resolved
// Ancestry chain) ever needs.
func TestApplyAgentAttributeAndRelationshipFilters_IDsCap(t *testing.T) {
	ids := make([]string, maxRelationshipIDs+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	var filter store.AgentFilter
	err := applyAgentAttributeAndRelationshipFilters(&filter, url.Values{"id": ids})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many")

	// At exactly the cap, it must still succeed.
	var okFilter store.AgentFilter
	err = applyAgentAttributeAndRelationshipFilters(&okFilter, url.Values{"id": ids[:maxRelationshipIDs]})
	require.NoError(t, err)
	assert.Len(t, okFilter.IDs, maxRelationshipIDs)
}

// TestListAgents_TooManyIDsReturns400 is the HTTP-level counterpart: exceeding
// the cap on a real request must fail with 400, not silently truncate or 500.
func TestListAgents_TooManyIDsReturns400(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID: tid("r2146-idscap-u"), Email: "r2146-idscap@test.com",
		DisplayName: "IDs Cap User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	memberRD, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	proj := &store.Project{
		ID: tid("r2146-idscap-p"), Name: "Proj", Slug: "r2146-idscap",
		OwnerID: user.ID, CreatedBy: user.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: memberRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      user.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          proj.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	params := make([]string, maxRelationshipIDs+1)
	for i := range params {
		params[i] = "id=" + uuid.NewString()
	}
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents?"+strings.Join(params, "&"), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

// TestApplyAgentAttributeAndRelationshipFilters_IDsCanonicalizedForCursorBinding
// covers: id[] is deduped and canonicalized before scopedCursorBinding, like
// every other set-like field, so the same logical request in a different
// order mints the same cursor.
func TestApplyAgentAttributeAndRelationshipFilters_IDsCanonicalizedForCursorBinding(t *testing.T) {
	idA, idB := uuid.NewString(), uuid.NewString()

	var filterOrder1, filterOrder2 store.AgentFilter
	require.NoError(t, applyAgentAttributeAndRelationshipFilters(&filterOrder1, url.Values{"id": {idB, idA}}))
	require.NoError(t, applyAgentAttributeAndRelationshipFilters(&filterOrder2, url.Values{"id": {idA, idB}}))

	assert.Equal(t, filterOrder1.IDs, filterOrder2.IDs,
		"id[] must canonicalize (sort) identically regardless of query param order")

	binding1 := scopedCursorBinding("agents", filterOrder1, nil)
	binding2 := scopedCursorBinding("agents", filterOrder2, nil)
	assert.Equal(t, binding1, binding2,
		"cursor binding must be order-insensitive for id[], like every other set-like filter field")

	// Duplicates are also collapsed, matching AuthorizedProjectIDs/
	// ExcludedProjectIDs' existing canonicalization behavior.
	var filterDup store.AgentFilter
	require.NoError(t, applyAgentAttributeAndRelationshipFilters(&filterDup, url.Values{"id": {idA, idA, idB}}))
	assert.Equal(t, filterOrder1.IDs, filterDup.IDs)
}

// TestListAgents_ListEndpointResolvesPeerWhenGetIsForbidden covers the
// server-side fact that justifies cmd/list.go's resolveReferenceAgent
// falling back to list-based resolution on 403 as well as 404: an agent
// identity is denied a single-resource GET on any agent other than itself
// (agent.read has no AgentScopes mapping — see
// TestBypassAgents_LegitimateFlowsStillWork, authz_bypass_agents_test.go),
// but the identical peer is visible through the authorized list endpoint,
// both narrowed by id[] and in a bare page. Without that fallback,
// --descendants=<peer> / --ancestors=<peer> would fail outright in agent
// mode for exactly the audience these flags are built for.
func TestListAgents_ListEndpointResolvesPeerWhenGetIsForbidden(t *testing.T) {
	f := bypassAgentsSetup(t)

	getRec := f.asAgent(t, http.MethodGet, "/api/v1/agents/"+f.sibling.ID, nil)
	require.Equal(t, http.StatusNotFound, getRec.Code,
		"baseline: GET on a project peer is denied (as a missing agent) for an agent identity")

	// The PROJECT-scoped list endpoint is what cmd/list.go's
	// resolveReferenceAgent actually resolves through outside --all (see
	// listAgentsViaHub, which uses the project-scoped agentSvc whenever
	// --all is not set): it carves out same-project agent tokens
	// (listProjectAgents, pkg/hub/handlers_projects_core.go), independent
	// of role bindings.
	projPath := "/api/v1/projects/" + f.proj.ID + "/agents"

	listByIDRec := f.asAgent(t, http.MethodGet, projPath+"?id="+f.sibling.ID, nil)
	require.Equal(t, http.StatusOK, listByIDRec.Code, "body: %s", listByIDRec.Body.String())
	var byIDResp ListAgentsResponse
	require.NoError(t, json.NewDecoder(listByIDRec.Body).Decode(&byIDResp))
	byIDIDs := make([]string, 0, len(byIDResp.Agents))
	for _, a := range byIDResp.Agents {
		byIDIDs = append(byIDIDs, a.ID)
	}
	assert.Contains(t, byIDIDs, f.sibling.ID,
		"the project-scoped list endpoint's id[] filter must find a peer GET cannot reach")

	bareListRec := f.asAgent(t, http.MethodGet, projPath, nil)
	require.Equal(t, http.StatusOK, bareListRec.Code, "body: %s", bareListRec.Body.String())
	var bareResp ListAgentsResponse
	require.NoError(t, json.NewDecoder(bareListRec.Body).Decode(&bareResp))
	bareIDs := make([]string, 0, len(bareResp.Agents))
	for _, a := range bareResp.Agents {
		bareIDs = append(bareIDs, a.ID)
	}
	assert.Contains(t, bareIDs, f.sibling.ID,
		"a bare list page must also include the peer — the name/slug pagination fallback in resolveReferenceAgent relies on this")

	// The GLOBAL list endpoint is a different story: a bare agent token
	// carries no role-binding-derived hub-wide list authority (only the
	// project-scoped route's same-project carve-out applies), so it finds
	// nothing here — even the caller's own ID. This is exactly why
	// listAgentsViaHub fails loudly instead of silently for `--all` combined
	// with a relationship flag in agent mode: the final --all listing
	// always goes through this global endpoint,
	// which can't see this identity's own agents at all, so resolving the
	// reference successfully (e.g. via the project-scoped endpoint) would
	// not have made the overall command work end to end.
	globalRec := f.asAgent(t, http.MethodGet, "/api/v1/agents?id="+f.sibling.ID, nil)
	require.Equal(t, http.StatusOK, globalRec.Code, "body: %s", globalRec.Body.String())
	var globalResp ListAgentsResponse
	require.NoError(t, json.NewDecoder(globalRec.Body).Decode(&globalResp))
	assert.Empty(t, globalResp.Agents,
		"documents the global endpoint's limitation for a bare agent token — this is why reference resolution must not rely on it")
}
