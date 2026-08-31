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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pre-defined test entity IDs.
var (
	acTestProjectID = uuid.MustParse("a0000000-0000-0000-0000-000000000001")
	acTestUserID    = uuid.MustParse("a0000000-0000-0000-0000-000000000002")
	acTestAgentID   = uuid.MustParse("a0000000-0000-0000-0000-000000000003")
	acTestGroupID   = uuid.MustParse("a0000000-0000-0000-0000-000000000004")
	// Same ID used by multiple entity types to test type-collision.
	acTestSharedID = uuid.MustParse("a0000000-0000-0000-0000-00000000000f")
)

// newTestACStore returns a fresh AccessConstraintStore with seeded entities
// for reference validation tests.
func newTestACStore(t *testing.T) (*AccessConstraintStore, *ent.Client) {
	t.Helper()
	client := enttest.NewClient(t)
	ctx := context.Background()

	// Seed a project.
	_, err := client.Project.Create().
		SetID(acTestProjectID).
		SetName("test-project").
		SetSlug("test-project").
		Save(ctx)
	require.NoError(t, err)

	// Seed a user.
	_, err = client.User.Create().
		SetID(acTestUserID).
		SetEmail("testuser@example.com").
		SetDisplayName("Test User").
		SetRole("member").
		SetStatus("active").
		Save(ctx)
	require.NoError(t, err)

	// Seed an agent.
	_, err = client.Agent.Create().
		SetID(acTestAgentID).
		SetSlug("test-agent").
		SetName("Test Agent").
		SetProjectID(acTestProjectID).
		SetVisibility("private").
		SetMessageMode("project").
		Save(ctx)
	require.NoError(t, err)

	// Seed a group.
	_, err = client.Group.Create().
		SetID(acTestGroupID).
		SetName("Test Group").
		SetSlug("test-group").
		Save(ctx)
	require.NoError(t, err)

	return NewAccessConstraintStore(client), client
}

// newTestACStoreWithSharedIDs creates entities with the same UUID across different
// entity types, for type-collision testing.
func newTestACStoreWithSharedIDs(t *testing.T) *AccessConstraintStore {
	t.Helper()
	client := enttest.NewClient(t)
	ctx := context.Background()

	// Seed a project.
	_, err := client.Project.Create().
		SetID(acTestProjectID).
		SetName("test-project").
		SetSlug("test-project").
		Save(ctx)
	require.NoError(t, err)

	// Create a user with the shared ID.
	_, err = client.User.Create().
		SetID(acTestSharedID).
		SetEmail("shared@example.com").
		SetDisplayName("Shared User").
		SetRole("member").
		SetStatus("active").
		Save(ctx)
	require.NoError(t, err)

	// Create a group with the shared ID.
	_, err = client.Group.Create().
		SetID(acTestSharedID).
		SetName("Shared Group").
		SetSlug("shared-group").
		Save(ctx)
	require.NoError(t, err)

	// Create an agent with the shared ID.
	_, err = client.Agent.Create().
		SetID(acTestSharedID).
		SetSlug("shared-agent").
		SetName("Shared Agent").
		SetProjectID(acTestProjectID).
		SetVisibility("private").
		SetMessageMode("project").
		Save(ctx)
	require.NoError(t, err)

	return NewAccessConstraintStore(client)
}

func strPtr(s string) *string { return &s }

func newBaseConstraint(name string) *store.AccessConstraint {
	return &store.AccessConstraint{
		Name:               name,
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          "system",
		ScopeID:            "",
		MaximumPermissions: []string{"agent.read", "project.read"},
		Purpose:            "test constraint",
		CreatedBy:          "test-user",
	}
}

// ---------------------------------------------------------------------------
// Basic CRUD tests
// ---------------------------------------------------------------------------

func TestCreateAccessConstraint_SetsRevision1(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-revision-init")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)
	assert.Equal(t, "test constraint", created.Purpose)
}

func TestCreateAccessConstraint_SetsPurposeAndCreatedBy(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-purpose")
	c.Purpose = "prevent lockout"
	c.CreatedBy = "admin@example.com"
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, "prevent lockout", created.Purpose)
	assert.Equal(t, "admin@example.com", created.CreatedBy)
}

