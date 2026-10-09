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
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// authorityCeilingColumns lists the authority ceiling columns shared by
// schedules and scheduled_events. A database that predates them has none.
var authorityCeilingColumns = []string{
	"authority_ceiling_kind", "authority_ceiling_version",
	"authority_ceiling_permission_ids", "authority_ceiling_boundary_kind",
	"authority_ceiling_boundary_project_id", "authority_ceiling_source_expires_at",
}

func testBoundedAuthorityCeiling(projectID string) store.EffectCeiling {
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           permissions.CeilingVersionV1,
		PermissionIDs:     []string{"agent.create", "project.read"},
		BoundaryKind:      "project",
		BoundaryProjectID: projectID,
		SourceExpiresAt:   &expires,
	}
}

func testRecordedAttribution(revision int) store.InitiatorAttribution {
	return store.InitiatorAttribution{
		InitiatorPrincipalKind:  "user",
		InitiatorPrincipalID:    "user-1",
		InitiatorCredentialKind: store.InitiatorCredentialKindSession,
		AttributionVersion:      1,
		AuthorizationRevision:   revision,
	}
}

func assertCeilingEqual(t *testing.T, want, got store.EffectCeiling) {
	t.Helper()
	assert.Equal(t, want.Kind, got.Kind)
	assert.Equal(t, want.Version, got.Version)
	assert.Equal(t, want.PermissionIDs, got.PermissionIDs)
	assert.Equal(t, want.BoundaryKind, got.BoundaryKind)
	assert.Equal(t, want.BoundaryProjectID, got.BoundaryProjectID)
	if want.SourceExpiresAt == nil {
		assert.Nil(t, got.SourceExpiresAt)
	} else if assert.NotNil(t, got.SourceExpiresAt) {
		assert.True(t, want.SourceExpiresAt.Equal(*got.SourceExpiresAt))
	}
}

// TestScheduleStoreRoundTripsAuthorityCeiling checks that the authority
// ceiling of a schedule and of a scheduled event survives a write and a
// read, for every kind, and that an empty bounded list stays distinct from
// "no list".
func TestScheduleStoreRoundTripsAuthorityCeiling(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduleStore(t)
	projectID := uuid.NewString()

	cases := map[string]store.EffectCeiling{
		"bounded":       testBoundedAuthorityCeiling(projectID),
		"empty bounded": {Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{}},
		"principal":     {Kind: store.EffectCeilingPrincipal},
		"unrecorded":    {},
	}
	for name, ceiling := range cases {
		t.Run(name, func(t *testing.T) {
			sc := newTestSchedule(projectID, "sched-"+strings.ReplaceAll(name, " ", "-"))
			sc.AuthorityCeiling = ceiling
			require.NoError(t, s.CreateSchedule(ctx, sc))
			gotSched, err := s.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			assertCeilingEqual(t, ceiling, gotSched.AuthorityCeiling)

			evt := &store.ScheduledEvent{
				ID: uuid.NewString(), ProjectID: projectID, EventType: "message",
				FireAt: time.Now().Add(time.Hour), Payload: "{}",
				AuthorityCeiling: ceiling,
			}
			require.NoError(t, s.CreateScheduledEvent(ctx, evt))
			gotEvt, err := s.GetScheduledEvent(ctx, evt.ID)
			require.NoError(t, err)
			assertCeilingEqual(t, ceiling, gotEvt.AuthorityCeiling)
		})
	}
}

