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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
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
	// createProjectMembersGroup also adds alice (CreatedBy) as an owner.
	srv.createProjectMembersGroup(ctx, project)

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
		SkillURI: "skill://scion/test-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Entries, 1)
	assert.Equal(t, "skill://scion/test-skill@1.0", resp.Entries[0].SkillURI)
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
	// createProjectMembersGroup also adds alice (CreatedBy) as an owner.
	srv.createProjectMembersGroup(ctx, project2)

	si2 := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project2.ID,
		SkillURI: "skill://scion/beta-skill@1.0",
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

	body := api.SkillInjectionEntry{SkillURI: "skill://scion/my-skill@2.0", Optional: true}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	assert.NotEmpty(t, entry.ID)
	assert.Equal(t, "skill://scion/my-skill@2.0", entry.SkillURI)
	assert.True(t, entry.Optional)

	// Verify in store.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Len(t, sis, 1)
	assert.Equal(t, "skill://scion/my-skill@2.0", sis[0].SkillURI)
}

func TestAddProjectInjectedSkill_MissingSkillURI(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: ""}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestAddProjectInjectedSkill_WhitespaceSkillURIRejected verifies C1 (project scope):
// a POST with a whitespace-only skillUri returns 400.
func TestAddProjectInjectedSkill_WhitespaceSkillURIRejected(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	body := api.SkillInjectionEntry{SkillURI: "   "}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "whitespace-only skillUri must return 400")

	// Nothing should have been stored.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "no entry must be stored for a whitespace-only skillUri")
}

func TestAddProjectInjectedSkill_DuplicateReturnsConflict(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: "skill://scion/dup-skill@1.0"}
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
		SkillURI: "skill://scion/old-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	// Replace with two new entries.
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/new-skill-a@1.0"},
			{SkillURI: "skill://scion/new-skill-b@2.0", Optional: true},
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
	assert.Contains(t, uris, "skill://scion/new-skill-a@1.0")
	assert.Contains(t, uris, "skill://scion/new-skill-b@2.0")
}

// TestSetProjectInjectedSkills_SortOrderPreserved verifies M-2:
// explicit SortOrder values are preserved through a PUT bulk-replace round-trip.
func TestSetProjectInjectedSkills_SortOrderPreserved(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/skill-c@1.0", SortOrder: 30},
			{SkillURI: "skill://scion/skill-a@1.0", SortOrder: 10},
			{SkillURI: "skill://scion/skill-b@1.0", SortOrder: 20},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Fetch from store directly — entries are returned sorted by SortOrder ascending.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	require.Len(t, sis, 3)
	// Store returns them sorted by sort_order, so expect 10, 20, 30.
	assert.Equal(t, 10, sis[0].SortOrder)
	assert.Equal(t, "skill://scion/skill-a@1.0", sis[0].SkillURI)
	assert.Equal(t, 20, sis[1].SortOrder)
	assert.Equal(t, "skill://scion/skill-b@1.0", sis[1].SkillURI)
	assert.Equal(t, 30, sis[2].SortOrder)
	assert.Equal(t, "skill://scion/skill-c@1.0", sis[2].SkillURI)
}

// TestSetProjectInjectedSkills_MixedSortOrder verifies N-1:
// a mixed list (some entries with explicit SortOrder, some without) produces
// non-colliding sort orders after a PUT bulk-replace.
func TestSetProjectInjectedSkills_MixedSortOrder(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Entry 0: no explicit SortOrder → gets default i+1 = 1
	// Entry 1: explicit SortOrder = 1 (would collide with the default if we used i=0)
	// Entry 2: no explicit SortOrder → gets default i+1 = 3
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/skill-auto-0@1.0"},                    // default → 1
			{SkillURI: "skill://scion/skill-explicit-1@1.0", SortOrder: 10}, // explicit 10
			{SkillURI: "skill://scion/skill-auto-2@1.0"},                    // default → 3
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	require.Len(t, sis, 3)

	// Collect sort orders and verify they are all distinct.
	orders := make(map[int]string)
	for _, si := range sis {
		prev, collision := orders[si.SortOrder]
		assert.False(t, collision, "sort order %d collides between %q and %q", si.SortOrder, prev, si.SkillURI)
		orders[si.SortOrder] = si.SkillURI
	}
	assert.Len(t, orders, 3, "all three sort orders must be distinct")

	// The explicit entry must preserve its value.
	for _, si := range sis {
		if si.SkillURI == "skill://scion/skill-explicit-1@1.0" {
			assert.Equal(t, 10, si.SortOrder, "explicit SortOrder must be stored as-is")
		}
	}
}

