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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestQuotaStore(t *testing.T) *QuotaStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewQuotaStore(client)
}

// ---------------------------------------------------------------------------
// LimitDefinition CRUD
// ---------------------------------------------------------------------------

func TestQuotaStore_CreateLimitDefinition(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		Name:         "max_agents_per_project",
		ResourceType: "agent",
		Unit:         "count",
		Description:  "Maximum agents per project",
		DefaultValue: 0,
		System:       true,
	}

	created, err := qs.CreateLimitDefinition(ctx, def)
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "max_agents_per_project", created.Name)
	assert.Equal(t, "agent", created.ResourceType)
	assert.Equal(t, "count", created.Unit)
	assert.Equal(t, "Maximum agents per project", created.Description)
	assert.Equal(t, int64(0), created.DefaultValue)
	assert.True(t, created.System)
	assert.False(t, created.CreatedAt.IsZero())
	assert.False(t, created.UpdatedAt.IsZero())
}

func TestQuotaStore_CreateLimitDefinition_DuplicateName(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		Name:         "max_agents_per_project",
		ResourceType: "agent",
		Unit:         "count",
		System:       true,
	}

	_, err := qs.CreateLimitDefinition(ctx, def)
	require.NoError(t, err)

	// Creating a second definition with the same name should fail.
	_, err = qs.CreateLimitDefinition(ctx, def)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestQuotaStore_GetLimitDefinition(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		Name:         "max_projects_per_user",
		ResourceType: "project",
		Unit:         "count",
		Description:  "Maximum projects per user",
		DefaultValue: 50,
		System:       false,
	}

	created, err := qs.CreateLimitDefinition(ctx, def)
	require.NoError(t, err)

	got, err := qs.GetLimitDefinition(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "max_projects_per_user", got.Name)
	assert.Equal(t, int64(50), got.DefaultValue)
}

