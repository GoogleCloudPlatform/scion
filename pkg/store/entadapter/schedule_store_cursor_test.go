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
	"encoding/base64"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover ListSchedules keyset paging. They run on SQLite by
// default and on Postgres in the CI job that sets SCION_TEST_POSTGRES_URL
// (Makefile target test-launch-store-postgres runs TestListSchedules_*).

// cursorTestBase is a fixed instant at microsecond precision, so values
// round-trip identically through SQLite and Postgres.
var cursorTestBase = time.Date(2026, 3, 14, 9, 26, 53, 589793000, time.UTC)

// seedCursorSchedules creates one schedule per entry in created (all in one
// project), returning their IDs in creation order.
func seedCursorSchedules(t *testing.T, s *ScheduleStore, projectID string, created []time.Time) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, len(created))
	for i, c := range created {
		sc := newTestSchedule(projectID, "cursor-"+uuid.NewString()[:8])
		sc.CreatedAt = c
		require.NoError(t, s.CreateSchedule(ctx, sc), "seed row %d", i)
		ids = append(ids, sc.ID)
	}
	return ids
}

func pageIDs(items []store.Schedule) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// listAll pages through ListSchedules to the end and returns every ID seen,
// failing if paging does not terminate.
func listAll(t *testing.T, s *ScheduleStore, filter store.ScheduleFilter, limit int) []string {
	t.Helper()
	ctx := context.Background()
	var all []string
	cursor := ""
	for i := 0; ; i++ {
		require.Less(t, i, 1000, "paging did not terminate")
		page, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: limit, Cursor: cursor})
		require.NoError(t, err)
		all = append(all, pageIDs(page.Items)...)
		if page.NextCursor == "" {
			return all
		}
		cursor = page.NextCursor
	}
}

func assertNoDuplicates(t *testing.T, ids []string) {
	t.Helper()
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		assert.False(t, seen[id], "duplicate id %s across pages", id)
		seen[id] = true
	}
}

func TestListSchedules_CursorReturnsNextPage(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	created := make([]time.Time, 5)
	for i := range created {
		created[i] = cursorTestBase.Add(time.Duration(i) * time.Minute)
	}
	ids := seedCursorSchedules(t, s, projectID, created)
	filter := store.ScheduleFilter{ProjectID: projectID}

	page1, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{ids[4], ids[3]}, pageIDs(page1.Items))
	assert.Equal(t, 5, page1.TotalCount)
	require.NotEmpty(t, page1.NextCursor)
	assert.NotEqual(t, ids[3], page1.NextCursor, "cursor must be an opaque token, not a bare ID")

	page2, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: page1.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{ids[2], ids[1]}, pageIDs(page2.Items))
	assert.Equal(t, 5, page2.TotalCount)
	require.NotEmpty(t, page2.NextCursor)

	page3, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: page2.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{ids[0]}, pageIDs(page3.Items))
	assert.Empty(t, page3.NextCursor)
}

func TestListSchedules_CursorEqualCreatedTies(t *testing.T) {
	s := newTestScheduleStore(t)
	projectID := uuid.NewString()

	// Every row shares the same created, so every page boundary is a tie
	// and order rests entirely on the id DESC tiebreak.
	created := make([]time.Time, 7)
	for i := range created {
		created[i] = cursorTestBase
	}
	ids := seedCursorSchedules(t, s, projectID, created)

	all := listAll(t, s, store.ScheduleFilter{ProjectID: projectID}, 3)
	assertNoDuplicates(t, all)
	assert.ElementsMatch(t, ids, all)
	for i := 1; i < len(all); i++ {
		assert.Greater(t, all[i-1], all[i], "ties must be ordered by id DESC")
	}
}

func TestListSchedules_CursorTieAcrossBoundaryMixed(t *testing.T) {
	s := newTestScheduleStore(t)
	projectID := uuid.NewString()

	// Rows 1-4 share a created value that straddles the limit-2 boundaries.
	created := []time.Time{
		cursorTestBase,
		cursorTestBase.Add(time.Minute),
		cursorTestBase.Add(time.Minute),
		cursorTestBase.Add(time.Minute),
		cursorTestBase.Add(time.Minute),
		cursorTestBase.Add(2 * time.Minute),
	}
	ids := seedCursorSchedules(t, s, projectID, created)

	all := listAll(t, s, store.ScheduleFilter{ProjectID: projectID}, 2)
	assertNoDuplicates(t, all)
	assert.ElementsMatch(t, ids, all)
	assert.Equal(t, ids[5], all[0])
	assert.Equal(t, ids[0], all[len(all)-1])
}

