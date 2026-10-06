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
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawTimeColumn inspects a time column's raw SQLite storage, per the tz-lead
// design constraint: scanning a time column into a Go string is misleading
// (the modernc SQLite driver round-trips TEXT through its own layout), so
// assertions here use typeof()/quote() against the raw driver connection
// instead.
func rawTimeColumn(t *testing.T, s *ScheduleStore, table, column, id string) (typ, val string) {
	t.Helper()
	drv, ok := s.client.Driver().(interface{ DB() *sql.DB })
	require.True(t, ok, "driver does not expose *sql.DB for raw inspection")
	row := drv.DB().QueryRow(
		"SELECT typeof("+column+"), quote("+column+") FROM "+table+" WHERE id = ?", id, //nolint:gosec // table/column are fixed literals from this file, id is parameterized
	)
	require.NoError(t, row.Scan(&typ, &val))
	return typ, val
}

// TestCreateScheduledEvent_OffsetFireAtRoundTrips reproduces ptone/scion#2473:
// a fireAt parsed from an offset RFC 3339 timestamp carries a nameless
// FixedZone. Without UTC normalisation at the store boundary, the modernc
// SQLite driver (v1.53.0) persisted that zone's numeric abbreviation
// (e.g. "+0200 +0200") as literal TEXT, which its own read-side layout could
// not parse back — breaking Get and List for the row (and, through ent, the
// whole query). entc.OpenSQLite's UTC handling (GoogleCloudPlatform/scion#2252)
// fixes this; the test fails with a Scan error if that handling is removed.
func TestCreateScheduledEvent_OffsetFireAtRoundTrips(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	fireAt, err := time.Parse(time.RFC3339, "2026-10-02T10:00:00+02:00")
	require.NoError(t, err)

	evt := &store.ScheduledEvent{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    fireAt,
		Payload:   `{"text":"hello"}`,
		CreatedBy: "user-123",
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	// The raw column must hold a UTC-normalised value, not the offset zone's
	// numeric abbreviation. SQLite only: Postgres stores timestamptz, so there
	// is no zone text to inspect; the round trips below run on both.
	if !enttest.Active() {
		typ, val := rawTimeColumn(t, s, "scheduled_events", "fire_at", evt.ID)
		assert.Equal(t, "text", typ)
		assert.Equal(t, "'2026-10-02 08:00:00 +0000 UTC'", val)
	}

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.True(t, fireAt.Equal(got.FireAt), "fireAt instant must round-trip: want %v got %v", fireAt, got.FireAt)

	listRes, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, listRes.Items, 1)
	assert.True(t, fireAt.Equal(listRes.Items[0].FireAt))
}

// withLocal temporarily replaces time.Local, restoring the original on
// cleanup. Used to simulate hubs running under a non-UTC system timezone.
func withLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

// nonUTCZones are the time.Local settings the timezone tests run under:
// Asia/Kathmandu has no letter abbreviation (its Zone() name is the
// numeric "+0545", which the SQLite driver cannot parse back), and
// Asia/Tokyo has a named abbreviation ("JST") that reads back fine but
// must still be stored as UTC text so TEXT comparisons stay correct.
var nonUTCZones = []string{"Asia/Kathmandu", "Asia/Tokyo"}

// forEachNonUTCZone runs fn in a subtest per nonUTCZones entry, with
// time.Local replaced for the subtest's duration.
func forEachNonUTCZone(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, zone := range nonUTCZones {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			require.NoError(t, err)
			withLocal(t, loc)
			fn(t)
		})
	}
}

// TestListDueSchedules_NonUTCLocal checks that ListDueSchedules returns
// exactly the due, active schedules under non-UTC time.Local settings,
// which requires "now" to be bound into the WHERE predicate as UTC text.
func TestListDueSchedules_NonUTCLocal(t *testing.T) {
	forEachNonUTCZone(t, func(t *testing.T) {
		s := newTestScheduleStore(t)
		ctx := context.Background()
		projectID := uuid.NewString()

		// now is deliberately NOT normalised by the test — it is
		// time.Now() under the replaced time.Local, mirroring a
		// scheduler loop running on a host in this zone.
		now := time.Now()

		due := newTestSchedule(projectID, "due")
		dueAt := now.Add(-time.Hour)
		due.NextRunAt = &dueAt
		require.NoError(t, s.CreateSchedule(ctx, due))

		notDue := newTestSchedule(projectID, "not-due")
		notDueAt := now.Add(time.Hour)
		notDue.NextRunAt = &notDueAt
		require.NoError(t, s.CreateSchedule(ctx, notDue))

		result, err := s.ListDueSchedules(ctx, now)
		require.NoError(t, err)
		require.Len(t, result, 1, "exactly the one due schedule must be returned")
		assert.Equal(t, due.ID, result[0].ID)
	})
}

