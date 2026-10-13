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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// User access tokens on the global catalog, skill registries, skill
// discovery and resolution, and the global and project branches of the
// template and harness config routes. A hub-level target is reached only
// on a hub boundary; a project target needs a boundary that contains the
// project.

// gcFixture is a server with storage, a super-admin who owns two projects
// (so it can mint tokens bound to either), and a project member of the
// first project.
type gcFixture struct {
	srv      *Server
	store    store.Store
	admin    *store.User
	member   *store.User
	projectA string
	projectB string
}

func newGCFixture(t *testing.T, withStorage bool) *gcFixture {
	t.Helper()
	srv, s := testServer(t)
	if withStorage {
		srv.SetStorage(newMockStorage("gc-storage"))
	}
	ctx := context.Background()
	adminID := tid("gc-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, adminID)

	f := &gcFixture{srv: srv, store: s, projectA: tid("gc-project-a"), projectB: tid("gc-project-b")}
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projectA, Name: "GC A", Slug: "gc-a"}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projectB, Name: "GC B", Slug: "gc-b"}))
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", f.projectA, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", f.projectB, store.ProjectRoleOwner)

	memberID := tid("gc-member")
	createTestUserWithProjectRole(t, s, memberID, memberID+"@test.com", f.projectA, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, memberID)

	var err error
	f.admin, err = s.GetUser(ctx, adminID)
	require.NoError(t, err)
	f.member, err = s.GetUser(ctx, memberID)
	require.NoError(t, err)
	return f
}

// hubToken mints a hub-boundary token of the super-admin.
func (f *gcFixture) hubToken(t *testing.T, selectors ...string) string {
	t.Helper()
	return mintHubConfigToken(t, f.srv, f.admin.ID, hubBoundary(), selectors...)
}

// projectToken mints a token of the super-admin bound to projectID.
func (f *gcFixture) projectToken(t *testing.T, projectID string, selectors ...string) string {
	t.Helper()
	return mintHubConfigToken(t, f.srv, f.admin.ID, projectBoundary(projectID), selectors...)
}

// cannotMint asserts the super-admin cannot mint selectors on boundary.
func (f *gcFixture) cannotMint(t *testing.T, boundary TokenBoundary, selectors ...string) {
	t.Helper()
	_, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.admin.ID), CreateTokenParams{
		UserID: f.admin.ID, Name: "gc-" + tid("refused"), Boundary: boundary, Scopes: selectors,
	})
	assert.Error(t, err, "%v on a %s boundary must not mint", selectors, boundary.Kind)
}

func (f *gcFixture) call(t *testing.T, key, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return callWithKey(t, f.srv, key, method, path, body)
}

// gcAdmitted asserts a status past the authorization check: not 401, 403
// or 404.
func gcAdmitted(t *testing.T, rec *httptest.ResponseRecorder, msgAndArgs ...interface{}) {
	t.Helper()
	assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, rec.Code,
		append(msgAndArgs, rec.Body.String())...)
}

