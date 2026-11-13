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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Phase 2 Slice 2: template.create_global, template.update_global,
// template.delete_global authorization (ptone/scion#1713)
// ============================================================================

func bindTemplateCatalogAuthor(t *testing.T, s store.Store, userID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)
	rb := &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}
	created, err := s.CreateRoleBinding(ctx, rb)
	require.NoError(t, err)
	return created
}

func bindTemplateHubAdmin(t *testing.T, s store.Store, userID string) *store.RoleBinding {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	rb := &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}
	created, err := s.CreateRoleBinding(ctx, rb)
	require.NoError(t, err)
	return created
}

type globalTemplateStubSourceFetcher struct {
	body []byte
}

func (f *globalTemplateStubSourceFetcher) FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error) {
	return f.FetchBytesAuthorized(ctx, rawURL, "")
}

func (f *globalTemplateStubSourceFetcher) FetchBytesAuthorized(_ context.Context, _, _ string) (*remotefetch.Result, error) {
	return &remotefetch.Result{Body: f.body}, nil
}

func buildGlobalTemplateArchive(t *testing.T, slug, readme string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := map[string]string{
		"repo-main/templates/" + slug + "/scion-agent.yaml": "schema_version: \"1\"\nharness: claude\n",
		"repo-main/templates/" + slug + "/README.md":        readme,
	}
	for path, content := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0o644,
			Size: int64(len(content)),
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestTemplateGlobalTwins_PermissionRegistered pins template.create_global,
// template.update_global, and template.delete_global in the permissions
// registry, applicability tables, collection target classes, and selector
// registry.
func TestTemplateGlobalTwins_PermissionRegistered(t *testing.T) {
	require.Equal(t, string(ActionCreateGlobal), permissions.ActionCreateGlobal)
	require.Equal(t, string(ActionUpdateGlobal), permissions.ActionUpdateGlobal)
	require.Equal(t, string(ActionDeleteGlobal), permissions.ActionDeleteGlobal)

	manageScopes := permissions.UATManageScopesFor(permissions.ResourceTemplate)
	for _, tc := range []struct {
		id         string
		action     string
		scope      string
		supported  []permissions.TargetClassKind
		collection []permissions.TargetClassKind
	}{
		{
			"template.create_global",
			permissions.ActionCreateGlobal,
			"template:create_global",
			[]permissions.TargetClassKind{permissions.TargetClassKindHubResource},
			[]permissions.TargetClassKind{permissions.TargetClassKindHubResource},
		},
		{
			"template.update_global",
			permissions.ActionUpdateGlobal,
			"template:update_global",
			[]permissions.TargetClassKind{permissions.TargetClassKindGlobalCatalog},
			[]permissions.TargetClassKind{},
		},
		{
			"template.delete_global",
			permissions.ActionDeleteGlobal,
			"template:delete_global",
			[]permissions.TargetClassKind{permissions.TargetClassKindGlobalCatalog},
			[]permissions.TargetClassKind{},
		},
	} {
		var found *permissions.Permission
		for i := range permissions.Registry {
			if permissions.Registry[i].ID == tc.id {
				found = &permissions.Registry[i]
				break
			}
		}
		require.NotNil(t, found, "%s must exist in permissions.Registry", tc.id)
		assert.Equal(t, permissions.ResourceTemplate, found.Resource)
		assert.Equal(t, tc.action, found.Action)
		assert.Equal(t, permissions.CapabilityNone, found.CapabilityKind, "%s must have CapabilityNone", tc.id)
		assert.Equal(t, tc.scope, found.UATScope)
		assert.True(t, found.ExcludeFromManageAlias, "%s must stay out of template:manage", tc.id)
		assert.NotContains(t, manageScopes, tc.scope)

		applies, reviewed := permissions.AppliesToExistingProjectTarget(tc.id)
		assert.True(t, reviewed)
		assert.False(t, applies, "%s must not apply to project targets", tc.id)

		boundaries, ok := permissions.SelectorAllowedBoundaries(tc.id)
		assert.True(t, ok)
		assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, boundaries)

		assert.Equal(t, tc.supported, permissions.SupportedTargetClassesFor(tc.id))
		collClasses, collReviewed := permissions.CollectionTargetClassesFor(tc.id)
		assert.True(t, collReviewed)
		assert.Equal(t, tc.collection, collClasses)

		sel, selOK := permissions.ResolveSelector(tc.scope)
		require.True(t, selOK, "ResolveSelector(%q) must succeed", tc.scope)
		assert.Equal(t, []string{tc.id}, sel.PermissionIDs)
		assert.Equal(t, []permissions.BoundaryKind{permissions.BoundaryKindHub}, sel.AllowedBoundaries)
		require.NoError(t, validatePermissionIDs([]string{tc.id}))
	}

	pickerScopes := make([]string, 0)
	for _, opt := range permissions.UATScopeOptions(false) {
		pickerScopes = append(pickerScopes, opt.UATScope)
	}
	assert.Contains(t, pickerScopes, "template:create_global")
	assert.Contains(t, pickerScopes, "template:update_global")
	assert.Contains(t, pickerScopes, "template:delete_global")
}

