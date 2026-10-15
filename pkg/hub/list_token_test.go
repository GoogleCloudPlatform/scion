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
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listPage is one decoded list response: the row IDs under itemsKey, the
// total and the next cursor.
type listPage struct {
	IDs        []string
	TotalCount int
	NextCursor string
}

func decodeListPage(t *testing.T, rec *httptest.ResponseRecorder, itemsKey string) listPage {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	var items []struct {
		ID string `json:"id"`
	}
	if body, ok := raw[itemsKey]; ok && string(body) != "null" {
		require.NoError(t, json.Unmarshal(body, &items))
	}
	page := listPage{IDs: []string{}}
	for _, it := range items {
		page.IDs = append(page.IDs, it.ID)
	}
	if body, ok := raw["totalCount"]; ok {
		require.NoError(t, json.Unmarshal(body, &page.TotalCount))
	}
	if body, ok := raw["nextCursor"]; ok {
		require.NoError(t, json.Unmarshal(body, &page.NextCursor))
	}
	return page
}

// mintRefused reports that minting scopes on boundary for userID fails.
func mintRefused(t *testing.T, srv *Server, userID string, boundary TokenBoundary, scopes ...string) bool {
	t.Helper()
	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "list-refused-" + uuid.NewString()[:8], Boundary: boundary, Scopes: scopes,
	})
	return err != nil
}

// TestListCaveats_ProjectBoundaryRequiresListPermissionInCeiling pins that
// a project-boundary token lists nothing for a permission its ceiling does
// not carry, the same rule as a hub boundary, and nothing for a permission
// that is not eligible for a project boundary. With the list permission it
// lists only its boundary project, end to end on GET /api/v1/agents.
func TestListCaveats_ProjectBoundaryRequiresListPermissionInCeiling(t *testing.T) {
	without := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("list-proj-user"), projectBoundary("proj-1"), []string{"agent:read"}, "list-proj-cred", bearerCeiling(t, "agent:read"), nil)
	for _, in := range []ScopeSet{ScopeSetAll(), ScopeSetExplicit("proj-1"), ScopeSetExplicit("proj-1", "proj-2")} {
		assert.True(t, applyCredentialCaveats(without, "agent.list", in).IsNone(), "a project ceiling without agent.list lists nothing from %v", in)
	}
	with := NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser("list-proj-user"), projectBoundary("proj-1"), []string{"agent:list"}, "list-proj-cred", bearerCeiling(t, "agent:list"), nil)
	assert.True(t, applyCredentialCaveats(with, "agent.list", ScopeSetAll()).Equal(ScopeSetExplicit("proj-1")))

	// A frozen legacy ceiling can carry a hub-only list permission; a
	// project boundary is not eligible for it.
	legacy := NewScopedUserIdentity(bearerUser("list-proj-legacy"), "proj-1", []string{"group:list"})
	require.True(t, legacy.Ceiling().Allows("group.list"), "fixture: the legacy ceiling carries group.list")
	assert.True(t, applyCredentialCaveats(legacy, "group.list", ScopeSetAll()).IsNone(), "group.list is not eligible for a project boundary")

	f := newInboxFixture(t)
	noList := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "agent:read"), http.MethodGet, "/api/v1/agents", nil), "agents")
	assert.Empty(t, noList.IDs)
	assert.Equal(t, 0, noList.TotalCount)

	listA := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "agent:list"), http.MethodGet, "/api/v1/agents", nil), "agents")
	assert.Equal(t, []string{f.agentA.ID}, listA.IDs)
	assert.Equal(t, 1, listA.TotalCount)

	listB := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projB), "agent:list"), http.MethodGet, "/api/v1/agents", nil), "agents")
	assert.Equal(t, []string{f.agentB.ID}, listB.IDs, "a token for another project lists only that project")

	hub := decodeListPage(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "agent:list"), http.MethodGet, "/api/v1/agents", nil), "agents")
	assert.Subset(t, hub.IDs, []string{f.agentA.ID, f.agentB.ID})

	session := decodeListPage(t, f.session(t, f.member, http.MethodGet, "/api/v1/agents", nil), "agents")
	assert.Equal(t, []string{f.agentA.ID}, session.IDs, "a session lists from its live grants")
}

