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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestScheduledEvent(projectID string) *store.ScheduledEvent {
	fireAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	return &store.ScheduledEvent{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    fireAt,
		Payload:   `{"text":"hello"}`,
		CreatedBy: "user-123",
	}
}

func TestScheduledEventCRUD(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	evt := newTestScheduledEvent(projectID)
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))
	assert.False(t, evt.CreatedAt.IsZero())
	assert.Equal(t, store.ScheduledEventPending, evt.Status)

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, evt.ID, got.ID)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, "message", got.EventType)
	assert.Equal(t, store.ScheduledEventPending, got.Status)
	assert.Equal(t, "user-123", got.CreatedBy)

	// Update status -> fired.
	firedAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, evt.ID, store.ScheduledEventFired, &firedAt, ""))
	got, err = s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	require.NotNil(t, got.FiredAt)
}

func TestScheduledEventGetNotFound(t *testing.T) {
	s := newTestScheduleStore(t)
	_, err := s.GetScheduledEvent(context.Background(), uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduledEventInvalidInput(t *testing.T) {
	s := newTestScheduleStore(t)
	err := s.CreateScheduledEvent(context.Background(), &store.ScheduledEvent{ID: uuid.NewString()})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestCancelScheduledEvent(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	evt := newTestScheduledEvent(uuid.NewString())
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	require.NoError(t, s.CancelScheduledEvent(ctx, evt.ID))
	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, got.Status)

	// Cancelling a non-pending event is ErrNotFound.
	err = s.CancelScheduledEvent(ctx, evt.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCancelScheduledEventWithReason(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	pending := newTestScheduledEvent(projectID)
	require.NoError(t, s.CreateScheduledEvent(ctx, pending))
	require.NoError(t, s.CancelScheduledEventWithReason(ctx, pending.ID, "target agent deleted"))
	got, err := s.GetScheduledEvent(ctx, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, got.Status)
	assert.Equal(t, "target agent deleted", got.Error)

	// A fired event is no longer pending: ErrNotFound, and it keeps its
	// status, fire time and (empty) error.
	fired := newTestScheduledEvent(projectID)
	require.NoError(t, s.CreateScheduledEvent(ctx, fired))
	claimed, err := s.ClaimScheduledEvent(ctx, fired.ID, store.ScheduledEventFired)
	require.NoError(t, err)
	require.True(t, claimed)
	err = s.CancelScheduledEventWithReason(ctx, fired.ID, "target agent deleted")
	assert.ErrorIs(t, err, store.ErrNotFound)
	got, err = s.GetScheduledEvent(ctx, fired.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	assert.NotNil(t, got.FiredAt)
	assert.Empty(t, got.Error)

	// A missing event is ErrNotFound.
	err = s.CancelScheduledEventWithReason(ctx, uuid.NewString(), "target agent deleted")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestListScheduledEventsFilter(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateScheduledEvent(ctx, newTestScheduledEvent(projectID)))
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, newTestScheduledEvent(uuid.NewString())))

	res, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, res.TotalCount)
	assert.Len(t, res.Items, 3)

	res, err = s.ListScheduledEvents(ctx, store.ScheduledEventFilter{Status: store.ScheduledEventPending}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 4, res.TotalCount)
}

func TestPurgeOldScheduledEvents(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	// Old fired event (should be purged).
	old := newTestScheduledEvent(projectID)
	old.CreatedAt = time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.CreateScheduledEvent(ctx, old))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, old.ID, store.ScheduledEventFired, nil, ""))

	// Old pending event (should NOT be purged — still pending).
	oldPending := newTestScheduledEvent(projectID)
	oldPending.CreatedAt = time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.CreateScheduledEvent(ctx, oldPending))

	cutoff := time.Now().Add(-24 * time.Hour)
	n, err := s.PurgeOldScheduledEvents(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = s.GetScheduledEvent(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetScheduledEvent(ctx, oldPending.ID)
	require.NoError(t, err)
}

// TestPurgeOldScheduledEvents_CompletionTime checks that the purge keys on
// fired_at, falling back to created only for a row with no fired_at (a
// cancelled one): an event created before the cutoff that fired after it
// is kept (ptone/scion#4517).
func TestPurgeOldScheduledEvents_CompletionTime(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-time.Hour)
	cutoff := now.Add(-24 * time.Hour)

	newEvent := func(created time.Time) *store.ScheduledEvent {
		ev := newTestScheduledEvent(projectID)
		ev.CreatedAt = created
		require.NoError(t, s.CreateScheduledEvent(ctx, ev))
		return ev
	}
	withStatus := func(created time.Time, status string, firedAt *time.Time) string {
		ev := newEvent(created)
		require.NoError(t, s.UpdateScheduledEventStatus(ctx, ev.ID, status, firedAt, ""))
		return ev.ID
	}
	cancelled := func(created time.Time) string {
		ev := newEvent(created)
		require.NoError(t, s.CancelScheduledEvent(ctx, ev.ID))
		got, err := s.GetScheduledEvent(ctx, ev.ID)
		require.NoError(t, err)
		require.Nil(t, got.FiredAt, "cancel sets no fired_at")
		return ev.ID
	}

	purged := map[string]string{}
	purged["fired before cutoff"] = withStatus(old, store.ScheduledEventFired, &old)
	purged["expired before cutoff"] = withStatus(old, store.ScheduledEventExpired, &old)
	purged["cancelled, created before"] = cancelled(old)
	purged["failed, no fired_at, created before"] = withStatus(old, store.ScheduledEventFailed, nil)
	kept := map[string]string{}
	kept["created before, fired after"] = withStatus(old, store.ScheduledEventFired, &recent)
	kept["created before, failed after"] = withStatus(old, store.ScheduledEventFailed, &recent)
	kept["created before, expired after"] = withStatus(old, store.ScheduledEventExpired, &recent)
	kept["cancelled, created after"] = cancelled(recent)
	kept["pending, created before"] = newEvent(old).ID
	kept["firing, fired_at before cutoff"] = withStatus(old, store.ScheduledEventFiring, &old)
	kept["firing, no fired_at, created before"] = withStatus(old, store.ScheduledEventFiring, nil)

	n, err := s.PurgeOldScheduledEvents(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, len(purged), n)

	for name, id := range purged {
		_, err := s.GetScheduledEvent(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "%s: purged", name)
	}
	for name, id := range kept {
		_, err := s.GetScheduledEvent(ctx, id)
		assert.NoError(t, err, "%s: kept", name)
	}
}