func TestUpdateAccessConstraint_IncrementsRevision(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-increment")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)

	// Update — revision should increment.
	created.Name = "test-increment-updated"
	created.UpdatedBy = "updater@example.com"
	updated, err := s.UpdateAccessConstraint(ctx, created, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	assert.Equal(t, "updater@example.com", updated.UpdatedBy)
}

func TestUpdateAccessConstraint_SetsPurposeAndUpdatedBy(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-updated-by")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)

	created.Purpose = "updated purpose"
	created.UpdatedBy = "admin2@example.com"
	updated, err := s.UpdateAccessConstraint(ctx, created, 0)
	require.NoError(t, err)
	assert.Equal(t, "updated purpose", updated.Purpose)
	assert.Equal(t, "admin2@example.com", updated.UpdatedBy)
}

// ---------------------------------------------------------------------------
// Optimistic concurrency tests
// ---------------------------------------------------------------------------

func TestConcurrentUpdate_RevisionConflict(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-conflict")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)

	// Simulate two concurrent reads.
	v1, err := s.GetAccessConstraint(ctx, created.ID)
	require.NoError(t, err)

	// First update succeeds (expected revision = 1).
	v1.Name = "winner"
	_, err = s.UpdateAccessConstraint(ctx, v1, 1)
	require.NoError(t, err)

	// Second update with stale revision (expected 1, but stored is now 2).
	v1.Name = "loser"
	_, err = s.UpdateAccessConstraint(ctx, v1, 1)
	require.ErrorIs(t, err, store.ErrRevisionConflict)
}

func TestConcurrentUpdate_NoRevisionCheck(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-no-check")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)

	// Update with expectedRevision=0 skips the check.
	created.Name = "updated"
	updated, err := s.UpdateAccessConstraint(ctx, created, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
}

func TestDeletionRace(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := newBaseConstraint("test-deletion-race")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)

	// Delete the constraint.
	err = s.DeleteAccessConstraint(ctx, created.ID)
	require.NoError(t, err)

	// Attempting to update a deleted constraint should return not found.
	created.Name = "ghost"
	_, err = s.UpdateAccessConstraint(ctx, created, 0)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Reference validation tests
// ---------------------------------------------------------------------------

func TestCreateAccessConstraint_ValidUserReference(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	c := &store.AccessConstraint{
		Name:                 "user-ref",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeUser),
		SubjectPrincipalID:   strPtr(acTestUserID.String()),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, acTestUserID.String(), *created.SubjectPrincipalID)
}

func TestCreateAccessConstraint_MissingUserReference(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	nonExistent := uuid.New().String()
	c := &store.AccessConstraint{
		Name:                 "missing-user",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeUser),
		SubjectPrincipalID:   strPtr(nonExistent),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	_, err := s.CreateAccessConstraint(ctx, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAccessConstraint_MissingAgentReference(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	nonExistent := uuid.New().String()
	c := &store.AccessConstraint{
		Name:                 "missing-agent",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeAgent),
		SubjectPrincipalID:   strPtr(nonExistent),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	_, err := s.CreateAccessConstraint(ctx, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAccessConstraint_MissingGroupReference(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	nonExistent := uuid.New().String()
	c := &store.AccessConstraint{
		Name:                 "missing-group",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeGroup),
		SubjectPrincipalID:   strPtr(nonExistent),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	_, err := s.CreateAccessConstraint(ctx, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAccessConstraint_MissingGroupClosureReference(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	nonExistent := uuid.New().String()
	c := &store.AccessConstraint{
		Name:               "missing-closure-group",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     strPtr(nonExistent),
		ScopeType:          "system",
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test",
		CreatedBy:          "test",
	}
	_, err := s.CreateAccessConstraint(ctx, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCreateAccessConstraint_MissingProjectScope(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	nonExistent := uuid.New().String()
	c := &store.AccessConstraint{
		Name:               "missing-project-scope",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          "project",
		ScopeID:            nonExistent,
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test",
		CreatedBy:          "test",
	}
	_, err := s.CreateAccessConstraint(ctx, c)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateAccessConstraint_MissingReference(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	// Create a valid constraint first.
	c := newBaseConstraint("update-missing-ref")
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)

	// Try to update to reference a non-existent user.
	nonExistent := uuid.New().String()
	created.SubjectKind = store.ConstraintSubjectPrincipal
	created.SubjectPrincipalType = strPtr(store.ConstraintPrincipalTypeUser)
	created.SubjectPrincipalID = strPtr(nonExistent)
	_, err = s.UpdateAccessConstraint(ctx, created, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Type-collision ID tests
// ---------------------------------------------------------------------------

func TestTypeCollisionIDs(t *testing.T) {
	s := newTestACStoreWithSharedIDs(t)
	ctx := context.Background()

	sharedIDStr := acTestSharedID.String()

	// Same ID works as user.
	c1 := &store.AccessConstraint{
		Name:                 "collision-user",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeUser),
		SubjectPrincipalID:   strPtr(sharedIDStr),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	_, err := s.CreateAccessConstraint(ctx, c1)
	require.NoError(t, err)

	// Same ID works as agent.
	c2 := &store.AccessConstraint{
		Name:                 "collision-agent",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeAgent),
		SubjectPrincipalID:   strPtr(sharedIDStr),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	_, err = s.CreateAccessConstraint(ctx, c2)
	require.NoError(t, err)

	// Same ID works as group.
	c3 := &store.AccessConstraint{
		Name:                 "collision-group",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeGroup),
		SubjectPrincipalID:   strPtr(sharedIDStr),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	_, err = s.CreateAccessConstraint(ctx, c3)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Group closure semantic distinction tests
// ---------------------------------------------------------------------------

func TestExactGroupVsClosure(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	groupIDStr := acTestGroupID.String()

	// Create an "exact principal = group" constraint.
	exact := &store.AccessConstraint{
		Name:                 "exact-group",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeGroup),
		SubjectPrincipalID:   strPtr(groupIDStr),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "exact group match",
		CreatedBy:            "test",
	}
	exactCreated, err := s.CreateAccessConstraint(ctx, exact)
	require.NoError(t, err)
	assert.Equal(t, store.ConstraintSubjectPrincipal, exactCreated.SubjectKind)

	// Create a "group_closure" constraint on same group.
	closure := &store.AccessConstraint{
		Name:               "closure-group",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     strPtr(groupIDStr),
		ScopeType:          "system",
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "group closure match",
		CreatedBy:          "test",
	}
	closureCreated, err := s.CreateAccessConstraint(ctx, closure)
	require.NoError(t, err)
	assert.Equal(t, store.ConstraintSubjectGroupClosure, closureCreated.SubjectKind)

	// The two constraints are semantically different.
	assert.NotEqual(t, exactCreated.ID, closureCreated.ID)
	assert.NotEqual(t, exactCreated.SubjectKind, closureCreated.SubjectKind)
}

// ---------------------------------------------------------------------------
// Subject field cleanup tests
// ---------------------------------------------------------------------------

func TestSubjectFieldCleanup_KindChange(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	groupIDStr := acTestGroupID.String()

	// Create a group_closure constraint.
	c := &store.AccessConstraint{
		Name:               "cleanup-test",
		SubjectKind:        store.ConstraintSubjectGroupClosure,
		SubjectGroupID:     strPtr(groupIDStr),
		ScopeType:          "system",
		MaximumPermissions: []string{"agent.read"},
		Purpose:            "test",
		CreatedBy:          "test",
	}
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)
	assert.NotNil(t, created.SubjectGroupID)

	// Change to all_principals — orphaned group field should be cleared.
	created.SubjectKind = store.ConstraintSubjectAllPrincipals
	created.SubjectGroupID = nil
	updated, err := s.UpdateAccessConstraint(ctx, created, 0)
	require.NoError(t, err)
	assert.Nil(t, updated.SubjectGroupID)
	assert.Nil(t, updated.SubjectPrincipalType)
	assert.Nil(t, updated.SubjectPrincipalID)
}

func TestSubjectFieldCleanup_PrincipalToGroupClosure(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	userIDStr := acTestUserID.String()
	groupIDStr := acTestGroupID.String()

	// Create a principal constraint targeting a user.
	c := &store.AccessConstraint{
		Name:                 "cleanup-principal",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: strPtr(store.ConstraintPrincipalTypeUser),
		SubjectPrincipalID:   strPtr(userIDStr),
		ScopeType:            "system",
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "test",
		CreatedBy:            "test",
	}
	created, err := s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)
	assert.NotNil(t, created.SubjectPrincipalType)
	assert.NotNil(t, created.SubjectPrincipalID)

	// Change to group_closure — principal fields should be cleared.
	created.SubjectKind = store.ConstraintSubjectGroupClosure
	created.SubjectGroupID = strPtr(groupIDStr)
	created.SubjectPrincipalType = nil
	created.SubjectPrincipalID = nil
	updated, err := s.UpdateAccessConstraint(ctx, created, 0)
	require.NoError(t, err)
	assert.Nil(t, updated.SubjectPrincipalType)
	assert.Nil(t, updated.SubjectPrincipalID)
	assert.NotNil(t, updated.SubjectGroupID)
	assert.Equal(t, groupIDStr, *updated.SubjectGroupID)
}

// ---------------------------------------------------------------------------
// Filter/cursor pagination tests
// ---------------------------------------------------------------------------

func TestListAccessConstraintsFiltered_Filters(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	userIDStr := acTestUserID.String()
	groupIDStr := acTestGroupID.String()

	// Create constraints with different subject kinds and scopes.
	constraints := []struct {
		name        string
		subjectKind string
		scopeType   string
	}{
		{"filter-all-system", store.ConstraintSubjectAllPrincipals, "system"},
		{"filter-principal-system", store.ConstraintSubjectPrincipal, "system"},
		{"filter-closure-system", store.ConstraintSubjectGroupClosure, "system"},
		{"filter-all-project", store.ConstraintSubjectAllPrincipals, "project"},
	}

	for _, tc := range constraints {
		c := &store.AccessConstraint{
			Name:               tc.name,
			SubjectKind:        tc.subjectKind,
			ScopeType:          tc.scopeType,
			MaximumPermissions: []string{"agent.read"},
			Purpose:            "test",
			CreatedBy:          "test",
		}
		if tc.subjectKind == store.ConstraintSubjectPrincipal {
			c.SubjectPrincipalType = strPtr(store.ConstraintPrincipalTypeUser)
			c.SubjectPrincipalID = strPtr(userIDStr)
		}
		if tc.subjectKind == store.ConstraintSubjectGroupClosure {
			c.SubjectGroupID = strPtr(groupIDStr)
		}
		if tc.scopeType == "project" {
			c.ScopeID = acTestProjectID.String()
		}
		_, err := s.CreateAccessConstraint(ctx, c)
		require.NoError(t, err, "creating %s", tc.name)
	}

	// Filter by subject kind.
	items, _, total, err := s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		SubjectKind: store.ConstraintSubjectAllPrincipals,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	assert.Len(t, items, 2)

	// Filter by scope type.
	items, _, total, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		ScopeType: "system",
	})
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Len(t, items, 3)

	// Filter by name contains.
	items, _, total, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		NameContains: "principal",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Equal(t, "filter-principal-system", items[0].Name)

	// Combined filters: subject_kind + scope_type.
	items, _, total, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		SubjectKind: store.ConstraintSubjectAllPrincipals,
		ScopeType:   "project",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Equal(t, "filter-all-project", items[0].Name)
}

func TestListAccessConstraintsFiltered_StatusFilter(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	now := time.Now()
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)

	// Active constraint: no time bounds.
	c1 := newBaseConstraint("status-active")
	_, err := s.CreateAccessConstraint(ctx, c1)
	require.NoError(t, err)

	// Expired constraint.
	c2 := newBaseConstraint("status-expired")
	c2.ExpiresAt = &past
	_, err = s.CreateAccessConstraint(ctx, c2)
	require.NoError(t, err)

	// Scheduled constraint.
	c3 := newBaseConstraint("status-scheduled")
	c3.NotBefore = &future
	_, err = s.CreateAccessConstraint(ctx, c3)
	require.NoError(t, err)

	// Recovery-disabled constraint.
	c4 := newBaseConstraint("status-disabled")
	c4.Disabled = true
	_, err = s.CreateAccessConstraint(ctx, c4)
	require.NoError(t, err)

	// Test active filter.
	items, _, _, err := s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		Status: "active",
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "status-active", items[0].Name)

	// Test expired filter.
	items, _, _, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		Status: "expired",
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "status-expired", items[0].Name)

	// Test scheduled filter.
	items, _, _, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		Status: "scheduled",
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "status-scheduled", items[0].Name)

	// Test recovery_disabled filter.
	items, _, _, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		Status: "recovery_disabled",
	})
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "status-disabled", items[0].Name)
}

