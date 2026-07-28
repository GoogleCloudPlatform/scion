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

package entadapter

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPSHStore(t *testing.T) *ProjectPreStartHookStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewProjectPreStartHookStore(client)
}

// =============================================================================
// GetActive — not found
// =============================================================================

func TestGetActiveProjectPreStartHook_NotFound(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.GetActiveProjectPreStartHook(ctx, "proj-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Create + GetActive
// =============================================================================

func TestCreateProjectPreStartHook_Basic(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Install dev tools",
		Slug:      "install-dev-tools",
		Script:    "#!/bin/sh\napt-get install -y jq\n",
		CreatedBy: "user@example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, "proj-1", hook.ProjectID)
	assert.Equal(t, "install-dev-tools", hook.Slug)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, hook.Status)
	assert.NotEmpty(t, hook.ID)

	// GetActive should return it.
	active, err := s.GetActiveProjectPreStartHook(ctx, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, hook.ID, active.ID)
}

// =============================================================================
// Create second hook archives the previous active one
// =============================================================================

func TestCreateProjectPreStartHook_ArchivesPrevious(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	first, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "First hook",
		Slug:      "first-hook",
		Script:    "#!/bin/sh\necho first\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, first.Status)

	second, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Second hook",
		Slug:      "second-hook",
		Script:    "#!/bin/sh\necho second\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, second.Status)

	// First hook must now be archived.
	reloaded, err := s.GetProjectPreStartHook(ctx, first.ID, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloaded.Status)

	// Active hook for the project must be the second one.
	active, err := s.GetActiveProjectPreStartHook(ctx, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, second.ID, active.ID)
}

// =============================================================================
// Slug uniqueness within project
// =============================================================================

func TestCreateProjectPreStartHook_SlugUniqueWithinProject(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hook A",
		Slug:      "my-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hook B",
		Slug:      "my-hook", // duplicate slug in same project
		Script:    "#!/bin/sh\n",
	})
	require.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestCreateProjectPreStartHook_SlugReusableAcrossProjects(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hook A",
		Slug:      "my-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-2", // different project — slug is allowed
		Name:      "Hook B",
		Slug:      "my-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
}

// =============================================================================
// ListProjectPreStartHooks
// =============================================================================

func TestListProjectPreStartHooks(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	// Two hooks for proj-1, one for proj-2.
	for _, slug := range []string{"hook-a", "hook-b"} {
		_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
			ProjectID: "proj-1",
			Name:      slug,
			Slug:      slug,
			Script:    "#!/bin/sh\n",
		})
		require.NoError(t, err)
	}
	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-2",
		Name:      "other",
		Slug:      "other",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	hooks, err := s.ListProjectPreStartHooks(ctx, "proj-1")
	require.NoError(t, err)
	assert.Len(t, hooks, 2)
	for _, h := range hooks {
		assert.Equal(t, "proj-1", h.ProjectID)
	}
}

// =============================================================================
// UpdateProjectPreStartHook
// =============================================================================

func TestUpdateProjectPreStartHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	created, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Original name",
		Slug:      "my-hook",
		Script:    "#!/bin/sh\necho original\n",
	})
	require.NoError(t, err)

	updated, err := s.UpdateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        created.ID,
		ProjectID: "proj-1",
		Name:      "Updated name",
		Script:    "#!/bin/sh\necho updated\n",
		UpdatedBy: "editor@example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, "Updated name", updated.Name)
	assert.Equal(t, "#!/bin/sh\necho updated\n", updated.Script)
	assert.Equal(t, "editor@example.com", updated.UpdatedBy)
	// Status must not change.
	assert.Equal(t, store.ProjectPreStartHookStatusActive, updated.Status)
}

// =============================================================================
// ActivateProjectPreStartHook
// =============================================================================

func TestActivateProjectPreStartHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	first, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "First",
		Slug:      "first",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	second, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Second",
		Slug:      "second",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
	// After creating second, first is archived.
	assert.Equal(t, store.ProjectPreStartHookStatusActive, second.Status)

	// Re-activate first; second should become archived.
	activated, err := s.ActivateProjectPreStartHook(ctx, first.ID, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, activated.Status)
	assert.Equal(t, first.ID, activated.ID)

	reloadedSecond, err := s.GetProjectPreStartHook(ctx, second.ID, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloadedSecond.Status)
}