// TestListPendingScheduledEventsClaimPath verifies the scheduled-event job-claim
// path (dialect-aware SKIP LOCKED helper): only pending events are returned,
// ordered by fire_at ascending.
func TestListPendingScheduledEventsClaimPath(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)

	early := newTestScheduledEvent(projectID)
	early.FireAt = now.Add(1 * time.Minute)
	require.NoError(t, s.CreateScheduledEvent(ctx, early))

	late := newTestScheduledEvent(projectID)
	late.FireAt = now.Add(10 * time.Minute)
	require.NoError(t, s.CreateScheduledEvent(ctx, late))

	// A fired event must be excluded from the pending claim.
	fired := newTestScheduledEvent(projectID)
	require.NoError(t, s.CreateScheduledEvent(ctx, fired))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, fired.ID, store.ScheduledEventFired, &now, ""))

	pending, err := s.ListPendingScheduledEvents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, early.ID, pending[0].ID, "ordered by fire_at ascending")
	assert.Equal(t, late.ID, pending[1].ID)
}

func TestCompleteFiringScheduledEvent(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)

	// One-shot firing event: first completion succeeds, second is fenced out.
	oneShot := newTestScheduledEvent(projectID)
	require.NoError(t, s.CreateScheduledEvent(ctx, oneShot))
	claimed, err := s.ClaimScheduledEvent(ctx, oneShot.ID, store.ScheduledEventFiring)
	require.NoError(t, err)
	require.True(t, claimed)

	completedAt := now.Add(5 * time.Second)
	ok, err := s.CompleteFiringScheduledEvent(ctx, oneShot.ID, store.ScheduledEventFired, completedAt, "", "", time.Time{})
	require.NoError(t, err)
	require.True(t, ok)

	got, err := s.GetScheduledEvent(ctx, oneShot.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	require.NotNil(t, got.FiredAt)
	assert.True(t, completedAt.Equal(*got.FiredAt))
	assert.Empty(t, got.Error)

	// Second call cannot overwrite the completed event.
	ok, err = s.CompleteFiringScheduledEvent(ctx, oneShot.ID, store.ScheduledEventFailed, completedAt.Add(time.Second), "late failure", "", time.Time{})
	require.NoError(t, err)
	assert.False(t, ok)
	got, err = s.GetScheduledEvent(ctx, oneShot.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	assert.Empty(t, got.Error)

	// Recurring firing event: atomically updates event status and schedule counters,
	// without rewinding next_run_at, and fences out duplicate completion.
	sched := newTestSchedule(projectID, "wake-rec")
	nextRun := now.Add(time.Hour)
	sched.NextRunAt = &nextRun
	require.NoError(t, s.CreateSchedule(ctx, sched))

	recEvt := newTestScheduledEvent(projectID)
	recEvt.Status = store.ScheduledEventFiring
	recEvt.FiredAt = &now
	recEvt.ScheduleID = sched.ID
	require.NoError(t, s.CreateScheduledEvent(ctx, recEvt))

	failedAt := now.Add(10 * time.Second)
	ok, err = s.CompleteFiringScheduledEvent(ctx, recEvt.ID, store.ScheduledEventFailed, failedAt, "hub stopped", sched.ID, now)
	require.NoError(t, err)
	require.True(t, ok)

	gotRec, err := s.GetScheduledEvent(ctx, recEvt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFailed, gotRec.Status)
	assert.Equal(t, "hub stopped", gotRec.Error)

	gotSched, err := s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotSched.RunCount)
	assert.Equal(t, 1, gotSched.ErrorCount)
	assert.Equal(t, store.ScheduleRunError, gotSched.LastRunStatus)
	assert.Equal(t, "hub stopped", gotSched.LastRunError)
	require.NotNil(t, gotSched.NextRunAt)
	assert.True(t, nextRun.Equal(*gotSched.NextRunAt))

	// Duplicate call neither changes the event nor increments RunCount a second time.
	ok, err = s.CompleteFiringScheduledEvent(ctx, recEvt.ID, store.ScheduledEventFired, failedAt.Add(time.Second), "", sched.ID, now)
	require.NoError(t, err)
	assert.False(t, ok)
	gotSched, err = s.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotSched.RunCount)
	assert.Equal(t, 1, gotSched.ErrorCount)

	// Deleted schedule does not prevent finalizing an orphaned firing event.
	orphanEvt := newTestScheduledEvent(projectID)
	orphanEvt.Status = store.ScheduledEventFiring
	orphanEvt.FiredAt = &now
	orphanEvt.ScheduleID = uuid.NewString()
	require.NoError(t, s.CreateScheduledEvent(ctx, orphanEvt))

	ok, err = s.CompleteFiringScheduledEvent(ctx, orphanEvt.ID, store.ScheduledEventFailed, failedAt, "orphaned", orphanEvt.ScheduleID, now)
	require.NoError(t, err)
	require.True(t, ok)
	gotOrphan, err := s.GetScheduledEvent(ctx, orphanEvt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFailed, gotOrphan.Status)
}

