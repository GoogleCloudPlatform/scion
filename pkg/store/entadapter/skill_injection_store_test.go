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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSkillInjectionStore returns a SkillInjectionStore backed by an
// in-memory SQLite database, using the test ent client.
func newTestSkillInjectionStore(t *testing.T) *SkillInjectionStore {
	t.Helper()
	cs := newTestCompositeStore(t)
	return cs.SkillInjectionStore
}

// TestSkillInjection_AddListRemove verifies basic CRUD operations.
func TestSkillInjection_AddListRemove(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()

	si := &store.SkillInjection{
		ID:        uuid.NewString(),
		Scope:     store.SkillInjectionScopeProject,
		ScopeID:   projectID,
		SkillURI:  "skill://my-org/my-skill@1.0.0",
		SkillAs:   "myskill",
		Optional:  false,
		SortOrder: 0,
		CreatedBy: "user-abc",
	}

	// Add.
	require.NoError(t, s.AddSkillInjection(ctx, si))
	assert.False(t, si.CreatedAt.IsZero(), "CreatedAt should be set after Add")

	// List.
	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	got := list[0]
	assert.Equal(t, si.ID, got.ID)
	assert.Equal(t, si.Scope, got.Scope)
	assert.Equal(t, si.ScopeID, got.ScopeID)
	assert.Equal(t, si.SkillURI, got.SkillURI)
	assert.Equal(t, si.SkillAs, got.SkillAs)
	assert.Equal(t, si.Optional, got.Optional)
	assert.Equal(t, si.SortOrder, got.SortOrder)
	assert.Equal(t, si.CreatedBy, got.CreatedBy)
	assert.False(t, got.CreatedAt.IsZero())

	// Remove.
	require.NoError(t, s.RemoveSkillInjection(ctx, si.ID))

	list, err = s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestSkillInjection_RemoveNotFound verifies that removing a non-existent entry
// returns ErrNotFound.
func TestSkillInjection_RemoveNotFound(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	err := s.RemoveSkillInjection(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestSkillInjection_Update verifies that mutable fields can be changed.
func TestSkillInjection_Update(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	si := &store.SkillInjection{
		ID:        uuid.NewString(),
		Scope:     store.SkillInjectionScopeProject,
		ScopeID:   projectID,
		SkillURI:  "skill://org/skill@1.0.0",
		SkillAs:   "original-alias",
		Optional:  false,
		SortOrder: 0,
		CreatedBy: "user-abc",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	// Update mutable fields.
	si.SkillURI = "skill://org/skill@2.0.0"
	si.SkillAs = "new-alias"
	si.Optional = true
	si.SortOrder = 5
	require.NoError(t, s.UpdateSkillInjection(ctx, si))

	// Verify changes.
	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	got := list[0]
	assert.Equal(t, "skill://org/skill@2.0.0", got.SkillURI)
	assert.Equal(t, "new-alias", got.SkillAs)
	assert.True(t, got.Optional)
	assert.Equal(t, 5, got.SortOrder)
}

// TestSkillInjection_UpdateClearsAlias verifies that setting SkillAs to empty
// clears the alias field.
func TestSkillInjection_UpdateClearsAlias(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	si := &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: "skill://org/skill@1.0.0",
		SkillAs:  "some-alias",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	si.SkillAs = ""
	require.NoError(t, s.UpdateSkillInjection(ctx, si))

	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Empty(t, list[0].SkillAs)
}

// TestSkillInjection_ListOrderedBySortOrder verifies that list results are
// returned sorted by sort_order ascending.
func TestSkillInjection_ListOrderedBySortOrder(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()

	// Insert in reverse order to ensure ordering is by sort_order, not insert order.
	for i := 2; i >= 0; i-- {
		si := &store.SkillInjection{
			ID:        uuid.NewString(),
			Scope:     store.SkillInjectionScopeProject,
			ScopeID:   projectID,
			SkillURI:  "skill://org/skill-" + string(rune('a'+i)),
			SortOrder: i,
		}
		require.NoError(t, s.AddSkillInjection(ctx, si))
	}

	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, list, 3)

	// Should come back in sort_order order: 0, 1, 2.
	for i, item := range list {
		assert.Equal(t, i, item.SortOrder)
	}
}

// TestSkillInjection_ScopeIsolation verifies that injections from different
// scope+scopeIDs are independent.
func TestSkillInjection_ScopeIsolation(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	proj1 := uuid.NewString()
	proj2 := uuid.NewString()
	userID := uuid.NewString()

	for _, tc := range []struct {
		scope, scopeID string
	}{
		{store.SkillInjectionScopeProject, proj1},
		{store.SkillInjectionScopeProject, proj2},
		{store.SkillInjectionScopeUser, userID},
	} {
		si := &store.SkillInjection{
			ID:       uuid.NewString(),
			Scope:    tc.scope,
			ScopeID:  tc.scopeID,
			SkillURI: "skill://org/skill@1.0.0",
		}
		require.NoError(t, s.AddSkillInjection(ctx, si))
	}

	list1, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, proj1)
	require.NoError(t, err)
	assert.Len(t, list1, 1)

	list2, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, proj2)
	require.NoError(t, err)
	assert.Len(t, list2, 1)

	listUser, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userID)
	require.NoError(t, err)
	assert.Len(t, listUser, 1)
}

