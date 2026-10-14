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