func TestListFiringScheduledEvents(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)

	oldestAt := now.Add(-10 * time.Minute)
	oldest := newTestScheduledEvent(projectID)
	oldest.Status = store.ScheduledEventFiring
	oldest.FiredAt = &oldestAt
	require.NoError(t, s.CreateScheduledEvent(ctx, oldest))

	midAt := now.Add(-5 * time.Minute)
	mid := newTestScheduledEvent(projectID)
	mid.Status = store.ScheduledEventFiring
	mid.FiredAt = &midAt
	require.NoError(t, s.CreateScheduledEvent(ctx, mid))

	recentAt := now.Add(-10 * time.Second)
	recent := newTestScheduledEvent(projectID)
	recent.Status = store.ScheduledEventFiring
	recent.FiredAt = &recentAt
	require.NoError(t, s.CreateScheduledEvent(ctx, recent))

	// Non-firing events are never returned.
	fired := newTestScheduledEvent(projectID)
	fired.Status = store.ScheduledEventFired
	fired.FiredAt = &oldestAt
	require.NoError(t, s.CreateScheduledEvent(ctx, fired))

	cutoff := now.Add(-time.Minute)
	stale, err := s.ListFiringScheduledEvents(ctx, cutoff, 10)
	require.NoError(t, err)
	require.Len(t, stale, 2)
	assert.Equal(t, oldest.ID, stale[0].ID)
	assert.Equal(t, mid.ID, stale[1].ID)

	// Limit is respected.
	paged, err := s.ListFiringScheduledEvents(ctx, cutoff, 1)
	require.NoError(t, err)
	require.Len(t, paged, 1)
	assert.Equal(t, oldest.ID, paged[0].ID)
}