// TestSetProjectInjectedSkills_ExplicitSortOrder1CollisionFree verifies C4 (project scope):
// when a caller sets sortOrder=1 on one entry and leaves another entry's sortOrder=0,
// the default-assigned value does not collide with the explicit 1.
func TestSetProjectInjectedSkills_ExplicitSortOrder1CollisionFree(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Entry 0: no explicit sortOrder (would naively get 1 via i+1 — the residual collision).
	// Entry 1: explicit sortOrder = 1.
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/proj-auto@1.0"},                     // default, must NOT get 1
			{SkillURI: "skill://scion/proj-explicit-1@1.0", SortOrder: 1}, // explicit 1
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	require.Len(t, sis, 2)

	// All sort orders must be distinct.
	orders := make(map[int]string)
	for _, si := range sis {
		prev, collision := orders[si.SortOrder]
		assert.False(t, collision, "sort order %d collides between %q and %q", si.SortOrder, prev, si.SkillURI)
		orders[si.SortOrder] = si.SkillURI
	}
	assert.Len(t, orders, 2, "both entries must have distinct sort orders")

	// The explicit entry must preserve its value.
	for _, si := range sis {
		if si.SkillURI == "skill://scion/proj-explicit-1@1.0" {
			assert.Equal(t, 1, si.SortOrder, "explicit sortOrder=1 must be preserved")
		}
	}
}

// TestSetProjectInjectedSkills_EmptySkillURIRejected verifies N-2 (project scope):
// a PUT with any entry missing skillUri returns 400 and nothing is stored.
func TestSetProjectInjectedSkills_EmptySkillURIRejected(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	badList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/valid-skill@1.0"},
			{SkillURI: ""},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", badList)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Nothing should have been stored.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "no entries must be stored when any skillUri is empty")
}

func TestSetProjectInjectedSkills_EmptyListClearsAll(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "skill://scion/to-be-cleared@1.0",
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

// TestSetProjectInjectedSkills_NormalizesSkillURI verifies that a bulk PUT with
// leading/trailing whitespace in skillUri stores the trimmed value.
func TestSetProjectInjectedSkills_NormalizesSkillURI(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "  skill://scion/proj-trimmed-skill@1.0  "},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The response should contain the trimmed URI.
	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "skill://scion/proj-trimmed-skill@1.0", resp.Entries[0].SkillURI,
		"response URI must be trimmed")

	// The stored value must also be trimmed.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	require.Len(t, sis, 1)
	assert.Equal(t, "skill://scion/proj-trimmed-skill@1.0", sis[0].SkillURI,
		"stored URI must not have surrounding whitespace")
}

// TestAddProjectInjectedSkill_NormalizesGitHubURL verifies that posting a full
// GitHub tree URL auto-transforms it to the canonical gh:// form.
func TestAddProjectInjectedSkill_NormalizesGitHubURL(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	body := api.SkillInjectionEntry{
		SkillURI: "https://github.com/org/repo/tree/main/skills/my-skill",
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	assert.Equal(t, "gh://org/repo/my-skill@main", entry.SkillURI,
		"GitHub tree URL should be transformed to canonical gh:// form")

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	require.Len(t, sis, 1)
	assert.Equal(t, "gh://org/repo/my-skill@main", sis[0].SkillURI,
		"stored URI must be the canonical gh:// form")
}

// TestAddProjectInjectedSkill_RejectsInvalidScheme verifies that unsupported
// schemes return a 400 with a specific error.
func TestAddProjectInjectedSkill_RejectsInvalidScheme(t *testing.T) {
	srv, _, project, alice, _ := setupInjectedSkillsTest(t)

	tests := []struct {
		name string
		uri  string
	}{
		{"scion scheme", "scion://my-skill"},
		{"ftp scheme", "ftp://example.com/skill"},
		{"bare repo github URL", "https://github.com/org/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := api.SkillInjectionEntry{SkillURI: tt.uri}
			rec := doRequestAsUser(t, srv, alice, http.MethodPost,
				"/api/v1/projects/"+project.ID+"/injected-skills", body)
			assert.Equal(t, http.StatusBadRequest, rec.Code,
				"invalid URI %q should return 400, body: %s", tt.uri, rec.Body.String())
		})
	}
}

