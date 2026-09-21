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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTemplateAuthzTest creates a test server with two users and a project.
// Alice is a hub member and project owner. Bob is NOT a hub member, so the
// seeded hub-member-read-all policy does not grant him read access.
func setupTemplateAuthzTest(t *testing.T) (srv *Server, s store.Store, alice, bob *store.User, project *store.Project) {
	t.Helper()

	srv, s = testServer(t)
	ctx := context.Background()

	alice = &store.User{
		ID:          tid("tpl-alice"),
		Email:       "tpl-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob = &store.User{
		ID:          tid("tpl-bob"),
		Email:       "tpl-bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	ensureHubMembership(ctx, s, alice.ID)
	// Bob is intentionally NOT added to hub-members, so default-deny applies.

	project = &store.Project{
		ID:        tid("tpl-project"),
		Name:      "Template Project",
		Slug:      "template-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	return srv, s, alice, bob, project
}

// createAuthzTestTemplate inserts a template directly into the store.
func createAuthzTestTemplate(t *testing.T, s store.Store, name, scope, scopeID, ownerID string) *store.Template {
	t.Helper()
	tpl := &store.Template{
		ID:          api.NewUUID(),
		Name:        name,
		Slug:        api.Slugify(name),
		Scope:       scope,
		ScopeID:     scopeID,
		OwnerID:     ownerID,
		Status:      "active",
		Visibility:  store.VisibilityPrivate,
		StoragePath: fmt.Sprintf("templates/%s/%s", scope, api.Slugify(name)),
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tpl))
	return tpl
}

// ============================================================================
// updateTemplateV2 (PUT) authorization tests
// ============================================================================

func TestTemplateAuthz_Update_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "update-priv", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodPut, "/api/v1/templates/"+tpl.ID, store.Template{
		Name:   "pwned",
		Status: "active",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should not be able to update a project template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Update_MemberDeniedOnGlobal(t *testing.T) {
	srv, s, _, _, _ := setupTemplateAuthzTest(t)
	// Create a global template owned by a different principal (not Alice).
	// Alice is a hub member but not the owner and not an admin.
	tpl := createAuthzTestTemplate(t, s, "global-tpl", store.TemplateScopeGlobal, "", "other-owner-id")

	// Alice is a hub member but not the owner — should be denied.
	rec := doRequestAsUser(t, srv, &store.User{
		ID:          tid("tpl-alice"),
		Email:       "tpl-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
	}, http.MethodPut, "/api/v1/templates/"+tpl.ID, store.Template{
		Name:   "pwned",
		Status: "active",
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"ordinary hub member should not be able to update a global template they don't own; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Update_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "owner-update", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/templates/"+tpl.ID, store.Template{
		Name:   "updated-name",
		Status: "active",
	})
	assert.Equal(t, http.StatusOK, rec.Code,
		"project owner should be able to update own template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Update_ScopeImmutable(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "scope-lock", store.TemplateScopeProject, project.ID, alice.ID)

	// Try to reparent the template to global scope and change owner via the
	// update body. The handler must preserve the original values.
	rec := doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/templates/"+tpl.ID, store.Template{
		Name:    "scope-lock-renamed",
		Status:  "active",
		Scope:   store.TemplateScopeGlobal,
		ScopeID: "",
		OwnerID: "attacker-id",
	})
	require.Equal(t, http.StatusOK, rec.Code,
		"update should succeed; got: %s", rec.Body.String())

	// Verify the stored record was NOT modified for Scope, ScopeID, OwnerID.
	stored, err := s.GetTemplate(context.Background(), tpl.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TemplateScopeProject, stored.Scope,
		"Scope must remain project, not be reparented to global")
	assert.Equal(t, project.ID, stored.ScopeID,
		"ScopeID must remain the original project ID")
	assert.Equal(t, alice.ID, stored.OwnerID,
		"OwnerID must remain the original owner")
}

// ============================================================================
// patchTemplateV2 (PATCH) authorization tests
// ============================================================================

func TestTemplateAuthz_Patch_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "patch-priv", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/templates/"+tpl.ID,
		map[string]string{"name": "pwned"})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should not be able to patch a project template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Patch_MemberDeniedOnGlobal(t *testing.T) {
	srv, s, _, _, _ := setupTemplateAuthzTest(t)
	// Global template owned by another principal.
	tpl := createAuthzTestTemplate(t, s, "global-patch", store.TemplateScopeGlobal, "", "other-owner-id")

	rec := doRequestAsUser(t, srv, &store.User{
		ID:          tid("tpl-alice"),
		Email:       "tpl-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
	}, http.MethodPatch, "/api/v1/templates/"+tpl.ID,
		map[string]string{"name": "pwned"})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"ordinary hub member should not be able to patch a global template they don't own; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Patch_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "patch-owner", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/templates/"+tpl.ID,
		map[string]string{"name": "patched-name"})
	assert.Equal(t, http.StatusOK, rec.Code,
		"project owner should be able to patch own template; got: %s", rec.Body.String())
}

// ============================================================================
// handleTemplateUpload authorization tests
// ============================================================================

