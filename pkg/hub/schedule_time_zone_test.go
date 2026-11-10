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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
	_ "time/tzdata" // the zones below must not depend on the host's zoneinfo

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-schedule time zones (ptone/scion#6771). The wall-clock and daylight
// saving rules are tested in pkg/cronzone; these tests cover the hub
// surfaces: validation, storage, the update semantics, re-attribution, and
// that every next-run computation uses the stored zone.

// createZonedSchedule creates a message schedule through the API and
// returns the decoded response.
func createZonedSchedule(t *testing.T, srv *Server, projectID, name, cronExpr, zone string) store.Schedule {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: name, CronExpr: cronExpr, TimeZone: zone, EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	return created
}

// errorDetailField returns details.field of an error response.
func errorDetailField(t *testing.T, body []byte) any {
	t.Helper()
	var resp struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	return resp.Error.Details["field"]
}

// assertLocalRun checks that next is a UTC instant at hh:mm local time in zone.
func assertLocalRun(t *testing.T, next *time.Time, zone string, hh, mm int) {
	t.Helper()
	require.NotNil(t, next)
	loc, err := time.LoadLocation(zone)
	require.NoError(t, err)
	local := next.In(loc)
	assert.Equal(t, hh, local.Hour(), "local hour of %v in %s", next, zone)
	assert.Equal(t, mm, local.Minute(), "local minute of %v in %s", next, zone)
	assert.True(t, next.After(time.Now().Add(-time.Minute)), "next run %v is in the future", next)
}

func TestScheduleTimeZone_Create(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	created := createZonedSchedule(t, srv, projectID, "pacific", "17 9 * * *", "America/Los_Angeles")
	assert.Equal(t, "America/Los_Angeles", created.TimeZone)
	assertLocalRun(t, created.NextRunAt, "America/Los_Angeles", 9, 17)
	got, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "America/Los_Angeles", got.TimeZone)

	// Empty and "UTC" both store as UTC (empty) and evaluate in UTC.
	for _, zone := range []string{"", "UTC"} {
		created := createZonedSchedule(t, srv, projectID, "utc-"+zone, "17 9 * * *", zone)
		assert.Empty(t, created.TimeZone, "zone %q", zone)
		assertLocalRun(t, created.NextRunAt, "UTC", 9, 17)
	}
}

func TestScheduleTimeZone_CreateRefusesInvalidZone(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	cases := map[string]string{
		"Mars/Olympus_Mons": `unknown time zone "Mars/Olympus_Mons": use an IANA name such as America/Los_Angeles`,
		"Local":             `time zone "Local" is not supported: use an IANA name such as America/Los_Angeles`,
		" Europe/London":    `time zone " Europe/London" has leading or trailing whitespace`,
	}
	for zone, want := range cases {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
			CreateScheduleRequest{
				Name: "bad-zone", CronExpr: "0 9 * * *", TimeZone: zone, EventType: "message",
				AgentName: "worker", Message: "hello",
			})
		require.Equal(t, http.StatusBadRequest, rec.Code, zone)
		assert.Equal(t, want, errorMessage(t, rec.Body.Bytes()), zone)
		assert.Equal(t, "timeZone", errorDetailField(t, rec.Body.Bytes()), zone)
	}

	// The zone prefix refusal is unchanged, and takes precedence, even when
	// a valid zone is given alongside.
	for _, expr := range zonePrefixedExprs {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
			CreateScheduleRequest{
				Name: "prefixed", CronExpr: expr, TimeZone: "Asia/Tokyo", EventType: "message",
				AgentName: "worker", Message: "hello",
			})
		require.Equal(t, http.StatusBadRequest, rec.Code, expr)
		assert.Equal(t, cronZonePrefixMessage, errorMessage(t, rec.Body.Bytes()), expr)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.TotalCount, "nothing is stored for a refused create")
}