// TestProjectListToken_RequiresProjectListSelector pins the project list
// rule for tokens: a token lists projects only with project:list in its
// ceiling, a project token lists only its boundary project, and a hub
// token lists every project its holder can list. project:list on a hub
// boundary is selectable only with a hub-wide grant of project.list; a
// project member selects it on its project's boundary.
func TestProjectListToken_RequiresProjectListSelector(t *testing.T) {
	f := newInboxFixture(t)
	const path = "/api/v1/projects"

	readOnly := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "project:read"), http.MethodGet, path, nil), "projects")
	assert.Empty(t, readOnly.IDs)
	assert.Equal(t, 0, readOnly.TotalCount)

	projA := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "project:list"), http.MethodGet, path, nil), "projects")
	assert.Equal(t, []string{f.projA}, projA.IDs)
	assert.Equal(t, 1, projA.TotalCount)

	projB := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projB), "project:list"), http.MethodGet, path, nil), "projects")
	assert.Equal(t, []string{f.projB}, projB.IDs, "a token for another project lists only that project")

	hubRead := decodeListPage(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:read"), http.MethodGet, path, nil), "projects")
	assert.Empty(t, hubRead.IDs, "a hub token without project:list lists nothing")

	hub := decodeListPage(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "project:list"), http.MethodGet, path, nil), "projects")
	assert.Subset(t, hub.IDs, []string{f.projA, f.projB})

	// A project member holds project.list only through its project role.
	assert.True(t, mintRefused(t, f.srv, f.member, hubBoundary(), "project:list"), "a member without a hub-wide project.list grant cannot select project:list on a hub boundary")
	member := decodeListPage(t, f.call(t, f.mint(t, f.member, projectBoundary(f.projA), "project:list"), http.MethodGet, path, nil), "projects")
	assert.Equal(t, []string{f.projA}, member.IDs)

	session := decodeListPage(t, f.session(t, f.member, http.MethodGet, path, nil), "projects")
	assert.Equal(t, []string{f.projA}, session.IDs)
}

// TestProjectAgentListToken_RequiresAgentListOnTheProject pins the project
// form of the agent list: a token needs agent:list and a boundary that
// contains the project.
func TestProjectAgentListToken_RequiresAgentListOnTheProject(t *testing.T) {
	f := newInboxFixture(t)
	path := "/api/v1/projects/" + f.projA + "/agents"

	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "agent:read"), http.MethodGet, path, nil).Code, "agent:read is not the list permission")
	assert.Equal(t, http.StatusForbidden, f.call(t, f.mint(t, f.admin, projectBoundary(f.projB), "agent:list"), http.MethodGet, path, nil).Code, "the project is outside the boundary")

	listA := decodeListPage(t, f.call(t, f.mint(t, f.admin, projectBoundary(f.projA), "agent:list"), http.MethodGet, path, nil), "agents")
	assert.Equal(t, []string{f.agentA.ID}, listA.IDs)
	hub := decodeListPage(t, f.call(t, f.mint(t, f.admin, hubBoundary(), "agent:list"), http.MethodGet, path, nil), "agents")
	assert.Equal(t, []string{f.agentA.ID}, hub.IDs)
	session := decodeListPage(t, f.session(t, f.member, http.MethodGet, path, nil), "agents")
	assert.Equal(t, []string{f.agentA.ID}, session.IDs)
}

// listSkillFixture adds skills to an inbox fixture: one global skill, two
// in project A, one in project B and one in another user's user scope.
type listSkillFixture struct {
	*inboxFixture
	global, a1, a2, b, otherUser string
}

func newListSkillFixture(t *testing.T) *listSkillFixture {
	t.Helper()
	f := &listSkillFixture{inboxFixture: newInboxFixture(t)}
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	add := func(name, scope, scopeID string, at time.Duration) string {
		id := uuid.NewString()
		created := base.Add(at)
		require.NoError(t, f.s.CreateSkill(ctx, &store.Skill{
			ID: id, Name: name + "-" + id[:8], Slug: name + "-" + id[:8], Scope: scope, ScopeID: scopeID,
			Status: "active", OwnerID: f.admin, Created: created, Updated: created,
		}))
		return id
	}
	f.global = add("list-global", store.SkillScopeGlobal, "", time.Minute)
	f.a1 = add("list-a1", store.SkillScopeProject, f.projA, 2*time.Minute)
	f.otherUser = add("list-other-user", store.SkillScopeUser, uuid.NewString(), 3*time.Minute)
	f.a2 = add("list-a2", store.SkillScopeProject, f.projA, 4*time.Minute)
	f.b = add("list-b", store.SkillScopeProject, f.projB, 5*time.Minute)
	return f
}