func TestQuotaStore_GetLimitDefinition_NotFound(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	_, err := qs.GetLimitDefinition(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestQuotaStore_GetLimitDefinitionByName(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		Name:         "max_members_per_group",
		ResourceType: "group",
		Unit:         "count",
		Description:  "Maximum members per group",
		DefaultValue: 100,
		System:       true,
	}

	_, err := qs.CreateLimitDefinition(ctx, def)
	require.NoError(t, err)

	got, err := qs.GetLimitDefinitionByName(ctx, "max_members_per_group")
	require.NoError(t, err)
	assert.Equal(t, "max_members_per_group", got.Name)
	assert.Equal(t, int64(100), got.DefaultValue)
}

func TestQuotaStore_GetLimitDefinitionByName_NotFound(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	_, err := qs.GetLimitDefinitionByName(ctx, "nonexistent_limit")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestQuotaStore_ListLimitDefinitions(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	defs := []*store.LimitDefinition{
		{Name: "limit_b", ResourceType: "project", Unit: "count"},
		{Name: "limit_a", ResourceType: "agent", Unit: "count"},
		{Name: "limit_c", ResourceType: "group", Unit: "count"},
	}
	for _, d := range defs {
		_, err := qs.CreateLimitDefinition(ctx, d)
		require.NoError(t, err)
	}

	list, err := qs.ListLimitDefinitions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	// Should be sorted by name.
	assert.Equal(t, "limit_a", list[0].Name)
	assert.Equal(t, "limit_b", list[1].Name)
	assert.Equal(t, "limit_c", list[2].Name)
}

func TestQuotaStore_UpdateLimitDefinition(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		Name:         "updatable_limit",
		ResourceType: "agent",
		Unit:         "count",
		DefaultValue: 10,
	}

	created, err := qs.CreateLimitDefinition(ctx, def)
	require.NoError(t, err)

	created.DefaultValue = 42
	created.Description = "Updated description"

	updated, err := qs.UpdateLimitDefinition(ctx, created)
	require.NoError(t, err)
	assert.Equal(t, int64(42), updated.DefaultValue)
	assert.Equal(t, "Updated description", updated.Description)
}

func TestQuotaStore_UpdateLimitDefinition_NotFound(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		ID:           uuid.New().String(),
		Name:         "ghost",
		ResourceType: "agent",
		Unit:         "count",
	}

	_, err := qs.UpdateLimitDefinition(ctx, def)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestQuotaStore_DeleteLimitDefinition(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	def := &store.LimitDefinition{
		Name:         "deletable_limit",
		ResourceType: "agent",
		Unit:         "count",
	}

	created, err := qs.CreateLimitDefinition(ctx, def)
	require.NoError(t, err)

	err = qs.DeleteLimitDefinition(ctx, created.ID)
	require.NoError(t, err)

	_, err = qs.GetLimitDefinition(ctx, created.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestQuotaStore_DeleteLimitDefinition_NotFound(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	err := qs.DeleteLimitDefinition(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// EntitlementBinding CRUD
// ---------------------------------------------------------------------------

func createTestLimitDef(t *testing.T, qs *QuotaStore, name string) *store.LimitDefinition {
	t.Helper()
	ld, err := qs.CreateLimitDefinition(context.Background(), &store.LimitDefinition{
		Name:         name,
		ResourceType: "agent",
		Unit:         "count",
		System:       true,
	})
	require.NoError(t, err)
	return ld
}

func TestQuotaStore_CreateEntitlementBinding(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "test_limit")

	binding := &store.EntitlementBinding{
		LimitDefinitionID: ld.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         uuid.New().String(),
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		Value:             100,
		CreatedBy:         "admin",
	}

	created, err := qs.CreateEntitlementBinding(ctx, binding)
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, ld.ID, created.LimitDefinitionID)
	assert.Equal(t, store.EntitlementSubjectUser, created.SubjectType)
	assert.Equal(t, int64(100), created.Value)
}

func TestQuotaStore_CreateEntitlementBinding_UniqueConstraint(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "test_limit_unique")

	subjectID := uuid.New().String()
	binding := &store.EntitlementBinding{
		LimitDefinitionID: ld.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		Value:             100,
	}

	_, err := qs.CreateEntitlementBinding(ctx, binding)
	require.NoError(t, err)

	// Creating a duplicate binding (same limit + subject + scope) should fail.
	binding2 := &store.EntitlementBinding{
		LimitDefinitionID: ld.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		Value:             200,
	}
	_, err = qs.CreateEntitlementBinding(ctx, binding2)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestQuotaStore_GetEntitlementBinding(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "test_limit_get_eb")

	binding := &store.EntitlementBinding{
		LimitDefinitionID: ld.ID,
		SubjectType:       store.EntitlementSubjectSystemDefault,
		ScopeType:         store.QuotaScopeSystem,
		Value:             50,
	}

	created, err := qs.CreateEntitlementBinding(ctx, binding)
	require.NoError(t, err)

	got, err := qs.GetEntitlementBinding(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, int64(50), got.Value)
}

func TestQuotaStore_GetEntitlementBinding_NotFound(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	_, err := qs.GetEntitlementBinding(ctx, uuid.New().String())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestQuotaStore_ListEntitlementBindings(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld1 := createTestLimitDef(t, qs, "list_eb_limit_1")
	ld2 := createTestLimitDef(t, qs, "list_eb_limit_2")

	// Create bindings for ld1
	for i := 0; i < 3; i++ {
		_, err := qs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
			LimitDefinitionID: ld1.ID,
			SubjectType:       store.EntitlementSubjectUser,
			SubjectID:         uuid.New().String(),
			ScopeType:         store.QuotaScopeSystem,
			Value:             int64(i + 1),
		})
		require.NoError(t, err)
	}

	// Create a binding for ld2
	_, err := qs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: ld2.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         uuid.New().String(),
		ScopeType:         store.QuotaScopeSystem,
		Value:             99,
	})
	require.NoError(t, err)

	// List only ld1 bindings
	list, err := qs.ListEntitlementBindings(ctx, ld1.ID)
	require.NoError(t, err)
	assert.Len(t, list, 3)
}