func TestListAccessConstraintsFiltered_CursorPagination(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	// Create 5 constraints with names that sort deterministically.
	for i := 0; i < 5; i++ {
		c := newBaseConstraint(fmt.Sprintf("page-%d", i))
		_, err := s.CreateAccessConstraint(ctx, c)
		require.NoError(t, err)
	}

	// Use name-based sorting for deterministic ordering regardless of
	// SQLite timestamp precision.
	sortOpts := store.AccessConstraintListOptions{
		PageSize:  2,
		SortBy:    "name",
		SortOrder: "asc",
	}

	// Page 1: size 2.
	items, nextToken, total, err := s.ListAccessConstraintsFiltered(ctx, sortOpts)
	require.NoError(t, err)
	assert.Equal(t, 5, total)
	assert.Len(t, items, 2)
	assert.NotEmpty(t, nextToken)
	assert.Equal(t, "page-0", items[0].Name)
	assert.Equal(t, "page-1", items[1].Name)

	// Page 2: use cursor.
	sortOpts.PageToken = nextToken
	items, nextToken, _, err = s.ListAccessConstraintsFiltered(ctx, sortOpts)
	require.NoError(t, err)
	assert.Len(t, items, 2)
	assert.NotEmpty(t, nextToken)
	assert.Equal(t, "page-2", items[0].Name)
	assert.Equal(t, "page-3", items[1].Name)

	// Page 3: last page.
	sortOpts.PageToken = nextToken
	items, nextToken, _, err = s.ListAccessConstraintsFiltered(ctx, sortOpts)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Empty(t, nextToken, "no more pages")
	assert.Equal(t, "page-4", items[0].Name)
}

