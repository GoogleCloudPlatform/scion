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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// User access tokens on writes to the caller's own user scope: user
// templates, user-scope skills, harness configs and imports, and the
// caller's injected skills. Each write needs the selector of its
// permission on a hub boundary; a project-boundary token is refused,
// because user-scope records belong to no project.

// usTemplate creates an active user-scope template owned by userID.
func usTemplate(t *testing.T, s store.Store, userID, name string) *store.Template {
	t.Helper()
	tmpl := &store.Template{ID: tid("us-tmpl-" + name), Name: name, Slug: name, Harness: "claude",
		Scope: store.TemplateScopeUser, ScopeID: userID, OwnerID: userID, CreatedBy: userID,
		Status: store.TemplateStatusActive, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// usHarnessConfig creates an active user-scope harness config owned by
// userID.
func usHarnessConfig(t *testing.T, s store.Store, userID, name string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{ID: tid("us-hc-" + name), Name: name, Slug: name, Harness: "claude",
		Scope: store.HarnessConfigScopeUser, ScopeID: userID, OwnerID: userID, CreatedBy: userID,
		Status: store.HarnessConfigStatusActive, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// usTemplateExists reports whether the template row is still stored.
func usTemplateExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetTemplate(context.Background(), id)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		return false
	}
	return true
}

// usHarnessConfigExists reports whether the harness config row is still
// stored.
func usHarnessConfigExists(t *testing.T, s store.Store, id string) bool {
	t.Helper()
	_, err := s.GetHarnessConfig(context.Background(), id)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		return false
	}
	return true
}

// usUserTemplateNames returns the names of userID's user-scope templates.
func usUserTemplateNames(t *testing.T, s store.Store, userID string) []string {
	t.Helper()
	result, err := s.ListTemplates(context.Background(), store.TemplateFilter{Scope: store.TemplateScopeUser, ScopeID: userID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	names := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		names = append(names, item.Name)
	}
	return names
}

// usRefused asserts a refusal: 403, or 404 where a read gate hides the
// record.
func usRefused(t *testing.T, code int, msgAndArgs ...interface{}) {
	t.Helper()
	assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, code, msgAndArgs...)
}

// TestUserSkillInjectionWrite_RequiresSelectorAndHubBoundary pins that
// adding to the caller's injected skills needs user_skill_injection:update
// on a hub boundary: a hub token without it and a project-boundary token
// are refused and change nothing, the selector cannot be minted on a
// project boundary, and a hub token with it adds the entry.
func TestUserSkillInjectionWrite_RequiresSelectorAndHubBoundary(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	body := api.SkillInjectionEntry{SkillURI: "skill://scion/us-added@1.0"}

	without := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:read")
	rec := callWithKey(t, srv, without, http.MethodPost, userInjectedSkillsPath, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	projectToken := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "project:read")
	rec = callWithKey(t, srv, projectToken, http.MethodPost, userInjectedSkillsPath, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Empty(t, userInjectedSkillURIs(t, s, alice.ID), "a refused write adds nothing")

	_, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(alice.ID), CreateTokenParams{
		UserID: alice.ID, Name: "us-" + tid("tok"), Boundary: projectBoundary(project.ID), Scopes: []string{"user_skill_injection:update"},
	})
	require.Error(t, err, "user_skill_injection:update is hub-only")

	with := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "user_skill_injection:update")
	rec = callWithKey(t, srv, with, http.MethodPost, userInjectedSkillsPath, body)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{body.SkillURI}, userInjectedSkillURIs(t, s, alice.ID))
}

// TestUserInjectedSkillsList_AnyTokenReadsOwnEntries pins the list's
// admit_self disposition: a hub token and a project-boundary token, each
// with an unrelated selector, read the holder's own entries and no other
// user's.
func TestUserInjectedSkillsList_AnyTokenReadsOwnEntries(t *testing.T) {
	srv, s, project, alice, bob := setupInjectedSkillsTest(t)
	seedUserInjectedSkill(t, s, alice.ID, "skill://scion/alice@1.0")
	seedUserInjectedSkill(t, s, bob.ID, "skill://scion/bob@1.0")

	for _, key := range []string{
		mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:read"),
		mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "project:read"),
	} {
		rec := callWithKey(t, srv, key, http.MethodGet, userInjectedSkillsPath, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var list api.SkillInjectionList
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Len(t, list.Entries, 1)
		assert.Equal(t, "skill://scion/alice@1.0", list.Entries[0].SkillURI)
	}
}

