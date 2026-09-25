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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// ptone/scion#1974: pagination correctness regression for the per-item scan
// path (authorizedList in authorized_list.go). This suite seeds more than
// one page of authorized ("visible") items interleaved with unauthorized
// ("hidden") ones and walks every page at several limits, for both list
// endpoints that go through authorizedList's per-item scan (templates,
// harness configs) plus the group list, which shares the same function.
//
// Each (limit, extra) combination below seeds its own small list sized
// 2*limit+extra visible items: extra=0 lands the visible count exactly on a
// limit boundary (the last page ends exactly full), extra=1 lands one item
// past it (the last page is a single leftover item). Sizing per limit
// instead of sharing one large list across all four limits keeps every case
// to at most three pages, regardless of how small the limit is.
// ============================================================================

// authorizedListPageBoundaryLimits are the page limits under test: 1, 10,
// 25, and the default (authorizedListBatchSize).
var authorizedListPageBoundaryLimits = []int{1, 10, 25, authorizedListBatchSize}

// authorizedListPageBoundaryExtras is added to 2*limit to size each seeded
// list: extra=0 lands the visible count exactly on a limit boundary (the
// last page ends exactly full), extra=1 lands one item past it (the last
// page is a single leftover item) — the two edge cases from the issue.
var authorizedListPageBoundaryExtras = []int{0, 1}

// assertAuthorizedListPagesCoverExpected walks the collected pages of a
// paginated list and asserts: every page but the last is full (page size ==
// limit); the union of every page's IDs equals exactly the expected
// (visible) set, so no visible item was dropped and no hidden item leaked
// in; no ID repeats across pages; and the page count is exactly
// ceil(len(expected)/limit).
func assertAuthorizedListPagesCoverExpected(t *testing.T, pages [][]string, expected map[string]bool, limit int) {
	t.Helper()
	seen := make(map[string]bool)
	union := make(map[string]bool)
	for i, page := range pages {
		if i < len(pages)-1 {
			assert.Len(t, page, limit, "page %d of %d should be full", i+1, len(pages))
		}
		for _, id := range page {
			require.False(t, seen[id], "item %s returned on more than one page", id)
			seen[id] = true
			union[id] = true
		}
	}
	assert.Equal(t, expected, union, "union of all pages must equal the expected visible set: no drops at a page boundary, no hidden item leaked")
	wantPages := (len(expected) + limit - 1) / limit
	assert.Equal(t, wantPages, len(pages), "page count must equal ceil(visible count / limit)")
}

// seedInterleavedTemplates creates 3*visibleCount templates with explicit,
// strictly decreasing Created timestamps so list order (newest-first) is
// pinned regardless of wall-clock timestamp ties: the item at list position
// i is a carol-owned, user-scoped (visible) template when (i+1)%3==0, and
// otherwise an alice-owned, project-scoped (hidden, outside carol's
// authorized scope) template. Returns the set of visible template IDs.
func seedInterleavedTemplates(t *testing.T, s store.Store, carolID, aliceID, projectID string, visibleCount int) map[string]bool {
	t.Helper()
	total := 3 * visibleCount
	base := time.Now()
	visible := make(map[string]bool, visibleCount)
	for i := 0; i < total; i++ {
		created := base.Add(-time.Duration(i) * time.Millisecond)
		scope, scopeID, owner, prefix := store.TemplateScopeProject, projectID, aliceID, "boundary-hidden"
		isVisible := (i+1)%3 == 0
		if isVisible {
			scope, scopeID, owner, prefix = store.TemplateScopeUser, carolID, carolID, "boundary-visible"
		}
		name := fmt.Sprintf("%s-tmpl-%d", prefix, i)
		tpl := &store.Template{
			ID:          api.NewUUID(),
			Name:        name,
			Slug:        api.Slugify(name),
			Scope:       scope,
			ScopeID:     scopeID,
			OwnerID:     owner,
			Status:      "active",
			StoragePath: fmt.Sprintf("templates/%s/%s", scope, api.Slugify(name)),
			Created:     created,
			Updated:     created,
		}
		require.NoError(t, s.CreateTemplate(context.Background(), tpl))
		if isVisible {
			visible[tpl.ID] = true
		}
	}
	return visible
}