// TestUpdateScheduleWritesCeilingWithAttribution checks the store contract:
// a re-attribution writes the ceiling in the same conditional update, the
// ceiling replaces (never merges with) the previous one, and an update
// without attribution leaves the ceiling columns alone.
func TestUpdateScheduleWritesCeilingWithAttribution(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduleStore(t)
	projectID := uuid.NewString()

	sc := newTestSchedule(projectID, "ceiling-update")
	sc.InitiatorAttribution = testRecordedAttribution(1)
	sc.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, s.CreateSchedule(ctx, sc))

	// Re-attribution with a bounded ceiling.
	bounded := testBoundedAuthorityCeiling(projectID)
	sc.AuthorityCeiling = bounded
	attr := testRecordedAttribution(2)
	require.NoError(t, s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{}, 1, true, &attr))
	got, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assertCeilingEqual(t, bounded, got.AuthorityCeiling)
	assert.Equal(t, 2, got.AuthorizationRevision)

	// An update without attribution does not touch the ceiling, even when
	// the struct passed in carries a different one.
	sc.Name = "ceiling-update-renamed"
	sc.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{Name: true}, 2, true, nil))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, "ceiling-update-renamed", got.Name)
	assertCeilingEqual(t, bounded, got.AuthorityCeiling)
	assert.Equal(t, 2, got.AuthorizationRevision)

	// A re-attribution to principal clears the bounded list and expiry.
	sc.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	attr = testRecordedAttribution(3)
	require.NoError(t, s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{}, 2, true, &attr))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assertCeilingEqual(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, got.AuthorityCeiling)

	// A re-attribution that omits the ceiling writes the unrecorded zero
	// value, never the previous ceiling.
	sc.AuthorityCeiling = store.EffectCeiling{}
	attr = testRecordedAttribution(4)
	require.NoError(t, s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{}, 3, true, &attr))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assertCeilingEqual(t, store.EffectCeiling{}, got.AuthorityCeiling)

	// A stale revision writes nothing, the ceiling included.
	sc.AuthorityCeiling = bounded
	attr = testRecordedAttribution(5)
	err = s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{}, 3, true, &attr)
	require.ErrorIs(t, err, store.ErrRevisionConflict)
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assertCeilingEqual(t, store.EffectCeiling{}, got.AuthorityCeiling)
	assert.Equal(t, 4, got.AuthorizationRevision)
}

// TestScheduleStoreRejectsInvalidAuthorityCeiling checks that a ceiling the
// store cannot persist faithfully is rejected before any write.
func TestScheduleStoreRejectsInvalidAuthorityCeiling(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduleStore(t)
	projectID := uuid.NewString()
	invalid := []store.EffectCeiling{
		{Kind: "wide"},
		{Kind: store.EffectCeilingPrincipal, PermissionIDs: []string{"agent.create"}},
		{PermissionIDs: []string{}},
	}
	for _, c := range invalid {
		sc := newTestSchedule(projectID, "invalid-"+uuid.NewString())
		sc.AuthorityCeiling = c
		require.ErrorIs(t, s.CreateSchedule(ctx, sc), store.ErrInvalidInput)
		_, err := s.GetSchedule(ctx, sc.ID)
		require.ErrorIs(t, err, store.ErrNotFound)

		evt := &store.ScheduledEvent{
			ID: uuid.NewString(), ProjectID: projectID, EventType: "message",
			FireAt: time.Now().Add(time.Hour), Payload: "{}", AuthorityCeiling: c,
		}
		require.ErrorIs(t, s.CreateScheduledEvent(ctx, evt), store.ErrInvalidInput)
	}

	sc := newTestSchedule(projectID, "valid")
	sc.InitiatorAttribution = testRecordedAttribution(1)
	sc.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, s.CreateSchedule(ctx, sc))
	sc.AuthorityCeiling = invalid[1]
	attr := testRecordedAttribution(2)
	require.ErrorIs(t, s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{}, 1, true, &attr), store.ErrInvalidInput)
	got, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.AuthorizationRevision)
	assertCeilingEqual(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, got.AuthorityCeiling)
}

const (
	legacyScheduleID      = "33333333-3333-3333-3333-333333333333"
	legacyScheduledEvtID  = "44444444-4444-4444-4444-444444444444"
	legacyScheduleProject = "55555555-5555-5555-5555-555555555555"
)

// insertLegacyScheduleRows inserts one schedule and one scheduled event the
// way a schema without the authority ceiling columns wrote them.
func insertLegacyScheduleRows(t *testing.T, db *sql.DB, placeholders func(int) string) {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx,
		"INSERT INTO schedules (id, project_id, name, cron_expr, event_type, payload, status, run_count, error_count, created_by, created, updated) "+
			"VALUES ("+placeholders(12)+")",
		legacyScheduleID, legacyScheduleProject, "legacy", "0 9 * * *", "dispatch_agent", "{}", "active", 0, 0, "user-legacy",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		"INSERT INTO scheduled_events (id, project_id, event_type, fire_at, payload, status, created_by, created) "+
			"VALUES ("+placeholders(8)+")",
		legacyScheduledEvtID, legacyScheduleProject, "dispatch_agent", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "{}", "pending", "user-legacy",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
}