// assertUTCColumn asserts that a time column holds UTC-normalised text. It
// is a no-op on Postgres, whose timestamptz columns hold instants with no
// stored zone text (rawTimeColumn's typeof()/quote() are SQLite functions);
// the callers' read-back assertions still run there.
func assertUTCColumn(t *testing.T, s *ScheduleStore, table, column, id string) {
	t.Helper()
	if enttest.Active() {
		return
	}
	typ, val := rawTimeColumn(t, s, table, column, id)
	assert.Equal(t, "text", typ, "%s.%s storage type", table, column)
	assert.True(t, strings.HasSuffix(val, " +0000 UTC'"),
		"%s.%s must be stored as UTC text, got %s", table, column, val)
}

// TestScheduleStoreWritesUTC_NonUTCLocal runs every schedule store
// mutator that writes a time column under a non-UTC time.Local, with
// local-zone inputs, then checks each written column's raw storage and
// that the row still reads back.
func TestScheduleStoreWritesUTC_NonUTCLocal(t *testing.T) {
	ctx := context.Background()

	// createSchedule/createEvent use the fixtures' UTC next_run_at/fire_at;
	// created/updated come from ent defaults, so each case asserts only the
	// columns its mutator writes.
	createSchedule := func(t *testing.T, s *ScheduleStore) *store.Schedule {
		t.Helper()
		sc := newTestSchedule(uuid.NewString(), "tz")
		require.NoError(t, s.CreateSchedule(ctx, sc))
		return sc
	}
	createEvent := func(t *testing.T, s *ScheduleStore) *store.ScheduledEvent {
		t.Helper()
		ev := newTestScheduledEvent(uuid.NewString())
		require.NoError(t, s.CreateScheduledEvent(ctx, ev))
		return ev
	}

	scheduleCases := []struct {
		name    string
		columns []string
		mutate  func(t *testing.T, s *ScheduleStore) string
	}{
		{
			name:    "CreateSchedule local times",
			columns: []string{"next_run_at", "last_run_at", "created", "updated"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				now := time.Now()
				next, last := now.Add(time.Hour), now.Add(-time.Hour)
				sc := newTestSchedule(uuid.NewString(), "tz")
				sc.NextRunAt, sc.LastRunAt = &next, &last
				sc.CreatedAt, sc.UpdatedAt = now, now
				require.NoError(t, s.CreateSchedule(ctx, sc))
				return sc.ID
			},
		},
		{
			name:    "CreateSchedule default created/updated",
			columns: []string{"created", "updated"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				return createSchedule(t, s).ID
			},
		},
		{
			name:    "UpdateSchedule next_run_at",
			columns: []string{"next_run_at", "updated"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				sc := createSchedule(t, s)
				next := time.Now().Add(2 * time.Hour)
				sc.NextRunAt = &next
				require.NoError(t, s.UpdateSchedule(ctx, sc,
					store.ScheduleFieldMask{NextRunAt: true}, 0, false, nil))
				return sc.ID
			},
		},
		{
			name:    "UpdateSchedule name only",
			columns: []string{"updated"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				sc := createSchedule(t, s)
				sc.Name = "renamed"
				require.NoError(t, s.UpdateSchedule(ctx, sc,
					store.ScheduleFieldMask{Name: true}, 0, false, nil))
				return sc.ID
			},
		},
		{
			name:    "UpdateScheduleStatus",
			columns: []string{"updated"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				sc := createSchedule(t, s)
				require.NoError(t, s.UpdateScheduleStatus(ctx, sc.ID, store.ScheduleStatusPaused))
				return sc.ID
			},
		},
		{
			name:    "UpdateScheduleAfterRun",
			columns: []string{"last_run_at", "next_run_at", "updated"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				sc := createSchedule(t, s)
				now := time.Now()
				require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, now, now.Add(time.Hour), ""))
				return sc.ID
			},
		},
	}
	for _, tc := range scheduleCases {
		t.Run(tc.name, func(t *testing.T) {
			forEachNonUTCZone(t, func(t *testing.T) {
				s := newTestScheduleStore(t)
				id := tc.mutate(t, s)
				for _, col := range tc.columns {
					assertUTCColumn(t, s, "schedules", col, id)
				}
				_, err := s.GetSchedule(ctx, id)
				require.NoError(t, err)
			})
		})
	}

	eventCases := []struct {
		name    string
		columns []string
		mutate  func(t *testing.T, s *ScheduleStore) string
	}{
		{
			name:    "CreateScheduledEvent local times",
			columns: []string{"fire_at", "fired_at", "created"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				now := time.Now()
				fired := now.Add(-time.Minute)
				ev := newTestScheduledEvent(uuid.NewString())
				ev.FireAt, ev.FiredAt, ev.CreatedAt = now.Add(time.Hour), &fired, now
				require.NoError(t, s.CreateScheduledEvent(ctx, ev))
				return ev.ID
			},
		},
		{
			name:    "CreateScheduledEvent default created",
			columns: []string{"created"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				return createEvent(t, s).ID
			},
		},
		{
			name:    "UpdateScheduledEventStatus fired_at",
			columns: []string{"fired_at"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				ev := createEvent(t, s)
				fired := time.Now()
				require.NoError(t, s.UpdateScheduledEventStatus(ctx, ev.ID, store.ScheduledEventFired, &fired, ""))
				return ev.ID
			},
		},
		{
			name:    "ClaimScheduledEvent fired_at",
			columns: []string{"fired_at"},
			mutate: func(t *testing.T, s *ScheduleStore) string {
				ev := createEvent(t, s)
				ok, err := s.ClaimScheduledEvent(ctx, ev.ID, store.ScheduledEventFired)
				require.NoError(t, err)
				require.True(t, ok)
				return ev.ID
			},
		},
	}
	for _, tc := range eventCases {
		t.Run(tc.name, func(t *testing.T) {
			forEachNonUTCZone(t, func(t *testing.T) {
				s := newTestScheduleStore(t)
				id := tc.mutate(t, s)
				for _, col := range tc.columns {
					assertUTCColumn(t, s, "scheduled_events", col, id)
				}
				_, err := s.GetScheduledEvent(ctx, id)
				require.NoError(t, err)
			})
		})
	}
}