// TestUserTemplateWrite_RequiresTemplateSelector pins that every route on
// the caller's user templates needs the template selector of its action on
// a hub boundary: list, get and download need template:read; create needs
// template:create; update, upload and finalize need template:update; and
// delete, through /api/v1/users/me/templates/{id} or
// /api/v1/templates/{id}, needs template:delete. A hub token with an
// unrelated selector and a project-boundary token carrying every template
// selector are refused and change nothing; a token with the right selector
// passes.
func TestUserTemplateWrite_RequiresTemplateSelector(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	tmpl := usTemplate(t, s, alice.ID, "us-own")
	viaGeneric := usTemplate(t, s, alice.ID, "us-own-generic")
	base := "/api/v1/users/me/templates"

	type call struct {
		method, path string
		body         interface{}
	}
	calls := []call{
		{http.MethodGet, base, nil},
		{http.MethodGet, base + "/" + tmpl.ID, nil},
		{http.MethodGet, base + "/" + tmpl.ID + "/download", nil},
		{http.MethodPost, base, CreateTemplateRequest{Name: "us-created"}},
		{http.MethodPut, base + "/" + tmpl.ID, map[string]string{"name": "us-renamed"}},
		{http.MethodPost, base + "/" + tmpl.ID + "/upload", UploadRequest{Files: []FileUploadRequest{{Path: "scion-agent.yaml", Size: 10}}}},
		{http.MethodPost, base + "/" + tmpl.ID + "/finalize", FinalizeRequest{}},
		{http.MethodDelete, base + "/" + tmpl.ID, nil},
		{http.MethodDelete, "/api/v1/templates/" + viaGeneric.ID, nil},
	}
	before := usUserTemplateNames(t, s, alice.ID)

	unrelated := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "project:read")
	projectToken := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID),
		"template:read", "template:create", "template:update", "template:delete")
	for _, key := range []string{unrelated, projectToken} {
		for _, c := range calls {
			rec := callWithKey(t, srv, key, c.method, c.path, c.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
		}
	}
	assert.Equal(t, before, usUserTemplateNames(t, s, alice.ID), "a refused call changes nothing")

	read := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:read")
	rec := callWithKey(t, srv, read, http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	assert.Len(t, list.Templates, 2)
	rec = callWithKey(t, srv, read, http.MethodGet, base+"/"+tmpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// The test server configures no storage, so an admitted download stops
	// there, after authorization.
	rec = callWithKey(t, srv, read, http.MethodGet, base+"/"+tmpl.ID+"/download", nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	for _, c := range calls[3:] {
		rec := callWithKey(t, srv, read, c.method, c.path, c.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "template:read alone on %s %s: %s", c.method, c.path, rec.Body.String())
	}

	create := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:create")
	rec = callWithKey(t, srv, create, calls[3].method, calls[3].path, calls[3].body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, alice.ID, created.Template.OwnerID)
	assert.Equal(t, alice.ID, created.Template.ScopeID)

	update := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:update")
	rec = callWithKey(t, srv, update, calls[4].method, calls[4].path, calls[4].body)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, c := range calls[5:7] {
		rec := callWithKey(t, srv, update, c.method, c.path, c.body)
		assert.Equal(t, http.StatusBadGateway, rec.Code, "template:update on %s %s stops at the missing storage: %s", c.method, c.path, rec.Body.String())
	}

	del := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:delete")
	for _, c := range calls[7:] {
		rec := callWithKey(t, srv, del, c.method, c.path, c.body)
		assert.Equal(t, http.StatusNoContent, rec.Code, "%s %s: %s", c.method, c.path, rec.Body.String())
	}
	assert.False(t, usTemplateExists(t, s, tmpl.ID))
	assert.False(t, usTemplateExists(t, s, viaGeneric.ID))
}

// TestUserScopeSkillCreate_RequiresSkillCreate pins that creating a skill
// in the caller's user scope needs skill:create on a hub boundary, that a
// refused create stores nothing, and that the created skill's scope and
// owner are the caller.
func TestUserScopeSkillCreate_RequiresSkillCreate(t *testing.T) {
	srv, s, project, alice, bob := setupInjectedSkillsTest(t)
	body := CreateSkillRequest{Name: "us-token-skill", Scope: store.SkillScopeUser, ScopeID: bob.ID}

	for _, key := range []string{
		mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "skill:read"),
		mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "skill:create"),
	} {
		rec := callWithKey(t, srv, key, http.MethodPost, "/api/v1/skills", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	}
	skills, err := s.ListSkills(context.Background(), store.SkillFilter{Scope: store.SkillScopeUser}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, skills.Items, "a refused create stores nothing")

	with := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "skill:create")
	rec := callWithKey(t, srv, with, http.MethodPost, "/api/v1/skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateSkillResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Skill)
	assert.Equal(t, alice.ID, resp.Skill.ScopeID)
	assert.Equal(t, alice.ID, resp.Skill.OwnerID)
}

// TestCloneToUserScope_RequiresCreateOnDestination pins that cloning a
// readable template or harness config into the caller's user scope needs
// the create selector for the destination (template:create,
// harness_config:create) on a hub boundary, besides read on the source: a
// token with only the read selector is refused and creates nothing.
func TestCloneToUserScope_RequiresCreateOnDestination(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	srcTmpl := usTemplate(t, s, alice.ID, "us-clone-src")
	srcHC := usHarnessConfig(t, s, alice.ID, "us-clone-src")
	clone := map[string]string{"name": "us-clone-dest", "scope": "user"}

	cases := []struct {
		path, read, create string
		exists             func() bool
	}{
		{"/api/v1/templates/" + srcTmpl.ID + "/clone", "template:read", "template:create", func() bool {
			_, err := s.GetTemplateBySlug(context.Background(), "us-clone-dest", store.TemplateScopeUser, alice.ID)
			return err == nil
		}},
		{"/api/v1/harness-configs/" + srcHC.ID + "/clone", "harness_config:read", "harness_config:create", func() bool {
			_, err := s.GetHarnessConfigBySlug(context.Background(), "us-clone-dest", store.HarnessConfigScopeUser, alice.ID)
			return err == nil
		}},
	}
	for _, c := range cases {
		readOnly := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), c.read)
		rec := callWithKey(t, srv, readOnly, http.MethodPost, c.path, clone)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s with %s only: %s", c.path, c.read, rec.Body.String())
		assert.False(t, c.exists(), "a refused clone creates nothing")

		both := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), c.read, c.create)
		rec = callWithKey(t, srv, both, http.MethodPost, c.path, clone)
		assert.Equal(t, http.StatusCreated, rec.Code, "%s with %s and %s: %s", c.path, c.read, c.create, rec.Body.String())
		assert.True(t, c.exists())
	}
}