// seedInterleavedHarnessConfigs is the harness-config twin of
// seedInterleavedTemplates.
func seedInterleavedHarnessConfigs(t *testing.T, s store.Store, carolID, aliceID, projectID string, visibleCount int) map[string]bool {
	t.Helper()
	total := 3 * visibleCount
	base := time.Now()
	visible := make(map[string]bool, visibleCount)
	for i := 0; i < total; i++ {
		created := base.Add(-time.Duration(i) * time.Millisecond)
		scope, scopeID, owner, prefix := store.HarnessConfigScopeProject, projectID, aliceID, "boundary-hidden"
		isVisible := (i+1)%3 == 0
		if isVisible {
			scope, scopeID, owner, prefix = store.HarnessConfigScopeUser, carolID, carolID, "boundary-visible"
		}
		name := fmt.Sprintf("%s-hc-%d", prefix, i)
		hc := &store.HarnessConfig{
			ID:          api.NewUUID(),
			Name:        name,
			Slug:        api.Slugify(name),
			Harness:     "claude",
			Scope:       scope,
			ScopeID:     scopeID,
			OwnerID:     owner,
			Status:      store.HarnessConfigStatusActive,
			StoragePath: fmt.Sprintf("harness-configs/%s/%s", scope, api.Slugify(name)),
			Config:      &store.HarnessConfigData{Image: "example/claude:latest"},
			Created:     created,
			Updated:     created,
		}
		require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
		if isVisible {
			visible[hc.ID] = true
		}
	}
	return visible
}

// seedInterleavedGroups creates 3*visibleCount groups with explicit,
// strictly decreasing Created timestamps: the group at list position i
// belongs to visibleProjectID (visible, in scope) when (i+1)%3==0, and
// otherwise to hiddenProjectID (out of scope). Returns the set of visible
// group IDs.
func seedInterleavedGroups(t *testing.T, s store.Store, ownerID, visibleProjectID, hiddenProjectID string, visibleCount int) map[string]bool {
	t.Helper()
	total := 3 * visibleCount
	base := time.Now()
	visible := make(map[string]bool, visibleCount)
	for i := 0; i < total; i++ {
		created := base.Add(-time.Duration(i) * time.Millisecond)
		projectID, prefix := hiddenProjectID, "boundary-hidden"
		isVisible := (i+1)%3 == 0
		if isVisible {
			projectID, prefix = visibleProjectID, "boundary-visible"
		}
		name := fmt.Sprintf("%s-grp-%d", prefix, i)
		g := &store.Group{
			ID:        api.NewUUID(),
			Name:      name,
			Slug:      api.Slugify(name) + "-" + fmt.Sprint(i),
			GroupType: store.GroupTypeExplicit,
			ProjectID: projectID,
			OwnerID:   ownerID,
			Created:   created,
			Updated:   created,
		}
		require.NoError(t, s.CreateGroup(context.Background(), g))
		if isVisible {
			visible[g.ID] = true
		}
	}
	return visible
}

// walkTemplatesAllPages walks /api/v1/templates as user, following
// NextCursor to exhaustion, and returns each page's item IDs.
func walkTemplatesAllPages(t *testing.T, srv *Server, user *store.User, limit int) [][]string {
	t.Helper()
	var pages [][]string
	cursor := ""
	for {
		path := fmt.Sprintf("/api/v1/templates?limit=%d", limit)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "page %d: got: %s", len(pages)+1, rec.Body.String())
		var resp ListTemplatesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := make([]string, len(resp.Templates))
		for i, tpl := range resp.Templates {
			ids[i] = tpl.ID
		}
		pages = append(pages, ids)
		if resp.NextCursor == "" {
			return pages
		}
		cursor = resp.NextCursor
		require.Less(t, len(pages), 500, "did not converge within a sane number of pages")
	}
}

// walkHarnessConfigsAllPages is the harness-config twin of
// walkTemplatesAllPages.
func walkHarnessConfigsAllPages(t *testing.T, srv *Server, user *store.User, limit int) [][]string {
	t.Helper()
	var pages [][]string
	cursor := ""
	for {
		path := fmt.Sprintf("/api/v1/harness-configs?limit=%d", limit)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := doRequestAsUser(t, srv, user, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "page %d: got: %s", len(pages)+1, rec.Body.String())
		var resp ListHarnessConfigsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := make([]string, len(resp.HarnessConfigs))
		for i, hc := range resp.HarnessConfigs {
			ids[i] = hc.ID
		}
		pages = append(pages, ids)
		if resp.NextCursor == "" {
			return pages
		}
		cursor = resp.NextCursor
		require.Less(t, len(pages), 500, "did not converge within a sane number of pages")
	}
}

// walkGroupsAllPages walks /api/v1/groups as identity (called directly
// against the handler, since the scoped identity used by the group boundary
// test is not a plain user token), following NextCursor to exhaustion, and
// returns each page's item IDs.
func walkGroupsAllPages(t *testing.T, srv *Server, identity Identity, limit int) [][]string {
	t.Helper()
	var pages [][]string
	cursor := ""
	for {
		path := fmt.Sprintf("/api/v1/groups?limit=%d", limit)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(contextWithIdentity(context.Background(), identity))
		rec := httptest.NewRecorder()
		srv.listGroups(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "page %d: got: %s", len(pages)+1, rec.Body.String())
		var resp ListGroupsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := make([]string, len(resp.Groups))
		for i, g := range resp.Groups {
			ids[i] = g.ID
		}
		pages = append(pages, ids)
		if resp.NextCursor == "" {
			return pages
		}
		cursor = resp.NextCursor
		require.Less(t, len(pages), 500, "did not converge within a sane number of pages")
	}
}