func TestTemplateAuthz_Upload_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "upload-priv", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/upload",
		UploadRequest{Files: []FileUploadRequest{{Path: "TEMPLATE.md", Size: 100}}})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should not be able to upload to a project template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Upload_MemberDeniedOnGlobal(t *testing.T) {
	srv, s, _, _, _ := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "global-upload", store.TemplateScopeGlobal, "", "other-owner-id")

	rec := doRequestAsUser(t, srv, &store.User{
		ID:          tid("tpl-alice"),
		Email:       "tpl-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
	}, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/upload",
		UploadRequest{Files: []FileUploadRequest{{Path: "TEMPLATE.md", Size: 100}}})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"ordinary hub member should not be able to upload to a global template they don't own; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Upload_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "upload-owner", store.TemplateScopeProject, project.ID, alice.ID)

	// The upload will fail due to storage not configured, but the authz gate
	// must pass (we'd get 403 if it didn't, not 500).
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/upload",
		UploadRequest{Files: []FileUploadRequest{{Path: "TEMPLATE.md", Size: 100}}})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project owner should pass the authz gate for upload; got: %s", rec.Body.String())
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
		"project owner should not get 401; got: %s", rec.Body.String())
}

// ============================================================================
// handleTemplateFinalize authorization tests
// ============================================================================

func TestTemplateAuthz_Finalize_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "fin-priv", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/finalize",
		FinalizeRequest{Manifest: &TemplateManifest{Files: []store.TemplateFile{{Path: "TEMPLATE.md"}}}})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should not be able to finalize a project template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Finalize_MemberDeniedOnGlobal(t *testing.T) {
	srv, s, _, _, _ := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "global-fin", store.TemplateScopeGlobal, "", "other-owner-id")

	rec := doRequestAsUser(t, srv, &store.User{
		ID:          tid("tpl-alice"),
		Email:       "tpl-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
	}, http.MethodPost, "/api/v1/templates/"+tpl.ID+"/finalize",
		FinalizeRequest{Manifest: &TemplateManifest{Files: []store.TemplateFile{{Path: "TEMPLATE.md"}}}})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"ordinary hub member should not be able to finalize a global template they don't own; got: %s", rec.Body.String())
}

// ============================================================================
// handleTemplateDownload authorization tests
// ============================================================================

func TestTemplateAuthz_Download_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "dl-priv", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/download", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should not be able to download a project template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Download_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "dl-owner", store.TemplateScopeProject, project.ID, alice.ID)

	// The download will fail due to no files, but the authz gate must pass.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/download", nil)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project owner should pass the authz gate for download; got: %s", rec.Body.String())
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
		"project owner should not get 401; got: %s", rec.Body.String())
}

// ============================================================================
// handleTemplateValidate authorization tests
// ============================================================================

func TestTemplateAuthz_Validate_NonMemberDenied(t *testing.T) {
	srv, s, alice, bob, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "val-priv", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/validate", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should not be able to validate a project template; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Validate_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "val-owner", store.TemplateScopeProject, project.ID, alice.ID)

	// Validate may fail due to storage, but authz must pass.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates/"+tpl.ID+"/validate", nil)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project owner should pass the authz gate for validate; got: %s", rec.Body.String())
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
		"project owner should not get 401; got: %s", rec.Body.String())
}

// ============================================================================
// Scope / ScopeID / OwnerID immutability — stored record assertion
// ============================================================================

func TestTemplateAuthz_Update_ScopeFieldsImmutableInStore(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "immut-store", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/templates/"+tpl.ID, store.Template{
		Name:    "immut-store-renamed",
		Status:  "active",
		Scope:   store.TemplateScopeGlobal,
		ScopeID: "evil-project",
		OwnerID: "evil-owner",
	})
	require.Equal(t, http.StatusOK, rec.Code, "update should succeed; got: %s", rec.Body.String())

	// Verify the response body reflects the preserved values.
	var resp store.Template
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, store.TemplateScopeProject, resp.Scope,
		"response Scope must be the original, not the attacker's value")
	assert.Equal(t, project.ID, resp.ScopeID,
		"response ScopeID must be the original project ID")
	assert.Equal(t, alice.ID, resp.OwnerID,
		"response OwnerID must be the original owner")

	// Verify the stored record was NOT modified for Scope, ScopeID, OwnerID.
	stored, err := s.GetTemplate(context.Background(), tpl.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TemplateScopeProject, stored.Scope,
		"stored Scope must remain project, not be reparented to global")
	assert.Equal(t, project.ID, stored.ScopeID,
		"stored ScopeID must remain the original project ID")
	assert.Equal(t, alice.ID, stored.OwnerID,
		"stored OwnerID must remain the original owner")
}

// ============================================================================
// Unauthenticated access
// ============================================================================

func TestTemplateAuthz_Update_UnauthenticatedDenied(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "unauth-update", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestNoAuth(t, srv, http.MethodPut, "/api/v1/templates/"+tpl.ID, store.Template{
		Name:   "pwned",
		Status: "active",
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated PUT should be rejected; got: %s", rec.Body.String())
}

func TestTemplateAuthz_Patch_UnauthenticatedDenied(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	tpl := createAuthzTestTemplate(t, s, "unauth-patch", store.TemplateScopeProject, project.ID, alice.ID)

	rec := doRequestNoAuth(t, srv, http.MethodPatch, "/api/v1/templates/"+tpl.ID,
		map[string]string{"name": "pwned"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"unauthenticated PATCH should be rejected; got: %s", rec.Body.String())
}