func gcGlobalTemplate(t *testing.T, s store.Store, name, sourceURL string) *store.Template {
	t.Helper()
	tmpl := &store.Template{ID: tid("gc-tmpl-" + name), Name: name, Slug: name, Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive, SourceURL: sourceURL, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

func gcProjectTemplate(t *testing.T, s store.Store, projectID, name, sourceURL string) *store.Template {
	t.Helper()
	tmpl := &store.Template{ID: tid("gc-ptmpl-" + name), Name: name, Slug: name, Harness: "claude",
		Scope: store.TemplateScopeProject, ScopeID: projectID, ProjectID: projectID, Status: store.TemplateStatusActive,
		SourceURL: sourceURL, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

func gcHarnessConfig(t *testing.T, s store.Store, scope, scopeID, name string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{ID: tid("gc-hc-" + name), Name: name, Slug: name, Harness: "claude",
		Scope: scope, ScopeID: scopeID, Status: store.HarnessConfigStatusActive, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// TestGlobalSkillCreate_RequiresCreateGlobalSelectorAndHubBoundary pins
// that a global or core skill create needs skill:create_global on a hub
// boundary: a hub token with skill:create is refused, the selector cannot
// be minted on a project boundary or by a user who holds no
// skill.create_global, and a hub token with it creates the skill for a
// super-admin and for a global catalog author.
func TestGlobalSkillCreate_RequiresCreateGlobalSelectorAndHubBoundary(t *testing.T) {
	f := newGCFixture(t, false)
	ctx := context.Background()

	for _, scope := range []string{store.SkillScopeGlobal, store.SkillScopeCore} {
		body := CreateSkillRequest{Name: "gc-" + scope + "-refused", Scope: scope}
		rec := f.call(t, f.hubToken(t, "skill:create", "skill:read"), http.MethodPost, "/api/v1/skills", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "scope %s with skill:create: %s", scope, rec.Body.String())
		rec = f.call(t, f.projectToken(t, f.projectA, "skill:create"), http.MethodPost, "/api/v1/skills", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "scope %s with a project token: %s", scope, rec.Body.String())
	}
	skills, err := f.store.ListSkills(ctx, store.SkillFilter{Scope: store.SkillScopeGlobal}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, skills.Items, "a refused create stores nothing")

	f.cannotMint(t, projectBoundary(f.projectA), "skill:create_global")
	_, _, err = f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.member.ID), CreateTokenParams{
		UserID: f.member.ID, Name: "gc-" + tid("member"), Boundary: hubBoundary(), Scopes: []string{"skill:create_global"},
	})
	assert.Error(t, err, "a project member holds no skill.create_global")

	with := f.hubToken(t, "skill:create_global")
	for _, scope := range []string{store.SkillScopeGlobal, store.SkillScopeCore} {
		rec := f.call(t, with, http.MethodPost, "/api/v1/skills", CreateSkillRequest{Name: "gc-" + scope + "-created", Scope: scope})
		assert.Equal(t, http.StatusCreated, rec.Code, "scope %s: %s", scope, rec.Body.String())
	}

	authorID := tid("gc-author")
	createTestUserWithRole(t, f.store, authorID, authorID+"@test.com", "member", store.SystemRoleGlobalCatalogAuthor)
	ensureHubMembership(ctx, f.store, authorID)
	authorKey := mintHubConfigToken(t, f.srv, authorID, hubBoundary(), "skill:create_global")
	rec := callWithKey(t, f.srv, authorKey, http.MethodPost, "/api/v1/skills", CreateSkillRequest{Name: "gc-author-skill"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestSkillRegistryToken_RequiresRegisterSelector pins that every skill
// registry route needs skill:register on a hub boundary: the route guard
// checks the hub-scoped registry target, a hub token without the selector
// is refused on every route, the selector cannot be minted on a project
// boundary, and a hub token with it reaches every route.
func TestSkillRegistryToken_RequiresRegisterSelector(t *testing.T) {
	f := newGCFixture(t, false)
	ctx := context.Background()
	reg := &store.SkillRegistry{Name: "gc-registry", Endpoint: "https://example.invalid/gc", Type: store.SkillRegistryTypeHub,
		TrustLevel: store.SkillRegistryTrustPinned, Status: store.SkillRegistryStatusActive}
	require.NoError(t, f.store.CreateSkillRegistry(ctx, reg))

	target, evidence, ok := routeGuardTarget(routeMetadataTable["/api/v1/skill-registries/"])
	require.True(t, ok)
	assert.Equal(t, hubScopedResource("skill", "hub"), target)
	assert.False(t, evidence.IsCollectionLevel)

	byID := "/api/v1/skill-registries/" + reg.ID
	type call struct {
		method, path string
		body         interface{}
		want         int
	}
	calls := []call{
		{http.MethodGet, "/api/v1/skill-registries", nil, http.StatusOK},
		{http.MethodPost, "/api/v1/skill-registries", CreateSkillRegistryRequest{Name: "gc-registry-new", Endpoint: "https://example.invalid/new"}, http.StatusCreated},
		{http.MethodGet, byID, nil, http.StatusOK},
		{http.MethodPut, byID, map[string]string{"description": "put"}, http.StatusOK},
		{http.MethodPatch, byID, map[string]string{"description": "patch"}, http.StatusOK},
		{http.MethodPost, byID + "/pin", PinSkillHashRequest{URI: "skill://gc-registry/x", Hash: "sha256:00"}, http.StatusOK},
		{http.MethodGet, byID + "/pins", nil, http.StatusOK},
		{http.MethodPost, byID + "/unpin", map[string]string{"uri": "skill://gc-registry/x"}, http.StatusOK},
		{http.MethodDelete, byID, nil, http.StatusOK},
	}

	without := f.hubToken(t, "skill:read", "skill:create_global")
	for _, c := range calls {
		rec := f.call(t, without, c.method, c.path, c.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s without skill:register: %s", c.method, c.path, rec.Body.String())
	}
	f.cannotMint(t, projectBoundary(f.projectA), "skill:register")

	with := f.hubToken(t, "skill:register")
	for _, c := range calls {
		rec := f.call(t, with, c.method, c.path, c.body)
		assert.Equal(t, c.want, rec.Code, "%s %s with skill:register: %s", c.method, c.path, rec.Body.String())
	}

	rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, "/api/v1/skill-registries", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "a member session holds no skill.register: %s", rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.admin, http.MethodGet, "/api/v1/skill-registries", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestGlobalResourceImport_ResolvesHubThroughEvidence pins that a global
// import or discovery of either kind is a hub collection request for the
// kind's create permission: a hub token with the create selector passes
// the check (the source, not a GitHub URL, stops it after), a hub token
// without it and a project-boundary token with it are refused, and the
// same token on the bare resource without the collection evidence is
// refused.
func TestGlobalResourceImport_ResolvesHubThroughEvidence(t *testing.T) {
	f := newGCFixture(t, true)
	ctx := context.Background()
	kinds := []struct{ kind, authzType, selector, otherSelector string }{
		{"template", "template", "template:create", "harness_config:create"},
		{"harness-config", "harness_config", "harness_config:create", "template:create"},
	}
	for _, k := range kinds {
		body := map[string]string{"kind": k.kind, "scope": "global", "sourceUrl": "https://example.invalid/gc-global"}
		refused := []string{
			f.hubToken(t, k.otherSelector),
			f.projectToken(t, f.projectA, k.selector),
		}
		admitted := f.hubToken(t, k.selector)
		for _, path := range []string{"/api/v1/resources/import", "/api/v1/resources/discover"} {
			for _, key := range refused {
				rec := f.call(t, key, http.MethodPost, path, body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s kind %s: %s", path, k.kind, rec.Body.String())
			}
			rec := f.call(t, admitted, http.MethodPost, path, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s kind %s with %s stops after the check: %s", path, k.kind, k.selector, rec.Body.String())
		}

		token := selfToken(t, f.admin.ID, hubBoundary(), k.selector)
		assert.True(t, f.srv.globalImportDecision(ctx, token, k.authzType).Allowed, "kind %s with hub evidence", k.kind)
		assert.False(t, f.srv.authzService.CheckAccess(ctx, token, Resource{Type: k.authzType}, ActionCreate).Allowed,
			"kind %s without evidence resolves no target scope", k.kind)
	}
}

// TestProjectResourceImport_TokenNeedsCreateOnTheProject pins the project
// branch of import and discovery: the kind's create selector on a boundary
// that contains the named project passes the check, a token bound to
// another project or without the selector is refused.
func TestProjectResourceImport_TokenNeedsCreateOnTheProject(t *testing.T) {
	f := newGCFixture(t, true)
	for _, k := range []struct{ kind, selector, otherSelector string }{
		{"template", "template:create", "harness_config:create"},
		{"harness-config", "harness_config:create", "template:create"},
	} {
		body := map[string]string{"kind": k.kind, "scope": "project", "scopeId": f.projectA, "sourceUrl": "https://example.invalid/gc-project"}
		for _, path := range []string{"/api/v1/resources/import", "/api/v1/resources/discover"} {
			for _, key := range []string{f.projectToken(t, f.projectB, k.selector), f.hubToken(t, k.otherSelector)} {
				rec := f.call(t, key, http.MethodPost, path, body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s kind %s: %s", path, k.kind, rec.Body.String())
			}
			for _, key := range []string{f.projectToken(t, f.projectA, k.selector), f.hubToken(t, k.selector)} {
				rec := f.call(t, key, http.MethodPost, path, body)
				assert.Equal(t, http.StatusBadRequest, rec.Code, "%s kind %s stops after the check: %s", path, k.kind, rec.Body.String())
			}
		}
	}
}

// discoverBody is a discovery request whose source is not a GitHub URL, so
// an admitted request stops with 400 after the authorization check.
func discoverBody(projectID string) DiscoverSkillsRequest {
	return DiscoverSkillsRequest{SourceURL: "https://example.invalid/gc-skills", ProjectID: projectID}
}

// TestDiscoverDirectory_RequiresSkillCreateOnProject pins that a user
// access token discovering skills for a named project needs skill:create on
// a boundary that contains the project: a token bound to the project, or a
// hub token, with skill:create passes the check; a token bound to another
// project with skill:create, and a hub token with agent:create and
// skill:read only, are refused.
func TestDiscoverDirectory_RequiresSkillCreateOnProject(t *testing.T) {
	f := newGCFixture(t, false)
	path := "/api/v1/skills/discover-directory"
	for _, key := range []string{f.projectToken(t, f.projectB, "skill:create"), f.hubToken(t, "agent:create", "skill:read")} {
		rec := f.call(t, key, http.MethodPost, path, discoverBody(f.projectA))
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
	for _, key := range []string{f.projectToken(t, f.projectA, "skill:create"), f.hubToken(t, "skill:create")} {
		rec := f.call(t, key, http.MethodPost, path, discoverBody(f.projectA))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "stops at the source after the check: %s", rec.Body.String())
	}
	rec := f.call(t, f.hubToken(t, "skill:create"), http.MethodPost, path, discoverBody(tid("gc-missing-project")))
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, "an unknown project is never discovered: %s", rec.Body.String())
}

// TestDiscoverDirectory_GlobalRequiresCreateGlobalSelector pins that a user
// access token discovering skills with no project named needs
// skill:create_global on a hub boundary: a hub token without it and a
// project-boundary token are refused, and a hub token with it passes the
// check.
func TestDiscoverDirectory_GlobalRequiresCreateGlobalSelector(t *testing.T) {
	f := newGCFixture(t, false)
	path := "/api/v1/skills/discover-directory"
	for _, key := range []string{f.hubToken(t, "skill:create", "skill:read"), f.projectToken(t, f.projectA, "skill:create", "skill:read")} {
		rec := f.call(t, key, http.MethodPost, path, discoverBody(""))
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
	f.cannotMint(t, projectBoundary(f.projectA), "skill:create_global")
	rec := f.call(t, f.hubToken(t, "skill:create_global"), http.MethodPost, path, discoverBody(""))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "stops at the source after the check: %s", rec.Body.String())
}

// TestSkillDiscoverySession_ProjectIdChecksAgentCreate pins that a session
// discovering skills for a named project needs agent.create on it: a
// member of the project passes the check, a user who is not a member is
// refused.
func TestSkillDiscoverySession_ProjectIdChecksAgentCreate(t *testing.T) {
	f := newGCFixture(t, false)
	path := "/api/v1/skills/discover-directory"
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, path, discoverBody(f.projectA))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, f.srv, f.member, http.MethodPost, path, discoverBody(f.projectB))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestSkillDiscoverySession_NoProjectIdAnyUser pins that a session
// discovering skills with no project named passes the check.
func TestSkillDiscoverySession_NoProjectIdAnyUser(t *testing.T) {
	f := newGCFixture(t, false)
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/skills/discover-directory", discoverBody(""))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// resolveCodes posts a batch resolve and returns the resolved URIs and the
// error code of each refused reference.
func resolveCodes(t *testing.T, rec *httptest.ResponseRecorder) (resolved []string, codes map[string]string) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ResolveSkillsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	codes = map[string]string{}
	for _, r := range resp.Resolved {
		resolved = append(resolved, r.URI)
	}
	for _, e := range resp.Errors {
		codes[e.URI] = e.Code
	}
	return resolved, codes
}

// gcInvalidGitHubRef is a gh:// reference that fails to parse, so an
// admitted resolve answers resolve_failed without any GitHub request.
const gcInvalidGitHubRef = "gh://gc-invalid"

func ghResolveBody(projectID string) ResolveSkillsRequest {
	return ResolveSkillsRequest{ProjectID: projectID, Skills: []ResolveSkillRef{{URI: gcInvalidGitHubRef}}}
}

// TestPublicGitHubSkillResolve_TokenNeedsSkillReadOnHub pins that a user
// access token resolving gh:// references with no project named needs
// skill:read on a hub boundary: without it, or on a project boundary, the
// reference answers forbidden; with it the reference is resolved (and
// fails only on its parse).
func TestPublicGitHubSkillResolve_TokenNeedsSkillReadOnHub(t *testing.T) {
	f := newGCFixture(t, false)
	for _, key := range []string{f.hubToken(t, "template:read"), f.projectToken(t, f.projectA, "skill:read")} {
		_, codes := resolveCodes(t, f.call(t, key, http.MethodPost, "/api/v1/skills/resolve", ghResolveBody("")))
		assert.Equal(t, agent.SkillErrCodeForbidden, codes[gcInvalidGitHubRef])
	}
	_, codes := resolveCodes(t, f.call(t, f.hubToken(t, "skill:read"), http.MethodPost, "/api/v1/skills/resolve", ghResolveBody("")))
	require.Contains(t, codes, gcInvalidGitHubRef)
	assert.NotEqual(t, agent.SkillErrCodeForbidden, codes[gcInvalidGitHubRef])
}

// TestPublicGitHubSkillResolve_SessionAnyUser pins that a session resolving
// gh:// references with no project named passes the check.
func TestPublicGitHubSkillResolve_SessionAnyUser(t *testing.T) {
	f := newGCFixture(t, false)
	_, codes := resolveCodes(t, doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/skills/resolve", ghResolveBody("")))
	require.Contains(t, codes, gcInvalidGitHubRef)
	assert.NotEqual(t, agent.SkillErrCodeForbidden, codes[gcInvalidGitHubRef])
}

// TestProjectGitHubSkillResolve_TokenNeedsSkillReadOnProject pins that
// resolving gh:// references for a named project needs skill read on the
// project: a token bound to the project, or a hub token, with skill:read
// resolves; a token bound to another project, or without the selector,
// answers forbidden.
func TestProjectGitHubSkillResolve_TokenNeedsSkillReadOnProject(t *testing.T) {
	f := newGCFixture(t, false)
	for _, key := range []string{f.projectToken(t, f.projectB, "skill:read"), f.hubToken(t, "template:read")} {
		_, codes := resolveCodes(t, f.call(t, key, http.MethodPost, "/api/v1/skills/resolve", ghResolveBody(f.projectA)))
		assert.Equal(t, agent.SkillErrCodeForbidden, codes[gcInvalidGitHubRef])
	}
	for _, key := range []string{f.projectToken(t, f.projectA, "skill:read"), f.hubToken(t, "skill:read")} {
		_, codes := resolveCodes(t, f.call(t, key, http.MethodPost, "/api/v1/skills/resolve", ghResolveBody(f.projectA)))
		require.Contains(t, codes, gcInvalidGitHubRef)
		assert.NotEqual(t, agent.SkillErrCodeForbidden, codes[gcInvalidGitHubRef])
	}
}

// TestLocalSkillResolve_TokenNeedsSkillRead pins that resolving a skill://
// reference checks skill read on each candidate: a hub token with
// skill:read resolves a global skill, while a hub token without it and a
// project-boundary token (the global skill is outside the boundary) get the
// reference answered as not resolved.
func TestLocalSkillResolve_TokenNeedsSkillRead(t *testing.T) {
	f := newGCFixture(t, true)
	ctx := context.Background()
	skillID := tid("gc-resolve-skill")
	require.NoError(t, f.store.CreateSkill(ctx, &store.Skill{ID: skillID, Name: "gc-resolve-skill", Slug: "gc-resolve-skill",
		Scope: store.SkillScopeGlobal, Status: "active", StoragePath: "gc/skills/gc-resolve-skill", Created: time.Now(), Updated: time.Now()}))
	require.NoError(t, f.store.CreateSkillVersion(ctx, &store.SkillVersion{ID: tid("gc-resolve-v1"), SkillID: skillID, Version: "1.0.0",
		Status: store.SkillVersionStatusPublished, Files: []store.TemplateFile{{Path: "SKILL.md", Size: 3, Hash: "sha256:00"}}, Created: time.Now()}))
	_, err := f.srv.GetStorage().Upload(ctx, "gc/skills/gc-resolve-skill/1.0.0/SKILL.md", bytes.NewReader([]byte("gc\n")), storage.UploadOptions{})
	require.NoError(t, err)

	const ref = "skill://gc-resolve-skill"
	body := ResolveSkillsRequest{Skills: []ResolveSkillRef{{URI: ref}}}
	for _, key := range []string{f.hubToken(t, "template:read"), f.projectToken(t, f.projectA, "skill:read")} {
		resolved, codes := resolveCodes(t, f.call(t, key, http.MethodPost, "/api/v1/skills/resolve", body))
		assert.Empty(t, resolved)
		assert.Contains(t, codes, ref)
	}
	resolved, codes := resolveCodes(t, f.call(t, f.hubToken(t, "skill:read"), http.MethodPost, "/api/v1/skills/resolve", body))
	assert.Len(t, resolved, 1, "errors: %v", codes)
}

// TestCatalogCloneToken_RequiresReadAndCreate pins that cloning a global
// template or harness config into the global scope or a project needs read
// on the source and create on the destination: a token with only the read
// selector is refused (403), one with only the create selector does not
// reach the source (404), a token bound to a project does not reach the
// global source, and a hub token with both creates the clone.
func TestCatalogCloneToken_RequiresReadAndCreate(t *testing.T) {
	f := newGCFixture(t, false)
	srcTmpl := gcGlobalTemplate(t, f.store, "gc-clone-src", "")
	srcHC := gcHarnessConfig(t, f.store, store.HarnessConfigScopeGlobal, "", "gc-clone-src")
	cases := []struct{ path, read, create string }{
		{"/api/v1/templates/" + srcTmpl.ID + "/clone", "template:read", "template:create"},
		{"/api/v1/harness-configs/" + srcHC.ID + "/clone", "harness_config:read", "harness_config:create"},
	}
	for _, c := range cases {
		for i, dest := range []CloneTemplateRequest{
			{Name: "gc-clone-global", Scope: "global"},
			{Name: "gc-clone-project", Scope: "project", ScopeID: f.projectA},
		} {
			rec := f.call(t, f.hubToken(t, c.read), http.MethodPost, c.path, dest)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s dest %s with %s only: %s", c.path, dest.Scope, c.read, rec.Body.String())
			rec = f.call(t, f.hubToken(t, c.create), http.MethodPost, c.path, dest)
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s dest %s with %s only: %s", c.path, dest.Scope, c.create, rec.Body.String())
			rec = f.call(t, f.projectToken(t, f.projectA, c.read, c.create), http.MethodPost, c.path, dest)
			usRefused(t, rec.Code, "%s dest %s with a project token: %s", c.path, dest.Scope, rec.Body.String())

			rec = f.call(t, f.hubToken(t, c.read, c.create), http.MethodPost, c.path, dest)
			assert.Equal(t, http.StatusCreated, rec.Code, "%s dest %s (%d): %s", c.path, dest.Scope, i, rec.Body.String())
		}
	}
}

// gcPastReimportCheck are the statuses of a template reimport past its
// authorization check on a server with no storage: the source check (400)
// or the missing storage (503).
var gcPastReimportCheck = []int{http.StatusBadRequest, http.StatusServiceUnavailable}

// TestCatalogReimportToken_RequiresReadAndCreate pins that reimporting a
// global or project template or harness config needs read on the record
// and create on its owning scope: a token with only the read selector is
// refused (403), one with only the create selector does not reach the
// record (404), a token bound to another project is refused, and a token
// with both passes the check (the server has no storage, so the reimport
// then stops at the source check or the missing storage).
func TestCatalogReimportToken_RequiresReadAndCreate(t *testing.T) {
	f := newGCFixture(t, false)
	const source = "https://github.com/example/gc-reimport"
	globalTmpl := gcGlobalTemplate(t, f.store, "gc-reimport-global", source)
	projectTmpl := gcProjectTemplate(t, f.store, f.projectA, "gc-reimport-project", source)
	globalHC := gcHarnessConfig(t, f.store, store.HarnessConfigScopeGlobal, "", "gc-reimport-global")
	projectHC := gcHarnessConfig(t, f.store, store.HarnessConfigScopeProject, f.projectA, "gc-reimport-project")
	body := map[string]string{"sourceUrl": source}

	cases := []struct {
		path, read, create string
		project            bool
		admitted           []int
	}{
		{"/api/v1/templates/" + globalTmpl.ID + "/reimport", "template:read", "template:create", false, gcPastReimportCheck},
		{"/api/v1/templates/" + projectTmpl.ID + "/reimport", "template:read", "template:create", true, gcPastReimportCheck},
		{"/api/v1/harness-configs/" + globalHC.ID + "/reimport", "harness_config:read", "harness_config:create", false, []int{http.StatusServiceUnavailable}},
		{"/api/v1/harness-configs/" + projectHC.ID + "/reimport", "harness_config:read", "harness_config:create", true, []int{http.StatusServiceUnavailable}},
	}
	for _, c := range cases {
		rec := f.call(t, f.hubToken(t, c.read), http.MethodPost, c.path, body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s with %s only: %s", c.path, c.read, rec.Body.String())
		rec = f.call(t, f.hubToken(t, c.create), http.MethodPost, c.path, body)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s with %s only: %s", c.path, c.create, rec.Body.String())
		rec = f.call(t, f.projectToken(t, f.projectB, c.read, c.create), http.MethodPost, c.path, body)
		usRefused(t, rec.Code, "%s with a token bound to another project: %s", c.path, rec.Body.String())

		rec = f.call(t, f.hubToken(t, c.read, c.create), http.MethodPost, c.path, body)
		assert.Contains(t, c.admitted, rec.Code, "%s with both selectors: %s", c.path, rec.Body.String())
		if c.project {
			rec = f.call(t, f.projectToken(t, f.projectA, c.read, c.create), http.MethodPost, c.path, body)
			assert.Contains(t, c.admitted, rec.Code, "%s with a token bound to its project: %s", c.path, rec.Body.String())
		}
	}
}

// TestHarnessDeleteToken_RequiresReadAndDelete pins that deleting a global
// or project harness config needs harness_config:read for the route gate
// and harness_config:delete on its owning scope: a token with only the read
// selector is refused (403), one with only the delete selector does not
// reach the record (404), a token bound to another project is refused, and
// a token with both deletes it.
func TestHarnessDeleteToken_RequiresReadAndDelete(t *testing.T) {
	f := newGCFixture(t, false)
	for _, project := range []bool{false, true} {
		scope, scopeID, name := store.HarnessConfigScopeGlobal, "", "gc-del-global"
		if project {
			scope, scopeID, name = store.HarnessConfigScopeProject, f.projectA, "gc-del-project"
		}
		hc := gcHarnessConfig(t, f.store, scope, scopeID, name)
		path := "/api/v1/harness-configs/" + hc.ID

		rec := f.call(t, f.hubToken(t, "harness_config:read"), http.MethodDelete, path, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s with read only: %s", scope, rec.Body.String())
		rec = f.call(t, f.hubToken(t, "harness_config:delete"), http.MethodDelete, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s with delete only: %s", scope, rec.Body.String())
		rec = f.call(t, f.projectToken(t, f.projectB, "harness_config:read", "harness_config:delete"), http.MethodDelete, path, nil)
		usRefused(t, rec.Code, "%s with a token bound to another project: %s", scope, rec.Body.String())
		assert.True(t, usHarnessConfigExists(t, f.store, hc.ID), "a refused delete keeps the record")

		key := f.hubToken(t, "harness_config:read", "harness_config:delete")
		if project {
			key = f.projectToken(t, f.projectA, "harness_config:read", "harness_config:delete")
		}
		rec = f.call(t, key, http.MethodDelete, path, nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "%s with both: %s", scope, rec.Body.String())
		assert.False(t, usHarnessConfigExists(t, f.store, hc.ID))
	}
}

// TestHarnessImageStatusToken_RequiresRead pins that a harness config's
// image status needs harness_config:read: a token without it, and a token
// bound to a project for a global config, get 404; a hub token with it
// passes the gate (a config with no image then answers 400).
func TestHarnessImageStatusToken_RequiresRead(t *testing.T) {
	f := newGCFixture(t, false)
	hc := gcHarnessConfig(t, f.store, store.HarnessConfigScopeGlobal, "", "gc-image-status")
	path := "/api/v1/harness-configs/" + hc.ID + "/image-status"
	for _, key := range []string{f.hubToken(t, "template:read"), f.projectToken(t, f.projectA, "harness_config:read")} {
		rec := f.call(t, key, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	}
	rec := f.call(t, f.hubToken(t, "harness_config:read"), http.MethodGet, path, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	gcAdmitted(t, rec)
}
