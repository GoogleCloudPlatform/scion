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
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestHubSettingStore(t *testing.T) *HubSettingStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewHubSettingStore(client)
}

// =============================================================================
// Get (missing)
// =============================================================================

func TestGetHubSetting_NotFound(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	_, err := s.GetHubSetting(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Create (expectedRevision == 0)
// =============================================================================

func TestUpsertHubSetting_Create(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	val := json.RawMessage(`{"admin_emails":["a@b.com"]}`)
	got, err := s.UpsertHubSetting(ctx, "access", val, "admin@test.com", 0, "")
	require.NoError(t, err)

	assert.Equal(t, "access", got.Section)
	assert.JSONEq(t, `{"admin_emails":["a@b.com"]}`, string(got.Value))
	assert.Equal(t, int64(1), got.Revision)
	assert.Equal(t, "admin@test.com", got.UpdatedBy)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.UpdatedAt.IsZero())
	assert.NotEmpty(t, got.ID)

	// Verify it's readable via Get.
	fetched, err := s.GetHubSetting(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, got.ID, fetched.ID)
	assert.Equal(t, got.Revision, fetched.Revision)
}

// =============================================================================
// Create-only conflict (expectedRevision == 0, row exists)
// =============================================================================

func TestUpsertHubSetting_CreateOnly_Conflict(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	val := json.RawMessage(`{"enabled":true}`)
	_, err := s.UpsertHubSetting(ctx, "telemetry", val, "admin@test.com", 0, "")
	require.NoError(t, err)

	// Second create-only for the same section should fail.
	_, err = s.UpsertHubSetting(ctx, "telemetry", val, "other@test.com", 0, "")
	assert.ErrorIs(t, err, store.ErrRevisionConflict)
}

// =============================================================================
// Update with revision bump (expectedRevision > 0)
// =============================================================================

func TestUpsertHubSetting_UpdateWithRevisionBump(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Create.
	v1 := json.RawMessage(`{"mode":"open"}`)
	created, err := s.UpsertHubSetting(ctx, "access", v1, "admin@test.com", 0, "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)

	// Update with correct revision.
	v2 := json.RawMessage(`{"mode":"invite_only"}`)
	updated, err := s.UpsertHubSetting(ctx, "access", v2, "admin2@test.com", 1, "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	assert.JSONEq(t, `{"mode":"invite_only"}`, string(updated.Value))
	assert.Equal(t, "admin2@test.com", updated.UpdatedBy)

	// Verify persisted.
	fetched, err := s.GetHubSetting(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, int64(2), fetched.Revision)
}

// =============================================================================
// CAS conflict (wrong expectedRevision)
// =============================================================================

func TestUpsertHubSetting_CAS_Conflict(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	val := json.RawMessage(`{"k":"v"}`)
	_, err := s.UpsertHubSetting(ctx, "lifecycle", val, "admin@test.com", 0, "")
	require.NoError(t, err)

	// Update with wrong revision → conflict.
	_, err = s.UpsertHubSetting(ctx, "lifecycle", val, "admin@test.com", 99, "")
	assert.ErrorIs(t, err, store.ErrRevisionConflict)
}

// =============================================================================
// CAS update on non-existent row (expectedRevision > 0, row missing)
// =============================================================================

func TestUpsertHubSetting_CAS_MissingRow(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	val := json.RawMessage(`{"k":"v"}`)
	_, err := s.UpsertHubSetting(ctx, "nonexistent", val, "admin@test.com", 5, "")
	assert.ErrorIs(t, err, store.ErrRevisionConflict)
}

// =============================================================================
// Unconditional upsert (expectedRevision == -1)
// =============================================================================

func TestUpsertHubSetting_Unconditional(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Create via unconditional upsert.
	v1 := json.RawMessage(`{"a":1}`)
	got, err := s.UpsertHubSetting(ctx, "maintenance", v1, "seed", -1, "seeded")
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Revision)

	// Overwrite via unconditional upsert — should succeed regardless of revision.
	v2 := json.RawMessage(`{"a":2}`)
	got2, err := s.UpsertHubSetting(ctx, "maintenance", v2, "seed2", -1, "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), got2.Revision)
	assert.JSONEq(t, `{"a":2}`, string(got2.Value))
}

// =============================================================================
// List
// =============================================================================

func TestListHubSettings(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Empty list.
	list, err := s.ListHubSettings(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)

	// Create a few sections.
	_, err = s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "", -1, "")
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{}`), "", -1, "")
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "maintenance", json.RawMessage(`{}`), "", -1, "")
	require.NoError(t, err)

	list, err = s.ListHubSettings(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 3)

	// Should be ordered by section name.
	assert.Equal(t, "access", list[0].Section)
	assert.Equal(t, "maintenance", list[1].Section)
	assert.Equal(t, "telemetry", list[2].Section)
}

// =============================================================================
// Delete
// =============================================================================

func TestDeleteHubSetting(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "", -1, "")
	require.NoError(t, err)

	err = s.DeleteHubSetting(ctx, "access")
	require.NoError(t, err)

	// Should be gone.
	_, err = s.GetHubSetting(ctx, "access")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteHubSetting_NotFound(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	err := s.DeleteHubSetting(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// UpdatedBy clearing
// =============================================================================

func TestUpsertHubSetting_ClearsUpdatedBy(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Create with updatedBy.
	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "admin@test.com", 0, "")
	require.NoError(t, err)

	// Update with empty updatedBy — should clear it.
	got, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "", 1, "")
	require.NoError(t, err)
	assert.Empty(t, got.UpdatedBy)
}

// =============================================================================
// Concurrent CAS race — only one winner
// =============================================================================

func TestUpsertHubSetting_ConcurrentCASRace(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Create the initial row.
	_, err := s.UpsertHubSetting(ctx, "race", json.RawMessage(`{"v":0}`), "setup", 0, "")
	require.NoError(t, err)

	const goroutines = 10
	var (
		wg        sync.WaitGroup
		wins      atomic.Int32
		conflicts atomic.Int32
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			val := json.RawMessage(`{"v":` + string(rune('0'+n)) + `}`)
			_, err := s.UpsertHubSetting(ctx, "race", val, "racer", 1, "")
			if err == nil {
				wins.Add(1)
			} else {
				conflicts.Add(1)
			}
		}(i)
	}
	wg.Wait()

	// Exactly one writer should succeed; the rest should get conflicts.
	assert.Equal(t, int32(1), wins.Load(), "exactly one CAS writer should succeed")
	assert.Equal(t, int32(goroutines-1), conflicts.Load(), "all other writers should get revision conflict")

	// Final revision should be 2 (initial 1 + one successful update).
	final, err := s.GetHubSetting(ctx, "race")
	require.NoError(t, err)
	assert.Equal(t, int64(2), final.Revision)
}

// =============================================================================
// Origin field behavior
// =============================================================================

func TestUpsertHubSetting_OriginSetOnCreate(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	got, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "seed", -1, "seeded")
	require.NoError(t, err)
	assert.Equal(t, "seeded", got.Origin)

	got2, err := s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{}`), "admin@test.com", 0, "managed")
	require.NoError(t, err)
	assert.Equal(t, "managed", got2.Origin)
}