func TestAuthorizedList_Template_MultiPageBoundary_UnionNoDuplicates(t *testing.T) {
	for _, limit := range authorizedListPageBoundaryLimits {
		for _, extra := range authorizedListPageBoundaryExtras {
			visibleCount := 2*limit + extra
			t.Run(fmt.Sprintf("limit=%d/visible=%d", limit, visibleCount), func(t *testing.T) {
				srv, s, alice, carol, project := setupTemplateScopeTest(t)
				visible := seedInterleavedTemplates(t, s, carol.ID, alice.ID, project.ID, visibleCount)

				pages := walkTemplatesAllPages(t, srv, carol, limit)
				assertAuthorizedListPagesCoverExpected(t, pages, visible, limit)

				// Cursor stability: an independent second walk must return
				// the identical sequence of pages.
				again := walkTemplatesAllPages(t, srv, carol, limit)
				assert.Equal(t, pages, again, "a repeated walk over the same list must be stable")
			})
		}
	}
}

func TestAuthorizedList_HarnessConfig_MultiPageBoundary_UnionNoDuplicates(t *testing.T) {
	for _, limit := range authorizedListPageBoundaryLimits {
		for _, extra := range authorizedListPageBoundaryExtras {
			visibleCount := 2*limit + extra
			t.Run(fmt.Sprintf("limit=%d/visible=%d", limit, visibleCount), func(t *testing.T) {
				srv, s, alice, carol, project := setupHarnessConfigScopeTest(t)
				visible := seedInterleavedHarnessConfigs(t, s, carol.ID, alice.ID, project.ID, visibleCount)

				pages := walkHarnessConfigsAllPages(t, srv, carol, limit)
				assertAuthorizedListPagesCoverExpected(t, pages, visible, limit)

				again := walkHarnessConfigsAllPages(t, srv, carol, limit)
				assert.Equal(t, pages, again, "a repeated walk over the same list must be stable")
			})
		}
	}
}

// TestAuthorizedList_Group_MultiPageBoundary_UnionNoDuplicates is the group
// list's twin of the template/harness-config boundary tests above: groups
// share authorizedList's per-item scan (handlers_groups.go), so the same
// page-boundary drop applied to it. The visibility split here mirrors
// TestScopedAdminListEndpointsFilterCrossProjectRowsAndCountAuthorizedMatches
// (capabilities_test.go): a project-scoped role binding restricted to
// visibleProject makes that project's groups visible and the other
// project's groups hidden.
func TestAuthorizedList_Group_MultiPageBoundary_UnionNoDuplicates(t *testing.T) {
	for _, limit := range authorizedListPageBoundaryLimits {
		for _, extra := range authorizedListPageBoundaryExtras {
			visibleCount := 2*limit + extra
			t.Run(fmt.Sprintf("limit=%d/visible=%d", limit, visibleCount), func(t *testing.T) {
				srv, s := testServer(t)
				ctx := context.Background()

				admin := NewAuthenticatedUser(tid("grp-pb-admin"), "grp-pb-admin@test.com", "Admin", store.UserRoleAdmin, "api")
				require.NoError(t, s.CreateUser(ctx, &store.User{ID: admin.ID(), Email: admin.Email(), DisplayName: admin.DisplayName(), Role: store.UserRoleAdmin, Status: "active"}))
				visibleProject := &store.Project{ID: tid("grp-pb-visible-project"), Name: "Visible Project", Slug: "grp-pb-visible-project"}
				hiddenProject := &store.Project{ID: tid("grp-pb-hidden-project"), Name: "Hidden Project", Slug: "grp-pb-hidden-project"}
				require.NoError(t, s.CreateProject(ctx, visibleProject))
				require.NoError(t, s.CreateProject(ctx, hiddenProject))

				rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
					Name:        "grp-pb-reader",
					ScopeType:   store.RoleScopeProject,
					Permissions: []string{"group.read", "group.list"},
				})
				require.NoError(t, err)
				_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      admin.ID(),
					ScopeType:        store.RoleScopeProject,
					ScopeID:          visibleProject.ID,
					CreatedBy:        "test",
				})
				require.NoError(t, err)
				scoped := NewScopedUserIdentity(admin, visibleProject.ID, []string{"group:read", "group:list"})

				visible := seedInterleavedGroups(t, s, admin.ID(), visibleProject.ID, hiddenProject.ID, visibleCount)

				pages := walkGroupsAllPages(t, srv, scoped, limit)
				assertAuthorizedListPagesCoverExpected(t, pages, visible, limit)
			})
		}
	}
}
