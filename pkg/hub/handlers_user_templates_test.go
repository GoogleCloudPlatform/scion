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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setupUserTemplateTest creates a test server with two users (alice and bob)
// and returns the server, store, and both users. Both users have hub membership.
func setupUserTemplateTest(t *testing.T) (*Server, store.Store, *store.User, *store.User) {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          tid("ut-alice"),
		Email:       "alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob := &store.User{
		ID:          tid("ut-bob"),
		Email:       "bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	return srv, s, alice, bob
}

// createUserTemplate creates a user-scoped template directly in the store.
func createUserTemplate(t *testing.T, s store.Store, ownerID, name string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:         api.NewUUID(),
		Name:       name,
		Slug:       api.Slugify(name),
		Harness:    "antigravity",
		Scope:      store.TemplateScopeUser,
		ScopeID:    ownerID,
		OwnerID:    ownerID,
		CreatedBy:  ownerID,
		Status:     store.TemplateStatusActive,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// TestListUserTemplates_IsolationBetweenUsers verifies that listing
// /api/v1/users/me/templates returns only the caller's user-scoped templates.
func TestListUserTemplates_IsolationBetweenUsers(t *testing.T) {
	srv, s, alice, bob := setupUserTemplateTest(t)

	// Create templates owned by each user.
	aliceTmpl := createUserTemplate(t, s, alice.ID, "alice-template")
	_ = createUserTemplate(t, s, bob.ID, "bob-template")

	// Alice lists her templates — should see only hers.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/users/me/templates", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, 1, resp.TotalCount, "alice should see exactly 1 template")
	require.Len(t, resp.Templates, 1)
	assert.Equal(t, aliceTmpl.ID, resp.Templates[0].ID)
	assert.Equal(t, "alice-template", resp.Templates[0].Name)

	// Bob lists his templates — should see only his.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/users/me/templates", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.TotalCount, "bob should see exactly 1 template")
	require.Len(t, resp.Templates, 1)
	assert.Equal(t, "bob-template", resp.Templates[0].Name)
}

// TestCreateUserTemplate_SetsScopeIDAndOwnerID verifies that creating a template
// via /api/v1/users/me/templates forces ScopeID and OwnerID from the authenticated user.
func TestCreateUserTemplate_SetsScopeIDAndOwnerID(t *testing.T) {
	srv, _, alice, _ := setupUserTemplateTest(t)

	body := CreateTemplateRequest{
		Name:    "my-template",
		Harness: "antigravity",
	}

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, store.TemplateScopeUser, resp.Template.Scope)
	assert.Equal(t, alice.ID, resp.Template.ScopeID, "ScopeID should be set to the authenticated user")
	assert.Equal(t, alice.ID, resp.Template.OwnerID, "OwnerID should be set to the authenticated user")
	assert.Equal(t, alice.ID, resp.Template.CreatedBy, "CreatedBy should be set to the authenticated user")
}

