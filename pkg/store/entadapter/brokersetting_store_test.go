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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBrokerSettingStore(t *testing.T) *BrokerSettingStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewBrokerSettingStore(client)
}

func int64ptr(v int64) *int64 { return &v }

// =============================================================================
// Dialect detection (GoogleCloudPlatform/scion#2126 review)
// =============================================================================

// TestUsesRowLocks_ReflectsBackend pins usesRowLocks to the actual test
// backend rather than deriving the expectation from the same
// client.Driver().Dialect() call the implementation uses (which would let a
// wrong comparison, e.g. against the wrong dialect constant, pass unnoticed).
// It runs unconditionally: under the default `go test` build this is SQLite
// (want false); under `-tags integration` with SCION_TEST_POSTGRES_URL set,
// enttest.Active() reports the harness switched to a real Postgres backend
// (want true). Either way the assertion is independent of usesRowLocks'
// own logic.
func TestUsesRowLocks_ReflectsBackend(t *testing.T) {
	s := newTestBrokerSettingStore(t)

	got := s.usesRowLocks(context.Background())

	if enttest.Active() {
		assert.True(t, got, "integration harness backend is Postgres; usesRowLocks must report true so PutBrokerSettings takes the FOR UPDATE path")
	} else {
		assert.False(t, got, "default test harness backend is SQLite; usesRowLocks must report false so PutBrokerSettings does not attempt FOR UPDATE")
	}
}

// =============================================================================
// Get (missing)
// =============================================================================

func TestGetBrokerSettings_NotFound(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	_, err := s.GetBrokerSettings(ctx, "nonexistent-broker")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// =============================================================================
// Create (expectedRevision == 0)
// =============================================================================

func TestPutBrokerSettings_Create(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	got, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(30)}, 0, "admin@test.com")
	require.NoError(t, err)

	assert.Equal(t, "broker-1", got.BrokerID)
	require.NotNil(t, got.Settings.MaxAgents)
	assert.EqualValues(t, 30, *got.Settings.MaxAgents)
	assert.Equal(t, int64(1), got.Revision)
	assert.Equal(t, "admin@test.com", got.UpdatedBy)
	assert.False(t, got.Updated.IsZero())

	fetched, err := s.GetBrokerSettings(ctx, "broker-1")
	require.NoError(t, err)
	assert.Equal(t, got.Revision, fetched.Revision)
	require.NotNil(t, fetched.Settings.MaxAgents)
	assert.EqualValues(t, 30, *fetched.Settings.MaxAgents)
}

// =============================================================================
// Create-only conflict (expectedRevision == 0, row exists)
// =============================================================================

func TestPutBrokerSettings_CreateOnly_Conflict(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	_, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)

	_, err = s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(6)}, 0, "other@test.com")
	assert.ErrorIs(t, err, store.ErrRevisionConflict)
}

// =============================================================================
// Update with revision bump (expectedRevision > 0)
// =============================================================================

func TestPutBrokerSettings_UpdateWithRevisionBump(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	created, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)
	assert.Equal(t, int64(1), created.Revision)

	updated, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(3)}, 1, "admin2@test.com")
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Revision)
	require.NotNil(t, updated.Settings.MaxAgents)
	assert.EqualValues(t, 3, *updated.Settings.MaxAgents)
	assert.Equal(t, "admin2@test.com", updated.UpdatedBy)
}

// =============================================================================
// CAS conflict (wrong expectedRevision)
// =============================================================================

func TestPutBrokerSettings_CAS_Conflict(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	_, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)

	_, err = s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(6)}, 99, "admin@test.com")
	assert.ErrorIs(t, err, store.ErrRevisionConflict)
}

// =============================================================================
// CAS update on non-existent row (expectedRevision > 0, row missing)
// =============================================================================

func TestPutBrokerSettings_CAS_MissingRow(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	_, err := s.PutBrokerSettings(ctx, "nonexistent-broker", store.BrokerSettings{MaxAgents: int64ptr(5)}, 5, "admin@test.com")
	assert.ErrorIs(t, err, store.ErrRevisionConflict)
}