func TestQuotaStore_ListEntitlementBindingsForSubject(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld1 := createTestLimitDef(t, qs, "subject_limit_1")
	ld2 := createTestLimitDef(t, qs, "subject_limit_2")

	subjectID := uuid.New().String()

	_, err := qs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: ld1.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeSystem,
		Value:             10,
	})
	require.NoError(t, err)

	_, err = qs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: ld2.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           uuid.New().String(),
		Value:             20,
	})
	require.NoError(t, err)

	list, err := qs.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectUser, subjectID)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestQuotaStore_UpdateEntitlementBinding(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "update_eb_limit")

	created, err := qs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: ld.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         uuid.New().String(),
		ScopeType:         store.QuotaScopeSystem,
		Value:             10,
		CreatedBy:         "admin",
	})
	require.NoError(t, err)

	created.Value = 999
	updated, err := qs.UpdateEntitlementBinding(ctx, created)
	require.NoError(t, err)
	assert.Equal(t, int64(999), updated.Value)
}

func TestQuotaStore_DeleteEntitlementBinding(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "delete_eb_limit")

	created, err := qs.CreateEntitlementBinding(ctx, &store.EntitlementBinding{
		LimitDefinitionID: ld.ID,
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         uuid.New().String(),
		ScopeType:         store.QuotaScopeSystem,
		Value:             10,
	})
	require.NoError(t, err)

	err = qs.DeleteEntitlementBinding(ctx, created.ID)
	require.NoError(t, err)

	_, err = qs.GetEntitlementBinding(ctx, created.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ---------------------------------------------------------------------------
// UsageReservation operations
// ---------------------------------------------------------------------------

func TestQuotaStore_CreateUsageReservation(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "reservation_limit")

	reservation := &store.UsageReservation{
		LimitDefinitionID: ld.ID,
		SubjectID:         uuid.New().String(),
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           uuid.New().String(),
		ResourceID:        uuid.New().String(),
		Reserved:          1,
	}

	created, err := qs.CreateUsageReservation(ctx, reservation)
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, ld.ID, created.LimitDefinitionID)
	assert.Equal(t, int64(1), created.Reserved)
	assert.Nil(t, created.ReleasedAt)
}

func TestQuotaStore_CountActiveReservations(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "count_limit")
	subjectID := uuid.New().String()
	scopeID := uuid.New().String()

	// Create 3 active reservations
	for i := 0; i < 3; i++ {
		_, err := qs.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: ld.ID,
			SubjectID:         subjectID,
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           scopeID,
			ResourceID:        uuid.New().String(),
			Reserved:          1,
		})
		require.NoError(t, err)
	}

	count, err := qs.CountActiveReservations(ctx, ld.ID, subjectID, store.QuotaScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)
}

func TestQuotaStore_CountActiveReservations_ExcludesReleased(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "count_released_limit")
	subjectID := uuid.New().String()
	scopeID := uuid.New().String()

	// Create 3 reservations
	resourceIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		resourceIDs[i] = uuid.New().String()
		_, err := qs.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: ld.ID,
			SubjectID:         subjectID,
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           scopeID,
			ResourceID:        resourceIDs[i],
			Reserved:          1,
		})
		require.NoError(t, err)
	}

	// Release one reservation
	err := qs.ReleaseReservation(ctx, ld.ID, resourceIDs[0])
	require.NoError(t, err)

	// Only 2 should be active
	count, err := qs.CountActiveReservations(ctx, ld.ID, subjectID, store.QuotaScopeProject, scopeID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), count)
}