func TestListSchedules_CursorBoundaryRowPaused(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	created := make([]time.Time, 6)
	for i := range created {
		created[i] = cursorTestBase.Add(time.Duration(i) * time.Second)
	}
	ids := seedCursorSchedules(t, s, projectID, created)
	// ids[2] is already paused, so it is never in the active listing.
	require.NoError(t, s.UpdateScheduleStatus(ctx, ids[2], store.ScheduleStatusPaused))

	filter := store.ScheduleFilter{ProjectID: projectID, Status: store.ScheduleStatusActive}
	page1, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{ids[5], ids[4]}, pageIDs(page1.Items))

	// Pause the page-boundary row: it no longer matches the filter.
	require.NoError(t, s.UpdateScheduleStatus(ctx, ids[4], store.ScheduleStatusPaused))

	page2, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: page1.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{ids[3], ids[1]}, pageIDs(page2.Items),
		"page 2 must start at the next older active row")

	page3, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: page2.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{ids[0]}, pageIDs(page3.Items))
	assert.Empty(t, page3.NextCursor)

	union := append(append(pageIDs(page1.Items), pageIDs(page2.Items)...), pageIDs(page3.Items)...)
	assertNoDuplicates(t, union)
	assert.ElementsMatch(t, []string{ids[5], ids[4], ids[3], ids[1], ids[0]}, union)
}

func TestListSchedules_CursorBoundaryRowDeleted(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	created := make([]time.Time, 5)
	for i := range created {
		created[i] = cursorTestBase.Add(time.Duration(i) * time.Second)
	}
	ids := seedCursorSchedules(t, s, projectID, created)
	filter := store.ScheduleFilter{ProjectID: projectID}

	page1, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{ids[4], ids[3]}, pageIDs(page1.Items))

	before, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: page1.NextCursor})
	require.NoError(t, err)

	// Hard-delete the page-boundary row.
	require.NoError(t, s.DeleteSchedule(ctx, ids[3]))

	after, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: page1.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, pageIDs(before.Items), pageIDs(after.Items), "page 2 must be unchanged by the delete")
	assert.Equal(t, []string{ids[2], ids[1]}, pageIDs(after.Items))
	assert.Equal(t, before.NextCursor, after.NextCursor)

	page3, err := s.ListSchedules(ctx, filter, store.ListOptions{Limit: 2, Cursor: after.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []string{ids[0]}, pageIDs(page3.Items))

	union := append(append(pageIDs(page1.Items), pageIDs(after.Items)...), pageIDs(page3.Items)...)
	assertNoDuplicates(t, union)
	assert.ElementsMatch(t, ids, union)
}

func TestListSchedules_InvalidCursor(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	ids := seedCursorSchedules(t, s, projectID, []time.Time{cursorTestBase, cursorTestBase.Add(time.Second)})

	for name, cursor := range map[string]string{
		"not base64":         "%%%not-base64%%%",
		"base64 no comma":    base64.URLEncoding.EncodeToString([]byte("garbage")),
		"base64 bad time":    base64.URLEncoding.EncodeToString([]byte("yesterday," + ids[0])),
		"base64 bad id":      base64.URLEncoding.EncodeToString([]byte(cursorTestBase.Format(time.RFC3339Nano) + ",nope")),
		"bare UUID (legacy)": ids[0],
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 1, Cursor: cursor})
			require.Error(t, err)
			assert.ErrorIs(t, err, store.ErrInvalidInput)
		})
	}
}

func TestListSchedules_CursorDefaultCreated(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	// created is left to the store default (time.Now, full backend
	// precision), so the cursor must round-trip whatever the backend stores.
	ids := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		sc := newTestSchedule(projectID, "default-"+uuid.NewString()[:8])
		require.NoError(t, s.CreateSchedule(ctx, sc))
		ids = append(ids, sc.ID)
	}

	all := listAll(t, s, store.ScheduleFilter{ProjectID: projectID}, 2)
	assertNoDuplicates(t, all)
	assert.ElementsMatch(t, ids, all)
}