// =============================================================================
// Clearing a setting (nil = inherit) and full-replace semantics
// =============================================================================

func TestPutBrokerSettings_ClearMaxAgents(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	created, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)

	cleared, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: nil}, created.Revision, "admin@test.com")
	require.NoError(t, err)
	assert.Nil(t, cleared.Settings.MaxAgents, "nil MaxAgents means inherit")
}

// =============================================================================
// Delete
// =============================================================================

func TestDeleteBrokerSettings(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	_, err := s.PutBrokerSettings(ctx, "broker-1", store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)

	require.NoError(t, s.DeleteBrokerSettings(ctx, "broker-1"))

	_, err = s.GetBrokerSettings(ctx, "broker-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestDeleteBrokerSettings_NotFound pins that deleting a broker with no
// settings row is a no-op, not an error: DeleteRuntimeBroker calls this
// unconditionally (pkg/store/entadapter/project_store.go) and must not fail
// just because the broker never had a settings row.
func TestDeleteBrokerSettings_NotFound(t *testing.T) {
	s := newTestBrokerSettingStore(t)
	ctx := context.Background()

	err := s.DeleteBrokerSettings(ctx, "nonexistent-broker")
	assert.NoError(t, err)
}

// =============================================================================
// DeleteRuntimeBroker cleans up the settings row (ptone/scion#2061 P2, AC-P2-4)
// =============================================================================

func TestDeleteRuntimeBroker_DeletesBrokerSettings(t *testing.T) {
	client := enttest.NewClient(t)
	projectStore := NewProjectStore(client)
	settingStore := NewBrokerSettingStore(client)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     "11111111-1111-1111-1111-111111111111",
		Name:   "Broker One",
		Slug:   "broker-one",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, projectStore.CreateRuntimeBroker(ctx, broker))

	_, err := settingStore.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)

	require.NoError(t, projectStore.DeleteRuntimeBroker(ctx, broker.ID))

	_, err = settingStore.GetBrokerSettings(ctx, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "deleting the broker must delete its settings row")
}

// TestDeleteRuntimeBroker_NoSettingsRow pins that deleting a broker that
// never had a settings row still succeeds (the explicit cleanup delete in
// DeleteRuntimeBroker must tolerate ErrNotFound rather than failing the
// whole broker delete).
func TestDeleteRuntimeBroker_NoSettingsRow(t *testing.T) {
	client := enttest.NewClient(t)
	projectStore := NewProjectStore(client)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     "22222222-2222-2222-2222-222222222222",
		Name:   "Broker Two",
		Slug:   "broker-two",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, projectStore.CreateRuntimeBroker(ctx, broker))

	assert.NoError(t, projectStore.DeleteRuntimeBroker(ctx, broker.ID))
}

// TestDeleteRuntimeBroker_NonCanonicalID_DeletesBrokerSettings pins AC-P2-4
// against a non-canonical delete path (review round 2, R3): deleting via an
// uppercase form of the broker's ID must still delete the settings row,
// which is always stored under the canonical (lowercase) ID.
func TestDeleteRuntimeBroker_NonCanonicalID_DeletesBrokerSettings(t *testing.T) {
	client := enttest.NewClient(t)
	projectStore := NewProjectStore(client)
	settingStore := NewBrokerSettingStore(client)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     "33333333-3333-3333-3333-333333333333",
		Name:   "Broker Three",
		Slug:   "broker-three",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, projectStore.CreateRuntimeBroker(ctx, broker))

	_, err := settingStore.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: int64ptr(5)}, 0, "admin@test.com")
	require.NoError(t, err)

	require.NoError(t, projectStore.DeleteRuntimeBroker(ctx, strings.ToUpper(broker.ID)))

	_, err = settingStore.GetBrokerSettings(ctx, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "deleting via a non-canonical ID must still delete the canonical settings row")
}