// TestGetUserTemplate_NotFoundForOtherUser verifies that GET returns 404
// when a user tries to access another user's template.
func TestGetUserTemplate_NotFoundForOtherUser(t *testing.T) {
	srv, s, alice, bob := setupUserTemplateTest(t)

	// Create a template owned by alice.
	aliceTmpl := createUserTemplate(t, s, alice.ID, "alice-private")

	// Alice can access her own template.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/users/me/templates/"+aliceTmpl.ID, nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Bob cannot access Alice's template — should get 404.
	rec = doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/users/me/templates/"+aliceTmpl.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestUpdateUserTemplate_NotFoundForOtherUser verifies that PUT returns 404
// when a user tries to update another user's template.
func TestUpdateUserTemplate_NotFoundForOtherUser(t *testing.T) {
	srv, s, alice, bob := setupUserTemplateTest(t)

	aliceTmpl := createUserTemplate(t, s, alice.ID, "alice-update-test")

	updateBody := store.Template{
		Name:        "updated-name",
		Description: "updated description",
		Status:      store.TemplateStatusActive,
		Visibility:  store.VisibilityPrivate,
	}

	// Alice can update her own template.
	rec := doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/users/me/templates/"+aliceTmpl.ID, updateBody)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Bob cannot update Alice's template — should get 404.
	rec = doRequestAsUser(t, srv, bob, http.MethodPut, "/api/v1/users/me/templates/"+aliceTmpl.ID, updateBody)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// TestDeleteUserTemplate_NotFoundForOtherUser verifies that DELETE returns 404
// when a user tries to delete another user's template.
func TestDeleteUserTemplate_NotFoundForOtherUser(t *testing.T) {
	srv, s, alice, bob := setupUserTemplateTest(t)

	aliceTmpl := createUserTemplate(t, s, alice.ID, "alice-delete-test")

	// Bob cannot delete Alice's template — should get 404.
	rec := doRequestAsUser(t, srv, bob, http.MethodDelete, "/api/v1/users/me/templates/"+aliceTmpl.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Alice can delete her own template.
	rec = doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/users/me/templates/"+aliceTmpl.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestListTemplatesV2_UserScopeIsolation verifies the R1 fix: listing templates
// via the generic /api/v1/templates?scope=user endpoint restricts results to
// the authenticated user's templates.
func TestListTemplatesV2_UserScopeIsolation(t *testing.T) {
	srv, s, alice, bob := setupUserTemplateTest(t)

	_ = createUserTemplate(t, s, alice.ID, "alice-v2-template")
	_ = createUserTemplate(t, s, bob.ID, "bob-v2-template")

	// Alice lists with scope=user via the generic endpoint.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/templates?scope=user", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Alice should only see her own template, not Bob's.
	require.Len(t, resp.Templates, 1, "generic scope=user should return only caller's templates")
	assert.Equal(t, "alice-v2-template", resp.Templates[0].Name)
}

// TestCreateTemplateV2_ScopeIDInjectionBlocked verifies the R2 fix: creating
// a user-scoped template via the generic /api/v1/templates endpoint always
// forces ScopeID from the authenticated user, ignoring any caller-supplied value.
func TestCreateTemplateV2_ScopeIDInjectionBlocked(t *testing.T) {
	srv, _, alice, bob := setupUserTemplateTest(t)

	// Alice tries to create a template with Bob's ID as ScopeID.
	body := CreateTemplateRequest{
		Name:    "injected-template",
		Harness: "antigravity",
		Scope:   store.TemplateScopeUser,
		ScopeID: bob.ID, // Attempt to inject Bob's ID
	}

	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/templates", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateTemplateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// ScopeID must be Alice's, not Bob's.
	assert.Equal(t, alice.ID, resp.Template.ScopeID, "ScopeID should be forced to the caller's ID")
	assert.Equal(t, alice.ID, resp.Template.OwnerID, "OwnerID should be forced to the caller's ID")
}

// TestIsUserTemplateOwner tests the ownership helper function.
func TestIsUserTemplateOwner(t *testing.T) {
	uid := "user-123"

	tests := []struct {
		name     string
		template *store.Template
		want     bool
	}{
		{
			name:     "owner by OwnerID",
			template: &store.Template{Scope: store.TemplateScopeUser, OwnerID: uid, ScopeID: "other"},
			want:     true,
		},
		{
			name:     "owner by ScopeID",
			template: &store.Template{Scope: store.TemplateScopeUser, OwnerID: "other", ScopeID: uid},
			want:     true,
		},
		{
			name:     "owner by both",
			template: &store.Template{Scope: store.TemplateScopeUser, OwnerID: uid, ScopeID: uid},
			want:     true,
		},
		{
			name:     "not owner",
			template: &store.Template{Scope: store.TemplateScopeUser, OwnerID: "other", ScopeID: "other"},
			want:     false,
		},
		{
			name:     "wrong scope global",
			template: &store.Template{Scope: store.TemplateScopeGlobal, OwnerID: uid},
			want:     false,
		},
		{
			name:     "wrong scope project",
			template: &store.Template{Scope: store.TemplateScopeProject, OwnerID: uid},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isUserTemplateOwner(tt.template, uid)
			assert.Equal(t, tt.want, got)
		})
	}
}
