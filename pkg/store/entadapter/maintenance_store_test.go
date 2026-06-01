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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestMaintenanceStore(t *testing.T) *MaintenanceStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewMaintenanceStore(client)
}

func TestSeedMaintenanceOperations(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()

	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	ops, err := s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, ops, len(defaultSeedOperations))

	// Every seeded op must have a valid Go-generated UUID id (not a randomblob).
	for _, op := range ops {
		_, err := uuid.Parse(op.ID)
		assert.NoError(t, err, "seeded op %q should have a valid UUID id", op.Key)
		assert.Equal(t, store.MaintenanceStatusPending, op.Status)
	}

	// Seeding is idempotent.
	require.NoError(t, s.SeedMaintenanceOperations(ctx))
	ops, err = s.ListMaintenanceOperations(ctx)
	require.NoError(t, err)
	assert.Len(t, ops, len(defaultSeedOperations))
}

func TestGetMaintenanceOperationNotFound(t *testing.T) {
	s := newTestMaintenanceStore(t)
	_, err := s.GetMaintenanceOperation(context.Background(), "does-not-exist")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateMaintenanceOperation(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	startedAt := time.Now().UTC().Truncate(time.Second)
	op := &store.MaintenanceOperation{
		Key:       "pull-images",
		Status:    store.MaintenanceStatusRunning,
		StartedAt: &startedAt,
		StartedBy: "admin",
		Metadata:  `{"foo":"bar"}`,
	}
	require.NoError(t, s.UpdateMaintenanceOperation(ctx, op))

	got, err := s.GetMaintenanceOperation(ctx, "pull-images")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusRunning, got.Status)
	assert.Equal(t, "admin", got.StartedBy)
	assert.Equal(t, `{"foo":"bar"}`, got.Metadata)
	require.NotNil(t, got.StartedAt)
}

func TestUpdateMaintenanceOperationNotFound(t *testing.T) {
	s := newTestMaintenanceStore(t)
	err := s.UpdateMaintenanceOperation(context.Background(), &store.MaintenanceOperation{
		Key:    "ghost",
		Status: store.MaintenanceStatusRunning,
	})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMaintenanceRunRMW(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	run := &store.MaintenanceOperationRun{
		ID:           uuid.NewString(),
		OperationKey: "pull-images",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
		StartedBy:    "admin",
		Log:          "starting",
	}
	require.NoError(t, s.CreateMaintenanceRun(ctx, run))

	got, err := s.GetMaintenanceRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusRunning, got.Status)
	assert.Equal(t, "pull-images", got.OperationKey)

	// Read-modify-write to completed.
	completedAt := time.Now().UTC().Truncate(time.Second)
	run.Status = store.MaintenanceStatusCompleted
	run.CompletedAt = &completedAt
	run.Result = `{"ok":true}`
	run.Log = "starting\ndone"
	require.NoError(t, s.UpdateMaintenanceRun(ctx, run))

	got, err = s.GetMaintenanceRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusCompleted, got.Status)
	require.NotNil(t, got.CompletedAt)
	assert.Equal(t, `{"ok":true}`, got.Result)
	assert.Equal(t, "starting\ndone", got.Log)

	// List runs for the operation.
	runs, err := s.ListMaintenanceRuns(ctx, "pull-images", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run.ID, runs[0].ID)
}

func TestGetMaintenanceRunNotFound(t *testing.T) {
	s := newTestMaintenanceStore(t)
	_, err := s.GetMaintenanceRun(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAbortRunningMaintenanceOps(t *testing.T) {
	s := newTestMaintenanceStore(t)
	ctx := context.Background()
	require.NoError(t, s.SeedMaintenanceOperations(ctx))

	// A running migration should be reset to pending.
	startedAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.UpdateMaintenanceOperation(ctx, &store.MaintenanceOperation{
		Key:       "secret-hub-id-migration",
		Status:    store.MaintenanceStatusRunning,
		StartedAt: &startedAt,
	}))

	// A running run should be marked failed.
	run := &store.MaintenanceOperationRun{
		ID:           uuid.NewString(),
		OperationKey: "pull-images",
		Status:       store.MaintenanceStatusRunning,
		StartedAt:    startedAt,
	}
	require.NoError(t, s.CreateMaintenanceRun(ctx, run))

	runs, migrations, err := s.AbortRunningMaintenanceOps(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), runs)
	assert.Equal(t, int64(1), migrations)

	gotRun, err := s.GetMaintenanceRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusFailed, gotRun.Status)
	require.NotNil(t, gotRun.CompletedAt)

	gotMig, err := s.GetMaintenanceOperation(ctx, "secret-hub-id-migration")
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceStatusPending, gotMig.Status)
	assert.Nil(t, gotMig.StartedAt)
}
