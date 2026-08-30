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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestProjectForPSH creates a test project for pre-start hook tests.
func createTestProjectForPSH(t *testing.T, s store.Store) *store.Project {
	t.Helper()
	project := &store.Project{
		ID:      tid("test-project-psh-" + t.Name()),
		Name:    "Test Project PSH",
		Slug:    "test-project-psh-" + strings.ToLower(t.Name()),
		OwnerID: "dev@localhost",
	}
	require.NoError(t, s.CreateProject(t.Context(), project))
	return project
}

// =============================================================================
// GET /pre-start-hooks — list (empty)
// =============================================================================

func TestProjectPreStartHooks_List_Empty(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/pre-start-hooks", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp ListProjectPreStartHooksResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Hooks)
}

// =============================================================================
// POST /pre-start-hooks — create
// =============================================================================

func TestProjectPreStartHooks_Create(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	body := CreateProjectPreStartHookRequest{
		Name:   "Install dev tools",
		Script: "#!/bin/sh\napt-get install -y jq\n",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var hook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&hook))
	assert.Equal(t, "Install dev tools", hook.Name)
	assert.Equal(t, "install-dev-tools", hook.Slug)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, hook.Status)
	assert.NotEmpty(t, hook.ID)
}

func TestProjectPreStartHooks_Create_MissingName(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	body := CreateProjectPreStartHookRequest{Script: "#!/bin/sh\n"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectPreStartHooks_Create_MissingScript(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	body := CreateProjectPreStartHookRequest{Name: "My hook"}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectPreStartHooks_Create_ScriptTooLarge(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// 65 KB script — over the 64 KB limit.
	bigScript := strings.Repeat("x", 65*1024)
	body := CreateProjectPreStartHookRequest{Name: "Big hook", Script: bigScript}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectPreStartHooks_Create_ArchivesPreviousActive(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// Create first hook.
	first := CreateProjectPreStartHookRequest{
		Name:   "First hook",
		Slug:   "first-hook",
		Script: "#!/bin/sh\necho first\n",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", first)
	require.Equal(t, http.StatusCreated, rec.Code)
	var firstHook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&firstHook))

	// Create second hook — should archive first.
	second := CreateProjectPreStartHookRequest{
		Name:   "Second hook",
		Slug:   "second-hook",
		Script: "#!/bin/sh\necho second\n",
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", second)
	require.Equal(t, http.StatusCreated, rec.Code)

	// GET the first hook — must be archived.
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+firstHook.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var reloaded store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&reloaded))
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloaded.Status)
}

// =============================================================================
// GET /pre-start-hooks/{id} — show
// =============================================================================

func TestProjectPreStartHooks_Get(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// Create one.
	body := CreateProjectPreStartHookRequest{
		Name:   "My hook",
		Script: "#!/bin/sh\necho hello\n",
	}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	// GET by ID.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+created.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&got))
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "#!/bin/sh\necho hello\n", got.Script)
}

func TestProjectPreStartHooks_Get_NotFound(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/pre-start-hooks/00000000-0000-0000-0000-000000000001", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// =============================================================================
// PUT /pre-start-hooks/{id} — update
// =============================================================================

func TestProjectPreStartHooks_Update(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	body := CreateProjectPreStartHookRequest{
		Name:   "Original",
		Script: "#!/bin/sh\necho original\n",
	}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	newName := "Updated name"
	newScript := "#!/bin/sh\necho updated\n"
	updateBody := UpdateProjectPreStartHookRequest{
		Name:   &newName,
		Script: &newScript,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+created.ID, updateBody)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var updated store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&updated))
	assert.Equal(t, "Updated name", updated.Name)
	assert.Equal(t, "#!/bin/sh\necho updated\n", updated.Script)
}

func TestProjectPreStartHooks_Update_EmptyNameRejected(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	body := CreateProjectPreStartHookRequest{Name: "Original", Script: "#!/bin/sh\necho original\n"}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	require.Equal(t, http.StatusCreated, createRec.Code)
	var created store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&created))

	emptyName := ""
	updateBody := UpdateProjectPreStartHookRequest{Name: &emptyName}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+created.ID, updateBody)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "empty name should be rejected")
}

// =============================================================================
// POST /pre-start-hooks/{id}/activate
// =============================================================================

func TestProjectPreStartHooks_Activate(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// Create two hooks (second is active, first is archived).
	firstBody := CreateProjectPreStartHookRequest{Name: "First", Slug: "first", Script: "#!/bin/sh\n"}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", firstBody)
	require.Equal(t, http.StatusCreated, rec1.Code)
	var first store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&first))

	secondBody := CreateProjectPreStartHookRequest{Name: "Second", Slug: "second", Script: "#!/bin/sh\n"}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", secondBody)
	require.Equal(t, http.StatusCreated, rec2.Code)
	var second store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&second))

	// Activate first — it should become active; second should become archived.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+first.ID+"/activate", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var activated store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&activated))
	assert.Equal(t, store.ProjectPreStartHookStatusActive, activated.Status)
	assert.Equal(t, first.ID, activated.ID)

	// Verify second is now archived.
	recSecond := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+second.ID, nil)
	require.Equal(t, http.StatusOK, recSecond.Code)
	var reloadedSecond store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(recSecond.Body).Decode(&reloadedSecond))
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloadedSecond.Status)
}

// =============================================================================
// DELETE /pre-start-hooks/{id}
// =============================================================================

func TestProjectPreStartHooks_Delete_OnlyActive_Succeeds(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// When this is the only hook, deleting the active hook is allowed.
	body := CreateProjectPreStartHookRequest{Name: "Hook", Script: "#!/bin/sh\n"}
	createRec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", body)
	require.Equal(t, http.StatusCreated, createRec.Code)
	var hook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(createRec.Body).Decode(&hook))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+hook.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestProjectPreStartHooks_Delete_Active_WithOtherHooks_Rejected(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// Create two hooks so the first is archived and the second is active.
	first := CreateProjectPreStartHookRequest{Name: "First", Slug: "first", Script: "#!/bin/sh\n"}
	doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", first)

	second := CreateProjectPreStartHookRequest{Name: "Second", Slug: "second", Script: "#!/bin/sh\n"}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", second)
	require.Equal(t, http.StatusCreated, rec2.Code)
	var secondHook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&secondHook))

	// Deleting the active hook when another exists must fail.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+secondHook.ID, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestProjectPreStartHooks_Delete_Archived(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForPSH(t, s)

	// Create two so the first becomes archived.
	first := CreateProjectPreStartHookRequest{Name: "First", Slug: "first", Script: "#!/bin/sh\n"}
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", first)
	require.Equal(t, http.StatusCreated, rec1.Code)
	var firstHook store.ProjectPreStartHook
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&firstHook))

	second := CreateProjectPreStartHookRequest{Name: "Second", Slug: "second", Script: "#!/bin/sh\n"}
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/pre-start-hooks", second)
	require.Equal(t, http.StatusCreated, rec2.Code)

	// Delete the archived first hook.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+firstHook.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify it's gone.
	recGet := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/pre-start-hooks/"+firstHook.ID, nil)
	assert.Equal(t, http.StatusNotFound, recGet.Code)
}