func TestActivateProjectPreStartHook_WrongProject(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hook",
		Slug:      "hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.ActivateProjectPreStartHook(ctx, hook.ID, "proj-2")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// DeleteProjectPreStartHook
// =============================================================================

func TestDeleteProjectPreStartHook_OnlyActive_Succeeds(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	// When this is the last (only) hook in the project, deleting the active
	// hook is allowed so operators can fully clear all pre-start hooks.
	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hook",
		Slug:      "hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, hook.Status)

	err = s.DeleteProjectPreStartHook(ctx, hook.ID, "proj-1")
	require.NoError(t, err, "deleting the only active hook should succeed")

	_, err = s.GetProjectPreStartHook(ctx, hook.ID, "proj-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteProjectPreStartHook_Active_WithOtherHooks_Rejected(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	// Create two hooks: first is archived (second is active).
	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "First",
		Slug:      "first",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	second, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Second",
		Slug:      "second",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, second.Status)

	// Deleting the active hook when another hook exists should fail.
	err = s.DeleteProjectPreStartHook(ctx, second.ID, "proj-1")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestDeleteProjectPreStartHook_Archived(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	// Create two hooks — first becomes archived when second is created.
	first, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "First",
		Slug:      "first",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Second",
		Slug:      "second",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	// Now first is archived — deletion should succeed.
	err = s.DeleteProjectPreStartHook(ctx, first.ID, "proj-1")
	require.NoError(t, err)

	_, err = s.GetProjectPreStartHook(ctx, first.ID, "proj-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteProjectPreStartHook_WrongProject(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	// Create two so first is archived.
	first, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "First",
		Slug:      "first",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Second",
		Slug:      "second",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	// Try to delete first from a different project — must fail.
	err = s.DeleteProjectPreStartHook(ctx, first.ID, "proj-2")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Hub scope — GetActive not found
// =============================================================================

func TestGetActiveHubPreStartHook_NotFound(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.GetActiveHubPreStartHook(ctx)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Hub scope — Create + GetActive + Get
// =============================================================================

func TestCreateHubPreStartHook_Basic(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:        "Baseline setup",
		Slug:        "baseline-setup",
		Description: "hub-wide default",
		Script:      "#!/bin/sh\necho baseline\n",
		CreatedBy:   "admin@example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, store.PreStartHookScopeHub, hook.Scope)
	assert.Empty(t, hook.ProjectID, "hub hooks must not carry a project ID")
	assert.Equal(t, "baseline-setup", hook.Slug)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, hook.Status)
	assert.NotEmpty(t, hook.ID)

	active, err := s.GetActiveHubPreStartHook(ctx)
	require.NoError(t, err)
	assert.Equal(t, hook.ID, active.ID)
	assert.Equal(t, store.PreStartHookScopeHub, active.Scope)

	byID, err := s.GetHubPreStartHook(ctx, hook.ID)
	require.NoError(t, err)
	assert.Equal(t, hook.ID, byID.ID)
	assert.Equal(t, "hub-wide default", byID.Description)
}

// CreateHubPreStartHook must ignore any project ID supplied by the caller so a
// hub hook can never be silently bound to a project.
func TestCreateHubPreStartHook_IgnoresProjectID(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hub hook",
		Slug:      "hub-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Empty(t, hook.ProjectID)
	assert.Equal(t, store.PreStartHookScopeHub, hook.Scope)

	// It must not be visible from the project-scoped API.
	_, err = s.GetProjectPreStartHook(ctx, hook.ID, "proj-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A project-scoped create with no project ID is rejected — the schema no
// longer enforces NotEmpty on project_id (hub hooks need it empty), so the
// store layer preserves the invariant.
func TestCreateProjectPreStartHook_RequiresProjectID(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "No project",
		Slug:   "no-project",
		Script: "#!/bin/sh\n",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// =============================================================================
// Hub scope — create archives the previous active hub hook
// =============================================================================

func TestCreateHubPreStartHook_ArchivesPrevious(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	first, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "First",
		Slug:   "first",
		Script: "#!/bin/sh\necho first\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, first.Status)

	second, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Second",
		Slug:   "second",
		Script: "#!/bin/sh\necho second\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, second.Status)

	reloaded, err := s.GetHubPreStartHook(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloaded.Status)

	active, err := s.GetActiveHubPreStartHook(ctx)
	require.NoError(t, err)
	assert.Equal(t, second.ID, active.ID)
}

// =============================================================================
// Hub scope — slug uniqueness
// =============================================================================

func TestCreateHubPreStartHook_SlugUniqueWithinHub(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hook A",
		Slug:   "my-hook",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hook B",
		Slug:   "my-hook",
		Script: "#!/bin/sh\n",
	})
	require.ErrorIs(t, err, store.ErrAlreadyExists)
}

// The same slug may exist at hub scope and project scope simultaneously.
func TestCreateHubPreStartHook_SlugReusableAcrossScopes(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook",
		Slug:      "shared-slug",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hub hook",
		Slug:   "shared-slug",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)
}

// =============================================================================
// Hub scope — List
// =============================================================================

func TestListHubPreStartHooks(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	for _, slug := range []string{"hub-a", "hub-b"} {
		_, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
			Name:   slug,
			Slug:   slug,
			Script: "#!/bin/sh\n",
		})
		require.NoError(t, err)
	}
	_, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "project hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	hooks, err := s.ListHubPreStartHooks(ctx)
	require.NoError(t, err)
	assert.Len(t, hooks, 2)
	for _, h := range hooks {
		assert.Equal(t, store.PreStartHookScopeHub, h.Scope)
		assert.Empty(t, h.ProjectID)
	}
}

// =============================================================================
// Hub scope — Update
// =============================================================================

func TestUpdateHubPreStartHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	created, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Original name",
		Slug:   "my-hook",
		Script: "#!/bin/sh\necho original\n",
	})
	require.NoError(t, err)

	updated, err := s.UpdateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:        created.ID,
		Name:      "Updated name",
		Script:    "#!/bin/sh\necho updated\n",
		UpdatedBy: "admin@example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, "Updated name", updated.Name)
	assert.Equal(t, "#!/bin/sh\necho updated\n", updated.Script)
	assert.Equal(t, "admin@example.com", updated.UpdatedBy)
	assert.Equal(t, store.PreStartHookScopeHub, updated.Scope)
	// Status must not change.
	assert.Equal(t, store.ProjectPreStartHookStatusActive, updated.Status)
}