// TestSkillInjection_SetSkillInjectionsAtomicReplace verifies that
// SetSkillInjections replaces the full list atomically.
func TestSkillInjection_SetSkillInjectionsAtomicReplace(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()

	// Seed an initial entry.
	si := &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: "skill://org/old-skill@1.0.0",
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	// Replace with two new entries (explicit SortOrder so we can verify round-trip).
	entries := []store.SkillInjection{
		{Scope: store.SkillInjectionScopeProject, ScopeID: projectID, SkillURI: "skill://org/new-skill-a@2.0.0", SkillAs: "a", SortOrder: 0},
		{Scope: store.SkillInjectionScopeProject, ScopeID: projectID, SkillURI: "skill://org/new-skill-b@3.0.0", Optional: true, SortOrder: 1},
	}
	require.NoError(t, s.SetSkillInjections(ctx, store.SkillInjectionScopeProject, projectID, entries, "user-xyz"))

	// The old entry should be gone; only the two new entries should exist.
	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Verify ordering and content (returned sorted by SortOrder asc).
	assert.Equal(t, 0, list[0].SortOrder)
	assert.Equal(t, "skill://org/new-skill-a@2.0.0", list[0].SkillURI)
	assert.Equal(t, "a", list[0].SkillAs)
	assert.False(t, list[0].Optional)
	assert.Equal(t, "user-xyz", list[0].CreatedBy)

	assert.Equal(t, 1, list[1].SortOrder)
	assert.Equal(t, "skill://org/new-skill-b@3.0.0", list[1].SkillURI)
	assert.Empty(t, list[1].SkillAs)
	assert.True(t, list[1].Optional)
}

// TestSkillInjection_SetSkillInjectionsEmpty verifies that setting an empty
// list removes all entries.
func TestSkillInjection_SetSkillInjectionsEmpty(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()

	// Seed a few entries with distinct URIs (unique constraint enforces this).
	for i := 0; i < 3; i++ {
		si := &store.SkillInjection{
			ID:        uuid.NewString(),
			Scope:     store.SkillInjectionScopeProject,
			ScopeID:   projectID,
			SkillURI:  fmt.Sprintf("skill://org/skill-%d@1.0.0", i),
			SortOrder: i,
		}
		require.NoError(t, s.AddSkillInjection(ctx, si))
	}

	// Set to empty list.
	require.NoError(t, s.SetSkillInjections(ctx, store.SkillInjectionScopeProject, projectID, nil, "user-abc"))

	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestSkillInjection_SetSkillInjections_DoesNotAffectOtherScopes verifies that
// SetSkillInjections only replaces entries for the target scope+scopeID.
func TestSkillInjection_SetSkillInjections_DoesNotAffectOtherScopes(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	proj1 := uuid.NewString()
	proj2 := uuid.NewString()

	// Seed proj1 and proj2.
	for _, pid := range []string{proj1, proj2} {
		si := &store.SkillInjection{
			ID:       uuid.NewString(),
			Scope:    store.SkillInjectionScopeProject,
			ScopeID:  pid,
			SkillURI: "skill://org/original@1.0.0",
		}
		require.NoError(t, s.AddSkillInjection(ctx, si))
	}

	// Replace only proj1.
	require.NoError(t, s.SetSkillInjections(ctx, store.SkillInjectionScopeProject, proj1,
		[]store.SkillInjection{{Scope: store.SkillInjectionScopeProject, ScopeID: proj1, SkillURI: "skill://org/replaced@2.0.0"}}, ""))

	// proj2 should be unchanged.
	list2, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, proj2)
	require.NoError(t, err)
	require.Len(t, list2, 1)
	assert.Equal(t, "skill://org/original@1.0.0", list2[0].SkillURI)

	// proj1 should have the new entry.
	list1, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, proj1)
	require.NoError(t, err)
	require.Len(t, list1, 1)
	assert.Equal(t, "skill://org/replaced@2.0.0", list1[0].SkillURI)
}