func TestScheduleTimeZone_Update(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	created := createZonedSchedule(t, srv, projectID, "nightly", "30 19 * * *", "")
	base := "/api/v1/projects/" + projectID + "/schedules/" + created.ID
	get := func() *store.Schedule {
		t.Helper()
		got, err := s.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		return got
	}
	patch := func(req UpdateScheduleRequest) int {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPatch, base, req)
		return rec.Code
	}
	rev0 := get().AuthorizationRevision

	// Setting a zone stores it, recomputes the next run in it, and counts
	// as a change to future dispatch (re-attribution bumps the revision).
	require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{TimeZone: strPtr("America/New_York")}))
	got := get()
	assert.Equal(t, "America/New_York", got.TimeZone)
	assertLocalRun(t, got.NextRunAt, "America/New_York", 19, 30)
	rev1 := got.AuthorizationRevision
	assert.Greater(t, rev1, rev0, "a zone change re-attributes")

	// Omitting the zone leaves it alone; resending the same zone is a
	// metadata-only update that does not re-attribute.
	require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{Name: "renamed"}))
	require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{TimeZone: strPtr("America/New_York")}))
	got = get()
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "America/New_York", got.TimeZone)
	assert.Equal(t, rev1, got.AuthorizationRevision, "an unchanged zone does not re-attribute")

	// A cron change alone evaluates in the stored zone.
	require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{CronExpr: "5 7 * * *"}))
	got = get()
	assertLocalRun(t, got.NextRunAt, "America/New_York", 7, 5)

	// Cron and zone together.
	require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{CronExpr: "45 22 * * *", TimeZone: strPtr("Asia/Kolkata")}))
	got = get()
	assert.Equal(t, "45 22 * * *", got.CronExpr)
	assert.Equal(t, "Asia/Kolkata", got.TimeZone)
	assertLocalRun(t, got.NextRunAt, "Asia/Kolkata", 22, 45)

	// An invalid zone is refused and nothing changes.
	rec := doRequest(t, srv, http.MethodPatch, base, UpdateScheduleRequest{CronExpr: "0 1 * * *", TimeZone: strPtr("Nowhere/City")})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, errorMessage(t, rec.Body.Bytes()), `unknown time zone "Nowhere/City"`)
	assert.Equal(t, "timeZone", errorDetailField(t, rec.Body.Bytes()))
	got = get()
	assert.Equal(t, "45 22 * * *", got.CronExpr)
	assert.Equal(t, "Asia/Kolkata", got.TimeZone)

	// "" and "UTC" both clear it back to UTC.
	for _, zone := range []string{"", "UTC"} {
		require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{TimeZone: strPtr("Europe/London")}))
		require.Equal(t, http.StatusOK, patch(UpdateScheduleRequest{TimeZone: strPtr(zone)}))
		got = get()
		assert.Empty(t, got.TimeZone, "zone %q clears", zone)
		assertLocalRun(t, got.NextRunAt, "UTC", 22, 45)
	}
}

func TestScheduleTimeZone_EnableAndResumeUseStoredZone(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	for _, path := range []string{"resume", "enable"} {
		created := createZonedSchedule(t, srv, projectID, "zoned-"+path, "10 6 * * *", "Australia/Sydney")
		base := "/api/v1/projects/" + projectID + "/schedules/" + created.ID
		rec := doRequest(t, srv, http.MethodPatch, base, UpdateScheduleRequest{Status: store.ScheduleStatusPaused})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Make the stored next run stale so the recompute is visible.
		stale := time.Now().UTC().Add(-48 * time.Hour)
		row, err := s.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		row.NextRunAt = &stale
		require.NoError(t, s.UpdateSchedule(ctx, row, store.ScheduleFieldMask{NextRunAt: true},
			row.AuthorizationRevision, row.AuthorizationRevision != 0, nil))

		if path == "resume" {
			rec = doRequest(t, srv, http.MethodPost, base+"/resume", nil)
		} else {
			rec = doRequest(t, srv, http.MethodPatch, base, UpdateScheduleRequest{Status: store.ScheduleStatusActive})
		}
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetSchedule(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduleStatusActive, got.Status, path)
		assertLocalRun(t, got.NextRunAt, "Australia/Sydney", 6, 10)
	}
}

func TestScheduleTimeZone_Evaluator(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	// A due zoned schedule: the evaluator computes the next run in its zone.
	created := createZonedSchedule(t, srv, projectID, "zoned-eval", "20 8 * * *", "Europe/Berlin")
	row, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	srv.executeSchedule(ctx, *row, time.Now().UTC())
	got, err := s.GetSchedule(ctx, created.ID)
	require.NoError(t, err)
	assertLocalRun(t, got.NextRunAt, "Europe/Berlin", 8, 20)
	assert.NotContains(t, got.LastRunError, "time zone")

	// A stored zone that no longer resolves records an error instead of
	// firing, the same way an invalid expression does.
	// The row is created through the API (so its authority is recorded) and
	// the zone is then written straight to the store, bypassing validation.
	badCreated := createZonedSchedule(t, srv, projectID, "bad-zone", "0 * * * *", "")
	bad, err := s.GetSchedule(ctx, badCreated.ID)
	require.NoError(t, err)
	bad.TimeZone = "Gone/Away"
	require.NoError(t, s.UpdateSchedule(ctx, bad, store.ScheduleFieldMask{TimeZone: true},
		bad.AuthorizationRevision, bad.AuthorizationRevision != 0, nil))
	bad, err = s.GetSchedule(ctx, badCreated.ID)
	require.NoError(t, err)
	require.Equal(t, "Gone/Away", bad.TimeZone)
	srv.executeSchedule(ctx, *bad, time.Now().UTC())
	got, err = s.GetSchedule(ctx, bad.ID)
	require.NoError(t, err)
	assert.Contains(t, got.LastRunError, `invalid schedule time zone: unknown time zone "Gone/Away"`)
	events, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID, ScheduleID: bad.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, events.Items, "no event is materialized for an invalid zone")
}