func TestListAccessConstraintsFiltered_StableCursorOrdering(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	// Create initial records.
	for i := 0; i < 3; i++ {
		c := newBaseConstraint(fmt.Sprintf("stable-%d", i))
		_, err := s.CreateAccessConstraint(ctx, c)
		require.NoError(t, err)
	}

	// Get first page sorted by name for deterministic ordering.
	items1, nextToken, _, err := s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		PageSize:  2,
		SortBy:    "name",
		SortOrder: "asc",
	})
	require.NoError(t, err)
	assert.Len(t, items1, 2)
	assert.Equal(t, "stable-0", items1[0].Name)
	assert.Equal(t, "stable-1", items1[1].Name)

	// Insert a new record while paginating — sorts between stable-1 and stable-2.
	c := newBaseConstraint("stable-1z")
	_, err = s.CreateAccessConstraint(ctx, c)
	require.NoError(t, err)

	// Get second page with cursor — keyset is anchored at stable-1, so
	// only items with name > "stable-1" appear. The insertion ("stable-1z")
	// sits between stable-1 and stable-2 and should appear.
	items2, _, _, err := s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		PageSize:  10,
		PageToken: nextToken,
		SortBy:    "name",
		SortOrder: "asc",
	})
	require.NoError(t, err)
	require.Len(t, items2, 2)
	assert.Equal(t, "stable-1z", items2[0].Name)
	assert.Equal(t, "stable-2", items2[1].Name)
}