// TestSeedReconcile_GlobalCatalogAuthorR3AppliesOnUpgrade verifies that
// reconciling from global-catalog-author revision 2 (skill twins only) to
// revision 3 adds the three template.*_global twins, and reconciling hub-admin
// from revision 11 to revision 12 adds the three template.*_global twins while
// keeping plain template.* out.
func TestSeedReconcile_GlobalCatalogAuthorR3AppliesOnUpgrade(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()

	r2Author := BuiltInRole{
		Name:        store.SystemRoleGlobalCatalogAuthor,
		Description: "Creates and maintains global (hub-scoped) skills",
		ScopeType:   store.RoleScopeSystem,
		Revision:    2,
		Permissions: []string{"skill.create_global", "skill.delete_global", "skill.update_global"},
	}
	reconcileBuiltInRole(ctx, s, r2Author)
	require.Equal(t, 2, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleGlobalCatalogAuthor).Revision)

	r11HubAdminPerms := make([]string, 0)
	for _, p := range hubAdminPermissionIDs() {
		switch p {
		case "template.create_global", "template.update_global", "template.delete_global":
			continue
		default:
			r11HubAdminPerms = append(r11HubAdminPerms, p)
		}
	}
	r11HubAdmin := BuiltInRole{
		Name:        store.SystemRoleHubAdmin,
		Description: "Hub operations administrator",
		ScopeType:   store.RoleScopeSystem,
		Revision:    11,
		Permissions: r11HubAdminPerms,
	}
	reconcileBuiltInRole(ctx, s, r11HubAdmin)
	require.Equal(t, 11, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleHubAdmin).Revision)

	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleGlobalCatalogAuthor || role.Name == store.SystemRoleHubAdmin {
			reconcileBuiltInRole(ctx, s, role)
		}
	}

	afterAuthor, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleGlobalCatalogAuthor, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"skill.create_global", "skill.update_global", "skill.delete_global",
		"template.create_global", "template.update_global", "template.delete_global",
	}, afterAuthor.Permissions)
	assert.Equal(t, 3, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleGlobalCatalogAuthor).Revision)

	afterAdmin, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	assert.Contains(t, afterAdmin.Permissions, "template.create_global")
	assert.Contains(t, afterAdmin.Permissions, "template.update_global")
	assert.Contains(t, afterAdmin.Permissions, "template.delete_global")
	assert.NotContains(t, afterAdmin.Permissions, "template.create")
	assert.NotContains(t, afterAdmin.Permissions, "template.update")
	assert.NotContains(t, afterAdmin.Permissions, "template.delete")
	assert.Equal(t, 12, getAppliedBuiltInRoleMarker(ctx, s, store.SystemRoleHubAdmin).Revision)
}