// A project-scoped hook cannot be mutated through the hub-scoped API.
func TestUpdateHubPreStartHook_RejectsProjectScopedHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	created, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.UpdateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		ID:   created.ID,
		Name: "Hijacked",
	})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Hub scope — Activate
// =============================================================================

func TestActivateHubPreStartHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	first, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "First",
		Slug:   "first",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	second, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Second",
		Slug:   "second",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, second.Status)

	// Re-activate first; second must become archived.
	activated, err := s.ActivateHubPreStartHook(ctx, first.ID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, activated.ID)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, activated.Status)

	reloadedSecond, err := s.GetHubPreStartHook(ctx, second.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusArchived, reloadedSecond.Status)

	active, err := s.GetActiveHubPreStartHook(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.ID, active.ID)
}

// Activating a project-scoped hook through the hub API must fail, and must not
// archive the active hub hook as a side effect.
func TestActivateHubPreStartHook_RejectsProjectScopedHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	projectHook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	hubHook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hub hook",
		Slug:   "hub-hook",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	_, err = s.ActivateHubPreStartHook(ctx, projectHook.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The transaction rolled back: the hub hook is still active.
	active, err := s.GetActiveHubPreStartHook(ctx)
	require.NoError(t, err)
	assert.Equal(t, hubHook.ID, active.ID)

	// And the project hook is untouched.
	reloaded, err := s.GetProjectPreStartHook(ctx, projectHook.ID, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, reloaded.Status)
}

// =============================================================================
// Hub scope — Delete
// =============================================================================