func TestListAccessConstraintsFiltered_CursorWithCommaInName(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	// Create constraints with commas in names to verify cursor encoding
	// handles the delimiter correctly (R1 regression test).
	names := []string{"alpha", "ops,staging", "ops,staging,v2", "zulu"}
	for _, name := range names {
		c := newBaseConstraint(name)
		_, err := s.CreateAccessConstraint(ctx, c)
		require.NoError(t, err)
	}

	// Page through by name, size 2.
	items, nextToken, total, err := s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		PageSize:  2,
		SortBy:    "name",
		SortOrder: "asc",
	})
	require.NoError(t, err)
	assert.Equal(t, 4, total)
	require.Len(t, items, 2)
	assert.Equal(t, "alpha", items[0].Name)
	assert.Equal(t, "ops,staging", items[1].Name)

	// Second page — cursor was encoded from "ops,staging" which contains a comma.
	items, nextToken, _, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		PageSize:  2,
		PageToken: nextToken,
		SortBy:    "name",
		SortOrder: "asc",
	})
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "ops,staging,v2", items[0].Name)
	assert.Equal(t, "zulu", items[1].Name)
	assert.Empty(t, nextToken, "no more pages")
}

func TestListAccessConstraintsFiltered_SortOrder(t *testing.T) {
	s, _ := newTestACStore(t)
	ctx := context.Background()

	names := []string{"charlie", "alpha", "bravo"}
	for _, name := range names {
		c := newBaseConstraint(name)
		_, err := s.CreateAccessConstraint(ctx, c)
		require.NoError(t, err)
	}

	// Sort by name ascending.
	items, _, _, err := s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		SortBy:    "name",
		SortOrder: "asc",
	})
	require.NoError(t, err)
	require.Len(t, items, 3)
	assert.Equal(t, "alpha", items[0].Name)
	assert.Equal(t, "bravo", items[1].Name)
	assert.Equal(t, "charlie", items[2].Name)

	// Sort by name descending.
	items, _, _, err = s.ListAccessConstraintsFiltered(ctx, store.AccessConstraintListOptions{
		SortBy:    "name",
		SortOrder: "desc",
	})
	require.NoError(t, err)
	require.Len(t, items, 3)
	assert.Equal(t, "charlie", items[0].Name)
	assert.Equal(t, "bravo", items[1].Name)
	assert.Equal(t, "alpha", items[2].Name)
}

// Ensure fmt import is used (used in test table names).
var _ = fmt.Sprintf