func TestUpsertHubSetting_OriginDefaultsToSeeded(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	got, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "seed", -1, "")
	require.NoError(t, err)
	assert.Equal(t, "seeded", got.Origin)
}

func TestUpsertHubSetting_OriginUpdatedOnWrite(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Create as seeded.
	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{"v":1}`), "seed", -1, "seeded")
	require.NoError(t, err)

	// Admin update flips to managed.
	got, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{"v":2}`), "admin@test.com", 1, "managed")
	require.NoError(t, err)
	assert.Equal(t, "managed", got.Origin)

	// Verify persisted.
	fetched, err := s.GetHubSetting(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, "managed", fetched.Origin)
}

// =============================================================================
// BackfillOrigin
// =============================================================================

func TestBackfillOrigin(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	// Create rows with different updated_by values (all default origin="seeded").
	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "seed", -1, "")
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{}`), "admin@test.com", 0, "")
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "_meta", json.RawMessage(`{}`), "system", -1, "")
	require.NoError(t, err)

	// Run backfill.
	err = s.BackfillOrigin(ctx)
	require.NoError(t, err)

	// "access" (updated_by=seed) → stays seeded.
	access, err := s.GetHubSetting(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, "seeded", access.Origin)

	// "telemetry" (updated_by=admin) → managed.
	telemetry, err := s.GetHubSetting(ctx, "telemetry")
	require.NoError(t, err)
	assert.Equal(t, "managed", telemetry.Origin)

	// "_meta" (exempt) → stays seeded.
	meta, err := s.GetHubSetting(ctx, "_meta")
	require.NoError(t, err)
	assert.Equal(t, "seeded", meta.Origin)
}

func TestBackfillOrigin_Idempotent(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "admin@test.com", 0, "")
	require.NoError(t, err)

	// Run twice.
	require.NoError(t, s.BackfillOrigin(ctx))
	require.NoError(t, s.BackfillOrigin(ctx))

	got, err := s.GetHubSetting(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, "managed", got.Origin)
}