// TestHarnessUserDelete_RequiresDeleteAction pins that deleting one of the
// caller's own user-scope harness configs needs harness_config:delete on a
// hub boundary, besides the route gate's harness_config:read: a token with
// only the read selector is refused on its own record, a token without the
// read selector does not reach the record, a project-boundary token is
// refused, and a session or a hub token with both selectors deletes it.
func TestHarnessUserDelete_RequiresDeleteAction(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	hc := usHarnessConfig(t, s, alice.ID, "us-del")
	path := "/api/v1/harness-configs/" + hc.ID

	readOnly := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "harness_config:read")
	rec := callWithKey(t, srv, readOnly, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "the read selector alone on the owner's record: %s", rec.Body.String())

	deleteOnly := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "harness_config:delete")
	rec = callWithKey(t, srv, deleteOnly, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "the route gate needs the read selector: %s", rec.Body.String())

	projectToken := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "harness_config:read", "harness_config:delete")
	rec = callWithKey(t, srv, projectToken, http.MethodDelete, path, nil)
	usRefused(t, rec.Code, "a project-boundary token: %s", rec.Body.String())
	assert.True(t, usHarnessConfigExists(t, s, hc.ID), "a refused delete keeps the record")

	both := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "harness_config:read", "harness_config:delete")
	rec = callWithKey(t, srv, both, http.MethodDelete, path, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.False(t, usHarnessConfigExists(t, s, hc.ID))

	sessionHC := usHarnessConfig(t, s, alice.ID, "us-del-session")
	rec = doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/harness-configs/"+sessionHC.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestHarnessUserReimport_RequiresUpdateAction pins that reimporting one of
// the caller's own user-scope harness configs needs harness_config:update
// on a hub boundary, besides the route gate's harness_config:read: a token
// with only the read selector is refused on its own record, and a token
// with both passes authorization (the test server then stops at its
// missing storage, before any fetch).
func TestHarnessUserReimport_RequiresUpdateAction(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	hc := usHarnessConfig(t, s, alice.ID, "us-reimport")
	path := "/api/v1/harness-configs/" + hc.ID + "/reimport"
	body := map[string]string{"sourceUrl": "https://github.com/example/us-reimport"}

	readOnly := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "harness_config:read")
	rec := callWithKey(t, srv, readOnly, http.MethodPost, path, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "the read selector alone on the owner's record: %s", rec.Body.String())

	projectToken := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "harness_config:read", "harness_config:update")
	rec = callWithKey(t, srv, projectToken, http.MethodPost, path, body)
	usRefused(t, rec.Code, "a project-boundary token: %s", rec.Body.String())

	both := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "harness_config:read", "harness_config:update")
	rec = callWithKey(t, srv, both, http.MethodPost, path, body)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