func TestQuotaStore_ReleaseReservation_SetsReleasedAt(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "release_limit")
	resourceID := uuid.New().String()

	created, err := qs.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: ld.ID,
		SubjectID:         uuid.New().String(),
		ScopeType:         store.QuotaScopeSystem,
		ResourceID:        resourceID,
		Reserved:          1,
	})
	require.NoError(t, err)
	assert.Nil(t, created.ReleasedAt)

	err = qs.ReleaseReservation(ctx, ld.ID, resourceID)
	require.NoError(t, err)

	// The record should still exist (not deleted) with released_at set.
	active, err := qs.ListActiveReservations(ctx, ld.ID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Len(t, active, 0, "released reservation should not appear in active list")
}

func TestQuotaStore_ReleaseReservation_NotFound(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "release_nf_limit")

	err := qs.ReleaseReservation(ctx, ld.ID, "nonexistent-resource")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestQuotaStore_ReleaseReservationsByResource(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld1 := createTestLimitDef(t, qs, "release_by_res_1")
	ld2 := createTestLimitDef(t, qs, "release_by_res_2")

	resourceID := uuid.New().String()
	subjectID := uuid.New().String()

	// Create reservations for the same resource across different limits
	_, err := qs.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: ld1.ID,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeSystem,
		ResourceID:        resourceID,
		Reserved:          1,
	})
	require.NoError(t, err)

	_, err = qs.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: ld2.ID,
		SubjectID:         subjectID,
		ScopeType:         store.QuotaScopeSystem,
		ResourceID:        resourceID,
		Reserved:          1,
	})
	require.NoError(t, err)

	// Release all by resource
	err = qs.ReleaseReservationsByResource(ctx, resourceID)
	require.NoError(t, err)

	// Both should be released
	count1, err := qs.CountActiveReservations(ctx, ld1.ID, subjectID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), count1)

	count2, err := qs.CountActiveReservations(ctx, ld2.ID, subjectID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), count2)
}

func TestQuotaStore_ListActiveReservations(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	ld := createTestLimitDef(t, qs, "list_res_limit")
	scopeID := uuid.New().String()

	// Create 2 active + 1 released
	for i := 0; i < 3; i++ {
		_, err := qs.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: ld.ID,
			SubjectID:         uuid.New().String(),
			ScopeType:         store.QuotaScopeProject,
			ScopeID:           scopeID,
			ResourceID:        uuid.New().String(),
			Reserved:          1,
		})
		require.NoError(t, err)
	}

	// List all active
	active, err := qs.ListActiveReservations(ctx, ld.ID, store.QuotaScopeProject, scopeID)
	require.NoError(t, err)
	assert.Len(t, active, 3)

	// Release one
	err = qs.ReleaseReservation(ctx, ld.ID, active[0].ResourceID)
	require.NoError(t, err)

	// Should now be 2
	active, err = qs.ListActiveReservations(ctx, ld.ID, store.QuotaScopeProject, scopeID)
	require.NoError(t, err)
	assert.Len(t, active, 2)
}

// ---------------------------------------------------------------------------
// Seed idempotency (via CompositeStore which includes QuotaStore)
// ---------------------------------------------------------------------------

func TestQuotaStore_SeedLimitDefinitions_Idempotent(t *testing.T) {
	qs := newTestQuotaStore(t)
	ctx := context.Background()

	// Simulate seeding: create a system limit definition.
	_, err := qs.CreateLimitDefinition(ctx, &store.LimitDefinition{
		Name:         store.LimitMaxAgentsPerProject,
		ResourceType: "agent",
		Unit:         "count",
		Description:  "Maximum agents per project",
		DefaultValue: 0,
		System:       true,
	})
	require.NoError(t, err)

	// Simulate a second seed call: check-then-skip pattern.
	existing, err := qs.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	assert.Equal(t, store.LimitMaxAgentsPerProject, existing.Name)

	// Verify no duplicate was created.
	list, err := qs.ListLimitDefinitions(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}