// TestPurgeOldScheduledEvents_NonUTCLocal checks the purge cutoff bind
// under non-UTC time.Local settings. The "inside" row sits 23h old
// against a 24h cutoff, within the zone offset of the cutoff, so it is
// deleted if the cutoff is compared as local-zone text.
func TestPurgeOldScheduledEvents_NonUTCLocal(t *testing.T) {
	forEachNonUTCZone(t, func(t *testing.T) {
		s := newTestScheduleStore(t)
		ctx := context.Background()
		projectID := uuid.NewString()
		now := time.Now()

		newFired := func(age time.Duration) *store.ScheduledEvent {
			ev := newTestScheduledEvent(projectID)
			ev.CreatedAt = now.Add(-age)
			require.NoError(t, s.CreateScheduledEvent(ctx, ev))
			require.NoError(t, s.UpdateScheduledEventStatus(ctx, ev.ID, store.ScheduledEventFired, nil, ""))
			return ev
		}
		old := newFired(48 * time.Hour)
		inside := newFired(23 * time.Hour)
		recent := newFired(0)

		n, err := s.PurgeOldScheduledEvents(ctx, now.Add(-24*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		_, err = s.GetScheduledEvent(ctx, old.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = s.GetScheduledEvent(ctx, inside.ID)
		require.NoError(t, err, "row newer than the cutoff must survive")
		_, err = s.GetScheduledEvent(ctx, recent.ID)
		require.NoError(t, err)
	})
}

// TestCreateScheduledEvent_FireInUnderNonUTCLocalRoundTrips mirrors the
// handler's fireIn computation (time.Now().Add(duration) —
// pkg/hub/handlers_scheduled_events.go) under the numeric-abbreviation
// Asia/Kathmandu time.Local. Without UTC normalisation, a fireIn-derived
// time.Time carries time.Now()'s Local zone and hits the same
// unparseable-TEXT failure on read as an offset fireAt.
//
// This lives at the store level rather than pkg/hub's HTTP handler tests:
// mutating the process-global time.Local while a full hub test server is up
// races its background goroutines (observed via `go test -race`: GCP/TLS
// client init reads time.Local concurrently via time.Parse).
//
// The "CreateScheduledEvent local times" writer case above overlaps this;
// it is kept as the documented home of the handler's fireIn path.
func TestCreateScheduledEvent_FireInUnderNonUTCLocalRoundTrips(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kathmandu")
	require.NoError(t, err)
	withLocal(t, loc)

	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	before := time.Now()
	fireAt := before.Add(30 * time.Minute) // the handler's fireIn computation

	evt := &store.ScheduledEvent{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    fireAt,
		Payload:   `{"text":"hello"}`,
		CreatedBy: "user-123",
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.True(t, sameInstantAtStoredPrecision(fireAt, got.FireAt), "fireAt instant must round-trip: want %v got %v", fireAt, got.FireAt)
}