// assertLegacyScheduleRowsUnrecorded reads the pre-existing rows back
// through the store and checks that their authority ceiling is unrecorded.
func assertLegacyScheduleRowsUnrecorded(t *testing.T, client *ent.Client) {
	t.Helper()
	ctx := context.Background()
	s := NewScheduleStore(client)
	sc, err := s.GetSchedule(ctx, legacyScheduleID)
	require.NoError(t, err)
	assert.Equal(t, store.EffectCeiling{}, sc.AuthorityCeiling)
	assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, sc.InitiatorCredentialKind)
	_, ok := sc.AuthorityCeiling.Frozen()
	assert.False(t, ok, "an unrecorded ceiling must not yield a frozen ceiling")

	evt, err := s.GetScheduledEvent(ctx, legacyScheduledEvtID)
	require.NoError(t, err)
	assert.Equal(t, store.EffectCeiling{}, evt.AuthorityCeiling)
	assert.Equal(t, store.InitiatorCredentialKindLegacyUnknown, evt.InitiatorCredentialKind)
}

func dropAuthorityCeilingColumns(t *testing.T, db *sql.DB, perColumn bool) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"schedules", "scheduled_events"} {
		if perColumn {
			for _, c := range authorityCeilingColumns {
				_, err := db.ExecContext(ctx, "ALTER TABLE "+table+" DROP COLUMN "+c)
				require.NoError(t, err)
			}
			continue
		}
		drops := make([]string, len(authorityCeilingColumns))
		for i, c := range authorityCeilingColumns {
			drops[i] = "DROP COLUMN " + c
		}
		_, err := db.ExecContext(ctx, "ALTER TABLE "+table+" "+strings.Join(drops, ", "))
		require.NoError(t, err)
	}
}

// TestMigrationExistingSchedulesUnrecorded returns the schedules and
// scheduled_events tables to their shape before the authority ceiling
// columns, inserts a row in each the way that schema wrote them, migrates,
// and checks that both rows read back with an unrecorded ceiling. The SQLite
// case runs in the SQLite build. The Postgres case runs under -tags
// integration with SCION_TEST_POSTGRES_URL set, as the Postgres CI job
// (make test-launch-store-postgres) runs the entadapter suite.
func TestMigrationExistingSchedulesUnrecorded(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		enttest.SkipOnPostgres(t, "the SQLite case runs in the SQLite build")
		ctx := context.Background()
		dsn := "file:" + filepath.Join(t.TempDir(), "test.db")

		client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		require.NoError(t, entc.AutoMigrate(ctx, client))
		require.NoError(t, client.Close())

		raw, err := sql.Open("sqlite", dsn)
		require.NoError(t, err)
		dropAuthorityCeilingColumns(t, raw, true)
		insertLegacyScheduleRows(t, raw, func(n int) string {
			return strings.TrimSuffix(strings.Repeat("?,", n), ",")
		})
		require.NoError(t, raw.Close())

		client, err = entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, entc.AutoMigrate(ctx, client))

		assertLegacyScheduleRowsUnrecorded(t, client)
	})

	t.Run("postgres", func(t *testing.T) {
		if !enttest.Active() {
			t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres migration case")
		}
		ctx := context.Background()
		dsn := enttest.NewSchemaURL(t)
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })

		dropAuthorityCeilingColumns(t, db, false)
		insertLegacyScheduleRows(t, db, func(n int) string {
			ps := make([]string, n)
			for i := range ps {
				ps[i] = fmt.Sprintf("$%d", i+1)
			}
			return strings.Join(ps, ",")
		})

		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close() })
		require.NoError(t, entc.AutoMigrate(ctx, client))

		assertLegacyScheduleRowsUnrecorded(t, client)
	})
}