func TestDeleteHubPreStartHook_Active_WithOtherHooks_Rejected(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	_, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "First",
		Slug:   "first",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	second, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Second",
		Slug:   "second",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, second.Status)

	err = s.DeleteHubPreStartHook(ctx, second.ID)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestDeleteHubPreStartHook_OnlyActive_Succeeds(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	// Mirrors the project-scope semantic: deleting the last remaining hook is
	// allowed so operators can fully clear the hub hook.
	hook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Only",
		Slug:   "only",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteHubPreStartHook(ctx, hook.ID))

	_, err = s.GetHubPreStartHook(ctx, hook.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteHubPreStartHook_Archived(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	first, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "First",
		Slug:   "first",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)
	_, err = s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Second",
		Slug:   "second",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	// first is now archived — deletion should succeed.
	require.NoError(t, s.DeleteHubPreStartHook(ctx, first.ID))

	_, err = s.GetHubPreStartHook(ctx, first.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A project-scoped hook must not be deletable through the hub API.
func TestDeleteHubPreStartHook_RejectsProjectScopedHook(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	err = s.DeleteHubPreStartHook(ctx, hook.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Still there.
	_, err = s.GetProjectPreStartHook(ctx, hook.ID, "proj-1")
	require.NoError(t, err)
}

// =============================================================================
// Scope isolation — hub and project rows never interfere
// =============================================================================

func TestPreStartHookScopeIsolation(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	projectHook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\necho project\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.PreStartHookScopeProject, projectHook.Scope)

	hubHook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hub hook",
		Slug:   "hub-hook",
		Script: "#!/bin/sh\necho hub\n",
	})
	require.NoError(t, err)

	// Creating a hub hook must NOT archive the project hook, and vice versa —
	// both scopes hold an active hook simultaneously.
	activeProject, err := s.GetActiveProjectPreStartHook(ctx, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, projectHook.ID, activeProject.ID)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, activeProject.Status)

	activeHub, err := s.GetActiveHubPreStartHook(ctx)
	require.NoError(t, err)
	assert.Equal(t, hubHook.ID, activeHub.ID)

	// Listing project hooks must not surface the hub hook.
	projectHooks, err := s.ListProjectPreStartHooks(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, projectHooks, 1)
	assert.Equal(t, projectHook.ID, projectHooks[0].ID)

	// Listing hub hooks must not surface the project hook.
	hubHooks, err := s.ListHubPreStartHooks(ctx)
	require.NoError(t, err)
	require.Len(t, hubHooks, 1)
	assert.Equal(t, hubHook.ID, hubHooks[0].ID)

	// Cross-scope gets by ID must fail.
	_, err = s.GetHubPreStartHook(ctx, projectHook.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetProjectPreStartHook(ctx, hubHook.ID, "proj-1")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// A hub hook must never be reachable via the empty project ID either.
	_, err = s.GetActiveProjectPreStartHook(ctx, "")
	assert.ErrorIs(t, err, store.ErrNotFound)
	emptyProjectHooks, err := s.ListProjectPreStartHooks(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, emptyProjectHooks)
}

// Archiving/activating within one scope must leave the other scope alone.
func TestPreStartHookScopeIsolation_ActivateDoesNotCrossScopes(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hubFirst, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hub first",
		Slug:   "hub-first",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)
	_, err = s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Name:   "Hub second",
		Slug:   "hub-second",
		Script: "#!/bin/sh\n",
	})
	require.NoError(t, err)

	projectHook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook",
		Slug:      "project-hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	// Activating an older hub hook must not touch the project hook.
	_, err = s.ActivateHubPreStartHook(ctx, hubFirst.ID)
	require.NoError(t, err)

	stillActive, err := s.GetProjectPreStartHook(ctx, projectHook.ID, "proj-1")
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, stillActive.Status)

	// And creating another project hook must not touch the active hub hook.
	_, err = s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Project hook 2",
		Slug:      "project-hook-2",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)

	activeHub, err := s.GetActiveHubPreStartHook(ctx)
	require.NoError(t, err)
	assert.Equal(t, hubFirst.ID, activeHub.ID)
}
