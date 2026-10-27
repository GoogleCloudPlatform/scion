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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hook"
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

	// The revision row is excluded by name, not by its NULL updated_by: with
	// an updated_by set it is still left alone, and it alone causes no bump
	// (ptone/scion#5666).
	_, err = client.HubSetting.Update().
		Where(hubsetting.SectionEQ(store.HubSettingsRevisionSection)).
		SetUpdatedBy("admin@test.com").
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, s.BackfillOrigin(ctx))
	row, err = s.GetHubSetting(ctx, store.HubSettingsRevisionSection)
	require.NoError(t, err)
	assert.Equal(t, "seeded", row.Origin, "the revision row is excluded by name")
	assert.Equal(t, int64(2), row.Revision, "no bump when only the revision row matches the other conditions")

	// An operational and a non-operational row backfilled together give one
	// bump.
	for _, sec := range []string{"lifecycle", "migration_example_marker_v2"} {
		_, err = client.HubSetting.Create().SetSection(sec).
			SetValue(json.RawMessage(`{}`)).SetUpdatedBy("admin@test.com").SetOrigin(hubsetting.OriginSeeded).Save(ctx)
		require.NoError(t, err)
	}
	require.NoError(t, s.BackfillOrigin(ctx))
	assert.Equal(t, int64(3), settingsRevision(t, s))
	for _, sec := range []string{"lifecycle", "migration_example_marker_v2"} {
		got, err := s.GetHubSetting(ctx, sec)
		require.NoError(t, err)
		assert.Equal(t, "managed", got.Origin, sec)
	}
}

// DeleteHubSettingRevision returns the revision its own transaction bumped
// to, and the composite store promotes it (ptone/scion#4457).
func TestHubSettingsRevision_DeleteReturnsRevision(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()

	_, err := s.UpsertHubSetting(ctx, "access", json.RawMessage(`{}`), "x", -1, "managed")
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "migration_example_marker_v1", json.RawMessage(`{}`), "migration", -1, "seeded")
	require.NoError(t, err)

	rev, err := s.DeleteHubSettingRevision(ctx, "access")
	require.NoError(t, err)
	assert.Equal(t, int64(2), rev)
	assert.Equal(t, int64(2), settingsRevision(t, s))

	rev, err = s.DeleteHubSettingRevision(ctx, "migration_example_marker_v1")
	require.NoError(t, err)
	assert.Equal(t, int64(0), rev, "a non-operational delete does not bump")
	_, err = s.DeleteHubSettingRevision(ctx, "access")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.DeleteHubSettingRevision(ctx, store.HubSettingsRevisionSection)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	var st store.Store = NewCompositeStore(enttest.NewClient(t))
	_, ok := st.(store.HubSettingRevisionDeleter)
	assert.True(t, ok, "the composite store promotes DeleteHubSettingRevision")
}

// A real duplicate-section insert on SQLite is classified as a section
// conflict, so the -1 retry sees it (ptone/scion#5772).
func TestIsHubSettingSectionConflict_SQLiteDuplicateSection(t *testing.T) {
	client := enttest.NewClient(t)
	ctx := context.Background()
	_, err := client.HubSetting.Create().SetSection("lifecycle").SetValue(json.RawMessage(`{}`)).Save(ctx)
	require.NoError(t, err)
	_, err = client.HubSetting.Create().SetSection("lifecycle").SetValue(json.RawMessage(`{}`)).Save(ctx)
	require.Error(t, err)
	assert.True(t, isHubSettingSectionConflict(err), "duplicate section: %v", err)
}

// BackfillOrigin partitions its two updates by section name, not by the
// rows its first read found: an operational row that appears after that
// read (committed by another writer between the statements) is updated by
// the operational update, with a bump, never silently by the other one
// (ptone/scion#5666 review). The hook stands in for the other writer by
// inserting the row just before the first update runs.
func TestHubSettingsRevision_BackfillOriginRowAppearingBetweenStatements(t *testing.T) {
	client := enttest.NewClient(t)
	s := NewHubSettingStore(client)
	ctx := context.Background()

	_, err := client.HubSetting.Create().SetSection("migration_example_marker_v1").
		SetValue(json.RawMessage(`{}`)).SetUpdatedBy("migration").SetOrigin(hubsetting.OriginSeeded).Save(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), settingsRevision(t, s))

	inserted := false
	client.HubSetting.Use(func(next ent.Mutator) ent.Mutator {
		return hook.HubSettingFunc(func(ctx context.Context, m *ent.HubSettingMutation) (ent.Value, error) {
			if !inserted && m.Op().Is(ent.OpUpdate) {
				inserted = true
				if _, err := m.Client().HubSetting.Create().SetSection("lifecycle").
					SetValue(json.RawMessage(`{}`)).SetUpdatedBy("admin@test.com").SetOrigin(hubsetting.OriginSeeded).Save(ctx); err != nil {
					return nil, err
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	require.NoError(t, s.BackfillOrigin(ctx))
	require.True(t, inserted, "the hook ran before the first update")
	got, err := s.GetHubSetting(ctx, "lifecycle")
	require.NoError(t, err)
	assert.Equal(t, "managed", got.Origin)
	assert.Equal(t, int64(1), settingsRevision(t, s), "the operational row's backfill bumps the revision")
	marker, err := s.GetHubSetting(ctx, "migration_example_marker_v1")
	require.NoError(t, err)
	assert.Equal(t, "managed", marker.Origin)
}