// TestSkillListToken_CountsOnlyAuthorizedRows pins the skill list rule for
// tokens: a token needs skill:list for any row and skill:read on each row,
// and its page, total and cursor cover only the rows it may read. A cursor
// opens only for the token it was issued to.
func TestSkillListToken_CountsOnlyAuthorizedRows(t *testing.T) {
	f := newListSkillFixture(t)
	const path = "/api/v1/skills"

	for name, scopes := range map[string][]string{
		"skill:list without skill:read": {"skill:list"},
		"skill:read without skill:list": {"skill:read"},
	} {
		page := decodeListPage(t, f.call(t, f.mint(t, f.admin, hubBoundary(), scopes...), http.MethodGet, path, nil), "skills")
		assert.Empty(t, page.IDs, name)
		assert.Equal(t, 0, page.TotalCount, name)
	}

	// The project token's read boundary offers the global skill too, but
	// the token cannot read a hub-level record, so it is neither listed
	// nor counted.
	keyA := f.mint(t, f.admin, projectBoundary(f.projA), "skill:list", "skill:read")
	all := decodeListPage(t, f.call(t, keyA, http.MethodGet, path, nil), "skills")
	assert.ElementsMatch(t, []string{f.a1, f.a2}, all.IDs)
	assert.Equal(t, 2, all.TotalCount)
	assert.Empty(t, all.NextCursor)

	first := decodeListPage(t, f.call(t, keyA, http.MethodGet, path+"?limit=1", nil), "skills")
	require.Len(t, first.IDs, 1)
	assert.Equal(t, 2, first.TotalCount, "the total counts every readable row, not the page")
	require.NotEmpty(t, first.NextCursor)
	second := decodeListPage(t, f.call(t, keyA, http.MethodGet, path+"?limit=1&cursor="+url.QueryEscape(first.NextCursor), nil), "skills")
	require.Len(t, second.IDs, 1)
	assert.ElementsMatch(t, []string{f.a1, f.a2}, append(first.IDs, second.IDs...))
	assert.Empty(t, second.NextCursor, "no readable row is left")

	otherKeyA := f.mint(t, f.admin, projectBoundary(f.projA), "skill:list", "skill:read")
	rec := f.call(t, otherKeyA, http.MethodGet, path+"?limit=1&cursor="+url.QueryEscape(first.NextCursor), nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a cursor opens only for the token it was issued to: %s", rec.Body.String())

	keyB := f.mint(t, f.admin, projectBoundary(f.projB), "skill:list", "skill:read")
	assert.Equal(t, []string{f.b}, decodeListPage(t, f.call(t, keyB, http.MethodGet, path, nil), "skills").IDs)

	member := decodeListPage(t, f.call(t, f.mint(t, f.member, hubBoundary(), "skill:list", "skill:read"), http.MethodGet, path, nil), "skills")
	assert.ElementsMatch(t, []string{f.global, f.a1, f.a2}, member.IDs, "a member's hub token lists the hub catalog and its project")
	assert.Equal(t, 3, member.TotalCount)

	session := decodeListPage(t, f.session(t, f.member, http.MethodGet, path, nil), "skills")
	assert.ElementsMatch(t, []string{f.global, f.a1, f.a2}, session.IDs)
	assert.NotContains(t, session.IDs, f.otherUser)
}

// TestCatalogWideList_UsesListSelectorThroughEvidence pins the unfiltered
// template and harness configuration list: it is used only through
// hub-level evidence for the list permission. A token takes it with the
// list selector on a hub boundary and hub-wide list authority; a token with
// only the read selector, on a project boundary, or without that authority
// gets the per-row read scan.
func TestCatalogWideList_UsesListSelectorThroughEvidence(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	tmplB := uuid.NewString()
	require.NoError(t, f.s.CreateTemplate(ctx, &store.Template{ID: tmplB, Name: "wide-tmpl-b", Slug: "wide-tmpl-b-" + tmplB[:8], Scope: store.TemplateScopeProject, ScopeID: f.projB, Harness: "claude", Status: store.TemplateStatusActive, Created: now, Updated: now}))
	cfgB := uuid.NewString()
	require.NoError(t, f.s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: cfgB, Name: "wide-cfg-b", Slug: "wide-cfg-b-" + cfgB[:8], Scope: store.HarnessConfigScopeProject, ScopeID: f.projB, Harness: "claude", Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))

	admin, err := f.s.GetUser(ctx, f.admin)
	require.NoError(t, err)
	member, err := f.s.GetUser(ctx, f.member)
	require.NoError(t, err)
	adminUser := NewAuthenticatedUser(admin.ID, admin.Email, admin.DisplayName, admin.Role, "api")
	memberUser := NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, member.Role, "api")
	token := func(user *AuthenticatedUser, boundary TokenBoundary, selectors ...string) Identity {
		return NewScopedUserIdentityWithBoundaryAndDecoration(user, boundary, selectors, "wide-"+uuid.NewString()[:8], bearerCeiling(t, selectors...), nil)
	}

	for _, tc := range []struct {
		resourceType, permission, listSel, readSel, path, itemsKey, rowB string
	}{
		{"template", "template.list", "template:list", "template:read", "/api/v1/templates", "templates", tmplB},
		{"harness_config", "harness_config.list", "harness_config:list", "harness_config:read", "/api/v1/harness-configs", "harnessConfigs", cfgB},
	} {
		t.Run(tc.resourceType, func(t *testing.T) {
			wide := func(id Identity) bool {
				return f.srv.hasCatalogWideListAccess(ctx, id, tc.resourceType, tc.permission)
			}
			assert.True(t, wide(adminUser), "a hub-wide session holder takes the unfiltered list")
			assert.False(t, wide(memberUser), "a member's catalog grant is not hub-wide list authority")
			assert.True(t, wide(token(adminUser, hubBoundary(), tc.listSel)), "the list selector on a hub boundary with hub-wide authority")
			assert.False(t, wide(token(adminUser, hubBoundary(), tc.readSel)), "the read selector is not the list permission")
			assert.False(t, wide(token(adminUser, projectBoundary(f.projA), tc.listSel)), "a project boundary does not contain a hub-level request")
			assert.False(t, wide(token(memberUser, hubBoundary(), tc.listSel)), "the selector does not add hub-wide authority")

			listKey := f.mint(t, f.admin, hubBoundary(), tc.listSel)
			assert.Contains(t, decodeListPage(t, f.call(t, listKey, http.MethodGet, tc.path, nil), tc.itemsKey).IDs, tc.rowB)
			readKey := f.mint(t, f.admin, hubBoundary(), tc.readSel)
			assert.Contains(t, decodeListPage(t, f.call(t, readKey, http.MethodGet, tc.path, nil), tc.itemsKey).IDs, tc.rowB, "per-row read admits the row")
			projKey := f.mint(t, f.admin, projectBoundary(f.projA), tc.listSel, tc.readSel)
			assert.NotContains(t, decodeListPage(t, f.call(t, projKey, http.MethodGet, tc.path, nil), tc.itemsKey).IDs, tc.rowB, "a project token reads only its project")
			memberKey := f.mint(t, f.member, hubBoundary(), tc.listSel)
			memberPage := decodeListPage(t, f.call(t, memberKey, http.MethodGet, tc.path, nil), tc.itemsKey)
			assert.Empty(t, memberPage.IDs, "without hub-wide authority the list selector alone reads no row")
			assert.Equal(t, 0, memberPage.TotalCount)
		})
	}
}