// TestSkillInjection_ToSkillReference verifies the ToSkillReference conversion.
func TestSkillInjection_ToSkillReference(t *testing.T) {
	tests := []struct {
		name string
		si   store.SkillInjection
		want api.SkillReference
	}{
		{
			name: "full fields",
			si: store.SkillInjection{
				SkillURI: "skill://org/skill@1.0.0",
				SkillAs:  "myalias",
				Optional: true,
			},
			want: api.SkillReference{
				URI:      "skill://org/skill@1.0.0",
				As:       "myalias",
				Optional: true,
			},
		},
		{
			name: "no alias not optional",
			si: store.SkillInjection{
				SkillURI: "skill://org/skill@2.0.0",
			},
			want: api.SkillReference{
				URI:      "skill://org/skill@2.0.0",
				As:       "",
				Optional: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.si.ToSkillReference()
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSkillInjection_DeleteSkillInjectionsByScope verifies that
// DeleteSkillInjectionsByScope removes all entries for the given scope+scopeID
// and leaves other scopes unaffected.
func TestSkillInjection_DeleteSkillInjectionsByScope(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	proj1 := uuid.NewString()
	proj2 := uuid.NewString()

	// Seed two entries for proj1 and one for proj2.
	for _, uri := range []string{"skill://org/a@1.0.0", "skill://org/b@1.0.0"} {
		require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
			ID:       uuid.NewString(),
			Scope:    store.SkillInjectionScopeProject,
			ScopeID:  proj1,
			SkillURI: uri,
		}))
	}
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  proj2,
		SkillURI: "skill://org/c@1.0.0",
	}))

	// Delete proj1's entries; expect 2 rows removed.
	n, err := s.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeProject, proj1)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "expected 2 rows deleted for proj1")

	// proj1 should now be empty.
	list1, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, proj1)
	require.NoError(t, err)
	assert.Empty(t, list1)

	// proj2 should be unaffected.
	list2, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, proj2)
	require.NoError(t, err)
	assert.Len(t, list2, 1)

	// Deleting an already-empty scope should succeed without error and return 0.
	n, err = s.DeleteSkillInjectionsByScope(ctx, store.SkillInjectionScopeProject, proj1)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "expected 0 rows deleted for already-empty scope")
}

// TestSkillInjection_AddDuplicateURIReturnsAlreadyExists verifies that adding a
// second injection with the same (scope, scope_id, skill_uri) returns ErrAlreadyExists.
func TestSkillInjection_AddDuplicateURIReturnsAlreadyExists(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	uri := "skill://org/my-skill@1.0.0"

	first := &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: uri,
	}
	require.NoError(t, s.AddSkillInjection(ctx, first))

	// A second entry with the same scope+scopeID+skillURI should be rejected.
	second := &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: uri,
	}
	err := s.AddSkillInjection(ctx, second)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

// TestSkillInjection_UpdateNotFound verifies that updating a non-existent entry
// returns ErrNotFound.
func TestSkillInjection_UpdateNotFound(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	err := s.UpdateSkillInjection(ctx, &store.SkillInjection{
		ID:       uuid.NewString(),
		SkillURI: "skill://org/some@1.0.0",
	})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestSkillInjection_AddAutoGeneratesID verifies that AddSkillInjection
// auto-generates a UUID when si.ID is empty and writes it back on success.
func TestSkillInjection_AddAutoGeneratesID(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()

	si := &store.SkillInjection{
		// ID intentionally left empty — store should auto-generate.
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: "skill://org/auto-id@1.0.0",
	}

	require.NoError(t, s.AddSkillInjection(ctx, si))
	assert.NotEmpty(t, si.ID, "si.ID should be populated after Add")
	assert.False(t, si.CreatedAt.IsZero(), "si.CreatedAt should be set after Add")

	// The auto-generated ID should be a valid UUID.
	_, err := uuid.Parse(si.ID)
	assert.NoError(t, err, "si.ID should be a valid UUID")

	// The entry should be retrievable by the generated ID.
	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeProject, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, si.ID, list[0].ID)
}