// TestSetProjectInjectedSkills_AcceptsLegacySingleSegmentSkillURI verifies that
// a bulk PUT containing a legacy skill://<slug> entry (single segment after
// skill://) does not 400. ParseSkillURI's single-segment heuristic treats this
// as skill://scion/<slug>. Regression test for ptone/scion#582.
func TestSetProjectInjectedSkills_AcceptsLegacySingleSegmentSkillURI(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed a legacy entry in the store (mimics what the old web picker stored).
	legacy := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "skill://scion-process",
	}
	require.NoError(t, s.AddSkillInjection(ctx, legacy))

	// Bulk PUT the same list back (this is what save/reorder does).
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion-process"},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, "legacy skill://<slug> must not 400; body: %s", rec.Body.String())

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "skill://scion-process", resp.Entries[0].SkillURI,
		"legacy URI must be accepted and returned as-is")

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	require.Len(t, sis, 1)
	assert.Equal(t, "skill://scion-process", sis[0].SkillURI,
		"stored URI must match the legacy form")
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
		SkillURI: "skill://scion/removable-skill@1.0",
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

// TestRemoveProjectInjectedSkill_CrossProjectIDORRejected verifies C-2:
// a project-A admin cannot delete a project-B entry by supplying a cross-project UUID.
func TestRemoveProjectInjectedSkill_CrossProjectIDORRejected(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Create a second project also owned by alice.
	projectB := &store.Project{
		ID:        tid("si-project-b"),
		Name:      "Project B",
		Slug:      "project-b",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	srv.createProjectMembersGroup(ctx, projectB)

	// Add an entry to project B.
	siB := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectB.ID,
		SkillURI: "skill://scion/project-b-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, siB))

	// Alice tries to DELETE project-B's entry via project (project-A)'s URL.
	// This is the IDOR: the UUID exists in the DB but belongs to project-B, not project.
	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/injected-skills/"+siB.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-project DELETE must return 404")

	// Verify the entry in project B is untouched.
	entries, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectB.ID)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "project-B entry must not have been deleted")
}