// TestHarnessUserScopeCreate_OwnerIsCaller pins that a user-scope harness
// config created through /api/v1/harness-configs has the caller as its
// scope, owner and creator for every caller, whatever scopeId the request
// names, and that a token needs harness_config:create.
func TestHarnessUserScopeCreate_OwnerIsCaller(t *testing.T) {
	srv, s := testServer(t)
	adminID := tid("us-hc-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(context.Background(), s, adminID)
	admin, err := s.GetUser(context.Background(), adminID)
	require.NoError(t, err)
	other := tid("us-hc-other")

	create := func(name string) CreateHarnessConfigRequest {
		return CreateHarnessConfigRequest{Name: name, Harness: "claude", Scope: store.HarnessConfigScopeUser, ScopeID: other}
	}
	check := func(rec *httptest.ResponseRecorder, name string) {
		t.Helper()
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var resp CreateHarnessConfigResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.HarnessConfig)
		stored, err := s.GetHarnessConfig(context.Background(), resp.HarnessConfig.ID)
		require.NoError(t, err)
		assert.Equal(t, adminID, stored.ScopeID, name)
		assert.Equal(t, adminID, stored.OwnerID, name)
		assert.Equal(t, adminID, stored.CreatedBy, name)
	}

	check(doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/harness-configs", create("us-hc-session")), "session")

	readOnly := mintHubConfigToken(t, srv, adminID, hubBoundary(), "harness_config:read")
	rec := callWithKey(t, srv, readOnly, http.MethodPost, "/api/v1/harness-configs", create("us-hc-refused"))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	with := mintHubConfigToken(t, srv, adminID, hubBoundary(), "harness_config:create")
	check(callWithKey(t, srv, with, http.MethodPost, "/api/v1/harness-configs", create("us-hc-token")), "token")
}

// TestUserScopeImport_RequiresCreateSelector pins that importing or
// discovering templates or harness configs for the caller's user scope
// needs the create selector of the kind (template:create or
// harness_config:create) on a hub boundary. Refused requests stop before
// any fetch; the admitting decision is checked directly, so the test makes
// no network request.
func TestUserScopeImport_RequiresCreateSelector(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)
	srv.SetStorage(newMockStorage("us-storage"))
	kinds := []struct{ kind, authzType, selector string }{
		{"template", "template", "template:create"},
		{"harness-config", "harness_config", "harness_config:create"},
	}

	unrelated := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:read", "harness_config:read")
	projectToken := mintHubConfigToken(t, srv, alice.ID, projectBoundary(project.ID), "template:create", "harness_config:create")
	for i, k := range kinds {
		otherKind := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), kinds[1-i].selector)
		body := map[string]string{"kind": k.kind, "scope": "user", "sourceUrl": "https://example.invalid/us-import"}
		for _, path := range []string{"/api/v1/resources/import", "/api/v1/resources/discover"} {
			for _, key := range []string{unrelated, projectToken, otherKind} {
				rec := callWithKey(t, srv, key, http.MethodPost, path, body)
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s kind %s: %s", path, k.kind, rec.Body.String())
			}
		}

		ctx := context.Background()
		admitted := selfToken(t, alice.ID, hubBoundary(), k.selector)
		assert.True(t, srv.authzService.CheckAccess(ctx, admitted, userScopeImportResource(k.authzType, admitted), ActionCreate).Allowed,
			"%s admits a hub token for kind %s", k.selector, k.kind)
		without := selfToken(t, alice.ID, hubBoundary(), kinds[1-i].selector)
		assert.False(t, srv.authzService.CheckAccess(ctx, without, userScopeImportResource(k.authzType, without), ActionCreate).Allowed,
			"%s does not admit kind %s", kinds[1-i].selector, k.kind)
	}
}

// TestUserScopeVariants_GlobalBranchRefusesTokens pins that the global
// branches of the routes whose user-scope branch admits tokens refuse a
// hub token of a super-admin holding the create selector: a global skill
// create and a global resource import or discover.
func TestUserScopeVariants_GlobalBranchRefusesTokens(t *testing.T) {
	srv, s := testServer(t)
	srv.SetStorage(newMockStorage("us-storage"))
	adminID := tid("us-global-admin")
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(context.Background(), s, adminID)

	skillToken := mintHubConfigToken(t, srv, adminID, hubBoundary(), "skill:create")
	rec := callWithKey(t, srv, skillToken, http.MethodPost, "/api/v1/skills", CreateSkillRequest{Name: "us-global-skill", Scope: store.SkillScopeGlobal})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	for _, k := range []struct{ kind, selector string }{{"template", "template:create"}, {"harness-config", "harness_config:create"}} {
		key := mintHubConfigToken(t, srv, adminID, hubBoundary(), k.selector)
		body := map[string]string{"kind": k.kind, "scope": "global", "sourceUrl": "https://example.invalid/us-global"}
		for _, path := range []string{"/api/v1/resources/import", "/api/v1/resources/discover"} {
			rec := callWithKey(t, srv, key, http.MethodPost, path, body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s kind %s: %s", path, k.kind, rec.Body.String())
		}
	}
}
