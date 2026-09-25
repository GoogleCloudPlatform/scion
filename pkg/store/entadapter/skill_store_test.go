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
	"fmt"
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

// TestSkillStore_ListLimitClamped is part of the ptone/scion#1901 pagination
// follow-up (uat PG-pagination-default50): before this fix, ListSkills
// honored any caller-supplied limit outright and never emitted a cursor, so
// a large limit could return the entire table in one page. The store must
// clamp to maxSkillListLimit (200) and report a nextCursor when more rows
// remain. This must fail against ac8fc87a6, which has no upper bound and
// never sets NextCursor.
func TestSkillStore_ListLimitClamped(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	const total = 210
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("clamp-skill-%03d", i)
		require.NoError(t, cs.CreateSkill(ctx, &store.Skill{
			ID:         uuid.New().String(),
			Name:       name,
			Slug:       name,
			Scope:      store.SkillScopeGlobal,
			Status:     "active",
			Visibility: "private",
		}))
	}

	page, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active"}, store.ListOptions{Limit: 100000})
	require.NoError(t, err)
	assert.Equal(t, total, page.TotalCount)
	assert.LessOrEqual(t, len(page.Items), 200, "a caller-supplied limit must be clamped to the documented max of 200")
	assert.NotEmpty(t, page.NextCursor, "more than 200 matching rows exist, so a cursor must be returned")
}

// TestSkillStore_ListCursorWalkVisitsEveryRowOnce walks ListSkills with a
// small page size and confirms every matching row is visited exactly once
// with no duplicates and none left behind — the keyset cursor ListSkills
// previously ignored entirely (opts.Cursor was never read and NextCursor
// was never set, so ?cursor= was silently a no-op).
func TestSkillStore_ListCursorWalkVisitsEveryRowOnce(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	const total = 23
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("cursor-walk-skill-%03d", i)
		skill := &store.Skill{
			ID:         uuid.New().String(),
			Name:       name,
			Slug:       name,
			Scope:      store.SkillScopeGlobal,
			Status:     "active",
			Visibility: "private",
		}
		require.NoError(t, cs.CreateSkill(ctx, skill))
		want[skill.ID] = true
	}

	seen := make(map[string]bool, total)
	cursor := ""
	for pages := 0; ; pages++ {
		require.LessOrEqual(t, pages, total, "cursor walk did not terminate")

		page, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active"}, store.ListOptions{Limit: 5, Cursor: cursor})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), 5)

		for _, it := range page.Items {
			require.True(t, want[it.ID], "unexpected row %s in cursor walk", it.Name)
			assert.False(t, seen[it.ID], "duplicate row %s across pages", it.Name)
			seen[it.ID] = true
		}

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	assert.Len(t, seen, total, "cursor walk must visit every matching row exactly once")
}

// TestSkillStore_ListAccessScope_UserAndProjectCombineWithScopeFilter is the
// store-level regression for the read boundary pushed down in ListSkills:
// hub-scoped skills are visible to anyone with IncludeHubScope, a
// user-scoped skill only to its owning caller, a project-scoped skill only
// to a caller whose project is in ProjectIDs, and none of that widens when
// combined with the ordinary Scope/ScopeID query filters.
func TestSkillStore_ListAccessScope_UserAndProjectCombineWithScopeFilter(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	const callerID = "caller-user"
	const memberProjectID = "member-project"
	const otherProjectID = "other-project"

	mine := &store.Skill{ID: uuid.New().String(), Name: "mine-user-skill", Slug: "mine-user-skill",
		Scope: store.SkillScopeUser, ScopeID: callerID, Status: "active", Visibility: "private"}
	require.NoError(t, cs.CreateSkill(ctx, mine))

	othersUser := &store.Skill{ID: uuid.New().String(), Name: "others-user-skill", Slug: "others-user-skill",
		Scope: store.SkillScopeUser, ScopeID: "someone-else", Status: "active", Visibility: "private"}
	require.NoError(t, cs.CreateSkill(ctx, othersUser))

	myProject := &store.Skill{ID: uuid.New().String(), Name: "my-project-skill", Slug: "my-project-skill",
		Scope: store.SkillScopeProject, ScopeID: memberProjectID, Status: "active", Visibility: "private"}
	require.NoError(t, cs.CreateSkill(ctx, myProject))

	otherProject := &store.Skill{ID: uuid.New().String(), Name: "other-project-skill", Slug: "other-project-skill",
		Scope: store.SkillScopeProject, ScopeID: otherProjectID, Status: "active", Visibility: "private"}
	require.NoError(t, cs.CreateSkill(ctx, otherProject))

	global := &store.Skill{ID: uuid.New().String(), Name: "hub-catalog-skill", Slug: "hub-catalog-skill",
		Scope: store.SkillScopeGlobal, Status: "active", Visibility: "private"}
	require.NoError(t, cs.CreateSkill(ctx, global))

	scope := &store.SkillAccessScope{
		IncludeHubScope: true,
		CallerID:        callerID,
		ProjectIDs:      []string{memberProjectID},
	}

	all, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active", AccessScope: scope}, store.ListOptions{})
	require.NoError(t, err)
	gotIDs := make(map[string]bool, len(all.Items))
	for _, it := range all.Items {
		gotIDs[it.ID] = true
	}
	assert.True(t, gotIDs[mine.ID], "caller's own user-scoped skill must be visible")
	assert.True(t, gotIDs[myProject.ID], "caller's member-project skill must be visible")
	assert.True(t, gotIDs[global.ID], "hub-scoped skill must be visible")
	assert.False(t, gotIDs[othersUser.ID], "another user's user-scoped skill must not be visible")
	assert.False(t, gotIDs[otherProject.ID], "a non-member project's skill must not be visible")
	assert.Equal(t, 3, all.TotalCount)

	// Combining with an explicit ?scope=user filter (no scopeId) must AND
	// with the access scope, not widen it: only the caller's own skill.
	userOnly, err := cs.ListSkills(ctx, store.SkillFilter{Status: "active", Scope: store.SkillScopeUser, AccessScope: scope}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, userOnly.Items, 1)
	assert.Equal(t, mine.ID, userOnly.Items[0].ID)
}