// TestRemoveProjectInjectedSkill_TrailingSlash verifies C2:
// a DELETE request with a trailing slash on the project-scope endpoint is routed
// correctly (not 404'd by the router) and succeeds.
func TestRemoveProjectInjectedSkill_TrailingSlash(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	si := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  project.ID,
		SkillURI: "skill://scion/trailing-slash-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	// DELETE with trailing slash — must be routed to the handler, not 404'd.
	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/injected-skills/"+si.ID+"/", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"DELETE with trailing slash must succeed (not 404)")

	// Verify the entry was actually removed.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "entry must be removed even when DELETE URL has trailing slash")
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
	body := api.SkillInjectionEntry{SkillURI: "skill://scion/forbidden@1.0"}
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

// TestListProjectInjectedSkills_ForbiddenForAgentToken verifies M-1:
// non-UserIdentity callers (e.g. agent tokens) receive 403 Forbidden for the
// GET endpoint, just like write endpoints do.
func TestListProjectInjectedSkills_ForbiddenForAgentToken(t *testing.T) {
	srv, s, project, _, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Create an agent in the project so we can generate a valid agent token.
	agent := &store.Agent{
		ID:        tid("si-authz-agent"),
		Slug:      "authz-agent",
		Name:      "Authz Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	agentToken, err := tokenSvc.GenerateAgentToken(agent.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	// Agent tokens are a non-UserIdentity caller; the else-guard must reject them.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+project.ID+"/injected-skills", nil)
	req.Header.Set("X-Scion-Agent-Token", agentToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"agent token must receive 403 on GET project injected-skills")
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
		SkillURI: "skill://scion/alice-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/users/me/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Entries, 1)
	assert.Equal(t, "skill://scion/alice-skill@1.0", resp.Entries[0].SkillURI)
}

func TestListUserInjectedSkills_IsolatedBetweenUsers(t *testing.T) {
	srv, s, _, alice, bob := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Add a skill to bob's list.
	siBob := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  bob.ID,
		SkillURI: "skill://scion/bob-skill@1.0",
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

	body := api.SkillInjectionEntry{SkillURI: "skill://scion/my-user-skill@1.0", SkillAs: "alias"}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	assert.NotEmpty(t, entry.ID)
	assert.Equal(t, "skill://scion/my-user-skill@1.0", entry.SkillURI)
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

// TestAddUserInjectedSkill_WhitespaceSkillURIRejected verifies C1 (user scope):
// a POST with a whitespace-only skillUri returns 400.
func TestAddUserInjectedSkill_WhitespaceSkillURIRejected(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	body := api.SkillInjectionEntry{SkillURI: "\t  \t"}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "whitespace-only skillUri must return 400")

	// Nothing should have been stored.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "no entry must be stored for a whitespace-only skillUri")
}

func TestAddUserInjectedSkill_DuplicateReturnsConflict(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{SkillURI: "skill://scion/dup-user-skill@1.0"}
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
		SkillURI: "skill://scion/old-user-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/new-user-skill-a@1.0"},
			{SkillURI: "skill://scion/new-user-skill-b@2.0"},
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

// TestSetUserInjectedSkills_SortOrderPreserved verifies N-3:
// explicit SortOrder values are preserved through a PUT bulk-replace round-trip
// for the user scope.
func TestSetUserInjectedSkills_SortOrderPreserved(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/user-skill-c@1.0", SortOrder: 30},
			{SkillURI: "skill://scion/user-skill-a@1.0", SortOrder: 10},
			{SkillURI: "skill://scion/user-skill-b@1.0", SortOrder: 20},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Fetch from store directly — entries are returned sorted by SortOrder ascending.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	require.Len(t, sis, 3)
	// Store returns them sorted by sort_order, so expect 10, 20, 30.
	assert.Equal(t, 10, sis[0].SortOrder)
	assert.Equal(t, "skill://scion/user-skill-a@1.0", sis[0].SkillURI)
	assert.Equal(t, 20, sis[1].SortOrder)
	assert.Equal(t, "skill://scion/user-skill-b@1.0", sis[1].SkillURI)
	assert.Equal(t, 30, sis[2].SortOrder)
	assert.Equal(t, "skill://scion/user-skill-c@1.0", sis[2].SkillURI)
}

// TestSetUserInjectedSkills_MixedSortOrder verifies N-1 for user scope:
// a mixed list produces non-colliding sort orders.
func TestSetUserInjectedSkills_MixedSortOrder(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/user-auto-0@1.0"},                     // default → 1
			{SkillURI: "skill://scion/user-explicit-10@1.0", SortOrder: 10}, // explicit 10
			{SkillURI: "skill://scion/user-auto-2@1.0"},                     // default → 3
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	require.Len(t, sis, 3)

	// All sort orders must be distinct.
	orders := make(map[int]string)
	for _, si := range sis {
		prev, collision := orders[si.SortOrder]
		assert.False(t, collision, "sort order %d collides between %q and %q", si.SortOrder, prev, si.SkillURI)
		orders[si.SortOrder] = si.SkillURI
	}
	assert.Len(t, orders, 3)

	// Explicit entry must keep its value.
	for _, si := range sis {
		if si.SkillURI == "skill://scion/user-explicit-10@1.0" {
			assert.Equal(t, 10, si.SortOrder)
		}
	}
}

// TestSetUserInjectedSkills_ExplicitSortOrder1CollisionFree verifies C4 (user scope):
// when a caller sets sortOrder=1 on one entry and leaves another entry's sortOrder=0,
// the default-assigned value does not collide with the explicit 1.
func TestSetUserInjectedSkills_ExplicitSortOrder1CollisionFree(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Entry 0: no explicit sortOrder (would naively get 1 via i+1 — the residual collision).
	// Entry 1: explicit sortOrder = 1.
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/user-auto@1.0"},                     // default, must NOT get 1
			{SkillURI: "skill://scion/user-explicit-1@1.0", SortOrder: 1}, // explicit 1
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	require.Len(t, sis, 2)

	// All sort orders must be distinct.
	orders := make(map[int]string)
	for _, si := range sis {
		prev, collision := orders[si.SortOrder]
		assert.False(t, collision, "sort order %d collides between %q and %q", si.SortOrder, prev, si.SkillURI)
		orders[si.SortOrder] = si.SkillURI
	}
	assert.Len(t, orders, 2, "both entries must have distinct sort orders")

	// The explicit entry must preserve its value.
	for _, si := range sis {
		if si.SkillURI == "skill://scion/user-explicit-1@1.0" {
			assert.Equal(t, 1, si.SortOrder, "explicit sortOrder=1 must be preserved")
		}
	}
}

// TestSetUserInjectedSkills_EmptySkillURIRejected verifies N-2 (user scope):
// a PUT with any entry missing skillUri returns 400 and nothing is stored.
func TestSetUserInjectedSkills_EmptySkillURIRejected(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	badList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/valid-user-skill@1.0"},
			{SkillURI: ""},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", badList)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Nothing should have been stored.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "no entries must be stored when any skillUri is empty")
}

// TestSetUserInjectedSkills_NormalizesSkillURI verifies that a bulk PUT with
// leading/trailing whitespace in skillUri stores the trimmed value.
func TestSetUserInjectedSkills_NormalizesSkillURI(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "  skill://scion/user-trimmed-skill@1.0  "},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The response should contain the trimmed URI.
	var resp api.SkillInjectionList
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "skill://scion/user-trimmed-skill@1.0", resp.Entries[0].SkillURI,
		"response URI must be trimmed")

	// The stored value must also be trimmed.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	require.Len(t, sis, 1)
	assert.Equal(t, "skill://scion/user-trimmed-skill@1.0", sis[0].SkillURI,
		"stored URI must not have surrounding whitespace")
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
		SkillURI: "skill://scion/removable-user-skill@1.0",
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

// TestRemoveUserInjectedSkill_CrossUserIDORRejected verifies C-1:
// user A cannot delete user B's entry by guessing its UUID via /users/me/.
func TestRemoveUserInjectedSkill_CrossUserIDORRejected(t *testing.T) {
	srv, s, _, alice, bob := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Add an entry to Bob's user-scope list.
	siBob := &store.SkillInjection{
		Scope:    store.SkillInjectionScopeUser,
		ScopeID:  bob.ID,
		SkillURI: "skill://scion/bob-private-skill@1.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, siBob))

	// Alice tries to DELETE Bob's entry via her own /users/me/ path.
	// The entry UUID belongs to Bob, not Alice.
	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/users/me/injected-skills/"+siBob.ID, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "cross-user DELETE must return 404")

	// Verify Bob's entry is untouched.
	entries, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, bob.ID)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "Bob's entry must not have been deleted by Alice")
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

func TestGetHubInjectedSkills_DefaultState(t *testing.T) {
	// After hub startup, platform skills are automatically seeded into the
	// system list by seedPlatformSkillInsertions (Phase 6). The system list
	// is therefore non-empty, while user_defined starts empty.
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/hub/settings/injected-skills", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// System entries are seeded from the embedded platform skills on startup.
	assert.NotEmpty(t, resp.System, "system list must contain seeded platform skills")
	for _, ref := range resp.System {
		assert.True(t, ref.Optional)
		assert.True(t, strings.HasPrefix(ref.URI, platformSkillURIPrefix),
			"system entry URI %q must start with %q", ref.URI, platformSkillURIPrefix)
	}
	// No user-configured skills yet.
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
		System:      []api.SkillReference{{URI: "skill://scion/platform-skill@1.0"}},
		UserDefined: []api.SkillReference{{URI: "skill://scion/admin-skill@1.0"}},
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
	assert.Equal(t, "skill://scion/platform-skill@1.0", resp.System[0].URI)
	assert.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "skill://scion/admin-skill@1.0", resp.UserDefined[0].URI)
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
			{"uri": "skill://scion/hub-custom-skill@1.0"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "skill://scion/hub-custom-skill@1.0", resp.UserDefined[0].URI)

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
		System:      []api.SkillReference{{URI: "skill://scion/system-skill@1.0"}},
		UserDefined: []api.SkillReference{},
	}
	raw, err := json.Marshal(initial)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "system", -1, "seeded")
	require.NoError(t, err)

	// Admin updates user_defined only.
	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "skill://scion/admin-added-skill@1.0"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// System entry must still be present.
	assert.Len(t, resp.System, 1)
	assert.Equal(t, "skill://scion/system-skill@1.0", resp.System[0].URI)
	assert.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "skill://scion/admin-added-skill@1.0", resp.UserDefined[0].URI)
}