// TestSkillInjection_AddDoesNotMutateOnFailure verifies that AddSkillInjection
// does not mutate the caller's CreatedAt before Save succeeds (regression guard
// for LOW-6).
func TestSkillInjection_AddDoesNotMutateOnFailure(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	projectID := uuid.NewString()
	uri := "skill://org/my-skill@1.0.0"

	// Seed an initial entry so the second Add will fail with ErrAlreadyExists.
	first := &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: uri,
	}
	require.NoError(t, s.AddSkillInjection(ctx, first))

	// Attempt to add a duplicate; this should fail.
	second := &store.SkillInjection{
		ID:       uuid.NewString(),
		Scope:    store.SkillInjectionScopeProject,
		ScopeID:  projectID,
		SkillURI: uri,
	}
	zeroTime := second.CreatedAt // should remain zero after failure
	err := s.AddSkillInjection(ctx, second)
	require.ErrorIs(t, err, store.ErrAlreadyExists)
	assert.Equal(t, zeroTime, second.CreatedAt, "CreatedAt must not be mutated when Save fails")
}

// =============================================================================
// Progeny skill injection tests
// =============================================================================

// TestListProgenySkillInjections verifies the transitive progeny-inheritance
// query: only user-scoped, allow_progeny=true skill injections whose created_by
// is within the ancestor set are returned. Mirrors TestListProgenySecretsInheritance.
func TestListProgenySkillInjections(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	ancestor1 := uuid.NewString()
	ancestor2 := uuid.NewString()
	stranger := uuid.NewString()

	// Eligible: user-scoped, allow_progeny=true, created by an ancestor.
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID: uuid.NewString(), Scope: store.SkillInjectionScopeUser, ScopeID: ancestor1,
		SkillURI: "skill://org/inherit-1@1.0", AllowProgeny: true, CreatedBy: ancestor1,
	}))
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID: uuid.NewString(), Scope: store.SkillInjectionScopeUser, ScopeID: ancestor2,
		SkillURI: "skill://org/inherit-2@1.0", AllowProgeny: true, CreatedBy: ancestor2,
	}))

	// Ineligible: allow_progeny=false.
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID: uuid.NewString(), Scope: store.SkillInjectionScopeUser, ScopeID: ancestor1,
		SkillURI: "skill://org/no-progeny@1.0", AllowProgeny: false, CreatedBy: ancestor1,
	}))
	// Ineligible: created by a non-ancestor.
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID: uuid.NewString(), Scope: store.SkillInjectionScopeUser, ScopeID: stranger,
		SkillURI: "skill://org/stranger@1.0", AllowProgeny: true, CreatedBy: stranger,
	}))
	// Ineligible: wrong scope (project), even though allow_progeny + ancestor creator.
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID: uuid.NewString(), Scope: store.SkillInjectionScopeProject, ScopeID: uuid.NewString(),
		SkillURI: "skill://org/project-scoped@1.0", AllowProgeny: true, CreatedBy: ancestor1,
	}))

	got, err := s.ListProgenySkillInjections(ctx, []string{ancestor1, ancestor2})
	require.NoError(t, err)
	require.Len(t, got, 2)

	uris := map[string]bool{}
	for _, si := range got {
		uris[si.SkillURI] = true
	}
	assert.True(t, uris["skill://org/inherit-1@1.0"])
	assert.True(t, uris["skill://org/inherit-2@1.0"])
}

// TestListProgenySkillInjections_EmptyAncestors verifies that empty/nil
// ancestor IDs return nil without error.
func TestListProgenySkillInjections_EmptyAncestors(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	got, err := s.ListProgenySkillInjections(ctx, nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = s.ListProgenySkillInjections(ctx, []string{})
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestSkillInjection_AllowProgenyRoundTrip verifies that AllowProgeny is
// correctly persisted and retrieved for user-scoped skill injections.
func TestSkillInjection_AllowProgenyRoundTrip(t *testing.T) {
	s := newTestSkillInjectionStore(t)
	ctx := context.Background()

	userID := uuid.NewString()
	si := &store.SkillInjection{
		ID:           uuid.NewString(),
		Scope:        store.SkillInjectionScopeUser,
		ScopeID:      userID,
		SkillURI:     "skill://org/progeny-skill@1.0",
		AllowProgeny: true,
		CreatedBy:    userID,
	}
	require.NoError(t, s.AddSkillInjection(ctx, si))

	list, err := s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].AllowProgeny, "AllowProgeny must be true after round-trip")

	// Update to false.
	si.AllowProgeny = false
	require.NoError(t, s.UpdateSkillInjection(ctx, si))

	list, err = s.ListSkillInjections(ctx, store.SkillInjectionScopeUser, userID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.False(t, list[0].AllowProgeny, "AllowProgeny must be false after update")
}