// TestTemplateAuthz_GlobalCRUD_CatalogAuthorAllowed verifies that
// global-catalog-author can perform every global template write (create, PUT,
// PATCH, upload, finalize, file write/delete, clone into global, reimport, and
// delete — including by a second global-catalog-author who is not the creator),
// while an ordinary hub member is denied (403) across every global write route.
func TestTemplateAuthz_GlobalCRUD_CatalogAuthorAllowed(t *testing.T) {
	srv, s := newCommitTestServer(t, newMockStorage("tmpl-gca-crud"))
	ctx := context.Background()

	alice := createNamedTestUser(t, s, "tmpl-gca-alice", store.UserRoleMember)
	ensureHubMembership(ctx, s, alice.ID)
	bindTemplateCatalogAuthor(t, s, alice.ID)

	bob := createNamedTestUser(t, s, "tmpl-gca-bob", store.UserRoleMember)
	ensureHubMembership(ctx, s, bob.ID)
	bindTemplateCatalogAuthor(t, s, bob.ID)

	carol := createNamedTestUser(t, s, "tmpl-member-carol", store.UserRoleMember)
	ensureHubMembership(ctx, s, carol.ID)

	// 1. Create global template as Alice.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name:    "gca-global-template",
		Harness: "claude",
		Scope:   store.TemplateScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	require.NotNil(t, createResp.Template)
	created := createResp.Template
	byID := "/api/v1/templates/" + created.ID

	// 2. Bob (second global-catalog-author, NOT the creator) performs PUT, PATCH,
	// upload, file write/delete, finalize, clone into global, and reimport.
	rec = doRequestAsUser(t, srv, bob, http.MethodPut, byID, store.Template{
		Name:        "gca-global-template",
		Description: "put by bob",
		Status:      store.TemplateStatusActive,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, bob, http.MethodPatch, byID, map[string]string{
		"description": "patched by bob",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, bob, http.MethodPost, byID+"/upload", UploadRequest{
		Files: []FileUploadRequest{{Path: "scion-agent.yaml", Size: 32}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, bob, http.MethodPut, byID+"/files/scion-agent.yaml", TemplateFileWriteRequest{
		Content: "default_harness_config: claude\n",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, bob, http.MethodPut, byID+"/files/README.md", TemplateFileWriteRequest{
		Content: "# Hello\n",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, bob, http.MethodDelete, byID+"/files/README.md", nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	stored, err := s.GetTemplate(ctx, created.ID)
	require.NoError(t, err)
	stageFiles := map[string]string{"scion-agent.yaml": "default_harness_config: claude\n", "NOTES.md": "staged\n"}
	stageObjects(t, srv, stored, stageFiles)
	rec = doRequestAsUser(t, srv, bob, http.MethodPost, byID+"/finalize", FinalizeRequest{
		Manifest: &TemplateManifest{Version: "1.0", Files: commitManifest(stageFiles)},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Clone into global scope as Bob.
	rec = doRequestAsUser(t, srv, bob, http.MethodPost, byID+"/clone", CloneTemplateRequest{
		Name:  "gca-global-cloned",
		Scope: store.TemplateScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var cloned store.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cloned))

	// Reimport global template as Bob.
	srv.templateSourceFetcher = &globalTemplateStubSourceFetcher{
		body: buildGlobalTemplateArchive(t, "gca-global-template", "reimported v2"),
	}
	rec = doRequestAsUser(t, srv, bob, http.MethodPost, byID+"/reimport", map[string]string{
		"sourceUrl": "https://github.com/acme/repo/tree/main/templates",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 3. Carol (ordinary hub-member without global-catalog-author) is denied 403
	// on every global template write surface.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"create", http.MethodPost, "/api/v1/templates", CreateTemplateRequest{Name: "carol-denied", Harness: "claude", Scope: store.TemplateScopeGlobal}},
		{"put", http.MethodPut, byID, store.Template{Name: "carol-denied", Status: store.TemplateStatusActive}},
		{"patch", http.MethodPatch, byID, map[string]string{"description": "nope"}},
		{"upload", http.MethodPost, byID + "/upload", UploadRequest{Files: []FileUploadRequest{{Path: "README.md", Size: 5}}}},
		{"finalize", http.MethodPost, byID + "/finalize", FinalizeRequest{Manifest: &TemplateManifest{Files: []store.TemplateFile{{Path: "README.md"}}}}},
		{"files-post", http.MethodPost, byID + "/files", map[string]string{"content": "nope"}},
		{"files-put", http.MethodPut, byID + "/files/NOTES.md", TemplateFileWriteRequest{Content: "nope"}},
		{"files-delete", http.MethodDelete, byID + "/files/NOTES.md", nil},
		{"clone-global", http.MethodPost, byID + "/clone", CloneTemplateRequest{Name: "carol-clone-denied", Scope: store.TemplateScopeGlobal}},
		{"reimport", http.MethodPost, byID + "/reimport", nil},
		{"delete", http.MethodDelete, byID, nil},
	} {
		t.Run("member-denied/"+tc.name, func(t *testing.T) {
			r := doRequestAsUser(t, srv, carol, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, r.Code, r.Body.String())
		})
	}

	// 4. Bob deletes Alice's global template and Alice deletes Bob's clone.
	rec = doRequestAsUser(t, srv, bob, http.MethodDelete, byID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/templates/"+cloned.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestTemplateAuthz_CatalogAuthorDeniedOnProjectAndUserTemplates verifies that
// global-catalog-author's system-scoped *_global twins never authorize project
// template writes (for a non-member or for a project-viewer/member lacking
// plain project write permissions) or writes on another user's user-scope
// template, and SystemAuthorityProof denies all three twins on project targets.
func TestTemplateAuthz_CatalogAuthorDeniedOnProjectAndUserTemplates(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	srv.SetStorage(newMockStorage("tmpl-gca-iso"))
	ctx := context.Background()

	// Seed a global template (for clone source), a project template owned by
	// Alice, and a user template owned by Alice.
	globalSrc := createAuthzTestTemplate(t, s, "iso-global-src", store.TemplateScopeGlobal, "", "")
	projTmpl := createAuthzTestTemplate(t, s, "iso-proj-tmpl", store.TemplateScopeProject, project.ID, alice.ID)
	projTmpl.SourceURL = "https://github.com/acme/repo/tree/main/templates/iso-proj-tmpl"
	require.NoError(t, s.UpdateTemplate(ctx, projTmpl))

	aliceUserTmpl := createAuthzTestTemplate(t, s, "iso-user-tmpl", store.TemplateScopeUser, alice.ID, alice.ID)
	aliceUserTmpl.SourceURL = "https://github.com/acme/repo/tree/main/templates/iso-user-tmpl"
	require.NoError(t, s.UpdateTemplate(ctx, aliceUserTmpl))

	// Bob is a non-member of project with global-catalog-author.
	ensureHubMembership(ctx, s, bob.ID)
	bindTemplateCatalogAuthor(t, s, bob.ID)

	// Charlie holds a read-only project role (project.read + template.read,
	// lacking template.create/update/delete) plus global-catalog-author at
	// system scope, so Charlie passes ActionRead on projTmpl and exercises the
	// write gate directly.
	charlie := createNamedTestUser(t, s, "tmpl-gca-iso-viewer", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	projTemplateReaderRole, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID:          tid("proj-template-reader"),
		Name:        "proj-template-reader",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read", "template.list", "template.read"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: projTemplateReaderRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	bindTemplateCatalogAuthor(t, s, charlie.ID)

	// Dana is a project-member in project (has template.create, lacks
	// template.update/delete) plus global-catalog-author at system scope.
	dana := createNamedTestUser(t, s, "tmpl-gca-iso-member", store.UserRoleMember)
	ensureHubMembership(ctx, s, dana.ID)
	createTestUserWithProjectRole(t, s, dana.ID, dana.Email, project.ID, store.ProjectRoleMember)
	bindTemplateCatalogAuthor(t, s, dana.ID)

	// SystemAuthorityProof must reject all 3 twins on a project target.
	for _, permID := range []string{"template.create_global", "template.update_global", "template.delete_global"} {
		ok, err := srv.authzService.SystemAuthorityProof(ctx, activeUserPrincipal(bob.ID), project.ID, permID, ContemplatedProjectClass(permID))
		require.NoError(t, err)
		assert.False(t, ok, "%s must not grant SystemAuthorityProof on a project", permID)
	}

	// Non-member (Bob) and project-viewer (Charlie) cannot create/clone/reimport in project.
	for _, u := range []*store.User{bob, charlie} {
		rec := doRequestAsUser(t, srv, u, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
			Name: "denied-proj-create", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s create project template: %s", u.ID, rec.Body.String())

		rec = doRequestAsUser(t, srv, u, http.MethodPost, "/api/v1/templates/"+globalSrc.ID+"/clone", CloneTemplateRequest{
			Name: "denied-proj-clone", Scope: store.TemplateScopeProject, ScopeID: project.ID,
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, "user %s clone into project: %s", u.ID, rec.Body.String())
	}
	// Charlie can read projTmpl, so reimport reaches the template.create check and returns 403.
	rec := doRequestAsUser(t, srv, charlie, http.MethodPost, "/api/v1/templates/"+projTmpl.ID+"/reimport", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Non-member (Bob), project-viewer (Charlie), and project-member (Dana) are
	// all denied update and delete on projTmpl and on Alice's user-scope template.
	for _, u := range []*store.User{bob, charlie, dana} {
		for _, target := range []*store.Template{projTmpl, aliceUserTmpl} {
			byID := "/api/v1/templates/" + target.ID
			for _, tc := range []struct {
				name   string
				method string
				path   string
				body   interface{}
			}{
				{"put", http.MethodPut, byID, store.Template{Name: "nope", Status: store.TemplateStatusActive}},
				{"patch", http.MethodPatch, byID, map[string]string{"description": "nope"}},
				{"upload", http.MethodPost, byID + "/upload", UploadRequest{Files: []FileUploadRequest{{Path: "README.md", Size: 5}}}},
				{"finalize", http.MethodPost, byID + "/finalize", FinalizeRequest{Manifest: &TemplateManifest{Files: []store.TemplateFile{{Path: "README.md"}}}}},
				{"files-put", http.MethodPut, byID + "/files/README.md", TemplateFileWriteRequest{Content: "nope"}},
				{"delete", http.MethodDelete, byID, nil},
			} {
				r := doRequestAsUser(t, srv, u, tc.method, tc.path, tc.body)
				assert.Equal(t, http.StatusForbidden, r.Code, "user %s %s on %s (%s): %s", u.ID, tc.name, target.ID, target.Scope, r.Body.String())
			}
		}
		// User-scope reimport checks ActionUpdate (denied for non-owners even with global-catalog-author -> 404 on read gate).
		r := doRequestAsUser(t, srv, u, http.MethodPost, "/api/v1/templates/"+aliceUserTmpl.ID+"/reimport", nil)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, r.Code)
	}
}

// TestGlobalTemplateCreate_StampsCreatedByNotOwner verifies design §3.2:
// creating a global template via POST /api/v1/templates, cloning into global
// scope, or importing via the resource pipeline stamps CreatedBy with the
// caller's user ID while keeping OwnerID empty, and reimport preserves both.
func TestGlobalTemplateCreate_StampsCreatedByNotOwner(t *testing.T) {
	srv, s := newCommitTestServer(t, newMockStorage("tmpl-gca-crud"))
	ctx := context.Background()

	author := createNamedTestUser(t, s, "tmpl-provenance-author", store.UserRoleMember)
	ensureHubMembership(ctx, s, author.ID)
	bindTemplateCatalogAuthor(t, s, author.ID)

	// 1. Direct POST /api/v1/templates (scope=global).
	rec := doRequestAsUser(t, srv, author, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name:    "prov-direct-global",
		Harness: "claude",
		Scope:   store.TemplateScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	storedDirect, err := s.GetTemplate(ctx, createResp.Template.ID)
	require.NoError(t, err)
	assert.Equal(t, author.ID, storedDirect.CreatedBy, "direct global create must stamp CreatedBy")
	assert.Empty(t, storedDirect.OwnerID, "direct global create must leave OwnerID empty")

	// 2. Clone into global scope.
	secondAuthor := createNamedTestUser(t, s, "tmpl-provenance-cloner", store.UserRoleMember)
	ensureHubMembership(ctx, s, secondAuthor.ID)
	bindTemplateCatalogAuthor(t, s, secondAuthor.ID)

	rec = doRequestAsUser(t, srv, secondAuthor, http.MethodPost, "/api/v1/templates/"+storedDirect.ID+"/clone", CloneTemplateRequest{
		Name:  "prov-cloned-global",
		Scope: store.TemplateScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var clonedResp store.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &clonedResp))
	storedClone, err := s.GetTemplate(ctx, clonedResp.ID)
	require.NoError(t, err)
	assert.Equal(t, secondAuthor.ID, storedClone.CreatedBy, "global clone must stamp CreatedBy with cloner")
	assert.Empty(t, storedClone.OwnerID, "global clone must leave OwnerID empty")

	// 3. Resource pipeline templatePersistence.Create with authenticated user in context.
	userIdent := NewAuthenticatedUser(author.ID, author.Email, author.DisplayName, author.Role, "api")
	importCtx := contextWithIdentity(ctx, userIdent)
	srv.templateSourceFetcher = &globalTemplateStubSourceFetcher{
		body: buildGlobalTemplateArchive(t, "prov-imported-global", "# Imported\n"),
	}
	importedDir := writeTemplateDir(t, t.TempDir(), "prov-imported-global", map[string]string{
		"scion-agent.yaml": "default_harness_config: claude\n",
		"README.md":        "# Imported\n",
	})
	_, err = srv.templateStore().Bootstrap(importCtx, "prov-imported-global", importedDir, store.TemplateScopeGlobal, "", "https://github.com/acme/repo/tree/main/templates", true)
	require.NoError(t, err)
	storedImported, err := s.GetTemplateBySlug(ctx, "prov-imported-global", store.TemplateScopeGlobal, "")
	require.NoError(t, err)
	assert.Equal(t, author.ID, storedImported.CreatedBy, "resource pipeline global create must stamp CreatedBy")
	assert.Empty(t, storedImported.OwnerID, "resource pipeline global create must leave OwnerID empty")

	// 4. Reimport preserves original CreatedBy and leaves OwnerID empty even when
	// invoked by a different global-catalog-author.
	rec = doRequestAsUser(t, srv, secondAuthor, http.MethodPost, "/api/v1/templates/"+storedImported.ID+"/reimport", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	afterReimport, err := s.GetTemplate(ctx, storedImported.ID)
	require.NoError(t, err)
	assert.Equal(t, author.ID, afterReimport.CreatedBy, "reimport must preserve original CreatedBy")
	assert.Empty(t, afterReimport.OwnerID, "reimport must keep OwnerID empty")
}

// TestGlobalTemplate_CreatorWithoutRoleDenied pins design §3.2 & §8: when a
// global-catalog-author creates a global template (or even if a legacy global
// template row has OwnerID set to the caller) and their role binding is
// removed, they lose all update, reimport, and delete authority via both
// session and pre-minted hub UAT.
func TestGlobalTemplate_CreatorWithoutRoleDenied(t *testing.T) {
	srv, s := newCommitTestServer(t, newMockStorage("tmpl-gca-crud"))
	ctx := context.Background()

	alice := createNamedTestUser(t, s, "tmpl-former-author", store.UserRoleMember)
	ensureHubMembership(ctx, s, alice.ID)
	rb := bindTemplateCatalogAuthor(t, s, alice.ID)

	// Mint a hub UAT while Alice still holds global-catalog-author.
	aliceToken := mintHubConfigToken(t, srv, alice.ID, hubBoundary(),
		"template:read", "template:create_global", "template:update_global", "template:delete_global")

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name:    "former-author-global-tmpl",
		Harness: "claude",
		Scope:   store.TemplateScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	created := createResp.Template

	// Also seed a legacy global template row that explicitly carries OwnerID = alice.ID.
	legacyOwned := createAuthzTestTemplate(t, s, "legacy-ownerid-global-tmpl", store.TemplateScopeGlobal, "", alice.ID)
	legacyOwned.CreatedBy = alice.ID
	legacyOwned.SourceURL = "https://github.com/acme/repo/tree/main/templates/legacy-ownerid-global-tmpl"
	require.NoError(t, s.UpdateTemplate(ctx, legacyOwned))

	// Remove Alice's global-catalog-author binding.
	require.NoError(t, s.DeleteRoleBinding(ctx, rb.ID))

	for _, targetID := range []string{created.ID, legacyOwned.ID} {
		byID := "/api/v1/templates/" + targetID
		for _, tc := range []struct {
			name   string
			method string
			path   string
			body   interface{}
		}{
			{"put", http.MethodPut, byID, store.Template{Name: "denied", Status: store.TemplateStatusActive}},
			{"patch", http.MethodPatch, byID, map[string]string{"description": "denied"}},
			{"upload", http.MethodPost, byID + "/upload", UploadRequest{Files: []FileUploadRequest{{Path: "README.md", Size: 5}}}},
			{"finalize", http.MethodPost, byID + "/finalize", FinalizeRequest{Manifest: &TemplateManifest{Files: []store.TemplateFile{{Path: "README.md"}}}}},
			{"files-put", http.MethodPut, byID + "/files/README.md", TemplateFileWriteRequest{Content: "denied"}},
			{"reimport", http.MethodPost, byID + "/reimport", nil},
			{"delete", http.MethodDelete, byID, nil},
		} {
			rSess := doRequestAsUser(t, srv, alice, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, rSess.Code, "session %s on %s: %s", tc.name, targetID, rSess.Body.String())

			rTok := callWithKey(t, srv, aliceToken, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, rTok.Code, "token %s on %s: %s", tc.name, targetID, rTok.Body.String())
		}
	}
}

// TestGlobalTemplateDelete_AuthorizesRecord pins the deleteTemplateV2 asymmetry
// fix (design §3.2): deleting a global template authorizes the stored template
// record (templateResource(existing)) with template.delete_global rather than a
// bare scope resource or plain template.delete.
func TestGlobalTemplateDelete_AuthorizesRecord(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	globalTmpl := createAuthzTestTemplate(t, s, "del-record-global", store.TemplateScopeGlobal, "", "")

	// User holding plain template.delete + create_global + update_global (missing delete_global) -> 403.
	noDeleteTwin := createNamedTestUser(t, s, "tmpl-no-del-twin", store.UserRoleMember)
	ensureHubMembership(ctx, s, noDeleteTwin.ID)
	rdNoDel := &store.RoleDefinition{
		ID:          tid("role-no-del-twin"),
		Name:        "role-no-del-twin",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"template.delete", "template.create_global", "template.update_global"},
	}
	_, err := s.CreateRoleDefinition(ctx, rdNoDel)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdNoDel.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      noDeleteTwin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, noDeleteTwin, http.MethodDelete, "/api/v1/templates/"+globalTmpl.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// On the global branch, deleteTemplateV2 authorizes templateResource(existing)
	// (carrying existing.ID for record-targeted policy and audit parity with
	// updateTemplateV2) with template.delete_global rather than an ID-less
	// templateScopeResource.
	onlyDeleteTwin := createNamedTestUser(t, s, "tmpl-only-del-twin", store.UserRoleMember)
	ensureHubMembership(ctx, s, onlyDeleteTwin.ID)
	rdOnlyDel := &store.RoleDefinition{
		ID:          tid("role-only-del-twin"),
		Name:        "role-only-del-twin",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"template.delete_global"},
	}
	_, err = s.CreateRoleDefinition(ctx, rdOnlyDel)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdOnlyDel.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      onlyDeleteTwin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	onlyDelIdent := NewAuthenticatedUser(onlyDeleteTwin.ID, onlyDeleteTwin.Email, onlyDeleteTwin.DisplayName, onlyDeleteTwin.Role, "api")
	recRes := templateResource(globalTmpl)
	assert.Equal(t, globalTmpl.ID, recRes.ID, "templateResource(existing) must carry record ID")
	recDec := srv.authzService.CheckAccess(ctx, onlyDelIdent, recRes, catalogWriteAction(permissions.ResourceTemplate, globalTmpl.Scope, ActionDelete))
	assert.True(t, recDec.Allowed, "templateResource(existing) must authorize template.delete_global: %s", recDec.Reason)

	rec = doRequestAsUser(t, srv, onlyDeleteTwin, http.MethodDelete, "/api/v1/templates/"+globalTmpl.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "DELETE handler must authorize template.delete_global: %s", rec.Body.String())

	// Hub UAT carrying only template:delete_global also deletes a global template.
	globalTmpl2 := createAuthzTestTemplate(t, s, "del-record-global-tok", store.TemplateScopeGlobal, "", "")
	onlyDelToken := mintHubConfigToken(t, srv, onlyDeleteTwin.ID, hubBoundary(), "template:delete_global")
	rec = callWithKey(t, srv, onlyDelToken, http.MethodDelete, "/api/v1/templates/"+globalTmpl2.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "hub UAT with template:delete_global must delete global template: %s", rec.Body.String())
}

// TestGlobalTemplate_PlainPermissionsDeniedOnGlobalAllowedOnProject verifies
// that a custom system-scoped role carrying plain template.create,
// template.update, and template.delete is denied (403) on global template
// writes while still authorizing project template writes.
func TestGlobalTemplate_PlainPermissionsDeniedOnGlobalAllowedOnProject(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	srv.SetStorage(newMockStorage("tmpl-plain-sys"))
	ctx := context.Background()

	globalTmpl := createAuthzTestTemplate(t, s, "plain-sys-global", store.TemplateScopeGlobal, "", "")
	globalTmpl.SourceURL = "https://github.com/acme/repo/tree/main/templates/plain-sys-global"
	require.NoError(t, s.UpdateTemplate(ctx, globalTmpl))

	projTmpl := createAuthzTestTemplate(t, s, "plain-sys-project", store.TemplateScopeProject, project.ID, alice.ID)

	charlie := createNamedTestUser(t, s, "tmpl-plain-sys-charlie", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	createTestUserWithProjectRole(t, s, charlie.ID, charlie.Email, project.ID, store.ProjectRoleViewer)

	customRole := &store.RoleDefinition{
		ID:          tid("custom-plain-tmpl-crud"),
		Name:        "custom-plain-tmpl-crud",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"template.create", "template.update", "template.delete"},
	}
	_, err := s.CreateRoleDefinition(ctx, customRole)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        alice.ID,
	})
	require.NoError(t, err)

	// Denied on global template writes.
	byGlobalID := "/api/v1/templates/" + globalTmpl.ID
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"create", http.MethodPost, "/api/v1/templates", CreateTemplateRequest{Name: "nope-global", Harness: "claude", Scope: store.TemplateScopeGlobal}},
		{"put", http.MethodPut, byGlobalID, store.Template{Name: "nope", Status: store.TemplateStatusActive}},
		{"patch", http.MethodPatch, byGlobalID, map[string]string{"description": "nope"}},
		{"upload", http.MethodPost, byGlobalID + "/upload", UploadRequest{Files: []FileUploadRequest{{Path: "README.md", Size: 5}}}},
		{"finalize", http.MethodPost, byGlobalID + "/finalize", FinalizeRequest{Manifest: &TemplateManifest{Files: []store.TemplateFile{{Path: "README.md"}}}}},
		{"files-put", http.MethodPut, byGlobalID + "/files/README.md", TemplateFileWriteRequest{Content: "nope"}},
		{"clone-global", http.MethodPost, byGlobalID + "/clone", CloneTemplateRequest{Name: "nope-clone", Scope: store.TemplateScopeGlobal}},
		{"reimport", http.MethodPost, byGlobalID + "/reimport", nil},
		{"delete", http.MethodDelete, byGlobalID, nil},
	} {
		t.Run("global-denied/"+tc.name, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, charlie, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}

	// Allowed on project template writes.
	rec := doRequestAsUser(t, srv, charlie, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "ok-proj-created", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, charlie, http.MethodPatch, "/api/v1/templates/"+projTmpl.ID, map[string]string{"description": "ok"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, charlie, http.MethodPost, byGlobalID+"/clone", CloneTemplateRequest{
		Name: "ok-proj-cloned", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, charlie, http.MethodDelete, "/api/v1/templates/"+projTmpl.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestGlobalTemplate_HubAdminGlobalOnlyNotNonMemberProject verifies design §3.4:
// hub-admin carries template.create_global, template.update_global, and
// template.delete_global (so global template CRUD succeeds), and carries NO
// plain template.* write permissions (so writes into a project where hub-admin
// is not a member are denied 403).
func TestGlobalTemplate_HubAdminGlobalOnlyNotNonMemberProject(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	srv.SetStorage(newMockStorage("tmpl-hub-admin"))
	ctx := context.Background()

	admin := createNamedTestUser(t, s, "tmpl-hub-admin-user", store.UserRoleMember)
	ensureHubMembership(ctx, s, admin.ID)
	bindTemplateHubAdmin(t, s, admin.ID)

	projTmpl := createAuthzTestTemplate(t, s, "hubadmin-nonmember-proj-tmpl", store.TemplateScopeProject, project.ID, alice.ID)

	// Allowed on global template CRUD.
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "hubadmin-global-tmpl", Harness: "claude", Scope: store.TemplateScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var createResp CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
	globalID := createResp.Template.ID

	rec = doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/templates/"+globalID, map[string]string{
		"description": "updated by hub-admin",
	})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Denied on project where hub-admin is not a member.
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
		Name: "hubadmin-proj-denied", Harness: "claude", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/templates/"+globalID+"/clone", CloneTemplateRequest{
		Name: "hubadmin-proj-clone-denied", Scope: store.TemplateScopeProject, ScopeID: project.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/templates/"+projTmpl.ID, map[string]string{"description": "denied"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/templates/"+projTmpl.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Allowed to delete global template.
	rec = doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/templates/"+globalID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestGlobalTemplateCapabilities_RecordAndList verifies design §3.5:
//   - record-level _capabilities.actions on a global template emits ["read",
//     "update", "delete"] for global-catalog-author and hub-admin, ["read"] for
//     an ordinary hub member (even if OwnerID is set), and never "*_global";
//   - list-level _capabilities.actions on GET /api/v1/templates?scope=global
//     emits ["create"] for global-catalog-author and hub-admin (and a hub UAT
//     with template:list + template:create_global), and [] for an ordinary
//     member/project-owner or a hub UAT with plain template:create;
//   - GET /api/v1/templates?scope=project&scopeId=... emits ["create"] for a
//     project-member and [] for a non-member global-catalog-author / hub-admin.
func TestGlobalTemplateCapabilities_RecordAndList(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	ctx := context.Background()

	// Seed a global template (even stamping bob.ID as legacy OwnerID) and a project template.
	globalTmpl := createAuthzTestTemplate(t, s, "caps-global-tmpl", store.TemplateScopeGlobal, "", bob.ID)
	projTmpl := createAuthzTestTemplate(t, s, "caps-project-tmpl", store.TemplateScopeProject, project.ID, alice.ID)

	ensureHubMembership(ctx, s, bob.ID)

	// Charlie holds project-member in project + global-catalog-author at system scope.
	charlie := createNamedTestUser(t, s, "tmpl-caps-charlie", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	createTestUserWithProjectRole(t, s, charlie.ID, charlie.Email, project.ID, store.ProjectRoleMember)
	bindTemplateCatalogAuthor(t, s, charlie.ID)

	// Dana holds hub-admin (not a member of project).
	dana := createNamedTestUser(t, s, "tmpl-caps-hubadmin", store.UserRoleMember)
	ensureHubMembership(ctx, s, dana.ID)
	bindTemplateHubAdmin(t, s, dana.ID)

	type tmplWithCaps struct {
		ID           string        `json:"id"`
		Capabilities *Capabilities `json:"_capabilities"`
	}
	getRecordCaps := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out tmplWithCaps
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.NotNil(t, out.Capabilities)
		return out.Capabilities.Actions
	}
	getListCaps := func(rec *httptest.ResponseRecorder) ([]string, map[string][]string) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var out ListTemplatesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.NotNil(t, out.Capabilities)
		items := make(map[string][]string, len(out.Templates))
		for _, item := range out.Templates {
			if item.Cap != nil {
				items[item.ID] = item.Cap.Actions
			}
		}
		return out.Capabilities.Actions, items
	}

	// 1. Record-level capabilities on globalTmpl:
	// Alice (project-owner of project, ordinary hub-member at system scope) -> ["read"].
	assert.Equal(t, []string{"read"}, getRecordCaps(doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+globalTmpl.ID, nil)))
	// Bob (stamped as legacy OwnerID on globalTmpl, ordinary hub-member) -> ["read"].
	assert.Equal(t, []string{"read"}, getRecordCaps(doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/templates/"+globalTmpl.ID, nil)))
	// Charlie (global-catalog-author) & Dana (hub-admin) -> ["read", "update", "delete"], never "*_global".
	for _, u := range []*store.User{charlie, dana} {
		caps := getRecordCaps(doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/templates/"+globalTmpl.ID, nil))
		assert.ElementsMatch(t, []string{"read", "update", "delete"}, caps)
		assert.NotContains(t, caps, "create_global")
		assert.NotContains(t, caps, "update_global")
		assert.NotContains(t, caps, "delete_global")
	}

	// Charlie on projTmpl (project-member lacks template.update/delete; global twins must NOT leak) -> ["read"].
	charlieProjCaps := getRecordCaps(doRequestAsUser(t, srv, charlie, http.MethodGet, "/api/v1/templates/"+projTmpl.ID, nil))
	assert.Equal(t, []string{"read"}, charlieProjCaps)

	// Scoped hub UAT with template:read + template:update_global -> ["read", "update"] (no delete).
	charlieUpdateTok := mintHubConfigToken(t, srv, charlie.ID, hubBoundary(), "template:read", "template:update_global")
	assert.ElementsMatch(t, []string{"read", "update"}, getRecordCaps(callWithKey(t, srv, charlieUpdateTok, http.MethodGet, "/api/v1/templates/"+globalTmpl.ID, nil)))

	// 2. List-level capabilities on GET /api/v1/templates?scope=global:
	aliceGlobalListCaps, aliceItems := getListCaps(doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates?scope=global", nil))
	assert.Empty(t, aliceGlobalListCaps, "project-owner without template.create_global must NOT see create on ?scope=global")
	assert.Equal(t, []string{"read"}, aliceItems[globalTmpl.ID])

	for _, u := range []*store.User{charlie, dana} {
		listCaps, items := getListCaps(doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/templates?scope=global", nil))
		assert.Equal(t, []string{"create"}, listCaps, "user %s must see [create] on ?scope=global", u.ID)
		assert.NotContains(t, listCaps, "create_global")
		assert.ElementsMatch(t, []string{"read", "update", "delete"}, items[globalTmpl.ID])
	}

	// Hub UAT on ?scope=global: plain template:create -> []; template:create_global -> ["create"].
	alicePlainCreateTok := mintHubConfigToken(t, srv, alice.ID, hubBoundary(), "template:list", "template:read", "template:create")
	plainListCaps, _ := getListCaps(callWithKey(t, srv, alicePlainCreateTok, http.MethodGet, "/api/v1/templates?scope=global", nil))
	assert.Empty(t, plainListCaps, "hub UAT with plain template:create must not get create capability on ?scope=global")

	charlieTwinCreateTok := mintHubConfigToken(t, srv, charlie.ID, hubBoundary(), "template:list", "template:read", "template:create_global")
	twinListCaps, _ := getListCaps(callWithKey(t, srv, charlieTwinCreateTok, http.MethodGet, "/api/v1/templates?scope=global", nil))
	assert.Equal(t, []string{"create"}, twinListCaps)

	// 3. List-level capabilities on GET /api/v1/templates?scope=project&scopeId=...:
	charlieProjListCaps, _ := getListCaps(doRequestAsUser(t, srv, charlie, http.MethodGet, "/api/v1/templates?scope=project&scopeId="+project.ID, nil))
	assert.Equal(t, []string{"create"}, charlieProjListCaps, "project-member must see [create] on own project")

	bobProjListCaps, _ := getListCaps(doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/templates?scope=project&scopeId="+project.ID, nil))
	assert.Empty(t, bobProjListCaps)

	danaProjListCaps, _ := getListCaps(doRequestAsUser(t, srv, dana, http.MethodGet, "/api/v1/templates?scope=project&scopeId="+project.ID, nil))
	assert.Empty(t, danaProjListCaps, "non-member hub-admin must NOT see [create] on project list")

	// Non-member global-catalog-author must NOT see [create] on another project.
	otherProject := &store.Project{ID: tid("caps-other-proj"), Name: "Other Project", Slug: "caps-other-proj"}
	require.NoError(t, s.CreateProject(ctx, otherProject))
	charlieOtherProjCaps, _ := getListCaps(doRequestAsUser(t, srv, charlie, http.MethodGet, "/api/v1/templates?scope=project&scopeId="+otherProject.ID, nil))
	assert.Empty(t, charlieOtherProjCaps, "global-catalog-author must NOT see [create] on non-member project")

	// 4. List-level capabilities on GET /api/v1/templates?scope=user:
	bobUserListCaps, _ := getListCaps(doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/templates?scope=user", nil))
	assert.Equal(t, []string{"create"}, bobUserListCaps, "local user must see [create] on ?scope=user")
}

// TestExplainAPI_GlobalTemplateWriteUsesTwin pins that POST /api/v1/authz/explain
// without an explicit permission resolves update/delete on a global template to
// template.update_global / template.delete_global, matching runtime enforcement.
func TestExplainAPI_GlobalTemplateWriteUsesTwin(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	ctx := context.Background()

	globalTmpl := createAuthzTestTemplate(t, s, "explain-global-tmpl", store.TemplateScopeGlobal, "", "")
	projectTmpl := createAuthzTestTemplate(t, s, "explain-project-tmpl", store.TemplateScopeProject, project.ID, alice.ID)

	charlie := createNamedTestUser(t, s, "explain-plain-tmpl-user", store.UserRoleMember)
	ensureHubMembership(ctx, s, charlie.ID)
	customRole := &store.RoleDefinition{
		ID:          tid("explain-plain-tmpl-rw"),
		Name:        "explain-plain-tmpl-rw",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"template.update", "template.delete"},
	}
	_, err := s.CreateRoleDefinition(ctx, customRole)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: customRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      charlie.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        alice.ID,
	})
	require.NoError(t, err)

	ensureHubMembership(ctx, s, bob.ID)
	bindTemplateCatalogAuthor(t, s, bob.ID)

	explain := func(t *testing.T, u *store.User, tmplID, action string) explainResponse {
		t.Helper()
		body, err := json.Marshal(map[string]interface{}{
			"resource": map[string]interface{}{"type": "template", "id": tmplID},
			"action":   action,
		})
		require.NoError(t, err)
		identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "api")
		req := newRequestWithIdentity(t, http.MethodPost, "/api/v1/authz/explain", body, identity)
		rec := httptest.NewRecorder()
		srv.handleAuthzExplain(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp explainResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	for _, action := range []string{"update", "delete"} {
		t.Run(action, func(t *testing.T) {
			denied := explain(t, charlie, globalTmpl.ID, action)
			assert.False(t, denied.Allowed, "plain template.%s must not allow a global template write", action)
			require.NotNil(t, denied.Provenance)
			assert.Equal(t, "template."+action+"_global", denied.Provenance.Permission)

			allowed := explain(t, bob, globalTmpl.ID, action)
			assert.True(t, allowed.Allowed, "global-catalog-author must be allowed to %s a global template", action)
			require.NotNil(t, allowed.Provenance)
			assert.Equal(t, "template."+action+"_global", allowed.Provenance.Permission)

			// Project templates keep the plain permission.
			plain := explain(t, charlie, projectTmpl.ID, action)
			require.NotNil(t, plain.Provenance)
			assert.Equal(t, "template."+action, plain.Provenance.Permission)
		})
	}
}
