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

func TestDeleteProjectPreStartHook_Active(t *testing.T) {
	s := newTestPSHStore(t)
	ctx := context.Background()

	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		ProjectID: "proj-1",
		Name:      "Hook",
		Slug:      "hook",
		Script:    "#!/bin/sh\n",
	})
	require.NoError(t, err)
	assert.Equal(t, store.ProjectPreStartHookStatusActive, hook.Status)

	// Deleting an active hook should fail.
	err = s.DeleteProjectPreStartHook(ctx, hook.ID, "proj-1")
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