func TestSetHubInjectedSkills_ForbiddenForNonAdmin(t *testing.T) {
	srv, _, _, _, bob := setupInjectedSkillsTest(t)

	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "skill://scion/unauthorized-skill@1.0"},
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

// TestSetHubInjectedSkills_SystemBodyFieldIgnored verifies that a 'system' array
// included in the PUT request body does NOT overwrite the stored system entries.
// System entries are immutable via this endpoint and are always taken from the DB.
func TestSetHubInjectedSkills_SystemBodyFieldIgnored(t *testing.T) {
	srv, s, _, _, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Pre-seed a known system entry (simulating seeded platform skills).
	initial := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: "scion-platform://known-system-skill"}},
		UserDefined: []api.SkillReference{},
	}
	raw, err := json.Marshal(initial)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", raw, "seed", -1, "seeded")
	require.NoError(t, err)

	// PUT with a 'system' field in the body — the handler must ignore it.
	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "skill://admin/my-skill"},
		},
		"system": []map[string]interface{}{
			{"uri": "skill://attacker/injected-system-skill"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp api.HubSkillInjectionSetting
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	// System entries must be unchanged — the 'system' body field is ignored.
	require.Len(t, resp.System, 1, "system list must preserve the DB value, not the request body")
	assert.Equal(t, "scion-platform://known-system-skill", resp.System[0].URI,
		"system entries must come from the DB, not from the PUT body")

	// user_defined must have been updated as requested.
	require.Len(t, resp.UserDefined, 1)
	assert.Equal(t, "skill://admin/my-skill", resp.UserDefined[0].URI)
}

// dbBacked is a local interface satisfied by entadapter.CompositeStore,
// which exposes the underlying *sql.DB for raw-SQL access in tests.
type dbBacked interface {
	DB() *sql.DB
}

// TestSetHubInjectedSkills_CorruptBlobReturns500 verifies H-1:
// if the stored hub_settings blob is invalid JSON, PUT returns 500 and does NOT
// overwrite the stored value (no silent destruction of system skill entries).
func TestSetHubInjectedSkills_CorruptBlobReturns500(t *testing.T) {
	srv, s, _, _, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// First seed a valid row so the table row exists.
	initial := api.HubSkillInjectionSetting{
		System:      []api.SkillReference{{URI: "skill://scion/system-skill@1.0"}},
		UserDefined: []api.SkillReference{},
	}
	validBlob, err := json.Marshal(initial)
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "injected_skills", validBlob, "system", -1, "managed")
	require.NoError(t, err)

	// Corrupt the blob via raw SQL — Ent validates JSON on write, so we bypass it.
	db, ok := s.(dbBacked)
	require.True(t, ok, "test store must implement DB() *sql.DB")
	corruptBlob := "this is not valid json {{{"
	_, err = db.DB().ExecContext(ctx,
		`UPDATE hub_settings SET value = ? WHERE section = 'injected_skills'`, corruptBlob)
	require.NoError(t, err)

	// Admin (dev user) attempts a PUT — handler must detect the corrupt blob and return 500.
	body := map[string]interface{}{
		"user_defined": []map[string]interface{}{
			{"uri": "skill://scion/should-not-persist@1.0"},
		},
	}
	rec := doRequest(t, srv, http.MethodPut,
		"/api/v1/hub/settings/injected-skills", body)
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"PUT must return 500 when stored hub blob is corrupt")

	// Verify the stored value was not overwritten by the failed PUT.
	var storedValue string
	row := db.DB().QueryRowContext(ctx,
		`SELECT value FROM hub_settings WHERE section = 'injected_skills'`)
	require.NoError(t, row.Scan(&storedValue))
	assert.Equal(t, corruptBlob, storedValue,
		"corrupt stored blob must not be overwritten by the failed PUT")
}

