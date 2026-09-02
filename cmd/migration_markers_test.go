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

package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrationMarker_AbsentDoc verifies that IsMigrationComplete returns
// false when the _migrations section does not exist at all.
func TestMigrationMarker_AbsentDoc(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.False(t, done, "absent _migrations section should report not complete")
}

// TestMigrationMarker_RoundTrip verifies the basic write-then-read path:
// writing a marker succeeds, and subsequent reads report the migration as
// complete.
func TestMigrationMarker_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Not complete yet.
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.False(t, done)

	// Mark complete with zero residuals.
	err = MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err)

	// Now complete.
	done, err = IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.True(t, done, "migration should be complete after marking")
}

// TestMigrationMarker_ResidualsPersisted verifies that the residual count
// is recorded in the marker document. Row-level refusals are a permanent
// outcome, not an error that blocks the marker (M-1').
func TestMigrationMarker_ResidualsPersisted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Mark complete with residuals (row-level refusals).
	err := MarkMigrationComplete(ctx, s, "dm_key_migration", 42)
	require.NoError(t, err)

	// Should still be marked complete — residuals do not block the marker.
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.True(t, done, "marker must be written even with residuals (M-1')")

	// Verify the residual count is persisted.
	hs, err := s.GetHubSetting(ctx, migrationsSectionName)
	require.NoError(t, err)

	var doc migrationsDoc
	err = json.Unmarshal(hs.Value, &doc)
	require.NoError(t, err)
	require.NotNil(t, doc.DMKeyMigration)
	assert.Equal(t, 42, doc.DMKeyMigration.Residuals,
		"residual count must be persisted in the marker")
}

// TestMigrationMarker_MalformedDoc verifies that a structurally unexpected
// _migrations document is treated as "not complete" (the safe direction:
// retry). The store validates JSON syntax, so we use valid JSON that does
// not match the migrationsDoc schema — a string instead of an object.
func TestMigrationMarker_MalformedDoc(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Write a JSON string — valid JSON but cannot unmarshal to migrationsDoc.
	badShape := json.RawMessage(`"this is a string, not an object"`)
	_, err := s.UpsertHubSetting(ctx, migrationsSectionName, badShape, "test", -1, "seeded")
	require.NoError(t, err)

	// Should report not complete (safe direction).
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.False(t, done, "structurally unexpected doc must be treated as not complete (retry)")
}

// TestMigrationMarker_MalformedDocOverwritten verifies that a marker write
// over a structurally unexpected document succeeds — the upsert overwrites
// the corrupt value with a well-formed one.
func TestMigrationMarker_MalformedDocOverwritten(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Write a JSON string — valid JSON but wrong shape.
	badShape := json.RawMessage(`"this is a string, not an object"`)
	_, err := s.UpsertHubSetting(ctx, migrationsSectionName, badShape, "test", -1, "seeded")
	require.NoError(t, err)

	// Mark complete — should overwrite the bad-shape value.
	err = MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err)

	// Now complete.
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.True(t, done, "marker should be readable after overwriting malformed doc")
}

// TestMigrationMarker_IndependentMigrations verifies that marking one
// migration complete does not affect another.
func TestMigrationMarker_IndependentMigrations(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Mark DM migration complete.
	err := MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err)

	// DM migration is complete.
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.True(t, done)

	// Backfill is NOT complete.
	done, err = IsMigrationComplete(ctx, s, "message_backfill")
	require.NoError(t, err)
	assert.False(t, done, "marking dm_key_migration must not affect message_backfill")
}

// TestMigrationMarker_UnknownMigrationName verifies that querying an unknown
// migration name returns not-complete rather than erroring.
func TestMigrationMarker_UnknownMigrationName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	done, err := IsMigrationComplete(ctx, s, "nonexistent_migration")
	require.NoError(t, err)
	assert.False(t, done, "unknown migration name should report not complete")
}

// TestMigrationMarker_DoubleWrite verifies that writing the same marker
// twice (concurrent replicas, or a retry after a missed marker) does not
// produce an error. The upsert must be conflict-safe on its own merits
// because the advisory lock is a no-op on SQLite (design F5).
func TestMigrationMarker_DoubleWrite(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// First write.
	err := MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err)

	// Second write (simulates a replica that didn't see the first).
	err = MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err, "double-write must not error (conflict-safe upsert)")

	// Still complete.
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.True(t, done)
}

// TestMigrationMarker_DocShape verifies the persisted JSON matches the
// design's document shape (§4.2).
func TestMigrationMarker_DocShape(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	err := MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err)

	// Read the raw document.
	hs, err := s.GetHubSetting(ctx, migrationsSectionName)
	require.NoError(t, err)

	// Unmarshal to a generic map to verify shape.
	var raw map[string]interface{}
	err = json.Unmarshal(hs.Value, &raw)
	require.NoError(t, err)

	dmSection, ok := raw["dm_key_migration"]
	require.True(t, ok, "document must have dm_key_migration key")

	dmMap, ok := dmSection.(map[string]interface{})
	require.True(t, ok, "dm_key_migration must be an object")

	_, hasCompleted := dmMap["completed_at"]
	assert.True(t, hasCompleted, "dm_key_migration must have completed_at field")
}

// TestMigrationMarker_BothMigrations verifies that both migrations can
// be independently completed in the same document.
func TestMigrationMarker_BothMigrations(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Mark both complete.
	err := MarkMigrationComplete(ctx, s, "dm_key_migration", 0)
	require.NoError(t, err)
	err = MarkMigrationComplete(ctx, s, "message_backfill", 5)
	require.NoError(t, err)

	// Both should report complete.
	done, err := IsMigrationComplete(ctx, s, "dm_key_migration")
	require.NoError(t, err)
	assert.True(t, done, "dm_key_migration should be complete")

	done, err = IsMigrationComplete(ctx, s, "message_backfill")
	require.NoError(t, err)
	assert.True(t, done, "message_backfill should be complete")

	// Verify the document has both keys.
	hs, err := s.GetHubSetting(ctx, migrationsSectionName)
	require.NoError(t, err)

	var doc migrationsDoc
	err = json.Unmarshal(hs.Value, &doc)
	require.NoError(t, err)
	require.NotNil(t, doc.DMKeyMigration)
	require.NotNil(t, doc.MessageBackfill)
	assert.NotNil(t, doc.DMKeyMigration.CompletedAt)
	assert.NotNil(t, doc.MessageBackfill.CompletedAt)
	assert.Equal(t, 5, doc.MessageBackfill.Residuals)
}
