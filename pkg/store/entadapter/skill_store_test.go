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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillStore_CreateAndGet(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skill := &store.Skill{
		ID:          uuid.New().String(),
		Name:        "test-skill",
		Slug:        "test-skill",
		Description: "A test skill",
		Tags:        []string{"test", "example"},
		Scope:       "global",
		Status:      "active",
		Visibility:  "private",
	}

	err := cs.CreateSkill(ctx, skill)
	require.NoError(t, err)
	assert.False(t, skill.Created.IsZero())

	got, err := cs.GetSkill(ctx, skill.ID)
	require.NoError(t, err)
	assert.Equal(t, skill.Name, got.Name)
	assert.Equal(t, skill.Slug, got.Slug)
	assert.Equal(t, skill.Description, got.Description)
	assert.Equal(t, []string{"test", "example"}, got.Tags)
	assert.Equal(t, "global", got.Scope)
	assert.Equal(t, "active", got.Status)
}

func TestSkillStore_GetBySlug(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skill := &store.Skill{
		ID:         uuid.New().String(),
		Name:       "my-skill",
		Slug:       "my-skill",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}
	require.NoError(t, cs.CreateSkill(ctx, skill))

	got, err := cs.GetSkillBySlug(ctx, "my-skill", "global", "")
	require.NoError(t, err)
	assert.Equal(t, skill.ID, got.ID)

	_, err = cs.GetSkillBySlug(ctx, "my-skill", "project", "")
	assert.Error(t, err)
}

func TestSkillStore_Update(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skill := &store.Skill{
		ID:         uuid.New().String(),
		Name:       "old-name",
		Slug:       "old-name",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}
	require.NoError(t, cs.CreateSkill(ctx, skill))

	skill.Name = "new-name"
	skill.Slug = "new-name"
	skill.Description = "Updated description"
	require.NoError(t, cs.UpdateSkill(ctx, skill))

	got, err := cs.GetSkill(ctx, skill.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-name", got.Name)
	assert.Equal(t, "Updated description", got.Description)
}

func TestSkillStore_DeleteSoftArchives(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skill := &store.Skill{
		ID:         uuid.New().String(),
		Name:       "to-delete",
		Slug:       "to-delete",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}
	require.NoError(t, cs.CreateSkill(ctx, skill))

	require.NoError(t, cs.DeleteSkill(ctx, skill.ID))

	got, err := cs.GetSkill(ctx, skill.ID)
	require.NoError(t, err)
	assert.Equal(t, "archived", got.Status)
}

func TestSkillStore_ListWithFilters(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	// Create skills in different scopes
	for _, s := range []struct {
		name  string
		scope string
	}{
		{"alpha-skill", "global"},
		{"beta-skill", "global"},
		{"gamma-skill", "project"},
	} {
		require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
			ID:         uuid.New().String(),
			Name:       s.name,
			Slug:       s.name,
			Scope:      s.scope,
			Status:     "active",
			Visibility: "private",
		}))
	}

	// List all
	result, err := cs.ListSkills(ctx, store.SkillFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// Filter by scope
	result, err = cs.ListSkills(ctx, store.SkillFilter{Scope: "global"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	// Filter by name
	result, err = cs.ListSkills(ctx, store.SkillFilter{Name: "alpha-skill"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, "alpha-skill", result.Items[0].Name)
}

func TestSkillStore_VersionCRUD(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "versioned-skill",
		Slug:       "versioned-skill",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	// Create version
	v1 := &store.SkillVersion{
		ID:      uuid.New().String(),
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusDraft,
	}
	require.NoError(t, cs.CreateSkillVersion(ctx, v1))

	// Get by ID
	got, err := cs.GetSkillVersion(ctx, v1.ID)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", got.Version)
	assert.Equal(t, store.SkillVersionStatusDraft, got.Status)

	// Get by version number
	got, err = cs.GetSkillVersionByNumber(ctx, skillID, "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, v1.ID, got.ID)

	// Update to published
	v1.Status = store.SkillVersionStatusPublished
	v1.ContentHash = "sha256:abc123"
	require.NoError(t, cs.UpdateSkillVersion(ctx, v1))

	got, err = cs.GetSkillVersion(ctx, v1.ID)
	require.NoError(t, err)
	assert.Equal(t, store.SkillVersionStatusPublished, got.Status)
	assert.Equal(t, "sha256:abc123", got.ContentHash)

	// List versions
	result, err := cs.ListSkillVersions(ctx, skillID, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
}

func TestSkillStore_VersionImmutability(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "immutable-test",
		Slug:       "immutable-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      uuid.New().String(),
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusPublished,
	}))

	// Duplicate version should fail (unique index)
	err := cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      uuid.New().String(),
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusDraft,
	})
	assert.Error(t, err)
}

