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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settingsRevision reads the operational-settings revision row (0 when it
// does not exist).
func settingsRevision(t *testing.T, s *HubSettingStore) int64 {
	t.Helper()
	row, err := s.GetHubSetting(context.Background(), store.HubSettingsRevisionSection)
	if err == store.ErrNotFound {
		return 0
	}
	require.NoError(t, err)
	return row.Revision
}

// Every write of an operational section bumps the revision in the same
// transaction and returns it; rows of other sections do not
// (ptone/scion#4457).
func TestHubSettingsRevision_BumpsOnOperationalWritesOnly(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()
	assert.Equal(t, int64(0), settingsRevision(t, s), "no revision before the first operational write")

	// A marker row (not an operational section) does not bump.
	m, err := s.UpsertHubSetting(ctx, "migration_example_marker_v1", json.RawMessage(`{"done":true}`), "migration", 0, "seeded")
	require.NoError(t, err)
	assert.Equal(t, int64(0), m.SettingsRevision)
	assert.Equal(t, int64(0), settingsRevision(t, s))

	// Create, CAS update and unconditional upsert of operational sections.
	a, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "admin@test.com", 0, "managed")
	require.NoError(t, err)
	assert.Equal(t, int64(1), a.SettingsRevision)
	a2, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{"admin_emails":["a@b.com"]}`), "admin@test.com", a.Revision, "managed")
	require.NoError(t, err)
	assert.Equal(t, int64(2), a2.SettingsRevision)
	tel, err := s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{}`), "seed", -1, "seeded")
	require.NoError(t, err)
	assert.Equal(t, int64(3), tel.SettingsRevision)
	assert.Equal(t, int64(3), settingsRevision(t, s))

	// A failed CAS write does not bump.
	_, err = s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "admin@test.com", 1, "managed")
	require.ErrorIs(t, err, store.ErrRevisionConflict)
	assert.Equal(t, int64(3), settingsRevision(t, s))

	// Deleting an operational section bumps; deleting a marker or a missing
	// section does not.
	require.NoError(t, s.DeleteHubSetting(ctx, "telemetry"))
	assert.Equal(t, int64(4), settingsRevision(t, s))
	require.NoError(t, s.DeleteHubSetting(ctx, "migration_example_marker_v1"))
	assert.Equal(t, int64(4), settingsRevision(t, s))
	require.ErrorIs(t, s.DeleteHubSetting(ctx, "telemetry"), store.ErrNotFound)
	assert.Equal(t, int64(4), settingsRevision(t, s))

	// The per-section revision is unchanged in meaning.
	got, err := s.GetHubSetting(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)
}

// Callers cannot write or delete the revision row (ptone/scion#4457).
func TestHubSettingsRevision_RowIsReserved(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	_, err := s.UpsertHubSetting(ctx, store.HubSettingsRevisionSection, json.RawMessage(`{}`), "x", -1, "")
	require.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "x", -1, "")
	require.NoError(t, err)
	require.ErrorIs(t, s.DeleteHubSetting(ctx, store.HubSettingsRevisionSection), store.ErrInvalidInput)
	assert.Equal(t, int64(1), settingsRevision(t, s))
}

// Concurrent operational writes get distinct revisions, and the revision
// counts every write (ptone/scion#4457).
func TestHubSettingsRevision_ConcurrentWritesDistinct(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	sections := []string{"access", "telemetry", "maintenance", "lifecycle", "quotas"}
	const perSection = 4
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		revs = map[int64]bool{}
	)
	for _, sec := range sections {
		for i := 0; i < perSection; i++ {
			wg.Add(1)
			go func(sec string) {
				defer wg.Done()
				got, err := s.UpsertHubSetting(ctx, sec, json.RawMessage(`{}`), "seed", -1, "seeded")
				if !assert.NoError(t, err) {
					return
				}
				mu.Lock()
				assert.False(t, revs[got.SettingsRevision], "revision %d returned twice", got.SettingsRevision)
				revs[got.SettingsRevision] = true
				mu.Unlock()
			}(sec)
		}
	}
	wg.Wait()

	n := int64(len(sections) * perSection)
	assert.Len(t, revs, int(n))
	assert.Equal(t, n, settingsRevision(t, s))
	for r := int64(1); r <= n; r++ {
		assert.True(t, revs[r], "revision %d missing", r)
	}
}

// BackfillOrigin leaves the revision row alone and bumps the revision only
// when it changes the origin of an operational row (ptone/scion#4457).
func TestHubSettingsRevision_BackfillOrigin(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewHubSettingStore(client)
	ctx := context.Background()

	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "seed", -1, "seeded")
	require.NoError(t, err)
	require.Equal(t, int64(1), settingsRevision(t, s))

	// Nothing to backfill: no bump, revision row untouched.
	require.NoError(t, s.BackfillOrigin(ctx))
	row, err := s.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, int64(1), row.Revision)
	assert.Equal(t, "seeded", row.Origin)
	assert.Empty(t, row.UpdatedBy)

	// A marker row written by a migration is backfilled without a bump.
	_, err = client.HubSetting.Create().SetSection("migration_example_marker_v1").
		SetValue(json.RawMessage(`{}`)).SetUpdatedBy("migration").SetOrigin(hubsetting.OriginSeeded).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, s.BackfillOrigin(ctx))
	assert.Equal(t, int64(1), settingsRevision(t, s))

	// An operational row written by an admin but still marked seeded is
	// backfilled to managed with a bump.
	_, err = client.HubSetting.Create().SetSection("telemetry").
		SetValue(json.RawMessage(`{}`)).SetUpdatedBy("admin@test.com").SetOrigin(hubsetting.OriginSeeded).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, s.BackfillOrigin(ctx))
	assert.Equal(t, int64(2), settingsRevision(t, s))
	tel, err := s.GetHubSetting(ctx, "telemetry")
	require.NoError(t, err)
	assert.Equal(t, "managed", tel.Origin)

	row, err = s.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, "seeded", row.Origin, "the revision row is never backfilled")
}
