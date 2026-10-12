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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// listAuthorizedOrAll's direct (wide-access/admin) query path seals and
// opens cursors exactly like the per-item-scan path; these tests walk every
// page on that path.
// ---------------------------------------------------------------------------

// assertSealedCursorFormat asserts that nextCursor carries the sealed-cursor
// version prefix, not the legacy pre-sealing wire format.
func assertSealedCursorFormat(t *testing.T, cursor string) {
	t.Helper()
	assert.True(t, strings.HasPrefix(cursor, listCursorPrefix),
		"nextCursor %q must start with the sealed-cursor version prefix %q, not be a legacy plain cursor", cursor, listCursorPrefix)
}

func TestListTemplatesCursor_WideAccessDirectPathWalkIsSealedAndResumes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := NewAuthenticatedUser(tid("cursor-direct-tpl-admin"), "cursor-direct-tpl-admin@example.com", "Admin", store.UserRoleAdmin, "api")
	createTestUserWithRole(t, s, tid("cursor-direct-tpl-admin"), "cursor-direct-tpl-admin@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)

	project := &store.Project{ID: tid("cursor-direct-tpl-project"), Name: "Cursor Direct Templates", Slug: "cursor-direct-tpl-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	base := time.Now()
	wantIDs := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := tid(fmt.Sprintf("cursor-direct-tpl-%d", i))
		require.NoError(t, s.CreateTemplate(ctx, &store.Template{
			ID: id, Name: fmt.Sprintf("direct-tpl-%d", i), Slug: fmt.Sprintf("direct-tpl-%d", i),
			Harness: "codex", Scope: store.TemplateScopeProject, ScopeID: project.ID,
			Status: store.TemplateStatusActive, StoragePath: fmt.Sprintf("templates/project/direct-tpl-%d", i),
			Created: base.Add(-time.Duration(i) * time.Second), Updated: base.Add(-time.Duration(i) * time.Second),
		}))
		wantIDs[id] = true
	}

	request := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/templates?"+query, nil).WithContext(contextWithIdentity(ctx, admin))
		srv.listTemplatesV2(rec, req)
		return rec
	}

	gotIDs := map[string]bool{}
	query := fmt.Sprintf("projectId=%s&limit=1", project.ID)
	for page := 0; page < len(wantIDs)+1; page++ {
		rec := request(query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListTemplatesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Templates, 1, "page %d", page)
		id := resp.Templates[0].ID
		assert.False(t, gotIDs[id], "item %s returned twice across the walk", id)
		gotIDs[id] = true
		if resp.NextCursor == "" {
			break
		}
		assertSealedCursorFormat(t, resp.NextCursor)
		query = fmt.Sprintf("projectId=%s&limit=1&cursor=%s", project.ID, resp.NextCursor)
	}
	assert.Equal(t, wantIDs, gotIDs, "the direct-path walk must return exactly the seeded set, no duplicates, no gaps")
}

func TestListHarnessConfigsCursor_WideAccessDirectPathWalkIsSealedAndResumes(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	admin := NewAuthenticatedUser(tid("cursor-direct-hc-admin"), "cursor-direct-hc-admin@example.com", "Admin", store.UserRoleAdmin, "api")
	createTestUserWithRole(t, s, tid("cursor-direct-hc-admin"), "cursor-direct-hc-admin@example.com", store.UserRoleAdmin, store.SystemRoleSuperAdmin)

	project := &store.Project{ID: tid("cursor-direct-hc-project"), Name: "Cursor Direct Harness Configs", Slug: "cursor-direct-hc-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	base := time.Now()
	wantIDs := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := tid(fmt.Sprintf("cursor-direct-hc-%d", i))
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID: id, Name: fmt.Sprintf("direct-hc-%d", i), Slug: fmt.Sprintf("direct-hc-%d", i),
			Harness: "codex", Scope: store.HarnessConfigScopeProject, ScopeID: project.ID,
			Status:  store.HarnessConfigStatusActive,
			Created: base.Add(-time.Duration(i) * time.Second), Updated: base.Add(-time.Duration(i) * time.Second),
		}))
		wantIDs[id] = true
	}

	request := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/harness-configs?"+query, nil).WithContext(contextWithIdentity(ctx, admin))
		srv.listHarnessConfigs(rec, req)
		return rec
	}

	gotIDs := map[string]bool{}
	query := fmt.Sprintf("projectId=%s&limit=1", project.ID)
	for page := 0; page < len(wantIDs)+1; page++ {
		rec := request(query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListHarnessConfigsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.HarnessConfigs, 1, "page %d", page)
		id := resp.HarnessConfigs[0].ID
		assert.False(t, gotIDs[id], "item %s returned twice across the walk", id)
		gotIDs[id] = true
		if resp.NextCursor == "" {
			break
		}
		assertSealedCursorFormat(t, resp.NextCursor)
		query = fmt.Sprintf("projectId=%s&limit=1&cursor=%s", project.ID, resp.NextCursor)
	}
	assert.Equal(t, wantIDs, gotIDs, "the direct-path walk must return exactly the seeded set, no duplicates, no gaps")
}

// TestListTemplatesCursor_PerItemPathCursorAcceptedOnDirectPathForSameIdentity
// proves listAuthorizedOrAll's doc-comment claim: a cursor minted while an
// identity was on the per-item-scan path (authorizeEach) is still accepted
// when that same identity's authority grows into the direct-query path
// before the next page -- the cursor format never depends on which path
// minted it.
func TestListTemplatesCursor_PerItemPathCursorAcceptedOnDirectPathForSameIdentity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userID := tid("cursor-path-switch-user")
	user := &store.User{ID: userID, Email: "cursor-path-switch-user@test.com", DisplayName: "Path Switch", Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(ctx, user))

	project := &store.Project{ID: tid("cursor-path-switch-project"), Name: "Path Switch Project", Slug: "cursor-path-switch-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "cursor-path-switch-reader",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"template.read", "template.list"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	base := time.Now()
	for i, name := range []string{"path-switch-a", "path-switch-b"} {
		require.NoError(t, s.CreateTemplate(ctx, &store.Template{
			ID: api.NewUUID(), Name: name, Slug: api.Slugify(name), Harness: "codex",
			Scope: store.TemplateScopeProject, ScopeID: project.ID, OwnerID: tid("cursor-path-switch-owner"),
			Status: store.TemplateStatusActive, StoragePath: "templates/project/" + api.Slugify(name),
			Created: base.Add(-time.Duration(i) * time.Second), Updated: base.Add(-time.Duration(i) * time.Second),
		}))
	}

	query := fmt.Sprintf("projectId=%s&limit=1", project.ID)
	rec1, first := getTemplatesPage(t, srv, user, query)
	require.Equal(t, http.StatusOK, rec1.Code, rec1.Body.String())
	require.Len(t, first.Templates, 1, "the per-item-scan path must return the one visible item")
	require.NotEmpty(t, first.NextCursor, "a second template remains, so a resume cursor must be returned")

	// Grant super-admin: hasCatalogWideListAccess now reports true for this
	// identity, so page 2's identical query switches from the per-item scan
	// onto listAuthorizedOrAll's direct-query path.
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	rec2, second := getTemplatesPage(t, srv, user, query+"&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String(),
		"a cursor minted on the per-item-scan path must be accepted on the direct path for the same identity")
	require.Len(t, second.Templates, 1)
	assert.NotEqual(t, first.Templates[0].ID, second.Templates[0].ID, "the resumed page must not repeat the first item")
}
