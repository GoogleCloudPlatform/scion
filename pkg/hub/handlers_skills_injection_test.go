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

// =============================================================================
// Helpers
// =============================================================================

// setupInjectedSkillsTest creates a test server with a project owned by alice
// and a second user bob who is NOT a project member. The dev user (from testServer)
// is a hub admin. All three identities can be used via doRequest (dev/admin),
// doRequestAsUser(alice), or doRequestAsUser(bob).
func setupInjectedSkillsTest(t *testing.T) (*Server, store.Store, *store.Project, *store.User, *store.User) {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          tid("si-user-alice"),
		Email:       "alice@skills-test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	bob := &store.User{
		ID:          tid("si-user-bob"),
		Email:       "bob@skills-test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))
	ensureHubMembership(ctx, s, bob.ID)

	project := &store.Project{
		ID:        tid("si-project-alpha"),
		Name:      "Alpha Project",
		Slug:      "alpha-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	// Create the project members group so authz works correctly.
	// createProjectMembersGroupAndPolicy also adds alice (CreatedBy) as an owner.
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	return srv, s, project, alice, bob
}

// =============================================================================
// Project-scope: list
// =============================================================================

func TestListProjectInjectedSkills_EmptyByDefault(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Entries)
}

func TestListProjectInjectedSkills_ReturnsEntries(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed an entry.
	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://test-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Entries, 1)
	assert.Equal(t, "scion://test-skill@1.0", resp.Entries[0].SkillURI)
	assert.NotEmpty(t, resp.Entries[0].ID)
}

func TestListProjectInjectedSkills_IsolatedBetweenProjects(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Create a second project with its own entry.
	project2 := &store.Project{
		ID:        tid("si-project-beta"),
		Name:      "Beta Project",
		Slug:      "beta-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project2))
	// createProjectMembersGroupAndPolicy also adds alice (CreatedBy) as an owner.
	srv.createProjectMembersGroupAndPolicy(ctx, project2)

	si2 := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project2.ID,
		SkillURI: "scion://beta-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si2))

	// Alpha project should be empty.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Entries)
}

// =============================================================================
// Project-scope: add
// =============================================================================

func TestAddProjectInjectedSkill_Success(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	body := api.SkillInjectionEntry{SkillURI: "scion://my-skill@2.0", Optional: true}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	assert.NotEmpty(t, entry.ID)
	assert.Equal(t, "scion://my-skill@2.0", entry.SkillURI)
	assert.True(t, entry.Optional)

	// Verify in store.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Len(t, sis, 1)
	assert.Equal(t, "scion://my-skill@2.0", sis[0].SkillURI)
}

func TestAddProjectInjectedSkill_MissingSkillURI(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: ""}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAddProjectInjectedSkill_DuplicateReturnsConflict(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: "scion://dup-skill@1.0"}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec2 := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	assert.Equal(t, http.StatusConflict, rec2.Code)
}

// =============================================================================
// Project-scope: set (bulk replace)
// =============================================================================

func TestSetProjectInjectedSkills_ReplacesListAtomically(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed one entry.
	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://old-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	// Replace with two new entries.
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "scion://new-skill-a@1.0"},
			{SkillURI: "scion://new-skill-b@2.0", Optional: true},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Entries, 2)

	// Verify in store: old entry gone.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Len(t, sis, 2)
	uris := []string{sis[0].SkillURI, sis[1].SkillURI}
	assert.Contains(t, uris, "scion://new-skill-a@1.0")
	assert.Contains(t, uris, "scion://new-skill-b@2.0")
}

func TestSetProjectInjectedSkills_EmptyListClearsAll(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://to-be-cleared@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills",
		api.SkillInjectionList{Entries: []api.SkillInjectionEntry{}})
	require.Equal(t, http.StatusOK, rec.Code)

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis)
}

// =============================================================================
// Project-scope: delete
// =============================================================================

func TestRemoveProjectInjectedSkill_Success(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "scion://removable-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/injected-skills/"+si.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis)
}

func TestRemoveProjectInjectedSkill_NotFound(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/injected-skills/"+tid("nonexistent-entry"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// =============================================================================
// Project-scope: authorization
// =============================================================================

func TestProjectInjectedSkills_UnauthorizedWithoutToken(t *testing.T) {
	srv, _, project, _, _ := setupInjectedSkillsTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/injected-skills", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestProjectInjectedSkills_ForbiddenForNonMember(t *testing.T) {
	srv, _, project, _, bob := setupInjectedSkillsTest(t)

	// Bob is not a member of the project, so POST should be forbidden.
	body := api.SkillInjectionEntry{SkillURI: "scion://forbidden@1.0"}
	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestProjectInjectedSkills_NotFoundForMissingProject(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+tid("does-not-exist")+"/injected-skills", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// =============================================================================
// User-scope: list
// =============================================================================

func TestListUserInjectedSkills_EmptyByDefault(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/users/me/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Entries)
}

func TestListUserInjectedSkills_ReturnsOwnEntries(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  alice.ID,
		SkillURI: "scion://alice-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/users/me/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Entries, 1)
	assert.Equal(t, "scion://alice-skill@1.0", resp.Entries[0].SkillURI)
}

func TestListUserInjectedSkills_IsolatedBetweenUsers(t *testing.T) {
	srv, s, _, alice, bob := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Add a skill to bob's list.
	siBob := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  bob.ID,
		SkillURI: "scion://bob-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, siBob))

	// Alice's list should be empty.
	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/users/me/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Entries)
}

// =============================================================================
// User-scope: add
// =============================================================================

func TestAddUserInjectedSkill_Success(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	body := api.SkillInjectionEntry{SkillURI: "scion://my-user-skill@1.0", SkillAs: "alias"}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	assert.NotEmpty(t, entry.ID)
	assert.Equal(t, "scion://my-user-skill@1.0", entry.SkillURI)
	assert.Equal(t, "alias", entry.SkillAs)

	// Verify in store.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	assert.Len(t, sis, 1)
}

func TestAddUserInjectedSkill_MissingSkillURI(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: ""}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAddUserInjectedSkill_DuplicateReturnsConflict(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: "scion://dup-user-skill@1.0"}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec2 := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	assert.Equal(t, http.StatusConflict, rec2.Code)
}