func TestSkillStore_ResolveVersion_Latest(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "resolve-test",
		Slug:       "resolve-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	// Create v1.0.0 and v1.1.0 as published
	for _, v := range []string{"1.0.0", "1.1.0"} {
		require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
			ID:      uuid.New().String(),
			SkillID: skillID,
			Version: v,
			Status:  store.SkillVersionStatusPublished,
		}))
	}

	// Create v2.0.0-beta.1 as published (pre-release)
	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      uuid.New().String(),
		SkillID: skillID,
		Version: "2.0.0-beta.1",
		Status:  store.SkillVersionStatusPublished,
	}))

	// "latest" should resolve to 1.1.0 (highest non-prerelease)
	sv, err := cs.ResolveSkillVersion(ctx, skillID, "latest")
	require.NoError(t, err)
	assert.Equal(t, "1.1.0", sv.Version)

	// Empty string also resolves to latest
	sv, err = cs.ResolveSkillVersion(ctx, skillID, "")
	require.NoError(t, err)
	assert.Equal(t, "1.1.0", sv.Version)
}

func TestSkillStore_ResolveVersion_Exact(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "exact-test",
		Slug:       "exact-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      uuid.New().String(),
		SkillID: skillID,
		Version: "1.2.3",
		Status:  store.SkillVersionStatusPublished,
	}))

	sv, err := cs.ResolveSkillVersion(ctx, skillID, "1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", sv.Version)

	// Non-existent exact version
	_, err = cs.ResolveSkillVersion(ctx, skillID, "9.9.9")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSkillStore_ResolveVersion_Constraint(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "constraint-test",
		Slug:       "constraint-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "2.0.0"} {
		require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
			ID:      uuid.New().String(),
			SkillID: skillID,
			Version: v,
			Status:  store.SkillVersionStatusPublished,
		}))
	}

	// ^1.0 → highest 1.x.x
	sv, err := cs.ResolveSkillVersion(ctx, skillID, "^1.0")
	require.NoError(t, err)
	assert.Equal(t, "1.2.0", sv.Version)

	// ~1.0 → highest 1.0.x
	sv, err = cs.ResolveSkillVersion(ctx, skillID, "~1.0")
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", sv.Version)

	// >= 2.0.0 → 2.0.0
	sv, err = cs.ResolveSkillVersion(ctx, skillID, ">= 2.0.0")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", sv.Version)
}

func TestSkillStore_ResolveVersion_ContentHash(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "hash-test",
		Slug:       "hash-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:          uuid.New().String(),
		SkillID:     skillID,
		Version:     "1.0.0",
		Status:      store.SkillVersionStatusPublished,
		ContentHash: "sha256:deadbeef",
	}))

	sv, err := cs.ResolveSkillVersion(ctx, skillID, "sha256:deadbeef")
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", sv.Version)

	_, err = cs.ResolveSkillVersion(ctx, skillID, "sha256:notfound")
	assert.Error(t, err)
}

func TestSkillStore_ResolveVersion_ExcludesDrafts(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "draft-test",
		Slug:       "draft-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	// Only a draft version exists
	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      uuid.New().String(),
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusDraft,
	}))

	// Should not be resolvable via "latest"
	_, err := cs.ResolveSkillVersion(ctx, skillID, "latest")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSkillStore_UniqueSlugPerScope(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         uuid.New().String(),
		Name:       "unique-test",
		Slug:       "unique-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	// Duplicate slug in same scope should fail
	err := cs.CreateSkill(ctx, &store.Skill{
		ID:         uuid.New().String(),
		Name:       "unique-test",
		Slug:       "unique-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	})
	assert.Error(t, err)

	// Same slug in different scope should succeed
	err = cs.CreateSkill(ctx, &store.Skill{
		ID:         uuid.New().String(),
		Name:       "unique-test",
		Slug:       "unique-test",
		Scope:      "project",
		ScopeID:    "proj-1",
		Status:     "active",
		Visibility: "private",
	})
	assert.NoError(t, err)
}

func TestSkillStore_DeleteSkillVersion(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	skillID := uuid.New().String()
	require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
		ID:         skillID,
		Name:       "delete-version-test",
		Slug:       "delete-version-test",
		Scope:      "global",
		Status:     "active",
		Visibility: "private",
	}))

	// Create a draft version and delete it successfully.
	draftID := uuid.New().String()
	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      draftID,
		SkillID: skillID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusDraft,
	}))

	err := cs.DeleteSkillVersion(ctx, draftID)
	require.NoError(t, err)

	// Verify the version is gone.
	_, err = cs.GetSkillVersion(ctx, draftID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Create a published version and verify delete is rejected.
	pubID := uuid.New().String()
	require.NoError(t, cs.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:      pubID,
		SkillID: skillID,
		Version: "2.0.0",
		Status:  store.SkillVersionStatusPublished,
	}))

	err = cs.DeleteSkillVersion(ctx, pubID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "only draft versions can be deleted")

	// Verify the published version still exists.
	got, err := cs.GetSkillVersion(ctx, pubID)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", got.Version)
}

func TestSkillStore_DeleteSkillVersion_NotFound(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	err := cs.DeleteSkillVersion(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}
