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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduleCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID := createTestGrove(t, s)

	scheduleID := api.NewUUID()
	nextRun := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	sched := &store.Schedule{
		ID:        scheduleID,
		GroveID:   groveID,
		Name:      "daily-standup",
		CronExpr:  "0 9 * * 1-5",
		EventType: "message",
		Payload:   `{"agentName":"all","message":"Status update please"}`,
		NextRunAt: &nextRun,
		CreatedBy: "user-123",
	}

	// Create
	err := s.CreateSchedule(ctx, sched)
	require.NoError(t, err)
	assert.False(t, sched.CreatedAt.IsZero())
	assert.Equal(t, store.ScheduleStatusActive, sched.Status)

	// Get
	got, err := s.GetSchedule(ctx, scheduleID)
	require.NoError(t, err)
	assert.Equal(t, scheduleID, got.ID)
	assert.Equal(t, groveID, got.GroveID)
	assert.Equal(t, "daily-standup", got.Name)
	assert.Equal(t, "0 9 * * 1-5", got.CronExpr)
	assert.Equal(t, "message", got.EventType)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Equal(t, "user-123", got.CreatedBy)
	assert.Equal(t, 0, got.RunCount)
	assert.Equal(t, 0, got.ErrorCount)
	assert.NotNil(t, got.NextRunAt)

	// Update
	got.Name = "weekly-standup"
	got.CronExpr = "0 9 * * 1"
	err = s.UpdateSchedule(ctx, got)
	require.NoError(t, err)

	updated, err := s.GetSchedule(ctx, scheduleID)
	require.NoError(t, err)
	assert.Equal(t, "weekly-standup", updated.Name)
	assert.Equal(t, "0 9 * * 1", updated.CronExpr)

	// Update status
	err = s.UpdateScheduleStatus(ctx, scheduleID, store.ScheduleStatusPaused)
	require.NoError(t, err)

	paused, err := s.GetSchedule(ctx, scheduleID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, paused.Status)

	// Delete
	err = s.DeleteSchedule(ctx, scheduleID)
	require.NoError(t, err)

	_, err = s.GetSchedule(ctx, scheduleID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestSchedule_DuplicateName(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID := createTestGrove(t, s)

	sched1 := &store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      "duplicate-name",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
	}
	require.NoError(t, s.CreateSchedule(ctx, sched1))

	sched2 := &store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      "duplicate-name",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
	}
	err := s.CreateSchedule(ctx, sched2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestSchedule_List(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID := createTestGrove(t, s)

	// Create 3 schedules
	for i, name := range []string{"sched-a", "sched-b", "sched-c"} {
		status := store.ScheduleStatusActive
		if i == 2 {
			status = store.ScheduleStatusPaused
		}
		sched := &store.Schedule{
			ID:        api.NewUUID(),
			GroveID:   groveID,
			Name:      name,
			CronExpr:  "0 * * * *",
			EventType: "message",
			Payload:   "{}",
			Status:    status,
		}
		require.NoError(t, s.CreateSchedule(ctx, sched))
	}

	// List all (excludes deleted)
	result, err := s.ListSchedules(ctx, store.ScheduleFilter{GroveID: groveID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)
	assert.Len(t, result.Items, 3)

	// Filter by status
	result, err = s.ListSchedules(ctx, store.ScheduleFilter{GroveID: groveID, Status: store.ScheduleStatusActive}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	result, err = s.ListSchedules(ctx, store.ScheduleFilter{GroveID: groveID, Status: store.ScheduleStatusPaused}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
}

func TestSchedule_UpdateAfterRun(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID := createTestGrove(t, s)

	nextRun := time.Now().Add(-1 * time.Minute).UTC().Truncate(time.Second)
	sched := &store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      "run-test",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
		NextRunAt: &nextRun,
	}
	require.NoError(t, s.CreateSchedule(ctx, sched))

	// Successful run
	ranAt := time.Now().UTC().Truncate(time.Second)
	newNextRun := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	err := s.UpdateScheduleAfterRun(ctx, sched.ID, ranAt, newNextRun, "")
	require.NoError(t, err)

	got, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, 0, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunSuccess, got.LastRunStatus)
	assert.Empty(t, got.LastRunError)
	assert.NotNil(t, got.LastRunAt)
	assert.NotNil(t, got.NextRunAt)

	// Error run
	err = s.UpdateScheduleAfterRun(ctx, sched.ID, ranAt, newNextRun, "agent not found")
	require.NoError(t, err)

	got, err = s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, got.RunCount)
	assert.Equal(t, 1, got.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, got.LastRunStatus)
	assert.Equal(t, "agent not found", got.LastRunError)
}

func TestSchedule_ListDue(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID := createTestGrove(t, s)

	now := time.Now().UTC()

	// Create a due schedule (next_run_at in the past)
	pastRun := now.Add(-5 * time.Minute)
	dueSchedule := &store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      "due-schedule",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
		NextRunAt: &pastRun,
	}
	require.NoError(t, s.CreateSchedule(ctx, dueSchedule))

	// Create a future schedule (not due yet)
	futureRun := now.Add(1 * time.Hour)
	futureSchedule := &store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      "future-schedule",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
		NextRunAt: &futureRun,
	}
	require.NoError(t, s.CreateSchedule(ctx, futureSchedule))

	// Create a paused schedule (should not be listed even if due)
	pausedSchedule := &store.Schedule{
		ID:        api.NewUUID(),
		GroveID:   groveID,
		Name:      "paused-schedule",
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   "{}",
		Status:    store.ScheduleStatusPaused,
		NextRunAt: &pastRun,
	}
	require.NoError(t, s.CreateSchedule(ctx, pausedSchedule))

	// List due schedules
	dueSchedules, err := s.ListDueSchedules(ctx, now)
	require.NoError(t, err)
	assert.Len(t, dueSchedules, 1)
	assert.Equal(t, "due-schedule", dueSchedules[0].Name)
}

func TestScheduledEvent_WithScheduleID(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	groveID := createTestGrove(t, s)

	scheduleID := api.NewUUID()
	eventID := api.NewUUID()

	evt := &store.ScheduledEvent{
		ID:         eventID,
		GroveID:    groveID,
		EventType:  "message",
		FireAt:     time.Now().UTC(),
		Payload:    `{"message":"test"}`,
		ScheduleID: scheduleID,
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	// Verify schedule_id is persisted
	got, err := s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, scheduleID, got.ScheduleID)

	// Filter by schedule_id
	result, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{
		GroveID:    groveID,
		ScheduleID: scheduleID,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, eventID, result.Items[0].ID)
}