// TestListFilters_CannotEnlargeTokenResults pins that caller-supplied list
// filters (projectId, a project slug, scopeId, ancestorId) only narrow a
// token's results: a project token asking for another project's rows gets
// none, while a hub token of the same holder gets them through the same
// filter.
func TestListFilters_CannotEnlargeTokenResults(t *testing.T) {
	f := newListSkillFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	tmplB := uuid.NewString()
	require.NoError(t, f.s.CreateTemplate(ctx, &store.Template{ID: tmplB, Name: "filter-tmpl-b", Slug: "filter-tmpl-b-" + tmplB[:8], Scope: store.TemplateScopeProject, ScopeID: f.projB, Harness: "claude", Status: store.TemplateStatusActive, Created: now, Updated: now}))
	child := &store.Agent{ID: uuid.NewString(), Slug: "filter-child-b", Name: "Filter Child B", ProjectID: f.projB, Phase: "running", Ancestry: []string{f.agentB.ID}}
	require.NoError(t, f.s.CreateAgent(ctx, child))
	projB, err := f.s.GetProject(ctx, f.projB)
	require.NoError(t, err)

	scopes := []string{"agent:list", "project:list", "skill:list", "skill:read", "template:read"}
	keyA := f.mint(t, f.admin, projectBoundary(f.projA), scopes...)
	hub := f.mint(t, f.admin, hubBoundary(), scopes...)

	for _, tc := range []struct {
		name, path, itemsKey, rowB string
	}{
		{"agents by projectId", "/api/v1/agents?projectId=" + f.projB, "agents", f.agentB.ID},
		{"agents by project slug", "/api/v1/agents?projectId=" + url.QueryEscape(projB.Slug), "agents", f.agentB.ID},
		{"agents by ancestorId", "/api/v1/agents?ancestorId=" + f.agentB.ID, "agents", child.ID},
		{"projects by slug", "/api/v1/projects?slug=" + url.QueryEscape(projB.Slug), "projects", f.projB},
		{"skills by scopeId", "/api/v1/skills?scope=project&scopeId=" + f.projB, "skills", f.b},
		{"templates by projectId", "/api/v1/templates?projectId=" + f.projB, "templates", tmplB},
		{"templates by scopeId", "/api/v1/templates?scope=project&scopeId=" + f.projB, "templates", tmplB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeListPage(t, f.call(t, keyA, http.MethodGet, tc.path, nil), tc.itemsKey)
			assert.Empty(t, got.IDs, "a project token gets no row of another project")
			assert.Equal(t, 0, got.TotalCount)
			assert.Contains(t, decodeListPage(t, f.call(t, hub, http.MethodGet, tc.path, nil), tc.itemsKey).IDs, tc.rowB, "fixture: the filter matches the row for a hub token")
		})
	}
}