// =============================================================================
// AllowProgeny validation tests
// =============================================================================

// TestAddProjectInjectedSkill_AllowProgenyRejected verifies that POSTing a
// project-scoped skill injection with AllowProgeny=true returns a validation error.
func TestAddProjectInjectedSkill_AllowProgenyRejected(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	body := api.SkillInjectionEntry{
		SkillURI:     "skill://scion/progeny-skill@1.0",
		AllowProgeny: true,
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/injected-skills", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"project-scoped POST with allowProgeny=true must return 400")

	// Nothing should have been stored.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "no entry must be stored when allowProgeny validation fails")
}

// TestSetProjectInjectedSkills_AllowProgenyRejected verifies that PUTting a
// project-scoped skill injection list with any AllowProgeny=true entry returns
// a validation error. This covers the N-3 fix.
func TestSetProjectInjectedSkills_AllowProgenyRejected(t *testing.T) {
	srv, s, project, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/valid-skill@1.0"},
			{SkillURI: "skill://scion/progeny-skill@1.0", AllowProgeny: true},
		},
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/projects/"+project.ID+"/injected-skills", newList)
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"project-scoped PUT with allowProgeny=true must return 400")

	// Nothing should have been stored.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, project.ID)
	require.NoError(t, err)
	assert.Empty(t, sis, "no entry must be stored when allowProgeny validation fails")
}

