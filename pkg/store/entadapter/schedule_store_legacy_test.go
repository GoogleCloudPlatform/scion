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
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover SQLite rows whose created column still holds text written
// by a hub that ran in a non-UTC zone, before timestamps were normalized to
// UTC. SQLite compares created as text, so such rows do not sort by instant
// against canonical UTC text until the utc-timestamp-normalize maintenance
// operation has rewritten them.

// openLegacyTextDB opens a second connection to the in-memory SQLite database
// behind enttest.NewClient for t, so a test can write raw column text.
func openLegacyTextDB(t *testing.T) *sql.DB {
	t.Helper()
	enttest.SkipOnPostgres(t, "legacy created text only exists on SQLite")
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// setLegacyCreated rewrites the created column of each row to the text a
// pre-normalization hub in zone loc would have written (Go Time.String()).
func setLegacyCreated(t *testing.T, db *sql.DB, loc *time.Location, ids []string, created []time.Time) {
	t.Helper()
	for i, id := range ids {
		res, err := db.Exec("UPDATE schedules SET created = ? WHERE id = ?", created[i].In(loc).String(), id)
		require.NoError(t, err)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, n, "row %s not rewritten", id)
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

// seedLegacyRows seeds n rows one minute apart and rewrites the rows at
// legacy indexes to legacy text in loc. It returns IDs in creation order.
func seedLegacyRows(t *testing.T, loc *time.Location, n int, legacy []int) (*ScheduleStore, string, []string, *sql.DB) {
	t.Helper()
	s := newTestScheduleStore(t)
	db := openLegacyTextDB(t)
	projectID := uuid.NewString()
	created := make([]time.Time, n)
	for i := range created {
		created[i] = cursorTestBase.Add(time.Duration(i) * time.Minute)
	}
	ids := seedCursorSchedules(t, s, projectID, created)
	var legacyIDs []string
	var legacyCreated []time.Time
	for _, i := range legacy {
		legacyIDs = append(legacyIDs, ids[i])
		legacyCreated = append(legacyCreated, created[i])
	}
	setLegacyCreated(t, db, loc, legacyIDs, legacyCreated)
	return s, projectID, ids, db
}

func TestListSchedulesLegacyText_EastZoneTerminates(t *testing.T) {
	s, projectID, ids, _ := seedLegacyRows(t, mustLoadLocation(t, "Asia/Tokyo"), 5, []int{0, 1, 2})
	filter := store.ScheduleFilter{ProjectID: projectID}

	all := listAll(t, s, filter, 1) // fails if paging does not terminate
	assertNoDuplicates(t, all)
	assert.Subset(t, ids, all)
	// Documented limitation: east of UTC, legacy text sorts after the UTC
	// text the cursor binds, so paging skips rows until the
	// utc-timestamp-normalize maintenance operation has run.
	t.Logf("east-zone legacy text: listed %d of %d rows", len(all), len(ids))
	assert.Less(t, len(all), len(ids), "expected the documented east-zone skip; if this now lists every row, update the test and the release note")
}

func TestListSchedulesLegacyText_WestZoneTerminates(t *testing.T) {
	s, projectID, ids, _ := seedLegacyRows(t, mustLoadLocation(t, "America/New_York"), 3, []int{0, 1, 2})
	filter := store.ScheduleFilter{ProjectID: projectID}

	// West of UTC the keyset returns the same row again on the next page;
	// the no-progress guard then ends the listing instead of returning the
	// same cursor forever. The repeated row is the documented limitation
	// until utc-timestamp-normalize has run.
	all := listAll(t, s, filter, 1) // fails if paging does not terminate
	assert.Subset(t, ids, all)
	t.Logf("west-zone legacy text: listed %d rows (%d distinct) of %d", len(all), countDistinct(all), len(ids))

	page1, err := s.ListSchedules(context.Background(), filter, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, page1.NextCursor)
	page2, err := s.ListSchedules(context.Background(), filter, store.ListOptions{Limit: 1, Cursor: page1.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, pageIDs(page1.Items), pageIDs(page2.Items), "west-zone legacy text repeats the boundary row")
	assert.Empty(t, page2.NextCursor, "a cursor that does not advance must not be returned")
}

func countDistinct(ids []string) int {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return len(seen)
}

func TestListSchedulesLegacyText_NormalizedIsExact(t *testing.T) {
	loc := mustLoadLocation(t, "Asia/Tokyo")
	s, projectID, ids, db := seedLegacyRows(t, loc, 5, []int{0, 1, 2})

	// Rewriting the rows to canonical UTC text, as utc-timestamp-normalize
	// does, makes paging exact again.
	created := []time.Time{cursorTestBase, cursorTestBase.Add(time.Minute), cursorTestBase.Add(2 * time.Minute)}
	setLegacyCreated(t, db, time.UTC, ids[:3], created)

	all := listAll(t, s, store.ScheduleFilter{ProjectID: projectID}, 1)
	assert.Equal(t, []string{ids[4], ids[3], ids[2], ids[1], ids[0]}, all)
}

func TestListActiveZonePrefixedSchedules(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	mk := func(expr, status string) string {
		sc := newTestSchedule(projectID, "zp-"+uuid.NewString()[:8])
		sc.CronExpr = expr
		sc.Status = status
		require.NoError(t, s.CreateSchedule(ctx, sc))
		return sc.ID
	}
	a := mk("CRON_TZ=Asia/Tokyo 0 9 * * *", store.ScheduleStatusActive)
	b := mk("TZ=America/New_York 0 9 * * *", store.ScheduleStatusActive)
	c := mk("CRON_TZ=Europe/Paris 0 9 * * *", store.ScheduleStatusActive)
	mk("0 9 * * *", store.ScheduleStatusActive)
	mk("CRON_TZ=Asia/Tokyo 0 9 * * *", store.ScheduleStatusPaused)
	mk("0 9 * * * TZ=UTC", store.ScheduleStatusActive)

	got, err := s.ListActiveZonePrefixedSchedules(ctx, 10, nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{a, b, c}, pageIDs(got))
	for i := 1; i < len(got); i++ {
		assert.Less(t, got[i-1].ID, got[i].ID, "ordered by id")
	}

	limited, err := s.ListActiveZonePrefixedSchedules(ctx, 2, nil)
	require.NoError(t, err)
	assert.Len(t, limited, 2)

	excluded, err := s.ListActiveZonePrefixedSchedules(ctx, 10, []string{a, c})
	require.NoError(t, err)
	assert.Equal(t, []string{b}, pageIDs(excluded))

	_, err = s.ListActiveZonePrefixedSchedules(ctx, 10, []string{"not-a-uuid"})
	assert.Error(t, err)
}
