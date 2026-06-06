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
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestScheduleStore(t *testing.T) *ScheduleStore {
	t.Helper()
	client := enttest.NewClient(t)
	return NewScheduleStore(client)
}

func newTestSchedule(projectID string, name string) *store.Schedule {
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	return &store.Schedule{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		Name:      name,
		CronExpr:  "0 9 * * 1-5",
		EventType: "message",
		Payload:   `{"message":"standup"}`,
		NextRunAt: &next,
		CreatedBy: "user-123",
	}
}

func TestScheduleCRUD(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	sc := newTestSchedule(projectID, "daily-standup")
	require.NoError(t, s.CreateSchedule(ctx, sc))
	assert.False(t, sc.CreatedAt.IsZero())
	assert.Equal(t, store.ScheduleStatusActive, sc.Status)

	got, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, sc.ID, got.ID)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, "daily-standup", got.Name)
	assert.Equal(t, "0 9 * * 1-5", got.CronExpr)
	assert.Equal(t, "message", got.EventType)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Equal(t, "user-123", got.CreatedBy)
	require.NotNil(t, got.NextRunAt)

	// Update
	got.Name = "weekly-standup"
	got.CronExpr = "0 9 * * 1"
	require.NoError(t, s.UpdateSchedule(ctx, got))
	updated, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, "weekly-standup", updated.Name)
	assert.Equal(t, "0 9 * * 1", updated.CronExpr)

	// Status
	require.NoError(t, s.UpdateScheduleStatus(ctx, sc.ID, store.ScheduleStatusPaused))
	paused, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, paused.Status)

	// Delete
	require.NoError(t, s.DeleteSchedule(ctx, sc.ID))
	_, err = s.GetSchedule(ctx, sc.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduleGetNotFound(t *testing.T) {
	s := newTestScheduleStore(t)
	_, err := s.GetSchedule(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduleInvalidInput(t *testing.T) {
	s := newTestScheduleStore(t)
	err := s.CreateSchedule(context.Background(), &store.Schedule{ID: uuid.NewString()})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestScheduleUpdateNotFound(t *testing.T) {
	s := newTestScheduleStore(t)
	sc := newTestSchedule(uuid.NewString(), "ghost")
	err := s.UpdateSchedule(context.Background(), sc)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUpdateScheduleAfterRun(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	sc := newTestSchedule(uuid.NewString(), "runner")
	require.NoError(t, s.CreateSchedule(ctx, sc))

	ranAt := time.Now().UTC().Truncate(time.Second)
	nextRun := ranAt.Add(time.Hour)

	// Success run increments run_count, clears error.
	require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, ranAt, nextRun, ""))
	got, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, 0, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunSuccess, got.LastRunStatus)
	assert.Empty(t, got.LastRunError)

	// Error run increments both counters and records the error.
	require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, ranAt, nextRun, "boom"))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, got.RunCount)
	assert.Equal(t, 1, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, got.LastRunStatus)
	assert.Equal(t, "boom", got.LastRunError)
}

func TestListSchedulesFilterAndPagination(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	other := uuid.NewString()

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateSchedule(ctx, newTestSchedule(projectID, "sched-"+uuid.NewString()[:8])))
	}
	require.NoError(t, s.CreateSchedule(ctx, newTestSchedule(other, "other")))

	// Filter by project.
	res, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, res.TotalCount)
	assert.Len(t, res.Items, 3)

	// Pagination: limit honored, total independent of limit.
	page, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page.Items, 2)
	assert.Equal(t, 3, page.TotalCount)
	assert.NotEmpty(t, page.NextCursor)
}

func TestListSchedulesExcludesDeletedByDefault(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	active := newTestSchedule(projectID, "active")
	require.NoError(t, s.CreateSchedule(ctx, active))
	deleted := newTestSchedule(projectID, "deleted")
	require.NoError(t, s.CreateSchedule(ctx, deleted))
	require.NoError(t, s.UpdateScheduleStatus(ctx, deleted.ID, store.ScheduleStatusDeleted))

	res, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.TotalCount)
	assert.Equal(t, active.ID, res.Items[0].ID)
}

// TestListDueSchedulesClaimPath exercises the dialect-aware SKIP LOCKED claim
// helper. On SQLite it degrades to a plain SELECT, so this verifies the
// functional contract of the claim query: only active, already-due schedules
// are returned, ordered by next_run_at ascending. The concurrency sub-test
// hammers the helper from multiple goroutines to flush out races in the
// claim/transaction handling.
func TestListDueSchedulesClaimPath(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)

	// Two due schedules (past next_run_at), at distinct times for ordering.
	dueEarly := newTestSchedule(projectID, "due-early")
	early := now.Add(-2 * time.Hour)
	dueEarly.NextRunAt = &early
	require.NoError(t, s.CreateSchedule(ctx, dueEarly))

	dueLate := newTestSchedule(projectID, "due-late")
	late := now.Add(-1 * time.Hour)
	dueLate.NextRunAt = &late
	require.NoError(t, s.CreateSchedule(ctx, dueLate))

	// Not due (future next_run_at).
	future := newTestSchedule(projectID, "future")
	require.NoError(t, s.CreateSchedule(ctx, future))

	// Due but paused — must be excluded (status != active).
	paused := newTestSchedule(projectID, "paused")
	paused.NextRunAt = &early
	require.NoError(t, s.CreateSchedule(ctx, paused))
	require.NoError(t, s.UpdateScheduleStatus(ctx, paused.ID, store.ScheduleStatusPaused))

	due, err := s.ListDueSchedules(ctx, now)
	require.NoError(t, err)
	require.Len(t, due, 2, "only the two active, due schedules should be claimed")
	assert.Equal(t, dueEarly.ID, due[0].ID, "ordered by next_run_at ascending")
	assert.Equal(t, dueLate.ID, due[1].ID)

	// Concurrent claims must not race or error. The expected per-call count is
	// backend-dependent:
	//   - SQLite has no SELECT ... FOR UPDATE SKIP LOCKED, and the test store
	//     pins MaxOpenConns=1, so the claim path serializes and every caller
	//     observes both due schedules.
	//   - Postgres uses FOR UPDATE SKIP LOCKED inside a transaction that holds
	//     the row locks until commit, so a concurrent caller skips rows locked
	//     by a sibling and may observe a disjoint subset (0..2). The cross-call
	//     invariant is only that no caller errors or observes more than the two
	//     due rows.
	isPostgres := s.client.Driver().Dialect() == dialect.Postgres
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.ListDueSchedules(ctx, now)
			if err != nil {
				errs <- err
				return
			}
			if isPostgres {
				if len(res) > 2 {
					errs <- assert.AnError
				}
			} else if len(res) != 2 {
				errs <- assert.AnError
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
