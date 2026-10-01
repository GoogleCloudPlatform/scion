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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
// FixedZone. Before the store normalised every write to UTC, the modernc
// SQLite driver (v1.53.0) persisted that zone's numeric abbreviation
// (e.g. "+0200 +0200") as literal TEXT, which its own read-side layout could
// not parse back — breaking Get and List for the row (and, through ent, the
// whole query). This test fails on main with a Scan error; it passes once
// schedule_store.go normalises fireAt with .UTC() at the write boundary.
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
	// numeric abbreviation.
	typ, val := rawTimeColumn(t, s, "scheduled_events", "fire_at", evt.ID)
	assert.Equal(t, "text", typ)
	assert.Equal(t, "'2026-10-02 08:00:00 +0000 UTC'", val)

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

// TestListDueSchedules_NonUTCLocal exercises ListDueSchedules's due-query
// predicate under two non-UTC time.Local settings: Asia/Kathmandu (whose
// tzdata entry has no letter abbreviation — its Zone() name is the literal
// numeric offset "+0545", the same shape that broke fire_at reads) and
// Asia/Tokyo (a normally-abbreviated zone, "JST", included as a control that
// already round-tripped before this fix). Both must return exactly the due,
// active schedules once "now" is normalised to UTC before being bound into
// the WHERE predicate.
func TestListDueSchedules_NonUTCLocal(t *testing.T) {
	for _, tc := range []struct {
		name string
		zone string
	}{
		{name: "Asia/Kathmandu (numeric-abbreviation zone)", zone: "Asia/Kathmandu"},
		{name: "Asia/Tokyo (named-abbreviation zone)", zone: "Asia/Tokyo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			require.NoError(t, err)
			withLocal(t, loc)

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
}

// TestPurgeOldScheduledEvents_NonUTCLocal exercises the purge cutoff
// predicate under the numeric-abbreviation Asia/Kathmandu zone, mirroring
// TestListDueSchedules_NonUTCLocal for PurgeOldScheduledEvents's CreatedLT
// bind.
func TestPurgeOldScheduledEvents_NonUTCLocal(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kathmandu")
	require.NoError(t, err)
	withLocal(t, loc)

	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	old := newTestScheduledEvent(projectID)
	old.CreatedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, s.CreateScheduledEvent(ctx, old))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, old.ID, store.ScheduledEventFired, nil, ""))

	recent := newTestScheduledEvent(projectID)
	require.NoError(t, s.CreateScheduledEvent(ctx, recent))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, recent.ID, store.ScheduledEventFired, nil, ""))

	cutoff := time.Now().Add(-24 * time.Hour)
	n, err := s.PurgeOldScheduledEvents(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = s.GetScheduledEvent(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetScheduledEvent(ctx, recent.ID)
	require.NoError(t, err)
}

// TestCreateScheduledEvent_FireInUnderNonUTCLocalRoundTrips mirrors the
// handler's fireIn computation (time.Now().Add(duration) —
// pkg/hub/handlers_scheduled_events.go) under the numeric-abbreviation
// Asia/Kathmandu time.Local. Before this fix, a fireIn-derived time.Time
// inherited time.Now()'s un-normalised Local zone and hit the same
// unparseable-TEXT failure on read as an offset fireAt.
//
// This lives at the store level rather than pkg/hub's HTTP handler tests:
// mutating the process-global time.Local while a full hub test server is up
// races its background goroutines (observed via `go test -race`: GCP/TLS
// client init reads time.Local concurrently via time.Parse).
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
	assert.True(t, fireAt.Equal(got.FireAt), "fireAt instant must round-trip: want %v got %v", fireAt, got.FireAt)
}