// =============================================================================
// User-scope: set (bulk replace)
// =============================================================================

func TestSetUserInjectedSkills_ReplacesListAtomically(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed.
	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  alice.ID,
		SkillURI: "scion://old-user-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "scion://new-user-skill-a@1.0"},
			{SkillURI: "scion://new-user-skill-b@2.0"},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Entries, 2)

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	assert.Len(t, sis, 2)
}

// =============================================================================
// User-scope: delete
// =============================================================================

func TestRemoveUserInjectedSkill_Success(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  alice.ID,
		SkillURI: "scion://removable-user-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/users/me/injected-skills/"+si.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	assert.Empty(t, sis)
}

func TestRemoveUserInjectedSkill_NotFound(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/users/me/injected-skills/"+tid("nonexistent-user-entry"), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// =============================================================================
// User-scope: authorization
// =============================================================================

func TestUserInjectedSkills_UnauthorizedWithoutToken(t *testing.T) {
	srv, _, _, _, _ := setupInjectedSkillsTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/users/me/injected-skills", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// =============================================================================
// Hub-scope: GET
// =============================================================================

func TestGetHubInjectedSkills_EmptyByDefault(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/hub/settings/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.System)
	assert.Empty(t, resp.UserDefined)
}

func TestGetHubInjectedSkills_AnyAuthenticatedUserCanRead(t *testing.T) {
	srv, _, _, _, bob := setupInjectedSkillsTest(t)

	// Bob is just a member, not admin.
	rec := doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/hub/settings/injected-skills", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetHubInjectedSkills_UnauthorizedWithoutToken(t *testing.T) {
	srv, _, _, _, _ := setupInjectedSkillsTest(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet,
		"/api/v1/hub/settings/injected-skills", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGetHubInjectedSkills_ReturnsStoredSetting(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed via store.
	setting := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: "scion://platform-skill@1.0"}},
		UserDefined: []api.SkillReference{{URI: "scion://admin-skill@1.0"}},
	}
	raw, err := json.Marshal(setting)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, alice.ID, -1, "managed")
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/hub/settings/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.System, 1)
	assert.Equal(t, "scion://platform-skill@1.0", resp.System[0].URI)
	assert.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "scion://admin-skill@1.0", resp.UserDefined[0].URI)
}

// =============================================================================
// Hub-scope: PUT
// =============================================================================

func TestSetHubInjectedSkills_AdminCanUpdate(t *testing.T) {
	srv, s, _, _, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Dev user is admin. Use doRequest which uses the dev token.
	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "scion://hub-custom-skill@1.0"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "scion://hub-custom-skill@1.0", resp.UserDefined[0].URI)

	// Verify in store.
	hs, err := s.GetHubSetting(ctx, "injected_skills")
	require.NoError(t, err)
	var stored api.HubSkillInjectionSetting
	require.NoError(t, json.Unmarshal(hs.Value, &stored))
	assert.Len(t, stored.UserDefined, 1)
}

func TestSetHubInjectedSkills_PreservesSystemEntries(t *testing.T) {
	srv, s, _, _, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed a system entry (simulating seeded platform skills).
	initial := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: "scion://system-skill@1.0"}},
		UserDefined: []api.SkillReference{},
	}
	raw, err := json.Marshal(initial)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "system", -1, "seeded")
	require.NoError(t, err)

	// Admin updates user_defined only.
	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "scion://admin-added-skill@1.0"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// System entry must still be present.
	assert.Len(t, resp.System, 1)
	assert.Equal(t, "scion://system-skill@1.0", resp.System[0].URI)
	assert.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "scion://admin-added-skill@1.0", resp.UserDefined[0].URI)
}

func TestSetHubInjectedSkills_ForbiddenForNonAdmin(t *testing.T) {
	srv, _, _, _, bob := setupInjectedSkillsTest(t)

	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "scion://unauthorized-skill@1.0"},
		},
	}
	rec := doRequestAsUser(t, srv, bob, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestSetHubInjectedSkills_UnauthorizedWithoutToken(t *testing.T) {
	srv, _, _, _, _ := setupInjectedSkillsTest(t)

	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{},
	}
	rec := doRequestNoAuth(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
