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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaintenanceOperationsSeeded(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	ops, err := s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	require.Len(t, ops, 5, "expected 5 seeded operations (1 migration + 4 operations)")

	// Verify categories
	var migrations, operations int
	for _, op := range ops {
		switch op.Category {
		case store.MaintenanceCategoryMigration:
			migrations++
		case store.MaintenanceCategoryOperation:
			operations++
		}
	}
	assert.Equal(t, 1, migrations, "expected 1 migration")
	assert.Equal(t, 4, operations, "expected 4 operations")
}

func TestMaintenanceGetOperationByKey(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	op, err := s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)
	assert.Equal(t, "Secret Hub ID Namespace Migration", op.Title)
	assert.Equal(t, store.MaintenanceCategoryMigration, op.Category)
	assert.Equal(t, store.MaintenanceStatusPending, op.Status)

	// Not found
	_, err = s.GetMaintenanceOperation(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMaintenanceUpdateOperation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	op, err := s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	op.Status = store.MaintenanceStatusCompleted
	op.StartedAt = &now
	op.CompletedAt = &now
	op.StartedBy = "admin-user"
	op.Result = `{"migrated": 5}`

	err = s.UpdateMaintenanceOperation(ctx, op)
	require.NoError(t, err)

	updated, err := s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusCompleted, updated.Status)
	assert.Equal(t, "admin-user", updated.StartedBy)
	assert.Equal(t, `{"migrated": 5}`, updated.Result)
}

func TestMaintenanceRunCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	runID := api.NewUUID()
	now := time.Now().UTC().Truncate(time.Second)

	run := &store.MaintenanceOperationRun{
		ID:           runID,
		OperationKey: "pull-images",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    now,
		StartedBy:    "admin-user",
		Log:          "Pulling images...",
	}

	err := s.CreateMaintenanceRun(ctx, run)
	require.NoError(t, err)

	// Get
	got, err := s.GetMaintenanceRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "pull-images", got.OperationKey)
	assert.Equal(t, store.MaintenanceStatusRunning, got.Status)
	assert.Equal(t, "Pulling images...", got.Log)

	// Update
	completedAt := now.Add(30 * time.Second)
	got.Status = store.MaintenanceStatusCompleted
	got.CompletedAt = &completedAt
	got.Result = `{"pulled": 3}`
	got.Log = "Pulling images...\nDone."

	err = s.UpdateMaintenanceRun(ctx, got)
	require.NoError(t, err)

	updated, err := s.GetMaintenanceRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusCompleted, updated.Status)
	assert.Equal(t, `{"pulled": 3}`, updated.Result)

	// List
	runs, err := s.ListMaintenanceRuns(ctx, "pull-images", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, runID, runs[0].ID)

	// Not found
	_, err = s.GetMaintenanceRun(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAbortRunningMaintenanceOps(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Set a migration to "running" to simulate an interrupted migration.
	op, err := s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)
	now := time.Now()
	op.Status = store.MaintenanceStatusRunning
	op.StartedAt = &now
	op.StartedBy = "admin@example.com"
	require.NoError(t, s.UpdateMaintenanceOperation(ctx, op))

	// Create two "running" operation runs to simulate interrupted operations.
	for i, key := range []string{"pull-images", "rebuild-server"} {
		run := &store.MaintenanceOperationRun{
			ID:           api.NewUUID(),
			OperationKey: key,
			Status:       store.MaintenanceStatusRunning,
			StartedAt:    now.Add(time.Duration(i) * time.Second),
			StartedBy:    "admin@example.com",
		}
		require.NoError(t, s.CreateMaintenanceRun(ctx, run))
	}

	// Create a completed run that should NOT be affected.
	completed := now.Add(-time.Hour)
	completedRun := &store.MaintenanceOperationRun{
		ID:           api.NewUUID(),
		OperationKey: "pull-images",
		Status:       store.MaintenanceStatusCompleted,
		StartedAt:    completed,
		CompletedAt:  &completed,
		StartedBy:    "admin@example.com",
	}
	require.NoError(t, s.CreateMaintenanceRun(ctx, completedRun))

	// Abort all running operations.
	runs, migrations, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), runs, "expected 2 stalled runs aborted")
	assert.Equal(t, int64(1), migrations, "expected 1 stalled migration reset")

	// Verify the migration was reset to pending.
	op, err = s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusPending, op.Status)
	assert.Nil(t, op.StartedAt)

	// Verify running runs were marked as failed.
	allRuns, err := s.ListMaintenanceRuns(ctx, "pull-images", 10)
	require.NoError(t, err)
	for _, r := range allRuns {
		if r.ID == completedRun.ID {
			assert.Equal(t, store.MaintenanceStatusCompleted, r.Status, "completed run should be unchanged")
		} else {
			assert.Equal(t, store.MaintenanceStatusFailed, r.Status, "running run should be failed")
			assert.NotNil(t, r.CompletedAt, "aborted run should have completedAt")
		}
	}

	// Running it again should be a no-op.
	runs, migrations, err = s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), runs)
	assert.Equal(t, int64(0), migrations)
}