// =============================================================================
// Progeny policy lifecycle tests
// =============================================================================

// TestAddUserInjectedSkill_AllowProgenyCreatesPolicy verifies that adding a
// user-scoped skill injection with AllowProgeny=true creates an implicit
// progeny policy.
func TestAddUserInjectedSkill_AllowProgenyCreatesPolicy(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	body := api.SkillInjectionEntry{
		SkillURI:     "skill://scion/progeny-skill@1.0",
		AllowProgeny: true,
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	require.NotEmpty(t, entry.ID)

	// CO1: progeny access is handled by the RelationshipGrantResolver;
	// no legacy policy is created (PolicyStore removed).
}

// TestRemoveUserInjectedSkill_AllowProgenyDeletesPolicy verifies that removing
// a user-scoped skill injection with AllowProgeny=true works cleanly.
// CO1: progeny policies are no longer created (RelationshipGrantResolver
// handles progeny access), so this test verifies the delete flow itself.
func TestRemoveUserInjectedSkill_AllowProgenyDeletesPolicy(t *testing.T) {
	srv, _, _, alice, _ := setupInjectedSkillsTest(t)

	// Add a skill with AllowProgeny=true.
	body := api.SkillInjectionEntry{
		SkillURI:     "skill://scion/removable-progeny-skill@1.0",
		AllowProgeny: true,
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var entry api.SkillInjectionEntry
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&entry))
	entryID := entry.ID

	// Delete the skill injection.
	rec = doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/users/me/injected-skills/"+entryID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// TestSetUserInjectedSkills_BulkReplaceCleansPolicies verifies that a bulk
// PUT on user-scope injected skills replaces entries correctly.
// CO1: progeny policies are no longer created (RelationshipGrantResolver
// handles progeny access). This test verifies the bulk replace flow and
// confirms no orphaned policies are left behind.
func TestSetUserInjectedSkills_BulkReplaceCleansPolicies(t *testing.T) {
	srv, s, _, alice, _ := setupInjectedSkillsTest(t)
	ctx := context.Background()

	// Add a skill with AllowProgeny=true via POST.
	body := api.SkillInjectionEntry{
		SkillURI:     "skill://scion/old-progeny-skill@1.0",
		AllowProgeny: true,
	}
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/users/me/injected-skills", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Bulk replace with a new list (new skill with AllowProgeny=true).
	newList := api.SkillInjectionList{
		Entries: []api.SkillInjectionEntry{
			{SkillURI: "skill://scion/new-progeny-skill@2.0", AllowProgeny: true},
		},
	}
	rec = doRequestAsUser(t, srv, alice, http.MethodPut,
		"/api/v1/users/me/injected-skills", newList)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Verify the new entry replaced the old one.
	sis, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, alice.ID)
	require.NoError(t, err)
	require.Len(t, sis, 1, "bulk replace should result in exactly one entry")
	assert.Equal(t, "skill://scion/new-progeny-skill@2.0", sis[0].SkillURI)

	// CO1: progeny policies no longer exist (PolicyStore removed).
}
